package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/model"
	"crease/internal/objectstore"
)

func cloneRead(t *testing.T, w *Workspace, p string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(w.Root, p))
	check(t, e)
	return string(b)
}

func cloneFixture(t *testing.T) (*Workspace, string, objectstore.Snapshot, map[string][]byte) {
	t.Helper()
	w := initWorkspace(t)
	snapshot := objectstore.Snapshot{}
	blobs := map[string][]byte{}
	for i := 0; i < 80; i++ {
		data := []byte(fmt.Sprintf("shared payload %d\n", i%10))
		id, e := w.Store.Put(model.Object{Type: "blob", Data: data})
		check(t, e)
		blobs[id] = data
		mode := "file"
		if i%3 == 0 {
			mode = "exec"
		}
		snapshot[fmt.Sprintf("dir-%d/nested/file-%03d", i%4, i)] = objectstore.File{ID: id, Mode: mode}
	}
	tree, e := w.Store.WriteTree(snapshot)
	check(t, e)
	head, e := w.Store.PutJSON("commit", model.Commit{Tree: tree, Author: "test", Time: "2026-10-26T00:00:00Z", Message: "clone fixture"})
	check(t, e)
	return w, head, snapshot, blobs
}

func TestCloneBatchFilesModesAndIsolation(t *testing.T) {
	w, head, snapshot, blobs := cloneFixture(t)
	stage, e := w.PrepareClone("main", head, nil)
	check(t, e)
	defer stage.Close()
	for id, data := range blobs {
		check(t, stage.AddVerified(id, data))
	}
	check(t, stage.Wait())
	if _, e = os.Stat(filepath.Join(w.Root, "dir-0")); !os.IsNotExist(e) {
		t.Fatal("staging exposed working files before publication")
	}
	check(t, stage.Publish())
	check(t, w.Ready())
	got, e := w.Head()
	check(t, e)
	if got != head {
		t.Fatal("clone did not publish its head")
	}
	for p, file := range snapshot {
		data, e := os.ReadFile(filepath.Join(w.Root, p))
		check(t, e)
		if string(data) != string(blobs[file.ID]) {
			t.Fatalf("wrong bytes for %s", p)
		}
		st, e := os.Stat(filepath.Join(w.Root, p))
		check(t, e)
		if (st.Mode()&0111 != 0) != (file.Mode == "exec") {
			t.Fatalf("wrong executable mode for %s", p)
		}
	}
	// Identical blob IDs must not create linked working files, including when
	// their executable modes differ.
	check(t, os.WriteFile(filepath.Join(w.Root, "dir-0/nested/file-000"), []byte("edit"), 0644))
	other, e := os.ReadFile(filepath.Join(w.Root, "dir-2/nested/file-010"))
	check(t, e)
	if string(other) != "shared payload 0\n" {
		t.Fatal("editing one path changed another path with the same blob")
	}
}

func TestCloneBatchInterruptedPublicationRecoversFromObjects(t *testing.T) {
	w, head, snapshot, blobs := cloneFixture(t)
	stage, e := w.PrepareClone("main", head, nil)
	check(t, e)
	for id, data := range blobs {
		check(t, stage.AddVerified(id, data))
	}
	var published string
	stage.afterPublish = func(p string) error { published = p; return errors.New("simulated interruption") }
	if e = stage.Publish(); e == nil {
		t.Fatal("publication interruption was ignored")
	}
	stage.Close()
	root := w.Root
	w.Close()
	w, e = Find(root)
	check(t, e)
	defer w.Close()
	if got, e := w.Head(); e != nil || got != "" {
		t.Fatal("interrupted clone published its head")
	}
	if w.Ready() == nil {
		t.Fatal("clone lost its recovery journal")
	}
	// Recovery must protect edits made after the interrupted clone.
	write(t, w, published, "intervening edit")
	if e = w.Recover(); e == nil {
		t.Fatal("recovery overwrote a user edit")
	}
	write(t, w, published, string(blobs[snapshot[published].ID]))
	check(t, w.Recover())
	check(t, w.Ready())
	if got, e := w.Head(); e != nil || got != head {
		t.Fatal("recovery did not publish the expected head")
	}
	for p, file := range snapshot {
		if got := cloneRead(t, w, p); got != string(blobs[file.ID]) {
			t.Fatalf("recovery lost %s", p)
		}
		_, e = w.Store.Verify(file.ID)
		check(t, e)
	}
}

