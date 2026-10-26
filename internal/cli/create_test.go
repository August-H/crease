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

func TestCreateRemoteFromExistingWorkspace(t *testing.T) {
	f := fixtureServer(t)
	project := filepath.Join(f.root, "existing")
	w, err := workspace.Init(project)
	must(t, err)
	put(t, project, "main.go", "committed\n")
	must(t, w.Add([]string{"main.go"}, project))
	head, err := w.Commit("hello", "test", nil)
	must(t, err)
	w.Close()
	put(t, project, "main.go", "uncommitted edit\n")
	put(t, project, "notes", "untracked\n")
	f.run(t, project, f.owner, "create", f.url, ".")
	// Repeating creation is safe and does not reinitialize the workspace.
	f.run(t, project, f.owner, "create", f.url, ".")
	w, err = workspace.Find(project)
	must(t, err)
	actual, err := w.Head()
	must(t, err)
	remote := w.Config("remote")
	w.Close()
	if actual != head || remote != f.url {
		t.Fatalf("existing history or remote lost: head=%s remote=%s", actual, remote)
	}
	if get(t, project, "main.go") != "uncommitted edit\n" || get(t, project, "notes") != "untracked\n" {
		t.Fatal("creation changed working files")
	}
	f.run(t, project, f.owner, "push")
	f.run(t, project, f.owner, "create", f.url, ".")
	f.run(t, project, f.owner, "push")
	clone := filepath.Join(f.root, "clone")
	f.run(t, f.root, f.owner, "clone", f.url, clone)
	if get(t, clone, "main.go") != "committed\n" {
		t.Fatal("push did not preserve the original commit")
	}
	if _, err := os.Stat(filepath.Join(clone, "notes")); !os.IsNotExist(err) {
		t.Fatal("untracked file was published")
	}
	if _, err := f.try(t, project, f.owner, "clone", f.url, "."); err == nil {
		t.Fatal("clone accepted an existing workspace")
	}
	if _, err := f.try(t, project, f.owner, "create", f.url+"-other", "."); err == nil || !strings.Contains(err.Error(), "different remote") {
		t.Fatalf("existing remote was not protected: %v", err)
	}
}

func TestCreateDestinationFileExplainsExistingWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	put(t, root, "snakegame", "compiled program")
	var out, stderr bytes.Buffer
	err := cli.Run([]string{"create", "ssh://localhost:2222/snakegame"}, strings.NewReader(""), &out, &stderr, "test")
	if err == nil || !strings.Contains(err.Error(), `destination "snakegame"`) || !strings.Contains(err.Error(), "crease create ssh://localhost:2222/snakegame .") {
		t.Fatalf("unhelpful destination error: %v", err)
	}
	if get(t, root, "snakegame") != "compiled program" {
		t.Fatal("destination file changed")
	}
}
