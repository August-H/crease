package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"crease/internal/transport"
	"crease/internal/workspace"
)

func (a *app) remote(w *workspace.Workspace, args []string) error {
	configured := w.Config("remote")
	if len(args) == 0 || len(args) == 1 && (args[0] == "-v" || args[0] == "--verbose") {
		if configured != "" {
			if len(args) == 0 {
				fmt.Fprintln(a.out, "origin")
			} else {
				fmt.Fprintf(a.out, "origin\t%s (fetch)\norigin\t%s (push)\n", configured, configured)
			}
		}
		return nil
	}
	op := args[0]
	if op == "set" && len(args) == 2 {
		if _, err := transport.Parse(args[1]); err != nil {
			return err
		}
		return w.SetConfig("remote", args[1])
	}
	if len(args) < 2 || args[1] != "origin" {
		return errors.New("this release supports one remote named 'origin'")
	}
	switch op {
	case "add", "set-url":
		if len(args) != 3 {
			return fmt.Errorf("usage: crease remote %s origin URL", op)
		}
		if op == "add" && configured != "" {
			return errors.New("remote origin already exists")
		}
		if op == "set-url" && configured == "" {
			return errors.New("no such remote: 'origin'")
		}
		if _, err := transport.Parse(args[2]); err != nil {
			return err
		}
		return w.SetConfig("remote", args[2])
	case "get-url":
		if len(args) != 2 {
			return errors.New("usage: crease remote get-url origin")
		}
		if configured == "" {
			return errors.New("no such remote: 'origin'")
		}
		fmt.Fprintln(a.out, configured)
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			return errors.New("usage: crease remote remove origin")
		}
		if configured == "" {
			return errors.New("no such remote: 'origin'")
		}
		tx, err := w.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`DELETE FROM config WHERE key='remote' OR key LIKE 'upstream:%'; DELETE FROM remote_refs; DELETE FROM fetched_heads`); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("usage: crease remote [-v] | add origin URL | set-url origin URL | get-url origin | remove origin")
	}
}

func (a *app) config(w *workspace.Workspace, args []string) error {
	f := a.flags("config")
	get := f.Bool("get", false, "read a value")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() < 1 || f.NArg() > 2 || *get && f.NArg() != 1 {
		return errors.New("usage: crease config [--get] user.name|user.email|author [VALUE]")
	}
	key := f.Arg(0)
	if key != "author" && key != "user.name" && key != "user.email" {
		return errors.New("supported config keys: user.name, user.email, author; settings are local to this repository")
	}
	if f.NArg() == 1 {
		value := w.Config(key)
		if value == "" {
			return &ExitError{Message: "configuration value is not set"}
		}
		fmt.Fprintln(a.out, value)
		return nil
	}
	if strings.TrimSpace(f.Arg(1)) == "" || strings.ContainsAny(f.Arg(1), "\r\n") {
		return errors.New("configuration value must be a nonempty single line")
	}
	return w.SetConfig(key, f.Arg(1))
}

func (a *app) add(w *workspace.Workspace, args []string) error {
	f := a.flags("add")
	all := f.Bool("A", false, "track all files")
	f.BoolVar(all, "all", false, "track all files")
	if err := parse(f, args); err != nil {
		return err
	}
	paths := f.Args()
	if len(paths) == 0 {
		if !*all {
			return errors.New("usage: crease add [-A] [--] PATH...")
		}
		paths = []string{w.Root}
	}
	return w.Add(paths, a.cwd)
}

func (a *app) rm(w *workspace.Workspace, args []string) error {
	f := a.flags("rm")
	cached := f.Bool("cached", false, "stop tracking paths while keeping working files")
	if err := parse(f, args); err != nil {
		return err
	}
	if !*cached || f.NArg() == 0 {
		return errors.New("usage: crease rm --cached PATH...; removing working files is not supported by this command")
	}
	return a.untrack(w, f.Args())
}

func (a *app) untrack(w *workspace.Workspace, paths []string) error {
	if len(paths) == 0 {
		return errors.New("usage: crease untrack PATH...")
	}
	var relative []string
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(a.cwd, path)
		}
		rel, err := filepath.Rel(w.Root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, err = w.SafePath(rel); err != nil {
			return err
		}
		relative = append(relative, rel)
	}
	for _, path := range relative {
		if err := w.Untrack(path); err != nil {
			return err
		}
	}
	return nil
}
