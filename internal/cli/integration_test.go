package cli_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crease/internal/backup"
	"crease/internal/cli"
	"crease/internal/protocol"
	"crease/internal/server"
	"crease/internal/syncer"
	"crease/internal/transport"
	"crease/internal/workspace"
	"golang.org/x/crypto/ssh"
)

type identity struct{ priv, pub string }

func request(op string) protocol.Request { return protocol.Request{Op: op} }
func key(t *testing.T, dir, name string) identity {
	t.Helper()
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	must(t, e)
	block, e := ssh.MarshalPrivateKey(priv, "")
	must(t, e)
	s, e := ssh.NewPublicKey(pub)
	must(t, e)
	id := identity{filepath.Join(dir, name), filepath.Join(dir, name+".pub")}
	must(t, os.WriteFile(id.priv, pem.EncodeToMemory(block), 0600))
	must(t, os.WriteFile(id.pub, ssh.MarshalAuthorizedKey(s), 0600))
	return id
}

func TestLegacyRPCAndHistoryFrontiers(t *testing.T) {
	f := fixtureServer(t)
	dir := filepath.Join(f.root, "source")
	f.run(t, f.root, f.owner, "create", f.url, dir)
	put(t, dir, "file", "base\n")
	f.run(t, dir, f.owner, "add", ".")
	f.run(t, dir, f.owner, "commit", "-m", "base")
	f.run(t, dir, f.owner, "push")
	put(t, dir, "file", "second\n")
	f.run(t, dir, f.owner, "commit", "-m", "second")
	f.run(t, dir, f.owner, "push")
	c, e := transport.Dial(f.url, transport.Options{KeyFile: f.owner.priv, KnownHosts: f.known, Err: &bytes.Buffer{}})
	must(t, e)
	defer c.Close()
	if !c.Capabilities["binary-stream-v1"] {
		t.Fatal("streaming was not negotiated")
	}
	c.Capabilities = map[string]bool{}
	clone, e := workspace.Init(filepath.Join(f.root, "legacy"))
	must(t, e)
	defer clone.Close()
	must(t, clone.SetConfig("remote", f.url))
	head, e := syncer.Fetch(c, clone, "main", false)
	must(t, e)
	must(t, clone.Checkout("main", head, true, false, nil))
	put(t, clone.Root, "file", "legacy client push\n")
	_, e = clone.Commit("legacy push", "tester", nil)
	must(t, e)
	must(t, syncer.Push(c, clone))
	head, e = clone.Head()
	must(t, e)
	first, e := c.Receive(protocol.Request{Op: "fetch_stream", Head: head}, func([]byte) error { return nil })
	must(t, e)
	if first.Records < 6 {
		t.Fatal("history stream omitted metadata")
	}
	second, e := c.Receive(protocol.Request{Op: "fetch_stream", Head: head, Haves: []string{head}}, func([]byte) error { return nil })
	must(t, e)
	if second.Records != 0 {
		t.Fatal("known metadata frontier was traversed again")
	}
	r, e := c.Call(protocol.Request{Op: "common", IDs: []string{head}})
	must(t, e)
	if len(r.IDs) != 1 {
		t.Fatal("published history proof was not persisted")
	}
}

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

type fixture struct {
	root, data, url, known string
	owner, reader          identity
	s                      *server.Server
	stop                   func()
}

func fixtureServer(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{root: root, data: filepath.Join(root, "server"), known: filepath.Join(root, "known_hosts")}
	f.owner = key(t, root, "owner")
	f.reader = key(t, root, "reader")
	f.s, f.url, f.stop = start(t, f.data, f.owner.pub)
	t.Cleanup(func() { f.stop() })
	return f
}
func start(t *testing.T, data, owner string) (*server.Server, string, func()) {
	t.Helper()
	s, e := server.Open(data, owner)
	must(t, e)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		s.Close()
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case e := <-done:
			must(t, e)
		case <-time.After(10 * time.Second):
			t.Fatal("server did not stop")
		}
		s.Close()
	}
	return s, "ssh://" + ln.Addr().String() + "/project", stop
}
func (f *fixture) run(t *testing.T, dir string, id identity, args ...string) string {
	t.Helper()
	out, e := f.try(t, dir, id, args...)
	if e != nil {
		t.Fatalf("crease %v: %v\n%s", args, e, out)
	}
	return out
}
func (f *fixture) try(t *testing.T, dir string, id identity, args ...string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	var out, stderr bytes.Buffer
	opts := []string{"--key", id.priv, "--known-hosts", f.known, "--trust-host"}
	e := cli.Run(append(opts, args...), strings.NewReader(""), &out, &stderr, "test")
	return out.String() + stderr.String(), e
}
func put(t *testing.T, dir, path, contents string) {
	t.Helper()
	file := filepath.Join(dir, path)
	must(t, os.MkdirAll(filepath.Dir(file), 0755))
	must(t, os.WriteFile(file, []byte(contents), 0644))
}
func get(t *testing.T, dir, path string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, path))
	must(t, e)
	return string(b)
}

