package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"crease/internal/model"
	"crease/internal/protocol"
	"golang.org/x/crypto/ssh"
)

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func token() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func rank(role string) int {
	switch role {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}
func (s *Server) permission(actor, name string, required int) (string, error) {
	var id, owner string
	e := s.DB.QueryRow(`SELECT id,owner FROM repositories WHERE name=?`, name).Scan(&id, &owner)
	if e != nil {
		return "", errors.New("repository unavailable or access denied")
	}
	if actor == s.owner || actor == owner {
		return id, nil
	}
	var role string
	_ = s.DB.QueryRow(`SELECT role FROM grants WHERE repo=? AND identity=?`, id, actor).Scan(&role)
	if rank(role) < required {
		return "", errors.New("repository unavailable or access denied")
	}
	return id, nil
}
func (s *Server) refs(repo string) (map[string]string, error) {
	rows, e := s.DB.Query(`SELECT name,head FROM refs WHERE repo=? ORDER BY name`, repo)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, head string
		if e = rows.Scan(&name, &head); e != nil {
			return nil, e
		}
		out[name] = head
	}
	return out, rows.Err()
}
func (s *Server) ref(repo, name string) (string, error) {
	var head string
	e := s.DB.QueryRow(`SELECT head FROM refs WHERE repo=? AND name=?`, repo, name).Scan(&head)
	return head, e
}

func (s *Server) Handle(actor string, q protocol.Request) (protocol.Response, error) {
	var out protocol.Response
	if q.Version != protocol.Version {
		return out, errors.New("unsupported protocol version")
	}
	if !model.ValidRef(q.Repo) {
		return out, errors.New("invalid repository name")
	}
	if len(q.IDs) > protocol.MaxBatch || len(q.Objects) > protocol.MaxBatch || len(q.Body) > 64<<10 || len(q.Title) > 512 || len(q.Token) > 128 || len(q.PublicKey) > 16<<10 {
		return out, errors.New("request exceeds limits")
	}
	if q.Op == "capabilities" {
		out.Capabilities = []string{"binary-stream-v1", "history-frontiers-v1", "graph-proofs-v1"}
		return out, nil
	}
	if q.Op == "create" {
		if actor != s.owner {
			return out, errors.New("only the server owner can create repositories")
		}
		var exists string
		e := s.DB.QueryRow(`SELECT id FROM repositories WHERE name=?`, q.Repo).Scan(&exists)
		if e == nil {
			return out, nil
		}
		if e != sql.ErrNoRows {
			return out, e
		}
		id := token()
		tx, e := s.DB.Begin()
		if e != nil {
			return out, e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`INSERT INTO repositories VALUES (?,?,?)`, id, q.Repo, actor); e != nil {
			return out, e
		}
		if _, e = tx.Exec(`INSERT INTO refs VALUES (?,'main','')`, id); e != nil {
			return out, e
		}
		return out, tx.Commit()
	}
	required := 1
	switch q.Op {
	case "put", "push", "pr_create", "pr_approve", "pr_comment", "pr_merge":
		required = 2
	case "access_grant", "access_revoke", "policy_set", "check_report":
		required = 3
	}
	repo, e := s.permission(actor, q.Repo, required)
	if e != nil {
		return out, e
	}
	store := s.Store(repo)
	switch q.Op {
	case "common":
		for _, id := range q.IDs {
			if !model.ValidID(id) {
				return out, errors.New("invalid history frontier")
			}
			p, ok, err := s.proof(repo, id)
			if err != nil {
				return out, err
			}
			if ok && p.Kind == "commit" {
				out.IDs = append(out.IDs, id)
			}
		}
	case "policy_get", "policy_set", "check_report", "check_list":
		return s.checks(repo, actor, q)
	case "refs":
		out.Refs, e = s.refs(repo)
		out.Default = "main"
	case "have":
		for _, id := range q.IDs {
			if !model.ValidID(id) {
				return out, errors.New("invalid object ID")
			}
			if !store.Exists(id) {
				out.IDs = append(out.IDs, id)
			}
		}
	case "get":
		out.Objects = map[string]model.Object{}
		size := 0
		for _, id := range q.IDs {
			o, err := store.Get(id)
			if err != nil {
				return out, fmt.Errorf("object unavailable: %s", id)
			}
			if size+len(o.Data) > model.MaxObject && len(out.Objects) > 0 {
				break
			}
			out.Objects[id] = o
			size += len(o.Data)
		}
	case "put":
		size := 0
		for _, o := range q.Objects {
			size += len(o.Data)
			if size > model.MaxObject {
				return out, errors.New("object batch exceeds limit")
			}
			id, err := store.Put(o)
			if err != nil {
				return out, err
			}
			out.IDs = append(out.IDs, id)
		}
		e = store.Flush()
	case "push":
		e = s.push(repo, actor, q)
	case "access_list":
		rows, err := s.DB.Query(`SELECT identity,role FROM grants WHERE repo=? ORDER BY identity`, repo)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var g protocol.Grant
			if err = rows.Scan(&g.Fingerprint, &g.Role); err != nil {
				rows.Close()
				return out, err
			}
			out.Grants = append(out.Grants, g)
		}
		e = rows.Err()
		rows.Close()
		out.Grants = append(out.Grants, protocol.Grant{Fingerprint: s.owner, Role: "owner"})
	case "access_grant", "access_revoke":
		pub, err := ParseKey(q.PublicKey)
		if err != nil {
			return out, err
		}
		fp := ssh.FingerprintSHA256(pub)
		if fp == s.owner {
			return out, errors.New("server owner access cannot be changed here")
		}
		if q.Op == "access_revoke" {
			_, e = s.DB.Exec(`DELETE FROM grants WHERE repo=? AND identity=?`, repo, fp)
			break
		}
		if rank(q.Role) == 0 {
			return out, errors.New("role must be read, write, or admin")
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return out, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`INSERT OR IGNORE INTO identities VALUES (?,?)`, fp, string(ssh.MarshalAuthorizedKey(pub))); err != nil {
			return out, err
		}
		if _, err = tx.Exec(`INSERT INTO grants VALUES (?,?,?) ON CONFLICT(repo,identity) DO UPDATE SET role=excluded.role`, repo, fp, q.Role); err != nil {
			return out, err
		}
		e = tx.Commit()
	case "pr_create", "pr_list", "pr_get", "pr_diff", "pr_approve", "pr_comment", "pr_merge", "job_get":
		return s.review(repo, actor, q)
	default:
		e = errors.New("unsupported operation")
	}
	return out, e
}

