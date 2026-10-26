package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crease/internal/diff"
	"crease/internal/model"
	"crease/internal/objectstore"
	"crease/internal/workspace"
)

// ExitError marks a failure already explained on stdout, such as an empty commit.
type ExitError struct{ Message string }

func (e *ExitError) Error() string { return e.Message }

func FormatError(err error) string {
	var reported *ExitError
	if errors.As(err, &reported) {
		return ""
	}
	if strings.HasPrefix(err.Error(), "usage:") || strings.Contains(err.Error(), "flag ") {
		return "error: " + err.Error()
	}
	return "fatal: " + err.Error()
}

func quotePath(s string) string {
	return diff.QuotePath(s)
}

func (a *app) status(w *workspace.Workspace, args []string) error {
	f := a.flags("status")
	shortOutput := f.Bool("short", false, "show short status")
	f.BoolVar(shortOutput, "s", false, "show short status")
	branch := f.Bool("branch", false, "include the branch in short status")
	f.BoolVar(branch, "b", false, "include the branch in short status")
	zero := f.Bool("z", false, "terminate paths with NUL")
	porcelain := f.String("porcelain", "", "stable short format, v1")
	// Git's optional --porcelain argument needs normalization for flag.FlagSet.
	for i, arg := range args {
		if arg == "--porcelain" {
			args = append([]string(nil), args...)
			args[i] = "--porcelain=v1"
		}
	}
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() != 0 || (*porcelain != "" && *porcelain != "v1" && *porcelain != "1") {
		return errors.New("usage: crease status [-sb] [--porcelain[=v1]] [-z]")
	}
	changes, err := w.Status()
	if err != nil {
		return err
	}
	head, err := w.Head()
	if err != nil {
		return err
	}
	if *shortOutput || *porcelain != "" || *zero {
		end := "\n"
		if *zero {
			end = "\x00"
		}
		if *branch {
			name := w.Branch()
			if head == "" {
				name = "No commits yet on " + name
			}
			fmt.Fprint(a.out, "## ", name, end)
		}
		for _, c := range changes {
			code := c.Code + " "
			if c.Code == "?" {
				code = "??"
			}
			path := c.Path
			if !*zero {
				path = quotePath(path)
			}
			fmt.Fprint(a.out, code, " ", path, end)
		}
		return nil
	}
	fmt.Fprintln(a.out, "On branch", w.Branch())
	if upstream := w.Config("upstream:" + w.Branch()); upstream != "" && head != "" {
		remote, err := remoteHead(w, strings.TrimPrefix(upstream, "origin/"))
		if err != nil {
			return err
		}
		if remote == head {
			fmt.Fprintf(a.out, "Your branch is up to date with '%s'.\n", upstream)
		}
	}
	if head == "" {
		fmt.Fprintln(a.out, "\nNo commits yet")
	}
	tracked, untracked := false, false
	for _, c := range changes {
		tracked = tracked || c.Code != "?"
		untracked = untracked || c.Code == "?"
	}
	if tracked {
		fmt.Fprintln(a.out, "\nChanges to be committed:")
		fmt.Fprint(a.out, "  (Crease commits all tracked changes; add tracks new files.)\n\n")
		for _, c := range changes {
			label := map[string]string{"A": "new file:", "M": "modified:", "D": "deleted:"}[c.Code]
			if label != "" {
				fmt.Fprintf(a.out, "\t%-12s%s\n", label, quotePath(c.Path))
			}
		}
	}
	if untracked {
		fmt.Fprintln(a.out, "\nUntracked files:")
		fmt.Fprint(a.out, "  (use \"crease add <file>...\" to include in what will be committed)\n\n")
		for _, c := range changes {
			if c.Code == "?" {
				fmt.Fprintln(a.out, "\t"+quotePath(c.Path))
			}
		}
	}
	switch {
	case tracked:
		fmt.Fprintln(a.out)
	case untracked:
		fmt.Fprintln(a.out, "\nnothing added to commit but untracked files present (use \"crease add\" to track)")
	case head == "":
		fmt.Fprintln(a.out, "\nnothing to commit (create/copy files and use \"crease add\" to track)")
	default:
		fmt.Fprintln(a.out, "nothing to commit, working tree clean")
	}
	return nil
}

func (a *app) commit(w *workspace.Workspace, args []string) error {
	f := a.flags("commit")
	message := f.String("m", "", "commit message")
	f.StringVar(message, "message", "", "commit message")
	all := f.Bool("a", false, "include all tracked changes, Crease's default")
	f.BoolVar(all, "all", false, "include all tracked changes")
	quiet := f.Bool("q", false, "suppress the summary")
	f.BoolVar(quiet, "quiet", false, "suppress the summary")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: crease commit [-a] [-q] -m MESSAGE")
	}
	id, err := w.Commit(*message, a.author(w), nil)
	if err != nil {
		if err.Error() == "nothing to commit" {
			if !*quiet {
				if e := a.status(w, nil); e != nil {
					return e
				}
			}
			return &ExitError{Message: err.Error()}
		}
		return err
	}
	if !*quiet {
		a.commitSummary(w, id, *message)
	}
	return nil
}

