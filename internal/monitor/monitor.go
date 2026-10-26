// Package monitor provides an optional local event journal. Its data is a hint:
// clients rescan after a restart, overflow, watch error, or an unavailable helper.
package monitor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crease/internal/model"
	"crease/internal/objectstore"
	"golang.org/x/sys/unix"
)

type Event struct {
	Path string
	Tree bool
}
type watcher interface {
	Drain() ([]Event, error)
	Refresh(string) error
	Close()
	Name() string
}
type endpoint struct{ Address, Token string }
type request struct {
	Token, Op, Epoch string
	Since            uint64
}
type Response struct {
	Epoch   string
	Seq     uint64
	Full    bool
	Events  []Event
	Backend string
	Error   string
}
type record struct {
	Event
	seq uint64
}
type call struct {
	q     request
	reply chan Response
	done  chan struct{}
}

const journalLimit = 100_000

func excluded(p string) bool {
	if p == "" {
		return false
	}
	return !model.ValidPath(filepath.ToSlash(p))
}

func safePrefix(root, prefix string) error {
	info, e := os.Lstat(root)
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("monitor workspace root is not a directory")
	}
	parts := strings.Split(prefix, "/")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, e = os.Lstat(cur)
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("monitor parent became a symlink or special file")
		}
	}
	return nil
}
func token() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func endpointFile(root string) string { return filepath.Join(root, ".crease", "monitor.json") }

func Query(root, op, epoch string, since uint64) (Response, error) {
	var ep endpoint
	var out Response
	b, e := os.ReadFile(endpointFile(root))
	if e != nil {
		return out, e
	}
	if e = json.Unmarshal(b, &ep); e != nil {
		return out, e
	}
	host, _, e := net.SplitHostPort(ep.Address)
	if e != nil || host != "127.0.0.1" || len(ep.Token) != 64 {
		return out, errors.New("invalid monitor endpoint")
	}
	c, e := net.DialTimeout("tcp", ep.Address, 200*time.Millisecond)
	if e != nil {
		return out, e
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if e = json.NewEncoder(c).Encode(request{Token: ep.Token, Op: op, Epoch: epoch, Since: since}); e != nil {
		return out, e
	}
	e = json.NewDecoder(io.LimitReader(c, 32<<20)).Decode(&out)
	if e == nil && out.Error != "" {
		e = errors.New(out.Error)
	}
	return out, e
}

// Run serves a single repository without holding its workspace lock. Every
// query drains the native event queue before capturing the returned sequence.
// Events arriving during a client's scan remain after that sequence for retry.
func Run(ctx context.Context, root string) error { return run(ctx, root, newWatcher) }
func run(ctx context.Context, root string, open func(string) (watcher, error)) error {
	lock, e := os.OpenFile(filepath.Join(root, ".crease", "monitor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return errors.New("a monitor is already running")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	w, e := open(root)
	if e != nil {
		return e
	}
	defer func() {
		if w != nil {
			w.Close()
		}
	}()
	if e = w.Refresh(""); e != nil {
		return fmt.Errorf("cannot monitor workspace: %w", e)
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer ln.Close()
	secret, epoch := token(), token()
	ep, _ := json.Marshal(endpoint{Address: ln.Addr().String(), Token: secret})
	if e = objectstore.CacheFile(endpointFile(root), ep); e != nil {
		return e
	}
	defer os.Remove(endpointFile(root))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	requests := make(chan call)
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				var q request
				if json.NewDecoder(io.LimitReader(c, 4096)).Decode(&q) != nil || subtle.ConstantTimeCompare([]byte(q.Token), []byte(secret)) != 1 {
					return
				}
				r := call{q: q, reply: make(chan Response, 1), done: make(chan struct{})}
				select {
				case requests <- r:
				case <-ctx.Done():
					return
				}
				select {
				case out := <-r.reply:
					_ = json.NewEncoder(c).Encode(out)
				case <-ctx.Done():
				}
				close(r.done)
			}(c)
		}
	}()
	var seq, floor uint64
	var journal []record
	journalBytes := 0
	drain := func() error {
		events, e := w.Drain()
		if e != nil {
			return e
		}
		refresh := map[string]bool{}
		for _, event := range events {
			seq++
			journalBytes += len(event.Path) + 32
			journal = append(journal, record{Event: event, seq: seq})
			refresh[event.Path] = true
		}
		paths := make([]string, 0, len(refresh))
		for p := range refresh {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		chosen := map[string]bool{}
		for _, p := range paths {
			covered := false
			for parent := p; ; {
				if chosen[parent] {
					covered = true
					break
				}
				if parent == "" {
					break
				}
				parent = path.Dir(parent)
				if parent == "." {
					parent = ""
				}
			}
			if covered {
				continue
			}
			chosen[p] = true
			if e = w.Refresh(p); e != nil {
				return e
			}
		}
		if len(journal) > journalLimit || journalBytes > 8<<20 {
			floor = seq
			journal = nil
			journalBytes = 0
			epoch = token()
		}
		return nil
	}
	reset := func() error {
		w.Close()
		var err error
		w, err = open(root)
		if err != nil {
			return err
		}
		epoch = token()
		seq = 0
		floor = 0
		journalBytes = 0
		journal = nil
		return w.Refresh("")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if e = drain(); e != nil {
				if e = reset(); e != nil {
					return e
				}
			}
		case c := <-requests:
			if e = drain(); e != nil {
				if e = reset(); e != nil {
					c.reply <- Response{Error: e.Error()}
					return e
				}
			}
			out := Response{Epoch: epoch, Seq: seq, Backend: w.Name()}
			switch c.q.Op {
			case "changes":
				out.Full = c.q.Epoch != epoch || c.q.Since < floor || c.q.Since > seq
				if !out.Full {
					seen := map[string]bool{}
					start := sort.Search(len(journal), func(i int) bool { return journal[i].seq > c.q.Since })
					for _, r := range journal[start:] {
						if r.seq > c.q.Since && !seen[r.Path] {
							out.Events = append(out.Events, r.Event)
							seen[r.Path] = true
						}
					}
				}
			case "status":
			case "stop":
			default:
				out.Error = "unsupported monitor operation"
			}
			c.reply <- out
			if c.q.Op == "stop" {
				select {
				case <-c.done:
				case <-time.After(time.Second):
				}
				return nil
			}
		}
	}
}

func beneath(p, prefix string) bool {
	return prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/")
}
