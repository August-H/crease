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

func TestLocalCommandsRequireInitializedRepository(t *testing.T) {
	// SSH trust files share the .crease name with workspace metadata. They do
	// not make the home directory a repository for projects below it.
	home := t.TempDir()
	credentials := filepath.Join(home, ".crease")
	must(t, os.Mkdir(credentials, 0700))
	known := filepath.Join(credentials, "known_hosts")
	must(t, os.WriteFile(known, []byte("trust settings\n"), 0600))
	project := filepath.Join(home, "projects", "snakegame")
	must(t, os.MkdirAll(project, 0755))
	put(t, project, "main.go", "package main\nfunc main() {}\n")
	t.Chdir(project)
	command := func(args ...string) (string, error) {
		var out, stderr bytes.Buffer
		err := cli.Run(args, strings.NewReader(""), &out, &stderr, "test")
		return out.String(), err
	}
	if _, err := command("add", "main.go"); err == nil || !strings.Contains(err.Error(), "crease init") {
		t.Fatalf("add without a repository should explain initialization: %v", err)
	}
	if _, err := command("commit", "-m", "hello"); err == nil || !strings.Contains(err.Error(), "crease init") {
		t.Fatalf("commit without a repository should explain initialization: %v", err)
	}
	entries, err := os.ReadDir(credentials)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "known_hosts" {
		t.Fatalf("repository discovery modified the credentials directory: %v", entries)
	}
	_, err = command("init")
	must(t, err)
	_, err = command("add", "main.go")
	must(t, err)
	_, err = command("commit", "-m", "hello")
	must(t, err)
	w, err := workspace.Find(project)
	must(t, err)
	defer w.Close()
	if w.Root != project || w.Branch() != "main" {
		t.Fatalf("wrong repository: %s on %s", w.Root, w.Branch())
	}
	head, err := w.Head()
	must(t, err)
	snapshot, err := w.Store.Snapshot(head)
	must(t, err)
	if len(snapshot) != 1 || snapshot["main.go"].ID == "" {
		t.Fatalf("unexpected committed files: %v", snapshot)
	}
}
