package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"crease/internal/backup"
	"crease/internal/diff"
	"crease/internal/model"
	"crease/internal/objectstore"
	"crease/internal/protocol"
	"crease/internal/server"
	"crease/internal/syncer"
	"crease/internal/transport"
	"crease/internal/workspace"
	"golang.org/x/crypto/ssh"
)

const help = `Crease: private, self-hosted version control

Usage: crease [-C DIRECTORY] [--key FILE] [--known-hosts FILE] [--trust-host] COMMAND

Local commands:
  init [-b BRANCH] [DIRECTORY]      Create a local repository
  add [-A] [--] PATH...            Track files; obey .creaseignore
  untrack PATH                     Stop tracking a path, keeping its files
  rm --cached PATH...              Stop tracking files, keeping working copies
  status [-sb] [--porcelain] [-z]   Show changes and untracked files
  commit [-a] -m MESSAGE           Commit every tracked change
  log [--oneline] [-n N] [REF]      Show first-parent history
  show [REF]                       Show a commit and its patch
  diff [--stat] [REF [REF]]         Compare revisions or the workspace
  branch [-v] [NAME [REF]]         List branches or create one
  branch -m [OLD] NEW | -d|-D NAME Rename or delete a branch
  switch [-c NAME] [REF]           Switch branches or create and switch
  checkout [-b NAME] [REF]         Alias for branch switching
  rev-parse [--short] REF          Resolve a revision or show workspace paths
  merge REF                        Merge a revision, or begin conflict resolution
  merge --continue|--abort         Commit a resolution, or restore pre-merge files
  restore [REF] [--force]           Restore tracked files; --force discards edits
  recover                          Finish an interrupted workspace update
  sparse [set PATH...|clear]        Select materialized paths, or restore all
  config user.name|user.email NAME Set repository-local authorship
  fsck                             Verify cached objects and report deferred blobs
  monitor start|stop|status|run    Manage the optional local filesystem monitor
  key init|use|show|share|copy      Create, select, or share your SSH public key

Remote commands:
  serve --data DIR --listen :2222 [--owner-key FILE]
  create ssh://HOST:2222/REPO [DIRECTORY]
                                   Use DIRECTORY . for an existing local repository
  clone ssh://HOST:2222/REPO [DIRECTORY] [--branch main] [--paths DIR,DIR]
  remote [-v]                      List the origin remote
  remote add origin URL            Configure an existing server repository
  remote set-url origin URL        Change origin's address
  fetch [origin [BRANCH]] [--full]  Fetch metadata and current files
  fetch --commit ID                Fetch a merge candidate or historical commit
  pull [--ff-only] [origin BRANCH]  Fetch and fast-forward the current branch
  push [-u] [origin BRANCH]         Publish the current branch
  access grant [KEY|-] [--clipboard] [--role read|write|admin]
  access list | revoke [KEY|-] [--clipboard]
  pr create --base main --title TITLE [--body TEXT]
  pr list [--after ID] [--limit N]
  pr [show|diff|approve|merge] ID    Merge accepts --wait
  pr comment ID --body TEXT
  job ID                           Show a merge job
  policy checks [set NAME...|clear] Set required CI checks as an administrator
  check JOB [--name NAME --head ID --status STATUS]
  backup --data DIR --output DIR   Back up a stopped server
  restore-server --input DIR --data DIR
  version

Use CREASE_KEY, CREASE_KNOWN_HOSTS, and CREASE_AUTHOR for defaults.
Crease commits every tracked change. add tracks files; there is no staging area.
Use crease help COMMAND for command usage. Legacy commands remain supported.
All repositories are private. SSH encrypts traffic; stored files are readable
by the server operator. File contents are limited to 8 MiB in this release.
`

type app struct {
	in       io.Reader
	out, err io.Writer
	opts     transport.Options
	cwd      string
}

