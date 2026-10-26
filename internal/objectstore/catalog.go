package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"crease/internal/codec"
	"crease/internal/model"
)

const catalogMagic = "CRMI\x00\x01"
const catalogWidth = 77 // ID32, pack32, offset8, length4, type1

func (st *state) locate(id string) (location, bool) {
	if loc, ok := st.index[id]; ok {
		return loc, true
	}
	raw, e := hex.DecodeString(id)
	if e != nil || len(raw) != 32 {
		return location{}, false
	}
	n := len(st.catalog) / catalogWidth
	i := sort.Search(n, func(i int) bool { return bytes.Compare(st.catalog[i*catalogWidth:i*catalogWidth+32], raw) >= 0 })
	if i == n {
		return location{}, false
	}
	row := st.catalog[i*catalogWidth : (i+1)*catalogWidth]
	if !bytes.Equal(row[:32], raw) {
		return location{}, false
	}
	return location{pack: hex.EncodeToString(row[32:64]), offset: int64(binary.BigEndian.Uint64(row[64:])), size: binary.BigEndian.Uint32(row[72:]), kind: codec.Kind(row[76])}, true
}
func (s Store) loadCatalog(entries []os.DirEntry) {
	st := s.state
	st.packs = map[string]bool{}
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".idx") {
			st.packs[strings.TrimSuffix(en.Name(), ".idx")] = true
		}
	}
	b, e := os.ReadFile(filepath.Join(s.Dir, "pack-index"))
	if e != nil || len(b) < 46 || string(b[:6]) != catalogMagic {
		return
	}
	sum := sha256.Sum256(b[:len(b)-32])
	if !bytes.Equal(sum[:], b[len(b)-32:]) {
		return
	}
	packs, count := int(binary.BigEndian.Uint32(b[6:])), int(binary.BigEndian.Uint32(b[10:]))
	if packs > 1_000_000 || count > 2_000_000 || len(b) != 14+packs*32+count*catalogWidth+32 {
		return
	}
	known := map[string]bool{}
	for i := 0; i < packs; i++ {
		name := hex.EncodeToString(b[14+i*32 : 14+(i+1)*32])
		if !st.packs[name] {
			return
		}
		known[name] = true
	}
	rows := b[14+packs*32 : len(b)-32]
	for i := 0; i < count; i++ {
		row := rows[i*catalogWidth : (i+1)*catalogWidth]
		if !known[hex.EncodeToString(row[32:64])] || codec.Kind(row[76]) == "" || binary.BigEndian.Uint64(row[64:]) < 6 || binary.BigEndian.Uint32(row[72:]) < codec.Header || binary.BigEndian.Uint32(row[72:]) > codec.MaxRecord {
			return
		}
		if i > 0 && bytes.Compare(rows[(i-1)*catalogWidth:(i-1)*catalogWidth+32], row[:32]) >= 0 {
			return
		}
	}
	st.catalog = rows
	st.catalogPacks = known
}
func (s Store) consolidateLocked() {
	st := s.state
	if st.catalog != nil && len(st.packs)-len(st.catalogPacks) < 32 {
		return
	}
	if len(st.packs) == 0 {
		return
	}
	// Consolidation is infrequent. Lookups between consolidations search the
	// sorted binary table plus the small map of newer packs.
	ids := make([]string, 0, len(st.catalog)/catalogWidth+len(st.index))
	seen := map[string]bool{}
	for i := 0; i < len(st.catalog); i += catalogWidth {
		id := hex.EncodeToString(st.catalog[i : i+32])
		ids = append(ids, id)
		seen[id] = true
	}
	for id := range st.index {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	names := make([]string, 0, len(st.packs))
	for name := range st.packs {
		names = append(names, name)
	}
	sort.Strings(names)
	b := make([]byte, 14, 14+len(names)*32+len(ids)*catalogWidth+32)
	copy(b, catalogMagic)
	binary.BigEndian.PutUint32(b[6:], uint32(len(names)))
	binary.BigEndian.PutUint32(b[10:], uint32(len(ids)))
	for _, name := range names {
		raw, _ := hex.DecodeString(name)
		b = append(b, raw...)
	}
	start := len(b)
	for _, id := range ids {
		loc, _ := st.locate(id)
		if !model.ValidID(loc.pack) {
			return
		}
		row := make([]byte, catalogWidth)
		raw, _ := hex.DecodeString(id)
		copy(row, raw)
		raw, _ = hex.DecodeString(loc.pack)
		copy(row[32:], raw)
		binary.BigEndian.PutUint64(row[64:], uint64(loc.offset))
		binary.BigEndian.PutUint32(row[72:], loc.size)
		switch loc.kind {
		case "blob":
			row[76] = 1
		case "tree":
			row[76] = 2
		case "commit":
			row[76] = 3
		}
		b = append(b, row...)
	}
	end := len(b)
	sum := sha256.Sum256(b)
	b = append(b, sum[:]...)
	_ = CacheFile(filepath.Join(s.Dir, "pack-index"), b)
	st.catalog = b[start:end]
	st.catalogPacks = map[string]bool{}
	for _, name := range names {
		st.catalogPacks[name] = true
	}
	st.index = map[string]location{}
}
