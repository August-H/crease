package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crease/internal/metadata"
	"crease/internal/objectstore"
	"crease/internal/protocol"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const schema = `
CREATE TABLE IF NOT EXISTS config (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS identities (fingerprint TEXT PRIMARY KEY, public_key TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS repositories (id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, owner TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS grants (repo TEXT NOT NULL, identity TEXT NOT NULL, role TEXT NOT NULL, PRIMARY KEY(repo,identity));
CREATE TABLE IF NOT EXISTS refs (repo TEXT NOT NULL, name TEXT NOT NULL, head TEXT NOT NULL, PRIMARY KEY(repo,name));
CREATE TABLE IF NOT EXISTS movements (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, branch TEXT NOT NULL, old TEXT, head TEXT, actor TEXT NOT NULL, time TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS prs (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL, source TEXT NOT NULL, target TEXT NOT NULL, head TEXT NOT NULL, base TEXT NOT NULL, state TEXT NOT NULL, author TEXT NOT NULL, created TEXT NOT NULL, token TEXT NOT NULL, UNIQUE(repo,author,token));
CREATE INDEX IF NOT EXISTS prs_listing ON prs(repo,id);
CREATE INDEX IF NOT EXISTS prs_source ON prs(repo,source,state);
CREATE TABLE IF NOT EXISTS revisions (pr INTEGER NOT NULL, head TEXT NOT NULL, base TEXT NOT NULL, PRIMARY KEY(pr,head,base));
CREATE TABLE IF NOT EXISTS approvals (pr INTEGER NOT NULL, actor TEXT NOT NULL, head TEXT NOT NULL, PRIMARY KEY(pr,actor));
CREATE TABLE IF NOT EXISTS comments (id INTEGER PRIMARY KEY, pr INTEGER NOT NULL, actor TEXT NOT NULL, head TEXT NOT NULL, body TEXT NOT NULL, created TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS comments_listing ON comments(pr,id);
CREATE TABLE IF NOT EXISTS jobs (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, pr INTEGER NOT NULL, state TEXT NOT NULL, message TEXT NOT NULL DEFAULT '', head TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, target_head TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL, token TEXT NOT NULL, UNIQUE(repo,actor,token));
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(state,id);
CREATE TABLE IF NOT EXISTS required_checks (repo TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY(repo,name));
CREATE TABLE IF NOT EXISTS checks (job INTEGER NOT NULL, candidate TEXT NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL, actor TEXT NOT NULL, PRIMARY KEY(job,candidate,name));
CREATE TABLE IF NOT EXISTS diffs (base TEXT NOT NULL, head TEXT NOT NULL, text TEXT NOT NULL, PRIMARY KEY(base,head));
CREATE TABLE IF NOT EXISTS graph_proofs (repo TEXT NOT NULL, id TEXT NOT NULL, kind TEXT NOT NULL, height INTEGER NOT NULL, PRIMARY KEY(repo,id));
`

type Server struct {
	Dir    string
	DB     *sql.DB
	Signer ssh.Signer
	owner  string
	lock   *os.File
	Log    *log.Logger
	wake   chan struct{}
	stores sync.Map
}

