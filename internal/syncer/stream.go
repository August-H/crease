package syncer

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"io"

	"crease/internal/codec"
	"crease/internal/model"
	"crease/internal/protocol"
	"crease/internal/transport"
	"crease/internal/workspace"
)

func fetchHistory(c *transport.Client, w *workspace.Workspace, head string, full bool) error {
	if head == "" {
		return nil
	}
	if !c.Capabilities["binary-stream-v1"] {
		if e := legacyFetchHistory(c, w, head, full); e != nil {
			return e
		}
	} else {
		rows, e := w.DB.Query(`SELECT head FROM fetched_heads WHERE full>=? ORDER BY rowid DESC LIMIT 128`, boolInt(full))
		if e != nil {
			return e
		}
		var haves []string
		stop := map[string]bool{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			if typ, e := w.Store.Kind(id); e == nil && typ == "commit" {
				haves = append(haves, id)
				stop[id] = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		_, e = c.Receive(protocol.Request{Op: "fetch_stream", Head: head, Haves: haves, Full: full}, func(record []byte) error {
			_, typ, _, e := codec.Inspect(record)
			if e != nil {
				return e
			}
			if !full && typ == "blob" {
				return errors.New("unexpected blob in metadata stream")
			}
			_, e = w.Store.Import(record)
			return e
		})
		if e != nil {
			return e
		}
		if e = w.Store.Walk(head, stop, false, func(string, model.Object) error { return nil }); e != nil {
			return e
		}
		oldHead, err := w.Head()
		if err != nil {
			return err
		}
		if e = EnsureSnapshot(c, w.Store, head, w.Sparse(), oldHead); e != nil {
			return e
		}
	}
	if e := w.Store.Flush(); e != nil {
		return e
	}
	_, e := w.DB.Exec(`INSERT INTO fetched_heads VALUES (?,?) ON CONFLICT(head) DO UPDATE SET full=max(full,excluded.full)`, head, boolInt(full))
	return e
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func Push(c *transport.Client, w *workspace.Workspace) error {
	if !c.Capabilities["graph-proofs-v1"] || !c.Capabilities["binary-stream-v1"] {
		return legacyPush(c, w)
	}
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
	if e = w.Store.Flush(); e != nil {
		return e
	}
	var expected string
	e = w.DB.QueryRow(`SELECT head FROM remote_refs WHERE name=?`, w.Branch()).Scan(&expected)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	rows, e := w.DB.Query(`SELECT DISTINCT head FROM remote_refs WHERE head!='' LIMIT 128`)
	if e != nil {
		return e
	}
	var candidates []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		if w.Store.Exists(id) {
			candidates = append(candidates, id)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	common, e := c.Call(protocol.Request{Op: "common", IDs: candidates})
	if e != nil {
		return e
	}
	stop := map[string]bool{}
	trees := map[string]bool{}
	allowed := map[string]bool{}
	for _, id := range candidates {
		allowed[id] = true
	}
	for _, id := range common.IDs {
		if !allowed[id] {
			return errors.New("unexpected common history frontier")
		}
		stop[id] = true
		cm, e := w.Store.Commit(id)
		if e != nil {
			return e
		}
		stack := []string{cm.Tree}
		for len(stack) > 0 {
			tree := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if trees[tree] {
				continue
			}
			trees[tree] = true
			t, e := w.Store.Tree(tree)
			if e != nil {
				return e
			}
			for _, en := range t.Entries {
				if en.Mode == "tree" {
					stack = append(stack, en.ID)
				}
			}
		}
	}
	ids, e := inventorySince(w.Store, head, stop, trees)
	if e != nil {
		return e
	}
	var missing []string
	for len(ids) > 0 {
		n := min(protocol.MaxBatch, len(ids))
		r, e := c.Call(protocol.Request{Op: "have", IDs: ids[:n]})
		if e != nil {
			return e
		}
		allow := map[string]bool{}
		for _, id := range ids[:n] {
			allow[id] = true
		}
		for _, id := range r.IDs {
			if !allow[id] {
				return errors.New("unexpected missing object")
			}
			missing = append(missing, id)
		}
		ids = ids[n:]
	}
	hints := map[string]string{}
	baseline := expected
	if baseline == "" && len(common.IDs) > 0 {
		baseline = common.IDs[0]
	}
	if baseline != "" {
		old, e := w.Store.Snapshot(baseline)
		if e == nil {
			next, e := w.Store.Snapshot(head)
			if e != nil {
				return e
			}
			for p, f := range next {
				if base, ok := old[p]; ok && base.ID != f.ID && w.Store.Exists(base.ID) {
					hints[f.ID] = base.ID
				}
			}
		}
	}
	index := 0
	for index < len(missing) {
		count, size := 0, 0
		_, e = c.Send(protocol.Request{Op: "put_stream"}, func() ([]byte, error) {
			if index >= len(missing) || count >= 4096 {
				return nil, io.EOF
			}
			id := missing[index]
			record, e := w.Store.Export(id, hints[id])
			if e != nil {
				return nil, e
			}
			rawSize := int(binary.BigEndian.Uint32(record[34:]))
			if count > 0 && size+rawSize > 64<<20 {
				return nil, io.EOF
			}
			index++
			count++
			size += rawSize
			return record, nil
		})
		if e != nil {
			return e
		}
	}
	if _, e = c.Call(protocol.Request{Op: "push", Branch: w.Branch(), Head: head, Expected: expected}); e != nil {
		return e
	}
	_, e = w.DB.Exec(`INSERT INTO remote_refs VALUES (?,?) ON CONFLICT(name) DO UPDATE SET head=excluded.head`, w.Branch(), head)
	return e
}
