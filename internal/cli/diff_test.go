package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/cli"
	"crease/internal/workspace"
)

func TestCommitDiffIgnoresWorkingFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	w, e := workspace.Init(root)
	must(t, e)
	put(t, root, "dir/file", "old\n")
	must(t, w.Add([]string{"."}, root))
	first, e := w.Commit("first", "test", nil)
	must(t, e)
	put(t, root, "dir/file", "new\n")
	second, e := w.Commit("second", "test", nil)
	must(t, e)
	w.Close()
	must(t, os.Remove(filepath.Join(root, "dir/file")))
	must(t, os.Symlink(t.TempDir(), filepath.Join(root, "dir/file")))
	t.Chdir(root)
	var out, stderr bytes.Buffer
	e = cli.Run([]string{"diff", first, second}, strings.NewReader(""), &out, &stderr, "test")
	must(t, e)
	if !strings.Contains(out.String(), "-old\n+new\n") {
		t.Fatalf("commit diff lost changes: %s", out.String())
	}
	if e = cli.Run([]string{"diff"}, strings.NewReader(""), &out, &stderr, "test"); e == nil {
		t.Fatal("working diff accepted a symlink")
	}
}