func Run(args []string, in io.Reader, out, errOut io.Writer, version string) error {
	a := &app{in: in, out: out, err: errOut}
	a.cwd, _ = os.Getwd()
	if len(args) == 1 && args[0] == "--version" {
		args = []string{"version"}
	}
	if len(args) == 2 && args[0] == "help" {
		return a.printHelp(args[1])
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		return a.printHelp(args[0])
	}
	global := flag.NewFlagSet("crease", flag.ContinueOnError)
	global.SetOutput(errOut)
	global.Usage = func() {}
	directory := global.String("C", "", "run as if started in this directory")
	versionFlag := global.Bool("version", false, "show the version")
	global.StringVar(&a.opts.KeyFile, "key", "", "private SSH key")
	global.StringVar(&a.opts.KnownHosts, "known-hosts", "", "known hosts file")
	global.BoolVar(&a.opts.TrustHost, "trust-host", false, "accept a new host key after verifying its fingerprint separately")
	a.opts.In = in
	a.opts.Err = errOut
	if e := global.Parse(args); e != nil {
		if e == flag.ErrHelp {
			fmt.Fprint(out, help)
			return nil
		}
		return e
	}
	if *directory != "" {
		previous := a.cwd
		if e := os.Chdir(*directory); e != nil {
			return e
		}
		defer os.Chdir(previous)
		a.cwd, _ = os.Getwd()
	}
	args = global.Args()
	if *versionFlag {
		args = []string{"version"}
	}
	if len(args) == 0 {
		return a.printHelp("")
	}
	if args[0] == "help" {
		if len(args) > 2 {
			return errors.New("usage: crease help [COMMAND]")
		}
		if len(args) == 2 {
			return a.printHelp(args[1])
		}
		return a.printHelp("")
	}
	if args[0] == "version" {
		if len(args) != 1 {
			return errors.New("usage: crease version")
		}
		fmt.Fprintf(out, "crease %s (format %d, protocol %d)\n", version, model.Version, protocol.Version)
		return nil
	}
	command, args := args[0], args[1:]
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return a.printHelp(command)
	}
	if _, known := usages[command]; !known && !strings.Contains(" serve access pr job policy check backup restore-server fsck sparse monitor recover untrack ", " "+command+" ") {
		return fmt.Errorf("'%s' is not a crease command. See 'crease --help'", command)
	}
	switch command {
	case "key":
		return a.key(args)
	case "monitor":
		return a.monitor(args)
	case "serve":
		return a.serve(args)
	case "backup", "restore-server":
		return a.backup(command, args)
	case "init":
		return a.init(args)
	case "create", "clone":
		return a.clone(command, args)
	}
	w, e := workspace.Find(a.cwd)
	if e != nil {
		return e
	}
	defer w.Close()
	if command != "recover" {
		if e = w.Ready(); e != nil {
			return e
		}
	}
	if pending, err := w.MergeState(); err != nil {
		return err
	} else if pending != nil {
		switch command {
		case "switch", "checkout", "branch", "pull", "sparse":
			return errors.New("finish the pending merge with commit or merge --continue, or use merge --abort")
		}
	}
	switch command {
	case "add":
		return a.add(w, args)
	case "untrack":
		return a.untrack(w, args)
	case "rm":
		return a.rm(w, args)
	case "status":
		return a.status(w, args)
	case "commit":
		return a.commit(w, args)
	case "config":
		return a.config(w, args)
	case "remote":
		return a.remote(w, args)
	case "log":
		return a.log(w, args)
	case "diff":
		return a.diff(w, args)
	case "show":
		return a.show(w, args)
	case "rev-parse":
		return a.revParse(w, args)
	case "branch":
		return a.branch(w, args)
	case "switch", "checkout":
		return a.switchBranch(w, command, args)
	case "restore":
		f := a.flags("restore")
		force := f.Bool("force", false, "discard tracked changes")
		if e := parse(f, args); e != nil {
			return e
		}
		if f.NArg() > 1 {
			return errors.New("usage: crease restore [ref] [--force]")
		}
		ref := "HEAD"
		if f.NArg() == 1 {
			ref = f.Arg(0)
		}
		head, e := resolve(w, ref)
		if e != nil {
			return e
		}
		if e = a.ensure(w, head, w.Sparse()); e != nil {
			return e
		}
		return w.Checkout(w.Branch(), head, false, *force, w.Sparse())
	case "recover":
		if len(args) != 0 {
			return errors.New("usage: crease recover")
		}
		return w.Recover()
	case "sparse":
		if len(args) == 0 {
			fmt.Fprintln(out, strings.Join(w.Sparse(), "\n"))
			return nil
		}
		var paths []string
		if args[0] == "set" && len(args) > 1 {
			for _, p := range args[1:] {
				paths = append(paths, strings.TrimSuffix(filepath.ToSlash(p), "/"))
			}
		} else if args[0] != "clear" || len(args) != 1 {
			return errors.New("usage: crease sparse set path... | crease sparse clear")
		}
		h, e := w.Head()
		if e != nil {
			return e
		}
		if e = a.ensure(w, h, paths); e != nil {
			return e
		}
		return w.SetSparse(paths)
	case "merge":
		if len(args) != 1 {
			return errors.New("usage: crease merge REF")
		}
		if args[0] == "--continue" || args[0] == "--abort" {
			m, e := w.MergeState()
			if e != nil {
				return e
			}
			if m == nil {
				return errors.New("no merge is pending")
			}
			if args[0] == "--abort" {
				if e = w.Checkout(m.Branch, m.Head, true, true, m.Sparse); e != nil {
					return e
				}
				return w.ClearMerge()
			}
			_, e = w.Commit("Merge "+m.Ref, a.author(w), []string{m.Head, m.Other})
			return e
		}
		return a.merge(w, args[0])
	case "fetch", "pull", "push":
		return a.sync(command, w, args)
	case "access":
		return a.access(w, args)
	case "policy", "check":
		return a.checks(command, w, args)
	case "pr":
		return a.pr(w, args)
	case "job":
		if len(args) != 1 {
			return errors.New("usage: crease job ID")
		}
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil {
			return e
		}
		c, e := a.connect(w)
		if e != nil {
			return e
		}
		defer c.Close()
		res, e := c.Call(protocol.Request{Op: "job_get", ID: id})
		if e == nil {
			fmt.Fprintf(out, "job %d: %s %s\nCandidate: %s\n", res.Job.ID, res.Job.State, res.Job.Message, res.Job.Head)
		}
		return e
	case "fsck":
		head, e := w.Head()
		if e != nil {
			return e
		}
		ids, e := syncer.Inventory(w.Store, head)
		if e != nil {
			return e
		}
		deferred := 0
		for _, id := range ids {
			if _, e = w.Store.Verify(id); os.IsNotExist(e) {
				deferred++
			} else if e != nil {
				return e
			}
		}
		fmt.Fprintf(out, "Verified %d cached objects; %d historical blobs deferred\n", len(ids)-deferred, deferred)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run crease help", command)
	}
}

