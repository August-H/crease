package workspace

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crease/internal/metadata"
	"crease/internal/model"
	"crease/internal/objectstore"
	"golang.org/x/sys/unix"
)

const schema = `
CREATE TABLE IF NOT EXISTS config (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS branches (name TEXT PRIMARY KEY, head TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tracked (path TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS remote_refs (name TEXT PRIMARY KEY, head TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS fetched_heads (head TEXT PRIMARY KEY, full INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS movements (id INTEGER PRIMARY KEY, branch TEXT NOT NULL, old TEXT, head TEXT, time TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS file_cache (path TEXT PRIMARY KEY, stamp TEXT NOT NULL, id TEXT NOT NULL, mode TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspace_revision (id INTEGER PRIMARY KEY, generation INTEGER NOT NULL);
INSERT OR IGNORE INTO workspace_revision VALUES (1,0);
CREATE TRIGGER IF NOT EXISTS tracked_insert AFTER INSERT ON tracked BEGIN UPDATE workspace_revision SET generation=generation+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS tracked_delete AFTER DELETE ON tracked BEGIN UPDATE workspace_revision SET generation=generation+1 WHERE id=1; END;
`

type Workspace struct {
	Root            string
	DB              *sql.DB
	Store           objectstore.Store
	lock            *os.File
	cache           map[string]cacheEntry
	indexDirty      bool
	indexLoaded     bool
	indexGeneration int64
	trackedCache    []string
	discovered      []string
	discoveryReady  bool
	monitorEpoch    string
	monitorSeq      uint64
	indexSparse     string
	baseID          string
	base            objectstore.Snapshot
}
type cacheEntry struct {
	stamp, id, mode string
	deleted         bool
}
type Change struct {
	Code string
	Path string
}
type Journal struct {
	From   objectstore.Snapshot `json:"from"`
	To     objectstore.Snapshot `json:"to"`
	Full   objectstore.Snapshot `json:"full"`
	Branch string               `json:"branch"`
	Head   string               `json:"head"`
	Move   bool                 `json:"move"`
	Sparse []string             `json:"sparse"`
}
type MergeState struct {
	Branch, Head, Other, Ref string
	Sparse                   []string
}

