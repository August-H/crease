package cli_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"crease/internal/cli"
)

func TestGitStyleLocalWorkflow(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	run := func(args ...string) (string, error) {
		var out, stderr bytes.Buffer
		err := cli.Run(args, strings.NewReader(""), &out, &stderr, "test")
		return out.String(), err
	}
	ok := func(args ...string) string { t.Helper(); out, err := run(args...); must(t, err); return out }
	contains := func(out string, values ...string) {
		t.Helper()
		for _, value := range values {
			if !strings.Contains(out, value) {
				t.Fatalf("missing %q in:\n%s", value, out)
			}
		}
	}
	contains(ok("init", "-b", "main"), "Initialized empty Crease repository in ", ".crease/")
	contains(ok("status"), "On branch main", "No commits yet")
	ok("config", "user.name", "Test User")
	ok("config", "user.email", "test@example.com")
	if got := ok("config", "--get", "user.name"); got != "Test User\n" {
		t.Fatalf("config: %q", got)
	}
	put(t, root, "main.go", "first\n")
	put(t, root, "file with spaces", "other\n")
	put(t, root, "-dash", "dash\n")
	contains(ok("status", "--porcelain"), "?? main.go", `?? "file with spaces"`, "?? -dash")
	ok("add", "--all")
	contains(ok("status", "-sb"), "## No commits yet on main", "A  main.go")
	zero := ok("status", "--porcelain=v1", "-z")
	if !strings.Contains(zero, "A  file with spaces\x00") || strings.Contains(zero, "\n") {
		t.Fatalf("bad NUL status: %q", zero)
	}
	initial := ok("commit", "--message", "hello")
	if !regexp.MustCompile(`\[main \(root-commit\) [0-9a-f]{7}\] hello`).MatchString(initial) {
		t.Fatalf("bad root commit: %s", initial)
	}
	contains(initial, "3 files changed, 3 insertions(+)", "create mode 100644 main.go")
	first := strings.TrimSpace(ok("rev-parse", "HEAD"))
	contains(ok("status"), "nothing to commit, working tree clean")
	contains(ok("init"), "Reinitialized existing Crease repository")
	if got := strings.TrimSpace(ok("rev-parse", "HEAD")); got != first {
		t.Fatal("reinitialization changed history")
	}
	if out := ok("status", "--porcelain"); out != "" {
		t.Fatalf("clean porcelain: %q", out)
	}
	put(t, root, "main.go", "second\nextra\n")
	put(t, root, "untracked", "do not commit\n")
	contains(ok("status"), "Changes to be committed:", "modified:", "Untracked files:", "Crease commits all tracked changes")
	if out := ok("diff", "--name-status"); out != "M\tmain.go\n" {
		t.Fatalf("name status: %q", out)
	}
	contains(ok("diff", "--stat"), "main.go | 3 ++-", "1 file changed, 2 insertions(+), 1 deletion(-)")
	contains(ok("diff"), "diff --git a/main.go b/main.go", "--- a/main.go", "+extra")
	out, err := run("diff", "--exit-code")
	var reported *cli.ExitError
	if !errors.As(err, &reported) || cli.FormatError(err) != "" || out == "" {
		t.Fatalf("diff exit behavior: %q %v", out, err)
	}
	contains(ok("commit", "-am", "second"), "1 file changed, 2 insertions(+), 1 deletion(-)")
	second := strings.TrimSpace(ok("rev-parse", "HEAD"))
	if got := strings.TrimSpace(ok("rev-parse", "HEAD~")); got != first {
		t.Fatalf("first parent: %s", got)
	}
	if got := strings.TrimSpace(ok("rev-parse", second[:7])); got != second {
		t.Fatalf("short ID: %s", got)
	}
	if got := ok("log", "--oneline", "-n1"); got != second[:7]+" second\n" {
		t.Fatalf("oneline: %q", got)
	}
	contains(ok("log", "--decorate", "-n", "1"), "(HEAD -> main)", "Author: Test User <test@example.com>", "    second\n")
	contains(ok("show", first[:7]), "commit "+first, "new file mode 100644", "+first")
	ok("branch", "feature")
	if got := ok("branch"); got != "  feature\n* main\n" {
		t.Fatalf("branch list: %q", got)
	}
	contains(ok("checkout", "feature"), "Switched to branch 'feature'")
	contains(ok("switch", "main"), "Switched to branch 'main'")
	ok("branch", "-m", "feature", "renamed")
	contains(ok("branch", "-d", "renamed"), "Deleted branch renamed")
	contains(ok("checkout", "-b", "unmerged"), "Switched to a new branch 'unmerged'")
	put(t, root, "main.go", "branch edit\n")
	ok("commit", "-qmbranch edit")
	ok("switch", "main")
	if _, err := run("branch", "-d", "unmerged"); err == nil || !strings.Contains(err.Error(), "not fully merged") {
		t.Fatalf("unmerged deletion: %v", err)
	}
	ok("branch", "-D", "unmerged")
	if _, err := run("branch", "-D", "main"); err == nil {
		t.Fatal("deleted current branch")
	}
	contains(ok("switch", "-c", "next"), "Switched to a new branch 'next'")
	ok("switch", "main")
	ok("rm", "--cached", "--", "-dash")
	if get(t, root, "-dash") != "dash\n" {
		t.Fatal("rm --cached removed working file")
	}
	contains(ok("commit", "-m", "untrack"), "delete mode 100644 -dash")
	out, err = run("commit", "-m", "empty")
	if !errors.As(err, &reported) || cli.FormatError(err) != "" {
		t.Fatalf("empty commit: %v", err)
	}
	contains(out, "nothing added to commit but untracked files present")
	if got := strings.TrimSpace(ok("-C", root, "rev-parse", "--show-toplevel")); got != root {
		t.Fatalf("-C: %s", got)
	}
}

