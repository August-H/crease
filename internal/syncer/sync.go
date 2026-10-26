package syncer

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"crease/internal/codec"
	"crease/internal/model"
	"crease/internal/objectstore"
	"crease/internal/protocol"
	"crease/internal/transport"
	"crease/internal/workspace"
)

func Download(c *transport.Client, store objectstore.Store, ids []string, hints ...map[string]string) error {
	var missing []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !model.ValidID(id) {
			return errors.New("invalid object ID")
		}
		if !seen[id] && !store.Has(id) {
			missing = append(missing, id)
			seen[id] = true
		}
	}
	for len(missing) > 0 {
		n := min(protocol.MaxBatch, len(missing))
		batch := missing[:n]
		if c.Capabilities["binary-stream-v1"] {
			allowed := map[string]bool{}
			for _, id := range batch {
				allowed[id] = true
			}
			bases := map[string]string{}
			if len(hints) > 0 {
				for _, id := range batch {
					if base := hints[0][id]; base != "" && base != id && store.Exists(base) {
						bases[id] = base
					}
				}
			}
			_, e := c.Receive(protocol.Request{Op: "get_stream", IDs: batch, Bases: bases}, func(record []byte) error {
				id, _, _, e := codec.Inspect(record)
				if e != nil {
					return e
				}
				if !allowed[id] {
					return errors.New("server returned an unexpected object")
				}
				delete(allowed, id)
				_, e = store.Import(record)
				return e
			})
			if e != nil {
				return e
			}
			if len(allowed) != 0 {
				return errors.New("server omitted requested objects")
			}
			missing = missing[n:]
			continue
		}
		res, e := c.Call(protocol.Request{Op: "get", IDs: batch})
		if e != nil {
			return e
		}
		if len(res.Objects) == 0 {
			return errors.New("server made no transfer progress")
		}
		allowed := map[string]bool{}
		for _, id := range batch {
			allowed[id] = true
		}
		for id, o := range res.Objects {
			if !allowed[id] || id != o.ID() {
				return errors.New("server returned unexpected object")
			}
			if _, e = store.Put(o); e != nil {
				return e
			}
		}
		remaining := make([]string, 0, len(missing)-len(res.Objects))
		for _, id := range batch {
			if _, ok := res.Objects[id]; !ok {
				remaining = append(remaining, id)
			}
		}
		missing = append(remaining, missing[n:]...)
	}
	return store.Flush()
}

