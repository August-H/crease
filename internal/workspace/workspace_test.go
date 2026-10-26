package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crease/internal/objectstore"
)

func initWorkspace(t *testing.T) *Workspace {
	t.Helper()
	w, e := Init(filepath.Join(t.TempDir(), "work"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(w.Close)
	return w
}
func write(t *testing.T, w *Workspace, p, s string) {
	t.Helper()
	file := filepath.Join(w.Root, p)
	if e := os.MkdirAll(filepath.Dir(file), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(file, []byte(s), 0644); e != nil {
		t.Fatal(e)
	}
}
func commit(t *testing.T, w *Workspace, msg string) string {
	t.Helper()
	id, e := w.Commit(msg, "tester", nil)
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func check(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

func TestCommitRestoreAndIgnore(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, ".creaseignore", "build/\n*.tmp\n")
	write(t, w, ".env", "SECRET=private\n")
	write(t, w, "src/main.go", "first\n")
	write(t, w, "build/out", "ignored")
	write(t, w, "cache.tmp", "ignored")
	check(t, w.Add([]string{"."}, w.Root))
	head := commit(t, w, "first")
	snapshot, e := w.Store.Snapshot(head)
	check(t, e)
	if len(snapshot) != 3 {
		t.Fatalf("unexpected tracked files: %v", snapshot)
	}
	write(t, w, "src/main.go", "second\n")
	if e = w.Checkout("main", head, false, false, nil); e == nil {
		t.Fatal("dirty checkout accepted")
	}
	check(t, w.Checkout("main", head, false, true, nil))
	b, e := os.ReadFile(filepath.Join(w.Root, "src/main.go"))
	check(t, e)
	if string(b) != "first\n" {
		t.Fatal("restore lost snapshot")
	}
	if e = os.Remove(filepath.Join(w.Root, ".env")); e != nil {
		t.Fatal(e)
	}
	second := commit(t, w, "delete secret")
	s, e := w.Store.Snapshot(second)
	check(t, e)
	if _, ok := s[".env"]; ok {
		t.Fatal("deleted file still present")
	}
	o, e := w.Store.Get(snapshot[".env"].ID)
	check(t, e)
	if string(o.Data) != "SECRET=private\n" {
		t.Fatal("historical secret lost")
	}
}

func TestSparseCommitPreservesHiddenFiles(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "api/a", "api\n")
	write(t, w, "web/b", "web\n")
	check(t, w.Add([]string{"."}, w.Root))
	commit(t, w, "both")
	check(t, w.SetSparse([]string{"api"}))
	if _, e := os.Stat(filepath.Join(w.Root, "web/b")); !os.IsNotExist(e) {
		t.Fatal("hidden file still materialized")
	}
	write(t, w, "api/a", "edited\n")
	head := commit(t, w, "api only")
	snapshot, e := w.Store.Snapshot(head)
	check(t, e)
	if _, ok := snapshot["web/b"]; !ok {
		t.Fatal("sparse commit deleted hidden file")
	}
	check(t, w.SetSparse(nil))
	b, e := os.ReadFile(filepath.Join(w.Root, "web/b"))
	check(t, e)
	if string(b) != "web\n" {
		t.Fatal("hidden contents changed")
	}
}

func TestUntrackRecordsDeletionAndKeepsFile(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "a", "keep")
	check(t, w.Add([]string{"a"}, w.Root))
	commit(t, w, "first")
	check(t, w.Untrack("a"))
	head := commit(t, w, "untrack")
	s, e := w.Store.Snapshot(head)
	check(t, e)
	if len(s) != 0 {
		t.Fatal("untrack did not remove snapshot entry")
	}
	if _, e = os.Stat(filepath.Join(w.Root, "a")); e != nil {
		t.Fatal("untrack removed workspace file")
	}
}

func TestWorkspaceProtections(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "a", "one")
	check(t, w.Add([]string{"a"}, w.Root))
	first := commit(t, w, "first")
	check(t, w.CreateBranch("next"))
	check(t, w.Switch("next"))
	write(t, w, "new", "tracked on next")
	check(t, w.Add([]string{"new"}, w.Root))
	commit(t, w, "next")
	check(t, w.Switch("main"))
	write(t, w, "new", "untracked")
	if e := w.Switch("next"); e == nil || !strings.Contains(e.Error(), "untracked") {
		t.Fatalf("untracked collision accepted: %v", e)
	}
	head, e := w.Head()
	check(t, e)
	if head != first {
		t.Fatal("failed switch moved branch")
	}
	if other, e := Find(w.Root); e == nil {
		other.Close()
		t.Fatal("concurrent workspace access allowed")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	check(t, os.WriteFile(outside, []byte("external"), 0644))
	check(t, os.Symlink(outside, filepath.Join(w.Root, "link")))
	if e = w.Add([]string{"link"}, w.Root); e == nil {
		t.Fatal("symlink accepted")
	}
	if e = w.Add([]string{"../"}, w.Root); e == nil {
		t.Fatal("outside path accepted")
	}
}

func TestInterruptedCheckoutRecovery(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "a", "before")
	check(t, w.Add([]string{"a"}, w.Root))
	old := commit(t, w, "old")
	write(t, w, "a", "after")
	newHead := commit(t, w, "new")
	from, e := w.Store.Snapshot(old)
	check(t, e)
	to, e := w.Store.Snapshot(newHead)
	check(t, e)
	check(t, w.Checkout("main", old, true, false, nil))
	j := Journal{From: from, To: to, Full: to, Branch: "main", Head: newHead, Move: true}
	b, e := json.Marshal(j)
	check(t, e)
	check(t, objectstore.Atomic(filepath.Join(w.Root, ".crease/journal.json"), b, 0600))
	if w.Ready() == nil {
		t.Fatal("interrupted update not detected")
	}
	write(t, w, "a", "intervening user edit")
	if w.Recover() == nil {
		t.Fatal("recovery overwrote intervening edits")
	}
	write(t, w, "a", "before")
	check(t, w.Recover())
	head, e := w.Head()
	check(t, e)
	if head != newHead {
		t.Fatal("recovery did not finish ref update")
	}
	check(t, w.Ready())
}
func TestFileCacheDoesNotHideSameSizeEdits(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "a", "one")
	check(t, w.Add([]string{"a"}, w.Root))
	commit(t, w, "initial")
	old := time.Now().Add(-10 * time.Second)
	check(t, os.Chtimes(filepath.Join(w.Root, "a"), old, old))
	time.Sleep(1100 * time.Millisecond)
	first, e := w.ReadFile("a")
	check(t, e)
	cached, e := w.ReadFile("a")
	check(t, e)
	if first != cached {
		t.Fatal("stable cache entry changed")
	}
	write(t, w, "a", "two")
	check(t, os.Chtimes(filepath.Join(w.Root, "a"), old, old))
	changed, e := w.ReadFile("a")
	check(t, e)
	if first.ID == changed.ID {
		t.Fatal("cache hid a same-size edit with restored mtime")
	}
	check(t, os.Chmod(filepath.Join(w.Root, "a"), 0755))
	exec, e := w.ReadFile("a")
	check(t, e)
	if exec.Mode != "exec" {
		t.Fatal("cache hid executable-bit change")
	}
}
func TestCommitDoesNotPublishWhenPackFlushFails(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "a", "pending")
	check(t, w.Add([]string{"a"}, w.Root))
	check(t, os.MkdirAll(w.Store.Dir, 0700))
	check(t, os.WriteFile(filepath.Join(w.Store.Dir, "packs"), []byte("not a directory"), 0600))
	if _, e := w.Commit("must fail", "test", nil); e == nil {
		t.Fatal("commit acknowledged an undurable object batch")
	}
	head, e := w.Head()
	check(t, e)
	if head != "" {
		t.Fatal("failed batch changed branch")
	}
	check(t, os.Remove(filepath.Join(w.Store.Dir, "packs")))
	commit(t, w, "retry")
}
