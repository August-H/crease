package objectstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"crease/internal/model"
)

type Store struct {
	Dir   string
	state *state
}
type File struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}
type Snapshot map[string]File

func SyncDir(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func Atomic(file string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(file)
	if err := DurableMkdirAll(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, file); err != nil {
		return err
	}
	return SyncDir(dir)
}
func (s Store) file(id string) (string, error) {
	if !model.ValidID(id) {
		return "", errors.New("invalid object ID")
	}
	return filepath.Join(s.Dir, id[:2], id[2:]), nil
}
func (s Store) Get(id string) (model.Object, error) { return s.get(id, 0, false) }
func (s Store) getLoose(id string) (model.Object, error) {
	var o model.Object
	file, err := s.file(id)
	if err != nil {
		return o, err
	}
	f, err := os.Open(file)
	if err != nil {
		return o, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (model.MaxObject*2)+1))
	if err != nil {
		return o, err
	}
	if len(b) > model.MaxObject*2 {
		return o, errors.New("oversized stored object")
	}
	if err = json.Unmarshal(b, &o); err != nil {
		return o, err
	}
	if err = o.Validate(); err != nil {
		return o, err
	}
	if o.ID() != id {
		return o, errors.New("object integrity check failed")
	}
	return o, nil
}
func (s Store) Has(id string) bool { _, err := s.Get(id); return err == nil }

// Exists checks validated pack indexes or legacy loose objects without decoding
// packed payloads. Use Get or Verify when payload integrity must be checked.
func (s Store) Exists(id string) bool              { _, e := s.Kind(id); return e == nil }
func (s Store) Put(o model.Object) (string, error) { return s.PutWithBase(o, "") }
func (s Store) PutJSON(kind string, v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	// Put's encoder checks canonical payloads and paths before publication.
	// Validating here as well would decode and re-encode every changed tree twice.
	return s.Put(model.Object{Type: kind, Data: b})
}
func (s Store) Commit(id string) (model.Commit, error) {
	if s.state == nil {
		s = New(s.Dir)
	}
	o, e := s.Get(id)
	if e != nil {
		return model.Commit{}, e
	}
	if o.Type != "commit" {
		return model.Commit{}, errors.New("expected commit")
	}
	s.state.mu.Lock()
	c, ok := s.state.cache[id]
	s.state.mu.Unlock()
	if ok && c.commit != nil {
		return *c.commit, nil
	}
	return o.Commit()
}

func (s Store) Snapshot(id string) (Snapshot, error) {
	out := Snapshot{}
	if id == "" {
		return out, nil
	}
	c, err := s.Commit(id)
	if err != nil {
		return nil, err
	}
	var walk func(string, string, int) error
	walk = func(id, prefix string, depth int) error {
		if depth > 100 {
			return errors.New("tree nesting exceeds limit")
		}
		t, e := s.Tree(id)
		if e != nil {
			return e
		}
		for _, entry := range t.Entries {
			p := path.Join(prefix, entry.Name)
			if !model.ValidPath(p) {
				return errors.New("unsafe snapshot path")
			}
			if entry.Mode == "tree" {
				if e := walk(entry.ID, p, depth+1); e != nil {
					return e
				}
			} else {
				out[p] = File{ID: entry.ID, Mode: entry.Mode}
				if len(out) > 1_000_000 {
					return errors.New("snapshot exceeds path limit")
				}
			}
		}
		return nil
	}
	err = walk(c.Tree, "", 0)
	return out, err
}

func (s Store) WriteTree(files Snapshot) (string, error) {
	type node struct {
		files map[string]File
		dirs  map[string]*node
	}
	root := &node{files: map[string]File{}, dirs: map[string]*node{}}
	for p, f := range files {
		if !model.ValidPath(p) {
			return "", fmt.Errorf("unsafe path %q", p)
		}
		parts := strings.Split(p, "/")
		n := root
		for _, part := range parts[:len(parts)-1] {
			if _, ok := n.files[part]; ok {
				return "", errors.New("file/directory collision")
			}
			if n.dirs[part] == nil {
				n.dirs[part] = &node{files: map[string]File{}, dirs: map[string]*node{}}
			}
			n = n.dirs[part]
		}
		name := parts[len(parts)-1]
		if n.dirs[name] != nil {
			return "", errors.New("file/directory collision")
		}
		n.files[name] = f
	}
	var write func(*node) (string, error)
	write = func(n *node) (string, error) {
		t := model.Tree{Entries: []model.Entry{}}
		for name, f := range n.files {
			t.Entries = append(t.Entries, model.Entry{Name: name, Mode: f.Mode, ID: f.ID})
		}
		for name, child := range n.dirs {
			id, e := write(child)
			if e != nil {
				return "", e
			}
			t.Entries = append(t.Entries, model.Entry{Name: name, Mode: "tree", ID: id})
		}
		model.Sort(t.Entries)
		return s.PutJSON("tree", t)
	}
	return write(root)
}