func (s *Server) push(repo, actor string, q protocol.Request) error {
	if !model.ValidRef(q.Branch) || !model.ValidID(q.Head) || (q.Expected != "" && !model.ValidID(q.Expected)) {
		return errors.New("invalid branch update")
	}
	store := s.Store(repo)
	if e := store.Flush(); e != nil {
		return e
	}
	old, e := s.ref(repo, q.Branch)
	if e == sql.ErrNoRows {
		old = ""
	} else if e != nil {
		return e
	}
	if old != q.Expected {
		if old == q.Head {
			return nil
		}
		return errors.New("remote branch changed; fetch and merge before pushing")
	}
	if old == "" {
		var count int
		if e = s.DB.QueryRow(`SELECT count(*) FROM refs WHERE repo=?`, repo).Scan(&count); e != nil {
			return e
		}
		if count >= 50000 {
			return errors.New("repository branch limit reached")
		}
	}
	proofs, e := s.validate(repo, q.Head)
	if e != nil {
		return fmt.Errorf("incomplete or invalid commit graph: %w", e)
	}
	ok, e := store.IsAncestor(old, q.Head)
	if e != nil {
		return e
	}
	if !ok {
		return errors.New("non-fast-forward push rejected")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var current string
	e = tx.QueryRow(`SELECT head FROM refs WHERE repo=? AND name=?`, repo, q.Branch).Scan(&current)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if current != old {
		return errors.New("remote branch changed concurrently; retry after fetching")
	}
	if e = persistProofs(tx, repo, proofs); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO refs VALUES (?,?,?) ON CONFLICT(repo,name) DO UPDATE SET head=excluded.head`, repo, q.Branch, q.Head); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO movements(repo,branch,old,head,actor,time) VALUES (?,?,?,?,?,?)`, repo, q.Branch, old, q.Head, actor, now()); e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE prs SET head=? WHERE repo=? AND source=? AND state='open'`, q.Head, repo, q.Branch); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT OR IGNORE INTO revisions SELECT id,head,base FROM prs WHERE repo=? AND source=? AND state='open'`, repo, q.Branch); e != nil {
		return e
	}
	return tx.Commit()
}

func limit(q protocol.Request) int {
	if q.Limit <= 0 {
		return 50
	}
	if q.Limit > 100 {
		return 100
	}
	return q.Limit
}
func (s *Server) getPR(repo string, id int64) (protocol.PR, error) {
	var p protocol.PR
	e := s.DB.QueryRow(`SELECT id,title,body,source,target,head,base,state,author,created FROM prs WHERE repo=? AND id=?`, repo, id).Scan(&p.ID, &p.Title, &p.Body, &p.Source, &p.Target, &p.Head, &p.Base, &p.State, &p.Author, &p.Created)
	if e == sql.ErrNoRows {
		return p, errors.New("pull request not found")
	}
	return p, e
}
func validText(s string) bool { return strings.TrimSpace(s) != "" }
