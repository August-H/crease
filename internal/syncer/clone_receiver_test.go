package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"crease/internal/codec"
	"crease/internal/model"
	"crease/internal/protocol"
	"crease/internal/workspace"
)

func TestCloneRejectsIncompleteOrInvalidStreamBeforePublication(t *testing.T) {
	source, err := workspace.Init(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := os.WriteFile(filepath.Join(source.Root, "file"), []byte("verified fixture contents\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := source.Add([]string{"file"}, source.Root); err != nil {
		t.Fatal(err)
	}
	head, err := source.Commit("fixture", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var records [][]byte
	if err := source.Store.Walk(head, nil, false, func(_ string, o model.Object) error {
		record, e := codec.Encode(o, nil)
		records = append(records, record)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Store.Snapshot(head)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := source.Store.Export(snapshot["file"].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := codec.Encode(model.Object{Type: "blob", Data: []byte("not in the snapshot")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing-blob", "duplicate-blob", "unexpected-blob", "corrupt-blob", "metadata-after-blob", "missing-tree", "server-error", "wrong-head"} {
		t.Run(name, func(t *testing.T) {
			w, err := workspace.Init(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			errAfter := false
			stream := append([][]byte{}, records...)
			stream = append(stream, blob)
			switch name {
			case "missing-blob":
				stream = stream[:len(stream)-1]
			case "duplicate-blob":
				stream = append(stream, blob)
			case "unexpected-blob":
				stream = append(stream, other)
			case "corrupt-blob":
				corrupt := append([]byte{}, blob...)
				corrupt[len(corrupt)-1] ^= 1
				stream[len(stream)-1] = corrupt
			case "metadata-after-blob":
				stream = append(stream, records[0])
			case "missing-tree":
				stream = [][]byte{records[0], blob}
			case "server-error":
				errAfter = true
			}
			_, err = cloneFromStream(w, "main", false, func(_ protocol.Request, begin func(protocol.Response) error, accept func([]byte) error) (protocol.Response, error) {
				response := protocol.Response{Version: protocol.Version, Head: head, Refs: map[string]string{"main": head}, Stream: true}
				if name == "wrong-head" {
					response.Head = snapshot["file"].ID
				}
				if e := begin(response); e != nil {
					return response, e
				}
				for _, record := range stream {
					if e := accept(record); e != nil {
						return response, e
					}
				}
				if errAfter {
					return response, errors.New("server failed before stream acknowledgement")
				}
				return response, nil
			})
			if err == nil {
				t.Fatal("invalid clone stream was accepted")
			}
			if got, err := w.Head(); err != nil || got != "" {
				t.Fatal("failed stream published a branch")
			}
			if err := w.Ready(); err != nil {
				t.Fatal("failed transfer should not publish a checkout journal", err)
			}
			if _, err := os.Stat(filepath.Join(w.Root, "file")); !os.IsNotExist(err) {
				t.Fatal("failed transfer exposed a working file")
			}
			stages, err := filepath.Glob(filepath.Join(w.Root, ".crease", "clone-stage-*"))
			if err != nil || len(stages) != 0 {
				t.Fatal("failed transfer leaked staging files")
			}
		})
	}
}
