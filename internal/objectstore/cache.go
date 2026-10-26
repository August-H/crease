package objectstore

import (
	"os"
	"path/filepath"
)

// CacheFile atomically replaces a disposable hint without a durability flush.
// Authoritative objects, refs, and journals must use their durable write paths.
func CacheFile(file string, b []byte) error {
	f, e := os.CreateTemp(filepath.Dir(file), ".cache-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), file)
}
