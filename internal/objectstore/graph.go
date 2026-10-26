package objectstore

import (
	"crease/internal/model"
	"errors"
)

// Proof describes a graph closure previously validated by this repository's
// server, never a client's claim. Tree height preserves nesting limits when a
// validated subtree is reused deeper in a new tree.
type Proof struct {
	Kind   string
	Height int
}
type ProofLookup func(string) (Proof, bool, error)
type edge struct {
	id, kind string
	depth    int
}

func (s Store) Validate(head string, known ProofLookup) (map[string]Proof, error) {
	type task struct {
		edge
		exit     bool
		children []edge
	}
	stack := []task{{edge: edge{head, "commit", 0}}}
	done := map[string]Proof{}
	fresh := map[string]Proof{}
	active := map[string]bool{}
	check := func(p Proof, w edge) error {
		if p.Kind != w.kind {
			return errors.New("wrong referenced object type")
		}
		if w.depth+p.Height > 101 {
			return errors.New("tree nesting exceeds limit")
		}
		return nil
	}
	for len(stack) > 0 {
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w.id == "" {
			continue
		}
		if len(done) > 2_000_000 {
			return nil, errors.New("graph exceeds object limit")
		}
		if w.exit {
			p := Proof{Kind: w.kind}
			if w.kind == "tree" {
				p.Height = 1
				for _, child := range w.children {
					if child.kind == "tree" && done[child.id].Height+1 > p.Height {
						p.Height = done[child.id].Height + 1
					}
				}
			}
			if e := check(p, w.edge); e != nil {
				return nil, e
			}
			done[w.id] = p
			fresh[w.id] = p
			delete(active, w.id)
			continue
		}
		if p, ok := done[w.id]; ok {
			if e := check(p, w.edge); e != nil {
				return nil, e
			}
			continue
		}
		if w.kind != "blob" && known != nil {
			p, ok, e := known(w.id)
			if e != nil {
				return nil, e
			}
			if ok {
				if e = check(p, w.edge); e != nil {
					return nil, e
				}
				done[w.id] = p
				continue
			}
		}
		if active[w.id] {
			return nil, errors.New("cyclic object graph")
		}
		typ, e := s.Kind(w.id)
		if e != nil {
			return nil, e
		}
		if typ != w.kind {
			return nil, errors.New("wrong referenced object type")
		}
		if typ == "blob" {
			done[w.id] = Proof{Kind: "blob"}
			continue
		}
		active[w.id] = true
		var children []edge
		if typ == "commit" {
			c, e := s.Commit(w.id)
			if e != nil {
				return nil, e
			}
			children = append(children, edge{c.Tree, "tree", 0})
			for _, p := range c.Parents {
				children = append(children, edge{p, "commit", 0})
			}
		} else {
			t, e := s.Tree(w.id)
			if e != nil {
				return nil, e
			}
			for _, en := range t.Entries {
				k := "blob"
				d := 0
				if en.Mode == "tree" {
					k = "tree"
					d = w.depth + 1
				}
				children = append(children, edge{en.ID, k, d})
			}
		}
		stack = append(stack, task{edge: w.edge, exit: true, children: children})
		for _, child := range children {
			stack = append(stack, task{edge: child})
		}
	}
	return fresh, nil
}

// Walk streams history without requiring all historical blobs. Stop commits are
// complete metadata frontiers already held by the receiver.
func (s Store) Walk(head string, stop map[string]bool, full bool, visit func(string, model.Object) error) error {
	stack := []edge{{head, "commit", 0}}
	seen := map[string]bool{}
	for len(stack) > 0 {
		w := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if w.id == "" || seen[w.id] || (w.kind == "commit" && stop[w.id]) {
			continue
		}
		if len(seen) > 2_000_000 || w.depth > 100 {
			return errors.New("history exceeds limits")
		}
		seen[w.id] = true
		o, e := s.Get(w.id)
		if e != nil {
			return e
		}
		if o.Type != w.kind {
			return errors.New("wrong referenced object type")
		}
		if e = visit(w.id, o); e != nil {
			return e
		}
		switch w.kind {
		case "commit":
			c, e := s.Commit(w.id)
			if e != nil {
				return e
			}
			for _, p := range c.Parents {
				stack = append(stack, edge{p, "commit", 0})
			}
			stack = append(stack, edge{c.Tree, "tree", 0})
		case "tree":
			t, e := s.Tree(w.id)
			if e != nil {
				return e
			}
			for _, en := range t.Entries {
				if en.Mode == "tree" {
					stack = append(stack, edge{en.ID, "tree", w.depth + 1})
				} else if full {
					stack = append(stack, edge{en.ID, "blob", 0})
				}
			}
		}
	}
	return nil
}