func TestTwoClientsReviewAndLazyHistory(t *testing.T) {
	f := fixtureServer(t)
	alice := filepath.Join(f.root, "alice")
	bob := filepath.Join(f.root, "bob")
	f.run(t, f.root, f.owner, "create", f.url, alice)
	put(t, alice, "api/main.txt", "one\ntwo\nthree\n")
	put(t, alice, "web/index.txt", "web\n")
	put(t, alice, ".env", "TEST_SECRET=version-one\n")
	f.run(t, alice, f.owner, "add", ".")
	f.run(t, alice, f.owner, "commit", "-m", "initial")
	f.run(t, alice, f.owner, "push")
	w, e := workspace.Find(alice)
	must(t, e)
	old, e := w.Head()
	must(t, e)
	snap, e := w.Store.Snapshot(old)
	must(t, e)
	oldBlob := snap["api/main.txt"].ID
	w.Close()
	put(t, alice, "api/main.txt", "one\ntwo updated\nthree\n")
	f.run(t, alice, f.owner, "commit", "-m", "second")
	f.run(t, alice, f.owner, "push")
	f.run(t, alice, f.owner, "access", "grant", f.reader.pub, "--role", "write")
	f.run(t, f.root, f.reader, "clone", f.url, bob, "--paths", "api")
	if _, e = os.Stat(filepath.Join(bob, "web/index.txt")); !os.IsNotExist(e) {
		t.Fatal("sparse clone materialized an unselected path")
	}
	w, e = workspace.Find(bob)
	must(t, e)
	if w.Store.Has(oldBlob) {
		t.Fatal("clone fetched historical contents eagerly")
	}
	w.Close()
	f.run(t, bob, f.reader, "diff", old, "HEAD")
	w, e = workspace.Find(bob)
	must(t, e)
	if !w.Store.Has(oldBlob) {
		t.Fatal("historical diff did not fetch required blob")
	}
	w.Close()
	f.run(t, bob, f.reader, "branch", "create", "feature/login")
	put(t, bob, "api/main.txt", "ONE\ntwo updated\nthree\n")
	f.run(t, bob, f.reader, "commit", "-m", "login")
	f.run(t, bob, f.reader, "push")
	created := f.run(t, bob, f.reader, "pr", "create", "--base", "main", "--title", "Login")
	if !strings.Contains(created, "#1") {
		t.Fatal(created)
	}
	f.run(t, alice, f.owner, "pr", "diff", "1")
	f.run(t, alice, f.owner, "pr", "comment", "1", "--body", "Reviewed revision")
	f.run(t, alice, f.owner, "pr", "approve", "1")
	merged := f.run(t, alice, f.owner, "pr", "merge", "1", "--wait")
	if !strings.Contains(merged, "Merged") {
		t.Fatal(merged)
	}
	f.run(t, alice, f.owner, "pull")
	if got := get(t, alice, "api/main.txt"); got != "ONE\ntwo updated\nthree\n" {
		t.Fatal(got)
	}
	if got := get(t, alice, "web/index.txt"); got != "web\n" {
		t.Fatal("sparse commit deleted hidden file")
	}
	if get(t, alice, ".env") != "TEST_SECRET=version-one\n" {
		t.Fatal("tracked secret changed")
	}
	w, e = workspace.Find(alice)
	must(t, e)
	head, e := w.Head()
	must(t, e)
	c, e := w.Store.Commit(head)
	must(t, e)
	if len(c.Parents) != 2 {
		t.Fatal("merge did not preserve both parents")
	}
	w.Close()
	f.run(t, bob, f.reader, "sparse", "clear")
	if get(t, bob, "web/index.txt") != "web\n" {
		t.Fatal("sparse expansion failed")
	}
	f.run(t, alice, f.owner, "fsck")
}

