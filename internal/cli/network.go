package cli

import (
	"errors"
	"fmt"

	"crease/internal/syncer"
	"crease/internal/workspace"
)

func (a *app) sync(command string, w *workspace.Workspace, args []string) error {
	f := a.flags(command)
	full := f.Bool("full", false, "download historical blobs")
	commit := f.String("commit", "", "specific commit ID to fetch")
	upstream := f.Bool("u", false, "set upstream after a successful push")
	f.BoolVar(upstream, "set-upstream", false, "set upstream after a successful push")
	ff := f.Bool("ff-only", false, "require a fast-forward, Crease's default")
	quiet := f.Bool("q", false, "suppress transfer summaries")
	f.BoolVar(quiet, "quiet", false, "suppress transfer summaries")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 2 || *upstream && command != "push" || *ff && command != "pull" || *full && command == "push" {
		return fmt.Errorf("usage: crease %s [origin [BRANCH]]", command)
	}
	branch := w.Branch()
	if f.NArg() == 1 {
		if f.Arg(0) != "origin" {
			if command != "fetch" {
				return errors.New("the remote is named 'origin'; use origin BRANCH")
			}
			branch = f.Arg(0) // Original fetch BRANCH syntax remains supported.
		}
	} else if f.NArg() == 2 {
		if f.Arg(0) != "origin" {
			return errors.New("this release supports one remote named 'origin'")
		}
		branch = f.Arg(1)
	}
	if command != "fetch" && branch != w.Branch() {
		return fmt.Errorf("switch to branch '%s' before running crease %s", branch, command)
	}
	if *commit != "" && (command != "fetch" || f.NArg() > 1 || f.NArg() == 1 && f.Arg(0) != "origin") {
		return errors.New("--commit applies only to fetch without a branch")
	}
	if command == "pull" {
		if err := w.Clean(); err != nil {
			return err
		}
	}
	c, err := a.connect(w)
	if err != nil {
		return err
	}
	defer c.Close()
	if *commit != "" {
		if err = syncer.FetchCommit(c, w, *commit, *full); err != nil {
			return err
		}
		if err = w.SetConfig("fetch_head", *commit); err != nil {
			return err
		}
		if !*quiet {
			fmt.Fprintf(a.out, "From %s\n * commit %s -> FETCH_HEAD\n", w.Config("remote"), *commit)
		}
		return nil
	}
	previous, err := remoteHead(w, branch)
	if err != nil {
		return err
	}
	if command == "push" {
		head, err := w.Head()
		if err != nil {
			return err
		}
		if err = syncer.Push(c, w); err != nil {
			return err
		}
		if *upstream {
			if err = w.SetConfig("upstream:"+branch, "origin/"+branch); err != nil {
				return err
			}
		}
		if !*quiet {
			if previous == head {
				fmt.Fprintln(a.out, "Everything up-to-date")
			} else {
				fmt.Fprintln(a.out, "To", w.Config("remote"))
				if previous == "" {
					fmt.Fprintf(a.out, " * [new branch]      %s -> %s\n", branch, branch)
				} else {
					fmt.Fprintf(a.out, "   %s..%s  %s -> %s\n", short(previous), short(head), branch, branch)
				}
			}
			if *upstream {
				fmt.Fprintf(a.out, "branch '%s' set up to track 'origin/%s'.\n", branch, branch)
			}
		}
		return nil
	}
	head, err := syncer.Fetch(c, w, branch, *full)
	if err != nil {
		return err
	}
	if err = w.SetConfig("fetch_head", head); err != nil {
		return err
	}
	if !*quiet && previous != head {
		fmt.Fprintln(a.out, "From", w.Config("remote"))
		if previous == "" {
			fmt.Fprintf(a.out, " * [new branch]      %s -> origin/%s\n", branch, branch)
		} else {
			fmt.Fprintf(a.out, "   %s..%s  %s -> origin/%s\n", short(previous), short(head), branch, branch)
		}
	}
	if command == "pull" {
		old, err := w.Head()
		if err != nil {
			return err
		}
		if old == head {
			if !*quiet {
				fmt.Fprintln(a.out, "Already up to date.")
			}
			return nil
		}
		ok, err := w.Store.IsAncestor(old, head)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("branches diverged; run crease merge origin/" + branch)
		}
		if err = w.Checkout(branch, head, true, false, w.Sparse()); err != nil {
			return err
		}
		if !*quiet {
			fmt.Fprintf(a.out, "Updating %s..%s\nFast-forward\n", short(old), short(head))
		}
	}
	return nil
}
