package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crease/internal/monitor"
)

func startMonitor(t *testing.T, root string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx, root) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case e := <-done:
			cancel()
			t.Fatalf("monitor startup: %v", e)
		default:
		}
		if _, e := monitor.Query(root, "status", "", 0); e == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("monitor startup timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}
	t.Cleanup(stop)
	return stop
}
func TestMonitorTracksEditsRenamesUntrackedFilesAndRestarts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	w, e := Init(root)
	check(t, e)
	write(t, w, "src/a", "one\n")
	write(t, w, "deleted/b", "old\n")
	check(t, w.Add([]string{"."}, root))
	commit(t, w, "initial")
	w.Close()
	status := func() map[string]string {
		w, e := Find(root)
		check(t, e)
		defer w.Close()
		changes, e := w.Status()
		check(t, e)
		out := map[string]string{}
		for _, c := range changes {
			out[c.Path] = c.Code
		}
		return out
	}
	stop := startMonitor(t, root)
	if got := status(); len(got) != 0 {
		t.Fatalf("initial status: %v", got)
	}
	// Query immediately after each write. The helper must drain pending events
	// even if its periodic polling tick has not run yet.
	check(t, os.WriteFile(filepath.Join(root, "src/a"), []byte("two\n"), 0644))
	if got := status(); got["src/a"] != "M" {
		t.Fatalf("in-place edit lost: %v", got)
	}
	check(t, os.WriteFile(filepath.Join(root, "replacement"), []byte("new\n"), 0644))
	check(t, os.Rename(filepath.Join(root, "replacement"), filepath.Join(root, "src/a")))
	check(t, os.RemoveAll(filepath.Join(root, "deleted")))
	check(t, os.MkdirAll(filepath.Join(root, "new/nested"), 0755))
	check(t, os.WriteFile(filepath.Join(root, "new/nested/file"), []byte("new"), 0644))
	if got := status(); got["src/a"] != "M" || got["deleted/b"] != "D" || got["new/nested/file"] != "?" {
		t.Fatalf("rename, deletion, or untracked creation lost: %v", got)
	}
	check(t, os.WriteFile(filepath.Join(root, ".creaseignore"), []byte("new/\n"), 0644))
	if got := status(); got["new/nested/file"] != "" {
		t.Fatalf("ignore change did not invalidate discovery: %v", got)
	}
	check(t, os.Chmod(filepath.Join(root, "src/a"), 0755))
	_ = status()
	w, e = Find(root)
	check(t, e)
	head, e := w.Commit("monitored edits", "test", nil)
	check(t, e)
	snapshot, e := w.Store.Snapshot(head)
	check(t, e)
	if snapshot["src/a"].Mode != "exec" {
		t.Fatal("monitor hid executable mode change")
	}
	if _, ok := snapshot["deleted/b"]; ok {
		t.Fatal("deleted parent directory remained committed")
	}
	w.Close()
	stop()
	check(t, os.WriteFile(filepath.Join(root, "src/a"), []byte("off\n"), 0755))
	if got := status(); got["src/a"] != "M" {
		t.Fatalf("unavailable monitor hid changes: %v", got)
	}
	stop = startMonitor(t, root)
	if got := status(); got["src/a"] != "M" {
		t.Fatalf("restart reused stale cursor: %v", got)
	}
	check(t, os.WriteFile(filepath.Join(root, ".crease/workspace-index"), []byte("truncated"), 0600))
	if got := status(); got["src/a"] != "M" {
		t.Fatalf("corrupt disposable index hid changes: %v", got)
	}
	stop()
}
func TestInspectionDoesNotPersistEdits(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	w, e := Init(root)
	check(t, e)
	write(t, w, "file", "initial")
	check(t, w.Add([]string{"."}, root))
	commit(t, w, "initial")
	w.Close()
	before, e := filepath.Glob(filepath.Join(root, ".crease/objects/packs/*.idx"))
	check(t, e)
	w, e = Find(root)
	check(t, e)
	write(t, w, "file", "uncommitted")
	_, e = w.Status()
	check(t, e)
	snapshot, e := w.InspectSnapshot()
	check(t, e)
	id := snapshot["file"].ID
	w.Close()
	after, e := filepath.Glob(filepath.Join(root, ".crease/objects/packs/*.idx"))
	check(t, e)
	if len(before) != len(after) {
		t.Fatal("inspection persisted a new pack")
	}
	w, e = Find(root)
	check(t, e)
	defer w.Close()
	if w.Store.Exists(id) {
		t.Fatal("inspection wrote the edited blob")
	}
	head := commit(t, w, "persist inspected edit")
	snapshot, e = w.Store.Snapshot(head)
	check(t, e)
	object, e := w.Store.Verify(snapshot["file"].ID)
	check(t, e)
	if string(object.Data) != "uncommitted" {
		t.Fatal("commit did not persist inspected bytes")
	}
}
func TestAnchoredScanRejectsSymlinkParents(t *testing.T) {
	w := initWorkspace(t)
	write(t, w, "dir/file", "inside")
	check(t, w.Add([]string{"."}, w.Root))
	commit(t, w, "initial")
	s, e := w.newScan()
	check(t, e)
	defer s.close()
	_, e = s.read("dir/file", false)
	check(t, e)
	outside := t.TempDir()
	check(t, os.WriteFile(filepath.Join(outside, "other"), []byte("outside"), 0644))
	check(t, os.Rename(filepath.Join(w.Root, "dir"), filepath.Join(w.Root, "old")))
	check(t, os.Symlink(outside, filepath.Join(w.Root, "dir")))
	if _, e = s.read("dir/other", false); e == nil {
		t.Fatal("cached parent traversal followed a replacement symlink")
	}
	if _, e = w.InspectSnapshot(); e == nil {
		t.Fatal("new scan accepted symlink parent")
	}
}