func TestPermissionsRevocationAndStalePush(t *testing.T) {
	f := fixtureServer(t)
	a, b := filepath.Join(f.root, "a"), filepath.Join(f.root, "b")
	f.run(t, f.root, f.owner, "create", f.url, a)
	put(t, a, "a", "initial")
	f.run(t, a, f.owner, "add", ".")
	f.run(t, a, f.owner, "commit", "-m", "initial")
	f.run(t, a, f.owner, "push")
	if _, e := f.try(t, f.root, f.reader, "clone", f.url, b); e == nil {
		t.Fatal("unknown key cloned private repository")
	}
	f.run(t, a, f.owner, "access", "grant", f.reader.pub, "--role", "read")
	f.run(t, f.root, f.reader, "clone", f.url, b)
	put(t, b, "a", "reader edit")
	f.run(t, b, f.reader, "commit", "-m", "reader change")
	if _, e := f.try(t, b, f.reader, "push"); e == nil {
		t.Fatal("read-only key pushed")
	}
	f.run(t, a, f.owner, "access", "grant", f.reader.pub, "--role", "write")
	put(t, a, "a", "owner edit")
	f.run(t, a, f.owner, "commit", "-m", "owner change")
	f.run(t, a, f.owner, "push")
	if _, e := f.try(t, b, f.reader, "push"); e == nil {
		t.Fatal("stale push overwrote remote branch")
	}
	if _, e := f.try(t, b, f.reader, "pull"); e == nil {
		t.Fatal("pull overwrote diverged history")
	}
	if get(t, b, "a") != "reader edit" {
		t.Fatal("failed pull overwrote local file")
	}
	c, e := transport.Dial(f.url, transport.Options{KeyFile: f.reader.priv, KnownHosts: f.known})
	must(t, e)
	defer c.Close()
	f.run(t, a, f.owner, "access", "revoke", f.reader.pub)
	if _, e = c.Call(request("refs")); e == nil {
		t.Fatal("revoked key retained access on an existing connection")
	}
}

func TestStaleApprovalAndBackupRestore(t *testing.T) {
	f := fixtureServer(t)
	a := filepath.Join(f.root, "a")
	f.run(t, f.root, f.owner, "create", f.url, a)
	put(t, a, "a", "base\n")
	f.run(t, a, f.owner, "add", ".")
	f.run(t, a, f.owner, "commit", "-m", "base")
	f.run(t, a, f.owner, "push")
	f.run(t, a, f.owner, "branch", "create", "feature")
	put(t, a, "a", "first\n")
	f.run(t, a, f.owner, "commit", "-m", "first")
	f.run(t, a, f.owner, "push")
	f.run(t, a, f.owner, "pr", "create", "--title", "Feature")
	f.run(t, a, f.owner, "pr", "approve", "1")
	put(t, a, "a", "second\n")
	f.run(t, a, f.owner, "commit", "-m", "second")
	f.run(t, a, f.owner, "push")
	if out, e := f.try(t, a, f.owner, "pr", "merge", "1", "--wait"); e == nil || !strings.Contains(e.Error(), "approval") {
		t.Fatalf("stale approval accepted: %v %s", e, out)
	}
	if e := backup.Create(f.data, filepath.Join(f.root, "live-backup")); e == nil {
		t.Fatal("backup ran against live server")
	}
	host := ssh.FingerprintSHA256(f.s.Signer.PublicKey())
	f.stop()
	alias := filepath.Join(f.root, "server-alias")
	must(t, os.Symlink(f.data, alias))
	if e := backup.Create(alias, filepath.Join(f.data, "repositories", "nested-backup")); e == nil {
		t.Fatal("backup accepted a recursive symlink alias")
	}
	out := filepath.Join(f.root, "backup")
	must(t, backup.Create(f.data, out))
	restored := filepath.Join(f.root, "restored")
	must(t, backup.Restore(out, restored))
	s, url, stop := start(t, restored, "")
	defer stop()
	if ssh.FingerprintSHA256(s.Signer.PublicKey()) != host {
		t.Fatal("restoration changed host identity")
	}
	f.url = url
	f.run(t, a, f.owner, "remote", "set", url)
	f.run(t, a, f.owner, "pr", "approve", "1")
	f.run(t, a, f.owner, "pr", "merge", "1", "--wait")
	f.run(t, a, f.owner, "switch", "main")
	f.run(t, a, f.owner, "pull")
	if get(t, a, "a") != "second\n" {
		t.Fatal("restored merge failed")
	}
	put(t, out, "host_key", "tampered")
	if e := backup.Restore(out, filepath.Join(f.root, "tampered")); e == nil {
		t.Fatal("corrupt backup restored")
	}
}

func TestHostKeyChangeRejected(t *testing.T) {
	f := fixtureServer(t)
	c, e := transport.Dial(f.url, transport.Options{KeyFile: f.owner.priv, KnownHosts: f.known, TrustHost: true, Err: &bytes.Buffer{}})
	must(t, e)
	c.Close()
	pub, _, e := ed25519.GenerateKey(rand.Reader)
	must(t, e)
	key, e := ssh.NewPublicKey(pub)
	must(t, e)
	line := fmt.Sprintf("[%s]:%s %s", "127.0.0.1", strings.Split(strings.Split(f.url, "//")[1], "/")[0][len("127.0.0.1:"):], ssh.MarshalAuthorizedKey(key))
	must(t, os.WriteFile(f.known, []byte(line), 0600))
	if c, e = transport.Dial(f.url, transport.Options{KeyFile: f.owner.priv, KnownHosts: f.known, TrustHost: true, Err: &bytes.Buffer{}}); e == nil {
		c.Close()
		t.Fatal("changed host key accepted")
	}
}