func (a *app) commitSummary(w *workspace.Workspace, id, message string) {
	c, err := w.Store.Commit(id)
	root, parent := "", ""
	if err == nil && len(c.Parents) > 0 {
		parent = c.Parents[0]
	} else if err == nil {
		root = " (root-commit)"
	}
	fmt.Fprintf(a.out, "[%s%s %s] %s\n", w.Branch(), root, short(id), strings.SplitN(message, "\n", 2)[0])
	if err != nil {
		return // Presentation must never turn a published commit into a failure.
	}
	left, right, err := w.Store.ChangedSnapshots(parent, id)
	if err != nil {
		return
	}
	changes := objectstore.FileChanges(left, right)
	stats, statsErr := diff.StatsWith(left, right, func(_ string, file objectstore.File, _ bool) ([]byte, error) {
		o, err := w.Store.Get(file.ID)
		return o.Data, err
	})
	if statsErr == nil {
		a.statSummary(stats)
	} else {
		fmt.Fprintf(a.out, " %d %s changed\n", len(changes), plural(len(changes), "file", "files"))
	}
	for _, p := range sortedPaths(changes) {
		before, had := left[p]
		after, has := right[p]
		switch {
		case !had && has:
			fmt.Fprintf(a.out, " create mode %s %s\n", fileMode(after.Mode), quotePath(p))
		case had && !has:
			fmt.Fprintf(a.out, " delete mode %s %s\n", fileMode(before.Mode), quotePath(p))
		case before.Mode != after.Mode:
			fmt.Fprintf(a.out, " mode change %s => %s %s\n", fileMode(before.Mode), fileMode(after.Mode), quotePath(p))
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func sortedPaths(s objectstore.Snapshot) []string {
	paths := make([]string, 0, len(s))
	for path := range s {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (a *app) statSummary(stats []diff.Stat) {
	insertions, deletions := 0, 0
	for _, s := range stats {
		insertions += s.Insertions
		deletions += s.Deletions
	}
	fmt.Fprintf(a.out, " %d %s changed", len(stats), plural(len(stats), "file", "files"))
	if insertions > 0 {
		fmt.Fprintf(a.out, ", %d %s(+)", insertions, plural(insertions, "insertion", "insertions"))
	}
	if deletions > 0 {
		fmt.Fprintf(a.out, ", %d %s(-)", deletions, plural(deletions, "deletion", "deletions"))
	}
	fmt.Fprintln(a.out)
}

func (a *app) diffStat(stats []diff.Stat) {
	if len(stats) == 0 {
		return
	}
	width := 0
	for _, s := range stats {
		width = max(width, len(quotePath(s.Path)))
	}
	width = min(width, 60)
	for _, s := range stats {
		name := quotePath(s.Path)
		if len(name) > 60 {
			name = "..." + name[len(name)-57:]
		}
		if s.Binary {
			fmt.Fprintf(a.out, " %-*s | Bin %d -> %d bytes\n", width, name, s.BeforeBytes, s.AfterBytes)
			continue
		}
		added, removed := s.Insertions, s.Deletions
		if added+removed > 50 {
			added = (added*50 + added + removed - 1) / (added + removed)
			removed = 50 - added
		}
		fmt.Fprintf(a.out, " %-*s | %d %s%s\n", width, name, s.Insertions+s.Deletions, strings.Repeat("+", added), strings.Repeat("-", removed))
	}
	a.statSummary(stats)
}

func fileMode(mode string) string {
	if mode == "exec" {
		return "100755"
	}
	return "100644"
}

func formatCommit(c model.Commit, id string) string {
	date := c.Time
	if t, err := time.Parse(time.RFC3339Nano, date); err == nil {
		date = t.Format("Mon Jan 2 15:04:05 2006 -0700")
	}
	return fmt.Sprintf("commit %s\nAuthor: %s\nDate:   %s\n\n    %s\n\n", id, c.Author, date, strings.ReplaceAll(strings.TrimRight(c.Message, "\n"), "\n", "\n    "))
}

func remoteHead(w *workspace.Workspace, branch string) (string, error) {
	var head string
	err := w.DB.QueryRow(`SELECT head FROM remote_refs WHERE name=?`, branch).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return head, err
}

func (a *app) init(args []string) error {
	f := a.flags("init")
	branch := f.String("b", "main", "initial branch name")
	f.StringVar(branch, "initial-branch", "main", "initial branch name")
	quiet := f.Bool("q", false, "suppress the initialization message")
	f.BoolVar(quiet, "quiet", false, "suppress the initialization message")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 1 || !model.ValidRef(*branch) {
		return errors.New("usage: crease init [-b BRANCH] [-q] [directory]")
	}
	dir := "."
	if f.NArg() == 1 {
		dir = f.Arg(0)
	}
	var w *workspace.Workspace
	var err error
	reinitialized := false
	if _, e := os.Lstat(filepath.Join(dir, ".crease", "state.db")); e == nil {
		w, err = workspace.Find(dir)
		reinitialized = true
	} else {
		w, err = workspace.Init(dir)
	}
	if err != nil {
		return err
	}
	defer w.Close()
	if *branch != "main" && !reinitialized {
		tx, e := w.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`UPDATE branches SET name=? WHERE name='main'`, *branch); e != nil {
			return e
		}
		if _, e = tx.Exec(`UPDATE config SET value=? WHERE key='branch'`, *branch); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if !*quiet {
		if reinitialized {
			fmt.Fprintf(a.out, "Reinitialized existing Crease repository in %s/\n", filepath.Join(w.Root, ".crease"))
		} else {
			fmt.Fprintf(a.out, "Initialized empty Crease repository in %s/\n", filepath.Join(w.Root, ".crease"))
		}
	}
	return nil
}
