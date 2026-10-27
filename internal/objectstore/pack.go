package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"crease/internal/codec"
	"crease/internal/model"
)

const packMagic = "CRPK\x00\x01"
const indexMagic = "CRIX\x00\x01"
const pendingLimit = 16 << 20

type location struct {
	pack   string
	offset int64
	size   uint32
	kind   string
}
type pendingObject struct {
	object  model.Object
	base    string
	encoded []byte
}
type decoded struct {
	object model.Object
	tree   *model.Tree
	commit *model.Commit
}
type state struct {
	mu           sync.Mutex
	loaded       bool
	index        map[string]location
	catalog      []byte
	catalogPacks map[string]bool
	packs        map[string]bool
	pending      map[string]pendingObject
	bytes        int
	cache        map[string]decoded
	cacheBytes   int
	depth        map[string]int
	// beforePublish is a test fault injection point after pack durability but
	// before the index makes its records visible to a reopened reader.
	beforePublish func() error
}

func New(dir string) Store {
	return Store{Dir: dir, state: &state{index: map[string]location{}, pending: map[string]pendingObject{}, cache: map[string]decoded{}, depth: map[string]int{}, packs: map[string]bool{}}}
}
func (s Store) loadLocked() error {
	st := s.state
	if st.loaded {
		return nil
	}
	dir := filepath.Join(s.Dir, "packs")
	entries, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		st.loaded = true
		return nil
	}
	if e != nil {
		return e
	}
	s.loadCatalog(entries)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".idx") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".idx")
		if st.catalogPacks[name] {
			continue
		}
		if !model.ValidID(name) {
			return errors.New("invalid pack index name")
		}
		b, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			return e
		}
		if len(b) < len(indexMagic)+4+32 || string(b[:6]) != indexMagic {
			return errors.New("invalid pack index")
		}
		sum := sha256.Sum256(b[:len(b)-32])
		if !bytes.Equal(sum[:], b[len(b)-32:]) {
			return errors.New("pack index checksum mismatch")
		}
		count := int(binary.BigEndian.Uint32(b[6:]))
		if count > 4096 || len(b) != 10+count*45+32 {
			return errors.New("invalid pack index size")
		}
		info, e := os.Stat(filepath.Join(dir, name+".pack"))
		if e != nil {
			return fmt.Errorf("pack index has no data file: %w", e)
		}
		for i := 0; i < count; i++ {
			row := b[10+i*45 : 10+(i+1)*45]
			id := hex.EncodeToString(row[:32])
			offset := int64(binary.BigEndian.Uint64(row[32:]))
			size := binary.BigEndian.Uint32(row[40:])
			kind := codec.Kind(row[44])
			if kind == "" || offset < 6 || size < codec.Header || size > codec.MaxRecord || offset > info.Size()-int64(size) {
				return errors.New("invalid pack index entry")
			}
			st.index[id] = location{pack: name, offset: offset, size: size, kind: kind}
		}
	}
	st.loaded = true
	s.consolidateLocked()
	return nil
}
func (s Store) Flush() error {
	if s.state == nil {
		return nil
	}
	st := s.state
	st.mu.Lock()
	defer st.mu.Unlock()
	return s.flushLocked()
}
func (s Store) flushLocked() error {
	st := s.state
	if len(st.pending) == 0 {
		return nil
	}
	if e := s.loadLocked(); e != nil {
		return e
	}
	dir := filepath.Join(s.Dir, "packs")
	if e := DurableMkdirAll(dir); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".write-pack-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	h := sha256.New()
	out := io.MultiWriter(f, h)
	if _, e = io.WriteString(out, packMagic); e != nil {
		return e
	}
	offset := int64(len(packMagic))
	ids := make([]string, 0, len(st.pending))
	for id := range st.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	index := make([]byte, 10, 10+len(ids)*45+32)
	copy(index, indexMagic)
	binary.BigEndian.PutUint32(index[6:], uint32(len(ids)))
	locations := map[string]location{}
	// Encode bases before acquiring this lock in PutWithBase; no storage reads
	// or recursive decoder calls occur while flushing the batch.
	for _, id := range ids {
		p := st.pending[id]
		record := p.encoded
		if record == nil {
			record, e = codec.Encode(p.object, nil)
			if e != nil {
				return e
			}
		}
		if _, e = out.Write(record); e != nil {
			return e
		}
		row := make([]byte, 45)
		raw, _ := hex.DecodeString(id)
		copy(row, raw)
		binary.BigEndian.PutUint64(row[32:], uint64(offset))
		binary.BigEndian.PutUint32(row[40:], uint32(len(record)))
		row[44] = record[32]
		index = append(index, row...)
		locations[id] = location{offset: offset, size: uint32(len(record)), kind: p.object.Type}
		offset += int64(len(record))
	}
	if e = f.Chmod(0600); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	name := hex.EncodeToString(h.Sum(nil))
	packFile := filepath.Join(dir, name+".pack")
	if e = os.Rename(tmp, packFile); e != nil {
		return e
	}
	if e = SyncDir(dir); e != nil {
		return e
	}
	if st.beforePublish != nil {
		if e = st.beforePublish(); e != nil {
			return e
		}
	}
	sum := sha256.Sum256(index)
	index = append(index, sum[:]...)
	if e = Atomic(filepath.Join(dir, name+".idx"), index, 0600); e != nil {
		return e
	}
	if e = SyncDir(s.Dir); e != nil {
		return e
	}
	if e = SyncDir(filepath.Dir(s.Dir)); e != nil {
		return e
	}
	for id, loc := range locations {
		loc.pack = name
		st.index[id] = loc
	}
	st.pending = map[string]pendingObject{}
	st.bytes = 0
	st.packs[name] = true
	s.consolidateLocked()
	return nil
}
func (s Store) PutWithBase(o model.Object, baseID string) (string, error) {
	if s.state == nil {
		temp := New(s.Dir)
		id, e := temp.PutWithBase(o, baseID)
		if e == nil {
			e = temp.Flush()
		}
		return id, e
	}
	id := o.ID()
	if s.Exists(id) {
		return id, nil
	}
	var base *model.Object
	if baseID != "" && baseID != id && o.Type == "blob" {
		if old, e := s.Get(baseID); e == nil && old.Type == "blob" {
			s.state.mu.Lock()
			depth := s.state.depth[baseID]
			s.state.mu.Unlock()
			if depth < codec.MaxDepth {
				base = &old
			}
		}
	}
	// Encode validates the complete model before this record can enter pending
	// storage. An existing ID already identifies the same validated payload.
	record, e := codec.Encode(o, base)
	if e != nil {
		return "", e
	}
	return s.putEncoded(o, record)
}
func (s Store) putEncoded(o model.Object, record []byte) (string, error) {
	if s.state == nil {
		temp := New(s.Dir)
		id, e := temp.putEncoded(o, record)
		if e == nil {
			e = temp.Flush()
		}
		return id, e
	}
	st := s.state
	// Both callers reach here only after Encode or Decode has checked the full
	// payload and hash. Reuse that record's ID rather than hashing it again.
	id, kind, base, e := codec.Inspect(record)
	if e != nil {
		return "", e
	}
	if kind != o.Type {
		return "", errors.New("record type does not match object")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := s.loadLocked(); e != nil {
		return "", e
	}
	if _, ok := st.locate(id); ok {
		return id, nil
	}
	if _, ok := st.pending[id]; ok {
		return id, nil
	}
	st.pending[id] = pendingObject{object: o, encoded: record}
	if base != "" {
		st.depth[id] = st.depth[base] + 1
	} else {
		st.depth[id] = 0
	}
	st.bytes += len(o.Data) + len(record)
	if len(st.pending) >= 4096 || st.bytes >= pendingLimit {
		if e := s.flushLocked(); e != nil {
			return "", e
		}
	}
	return id, nil
}
func DurableMkdirAll(dir string) error {
	var missing []string
	cur := dir
	for {
		if _, e := os.Stat(cur); e == nil {
			break
		} else if !os.IsNotExist(e) {
			return e
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	for _, d := range missing {
		if e := SyncDir(d); e != nil {
			return e
		}
		if e := SyncDir(filepath.Dir(d)); e != nil {
			return e
		}
	}
	return nil
}
func (s Store) Import(record []byte) (string, error) {
	_, e := s.ImportObject(record)
	if e != nil {
		return "", e
	}
	id, _, _, e := codec.Inspect(record)
	return id, e
}

// ImportObject returns the already verified payload so a streaming checkout can
// consume it without reading and decoding the newly stored record again.
func (s Store) ImportObject(record []byte) (model.Object, error) {
	if s.state == nil {
		temp := New(s.Dir)
		o, e := temp.ImportObject(record)
		if e == nil {
			e = temp.Flush()
		}
		return o, e
	}
	o, e := codec.Decode(record, s.Get)
	if e != nil {
		return model.Object{}, e
	}
	_, _, base, _ := codec.Inspect(record)
	if base != "" {
		s.state.mu.Lock()
		depth := s.state.depth[base]
		s.state.mu.Unlock()
		if depth >= codec.MaxDepth {
			record, e = codec.Encode(o, nil)
			if e != nil {
				return model.Object{}, e
			}
		}
	}
	_, e = s.putEncoded(o, record)
	return o, e
}
func (s Store) Export(id, baseID string) ([]byte, error) {
	o, e := s.Get(id)
	if e != nil {
		return nil, e
	}
	var base *model.Object
	if baseID != "" && baseID != id {
		if b, e := s.Get(baseID); e == nil && b.Type == "blob" {
			base = &b
		}
	}
	return codec.Encode(o, base)
}
func (s Store) get(id string, depth int, verify bool) (model.Object, error) {
	if depth > codec.MaxDepth {
		return model.Object{}, errors.New("delta chain exceeds limit")
	}
	if !model.ValidID(id) {
		return model.Object{}, errors.New("invalid object ID")
	}
	if s.state == nil {
		s = New(s.Dir)
	}
	st := s.state
	st.mu.Lock()
	if e := s.loadLocked(); e != nil {
		st.mu.Unlock()
		return model.Object{}, e
	}
	if !verify {
		if p, ok := st.pending[id]; ok {
			if depth+st.depth[id] > codec.MaxDepth {
				st.mu.Unlock()
				return model.Object{}, errors.New("delta chain exceeds limit")
			}
			st.mu.Unlock()
			return p.object, nil
		}
		if c, ok := st.cache[id]; ok {
			st.mu.Unlock()
			return c.object, nil
		}
	}
	loc, packed := st.locate(id)
	st.mu.Unlock()
	var o model.Object
	var e error
	chain := 0
	if packed {
		f, err := os.Open(filepath.Join(s.Dir, "packs", loc.pack+".pack"))
		if err != nil {
			return o, err
		}
		b := make([]byte, int(loc.size))
		_, e = f.ReadAt(b, loc.offset)
		f.Close()
		if e != nil {
			return o, e
		}
		encodedID, typ, base, e := codec.Inspect(b)
		if e != nil {
			return o, e
		}
		if encodedID != id || typ != loc.kind {
			return o, errors.New("pack index does not match its record")
		}
		o, e = codec.Decode(b, func(baseID string) (model.Object, error) {
			baseObject, e := s.get(baseID, depth+1, verify)
			st.mu.Lock()
			chain = st.depth[baseID] + 1
			st.mu.Unlock()
			return baseObject, e
		})
		if e != nil {
			return o, e
		}
		if base == "" {
			chain = 0
		}
	} else {
		o, e = s.getLoose(id)
		if e != nil {
			return o, e
		}
	}
	st.mu.Lock()
	st.depth[id] = chain
	if o.Type != "blob" && !verify {
		if len(st.cache) >= 2048 || st.cacheBytes+len(o.Data) > 8<<20 {
			st.cache = map[string]decoded{}
			st.cacheBytes = 0
		}
		c := decoded{object: o}
		if o.Type == "tree" {
			t, _ := o.Tree()
			c.tree = &t
		} else {
			cm, _ := o.Commit()
			c.commit = &cm
		}
		st.cache[id] = c
		st.cacheBytes += len(o.Data)
	}
	st.mu.Unlock()
	return o, nil
}
func (s Store) Verify(id string) (model.Object, error) { return s.get(id, 0, true) }
func (s Store) Kind(id string) (string, error) {
	if s.state == nil {
		s = New(s.Dir)
	}
	st := s.state
	st.mu.Lock()
	if e := s.loadLocked(); e != nil {
		st.mu.Unlock()
		return "", e
	}
	if p, ok := st.pending[id]; ok {
		st.mu.Unlock()
		return p.object.Type, nil
	}
	if loc, ok := st.locate(id); ok {
		st.mu.Unlock()
		return loc.kind, nil
	}
	st.mu.Unlock()
	o, e := s.getLoose(id)
	return o.Type, e
}
func (s Store) Tree(id string) (model.Tree, error) {
	if s.state == nil {
		s = New(s.Dir)
	}
	o, e := s.Get(id)
	if e != nil {
		return model.Tree{}, e
	}
	if o.Type != "tree" {
		return model.Tree{}, errors.New("expected tree")
	}
	s.state.mu.Lock()
	c, ok := s.state.cache[id]
	s.state.mu.Unlock()
	if ok && c.tree != nil {
		return *c.tree, nil
	}
	return o.Tree()
}
