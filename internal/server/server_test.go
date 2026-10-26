package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"crease/internal/model"
	"crease/internal/objectstore"
	"crease/internal/protocol"
	"golang.org/x/crypto/ssh"
)

func testServer(t testing.TB) *Server {
	t.Helper()
	dir := t.TempDir()
	pub, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	key, e := ssh.NewPublicKey(pub)
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(dir, "owner.pub")
	if e = os.WriteFile(file, ssh.MarshalAuthorizedKey(key), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := Open(filepath.Join(dir, "data"), file)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}
func call(t testing.TB, s *Server, actor string, q protocol.Request) protocol.Response {
	t.Helper()
	q.Version = protocol.Version
	if q.Repo == "" {
		q.Repo = "project"
	}
	r, e := s.Handle(actor, q)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func makeCommit(t testing.TB, store objectstore.Store, parents []string, message string) string {
	t.Helper()
	id, e := store.Put(model.Object{Type: "blob", Data: []byte(message)})
	if e != nil {
		t.Fatal(e)
	}
	tree, e := store.WriteTree(objectstore.Snapshot{"file": {ID: id, Mode: "file"}})
	if e != nil {
		t.Fatal(e)
	}
	head, e := store.PutJSON("commit", model.Commit{Tree: tree, Parents: parents, Author: "test", Time: "2026-10-26T00:00:00Z", Message: message})
	if e != nil {
		t.Fatal(e)
	}
	return head
}

func TestConcurrentPushAndRepositoryIsolation(t *testing.T) {
	s := testServer(t)
	call(t, s, s.owner, protocol.Request{Op: "create"})
	repo, e := s.permission(s.owner, "project", 3)
	if e != nil {
		t.Fatal(e)
	}
	store := s.Store(repo)
	base := makeCommit(t, store, []string{}, "base")
	call(t, s, s.owner, protocol.Request{Op: "push", Branch: "main", Head: base})
	a, b := makeCommit(t, store, []string{base}, "a"), makeCommit(t, store, []string{base}, "b")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, head := range []string{a, b} {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			_, e := s.Handle(s.owner, protocol.Request{Version: 1, Repo: "project", Op: "push", Branch: "main", Head: h, Expected: base})
			errs <- e
		}(head)
	}
	wg.Wait()
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent pushes succeeded %d times", success)
	}
	call(t, s, s.owner, protocol.Request{Op: "create", Repo: "other"})
	pub, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	key, e := ssh.NewPublicKey(pub)
	if e != nil {
		t.Fatal(e)
	}
	call(t, s, s.owner, protocol.Request{Op: "access_grant", PublicKey: string(ssh.MarshalAuthorizedKey(key)), Role: "read"})
	if _, e = s.Handle(ssh.FingerprintSHA256(key), protocol.Request{Version: 1, Repo: "other", Op: "get", IDs: []string{base}}); e == nil {
		t.Fatal("reader crossed repository boundary")
	}
}

func TestIncompleteGraphAndBootstrapProtection(t *testing.T) {
	s := testServer(t)
	call(t, s, s.owner, protocol.Request{Op: "create"})
	repo, e := s.permission(s.owner, "project", 3)
	if e != nil {
		t.Fatal(e)
	}
	store := s.Store(repo)
	tree, e := store.PutJSON("tree", model.Tree{Entries: []model.Entry{{Name: "missing", Mode: "file", ID: model.Object{Type: "blob", Data: []byte("missing")}.ID()}}})
	if e != nil {
		t.Fatal(e)
	}
	head, e := store.PutJSON("commit", model.Commit{Tree: tree, Parents: []string{}, Author: "test", Time: "2026-10-26T00:00:00Z", Message: "incomplete"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Handle(s.owner, protocol.Request{Version: 1, Repo: "project", Op: "push", Branch: "main", Head: head}); e == nil {
		t.Fatal("incomplete graph published")
	}
	ref, e := s.ref(repo, "main")
	if e != nil || ref != "" {
		t.Fatal("invalid push changed ref")
	}
	if other, e := Open(s.Dir, ""); e == nil {
		other.Close()
		t.Fatal("second server opened live data directory")
	}
}

func BenchmarkReviewList10000(b *testing.B) {
	s := testServer(b)
	call(b, s, s.owner, protocol.Request{Op: "create"})
	repo, e := s.permission(s.owner, "project", 3)
	if e != nil {
		b.Fatal(e)
	}
	tx, e := s.DB.Begin()
	if e != nil {
		b.Fatal(e)
	}
	for i := 0; i < 10000; i++ {
		_, e = tx.Exec(`INSERT INTO prs(repo,title,body,source,target,head,base,state,author,created,token) VALUES (?,'title','','feature','main','','','open','test','now',?)`, repo, i)
		if e != nil {
			tx.Rollback()
			b.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		b.Fatal(e)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := s.Handle(s.owner, protocol.Request{Version: 1, Op: "pr_list", Repo: "project", Limit: 50})
		if e != nil || len(r.PRs) != 50 {
			b.Fatalf("review list: %v", e)
		}
	}
}
