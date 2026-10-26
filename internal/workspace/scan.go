package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"crease/internal/model"
	"crease/internal/objectstore"
	"golang.org/x/sys/unix"
)

// A scan anchors directory lookups to opened descriptors. Shared parents open
// once, and O_NOFOLLOW still prevents an editor's rename from redirecting reads
// outside the workspace through a symlink.
type fileScan struct {
	w      *Workspace
	dirs   map[string]int
	cutoff time.Time
}

func (w *Workspace) newScan() (*fileScan, error) {
	fd, e := unix.Open(w.Root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	return &fileScan{w: w, dirs: map[string]int{"": fd}, cutoff: time.Now().Add(-time.Second)}, nil
}
func (s *fileScan) close() {
	for _, fd := range s.dirs {
		unix.Close(fd)
	}
}

// A cached directory descriptor cannot follow a replacement symlink, but its
// former name might now refer to another directory. Reject that race before
// returning a snapshot rather than committing files from the renamed directory.
func (s *fileScan) check() error {
	for p, fd := range s.dirs {
		var held, current unix.Stat_t
		if e := unix.Fstat(fd, &held); e != nil {
			return e
		}
		if p == "" {
			if e := unix.Lstat(s.w.Root, &current); e != nil {
				return e
			}
		} else {
			parent, name := path.Split(p)
			parent = strings.TrimSuffix(parent, "/")
			if e := unix.Fstatat(s.dirs[parent], name, &current, unix.AT_SYMLINK_NOFOLLOW); e != nil {
				return e
			}
		}
		if current.Mode&unix.S_IFMT != unix.S_IFDIR || current.Dev != held.Dev || current.Ino != held.Ino {
			return fmt.Errorf("directory changed during scan; retry: %s", p)
		}
	}
	return nil
}
func (s *fileScan) parent(p string) (int, string, error) {
	if !model.ValidPath(p) {
		return 0, "", fmt.Errorf("unsafe path %q", p)
	}
	dir, name := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	fd, e := s.directory(dir)
	return fd, name, e
}
func (s *fileScan) directory(p string) (int, error) {
	if fd, ok := s.dirs[p]; ok {
		return fd, nil
	}
	parent, name := path.Split(p)
	fd, e := s.directory(strings.TrimSuffix(parent, "/"))
	if e != nil {
		return 0, e
	}
	fd, e = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return 0, &os.PathError{Op: "open directory without following symlinks", Path: p, Err: e}
	}
	s.dirs[p] = fd
	return fd, nil
}
func (s *fileScan) stat(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	e := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if e == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
		e = errors.New("symlinks and special files are unsupported")
	}
	if e == nil && st.Size > model.MaxObject {
		e = errors.New("file exceeds 8 MiB limit")
	}
	return st, e
}
func (s *fileScan) bytes(p string) ([]byte, unix.Stat_t, error) {
	fd, name, e := s.parent(p)
	if e != nil {
		return nil, unix.Stat_t{}, e
	}
	before, e := s.stat(fd, name)
	if e != nil {
		return nil, before, e
	}
	filefd, e := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if e != nil {
		return nil, before, e
	}
	f := os.NewFile(uintptr(filefd), p)
	defer f.Close()
	var opened unix.Stat_t
	if e = unix.Fstat(filefd, &opened); e != nil {
		return nil, before, e
	}
	a, _, _ := nativeStamp(&before)
	b, _, _ := nativeStamp(&opened)
	if a != b {
		return nil, before, fmt.Errorf("file changed while reading; retry: %s", p)
	}
	data, e := io.ReadAll(io.LimitReader(f, model.MaxObject+1))
	if e != nil {
		return nil, before, e
	}
	after, e := s.stat(fd, name)
	if e != nil {
		return nil, before, e
	}
	b, _, _ = nativeStamp(&after)
	if a != b || len(data) > model.MaxObject {
		return nil, before, fmt.Errorf("file changed while reading; retry: %s", p)
	}
	return data, before, nil
}
func (s *fileScan) read(p string, persist bool) (objectstore.File, error) {
	w := s.w
	if e := w.loadIndex(); e != nil {
		return objectstore.File{}, e
	}
	fd, name, e := s.parent(p)
	if e != nil {
		return objectstore.File{}, e
	}
	st, e := s.stat(fd, name)
	if e != nil {
		return objectstore.File{}, e
	}
	stamp, mtime, ctime := nativeStamp(&st)
	old, ok := w.cache[p]
	file := objectstore.File{ID: old.id, Mode: old.mode}
	// Unchanged entries already belong to the durable committed snapshot. An
	// inspected edit can have a cached digest without a stored object, so only
	// those entries need a store lookup before reusing their digest for commit.
	if ok && !old.deleted && old.stamp == stamp && mtime.Before(s.cutoff) && ctime.Before(s.cutoff) && (!persist || file == w.base[p] || w.Store.Exists(old.id)) {
		return objectstore.File{ID: old.id, Mode: old.mode}, nil
	}
	data, st, e := s.bytes(p)
	if e != nil {
		return objectstore.File{}, e
	}
	stamp, _, _ = nativeStamp(&st)
	o := model.Object{Type: "blob", Data: data}
	id := o.ID()
	if persist {
		base := old.id
		if f, ok := w.base[p]; ok {
			base = f.ID
		}
		if _, e = w.Store.PutWithBase(o, base); e != nil {
			return objectstore.File{}, e
		}
	}
	mode := "file"
	if st.Mode&0111 != 0 {
		mode = "exec"
	}
	c := cacheEntry{stamp: stamp, id: id, mode: mode}
	if old != c {
		w.cache[p] = c
		w.indexDirty = true
	}
	return objectstore.File{ID: id, Mode: mode}, nil
}

// WorkingBytes reads only a changed diff input and checks that it still has the
// identity discovered by the inspection pass. No repository object is written.
func (w *Workspace) WorkingBytes(p string, expected objectstore.File) ([]byte, error) {
	s, e := w.newScan()
	if e != nil {
		return nil, e
	}
	defer s.close()
	b, _, e := s.bytes(p)
	if e == nil {
		e = s.check()
	}
	if e == nil && (model.Object{Type: "blob", Data: b}).ID() != expected.ID {
		e = fmt.Errorf("file changed during diff; retry: %s", p)
	}
	return b, e
}