func (w *Workspace) MergeState() (*MergeState, error) {
	b, e := os.ReadFile(filepath.Join(w.Root, ".crease", "merge.json"))
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var m MergeState
	if e = json.Unmarshal(b, &m); e != nil {
		return nil, e
	}
	if !model.ValidRef(m.Branch) || !model.ValidID(m.Head) || !model.ValidID(m.Other) {
		return nil, errors.New("invalid pending merge")
	}
	return &m, nil
}
func (w *Workspace) BeginMerge(m MergeState) error {
	if old, e := w.MergeState(); e != nil {
		return e
	} else if old != nil {
		return errors.New("a merge is already pending")
	}
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	return objectstore.Atomic(filepath.Join(w.Root, ".crease", "merge.json"), b, 0600)
}
func (w *Workspace) ClearMerge() error {
	e := os.Remove(filepath.Join(w.Root, ".crease", "merge.json"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	return objectstore.SyncDir(filepath.Join(w.Root, ".crease"))
}

func Init(dir string) (*Workspace, error) {
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	meta := filepath.Join(abs, ".crease")
	if _, e = os.Lstat(meta); !os.IsNotExist(e) {
		return nil, errors.New("repository already exists or metadata cannot be accessed")
	}
	if e = os.MkdirAll(abs, 0755); e != nil {
		return nil, e
	}
	if e = os.Mkdir(meta, 0700); e != nil {
		return nil, e
	}
	w, e := open(abs)
	if e != nil {
		return nil, e
	}
	if _, e = w.DB.Exec(`INSERT INTO config VALUES ('branch','main'); INSERT INTO config VALUES ('sparse','[]'); INSERT INTO branches VALUES ('main','')`); e != nil {
		w.Close()
		return nil, e
	}
	return w, nil
}
func Find(dir string) (*Workspace, error) {
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	for {
		meta := filepath.Join(abs, ".crease")
		if st, e := os.Lstat(meta); e == nil {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("invalid metadata directory")
			}
			// The user's SSH trust settings also live in ~/.crease. Only a
			// workspace database identifies a repository; discovery must not
			// create one in a directory that merely holds client settings.
			if state, e := os.Lstat(filepath.Join(meta, "state.db")); e == nil {
				if !state.Mode().IsRegular() {
					return nil, fmt.Errorf("invalid workspace database at %s", meta)
				}
				w, e := open(abs)
				if e != nil {
					return nil, e
				}
				if _, e = w.Head(); e != nil {
					w.Close()
					return nil, e
				}
				return w, nil
			} else if !os.IsNotExist(e) {
				return nil, e
			}
		} else if !os.IsNotExist(e) {
			return nil, e
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return nil, errors.New("not inside a Crease repository; run crease init in your project directory")
		}
		abs = parent
	}
}
func open(root string) (*Workspace, error) {
	meta := filepath.Join(root, ".crease")
	f, e := os.OpenFile(filepath.Join(meta, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another Crease command is using this workspace")
	}
	db, e := metadata.Open(filepath.Join(meta, "state.db"), schema)
	if e != nil {
		f.Close()
		return nil, e
	}
	w := &Workspace{Root: root, DB: db, Store: objectstore.New(filepath.Join(meta, "objects")), lock: f, cache: map[string]cacheEntry{}, indexGeneration: -1}
	return w, nil
}
func (w *Workspace) Close() {
	// The workspace index is a disposable hint. Reference and object durability
	// is established by their own publication paths, never by this cache.
	_ = w.Store.Flush()
	if w.indexDirty {
		_ = w.saveIndex()
	}
	w.DB.Close()
	unix.Flock(int(w.lock.Fd()), unix.LOCK_UN)
	w.lock.Close()
}
func (w *Workspace) Config(k string) string {
	var v string
	_ = w.DB.QueryRow(`SELECT value FROM config WHERE key=?`, k).Scan(&v)
	return v
}
func (w *Workspace) SetConfig(k, v string) error {
	if k == "remote" && w.Config(k) != v {
		tx, e := w.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`DELETE FROM remote_refs; DELETE FROM fetched_heads`); e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO config VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); e != nil {
			return e
		}
		return tx.Commit()
	}
	_, e := w.DB.Exec(`INSERT INTO config VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return e
}
func (w *Workspace) Branch() string { return w.Config("branch") }
func (w *Workspace) Head() (string, error) {
	var branch string
	e := w.DB.QueryRow(`SELECT value FROM config WHERE key='branch'`).Scan(&branch)
	if errors.Is(e, sql.ErrNoRows) || e == nil && !model.ValidRef(branch) {
		return "", fmt.Errorf("metadata at %s has no valid current branch; run crease init in your project directory", filepath.Join(w.Root, ".crease"))
	}
	if e != nil {
		return "", fmt.Errorf("read current branch: %w", e)
	}
	head, e := objectstore.Ref(w.DB, branch)
	if errors.Is(e, sql.ErrNoRows) {
		return "", fmt.Errorf("current branch %q is missing from repository metadata at %s", branch, filepath.Join(w.Root, ".crease"))
	}
	return head, e
}
func (w *Workspace) Sparse() []string {
	var p []string
	_ = json.Unmarshal([]byte(w.Config("sparse")), &p)
	return p
}
func Selected(p string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, s := range prefixes {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}
func visible(s objectstore.Snapshot, p []string) objectstore.Snapshot {
	out := objectstore.Snapshot{}
	for k, v := range s {
		if Selected(k, p) {
			out[k] = v
		}
	}
	return out
}
func (w *Workspace) Ready() error {
	if _, e := os.Stat(filepath.Join(w.Root, ".crease", "journal.json")); e == nil {
		return errors.New("interrupted workspace update; run crease recover before continuing")
	}
	return nil
}
func (w *Workspace) Tracked() ([]string, error) {
	if e := w.loadIndex(); e != nil {
		return nil, e
	}
	var generation int64
	if e := w.DB.QueryRow(`SELECT generation FROM workspace_revision WHERE id=1`).Scan(&generation); e != nil {
		return nil, e
	}
	if w.trackedCache != nil && generation == w.indexGeneration {
		return w.trackedCache, nil
	}
	rows, e := w.DB.Query(`SELECT path FROM tracked ORDER BY path`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	w.trackedCache = out
	w.indexGeneration = generation
	w.monitorEpoch = ""
	w.indexDirty = true
	return out, nil
}

// SafePath refuses symlinks in every existing component, including the leaf.
func (w *Workspace) SafePath(p string) (string, error) {
	if !model.ValidPath(p) {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	parts := strings.Split(p, "/")
	cur := w.Root
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		st, e := os.Lstat(cur)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlinks are unsupported: %s", p)
		}
	}
	return filepath.Join(w.Root, filepath.FromSlash(p)), nil
}
func (w *Workspace) ReadFile(p string) (objectstore.File, error) {
	s, e := w.newScan()
	if e != nil {
		return objectstore.File{}, e
	}
	defer s.close()
	f, e := s.read(p, true)
	if e == nil {
		e = s.check()
	}
	return f, e
}
func (w *Workspace) ignored(p string, isDir bool, patterns []string) bool {
	if !model.ValidPath(p) {
		return true
	}
	for _, line := range patterns {
		pat := strings.TrimSpace(line)
		if pat == "" || strings.HasPrefix(pat, "#") {
			continue
		}
		dirOnly := strings.HasSuffix(pat, "/")
		pat = strings.Trim(pat, "/")
		if p == pat || strings.HasPrefix(p, pat+"/") {
			if !dirOnly || isDir || p != pat {
				return true
			}
		}
		if !dirOnly || isDir {
			if ok, _ := path.Match(pat, p); ok {
				return true
			}
			if !strings.Contains(pat, "/") {
				for _, part := range strings.Split(p, "/") {
					if ok, _ := path.Match(pat, part); ok {
						return true
					}
				}
			}
		}
	}
	return false
}
func (w *Workspace) discover(start string) ([]string, error) {
	var out []string
	prefix := w.Sparse()
	b, _ := os.ReadFile(filepath.Join(w.Root, ".creaseignore"))
	patterns := strings.Split(string(b), "\n")
	e := filepath.WalkDir(start, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(w.Root, file)
		if e != nil {
			return e
		}
		p := filepath.ToSlash(rel)
		if p == "." {
			return nil
		}
		if w.ignored(p, d.IsDir(), patterns) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are unsupported: %s", p)
		}
		if !d.IsDir() && Selected(p, prefix) {
			out = append(out, p)
		}
		return nil
	})
	return out, e
}
func (w *Workspace) Add(args []string, cwd string) error {
	if e := w.Ready(); e != nil {
		return e
	}
	var all []string
	for _, arg := range args {
		file := arg
		if !filepath.IsAbs(file) {
			file = filepath.Join(cwd, arg)
		}
		file = filepath.Clean(file)
		rel, e := filepath.Rel(w.Root, file)
		if e != nil {
			return e
		}
		if rel != "." {
			if _, e = w.SafePath(filepath.ToSlash(rel)); e != nil {
				return e
			}
		}
		files, e := w.discover(file)
		if e != nil {
			return e
		}
		all = append(all, files...)
	}
	tx, e := w.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, p := range all {
		if _, e = tx.Exec(`INSERT OR IGNORE INTO tracked VALUES (?)`, p); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (w *Workspace) baseSnapshot(head string) (objectstore.Snapshot, error) {
	if e := w.loadIndex(); e != nil {
		return nil, e
	}
	if w.base == nil || w.baseID != head {
		base, e := w.Store.Snapshot(head)
		if e != nil {
			return nil, e
		}
		w.baseID = head
		w.base = base
		w.indexDirty = true
	}
	return w.base, nil
}
func (w *Workspace) CommittedSnapshot(head string) (objectstore.Snapshot, error) {
	current, e := w.Head()
	if e != nil {
		return nil, e
	}
	if current == head {
		return w.baseSnapshot(head)
	}
	return w.Store.Snapshot(head)
}
func (w *Workspace) Snapshot() (objectstore.Snapshot, error)        { return w.workingSnapshot(true) }
func (w *Workspace) InspectSnapshot() (objectstore.Snapshot, error) { return w.workingSnapshot(false) }
func (w *Workspace) workingSnapshot(persist bool) (objectstore.Snapshot, error) {
	head, e := w.Head()
	if e != nil {
		return nil, e
	}
	base, e := w.baseSnapshot(head)
	if e != nil {
		return nil, e
	}
	tracked, e := w.Tracked()
	if e != nil {
		return nil, e
	}
	return w.snapshotFrom(base, tracked, persist)
}
func (w *Workspace) snapshotFrom(base objectstore.Snapshot, tracked []string, persist bool) (objectstore.Snapshot, error) {
	scan, e := w.newScan()
	if e != nil {
		return nil, e
	}
	defer scan.close()
	out := maps.Clone(base)
	known := make(map[string]bool, len(tracked))
	prefixes := w.Sparse()
	for _, p := range tracked {
		known[p] = true
	}
	for p := range out {
		if Selected(p, prefixes) && !known[p] {
			delete(out, p)
		}
	}
	events := w.monitorChanges()
	dirty := eventSet(events)
	for _, p := range tracked {
		if !Selected(p, prefixes) {
			continue
		}
		if events != nil && !events.Full && !candidate(p, dirty) {
			if c, ok := w.cache[p]; ok {
				if c.deleted {
					delete(out, p)
					continue
				}
				file := objectstore.File{ID: c.id, Mode: c.mode}
				if !persist || file == base[p] || w.Store.Exists(c.id) {
					out[p] = file
					continue
				}
			}
		}
		f, e := scan.read(p, persist)
		if os.IsNotExist(e) {
			delete(out, p)
			if c := w.cache[p]; !c.deleted {
				c.deleted = true
				w.cache[p] = c
				w.indexDirty = true
			}
			continue
		}
		if e != nil {
			return nil, e
		}
		out[p] = f
	}
	if e = scan.check(); e != nil {
		return nil, e
	}
	if events != nil {
		if e = w.updateDiscovery(events); e != nil {
			w.monitorEpoch = ""
			w.discoveryReady = false
			return nil, e
		}
	}

	return out, nil
}
func (w *Workspace) Status() ([]Change, error) {
	if e := w.Ready(); e != nil {
		return nil, e
	}
	head, e := w.Head()
	if e != nil {
		return nil, e
	}
	base, e := w.baseSnapshot(head)
	if e != nil {
		return nil, e
	}
	tracked, e := w.Tracked()
	if e != nil {
		return nil, e
	}
	current, e := w.snapshotFrom(base, tracked, false)
	if e != nil {
		return nil, e
	}
	var out []Change
	prefixes := w.Sparse()
	for p, f := range current {
		if !Selected(p, prefixes) {
			continue
		}
		if b, ok := base[p]; !ok {
			out = append(out, Change{"A", p})
		} else if b != f {
			out = append(out, Change{"M", p})
		}
	}
	for p := range base {
		if _, ok := current[p]; !ok && Selected(p, prefixes) {
			out = append(out, Change{"D", p})
		}
	}
	known := map[string]bool{}
	for _, p := range tracked {
		known[p] = true
	}
	files := w.discovered
	if !w.discoveryReady {
		files, e = w.discover(w.Root)
		if e != nil {
			return nil, e
		}
	}
	for _, p := range files {
		if !known[p] {
			out = append(out, Change{"?", p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func (w *Workspace) Clean() error {
	changes, e := w.Status()
	if e != nil {
		return e
	}
	for _, c := range changes {
		if c.Code != "?" {
			return errors.New("workspace has tracked changes; commit or explicitly restore them first")
		}
	}
	return nil
}
func (w *Workspace) Commit(message, author string, parents []string) (string, error) {
	if e := w.Ready(); e != nil {
		return "", e
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("commit message is required")
	}
	head, e := w.Head()
	if e != nil {
		return "", e
	}
	pending, e := w.MergeState()
	if e != nil {
		return "", e
	}
	if pending != nil {
		if pending.Branch != w.Branch() {
			return "", errors.New("pending merge belongs to a different branch")
		}
		if pending.Head != head {
			c, e := w.Store.Commit(head)
			if e != nil {
				return "", e
			}
			if len(c.Parents) == 2 && c.Parents[0] == pending.Head && c.Parents[1] == pending.Other {
				if e = w.ClearMerge(); e != nil {
					return "", e
				}
				pending = nil
			} else {
				return "", errors.New("branch changed during pending merge")
			}
		} else {
			parents = []string{head, pending.Other}
		}
	}
	snapshot, e := w.Snapshot()
	if e != nil {
		return "", e
	}
	base, e := w.baseSnapshot(head)
	if e != nil {
		return "", e
	}
	changes := objectstore.FileChanges(base, snapshot)
	root := ""
	if head != "" {
		cm, err := w.Store.Commit(head)
		if err != nil {
			return "", err
		}
		root = cm.Tree
	}
	tree, e := w.Store.UpdateTree(root, changes)
	if e != nil {
		return "", e
	}
	if parents == nil {
		parents = []string{}
		if head != "" {
			parents = append(parents, head)
		}
	}
	if head != "" && len(parents) < 2 {
		old, e := w.Store.Commit(head)
		if e != nil {
			return "", e
		}
		if old.Tree == tree {
			return "", errors.New("nothing to commit")
		}
	}
	id, e := w.Store.PutJSON("commit", model.Commit{Tree: tree, Parents: parents, Author: author, Time: time.Now().UTC().Format(time.RFC3339Nano), Message: message})
	if e != nil {
		return "", e
	}
	if e = w.Store.Flush(); e != nil {
		return "", e
	}
	branch := w.Branch()
	tx, e := w.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`UPDATE branches SET head=? WHERE name=? AND head=?`, id, branch, head); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`INSERT INTO movements(branch,old,head,time) VALUES (?,?,?,?)`, branch, head, id, time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
		return "", e
	}
	if e = replaceTracked(tx, snapshot); e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	w.baseID = id
	w.base = snapshot
	w.indexDirty = true
	if pending != nil {
		if e = w.ClearMerge(); e != nil {
			return id, e
		}
	}
	return id, nil
}
func replaceTracked(tx *sql.Tx, s objectstore.Snapshot) error {
	rows, e := tx.Query(`SELECT path FROM tracked`)
	if e != nil {
		return e
	}
	old := map[string]bool{}
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			rows.Close()
			return e
		}
		old[p] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	var insert, remove *sql.Stmt
	defer func() {
		if insert != nil {
			insert.Close()
		}
		if remove != nil {
			remove.Close()
		}
	}()
	for p := range s {
		if old[p] {
			continue
		}
		if insert == nil {
			insert, e = tx.Prepare(`INSERT INTO tracked VALUES (?)`)
			if e != nil {
				return e
			}
		}
		if _, e = insert.Exec(p); e != nil {
			return e
		}
	}
	for p := range old {
		if _, ok := s[p]; ok {
			continue
		}
		if remove == nil {
			remove, e = tx.Prepare(`DELETE FROM tracked WHERE path=?`)
			if e != nil {
				return e
			}
		}
		if _, e = remove.Exec(p); e != nil {
			return e
		}
	}
	return nil
}
func (w *Workspace) CreateBranch(name string) error {
	if e := w.Ready(); e != nil {
		return e
	}
	if !model.ValidRef(name) {
		return errors.New("invalid branch name")
	}
	head, e := w.Head()
	if e != nil {
		return e
	}
	_, e = w.DB.Exec(`INSERT INTO branches VALUES (?,?)`, name, head)
	return e
}
func (w *Workspace) Switch(name string) error {
	head, e := objectstore.Ref(w.DB, name)
	if e != nil {
		return e
	}
	return w.Checkout(name, head, true, false, w.Sparse())
}

func (w *Workspace) Checkout(branch, head string, move, force bool, sparse []string) error {
	if e := w.Ready(); e != nil {
		return e
	}
	if !force {
		if e := w.Clean(); e != nil {
			return e
		}
	}
	from, e := w.Snapshot()
	if e != nil {
		return e
	}
	full, e := w.Store.Snapshot(head)
	if e != nil {
		return e
	}
	j := Journal{From: visible(from, w.Sparse()), To: visible(full, sparse), Full: full, Branch: branch, Head: head, Move: move, Sparse: sparse}
	for p := range j.To {
		file, e := w.SafePath(p)
		if e != nil {
			return e
		}
		if _, ok := j.From[p]; !ok {
			if _, e = os.Lstat(file); e == nil {
				return fmt.Errorf("untracked path would be overwritten: %s", p)
			} else if !os.IsNotExist(e) {
				return e
			}
		}
		if _, e = w.Store.Get(j.To[p].ID); e != nil {
			return e
		}
	}
	if e = w.Store.Flush(); e != nil {
		return e
	}
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	if e = objectstore.Atomic(filepath.Join(w.Root, ".crease", "journal.json"), b, 0600); e != nil {
		return e
	}
	return w.apply(j, false)
}
func (w *Workspace) Recover() error {
	b, e := os.ReadFile(filepath.Join(w.Root, ".crease", "journal.json"))
	if e != nil {
		return e
	}
	var j Journal
	if e = json.Unmarshal(b, &j); e != nil {
		return e
	}
	return w.apply(j, true)
}
func (w *Workspace) apply(j Journal, recovering bool) error {
	// Validate everything before a journal can remove or overwrite a path.
	if !model.ValidRef(j.Branch) || (j.Head != "" && !model.ValidID(j.Head)) {
		return errors.New("invalid operation journal reference")
	}
	for _, p := range j.Sparse {
		if !model.ValidPath(p) {
			return errors.New("invalid operation journal selection")
		}
	}
	full, e := w.Store.Snapshot(j.Head)
	if e != nil {
		return e
	}
	if !maps.Equal(full, j.Full) || !maps.Equal(visible(full, j.Sparse), j.To) {
		return errors.New("operation journal does not match its commit")
	}
	for _, s := range []objectstore.Snapshot{j.From, j.To, j.Full} {
		for p, f := range s {
			if _, e := w.SafePath(p); e != nil {
				return e
			}
			if !model.ValidID(f.ID) || (f.Mode != "file" && f.Mode != "exec") {
				return errors.New("invalid operation journal")
			}
		}
	}
	for _, f := range j.To {
		o, e := w.Store.Get(f.ID)
		if e != nil {
			return e
		}
		if o.Type != "blob" {
			return errors.New("expected blob")
		}
	}
	if recovering {
		for p := range union(j.From, j.To) {
			f, e := w.ReadFile(p)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if f != j.From[p] && f != j.To[p] {
				return fmt.Errorf("path changed after interruption; save it elsewhere before recovering: %s", p)
			}
		}
	}
	var removed []string
	for p := range j.From {
		if _, ok := j.To[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(removed)))
	for _, p := range removed {
		file, e := w.SafePath(p)
		if e != nil {
			return e
		}
		if e = os.Remove(file); e != nil && !os.IsNotExist(e) {
			return e
		}
		for dir := filepath.Dir(file); dir != w.Root; dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break
			}
		}
	}
	for p, f := range j.To {
		if old, ok := j.From[p]; ok && old == f && !recovering {
			continue
		}
		file, e := w.SafePath(p)
		if e != nil {
			return e
		}
		o, e := w.Store.Get(f.ID)
		if e != nil {
			return e
		}
		if o.Type != "blob" {
			return errors.New("expected blob")
		}
		mode := os.FileMode(0644)
		if f.Mode == "exec" {
			mode = 0755
		}
		if e = objectstore.Atomic(file, o.Data, mode); e != nil {
			return e
		}
	}
	tx, e := w.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = replaceTracked(tx, j.Full); e != nil {
		return e
	}
	if j.Move {
		var old string
		_ = tx.QueryRow(`SELECT head FROM branches WHERE name=?`, j.Branch).Scan(&old)
		if _, e = tx.Exec(`INSERT INTO branches VALUES (?,?) ON CONFLICT(name) DO UPDATE SET head=excluded.head`, j.Branch, j.Head); e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO config VALUES ('branch',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, j.Branch); e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO movements(branch,old,head,time) VALUES (?,?,?,?)`, j.Branch, old, j.Head, time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
			return e
		}
	}
	b, _ := json.Marshal(j.Sparse)
	if _, e = tx.Exec(`UPDATE config SET value=? WHERE key='sparse'`, string(b)); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(w.Root, ".crease", "journal.json")); e != nil {
		return e
	}
	return objectstore.SyncDir(filepath.Join(w.Root, ".crease"))
}
func union(a, b objectstore.Snapshot) map[string]bool {
	out := map[string]bool{}
	for p := range a {
		out[p] = true
	}
	for p := range b {
		out[p] = true
	}
	return out
}

func (w *Workspace) SetSparse(paths []string) error {
	for _, p := range paths {
		if !model.ValidPath(p) {
			return fmt.Errorf("invalid sparse path %q", p)
		}
	}
	head, e := w.Head()
	if e != nil {
		return e
	}
	return w.Checkout(w.Branch(), head, true, false, paths)
}
func (w *Workspace) Untrack(p string) error {
	if !model.ValidPath(p) {
		return errors.New("invalid path")
	}
	_, e := w.DB.Exec(`DELETE FROM tracked WHERE path=? OR substr(path,1,?)=?`, p, len(p)+1, p+"/")
	return e
}
