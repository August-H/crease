package cli

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"crease/internal/model"
	"crease/internal/workspace"
)

func resolve(w *workspace.Workspace, ref string) (string, error) {
	if len(ref) == 0 || len(ref) > 1024 {
		return "", errors.New("invalid revision")
	}
	if i := strings.LastIndexAny(ref, "~^"); i >= 0 {
		count := 1
		if i+1 < len(ref) {
			n, err := strconv.Atoi(ref[i+1:])
			if err != nil || n < 0 || n > 10000 {
				return "", errors.New("invalid revision ancestry")
			}
			count = n
		}
		head, err := resolve(w, ref[:i])
		if err != nil {
			return "", err
		}
		for n := 0; n < count; n++ {
			c, err := w.Store.Commit(head)
			if err != nil {
				return "", err
			}
			parent := 0
			if ref[i] == '^' {
				parent = count - 1
			}
			if parent >= len(c.Parents) {
				return "", fmt.Errorf("revision '%s' has no such parent", ref)
			}
			head = c.Parents[parent]
			if ref[i] == '^' {
				break
			}
		}
		return head, nil
	}
	if ref == "HEAD" {
		return w.Head()
	}
	if ref == "FETCH_HEAD" {
		head := w.Config("fetch_head")
		if head == "" {
			return "", errors.New("FETCH_HEAD has no fetched commit")
		}
		return head, nil
	}
	if model.ValidID(ref) {
		_, err := w.Store.Commit(ref)
		return ref, err
	}
	table, name := "branches", ref
	if strings.HasPrefix(ref, "origin/") {
		table, name = "remote_refs", strings.TrimPrefix(ref, "origin/")
	} else if strings.HasPrefix(ref, "remote/") {
		table, name = "remote_refs", strings.TrimPrefix(ref, "remote/")
	}
	var head string
	err := w.DB.QueryRow(`SELECT head FROM `+table+` WHERE name=?`, name).Scan(&head)
	if err == nil {
		return head, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if len(ref) >= 4 && len(ref) < 64 {
		// Short IDs shown by log refer to reachable cached commit metadata.
		_, hexErr := hex.DecodeString(ref + strings.Repeat("0", len(ref)%2))
		if hexErr == nil && strings.ToLower(ref) == ref {
			rows, err := w.DB.Query(`SELECT head FROM branches UNION SELECT head FROM remote_refs`)
			if err != nil {
				return "", err
			}
			var heads []string
			for rows.Next() {
				var h string
				if err = rows.Scan(&h); err != nil {
					rows.Close()
					return "", err
				}
				if h != "" {
					heads = append(heads, h)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return "", err
			}
			seen := map[string]bool{}
			found := ""
			for len(heads) > 0 {
				h := heads[len(heads)-1]
				heads = heads[:len(heads)-1]
				if seen[h] {
					continue
				}
				seen[h] = true
				if len(seen) > 2_000_000 {
					return "", errors.New("revision history exceeds object limit")
				}
				if strings.HasPrefix(h, ref) {
					if found != "" && found != h {
						return "", fmt.Errorf("short object ID %s is ambiguous", ref)
					}
					found = h
				}
				c, err := w.Store.Commit(h)
				if err != nil {
					return "", err
				}
				heads = append(heads, c.Parents...)
			}
			if found != "" {
				return found, nil
			}
		}
	}
	return "", fmt.Errorf("unknown revision '%s'", ref)
}

func (a *app) revParse(w *workspace.Workspace, args []string) error {
	f := a.flags("rev-parse")
	abbreviate := f.Bool("short", false, "abbreviate commit IDs")
	root := f.Bool("show-toplevel", false, "show the workspace root")
	inside := f.Bool("is-inside-work-tree", false, "report whether this is a workspace")
	if err := parse(f, args); err != nil {
		return err
	}
	if (*root || *inside) && (f.NArg() != 0 || *abbreviate || *root && *inside) {
		return errors.New("choose one rev-parse query")
	}
	if *root {
		fmt.Fprintln(a.out, w.Root)
		return nil
	}
	if *inside {
		fmt.Fprintln(a.out, "true")
		return nil
	}
	if f.NArg() != 1 {
		return errors.New("usage: crease rev-parse [--short] REF")
	}
	head, err := resolve(w, f.Arg(0))
	if err != nil {
		return err
	}
	if head == "" {
		return errors.New("HEAD has no commits yet")
	}
	if *abbreviate {
		head = short(head)
	}
	fmt.Fprintln(a.out, head)
	return nil
}