// Inventory traverses all history metadata but does not require historical blobs
// to be cached locally. This supports incremental pushes from partial clones.
func Inventory(store objectstore.Store, head string) ([]string, error) {
	return inventorySince(store, head, nil, nil)
}
func inventorySince(store objectstore.Store, head string, stop, trees map[string]bool) ([]string, error) {
	queue := []struct {
		id, kind string
		depth    int
	}{{head, "commit", 0}}
	seen := map[string]string{}
	var out []string
	for len(queue) > 0 {
		w := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if w.id == "" || (w.kind == "commit" && stop[w.id]) || (w.kind == "tree" && trees[w.id]) {
			continue
		}
		if kind, ok := seen[w.id]; ok {
			if kind != w.kind {
				return nil, errors.New("inconsistent object type")
			}
			continue
		}
		if len(seen) > 2_000_000 || w.depth > 100 {
			return nil, errors.New("repository graph exceeds limits")
		}
		seen[w.id] = w.kind
		out = append(out, w.id)
		if w.kind == "blob" {
			continue
		}
		o, e := store.Get(w.id)
		if e != nil {
			return nil, e
		}
		if o.Type != w.kind {
			return nil, errors.New("incorrect object type")
		}
		switch o.Type {
		case "commit":
			cm, _ := o.Commit()
			queue = append(queue, struct {
				id, kind string
				depth    int
			}{cm.Tree, "tree", 0})
			for _, p := range cm.Parents {
				queue = append(queue, struct {
					id, kind string
					depth    int
				}{p, "commit", 0})
			}
		case "tree":
			t, _ := o.Tree()
			for _, en := range t.Entries {
				kind := "blob"
				depth := 0
				if en.Mode == "tree" {
					kind = "tree"
					depth = w.depth + 1
				}
				queue = append(queue, struct {
					id, kind string
					depth    int
				}{en.ID, kind, depth})
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func Fetch(c *transport.Client, w *workspace.Workspace, branch string, full bool) (string, error) {
	res, e := c.Call(protocol.Request{Op: "refs"})
	if e != nil {
		return "", e
	}
	head, ok := res.Refs[branch]
	if !ok {
		return "", errors.New("remote branch not found")
	}
	if e = fetchHistory(c, w, head, full); e != nil {
		return "", e
	}

	tx, e := w.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`DELETE FROM remote_refs`); e != nil {
		return "", e
	}
	for name, id := range res.Refs {
		if !model.ValidRef(name) || (id != "" && !model.ValidID(id)) {
			return "", errors.New("server returned invalid reference")
		}
		if _, e = tx.Exec(`INSERT INTO remote_refs VALUES (?,?)`, name, id); e != nil {
			return "", e
		}
	}
	return head, tx.Commit()
}
func EnsureSnapshot(c *transport.Client, store objectstore.Store, head string, paths []string, baseHeads ...string) error {
	snapshot, e := store.Snapshot(head)
	if e != nil {
		return e
	}
	var ids []string
	for p, f := range snapshot {
		if workspace.Selected(p, paths) {
			ids = append(ids, f.ID)
		}
	}
	hints := map[string]string{}
	if len(baseHeads) > 0 && baseHeads[0] != "" {
		if old, e := store.Snapshot(baseHeads[0]); e == nil {
			for p, f := range snapshot {
				if base, ok := old[p]; ok && base.ID != f.ID && store.Exists(base.ID) {
					hints[f.ID] = base.ID
				}
			}
		}
	}
	return Download(c, store, ids, hints)
}
func legacyPush(c *transport.Client, w *workspace.Workspace) error {
	if e := w.Ready(); e != nil {
		return e
	}
	head, e := w.Head()
	if e != nil {
		return e
	}
	if head == "" {
		return errors.New("nothing to push")
	}
	ids, e := Inventory(w.Store, head)
	if e != nil {
		return e
	}
	var missing []string
	for len(ids) > 0 {
		n := min(protocol.MaxBatch, len(ids))
		res, e := c.Call(protocol.Request{Op: "have", IDs: ids[:n]})
		if e != nil {
			return e
		}
		allowed := map[string]bool{}
		for _, id := range ids[:n] {
			allowed[id] = true
		}
		for _, id := range res.IDs {
			if !allowed[id] {
				return errors.New("server requested an unexpected object")
			}
			missing = append(missing, id)
		}
		ids = ids[n:]
	}
	var batch []model.Object
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, e := c.Call(protocol.Request{Op: "put", Objects: batch})
		batch = nil
		size = 0
		return e
	}
	for _, id := range missing {
		o, e := w.Store.Get(id)
		if e != nil {
			return fmt.Errorf("cannot publish missing local object %s: %w", id, e)
		}
		if len(batch) == protocol.MaxBatch || size+len(o.Data) > model.MaxObject {
			if e = flush(); e != nil {
				return e
			}
		}
		batch = append(batch, o)
		size += len(o.Data)
	}
	if e = flush(); e != nil {
		return e
	}
	var expected string
	e = w.DB.QueryRow(`SELECT head FROM remote_refs WHERE name=?`, w.Branch()).Scan(&expected)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if _, e = c.Call(protocol.Request{Op: "push", Branch: w.Branch(), Head: head, Expected: expected}); e != nil {
		return e
	}
	_, e = w.DB.Exec(`INSERT INTO remote_refs VALUES (?,?) ON CONFLICT(name) DO UPDATE SET head=excluded.head`, w.Branch(), head)
	return e
}

func legacyFetchHistory(c *transport.Client, w *workspace.Workspace, head string, full bool) error {
	var e error
	queue := []string{}
	if head != "" {
		queue = append(queue, head)
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		var batch []string
		for len(queue) > 0 && len(batch) < protocol.MaxBatch {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			batch = append(batch, id)
		}
		if len(seen) > 2_000_000 {
			return errors.New("history exceeds object limit")
		}
		if e = Download(c, w.Store, batch); e != nil {
			return e
		}
		for _, id := range batch {
			o, e := w.Store.Get(id)
			if e != nil {
				return e
			}
			switch o.Type {
			case "commit":
				cm, _ := o.Commit()
				queue = append(queue, cm.Tree)
				queue = append(queue, cm.Parents...)
			case "tree":
				t, _ := o.Tree()
				for _, en := range t.Entries {
					if en.Mode == "tree" || full {
						queue = append(queue, en.ID)
					}
				}
			}
		}
	}
	if e = EnsureSnapshot(c, w.Store, head, w.Sparse()); e != nil {
		return e
	}
	return nil
}

func FetchCommit(c *transport.Client, w *workspace.Workspace, head string, full bool) error {
	if !model.ValidID(head) {
		return errors.New("invalid commit ID")
	}
	if e := fetchHistory(c, w, head, full); e != nil {
		return e
	}
	_, e := w.Store.Commit(head)
	return e
}
