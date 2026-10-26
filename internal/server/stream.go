package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"crease/internal/codec"
	"crease/internal/model"
	"crease/internal/protocol"
	"golang.org/x/crypto/ssh"
)

// Each record is bounded even though an entire history may span many records.
const maxUploadObjects = 4096
const maxUploadBytes = 64 << 20

func (s *Server) stream(conn net.Conn, ch ssh.Channel, actor string, q protocol.Request) error {
	fail := func(e error) error {
		return protocol.Write(ch, protocol.Response{Version: protocol.Version, Error: e.Error()})
	}
	if q.Version != protocol.Version || !model.ValidRef(q.Repo) || len(q.IDs) > protocol.MaxBatch || len(q.Haves) > protocol.MaxBatch || len(q.Bases) > protocol.MaxBatch || len(q.Paths) > protocol.MaxBatch {
		return fail(errors.New("invalid stream request"))
	}
	for _, id := range append(append([]string{}, q.IDs...), q.Haves...) {
		if !model.ValidID(id) {
			return fail(errors.New("invalid stream object ID"))
		}
	}
	for id, base := range q.Bases {
		if !model.ValidID(id) || !model.ValidID(base) {
			return fail(errors.New("invalid delta hint"))
		}
	}
	level := 1
	if q.Op == "put_stream" {
		level = 2
	}
	repo, e := s.permission(actor, q.Repo, level)
	if e != nil {
		return fail(e)
	}
	store := s.Store(repo)
	stop := map[string]bool{}
	if q.Op == "fetch_stream" {
		if !model.ValidID(q.Head) {
			return fail(errors.New("invalid history head"))
		}
		for _, id := range q.Haves {
			if typ, e := store.Kind(id); e == nil && typ == "commit" {
				stop[id] = true
			}
		}
	}
	if e = protocol.Write(ch, protocol.Response{Version: protocol.Version, Stream: true}); e != nil {
		return e
	}
	res := protocol.Response{Version: protocol.Version}
	if q.Op == "put_stream" {
		for {
			conn.SetDeadline(time.Now().Add(2 * time.Minute))
			record, err := codec.Read(ch)
			if err == codec.ErrEnd {
				break
			}
			if err != nil {
				return err
			}
			res.Records++
			if res.Records > maxUploadObjects {
				return errors.New("upload exceeds object count limit")
			}
			_, _, _, err = codec.Inspect(record)
			if err != nil {
				return err
			}
			res.RawBytes += int64(binary.BigEndian.Uint32(record[34:]))
			if res.RawBytes > maxUploadBytes {
				return errors.New("upload exceeds byte limit")
			}
			if _, err = store.Import(record); err != nil {
				return err
			}
		}
		if e = store.Flush(); e != nil {
			res.Error = e.Error()
		}
		return protocol.Write(ch, res)
	}
	send := func(id string, o model.Object) error {
		conn.SetDeadline(time.Now().Add(2 * time.Minute))
		record, e := store.Export(id, q.Bases[id])
		if e != nil {
			return e
		}
		if e = codec.Write(ch, record); e != nil {
			return e
		}
		res.Records++
		res.RawBytes += int64(len(o.Data))
		return nil
	}
	switch q.Op {
	case "fetch_stream":
		e = store.Walk(q.Head, stop, q.Full, send)
	case "get_stream":
		for _, id := range q.IDs {
			o, err := store.Get(id)
			if err != nil {
				e = fmt.Errorf("object unavailable: %s", id)
				break
			}
			if e = send(id, o); e != nil {
				break
			}
		}
	default:
		e = errors.New("unknown stream operation")
	}
	if e != nil {
		res.Error = e.Error()
	}
	if e = codec.End(ch); e != nil {
		return e
	}
	return protocol.Write(ch, res)
}
