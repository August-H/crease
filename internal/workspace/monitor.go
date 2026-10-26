package workspace

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"crease/internal/monitor"
)

func touched(p string, events []monitor.Event) bool {
	for _, ev := range events {
		if ev.Path == "" || p == ev.Path || strings.HasPrefix(p, ev.Path+"/") {
			return true
		}
	}
	return false
}
func (w *Workspace) monitorChanges() *monitor.Response {
	sparse := w.Config("sparse")
	if sparse != w.indexSparse {
		w.monitorEpoch = ""
		w.discoveryReady = false
		w.indexSparse = sparse
		w.indexDirty = true
	}
	r, e := monitor.Query(w.Root, "changes", w.monitorEpoch, w.monitorSeq)
	if e != nil {
		if w.monitorEpoch != "" {
			w.monitorEpoch = ""
			w.indexDirty = true
		}
		w.discoveryReady = false
		return nil
	}
	return &r
}
func (w *Workspace) updateDiscovery(r *monitor.Response) error {
	if r.Full || !w.discoveryReady || touched(".creaseignore", r.Events) {
		files, e := w.discover(w.Root)
		if e != nil {
			return e
		}
		w.discovered = files
	} else if len(r.Events) > 0 {
		// Coalesce child events under dirty parents before rescanning directories.
		prefixes := make([]string, 0, len(r.Events))
		for _, ev := range r.Events {
			prefixes = append(prefixes, ev.Path)
		}
		sort.Strings(prefixes)
		var roots []string
		chosen := map[string]bool{}
		for _, p := range prefixes {
			if !candidate(p, chosen) {
				roots = append(roots, p)
				chosen[p] = true
			}
		}
		files := map[string]bool{}
		for _, p := range w.discovered {
			files[p] = true
		}
		for _, prefix := range roots {
			for p := range files {
				if prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/") {
					delete(files, p)
				}
			}
			// A removed path has no replacement discovery entries. A symlink or
			// permission error still fails inspection instead of hiding it.
			found, e := w.discover(filepath.Join(w.Root, filepath.FromSlash(prefix)))
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			for _, p := range found {
				files[p] = true
			}
		}
		w.discovered = w.discovered[:0]
		for p := range files {
			w.discovered = append(w.discovered, p)
		}
		sort.Strings(w.discovered)
	}
	if w.monitorEpoch != r.Epoch || w.monitorSeq != r.Seq || !w.discoveryReady {
		w.indexDirty = true
	}
	w.monitorEpoch = r.Epoch
	w.monitorSeq = r.Seq
	w.discoveryReady = true
	return nil
}

// eventSet makes matching candidate paths proportional to path depth instead
// of the number of journal events during large directory updates.
func eventSet(r *monitor.Response) map[string]bool {
	out := map[string]bool{}
	if r != nil {
		for _, ev := range r.Events {
			out[ev.Path] = true
		}
	}
	return out
}
func candidate(p string, dirty map[string]bool) bool {
	for {
		if dirty[p] {
			return true
		}
		if p == "" {
			return false
		}
		p = path.Dir(p)
		if p == "." {
			p = ""
		}
	}
}
