package diff

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/objectstore"
)

func TestEditsReconstructInputs(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for sample := 0; sample < 1000; sample++ {
		a, b := make([]string, r.Intn(35)), make([]string, r.Intn(35))
		for i := range a {
			a[i] = fmt.Sprint(r.Intn(5))
		}
		for i := range b {
			b[i] = fmt.Sprint(r.Intn(5))
		}
		ed, e := edits(a, b)
		if e != nil {
			t.Fatal(e)
		}
		var out []string
		pos := 0
		for _, change := range ed {
			out = append(out, a[pos:change.start]...)
			out = append(out, change.lines...)
			pos = change.end
		}
		out = append(out, a[pos:]...)
		if strings.Join(out, ",") != strings.Join(b, ",") {
			t.Fatalf("edits failed: %v -> %v gave %v", a, b, out)
		}
	}
}

func TestModeOnlyDiffDoesNotLoadContents(t *testing.T) {
	left := objectstore.Snapshot{"file": {ID: "same", Mode: "file"}}
	right := objectstore.Snapshot{"file": {ID: "same", Mode: "exec"}}
	out, e := UnifiedWith(left, right, func(string, objectstore.File, bool) ([]byte, error) {
		t.Fatal("mode change loaded file contents")
		return nil, nil
	})
	if e != nil || !strings.Contains(out, "old mode 100644\nnew mode 100755") {
		t.Fatalf("wrong mode-only diff: %s %v", out, e)
	}
}
func TestLargeDiffHasApplicableContextHunks(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&a, "line %05d\n", i)
		if i == 100 || i == 9900 {
			fmt.Fprintf(&b, "edited %05d\n", i)
		} else {
			fmt.Fprintf(&b, "line %05d\n", i)
		}
	}
	left, right := objectstore.Snapshot{"file": {ID: "left", Mode: "file"}}, objectstore.Snapshot{"file": {ID: "right", Mode: "file"}}
	text, e := UnifiedWith(left, right, func(_ string, _ objectstore.File, isRight bool) ([]byte, error) {
		if isRight {
			return []byte(b.String()), nil
		}
		return []byte(a.String()), nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if len(text) > 1000 || strings.Count(text, "@@ -") != 2 {
		t.Fatalf("expected two small context hunks, got %d bytes", len(text))
	}
	git, e := exec.LookPath("git")
	if e != nil {
		t.Skip("git unavailable for independent patch validation")
	}
	dir := t.TempDir()
	if e = os.WriteFile(filepath.Join(dir, "file"), []byte(a.String()), 0644); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(git, "apply", "-")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(text)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("patch rejected: %v\n%s\n%s", e, out, text)
	}
	got, e := os.ReadFile(filepath.Join(dir, "file"))
	if e != nil || string(got) != b.String() {
		t.Fatalf("patch reconstructed wrong contents: %v", e)
	}
}
