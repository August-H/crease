package server

import (
	"errors"
	"net"
	"sort"
	"time"

	"crease/internal/codec"
	"crease/internal/model"
	"crease/internal/protocol"
	"crease/internal/workspace"
	"golang.org/x/crypto/ssh"
)

// cloneStream sends metadata first, followed by selected current blobs and any
// requested historical blobs. The opening response pins refs and the head, so
// concurrent branch updates cannot mix two snapshots within one clone.
func (s *Server) cloneStream(conn net.Conn, ch ssh.Channel, actor string, q protocol.Request) error {
	fail := func(e error) error {
		return protocol.Write(ch, protocol.Response{Version: protocol.Version, Error: e.Error()})
	}
	if q.Version != protocol.Version || !model.ValidRef(q.Repo) || !model.ValidRef(q.Branch) || len(q.Paths) > protocol.MaxBatch {
		return fail(errors.New("invalid clone request"))
	}
	for _, p := range q.Paths {
		if !model.ValidPath(p) {
			return fail(errors.New("invalid clone selection"))
		}
	}
	repo, e := s.permission(actor, q.Repo, 1)
	if e != nil {
		return fail(e)
	}
	refs, e := s.refs(repo)
	if e != nil {
		return fail(e)
	}
	head, ok := refs[q.Branch]
	if !ok {
		return fail(errors.New("remote branch not found"))
	}
	store := s.Store(repo)
	snapshot, e := store.Snapshot(head)
	if e != nil {
		return fail(e)
	}
	if e = protocol.Write(ch, protocol.Response{Version: protocol.Version, Stream: true, Refs: refs, Head: head}); e != nil {
		return e
	}
	result := protocol.Response{Version: protocol.Version}
	send := func(_ string, o model.Object) error {
		conn.SetDeadline(time.Now().Add(2 * time.Minute))
		// Walk/Get already decoded this object. Do not fetch it again to export.
		record, e := codec.Encode(o, nil)
		if e != nil {
			return e
		}
		if e = codec.Write(ch, record); e != nil {
			return e
		}
		result.Records++
		result.RawBytes += int64(len(o.Data))
		return nil
	}
	e = store.Walk(head, nil, false, send)
	sent := map[string]bool{}
	if e == nil {
		paths := make([]string, 0, len(snapshot))
		for p := range snapshot {
			if workspace.Selected(p, q.Paths) {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			id := snapshot[p].ID
			if sent[id] {
				continue
			}
			o, err := store.Get(id)
			if err != nil {
				e = err
				break
			}
			if e = send(id, o); e != nil {
				break
			}
			sent[id] = true
		}
	}
	if e == nil && q.Full {
		e = store.Walk(head, nil, true, func(id string, o model.Object) error {
			if o.Type != "blob" || sent[id] {
				return nil
			}
			sent[id] = true
			return send(id, o)
		})
	}
	if e != nil {
		result.Error = e.Error()
	}
	if e = codec.End(ch); e != nil {
		return e
	}
	return protocol.Write(ch, result)
}
