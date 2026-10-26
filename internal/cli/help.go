package cli

import (
	"fmt"
	"strings"
)

var usages = map[string]string{
	"key":       "key [show|share|copy|fingerprint]\n       crease key init [--file PRIVATE_KEY_FILE] [--comment TEXT]\n       crease key use PRIVATE_KEY_FILE\n\nshare prints and copies only the public key. The private key stays on this computer.",
	"access":    "access grant [PUBLIC_KEY|FILE|-] [--clipboard] [--role read|write|admin]\n       crease access revoke [PUBLIC_KEY|FILE|-] [--clipboard]\n       crease access list\n\nOmit the key to paste a public key interactively. The default role is read.",
	"init":      "init [-b BRANCH] [-q] [DIRECTORY]",
	"add":       "add [-A|--all] [--] PATH...",
	"status":    "status [-sb] [--porcelain[=v1]] [-z]",
	"commit":    "commit [-a|--all] [-q] -m MESSAGE",
	"log":       "log [--oneline] [--no-decorate] [-n COUNT] [REF]",
	"diff":      "diff [REF [REF]]",
	"show":      "show [REF]",
	"branch":    "branch [-v] | NAME [START-POINT] | -m [OLD] NEW | -d|-D NAME",
	"switch":    "switch BRANCH | -c NAME [START-POINT]",
	"checkout":  "checkout BRANCH | -b NAME [START-POINT]",
	"remote":    "remote [-v] | add origin URL | set-url origin URL | get-url origin | remove origin",
	"config":    "config [--get] user.name|user.email|author [VALUE]",
	"push":      "push [-u|--set-upstream] [-q] [origin [BRANCH]]",
	"pull":      "pull [--ff-only] [-q] [origin [BRANCH]]",
	"fetch":     "fetch [--full] [-q] [origin [BRANCH]] | --commit ID [--full]",
	"clone":     "clone URL [DIRECTORY] [--branch BRANCH] [--paths DIR,DIR] [--full]",
	"create":    "create URL [DIRECTORY]\n\nUse . as DIRECTORY to connect an existing local Crease repository.",
	"rm":        "rm --cached [--] PATH...",
	"restore":   "restore [REF] [--force]",
	"merge":     "merge REF | --continue | --abort",
	"rev-parse": "rev-parse [--short] HEAD|REF | --show-toplevel | --is-inside-work-tree",
}

func (a *app) printHelp(command string) error {
	if command == "" {
		fmt.Fprint(a.out, help)
		return nil
	}
	if usage, ok := usages[command]; ok {
		fmt.Fprintf(a.out, "usage: crease %s\n", usage)
		if command == "add" || command == "commit" || command == "status" {
			fmt.Fprintln(a.out, "\nCrease commits all tracked changes. There is no separate staging area.")
		}
		return nil
	}
	if strings.Contains(" serve access pr job policy check backup restore-server fsck sparse monitor recover untrack version ", " "+command+" ") {
		fmt.Fprint(a.out, help)
		return nil
	}
	return fmt.Errorf("'%s' is not a crease command. See 'crease --help'", command)
}
