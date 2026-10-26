package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"crease/internal/model"
	"crease/internal/objectstore"
)

const workspaceMagic = "CRWI\x00\x01"

type indexReader struct {
	*bytes.Reader
	err error
}

func (r *indexReader) number() uint64 {
	var n uint64
	if r.err == nil {
		r.err = binary.Read(r.Reader, binary.BigEndian, &n)
	}
	return n
}
func (r *indexReader) text() string {
	var n uint16
	if r.err == nil {
		r.err = binary.Read(r.Reader, binary.BigEndian, &n)
	}
	b := make([]byte, int(n))
	if r.err == nil {
		_, r.err = io.ReadFull(r.Reader, b)
	}
	return string(b)
}
func (r *indexReader) file() objectstore.File {
	var b [33]byte
	if r.err == nil {
		_, r.err = io.ReadFull(r.Reader, b[:])
	}
	mode := "file"
	if b[0] == 2 {
		mode = "exec"
	} else if b[0] != 1 {
		r.err = errors.New("invalid index mode")
	}
	return objectstore.File{Mode: mode, ID: hex.EncodeToString(b[1:])}
}
func (r *indexReader) count() int {
	n := r.number()
	if n > 1_000_000 || n > uint64(r.Len()+1) {
		r.err = errors.New("oversized workspace index")
		return 0
	}
	return int(n)
}
func (r *indexReader) path() string {
	p := r.text()
	if !model.ValidPath(p) {
		r.err = errors.New("invalid index path")
	}
	return p
}
func (w *Workspace) loadIndex() error {
	if w.indexLoaded {
		return nil
	}
	w.indexLoaded = true
	b, e := os.ReadFile(filepath.Join(w.Root, ".crease", "workspace-index"))
	if e == nil && len(b) >= 38 && len(b) <= 256<<20 && string(b[:6]) == workspaceMagic {
		sum := sha256.Sum256(b[:len(b)-32])
		if bytes.Equal(sum[:], b[len(b)-32:]) {
			r := &indexReader{Reader: bytes.NewReader(b[6 : len(b)-32])}
			generation := int64(r.number())
			head, sparse, epoch := r.text(), r.text(), r.text()
			seq := r.number()
			ready := r.number()
			base := objectstore.Snapshot{}
			for n := r.count(); n > 0; n-- {
				p := r.path()
				base[p] = r.file()
			}
			tracked := []string{}
			for n := r.count(); n > 0; n-- {
				tracked = append(tracked, r.path())
			}
			cache := map[string]cacheEntry{}
			for n := r.count(); n > 0; n-- {
				p, stamp := r.path(), r.text()
				f := r.file()
				deleted := r.number() != 0
				cache[p] = cacheEntry{stamp: stamp, id: f.ID, mode: f.Mode, deleted: deleted}
			}
			var discovered []string
			for n := r.count(); n > 0; n-- {
				discovered = append(discovered, r.path())
			}
			if r.err == nil && r.Len() == 0 && generation >= 0 && (head == "" || model.ValidID(head)) && sort.StringsAreSorted(tracked) {
				w.indexGeneration = generation
				w.baseID = head
				w.base = base
				w.trackedCache = tracked
				w.cache = cache
				w.discovered = discovered
				w.discoveryReady = ready == 1
				w.monitorEpoch = epoch
				w.monitorSeq = seq
				w.indexSparse = sparse
				return nil
			}
		}
	}
	// Old releases stored only file fingerprints in SQLite. Import them once;
	// all new cache writes use the disposable compact index.
	rows, e := w.DB.Query(`SELECT path,stamp,id,mode FROM file_cache`)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var c cacheEntry
		if e = rows.Scan(&p, &c.stamp, &c.id, &c.mode); e != nil {
			return e
		}
		w.cache[p] = c
	}
	w.indexDirty = true
	return rows.Err()
}
func putNumber(b *bytes.Buffer, n uint64) { _ = binary.Write(b, binary.BigEndian, n) }
func putText(b *bytes.Buffer, s string) error {
	if len(s) > 65535 {
		return errors.New("index string too long")
	}
	_ = binary.Write(b, binary.BigEndian, uint16(len(s)))
	b.WriteString(s)
	return nil
}
func putFile(b *bytes.Buffer, f objectstore.File) {
	mode := byte(1)
	if f.Mode == "exec" {
		mode = 2
	}
	b.WriteByte(mode)
	id, _ := hex.DecodeString(f.ID)
	if len(id) != 32 {
		id = make([]byte, 32)
	}
	b.Write(id)
}
func (w *Workspace) saveIndex() error {
	if !w.indexLoaded || w.trackedCache == nil || w.base == nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString(workspaceMagic)
	putNumber(&b, uint64(w.indexGeneration))
	for _, s := range []string{w.baseID, w.indexSparse, w.monitorEpoch} {
		if e := putText(&b, s); e != nil {
			return e
		}
	}
	putNumber(&b, w.monitorSeq)
	ready := uint64(0)
	if w.discoveryReady {
		ready = 1
	}
	putNumber(&b, ready)
	putNumber(&b, uint64(len(w.base)))
	for p, f := range w.base {
		if e := putText(&b, p); e != nil {
			return e
		}
		putFile(&b, f)
	}
	putNumber(&b, uint64(len(w.trackedCache)))
	for _, p := range w.trackedCache {
		if e := putText(&b, p); e != nil {
			return e
		}
	}
	putNumber(&b, uint64(len(w.cache)))
	for p, c := range w.cache {
		if e := putText(&b, p); e != nil {
			return e
		}
		if e := putText(&b, c.stamp); e != nil {
			return e
		}
		putFile(&b, objectstore.File{ID: c.id, Mode: c.mode})
		d := uint64(0)
		if c.deleted {
			d = 1
		}
		putNumber(&b, d)
	}
	putNumber(&b, uint64(len(w.discovered)))
	for _, p := range w.discovered {
		if e := putText(&b, p); e != nil {
			return e
		}
	}
	sum := sha256.Sum256(b.Bytes())
	b.Write(sum[:])
	return objectstore.CacheFile(filepath.Join(w.Root, ".crease", "workspace-index"), b.Bytes())
}