func TestManualConflictResolution(t *testing.T) {
	f := fixtureServer(t)
	dir := filepath.Join(f.root, "local")
	f.run(t, f.root, f.owner, "init", dir)
	put(t, dir, "file", "base\n")
	f.run(t, dir, f.owner, "add", ".")
	f.run(t, dir, f.owner, "commit", "-m", "base")
	f.run(t, dir, f.owner, "branch", "create", "feature")
	put(t, dir, "file", "theirs\n")
	f.run(t, dir, f.owner, "commit", "-m", "theirs")
	f.run(t, dir, f.owner, "switch", "main")
	put(t, dir, "file", "ours\n")
	f.run(t, dir, f.owner, "commit", "-m", "ours")
	if _, e := f.try(t, dir, f.owner, "merge", "feature"); e == nil {
		t.Fatal("overlap silently merged")
	}
	f.run(t, dir, f.owner, "merge", "--abort")
	if get(t, dir, "file") != "ours\n" {
		t.Fatal("abort lost original files")
	}
	if _, e := f.try(t, dir, f.owner, "merge", "feature"); e == nil {
		t.Fatal("expected pending resolution")
	}
	put(t, dir, "file", "resolved\n")
	f.run(t, dir, f.owner, "merge", "--continue")
	w, e := workspace.Find(dir)
	must(t, e)
	defer w.Close()
	head, e := w.Head()
	must(t, e)
	c, e := w.Store.Commit(head)
	must(t, e)
	if len(c.Parents) != 2 {
		t.Fatal("resolution lost merge parents")
	}
	if get(t, dir, "file") != "resolved\n" {
		t.Fatal("resolution not preserved")
	}
}

func TestRequiredChecksApplyToExactCandidate(t *testing.T) {
	f := fixtureServer(t)
	dir := filepath.Join(f.root, "work")
	f.run(t, f.root, f.owner, "create", f.url, dir)
	put(t, dir, "a", "base\n")
	f.run(t, dir, f.owner, "add", ".")
	f.run(t, dir, f.owner, "commit", "-m", "base")
	f.run(t, dir, f.owner, "push")
	f.run(t, dir, f.owner, "branch", "create", "feature")
	put(t, dir, "b", "feature\n")
	f.run(t, dir, f.owner, "add", "b")
	f.run(t, dir, f.owner, "commit", "-m", "feature")
	f.run(t, dir, f.owner, "push")
	f.run(t, dir, f.owner, "pr", "create", "--title", "Feature")
	f.run(t, dir, f.owner, "pr", "approve", "1")
	f.run(t, dir, f.owner, "policy", "checks", "set", "tests", "lint")
	f.run(t, dir, f.owner, "pr", "merge", "1")
	c, e := transport.Dial(f.url, transport.Options{KeyFile: f.owner.priv, KnownHosts: f.known, Err: &bytes.Buffer{}})
	must(t, e)
	defer c.Close()
	await := func(state, previous string) protocol.Job {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			r, e := c.Call(protocol.Request{Op: "job_get", ID: 1})
			must(t, e)
			if r.Job.State == state && (previous == "" || r.Job.Head != previous) {
				return *r.Job
			}
			if r.Job.State == "failed" {
				t.Fatal(r.Job.Message)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("job did not reach " + state)
		return protocol.Job{}
	}
	first := await("checking", "")
	f.run(t, dir, f.owner, "fetch", "--commit", first.Head)
	f.run(t, dir, f.owner, "branch", "create", "ci-run", first.Head)
	if get(t, dir, "b") != "feature\n" {
		t.Fatal("CI could not check out candidate")
	}
	f.run(t, dir, f.owner, "switch", "main")
	put(t, dir, "c", "target advanced\n")
	f.run(t, dir, f.owner, "add", "c")
	f.run(t, dir, f.owner, "commit", "-m", "advance main")
	f.run(t, dir, f.owner, "push")
	second := await("checking", first.Head)
	if _, e := f.try(t, dir, f.owner, "check", "1", "--name", "tests", "--head", first.Head, "--status", "success"); e == nil {
		t.Fatal("old candidate check accepted")
	}
	f.run(t, dir, f.owner, "check", "1", "--name", "tests", "--head", second.Head, "--status", "success")
	f.run(t, dir, f.owner, "check", "1", "--name", "lint", "--head", second.Head, "--status", "success")
	await("merged", "")
	f.run(t, dir, f.owner, "pull")
	if get(t, dir, "c") != "target advanced\n" {
		t.Fatal("merge lost target update")
	}
	if get(t, dir, "b") != "feature\n" {
		t.Fatal("merge lost source update")
	}
}
