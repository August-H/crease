package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"crease/internal/model"
	"crease/internal/objectstore"
	"golang.org/x/sys/unix"
)

const cloneMemoryLimit = 64 << 20

type cloneBlob struct {
	id   string
	data []byte
}

// CloneCheckout is only for a new, empty workspace. Verified blobs are written
// to private staging files while the network stream continues. No user path or
// branch moves until canonical objects are durable and the journal is synced.
type CloneCheckout struct {
	w        *Workspace
	root     *os.Root
	stage    string
	journal  Journal
	paths    []string
	byID     map[string][]string
	files    map[string]string
	received map[string]bool
	jobs     chan cloneBlob
	wg       sync.WaitGroup
	once     sync.Once
	syncOnce sync.Once
	mu       sync.Mutex
	cond     *sync.Cond
	bytes    int
	peak     int
	err      error
	Workers  int
	SyncMS   float64
	// Test interruptions after a particular working path has been published.
	afterPublish func(string) error
}

func (w *Workspace) PrepareClone(branch, head string, sparse []string) (*CloneCheckout, error) {
	if e := w.Ready(); e != nil {
		return nil, e
	}
	old, e := w.Head()
	if e != nil {
		return nil, e
	}
	var tracked int
	if e = w.DB.QueryRow(`SELECT count(*) FROM tracked`).Scan(&tracked); e != nil {
		return nil, e
	}
	if old != "" || tracked != 0 || !model.ValidRef(branch) || (head != "" && !model.ValidID(head)) {
		return nil, errors.New("streaming checkout requires an empty workspace and valid reference")
	}
	for _, p := range sparse {
		if !model.ValidPath(p) {
			return nil, errors.New("invalid selected path")
		}
	}
	full, e := w.Store.Snapshot(head)
	if e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(w.Root)
	if e != nil {
		return nil, e
	}
	stage, e := os.MkdirTemp(filepath.Join(w.Root, ".crease"), "clone-stage-")
	if e != nil {
		root.Close()
		return nil, e
	}
	c := &CloneCheckout{w: w, root: root, stage: path.Join(".crease", filepath.Base(stage)),
		journal: Journal{From: objectstore.Snapshot{}, To: visible(full, sparse), Full: full,
			Branch: branch, Head: head, Move: true, Sparse: sparse},
		byID: map[string][]string{}, files: map[string]string{}, received: map[string]bool{}}
	for p, f := range c.journal.To {
		if !model.ValidPath(p) || !model.ValidID(f.ID) || (f.Mode != "file" && f.Mode != "exec") {
			c.Close()
			return nil, errors.New("invalid checkout entry")
		}
		c.paths = append(c.paths, p)
		c.byID[f.ID] = append(c.byID[f.ID], p)
	}
	sort.Strings(c.paths)
	for i, p := range c.paths {
		c.files[p] = path.Join(c.stage, strconv.Itoa(i))
	}
	c.Workers = min(8, max(1, runtime.GOMAXPROCS(0)), max(1, len(c.paths)))
	c.jobs = make(chan cloneBlob, c.Workers)
	c.cond = sync.NewCond(&c.mu)
	for i := 0; i < c.Workers; i++ {
		c.wg.Add(1)
		go c.worker()
	}
	return c, nil
}

// Wants reports whether a blob belongs to the selected current snapshot.
func (c *CloneCheckout) Wants(id string) bool { return len(c.byID[id]) != 0 }

// AddVerified accepts an immutable payload checked by Store.ImportObject or Get.
// The caller must not modify data until Wait completes. Queued and active blob
// payloads share a 64 MiB budget, independent of repository size.
func (c *CloneCheckout) AddVerified(id string, data []byte) error {
	if !c.Wants(id) || c.received[id] {
		return errors.New("unexpected or duplicate checkout blob")
	}
	if len(data) > model.MaxObject {
		return errors.New("checkout blob exceeds limit")
	}
	c.mu.Lock()
	for c.err == nil && c.bytes+len(data) > cloneMemoryLimit {
		c.cond.Wait()
	}
	if c.err != nil {
		e := c.err
		c.mu.Unlock()
		return e
	}
	c.bytes += len(data)
	c.peak = max(c.peak, c.bytes)
	c.mu.Unlock()
	c.received[id] = true
	c.jobs <- cloneBlob{id, data}
	return nil
}

