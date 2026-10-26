package diff

import (
	"bytes"
	"sort"

	"crease/internal/objectstore"
)

type Stat struct {
	Path                    string
	Insertions, Deletions   int
	Binary                  bool
	BeforeBytes, AfterBytes int
}

func StatsWith(a, b objectstore.Snapshot, read func(string, objectstore.File, bool) ([]byte, error)) ([]Stat, error) {
	changes := objectstore.FileChanges(a, b)
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]Stat, 0, len(paths))
	for _, p := range paths {
		before, had := a[p]
		after, has := b[p]
		stat := Stat{Path: p}
		if had && has && before.ID == after.ID {
			out = append(out, stat)
			continue
		}
		var left, right []byte
		if had {
			data, err := read(p, before, false)
			if err != nil {
				return nil, err
			}
			left = data
		}
		if has {
			data, err := read(p, after, true)
			if err != nil {
				return nil, err
			}
			right = data
		}
		stat.BeforeBytes, stat.AfterBytes = len(left), len(right)
		if bytes.IndexByte(left, 0) >= 0 || bytes.IndexByte(right, 0) >= 0 {
			stat.Binary = true
		} else {
			al, bl := lines(left), lines(right)
			ed, err := edits(al, bl)
			if err != nil {
				stat.Deletions, stat.Insertions = len(al), len(bl)
			} else {
				for _, e := range ed {
					stat.Deletions += e.end - e.start
					stat.Insertions += len(e.lines)
				}
			}
		}
		out = append(out, stat)
	}
	return out, nil
}
