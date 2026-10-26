package cli

import (
	"errors"
	"fmt"
	"strconv"

	"crease/internal/protocol"
	"crease/internal/workspace"
)

func (a *app) checks(command string, w *workspace.Workspace, args []string) error {
	q := protocol.Request{}
	if command == "policy" {
		if len(args) == 0 || args[0] != "checks" {
			return errors.New("usage: crease policy checks [set name...|clear]")
		}
		q.Op = "policy_get"
		if len(args) > 1 {
			q.Op = "policy_set"
			if args[1] == "set" && len(args) > 2 {
				q.Names = args[2:]
			} else if args[1] != "clear" || len(args) != 2 {
				return errors.New("usage: crease policy checks [set name...|clear]")
			}
		}
	} else {
		f := a.flags("check")
		name := f.String("name", "", "check name")
		head := f.String("head", "", "exact merge candidate commit ID")
		status := f.String("status", "", "pending, success, or failure")
		if e := parse(f, args); e != nil {
			return e
		}
		if f.NArg() != 1 {
			return errors.New("usage: crease check JOB [--name name --head candidate --status success|failure|pending]")
		}
		id, e := strconv.ParseInt(f.Arg(0), 10, 64)
		if e != nil || id < 1 {
			return errors.New("invalid job ID")
		}
		q.ID = id
		q.Op = "check_list"
		if *name != "" || *head != "" || *status != "" {
			q.Op = "check_report"
			q.Name = *name
			q.Candidate = *head
			q.Status = *status
		}
	}
	c, e := a.connect(w)
	if e != nil {
		return e
	}
	defer c.Close()
	res, e := c.Call(q)
	if e != nil {
		return e
	}
	for _, name := range res.Names {
		fmt.Fprintln(a.out, name)
	}
	for _, check := range res.Checks {
		fmt.Fprintf(a.out, "%s %s %s\n", check.Name, check.Status, check.Candidate)
	}
	return nil
}