func (c *CloneCheckout) worker() {
	defer c.wg.Done()
	for job := range c.jobs {
		c.mu.Lock()
		failed := c.err != nil
		c.mu.Unlock()
		var err error
		if !failed {
			for _, p := range c.byID[job.id] {
				mode := os.FileMode(0644)
				if c.journal.To[p].Mode == "exec" {
					mode = 0755
				}
				f, e := c.root.OpenFile(c.files[p], os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
				if e != nil {
					err = e
					break
				}
				// Chmod ignores the caller's umask, just like ordinary checkout.
				e = f.Chmod(mode)
				if e == nil {
					_, e = f.Write(job.data)
				}
				err = errors.Join(e, f.Close())
				if err != nil {
					break
				}
			}
		}
		c.mu.Lock()
		if err != nil && c.err == nil {
			c.err = err
		}
		c.bytes -= len(job.data)
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

func (c *CloneCheckout) Wait() error {
	if c.jobs == nil {
		return nil
	}
	c.once.Do(func() { close(c.jobs) })
	c.wg.Wait()
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	if len(c.received) != len(c.byID) {
		c.mu.Unlock()
		return errors.New("clone omitted current file contents")
	}
	c.mu.Unlock()
	// Prepare the whole batch before flushing it. Interleaving each tiny write
	// with a filesystem barrier prevents the OS from coalescing dirty writes.
	c.syncOnce.Do(func() {
		started := time.Now()
		jobs := make(chan string)
		var workers sync.WaitGroup
		for i := 0; i < c.Workers; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for p := range jobs {
					f, e := c.root.Open(c.files[p])
					if e == nil {
						e = errors.Join(f.Sync(), f.Close())
					}
					if e != nil {
						c.mu.Lock()
						if c.err == nil {
							c.err = e
						}
						c.mu.Unlock()
					}
				}
			}()
		}
		for _, p := range c.paths {
			jobs <- p
		}
		close(jobs)
		workers.Wait()
		c.SyncMS = float64(time.Since(started)) / float64(time.Millisecond)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *CloneCheckout) PeakBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// Publish never trusts staging files during recovery. Once the journal exists,
// the ordinary recovery path can recreate every file from canonical objects.
func (c *CloneCheckout) Publish() error {
	if e := c.Wait(); e != nil {
		return e
	}
	if e := c.w.Store.Flush(); e != nil {
		return e
	}
	for _, p := range c.paths {
		if _, e := c.w.SafePath(p); e != nil {
			return e
		}
		if _, e := c.root.Lstat(p); !os.IsNotExist(e) {
			if e != nil {
				return e
			}
			return fmt.Errorf("untracked path would be overwritten: %s", p)
		}
	}
	b, e := json.Marshal(c.journal)
	if e != nil {
		return e
	}
	if e = objectstore.Atomic(filepath.Join(c.w.Root, ".crease", "journal.json"), b, 0600); e != nil {
		return e
	}
	dirs := map[string]bool{"": true}
	for _, p := range c.paths {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Strings(ordered)
	for _, d := range ordered {
		if d != "" {
			if e = c.root.MkdirAll(d, 0700); e != nil {
				return e
			}
		}
	}
	// Reuse anchored, no-follow directory descriptors instead of resolving all
	// parent components for each link and directory flush. Revalidate identities
	// before metadata publication, as in the workspace scanner.
	scan, e := c.w.newScan()
	if e != nil {
		return e
	}
	defer scan.close()
	source, e := c.root.Open(c.stage)
	if e != nil {
		return e
	}
	defer source.Close()
	for _, p := range c.paths {
		parent, name, e := scan.parent(p)
		if e != nil {
			return e
		}
		// Linkat is atomic and never replaces an existing working file.
		if e = unix.Linkat(int(source.Fd()), path.Base(c.files[p]), parent, name, 0); e != nil {
			return e
		}
		if c.afterPublish != nil {
			if e = c.afterPublish(p); e != nil {
				return e
			}
		}
	}
	if e = scan.check(); e != nil {
		return e
	}
	// Child directories and their contents become durable before their parents.
	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))
	for _, d := range ordered {
		fd, e := scan.directory(d)
		if e != nil {
			return e
		}
		if e = unix.Fsync(fd); e != nil {
			return e
		}
	}
	if e = scan.check(); e != nil {
		return e
	}
	return c.w.finishCheckout(c.journal)
}

// Close drains workers before removing private staging data. Published files
// and any recovery journal are left in place on errors.
func (c *CloneCheckout) Close() {
	_ = c.Wait()
	if c.root != nil {
		_ = c.root.RemoveAll(c.stage)
		_ = c.root.Close()
		c.root = nil
	}
}
