package model

import "testing"

func TestCanonicalFormat(t *testing.T) {
	o := Object{Type: "blob", Data: []byte("hello\n")}
	const want = "6b2ef4b5a175d7f4c416b60c2515ba02df34823a4cc4868388dfa220aca578fd"
	if got := o.ID(); got != want {
		t.Fatalf("object format changed: got %s want %s", got, want)
	}
	bad := []Object{{Type: "tree", Data: []byte(`{"entries":[],"extra":1}`)}, {Type: "tree", Data: []byte(`{ "entries":[]}`)}, {Type: "unknown"}}
	for _, o := range bad {
		if o.Validate() == nil {
			t.Fatalf("accepted invalid object %+v", o)
		}
	}
}
func TestPaths(t *testing.T) {
	for _, p := range []string{"../escape", "/absolute", "a/../../b", "a\\b", ".crease/config", "a/.Crease/x", ".git/config", "a//b", "a/CON.txt", "a/b.", "a/b "} {
		if ValidPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	for _, p := range []string{".env", "src/main.go", "a/.hidden"} {
		if !ValidPath(p) {
			t.Errorf("rejected %q", p)
		}
	}
}
func TestUnicodeNameCollisions(t *testing.T) {
	id := Object{Type: "blob", Data: []byte("file")}.ID()
	for _, names := range [][2]string{{"e\u0301.txt", "é.txt"}, {"Σ.txt", "ς.txt"}} {
		entries := []Entry{{Name: names[0], Mode: "file", ID: id}, {Name: names[1], Mode: "file", ID: id}}
		Sort(entries)
		if _, e := JSON("tree", Tree{Entries: entries}); e == nil {
			t.Fatalf("accepted filesystem aliases: %v", names)
		}
	}
}