func TestGitStyleRemoteWorkflow(t *testing.T) {
	f := fixtureServer(t)
	project := filepath.Join(f.root, "existing")
	f.run(t, f.root, f.owner, "init", project)
	put(t, project, "main.go", "base\n")
	f.run(t, project, f.owner, "add", "-A")
	f.run(t, project, f.owner, "commit", "-m", "hello")
	f.run(t, project, f.owner, "create", f.url, ".")
	if out := f.run(t, project, f.owner, "remote"); out != "origin\n" {
		t.Fatalf("remote list: %q", out)
	}
	if out := f.run(t, project, f.owner, "remote", "-v"); !strings.Contains(out, "origin\t"+f.url+" (push)") {
		t.Fatalf("verbose remotes: %s", out)
	}
	if out := f.run(t, project, f.owner, "push", "-u", "origin", "main"); !strings.Contains(out, "[new branch]") || !strings.Contains(out, "set up to track 'origin/main'") {
		t.Fatalf("first push: %s", out)
	}
	if out := f.run(t, project, f.owner, "push", "origin", "main"); out != "Everything up-to-date\n" {
		t.Fatalf("noop push: %q", out)
	}
	if out := f.run(t, project, f.owner, "pull", "--ff-only", "origin", "main"); out != "Already up to date.\n" {
		t.Fatalf("noop pull: %q", out)
	}
	if out := f.run(t, project, f.owner, "status"); !strings.Contains(out, "Your branch is up to date with 'origin/main'") {
		t.Fatalf("upstream status: %s", out)
	}
	clone := filepath.Join(f.root, "clone")
	if out := f.run(t, f.root, f.owner, "clone", f.url, clone); !strings.Contains(out, "Cloning into ") || !strings.HasSuffix(out, "done.\n") {
		t.Fatalf("clone output: %s", out)
	}
	put(t, project, "main.go", "published\n")
	f.run(t, project, f.owner, "commit", "-m", "next")
	if out := f.run(t, project, f.owner, "push"); !strings.Contains(out, "To "+f.url) || !strings.Contains(out, "main -> main") {
		t.Fatalf("push output: %s", out)
	}
	if out := f.run(t, clone, f.owner, "pull", "origin", "main"); !strings.Contains(out, "Fast-forward") || !strings.Contains(out, "-> origin/main") {
		t.Fatalf("pull output: %s", out)
	}
	if got := get(t, clone, "main.go"); got != "published\n" {
		t.Fatalf("pull contents: %q", got)
	}
	if out := f.run(t, clone, f.owner, "diff", "origin/main", "HEAD"); out != "" {
		t.Fatalf("origin ref: %q", out)
	}
	if out := f.run(t, clone, f.owner, "diff", "FETCH_HEAD", "HEAD"); out != "" {
		t.Fatalf("fetched revision: %q", out)
	}
	f.run(t, clone, f.owner, "remote", "remove", "origin")
	if out := f.run(t, clone, f.owner, "remote"); out != "" {
		t.Fatalf("removed remote still listed: %s", out)
	}
	f.run(t, clone, f.owner, "remote", "add", "origin", f.url)
	if _, err := f.try(t, clone, f.owner, "remote", "add", "origin", f.url); err == nil {
		t.Fatal("duplicate origin accepted")
	}
	f.run(t, clone, f.owner, "remote", "set-url", "origin", f.url)
	if out := f.run(t, clone, f.owner, "remote", "get-url", "origin"); out != f.url+"\n" {
		t.Fatalf("get-url: %q", out)
	}
	if _, err := f.try(t, clone, f.owner, "push", "--force", "origin", "main"); err == nil {
		t.Fatal("force push accepted")
	}
}

func TestGitStyleHelpAndErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"help", "status"}, {"status", "--help"}, {"branch", "-h"}, {"--version"}} {
		var out, stderr bytes.Buffer
		must(t, cli.Run(args, strings.NewReader(""), &out, &stderr, "test"))
		if out.Len() == 0 {
			t.Fatalf("empty help for %v", args)
		}
	}
	if got := cli.FormatError(errors.New("no remote configured")); got != "fatal: no remote configured" {
		t.Fatalf("fatal formatting: %s", got)
	}
	if got := cli.FormatError(errors.New("unknown flag --force")); got != "error: unknown flag --force" {
		t.Fatalf("usage formatting: %s", got)
	}
	if _, err := os.Stat(".crease"); !os.IsNotExist(err) {
		t.Fatal("help created a repository")
	}
}
