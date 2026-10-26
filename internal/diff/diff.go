// Package diff implements bounded line comparison and conservative three-way merges.
package diff

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"crease/internal/model"
	"crease/internal/objectstore"
)

type edit struct {
	start, end int
	lines      []string
}
type ConflictError struct{ Paths []string }

func (e *ConflictError) Error() string { return "merge conflicts: " + strings.Join(e.Paths, ", ") }

func lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	out := strings.SplitAfter(string(b), "\n")
	if out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
func Text(base, left, right []byte) ([]byte, error) {
	if bytes.Equal(left, right) {
		return left, nil
	}
	if bytes.Equal(base, left) {
		return right, nil
	}
	if bytes.Equal(base, right) {
		return left, nil
	}
	if bytes.IndexByte(base, 0) >= 0 || bytes.IndexByte(left, 0) >= 0 || bytes.IndexByte(right, 0) >= 0 {
		return nil, errors.New("binary conflict")
	}
	a := lines(base)
	l, e := edits(a, lines(left))
	if e != nil {
		return nil, e
	}
	r, e := edits(a, lines(right))
	if e != nil {
		return nil, e
	}
	var all []edit
	all = append(all, l...)
	for _, re := range r {
		duplicate := false
		for _, le := range l {
			if le.start == re.start && le.end == re.end && strings.Join(le.lines, "") == strings.Join(re.lines, "") {
				duplicate = true
				break
			}
			overlap := le.start < re.end && re.start < le.end
			if le.start == le.end {
				overlap = le.start >= re.start && le.start <= re.end
			}
			if re.start == re.end {
				overlap = overlap || (re.start >= le.start && re.start <= le.end)
			}
			if overlap {
				return nil, errors.New("overlapping text edits")
			}
		}
		if !duplicate {
			all = append(all, re)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })
	var out []string
	pos := 0
	for _, ed := range all {
		out = append(out, a[pos:ed.start]...)
		out = append(out, ed.lines...)
		pos = ed.end
	}
	out = append(out, a[pos:]...)
	return []byte(strings.Join(out, "")), nil
}

func Snapshots(store objectstore.Store, base, left, right objectstore.Snapshot) (objectstore.Snapshot, error) {
	out := objectstore.Snapshot{}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range left {
		paths[p] = true
	}
	for p := range right {
		paths[p] = true
	}
	var conflicts []string
	for p := range paths {
		b, bok := base[p]
		l, lok := left[p]
		r, rok := right[p]
		if l == r && lok == rok {
			if lok {
				out[p] = l
			}
			continue
		}
		if l == b && lok == bok {
			if rok {
				out[p] = r
			}
			continue
		}
		if r == b && rok == bok {
			if lok {
				out[p] = l
			}
			continue
		}
		if !bok || !lok || !rok {
			conflicts = append(conflicts, p)
			continue
		}
		mode := l.Mode
		if l.Mode != r.Mode {
			if l.Mode == b.Mode {
				mode = r.Mode
			} else if r.Mode != b.Mode {
				conflicts = append(conflicts, p)
				continue
			}
		}
		bo, e := store.Get(b.ID)
		if e != nil {
			return nil, e
		}
		lo, e := store.Get(l.ID)
		if e != nil {
			return nil, e
		}
		ro, e := store.Get(r.ID)
		if e != nil {
			return nil, e
		}
		data, e := Text(bo.Data, lo.Data, ro.Data)
		if e != nil {
			conflicts = append(conflicts, p)
			continue
		}
		id, e := store.Put(modelBlob(data))
		if e != nil {
			return nil, e
		}
		out[p] = objectstore.File{ID: id, Mode: mode}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return out, &ConflictError{Paths: conflicts}
	}
	return out, nil
}