func ParseKey(raw string) (ssh.PublicKey, error) {
	pub, _, opts, rest, e := ssh.ParseAuthorizedKey([]byte(raw))
	if e != nil {
		return nil, e
	}
	if len(opts) > 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("expected one public key without authorized_keys options")
	}
	return pub, nil
}
func Open(dir, ownerFile string) (*Server, error) {
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(abs, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("server data directory is already in use")
	}
	db, e := metadata.Open(filepath.Join(abs, "server.db"), schema)
	if e != nil {
		f.Close()
		return nil, e
	}
	s := &Server{Dir: abs, DB: db, lock: f, Log: log.New(os.Stderr, "crease: ", log.LstdFlags), wake: make(chan struct{}, 1)}
	fail := func(e error) (*Server, error) { s.Close(); return nil, e }
	e = db.QueryRow(`SELECT value FROM config WHERE key='owner'`).Scan(&s.owner)
	if e == sql.ErrNoRows {
		b, e := os.ReadFile(ownerFile)
		if e != nil {
			return fail(fmt.Errorf("first initialization requires --owner-key: %w", e))
		}
		pub, e := ParseKey(string(b))
		if e != nil {
			return fail(e)
		}
		s.owner = ssh.FingerprintSHA256(pub)
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		if _, e = tx.Exec(`INSERT INTO identities VALUES (?,?)`, s.owner, string(ssh.MarshalAuthorizedKey(pub))); e == nil {
			_, e = tx.Exec(`INSERT INTO config VALUES ('owner',?)`, s.owner)
		}
		if e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	} else if e != nil {
		return fail(e)
	}
	keyFile := filepath.Join(abs, "host_key")
	b, e := os.ReadFile(keyFile)
	if os.IsNotExist(e) {
		_, priv, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return fail(e)
		}
		block, e := ssh.MarshalPrivateKey(priv, "crease server")
		if e != nil {
			return fail(e)
		}
		b = pem.EncodeToMemory(block)
		if e = objectstore.Atomic(keyFile, b, 0600); e != nil {
			return fail(e)
		}
	} else if e != nil {
		return fail(e)
	}
	s.Signer, e = ssh.ParsePrivateKey(b)
	if e != nil {
		return fail(e)
	}
	if _, e = db.Exec(`UPDATE jobs SET state='queued', message='resuming after restart' WHERE state='running'`); e != nil {
		return fail(e)
	}
	return s, nil
}
func (s *Server) Close() { s.DB.Close(); unix.Flock(int(s.lock.Fd()), unix.LOCK_UN); s.lock.Close() }
func (s *Server) Store(repo string) objectstore.Store {
	if cached, ok := s.stores.Load(repo); ok {
		return cached.(objectstore.Store)
	}
	store := objectstore.New(filepath.Join(s.Dir, "repositories", repo, "objects"))
	actual, _ := s.stores.LoadOrStore(repo, store)
	return actual.(objectstore.Store)
}
func (s *Server) authorized(key ssh.PublicKey) (string, error) {
	fp := ssh.FingerprintSHA256(key)
	var count int
	e := s.DB.QueryRow(`SELECT count(*) FROM identities WHERE fingerprint=? AND (fingerprint=? OR EXISTS (SELECT 1 FROM grants WHERE identity=?))`, fp, s.owner, fp).Scan(&count)
	if e != nil {
		return "", e
	}
	if count == 0 {
		return "", errors.New("key has no server access")
	}
	return fp, nil
}

// Serve blocks until cancellation and waits for connections and the merge worker
// before returning. Call Close only after Serve has returned.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var conns sync.Map
	wg.Add(1)
	go func() { defer wg.Done(); s.worker(ctx) }()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
			conns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
		case <-stop:
		}
	}()
	sem := make(chan struct{}, 64)
	var result error
	for {
		conn, e := listener.Accept()
		if e != nil {
			if ctx.Err() == nil {
				result = e
			}
			break
		}
		select {
		case sem <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		conns.Store(conn, true)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem; conns.Delete(conn); conn.Close() }()
			s.connection(conn)
		}()
	}
	cancel()
	conns.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
	wg.Wait()
	close(stop)
	return result
}
func (s *Server) connection(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	config := &ssh.ServerConfig{MaxAuthTries: 3, PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		fp, e := s.authorized(key)
		if e != nil {
			return nil, e
		}
		return &ssh.Permissions{Extensions: map[string]string{"identity": fp}}, nil
	}}
	config.AddHostKey(s.Signer)
	sc, chans, reqs, e := ssh.NewServerConn(conn, config)
	if e != nil {
		return
	}
	defer sc.Close()
	var sessions sync.WaitGroup
	defer func() { sc.Close(); sessions.Wait() }()
	go ssh.DiscardRequests(reqs)
	identity := sc.Permissions.Extensions["identity"]
	// Only one repository RPC session per connection. Reject shell and forwarding.
	used := false
	for channel := range chans {
		if channel.ChannelType() != "session" || used {
			channel.Reject(ssh.Prohibited, "repository RPC only")
			continue
		}
		used = true
		ch, requests, e := channel.Accept()
		if e != nil {
			return
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer ch.Close()
			for req := range requests {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var exec struct{ Command string }
				if ssh.Unmarshal(req.Payload, &exec) != nil || exec.Command != "crease-rpc-v1" {
					req.Reply(false, nil)
					return
				}
				req.Reply(true, nil)
				s.rpc(conn, ch, identity)
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}
func (s *Server) rpc(conn net.Conn, ch ssh.Channel, identity string) {
	for {
		conn.SetDeadline(time.Now().Add(2 * time.Minute))
		var req protocol.Request
		if e := protocol.Read(ch, &req); e != nil {
			if e != io.EOF {
				s.Log.Printf("RPC framing rejected for %s", identity)
			}
			return
		}
		if req.Op == "fetch_stream" || req.Op == "get_stream" || req.Op == "put_stream" {
			if e := s.stream(conn, ch, identity, req); e != nil {
				return
			}
			continue
		}
		res, e := s.Handle(identity, req)
		res.Version = protocol.Version
		if e != nil {
			res = protocol.Response{Version: protocol.Version, Error: e.Error()}
		}
		if e = protocol.Write(ch, res); e != nil {
			return
		}
	}
}
