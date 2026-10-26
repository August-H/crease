package objectstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"crease/internal/model"
)

func TestIntegrityAndReferencedTypes(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "objects")}
	o := model.Object{Type: "blob", Data: []byte("contents")}
	id := o.ID()
	raw, _ := json.Marshal(o)
	file, _ := s.file(id)
	e := Atomic(file, raw, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(file, []byte(`{"type":"blob","data":"Y29ycnVwdA=="}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(id); e == nil {
		t.Fatal("corrupt object accepted")
	}
	tree, e := s.PutJSON("tree", model.Tree{Entries: []model.Entry{}})
	if e != nil {
		t.Fatal(e)
	}
	badTree, e := s.PutJSON("tree", model.Tree{Entries: []model.Entry{{Name: "file", Mode: "file", ID: tree}}})
	if e != nil {
		t.Fatal(e)
	}
	head, e := s.PutJSON("commit", model.Commit{Tree: badTree, Parents: []string{}, Author: "test", Time: "now", Message: "wrong type"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Graph([]string{head}); e == nil {
		t.Fatal("incorrect referenced type accepted")
	}
}

func TestMergeBaseWithShortcutParent(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "objects")}
	tree, e := s.WriteTree(Snapshot{})
	if e != nil {
		t.Fatal(e)
	}
	commit := func(msg string, parents ...string) string {
		id, e := s.PutJSON("commit", model.Commit{Tree: tree, Parents: parents, Author: "test", Time: "now", Message: msg})
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	root := commit("root")
	a := commit("a", root)
	b := commit("b", a)
	c := commit("c", b)
	left := commit("left", c, root)
	right := commit("right", c, root)
	base, e := s.MergeBase(left, right)
	if e != nil || base != c {
		t.Fatalf("wrong best common ancestor %s %v", base, e)
	}
	x, y := commit("x", c), commit("y", c)
	m, n := commit("m", x, y), commit("n", y, x)
	if _, e = s.MergeBase(m, n); e == nil {
		t.Fatal("ambiguous merge bases accepted")
	}
}
