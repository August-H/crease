package objectstore

import (
	"errors"
	"path"
	"strings"

	"crease/internal/model"
)

// A missing ID is a deletion. Only changed paths enter this map.
func FileChanges(base, next Snapshot) Snapshot {
	out := Snapshot{}
	for p, f := range next {
		if old, ok := base[p]; !ok || old != f {
			out[p] = f
		}
	}
	for p := range base {
		if _, ok := next[p]; !ok {
			out[p] = File{}
		}
	}
	return out
}

// UpdateTree rewrites the ancestors of changed leaves. Unchanged subtrees are
// neither decoded nor serialized. Empty directories disappear as in WriteTree.
func (s Store) UpdateTree(root string, changes Snapshot) (string, error) {
	if len(changes) == 0 && root != "" {
		return root, nil
	}
	type update struct {
		files Snapshot
		dirs  map[string]*update
	}
	newUpdate := func() *update { return &update{files: Snapshot{}, dirs: map[string]*update{}} }
	u := newUpdate()
	for p, f := range changes {
		if !model.ValidPath(p) {
			return "", errors.New("unsafe update path")
		}
		parts := strings.Split(p, "/")
		cur := u
		for _, part := range parts[:len(parts)-1] {
			if cur.dirs[part] == nil {
				cur.dirs[part] = newUpdate()
			}
			cur = cur.dirs[part]
		}
		cur.files[parts[len(parts)-1]] = f
	}
	var write func(string, *update, int) (string, bool, error)
	write = func(id string, u *update, depth int) (string, bool, error) {
		if depth > 100 {
			return "", false, errors.New("tree nesting exceeds limit")
		}
		entries := map[string]model.Entry{}
		if id != "" {
			t, e := s.Tree(id)
			if e != nil {
				return "", false, e
			}
			for _, en := range t.Entries {
				entries[en.Name] = en
			}
		}
		for name, f := range u.files {
			if u.dirs[name] != nil {
				return "", false, errors.New("file/directory collision")
			}
			if f.ID == "" {
				delete(entries, name)
			} else {
				if en, ok := entries[name]; ok && en.Mode == "tree" {
					return "", false, errors.New("file/directory transitions are unsupported")
				}
				entries[name] = model.Entry{Name: name, Mode: f.Mode, ID: f.ID}
			}
		}
		for name, child := range u.dirs {
			old := entries[name]
			if old.ID != "" && old.Mode != "tree" {
				return "", false, errors.New("file/directory transitions are unsupported")
			}
			next, empty, e := write(old.ID, child, depth+1)
			if e != nil {
				return "", false, e
			}
			if empty {
				delete(entries, name)
			} else {
				entries[name] = model.Entry{Name: name, Mode: "tree", ID: next}
			}
		}
		t := model.Tree{Entries: []model.Entry{}}
		for _, en := range entries {
			t.Entries = append(t.Entries, en)
		}
		model.Sort(t.Entries)
		next, e := s.PutJSON("tree", t)
		return next, len(entries) == 0, e
	}
	id, _, e := write(root, u, 0)
	return id, e
}

// ChangedSnapshots compares tree IDs before descending. Returned snapshots
// contain only differing file entries, suitable for fetching diff inputs.
func (s Store) ChangedSnapshots(a, b string) (Snapshot, Snapshot, error) {
	left, right := Snapshot{}, Snapshot{}
	root := func(head string) (string, error) {
		if head == "" {
			return "", nil
		}
		c, e := s.Commit(head)
		return c.Tree, e
	}
	ar, e := root(a)
	if e != nil {
		return nil, nil, e
	}
	br, e := root(b)
	if e != nil {
		return nil, nil, e
	}
	var walk func(string, string, string, int) error
	walk = func(ai, bi, prefix string, depth int) error {
		if ai == bi {
			return nil
		}
		if depth > 100 {
			return errors.New("tree nesting exceeds limit")
		}
		am, bm := map[string]model.Entry{}, map[string]model.Entry{}
		for _, side := range []struct {
			id      string
			entries map[string]model.Entry
		}{{ai, am}, {bi, bm}} {
			if side.id != "" {
				t, e := s.Tree(side.id)
				if e != nil {
					return e
				}
				for _, en := range t.Entries {
					side.entries[en.Name] = en
				}
			}
		}
		names := map[string]bool{}
		for name := range am {
			names[name] = true
		}
		for name := range bm {
			names[name] = true
		}
		for name := range names {
			a, b := am[name], bm[name]
			if a == b {
				continue
			}
			p := path.Join(prefix, name)
			if a.Mode == "tree" || b.Mode == "tree" {
				aa, bb := "", ""
				if a.Mode == "tree" {
					aa = a.ID
				}
				if b.Mode == "tree" {
					bb = b.ID
				}
				if e := walk(aa, bb, p, depth+1); e != nil {
					return e
				}
			}
			if a.ID != "" && a.Mode != "tree" {
				left[p] = File{ID: a.ID, Mode: a.Mode}
			}
			if b.ID != "" && b.Mode != "tree" {
				right[p] = File{ID: b.ID, Mode: b.Mode}
			}
		}
		return nil
	}
	e = walk(ar, br, "", 0)
	return left, right, e
}