func Unified(store objectstore.Store, a, b objectstore.Snapshot) (string, error) {
	return UnifiedWith(a, b, func(_ string, f objectstore.File, _ bool) ([]byte, error) { o, e := store.Get(f.ID); return o.Data, e })
}
func UnifiedWith(a, b objectstore.Snapshot, read func(string, objectstore.File, bool) ([]byte, error)) (string, error) {
	paths := map[string]bool{}
	for p := range a {
		paths[p] = true
	}
	for p := range b {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out strings.Builder
	for _, p := range sorted {
		af, aok := a[p]
		bf, bok := b[p]
		if af == bf && aok == bok {
			continue
		}
		fmt.Fprintf(&out, "diff --git %s %s\n", patchPath("a/"+p), patchPath("b/"+p))
		switch {
		case !aok:
			fmt.Fprintf(&out, "new file mode %s\n", patchMode(bf.Mode))
		case !bok:
			fmt.Fprintf(&out, "deleted file mode %s\n", patchMode(af.Mode))
		case af.Mode != bf.Mode:
			fmt.Fprintf(&out, "old mode %s\nnew mode %s\n", patchMode(af.Mode), patchMode(bf.Mode))
		}
		if aok && bok && af.ID == bf.ID {
			if out.Len() > 4<<20 {
				return "", errors.New("diff exceeds 4 MiB output limit")
			}
			continue
		}
		oldID, newID := "0000000", "0000000"
		if aok && model.ValidID(af.ID) {
			oldID = af.ID[:7]
		}
		if bok && model.ValidID(bf.ID) {
			newID = bf.ID[:7]
		}
		if (!aok || model.ValidID(af.ID)) && (!bok || model.ValidID(bf.ID)) {
			fmt.Fprintf(&out, "index %s..%s", oldID, newID)
			if aok && bok && af.Mode == bf.Mode {
				fmt.Fprintf(&out, " %s", patchMode(af.Mode))
			}
			out.WriteByte('\n')
		}
		var ab, bb []byte
		if aok {
			data, e := read(p, af, false)
			if e != nil {
				return "", e
			}
			ab = data
		}
		if bok {
			data, e := read(p, bf, true)
			if e != nil {
				return "", e
			}
			bb = data
		}
		an, bn := "a/"+p, "b/"+p
		if !aok {
			an = "/dev/null"
		}
		if !bok {
			bn = "/dev/null"
		}
		if bytes.IndexByte(ab, 0) >= 0 || bytes.IndexByte(bb, 0) >= 0 {
			fmt.Fprintf(&out, "Binary files %s and %s differ\n", patchPath(an), patchPath(bn))
		} else {
			if len(ab) != 0 || len(bb) != 0 {
				fmt.Fprintf(&out, "--- %s\n+++ %s\n", patchPath(an), patchPath(bn))
				al, bl := lines(ab), lines(bb)
				ed, e := edits(al, bl)
				if e != nil {
					ed = []edit{{start: 0, end: len(al), lines: bl}}
				}
				emitHunks(&out, al, bl, ed)
			}
		}
		if out.Len() > 4<<20 {
			return "", errors.New("diff exceeds 4 MiB output limit")
		}
	}
	return out.String(), nil
}
func patchMode(mode string) string {
	if mode == "exec" {
		return "100755"
	}
	return "100644"
}
func patchPath(path string) string { return QuotePath(path) }

// QuotePath uses Git's C-style byte quoting, including octal UTF-8 bytes.
func QuotePath(path string) string {
	quoted := false
	for _, b := range []byte(path) {
		if b <= 32 || b >= 127 || b == '"' || b == '\\' {
			quoted = true
			break
		}
	}
	if !quoted {
		return path
	}
	var out strings.Builder
	out.WriteByte('"')
	for _, b := range []byte(path) {
		switch {
		case b == '"' || b == '\\':
			out.WriteByte('\\')
			out.WriteByte(b)
		case b == '\n':
			out.WriteString(`\n`)
		case b == '\t':
			out.WriteString(`\t`)
		case b == '\r':
			out.WriteString(`\r`)
		case b < 32 || b >= 127:
			fmt.Fprintf(&out, `\%03o`, b)
		default:
			out.WriteByte(b)
		}
	}
	out.WriteByte('"')
	return out.String()
}
func emit(out *strings.Builder, prefix, line string) {
	out.WriteString(prefix)
	out.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		out.WriteString("\n\\ No newline at end of file\n")
	}
}
func modelBlob(b []byte) model.Object { return model.Object{Type: "blob", Data: b} }
