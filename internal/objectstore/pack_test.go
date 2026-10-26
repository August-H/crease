package objectstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"crease/internal/codec"
	"crease/internal/model"
)

func TestPackBatchDurabilityAndOrphanRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "objects")
	s := New(dir)
	var ids []string
	for i := 0; i < 1000; i++ {
		id, e := s.Put(model.Object{Type: "blob", Data: []byte{byte(i), byte(i >> 8)}})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	if New(dir).Exists(ids[0]) {
		t.Fatal("unflushed data published")
	}
	s.state.beforePublish = func() error { return errors.New("simulated interrupted index publication") }
	if s.Flush() == nil {
		t.Fatal("injected failure ignored")
	}
	reopen := New(dir)
	if reopen.Exists(ids[0]) {
		t.Fatal("orphan pack treated as published")
	}
	s.state.beforePublish = nil
	if e := s.Flush(); e != nil {
		t.Fatal(e)
	}
	reopen = New(dir)
	for _, id := range ids {
		if _, e := reopen.Verify(id); e != nil {
			t.Fatal(e)
		}
	}
	packs, _ := filepath.Glob(filepath.Join(dir, "packs", "*.idx"))
	if len(packs) != 1 {
		t.Fatalf("1000 objects required %d indexes", len(packs))
	}
	pack, _ := filepath.Glob(filepath.Join(dir, "packs", "*.pack"))
	f, e := os.OpenFile(pack[0], os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.WriteAt([]byte{255}, 6); e != nil {
		t.Fatal(e)
	}
	f.Close()
	found := false
	for _, id := range ids {
		if _, e = reopen.Verify(id); e != nil {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("pack corruption was not detected")
	}
}

func TestDeltaChainLimitAndIndependentReopen(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "objects"))
	data := bytes.Repeat([]byte("source line content\n"), 10000)
	previous := ""
	var ids []string
	for i := 0; i < 24; i++ {
		next := bytes.Clone(data)
		copy(next[i*100:], []byte{byte('A' + i)})
		id, e := s.PutWithBase(model.Object{Type: "blob", Data: next}, previous)
		if e != nil {
			t.Fatal(e)
		}
		previous = id
		ids = append(ids, id)
		data = next
	}
	if e := s.Flush(); e != nil {
		t.Fatal(e)
	}
	reopened := New(s.Dir)
	for _, id := range ids {
		if _, e := reopened.Verify(id); e != nil {
			t.Fatal(e)
		}
		if reopened.state.depth[id] > codec.MaxDepth {
			t.Fatal("unbounded delta chain")
		}
	}
}

func TestValidatedSubtreesKeepDepthLimit(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "objects"))
	tree, e := s.WriteTree(Snapshot{})
	if e != nil {
		t.Fatal(e)
	}
	proof := Proof{Kind: "tree", Height: 101}
	root, e := s.PutJSON("tree", model.Tree{Entries: []model.Entry{{Name: "nested", Mode: "tree", ID: tree}}})
	if e != nil {
		t.Fatal(e)
	}
	head, e := s.PutJSON("commit", model.Commit{Tree: root, Parents: []string{}, Author: "test", Time: "now", Message: "depth"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Validate(head, func(id string) (Proof, bool, error) { return proof, id == tree, nil }); e == nil {
		t.Fatal("validated subtree bypassed nesting limit")
	}
}