// Graph reads a complete graph, including blobs. Server publication uses
// Validate to reuse persisted closure proofs instead of walking old history.
func (s Store) Graph(heads []string) ([]string, error) {
	type want struct {
		id, kind string
		depth    int
	}
	queue := []want{}
	for _, h := range heads {
		if h != "" {
			queue = append(queue, want{h, "commit", 0})
		}
	}
	seen := map[string]string{}
	var ids []string
	for len(queue) > 0 {
		w := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if kind, ok := seen[w.id]; ok {
			if kind != w.kind {
				return nil, errors.New("inconsistent object type")
			}
			continue
		}
		if len(seen) > 2_000_000 {
			return nil, errors.New("graph exceeds object limit")
		}
		if w.depth > 100 {
			return nil, errors.New("tree nesting exceeds limit")
		}
		o, e := s.Get(w.id)
		if e != nil {
			return nil, e
		}
		if o.Type != w.kind {
			return nil, errors.New("wrong referenced object type")
		}
		seen[w.id] = w.kind
		ids = append(ids, w.id)
		switch o.Type {
		case "commit":
			c, _ := o.Commit()
			queue = append(queue, want{c.Tree, "tree", 0})
			for _, p := range c.Parents {
				queue = append(queue, want{p, "commit", 0})
			}
		case "tree":
			t, _ := o.Tree()
			for _, entry := range t.Entries {
				kind := "blob"
				depth := 0
				if entry.Mode == "tree" {
					kind = "tree"
					depth = w.depth + 1
				}
				queue = append(queue, want{entry.ID, kind, depth})
			}
		}
	}
	return ids, nil
}

func (s Store) Ancestors(head string) (map[string]int, error) {
	out := map[string]int{}
	if head == "" {
		return out, nil
	}
	queue := []string{head}
	out[head] = 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		c, e := s.Commit(id)
		if e != nil {
			return nil, e
		}
		for _, p := range c.Parents {
			if _, ok := out[p]; !ok {
				out[p] = out[id] + 1
				queue = append(queue, p)
			}
		}
		if len(out) > 1_000_000 {
			return nil, errors.New("history exceeds limit")
		}
	}
	return out, nil
}
func (s Store) IsAncestor(old, head string) (bool, error) {
	if old == "" {
		return true, nil
	}
	stack := []string{head}
	seen := map[string]bool{}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == old {
			return true, nil
		}
		if id == "" || seen[id] {
			continue
		}
		if len(seen) > 1_000_000 {
			return false, errors.New("history exceeds limit")
		}
		seen[id] = true
		c, e := s.Commit(id)
		if e != nil {
			return false, e
		}
		stack = append(stack, c.Parents...)
	}
	return false, nil
}
func (s Store) MergeBase(a, b string) (string, error) {
	aa, e := s.Ancestors(a)
	if e != nil {
		return "", e
	}
	bb, e := s.Ancestors(b)
	if e != nil {
		return "", e
	}
	var common []string
	for id := range aa {
		if _, ok := bb[id]; ok {
			common = append(common, id)
		}
	}
	if len(common) == 0 {
		return "", errors.New("histories have no common ancestor")
	}
	// A parent of any common ancestor cannot be a best common ancestor.
	// All of its ancestors are also common, so one pass marks every older node.
	older := map[string]bool{}
	for _, id := range common {
		c, err := s.Commit(id)
		if err != nil {
			return "", err
		}
		for _, parent := range c.Parents {
			older[parent] = true
		}
	}
	var best []string
	for _, id := range common {
		if !older[id] {
			best = append(best, id)
		}
	}
	if len(best) != 1 {
		return "", errors.New("multiple merge bases require manual resolution")
	}
	sort.Strings(best)
	return best[0], nil
}

func Ref(db *sql.DB, name string) (string, error) {
	var head string
	e := db.QueryRow(`SELECT head FROM branches WHERE name=?`, name).Scan(&head)
	return head, e
}
