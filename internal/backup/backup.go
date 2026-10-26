// Package backup creates consistent offline server backups with an integrity manifest.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"crease/internal/model"
	"crease/internal/objectstore"
	"crease/internal/server"
)

type Manifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

func hash(file string) (string, error) {
	f, e := os.Open(file)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func stage(destination string) (string, string, error) {
	abs, e := filepath.Abs(destination)
	if e != nil {
		return "", "", e
	}
	if _, e = os.Lstat(abs); !os.IsNotExist(e) {
		return "", "", errors.New("destination must not exist")
	}
	if e = os.MkdirAll(filepath.Dir(abs), 0700); e != nil {
		return "", "", e
	}
	tmp, e := os.MkdirTemp(filepath.Dir(abs), ".crease-copy-")
	return abs, tmp, e
}
func outside(source, dest string) bool {
	rel, e := filepath.Rel(source, dest)
	return e == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Resolve symlink aliases in the existing portion of an output path before
// deciding whether a backup would recursively copy itself.
func canonicalDestination(p string) (string, error) {
	abs, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	var tail []string
	cur := abs
	for {
		if _, e = os.Lstat(cur); e == nil {
			resolved, e := filepath.EvalSymlinks(cur)
			if e != nil {
				return "", e
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", errors.New("cannot resolve destination")
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}
func copyFile(source, dest string) error {
	st, e := os.Lstat(source)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return errors.New("backup contains a nonregular file")
	}
	in, e := os.Open(source)
	if e != nil {
		return e
	}
	defer in.Close()
	if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
		return e
	}
	out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e == nil {
		e = ce
	}
	return e
}
func syncDirs(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return objectstore.SyncDir(p)
		}
		return nil
	})
}

func Create(data, output string) error {
	source, e := filepath.Abs(data)
	if e != nil {
		return e
	}
	source, e = filepath.EvalSymlinks(source)
	if e != nil {
		return e
	}
	dest, e := canonicalDestination(output)
	if e != nil {
		return e
	}
	if !outside(source, dest) {
		return errors.New("backup output must be outside the server data directory")
	}
	if _, e = os.Stat(filepath.Join(source, "server.db")); e != nil {
		return e
	}
	s, e := server.Open(source, "")
	if e != nil {
		return fmt.Errorf("stop the server before backup: %w", e)
	}
	defer s.Close()
	dest, tmp, e := stage(dest)
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	if _, e = s.DB.Exec(`VACUUM INTO ?`, filepath.Join(tmp, "server.db")); e != nil {
		return e
	}
	if e = os.Chmod(filepath.Join(tmp, "server.db"), 0600); e != nil {
		return e
	}
	if e = copyFile(filepath.Join(source, "host_key"), filepath.Join(tmp, "host_key")); e != nil {
		return e
	}
	repos := filepath.Join(source, "repositories")
	if _, e = os.Stat(repos); e == nil {
		e = filepath.WalkDir(repos, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlink in server data")
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".write-") {
				return nil
			}
			rel, e := filepath.Rel(source, p)
			if e != nil {
				return e
			}
			return copyFile(p, filepath.Join(tmp, rel))
		})
		if e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	m := Manifest{Version: 1, Files: map[string]string{}}
	e = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(tmp, p)
		if e != nil {
			return e
		}
		sum, e := hash(p)
		if e != nil {
			return e
		}
		m.Files[filepath.ToSlash(rel)] = sum
		return nil
	})
	if e != nil {
		return e
	}
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return e
	}
	if e = objectstore.Atomic(filepath.Join(tmp, "manifest.json"), b, 0600); e != nil {
		return e
	}
	if e = syncDirs(tmp); e != nil {
		return e
	}
	if e = os.Rename(tmp, dest); e != nil {
		return e
	}
	return objectstore.SyncDir(filepath.Dir(dest))
}
func Restore(input, data string) error {
	source, e := filepath.Abs(input)
	if e != nil {
		return e
	}
	source, e = filepath.EvalSymlinks(source)
	if e != nil {
		return e
	}
	destination, e := canonicalDestination(data)
	if e != nil {
		return e
	}
	if !outside(source, destination) {
		return errors.New("restore destination must be outside the backup")
	}
	b, e := os.ReadFile(filepath.Join(source, "manifest.json"))
	if e != nil {
		return e
	}
	var m Manifest
	if e = json.Unmarshal(b, &m); e != nil {
		return e
	}
	if m.Version != 1 || len(m.Files) == 0 || m.Files["server.db"] == "" || m.Files["host_key"] == "" {
		return errors.New("unsupported or incomplete backup manifest")
	}
	dest, tmp, e := stage(destination)
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	if !outside(source, dest) {
		return errors.New("restore destination must be outside the backup")
	}
	for rel, sum := range m.Files {
		if !model.ValidPath(rel) || len(sum) != 64 {
			return errors.New("invalid backup manifest path or hash")
		}
		file := filepath.Join(source, filepath.FromSlash(rel))
		cur := source
		for _, part := range strings.Split(rel, "/") {
			cur = filepath.Join(cur, part)
			st, e := os.Lstat(cur)
			if e != nil {
				return e
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return errors.New("symlink in backup")
			}
		}
		got, e := hash(file)
		if e != nil {
			return e
		}
		if got != sum {
			return fmt.Errorf("backup integrity failure: %s", rel)
		}
		if e = copyFile(file, filepath.Join(tmp, filepath.FromSlash(rel))); e != nil {
			return e
		}
	}
	s, e := server.Open(tmp, "")
	if e != nil {
		return fmt.Errorf("invalid restored server: %w", e)
	}
	s.Close()
	_ = os.Remove(filepath.Join(tmp, "server.lock"))
	if e = syncDirs(tmp); e != nil {
		return e
	}
	if e = os.Rename(tmp, dest); e != nil {
		return e
	}
	return objectstore.SyncDir(filepath.Dir(dest))
}
