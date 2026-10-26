package diff

import (
	"testing"
)

func TestThreeWay(t *testing.T) {
	cases := []struct {
		name, base, left, right, want string
		conflict                      bool
	}{
		{"independent", "a\nb\nc\n", "A\nb\nc\n", "a\nb\nC\n", "A\nb\nC\n", false},
		{"identical", "a\n", "b\n", "b\n", "b\n", false},
		{"overlap", "a\n", "b\n", "c\n", "", true},
		{"insert conflict", "a\n", "a\nb\n", "a\nc\n", "", true},
		{"binary", "a\x00", "b\x00", "c\x00", "", true},
		{"no final newline", "a\nb", "A\nb", "a\nB", "A\nB", false},
		{"one unchanged", "a\n", "a\n", "b\n", "b\n", false},
		{"deletion", "a\nb\nc\n", "b\nc\n", "a\nb\nC\n", "b\nC\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, e := Text([]byte(c.base), []byte(c.left), []byte(c.right))
			if (e != nil) != c.conflict {
				t.Fatalf("conflict=%v error=%v", c.conflict, e)
			}
			if e == nil && string(out) != c.want {
				t.Fatalf("got %q want %q", out, c.want)
			}
		})
	}
}
func FuzzMergeIdentity(f *testing.F) {
	f.Add("a\nb\n", "a\nc\n")
	f.Fuzz(func(t *testing.T, base, change string) {
		out, e := Text([]byte(base), []byte(change), []byte(base))
		if e != nil || string(out) != change {
			t.Fatalf("unchanged side lost edits: %v", e)
		}
	})
}
