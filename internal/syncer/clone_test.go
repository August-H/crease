package syncer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"crease/internal/syncer"
	"crease/internal/transport"
	"crease/internal/workspace"
)

func TestCloneStreamSparseFullAndFallback(t *testing.T) {
	f := setup(t)
	source, e := workspace.Find(filepath.Join(f.root, "source"))
	check(t, e)
	old, e := source.Head()
	check(t, e)
	oldSnapshot, e := source.Store.Snapshot(old)
	check(t, e)
	check(t, os.MkdirAll(filepath.Join(source.Root, "src"), 0755))
	for i := 0; i < 260; i++ {
		check(t, os.WriteFile(filepath.Join(source.Root, "src", fmt.Sprintf("file-%03d", i)), []byte(fmt.Sprintf("payload %d\n", i)), 0644))
	}
	check(t, os.WriteFile(filepath.Join(source.Root, "file"), []byte("new current version\n"), 0644))
	check(t, source.Add([]string{"."}, source.Root))
	head, e := source.Commit("clone stream fixture", "test", nil)
	check(t, e)
	writer, e := transport.Dial(f.url, f.options)
	check(t, e)
	check(t, syncer.Push(writer, source))
	writer.Close()
	source.Close()

	for _, test := range []struct {
		name, capability string
		sparse, full     bool
	}{
		{name: "stream"},
		{name: "sparse", sparse: true},
		{name: "full", full: true},
		{name: "full-sparse", sparse: true, full: true},
		{name: "binary-fallback", capability: "binary"},
		{name: "json-fallback", capability: "json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, e := workspace.Init(filepath.Join(f.root, test.name))
			check(t, e)
			defer w.Close()
			check(t, w.SetConfig("remote", f.url))
			if test.sparse {
				check(t, w.SetConfig("sparse", `["src"]`))
			}
			client, e := transport.Dial(f.url, f.options)
			check(t, e)
			defer client.Close()
			if test.capability == "binary" {
				delete(client.Capabilities, "clone-stream-v1")
			} else if test.capability == "json" {
				client.Capabilities = map[string]bool{}
			}
			client.Stats = transport.Traffic{}
			timings, e := syncer.Clone(client, w, "main", test.full)
			check(t, e)
			if test.capability == "" && (client.Stats.Requests != 1 || timings.Requests != 1 || timings.Mode != "clone-stream-v1") {
				t.Fatalf("clone with 261 files used more than one request: %+v %+v", timings, client.Stats)
			}
			if timings.Workers < 1 || timings.Workers > 8 {
				t.Fatal("checkout worker count is not bounded")
			}
			if got, e := w.Head(); e != nil || got != head {
				t.Fatal("clone published the wrong head")
			}
			for i := 0; i < 260; i++ {
				b, e := os.ReadFile(filepath.Join(w.Root, "src", fmt.Sprintf("file-%03d", i)))
				check(t, e)
				if string(b) != fmt.Sprintf("payload %d\n", i) {
					t.Fatal("clone file contents differ")
				}
			}
			if test.sparse {
				if _, e := os.Stat(filepath.Join(w.Root, "file")); !os.IsNotExist(e) {
					t.Fatal("sparse clone materialized an unselected file")
				}
			}
			if test.capability == "" && w.Store.Has(oldSnapshot["file"].ID) != test.full {
				t.Fatal("historical blob policy differs from --full")
			}
			check(t, w.Ready())
			staging, e := filepath.Glob(filepath.Join(w.Root, ".crease", "clone-stage-*"))
			check(t, e)
			if len(staging) != 0 {
				t.Fatal("clone left staging files after completion")
			}
			t.Logf("%s: %s", test.name, mustCloneJSON(t, timings))
		})
	}
}

func mustCloneJSON(t *testing.T, value any) string {
	t.Helper()
	b, e := json.Marshal(value)
	check(t, e)
	return string(b)
}
