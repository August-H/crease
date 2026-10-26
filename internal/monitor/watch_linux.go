package monitor

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type nativeWatcher struct {
	root   string
	fd     int
	names  map[int]string
	buffer [64 << 10]byte
}

func newWatcher(root string) (watcher, error) {
	fd, e := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if e != nil {
		return nil, e
	}
	return &nativeWatcher{root: root, fd: fd, names: map[int]string{}}, nil
}
func (w *nativeWatcher) Name() string { return "inotify" }
func (w *nativeWatcher) Close() {
	if w != nil && w.fd >= 0 {
		unix.Close(w.fd)
		w.fd = -1
	}
}
func (w *nativeWatcher) Refresh(prefix string) error {
	if e := safePrefix(w.root, prefix); e != nil {
		if os.IsNotExist(e) && prefix != "" {
			return nil
		}
		return e
	}
	for wd, p := range w.names {
		if beneath(p, prefix) {
			_, _ = unix.InotifyRmWatch(w.fd, uint32(wd))
			delete(w.names, wd)
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
		if !d.IsDir() {
			return nil
		}
		var st unix.Statfs_t
		if e = unix.Statfs(file, &st); e != nil {
			return e
		}
		switch st.Type {
		case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.RAMFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		default:
			return errors.New("native monitoring requires a supported local filesystem")
		}
		if len(w.names) >= 50000 {
			return errors.New("monitor watch limit reached")
		}
		mask := uint32(unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_MOVE_SELF | unix.IN_DELETE_SELF | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW)
		wd, e := unix.InotifyAddWatch(w.fd, file, mask)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		w.names[wd] = p
		return nil
	})
}
func (w *nativeWatcher) Drain() ([]Event, error) {
	var out []Event
	buffer := w.buffer[:]
	for {
		n, e := unix.Read(w.fd, buffer)
		if e == unix.EAGAIN {
			return out, nil
		}
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			return nil, e
		}
		for off := 0; off+16 <= n; {
			r := buffer[off:]
			wd := int(int32(binary.NativeEndian.Uint32(r)))
			mask := binary.NativeEndian.Uint32(r[4:])
			size := int(binary.NativeEndian.Uint32(r[12:]))
			off += 16 + size
			if off > n || mask&unix.IN_Q_OVERFLOW != 0 {
				return nil, errors.New("monitor event overflow")
			}
			parent, ok := w.names[wd]
			if mask&unix.IN_IGNORED != 0 {
				if ok {
					return nil, errors.New("monitor lost a directory watch")
				}
				continue
			}
			if !ok {
				continue
			}
			if mask&unix.IN_UNMOUNT != 0 {
				return nil, errors.New("monitored filesystem unmounted")
			}
			if mask&unix.IN_DELETE_SELF != 0 {
				delete(w.names, wd)
			}
			name := strings.TrimRight(string(r[16:16+size]), "\x00")
			p := filepath.ToSlash(filepath.Join(parent, name))
			if p == "." {
				p = ""
			}
			if !excluded(p) {
				out = append(out, Event{Path: p, Tree: mask&unix.IN_ISDIR != 0 || name == ""})
			}
		}
		if len(out) > journalLimit {
			return nil, errors.New("monitor event overflow")
		}
		if n == 0 {
			return out, nil
		}
	}
}