func (a *app) flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(a.err)
	return f
}

// Accept command flags before or after positional arguments, as the documented
// CLI examples do. -- ends option parsing for paths beginning with a dash.
func parse(f *flag.FlagSet, args []string) error {
	var options, pos []string
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			pos = append(pos, token)
			continue
		}
		name := strings.SplitN(strings.TrimLeft(token, "-"), "=", 2)
		if fl := f.Lookup(name[0]); fl != nil {
			options = append(options, "-"+strings.Join(name, "="))
			isBool, ok := fl.Value.(interface{ IsBoolFlag() bool })
			if len(name) == 1 && (!ok || !isBool.IsBoolFlag()) {
				i++
				if i >= len(args) {
					return fmt.Errorf("flag %s requires a value", token)
				}
				options = append(options, args[i])
			}
			continue
		}
		if strings.HasPrefix(token, "--") || strings.Contains(token, "=") {
			return fmt.Errorf("unknown flag %s", token)
		}
		// Git accepts combined boolean flags and attached values: -sb, -am msg, -n3.
		rest := token[1:]
		for len(rest) > 0 {
			name := rest[:1]
			rest = rest[1:]
			fl := f.Lookup(name)
			if fl == nil {
				return fmt.Errorf("unknown flag -%s", name)
			}
			options = append(options, "-"+name)
			isBool, ok := fl.Value.(interface{ IsBoolFlag() bool })
			if !ok || !isBool.IsBoolFlag() {
				if rest != "" {
					options = append(options, rest)
					rest = ""
				} else {
					i++
					if i >= len(args) {
						return fmt.Errorf("flag -%s requires a value", name)
					}
					options = append(options, args[i])
				}
			}
		}
	}
	options = append(options, "--")
	return f.Parse(append(options, pos...))
}
func (a *app) author(w *workspace.Workspace) string {
	if s := os.Getenv("CREASE_AUTHOR"); s != "" {
		return s
	}
	if name := w.Config("user.name"); name != "" {
		if email := w.Config("user.email"); email != "" {
			return name + " <" + email + ">"
		}
		return name
	}
	if s := w.Config("author"); s != "" {
		return s
	}
	if s := os.Getenv("USER"); s != "" {
		return s
	}
	return "unknown"
}
func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
func (a *app) connect(w *workspace.Workspace) (*transport.Client, error) {
	remote := w.Config("remote")
	if remote == "" {
		return nil, errors.New("no remote configured; use crease remote set URL")
	}
	return transport.Dial(remote, a.opts)
}
func (a *app) ensure(w *workspace.Workspace, head string, paths []string) error {
	snapshot, e := w.Store.Snapshot(head)
	if e != nil {
		return e
	}
	need := false
	for p, f := range snapshot {
		if workspace.Selected(p, paths) && !w.Store.Has(f.ID) {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	c, e := a.connect(w)
	if e != nil {
		return e
	}
	defer c.Close()
	return syncer.EnsureSnapshot(c, w.Store, head, paths)
}

func (a *app) serve(args []string) error {
	f := a.flags("serve")
	data := f.String("data", "", "server data directory")
	listen := f.String("listen", ":2222", "SSH listen address")
	owner := f.String("owner-key", "", "initial owner public key")
	if e := parse(f, args); e != nil {
		return e
	}
	if *data == "" || f.NArg() != 0 {
		return errors.New("usage: crease serve --data DIR [--listen :2222] [--owner-key FILE]")
	}
	s, e := server.Open(*data, *owner)
	if e != nil {
		return e
	}
	defer s.Close()
	ln, e := net.Listen("tcp", *listen)
	if e != nil {
		return e
	}
	fmt.Fprintf(a.out, "Crease listening on %s\nHost key: %s\n", ln.Addr(), fingerprint(s))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return s.Serve(ctx, ln)
}
func fingerprint(s *server.Server) string { return ssh.FingerprintSHA256(s.Signer.PublicKey()) }
func (a *app) backup(command string, args []string) error {
	f := a.flags(command)
	data := f.String("data", "", "server data directory")
	output := f.String("output", "", "backup destination")
	input := f.String("input", "", "backup source")
	if e := parse(f, args); e != nil {
		return e
	}
	if f.NArg() != 0 || *data == "" {
		return errors.New("provide --data with no positional arguments")
	}
	if command == "backup" {
		if *output == "" {
			return errors.New("provide --output")
		}
		return backup.Create(*data, *output)
	}
	if *input == "" {
		return errors.New("provide --input")
	}
	return backup.Restore(*input, *data)
}

func (a *app) clone(command string, args []string) error {
	cloneStarted := time.Now()
	f := a.flags(command)
	branch := f.String("branch", "main", "branch to check out")
	f.StringVar(branch, "b", "main", "branch to check out")
	quiet := f.Bool("q", false, "suppress clone messages")
	f.BoolVar(quiet, "quiet", false, "suppress clone messages")
	paths := f.String("paths", "", "comma-separated selected paths")
	full := f.Bool("full", false, "download historical file contents")
	showTimings := f.Bool("timings", false, "write clone phase timings as JSON to stderr")
	if e := parse(f, args); e != nil {
		return e
	}
	var timings syncer.CloneTimings
	defer func() {
		if *showTimings {
			timings.TotalMS = syncer.Milliseconds(cloneStarted)
			b, _ := json.Marshal(timings)
			fmt.Fprintf(a.err, "clone timings: %s\n", b)
		}
	}()
	if f.NArg() < 1 || f.NArg() > 2 {
		return fmt.Errorf("usage: crease %s URL [directory]", command)
	}
	remote := f.Arg(0)
	r, e := transport.Parse(remote)
	if e != nil {
		return e
	}
	dir := filepath.Base(r.Repo)
	if f.NArg() == 2 {
		dir = f.Arg(1)
	}
	if st, e := os.Stat(dir); e == nil {
		if !st.IsDir() {
			return fmt.Errorf("destination %q is not a directory; for an existing local repository, run crease create %s .", dir, remote)
		}
		items, e := os.ReadDir(dir)
		if e != nil {
			return e
		}
		if len(items) != 0 {
			if command == "create" && f.NArg() == 2 {
				if *branch != "main" || *paths != "" || *full {
					return errors.New("--branch, --paths, and --full apply to new workspaces only")
				}
				return a.createRemote(remote, dir, *quiet)
			}
			return errors.New("destination must be empty")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if command == "clone" && !*quiet {
		fmt.Fprintf(a.out, "Cloning into '%s'...\n", quotePath(dir))
	}
	authStarted := time.Now()
	c, e := transport.Dial(remote, a.opts)
	authMS := syncer.Milliseconds(authStarted)
	timings.AuthenticationMS = authMS
	if e != nil {
		return e
	}
	defer c.Close()
	if command == "create" {
		if _, e = c.Call(protocol.Request{Op: "create"}); e != nil {
			return e
		}
	}
	initStarted := time.Now()
	w, e := workspace.Init(dir)
	if e != nil {
		return e
	}
	defer w.Close()
	if e = w.SetConfig("remote", remote); e != nil {
		return e
	}
	var sparse []string
	if *paths != "" {
		for _, p := range strings.Split(*paths, ",") {
			p = strings.TrimSuffix(p, "/")
			if !model.ValidPath(p) {
				return errors.New("invalid selected path")
			}
			sparse = append(sparse, p)
		}
		b, _ := json.Marshal(sparse)
		if e = w.SetConfig("sparse", string(b)); e != nil {
			return e
		}
	}
	initMS := syncer.Milliseconds(initStarted)
	timings, e = syncer.Clone(c, w, *branch, *full)
	timings.AuthenticationMS, timings.InitializeMS = authMS, initMS
	if e != nil {
		return fmt.Errorf("clone incomplete; local metadata is preserved; use crease recover if an update is pending: %w", e)
	}
	if e = w.SetConfig("upstream:"+*branch, "origin/"+*branch); e != nil {
		return e
	}
	if !*quiet {
		if command == "clone" {
			fmt.Fprintln(a.out, "done.")
		} else {
			fmt.Fprintf(a.out, "Initialized empty Crease repository in %s/\n", filepath.Join(w.Root, ".crease"))
		}
	}
	return nil
}

// createRemote attaches a server repository to an explicitly selected local
// workspace. It never checks out files or replaces local commits.
func (a *app) createRemote(remote, dir string, quiet bool) error {
	if st, e := os.Lstat(filepath.Join(dir, ".crease", "state.db")); e != nil || !st.Mode().IsRegular() {
		return errors.New("destination must be empty or an initialized local repository; run crease init in the project directory first")
	}
	w, e := workspace.Find(dir)
	if e != nil {
		return e
	}
	defer w.Close()
	if e = w.Ready(); e != nil {
		return e
	}
	if configured := w.Config("remote"); configured != "" && configured != remote {
		return errors.New("a different remote is already configured; use crease remote set URL to change it explicitly")
	}
	c, e := transport.Dial(remote, a.opts)
	if e != nil {
		return e
	}
	defer c.Close()
	if _, e = c.Call(protocol.Request{Op: "create"}); e != nil {
		return e
	}
	if e = w.SetConfig("remote", remote); e != nil {
		return e
	}
	if !quiet {
		fmt.Fprintf(a.out, "Remote %s connected to %s; run crease push to publish commits\n", remote, w.Root)
	}
	return nil
}

func (a *app) log(w *workspace.Workspace, args []string) error {
	f := a.flags("log")
	n := f.Int("limit", 20, "maximum commits")
	f.IntVar(n, "n", 20, "maximum commits")
	f.IntVar(n, "max-count", 20, "maximum commits")
	oneline := f.Bool("oneline", false, "show short commit IDs and subjects")
	decorate := f.Bool("decorate", false, "show HEAD and the current branch")
	noDecorate := f.Bool("no-decorate", false, "omit decorations")
	if e := parse(f, args); e != nil {
		return e
	}
	if f.NArg() > 1 || *n < 1 || *n > 10000 {
		return errors.New("usage: crease log [--oneline] [-n COUNT] [REF]")
	}
	ref := "HEAD"
	if f.NArg() == 1 {
		ref = f.Arg(0)
	}
	head, e := resolve(w, ref)
	if e != nil {
		return e
	}
	current, e := w.Head()
	if e != nil {
		return e
	}
	for i := 0; i < *n && head != ""; i++ {
		c, e := w.Store.Commit(head)
		if e != nil {
			return e
		}
		decoration := ""
		if *decorate && !*noDecorate && head == current {
			decoration = " (HEAD -> " + w.Branch() + ")"
		}
		if *oneline {
			fmt.Fprintf(a.out, "%s%s %s\n", short(head), decoration, strings.SplitN(c.Message, "\n", 2)[0])
		} else {
			text := formatCommit(c, head)
			if decoration != "" {
				text = strings.Replace(text, "commit "+head+"\n", "commit "+head+decoration+"\n", 1)
			}
			fmt.Fprint(a.out, text)
		}
		head = ""
		if len(c.Parents) > 0 {
			head = c.Parents[0]
		}
	}
	return nil
}
func (a *app) diff(w *workspace.Workspace, args []string) error {
	f := a.flags("diff")
	names := f.Bool("name-only", false, "show changed file names")
	nameStatus := f.Bool("name-status", false, "show change codes and file names")
	stat := f.Bool("stat", false, "show a diffstat")
	exitCode := f.Bool("exit-code", false, "exit with 1 if there are changes")
	if e := parse(f, args); e != nil {
		return e
	}
	args = f.Args()
	if len(args) > 2 {
		return errors.New("usage: crease diff [--stat|--name-only|--name-status] [REF [REF]]")
	}
	ref := "HEAD"
	if len(args) > 0 {
		ref = args[0]
	}
	ah, e := resolve(w, ref)
	if e != nil {
		return e
	}
	var left, right objectstore.Snapshot
	working := len(args) < 2
	if !working {
		bh, err := resolve(w, args[1])
		if err != nil {
			return err
		}
		left, right, e = w.Store.ChangedSnapshots(ah, bh)
	} else {
		left, e = w.CommittedSnapshot(ah)
		if e != nil {
			return e
		}
		right, e = w.InspectSnapshot()
		if e != nil {
			return e
		}
		changes := objectstore.FileChanges(left, right)
		l, r := objectstore.Snapshot{}, objectstore.Snapshot{}
		for p := range changes {
			if f, ok := left[p]; ok {
				l[p] = f
			}
			if f, ok := right[p]; ok {
				r[p] = f
			}
		}
		left, right = l, r
	}
	if e != nil {
		return e
	}
	if *names || *nameStatus {
		changes := objectstore.FileChanges(left, right)
		for _, p := range sortedPaths(changes) {
			if *nameStatus {
				code := "M"
				if _, ok := left[p]; !ok {
					code = "A"
				} else if _, ok := right[p]; !ok {
					code = "D"
				}
				fmt.Fprintf(a.out, "%s\t", code)
			}
			fmt.Fprintln(a.out, quotePath(p))
		}
	} else if e = a.printDiff(w, left, right, working, *stat); e != nil {
		return e
	}
	if *exitCode && len(objectstore.FileChanges(left, right)) > 0 {
		return &ExitError{Message: "differences found"}
	}
	return nil
}

func (a *app) printDiff(w *workspace.Workspace, left, right objectstore.Snapshot, working, stat bool) error {
	var needed []string
	for p, file := range left {
		if other, ok := right[p]; !ok || other.ID != file.ID {
			needed = append(needed, file.ID)
		}
	}
	if !working {
		for p, file := range right {
			if other, ok := left[p]; !ok || other.ID != file.ID {
				needed = append(needed, file.ID)
			}
		}
	}
	var missing []string
	for _, id := range needed {
		if !w.Store.Exists(id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		c, err := a.connect(w)
		if err != nil {
			return err
		}
		defer c.Close()
		if err = syncer.Download(c, w.Store, missing); err != nil {
			return err
		}
	}
	read := func(p string, file objectstore.File, isRight bool) ([]byte, error) {
		if working && isRight {
			return w.WorkingBytes(p, file)
		}
		o, err := w.Store.Get(file.ID)
		return o.Data, err
	}
	if stat {
		stats, err := diff.StatsWith(left, right, read)
		if err != nil {
			return err
		}
		a.diffStat(stats)
		return nil
	}
	text, err := diff.UnifiedWith(left, right, read)
	if err != nil {
		return err
	}
	fmt.Fprint(a.out, text)
	return nil
}

func (a *app) show(w *workspace.Workspace, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: crease show [REF]")
	}
	ref := "HEAD"
	if len(args) == 1 {
		ref = args[0]
	}
	head, err := resolve(w, ref)
	if err != nil {
		return err
	}
	if head == "" {
		return errors.New("HEAD has no commits yet")
	}
	c, err := w.Store.Commit(head)
	if err != nil {
		return err
	}
	parent := ""
	if len(c.Parents) > 0 {
		parent = c.Parents[0]
	}
	left, right, err := w.Store.ChangedSnapshots(parent, head)
	if err != nil {
		return err
	}
	fmt.Fprint(a.out, formatCommit(c, head))
	return a.printDiff(w, left, right, false, false)
}

func (a *app) merge(w *workspace.Workspace, ref string) error {
	if pending, e := w.MergeState(); e != nil {
		return e
	} else if pending != nil {
		return errors.New("a merge is pending; use merge --continue or merge --abort")
	}
	if e := w.Clean(); e != nil {
		return e
	}
	head, e := w.Head()
	if e != nil {
		return e
	}
	other, e := resolve(w, ref)
	if e != nil {
		return e
	}
	if other == "" {
		return errors.New("cannot merge an empty branch")
	}
	if ok, e := w.Store.IsAncestor(other, head); e != nil {
		return e
	} else if ok {
		fmt.Fprintln(a.out, "Already up to date.")
		return nil
	}
	if e = a.ensure(w, other, nil); e != nil {
		return e
	}
	if ok, e := w.Store.IsAncestor(head, other); e != nil {
		return e
	} else if ok {
		return w.Checkout(w.Branch(), other, true, false, w.Sparse())
	}
	base, e := w.Store.MergeBase(head, other)
	if e != nil {
		return e
	}
	if e = a.ensure(w, base, nil); e != nil {
		return e
	}
	if e = a.ensure(w, head, nil); e != nil {
		return e
	}
	bs, e := w.Store.Snapshot(base)
	if e != nil {
		return e
	}
	left, e := w.Store.Snapshot(head)
	if e != nil {
		return e
	}
	right, e := w.Store.Snapshot(other)
	if e != nil {
		return e
	}
	merged, e := diff.Snapshots(w.Store, bs, left, right)
	if e != nil {
		var conflicts *diff.ConflictError
		if !errors.As(e, &conflicts) {
			return e
		}
		for _, p := range conflicts.Paths {
			if f, ok := left[p]; ok {
				merged[p] = f
			} else {
				delete(merged, p)
			}
		}
		tree, err := w.Store.WriteTree(merged)
		if err != nil {
			return err
		}
		temporary, err := w.Store.PutJSON("commit", model.Commit{Tree: tree, Parents: []string{head, other}, Author: a.author(w), Time: time.Now().UTC().Format(time.RFC3339Nano), Message: "Pending merge of " + ref})
		if err != nil {
			return err
		}
		originalSparse := w.Sparse()
		selected := append([]string{}, originalSparse...)
		if len(originalSparse) > 0 {
			selected = append(selected, conflicts.Paths...)
		}
		if err = w.BeginMerge(workspace.MergeState{Branch: w.Branch(), Head: head, Other: other, Ref: ref, Sparse: originalSparse}); err != nil {
			return err
		}
		if err = w.Checkout(w.Branch(), temporary, false, false, selected); err != nil {
			return err
		}
		return fmt.Errorf("%w; nonconflicting changes applied, conflicting paths retain this branch's contents. Resolve them, then run crease merge --continue or crease merge --abort", e)
	}
	tree, e := w.Store.WriteTree(merged)
	if e != nil {
		return e
	}
	id, e := w.Store.PutJSON("commit", model.Commit{Tree: tree, Parents: []string{head, other}, Author: a.author(w), Time: time.Now().UTC().Format(time.RFC3339Nano), Message: "Merge " + ref})
	if e != nil {
		return e
	}
	return w.Checkout(w.Branch(), id, true, false, w.Sparse())
}

func (a *app) access(w *workspace.Workspace, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: crease access list|grant|revoke")
	}
	op := args[0]
	for _, arg := range args[1:] {
		if strings.Contains(arg, "PRIVATE KEY") {
			return errors.New("expected an SSH public key; private key contents must stay on their owner's computer")
		}
	}
	f := a.flags("access")
	role := f.String("role", "read", "read, write, or admin")
	fromClipboard := f.Bool("clipboard", false, "read the public key from the clipboard")
	if e := parse(f, args[1:]); e != nil {
		return e
	}
	q := protocol.Request{Op: "access_" + op, Role: *role}
	var fingerprint string
	if op == "list" {
		if f.NArg() != 0 || *fromClipboard {
			return errors.New("access list takes no arguments")
		}
	} else if op == "grant" || op == "revoke" {
		if op == "grant" && *role != "read" && *role != "write" && *role != "admin" {
			return errors.New("role must be read, write, or admin")
		}
		pub, e := a.accessKey(f.Args(), *fromClipboard)
		if e != nil {
			return e
		}
		q.PublicKey = string(ssh.MarshalAuthorizedKey(pub))
		fingerprint = ssh.FingerprintSHA256(pub)
	} else {
		return errors.New("unknown access command")
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
	if op == "grant" {
		fmt.Fprintf(a.out, "Granted %s access to %s.\n", *role, fingerprint)
	}
	if op == "revoke" {
		fmt.Fprintf(a.out, "Revoked access for %s.\n", fingerprint)
	}
	for _, g := range res.Grants {
		fmt.Fprintf(a.out, "%s %s\n", g.Role, g.Fingerprint)
	}
	return nil
}

func requestToken(w *workspace.Workspace, q protocol.Request) (string, error) {
	b, _ := json.Marshal(q)
	key := "pending:" + q.Op
	var old struct {
		Request string
		Token   string
	}
	_ = json.Unmarshal([]byte(w.Config(key)), &old)
	if old.Request == string(b) && old.Token != "" {
		return old.Token, nil
	}
	var random [16]byte
	if _, e := rand.Read(random[:]); e != nil {
		return "", e
	}
	old.Request = string(b)
	old.Token = hex.EncodeToString(random[:])
	raw, _ := json.Marshal(old)
	return old.Token, w.SetConfig(key, string(raw))
}
func (a *app) pr(w *workspace.Workspace, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: crease pr create|list|show|diff|approve|comment|merge")
	}
	op := args[0]
	f := a.flags("pr")
	base := f.String("base", "main", "target branch")
	title := f.String("title", "", "pull request title")
	body := f.String("body", "", "description or comment")
	after := f.Int64("after", 0, "pagination cursor")
	lim := f.Int("limit", 50, "page size, up to 100")
	wait := f.Bool("wait", false, "wait for the merge job")
	if e := parse(f, args[1:]); e != nil {
		return e
	}
	q := protocol.Request{Op: "pr_" + op, Title: *title, Body: *body, Cursor: *after, Limit: *lim}
	if op == "show" {
		q.Op = "pr_get"
	}
	switch op {
	case "create":
		if f.NArg() != 0 {
			return errors.New("pr create takes flags only")
		}
		q.Source = w.Branch()
		q.Target = *base
	case "list":
		if f.NArg() != 0 {
			return errors.New("pr list takes flags only")
		}
	case "show", "diff", "approve", "comment", "merge":
		if f.NArg() != 1 {
			return errors.New("provide one pull request ID")
		}
		id, e := strconv.ParseInt(f.Arg(0), 10, 64)
		if e != nil || id < 1 {
			return errors.New("invalid pull request ID")
		}
		q.ID = id
	default:
		return errors.New("unknown pull request command")
	}
	c, e := a.connect(w)
	if e != nil {
		return e
	}
	defer c.Close()
	if op == "approve" || op == "comment" || op == "merge" {
		res, e := c.Call(protocol.Request{Op: "pr_get", ID: q.ID})
		if e != nil {
			return e
		}
		q.Head = res.PR.Head
	}
	if op == "create" || op == "merge" {
		q.Token, e = requestToken(w, q)
		if e != nil {
			return e
		}
	}
	res, e := c.Call(q)
	if e != nil {
		return e
	}
	if q.Token != "" {
		_ = w.SetConfig("pending:"+q.Op, "")
	}
	if res.PR != nil && op != "diff" {
		p := res.PR
		fmt.Fprintf(a.out, "#%d %s [%s] %s -> %s (%s)\n", p.ID, p.Title, p.State, p.Source, p.Target, short(p.Head))
		if op == "show" {
			fmt.Fprintln(a.out, p.Body)
			for _, comment := range res.Comments {
				fmt.Fprintf(a.out, "comment %d by %s at %s\n%s\n", comment.ID, comment.Author, short(comment.Head), comment.Body)
			}
		}
	}
	for _, p := range res.PRs {
		fmt.Fprintf(a.out, "#%d [%s] %s\n", p.ID, p.State, p.Title)
	}
	if res.Text != "" {
		fmt.Fprint(a.out, res.Text)
	}
	if res.Job != nil {
		job := res.Job
		fmt.Fprintf(a.out, "job %d: %s\n", job.ID, job.State)
		if *wait {
			deadline := time.Now().Add(2 * time.Minute)
			for time.Now().Before(deadline) {
				if job.State == "merged" {
					fmt.Fprintln(a.out, "Merged", short(job.Head))
					return nil
				}
				if job.State == "failed" {
					return errors.New(job.Message)
				}
				time.Sleep(100 * time.Millisecond)
				r, e := c.Call(protocol.Request{Op: "job_get", ID: job.ID})
				if e != nil {
					return e
				}
				job = r.Job
			}
			return errors.New("merge is still queued; use crease job to check progress")
		}
	}
	return nil
}
