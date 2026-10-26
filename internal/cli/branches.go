package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"crease/internal/model"
	"crease/internal/syncer"
	"crease/internal/workspace"
)

func (a *app) branch(w *workspace.Workspace, args []string) error {
	// Preserve the original command, which both creates and switches.
	if len(args) >= 1 && args[0] == "create" {
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: crease branch create NAME [REF]")
		}
		start := ""
		if len(args) == 3 {
			start = args[2]
		}
		return a.createBranch(w, args[1], start, true)
	}
	if len(args) == 1 && args[0] == "list" {
		args = nil
	}
	f := a.flags("branch")
	verbose := f.Bool("v", false, "show commit IDs and subjects")
	f.BoolVar(verbose, "verbose", false, "show commit IDs and subjects")
	list := f.Bool("list", false, "list branches")
	remove := f.Bool("d", false, "delete a fully merged branch")
	f.BoolVar(remove, "delete", false, "delete a fully merged branch")
	force := f.Bool("D", false, "delete a branch even if unmerged")
	move := f.Bool("m", false, "rename a branch")
	f.BoolVar(move, "move", false, "rename a branch")
	if err := parse(f, args); err != nil {
		return err
	}
	operations := 0
	for _, active := range []bool{*remove || *force, *move, *list} {
		if active {
			operations++
		}
	}
	if operations > 1 {
		return errors.New("choose one branch operation")
	}
	if *remove || *force {
		if f.NArg() != 1 {
			return errors.New("usage: crease branch -d|-D NAME")
		}
		name := f.Arg(0)
		if name == w.Branch() {
			return fmt.Errorf("cannot delete branch '%s' checked out at '%s'", name, w.Root)
		}
		var head string
		if err := w.DB.QueryRow(`SELECT head FROM branches WHERE name=?`, name).Scan(&head); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("branch '%s' not found", name)
			}
			return err
		}
		if !*force && head != "" {
			current, err := w.Head()
			if err != nil {
				return err
			}
			merged, err := w.Store.IsAncestor(head, current)
			if err != nil {
				return err
			}
			if !merged {
				return fmt.Errorf("the branch '%s' is not fully merged; use crease branch -D %s to delete it", name, name)
			}
		}
		tx, err := w.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`DELETE FROM branches WHERE name=?`, name); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM config WHERE key=?`, "upstream:"+name); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Deleted branch %s (was %s).\n", name, short(head))
		return nil
	}
	if *move {
		if f.NArg() < 1 || f.NArg() > 2 {
			return errors.New("usage: crease branch -m [OLD] NEW")
		}
		old, name := w.Branch(), f.Arg(0)
		if f.NArg() == 2 {
			old, name = f.Arg(0), f.Arg(1)
		}
		if !model.ValidRef(name) {
			return errors.New("invalid branch name")
		}
		tx, err := w.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.Exec(`UPDATE branches SET name=? WHERE name=?`, name, old)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("branch '%s' not found", old)
		}
		if _, err = tx.Exec(`UPDATE config SET value=? WHERE key='branch' AND value=?`, name, old); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE config SET key=? WHERE key=?`, "upstream:"+name, "upstream:"+old); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE movements SET branch=? WHERE branch=?`, name, old); err != nil {
			return err
		}
		return tx.Commit()
	}
	if f.NArg() > 0 && !*list {
		if f.NArg() > 2 {
			return errors.New("usage: crease branch NAME [START-POINT]")
		}
		start := ""
		if f.NArg() == 2 {
			start = f.Arg(1)
		}
		return a.createBranch(w, f.Arg(0), start, false)
	}
	if f.NArg() > 0 {
		return errors.New("usage: crease branch [--list] [-v]")
	}
	rows, err := w.DB.Query(`SELECT name,head FROM branches ORDER BY name`)
	if err != nil {
		return err
	}
	// Release the single database connection before reading configuration.
	type entry struct{ name, head string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.name, &e.head); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	current := w.Branch()
	for _, e := range entries {
		prefix := " "
		if e.name == current {
			prefix = "*"
		}
		fmt.Fprintf(a.out, "%s %s", prefix, e.name)
		if *verbose {
			subject := ""
			if e.head != "" {
				c, err := w.Store.Commit(e.head)
				if err != nil {
					return err
				}
				subject = strings.SplitN(c.Message, "\n", 2)[0]
			}
			fmt.Fprintf(a.out, " %s %s", short(e.head), subject)
		}
		fmt.Fprintln(a.out)
	}
	return nil
}

func (a *app) createBranch(w *workspace.Workspace, name, start string, change bool) error {
	if !model.ValidRef(name) {
		return errors.New("invalid branch name")
	}
	head, err := w.Head()
	if err != nil {
		return err
	}
	if start != "" {
		head, err = resolve(w, start)
		if err != nil {
			return err
		}
	}
	if change {
		if err = w.Clean(); err != nil {
			return err
		}
		if err = a.ensure(w, head, w.Sparse()); err != nil {
			return err
		}
	}
	if _, err = w.DB.Exec(`INSERT INTO branches VALUES (?,?)`, name, head); err != nil {
		return fmt.Errorf("cannot create branch '%s': %w", name, err)
	}
	if change {
		if err = w.Switch(name); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Switched to a new branch '%s'\n", name)
	}
	return nil
}

func (a *app) switchBranch(w *workspace.Workspace, command string, args []string) error {
	f := a.flags(command)
	create := f.String("c", "", "create and switch to a branch")
	if command == "checkout" {
		f.StringVar(create, "b", "", "create and switch to a branch")
	} else {
		f.StringVar(create, "create", "", "create and switch to a branch")
	}
	if err := parse(f, args); err != nil {
		return err
	}
	if *create != "" {
		if f.NArg() > 1 {
			return errors.New("provide at most one start point")
		}
		start := ""
		if f.NArg() == 1 {
			start = f.Arg(0)
		}
		return a.createBranch(w, *create, start, true)
	}
	if f.NArg() != 1 {
		return fmt.Errorf("usage: crease %s BRANCH", command)
	}
	name := f.Arg(0)
	if name == w.Branch() {
		fmt.Fprintf(a.out, "Already on '%s'\n", name)
		return nil
	}
	var head string
	err := w.DB.QueryRow(`SELECT head FROM branches WHERE name=?`, name).Scan(&head)
	if err == nil {
		if err = a.ensure(w, head, w.Sparse()); err != nil {
			return err
		}
		if err = w.Switch(name); err != nil {
			return err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if !model.ValidRef(name) {
			return errors.New("invalid branch name")
		}
		c, err := a.connect(w)
		if err != nil {
			return err
		}
		defer c.Close()
		head, err = syncer.Fetch(c, w, name, false)
		if err != nil {
			return err
		}
		if err = w.Checkout(name, head, true, false, w.Sparse()); err != nil {
			return err
		}
		if err = w.SetConfig("upstream:"+name, "origin/"+name); err != nil {
			return err
		}
	} else {
		return err
	}
	fmt.Fprintf(a.out, "Switched to branch '%s'\n", name)
	return nil
}
