package monitor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type vnode struct {
	fd  int
	dir bool
}
type nativeWatcher struct {
	root   string
	fd     int
	paths  map[string]vnode
	names  map[int]string
	events [256]unix.Kevent_t
}

func newWatcher(root string) (watcher, error) {
	var lim unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &lim) == nil && lim.Cur < min(lim.Max, 65536) {
		lim.Cur = min(lim.Max, 65536)
		_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &lim)
	}
	fd, e := unix.Kqueue()
	if e != nil {
		return nil, e
	}
	unix.CloseOnExec(fd)
	return &nativeWatcher{root: root, fd: fd, paths: map[string]vnode{}, names: map[int]string{}}, nil
}
func (w *nativeWatcher) Name() string { return "kqueue" }
func (w *nativeWatcher) Close() {
	if w == nil {
		return
	}
	for _, v := range w.paths {
		unix.Close(v.fd)
	}
	if w.fd >= 0 {
		unix.Close(w.fd)
		w.fd = -1
	}
	w.paths = map[string]vnode{}
}
func (w *nativeWatcher) Refresh(prefix string) error {
	if e := safePrefix(w.root, prefix); e != nil {
		if os.IsNotExist(e) && prefix != "" {
			return nil
		}
		return e
	}
	for p, v := range w.paths {
		if beneath(p, prefix) {
			unix.Close(v.fd)
			delete(w.names, v.fd)
			delete(w.paths, p)
		}
	}
	start := filepath.Join(w.root, filepath.FromSlash(prefix))
	return filepath.WalkDir(start, func(file string, d fs.DirEntry, e error) error {
		if os.IsNotExist(e) && prefix != "" {
			return nil
		}
		if e != nil {
			return e
		}
		p, e := filepath.Rel(w.root, file)
		if e != nil {
			return e
		}
		if p == "." {
			p = ""
		}
		p = filepath.ToSlash(p)
		if excluded(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if p == "" {
				return errors.New("workspace root is a symlink")
			}
			return nil
		}
		if d.IsDir() {
			var st unix.Statfs_t
			if e = unix.Statfs(file, &st); e != nil {
				return e
			}
			kind := strings.TrimRight(string(st.Fstypename[:]), "\x00")
			if kind != "apfs" && kind != "hfs" {
				return errors.New("native monitoring requires an APFS or HFS workspace")
			}
		}
		if len(w.paths) >= 50000 {
			return errors.New("monitor watch limit reached")
		}
		fd, e := unix.Open(file, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		ev := unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_VNODE, Flags: unix.EV_ADD | unix.EV_CLEAR, Fflags: unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB | unix.NOTE_RENAME | unix.NOTE_DELETE | unix.NOTE_REVOKE}
		if _, e = unix.Kevent(w.fd, []unix.Kevent_t{ev}, nil, nil); e != nil {
			unix.Close(fd)
			return e
		}
		w.paths[p] = vnode{fd: fd, dir: d.IsDir()}
		w.names[fd] = p
		return nil
	})
}
func (w *nativeWatcher) Drain() ([]Event, error) {
	var out []Event
	for {
		buffer := w.events[:]
		zero := unix.Timespec{}
		n, e := unix.Kevent(w.fd, nil, buffer, &zero)
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			return nil, e
		}
		for _, ev := range buffer[:n] {
			if ev.Flags&unix.EV_ERROR != 0 || ev.Fflags&unix.NOTE_REVOKE != 0 {
				return nil, errors.New("monitor lost a watch")
			}
			p, ok := w.names[int(ev.Ident)]
			if ok {
				out = append(out, Event{Path: p, Tree: w.paths[p].dir})
			}
		}
		if len(out) > journalLimit {
			return nil, errors.New("monitor event overflow")
		}
		if n < 256 {
			return out, nil
		}
	}
}
