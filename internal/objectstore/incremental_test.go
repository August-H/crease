package objectstore

import (
	"os"
	"path/filepath"
	"testing"

	"crease/internal/model"
)

func TestIncrementalTreeMatchesFullRebuild(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "objects"))
	blob := func(text string) string {
		id, e := s.Put(model.Object{Type: "blob", Data: []byte(text)})
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	base := Snapshot{"api/a": {ID: blob("a"), Mode: "file"}, "web/b": {ID: blob("b"), Mode: "file"}, "remove/nested/c": {ID: blob("c"), Mode: "file"}}
	root, e := s.WriteTree(base)
	if e != nil {
		t.Fatal(e)
	}
	next := Snapshot{"api/a": {ID: blob("changed"), Mode: "exec"}, "web/b": base["web/b"], "new/nested/d": {ID: blob("new"), Mode: "file"}}
	got, e := s.UpdateTree(root, FileChanges(base, next))
	if e != nil {
		t.Fatal(e)
	}
	want, e := s.WriteTree(next)
	if e != nil {
		t.Fatal(e)
	}
	if got != want {
		t.Fatalf("incremental tree differs from full rebuild: %s != %s", got, want)
	}
	commit := func(tree string) string {
		id, e := s.PutJSON("commit", model.Commit{Tree: tree, Author: "test", Time: "now", Message: "tree"})
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	l, r, e := s.ChangedSnapshots(commit(root), commit(got))
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := l["web/b"]; ok {
		t.Fatal("unchanged subtree entered diff")
	}
	if len(l) != 2 || len(r) != 2 {
		t.Fatalf("wrong changed paths: %v %v", l, r)
	}
	empty, e := s.UpdateTree(got, FileChanges(next, Snapshot{}))
	if e != nil {
		t.Fatal(e)
	}
	want, e = s.WriteTree(Snapshot{})
	if e != nil || empty != want {
		t.Fatal("empty directory pruning differs from full rebuild")
	}
}
func TestConsolidatedIndexNewPacksAndCorruptCacheFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "objects")
	s := New(dir)
	var ids []string
	for i := 0; i < 40; i++ {
		id, e := s.Put(model.Object{Type: "blob", Data: []byte{byte(i)}})
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Flush(); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	for _, corrupt := range []bool{false, true} {
		if corrupt {
			if e := os.WriteFile(filepath.Join(dir, "pack-index"), []byte("truncated cache"), 0600); e != nil {
				t.Fatal(e)
			}
		}
		s = New(dir)
		for _, id := range ids {
			if _, e := s.Verify(id); e != nil {
				t.Fatal(e)
			}
		}
	}
}

func TestPackedWriterRejectsInvalidPayloads(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "objects"))
	if _, e := s.Put(model.Object{Type: "tree", Data: []byte(`{"entries":[{"name":"../outside","mode":"file","id":"0000000000000000000000000000000000000000000000000000000000000000"}]}`)}); e == nil {
		t.Fatal("writer accepted an unsafe tree")
	}
	if _, e := s.PutJSON("commit", model.Commit{}); e == nil {
		t.Fatal("writer accepted an invalid commit")
	}
}