func TestCloneBatchRejectsMissingBlobsAndWorkerErrors(t *testing.T) {
	for _, workerError := range []bool{false, true} {
		t.Run(fmt.Sprint(workerError), func(t *testing.T) {
			w, head, _, blobs := cloneFixture(t)
			stage, e := w.PrepareClone("main", head, nil)
			check(t, e)
			defer stage.Close()
			if workerError {
				// An unavailable staging directory must propagate the worker
				// error and drain the remaining work without deadlocking.
				check(t, stage.root.RemoveAll(stage.stage))
				for id, data := range blobs {
					if stage.AddVerified(id, data) != nil {
						break
					}
				}
			}
			if e = stage.Publish(); e == nil {
				t.Fatal("incomplete or failed staging was published")
			}
			check(t, w.Ready())
			if got, e := w.Head(); e != nil || got != "" {
				t.Fatal("failed clone moved its branch")
			}
		})
	}
}

func TestCloneBatchProtectsUntrackedPathsAndSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			w, head, _, blobs := cloneFixture(t)
			stage, e := w.PrepareClone("main", head, nil)
			check(t, e)
			defer stage.Close()
			for id, data := range blobs {
				check(t, stage.AddVerified(id, data))
			}
			outside := t.TempDir()
			if symlink {
				check(t, os.Symlink(outside, filepath.Join(w.Root, "dir-0")))
			} else {
				write(t, w, "dir-0/nested/file-000", "keep me")
			}
			if e = stage.Publish(); e == nil {
				t.Fatal("clone overwrote an untracked path or followed a symlink")
			}
			if !symlink && cloneRead(t, w, "dir-0/nested/file-000") != "keep me" {
				t.Fatal("clone damaged an untracked file")
			}
			entries, e := os.ReadDir(outside)
			check(t, e)
			if len(entries) != 0 {
				t.Fatal("clone wrote outside the workspace")
			}
			check(t, w.Ready())
		})
	}
}

func TestCloneBatchRejectsDuplicateAndUnexpectedBlobs(t *testing.T) {
	w, head, _, blobs := cloneFixture(t)
	stage, e := w.PrepareClone("main", head, nil)
	check(t, e)
	defer stage.Close()
	if e = stage.AddVerified(strings.Repeat("0", 64), nil); e == nil {
		t.Fatal("unknown blob accepted")
	}
	for id, data := range blobs {
		check(t, stage.AddVerified(id, data))
		if e = stage.AddVerified(id, data); e == nil {
			t.Fatal("duplicate blob accepted")
		}
		break
	}
}

func TestCloneBatchDetectsDirectoryReplacementDuringPublication(t *testing.T) {
	w, head, _, blobs := cloneFixture(t)
	stage, e := w.PrepareClone("main", head, nil)
	check(t, e)
	defer stage.Close()
	for id, data := range blobs {
		check(t, stage.AddVerified(id, data))
	}
	outside := t.TempDir()
	replaced := false
	stage.afterPublish = func(p string) error {
		if replaced || !strings.HasPrefix(p, "dir-0/") {
			return nil
		}
		replaced = true
		if e := os.Rename(filepath.Join(w.Root, "dir-0"), filepath.Join(w.Root, "moved")); e != nil {
			return e
		}
		return os.Symlink(outside, filepath.Join(w.Root, "dir-0"))
	}
	if e = stage.Publish(); e == nil {
		t.Fatal("directory replacement was accepted")
	}
	if got, e := w.Head(); e != nil || got != "" {
		t.Fatal("directory replacement published a branch")
	}
	entries, e := os.ReadDir(outside)
	check(t, e)
	if len(entries) != 0 {
		t.Fatal("publication followed the replacement symlink")
	}
}
