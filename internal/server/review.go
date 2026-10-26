package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"crease/internal/diff"
	"crease/internal/model"
	"crease/internal/protocol"
)

func (s *Server) review(repo, actor string, q protocol.Request) (protocol.Response, error) {
	var out protocol.Response
	if q.Op == "pr_create" {
		if !model.ValidRef(q.Source) || !model.ValidRef(q.Target) || q.Source == q.Target || !validText(q.Title) || q.Token == "" {
			return out, errors.New("source, target, title, and idempotency token are required")
		}
		var id int64
		e := s.DB.QueryRow(`SELECT id FROM prs WHERE repo=? AND author=? AND token=?`, repo, actor, q.Token).Scan(&id)
		if e == nil {
			p, e := s.getPR(repo, id)
			out.PR = &p
			return out, e
		}
		if e != sql.ErrNoRows {
			return out, e
		}
		head, e := s.ref(repo, q.Source)
		if e != nil || head == "" {
			return out, errors.New("source branch has no published commit")
		}
		target, e := s.ref(repo, q.Target)
		if e != nil || target == "" {
			return out, errors.New("target branch has no published commit")
		}
		base, e := s.Store(repo).MergeBase(head, target)
		if e != nil {
			return out, e
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return out, e
		}
		defer tx.Rollback()
		result, e := tx.Exec(`INSERT INTO prs(repo,title,body,source,target,head,base,state,author,created,token) VALUES (?,?,?,?,?,?,?,'open',?,?,?)`, repo, q.Title, q.Body, q.Source, q.Target, head, base, actor, now(), q.Token)
		if e != nil {
			return out, e
		}
		id, e = result.LastInsertId()
		if e != nil {
			return out, e
		}
		if _, e = tx.Exec(`INSERT INTO revisions VALUES (?,?,?)`, id, head, base); e != nil {
			return out, e
		}
		if e = tx.Commit(); e != nil {
			return out, e
		}
		p, e := s.getPR(repo, id)
		out.PR = &p
		return out, e
	}
	if q.Op == "pr_list" {
		rows, e := s.DB.Query(`SELECT id,title,'',source,target,head,base,state,author,created FROM prs WHERE repo=? AND id>? ORDER BY id LIMIT ?`, repo, q.Cursor, limit(q))
		if e != nil {
			return out, e
		}
		defer rows.Close()
		for rows.Next() {
			var p protocol.PR
			if e = rows.Scan(&p.ID, &p.Title, &p.Body, &p.Source, &p.Target, &p.Head, &p.Base, &p.State, &p.Author, &p.Created); e != nil {
				return out, e
			}
			out.PRs = append(out.PRs, p)
		}
		return out, rows.Err()
	}
	if q.Op == "job_get" {
		var j protocol.Job
		e := s.DB.QueryRow(`SELECT id,pr,state,message,head FROM jobs WHERE repo=? AND id=?`, repo, q.ID).Scan(&j.ID, &j.PR, &j.State, &j.Message, &j.Head)
		out.Job = &j
		return out, e
	}
	p, e := s.getPR(repo, q.ID)
	if e != nil {
		return out, e
	}
	out.PR = &p
	switch q.Op {
	case "pr_get":
		rows, err := s.DB.Query(`SELECT id,actor,head,body,created FROM comments WHERE pr=? AND id>? ORDER BY id LIMIT ?`, p.ID, q.Cursor, limit(q))
		if err != nil {
			return out, err
		}
		defer rows.Close()
		size := 0
		for rows.Next() {
			var c protocol.Comment
			if err = rows.Scan(&c.ID, &c.Author, &c.Head, &c.Body, &c.Created); err != nil {
				return out, err
			}
			if size+len(c.Body) > 1<<20 {
				break
			}
			size += len(c.Body)
			out.Comments = append(out.Comments, c)
		}
		e = rows.Err()
	case "pr_diff":
		e = s.DB.QueryRow(`SELECT text FROM diffs WHERE base=? AND head=?`, p.Base, p.Head).Scan(&out.Text)
		if e == sql.ErrNoRows {
			store := s.Store(repo)
			a, err := store.Snapshot(p.Base)
			if err != nil {
				return out, err
			}
			b, err := store.Snapshot(p.Head)
			if err != nil {
				return out, err
			}
			out.Text, e = diff.Unified(store, a, b)
			if e == nil {
				_, e = s.DB.Exec(`INSERT OR IGNORE INTO diffs VALUES (?,?,?)`, p.Base, p.Head, out.Text)
			}
		}
	case "pr_approve":
		if p.State != "open" {
			return out, errors.New("pull request is not open")
		}
		if q.Head == "" || q.Head != p.Head {
			return out, errors.New("pull request changed; inspect the latest revision before approving")
		}
		_, e = s.DB.Exec(`INSERT INTO approvals VALUES (?,?,?) ON CONFLICT(pr,actor) DO UPDATE SET head=excluded.head`, p.ID, actor, p.Head)
	case "pr_comment":
		if !validText(q.Body) {
			return out, errors.New("comment cannot be empty")
		}
		if !model.ValidID(q.Head) {
			return out, errors.New("comment requires a reviewed revision")
		}
		var known int
		if e = s.DB.QueryRow(`SELECT count(*) FROM revisions WHERE pr=? AND head=?`, p.ID, q.Head).Scan(&known); e != nil {
			return out, e
		}
		if known == 0 {
			return out, errors.New("unknown pull request revision")
		}
		_, e = s.DB.Exec(`INSERT INTO comments(pr,actor,head,body,created) VALUES (?,?,?,?,?)`, p.ID, actor, q.Head, q.Body, now())
	case "pr_merge":
		var replay protocol.Job
		replayErr := s.DB.QueryRow(`SELECT id,pr,state,message,head FROM jobs WHERE repo=? AND actor=? AND token=?`, repo, actor, q.Token).Scan(&replay.ID, &replay.PR, &replay.State, &replay.Message, &replay.Head)
		if replayErr == nil {
			if replay.PR != p.ID {
				return out, errors.New("idempotency token belongs to another pull request")
			}
			out.Job = &replay
			return out, nil
		}
		if replayErr != sql.ErrNoRows {
			return out, replayErr
		}
		if p.State != "open" || q.Token == "" {
			return out, errors.New("open pull request and idempotency token required")
		}
		if q.Head != p.Head {
			return out, errors.New("pull request changed; inspect the latest revision before merging")
		}
		_, e = s.DB.Exec(`INSERT OR IGNORE INTO jobs(repo,pr,state,actor,token,source) VALUES (?,?,'queued',?,?,?)`, repo, p.ID, actor, q.Token, p.Head)
		if e != nil {
			return out, e
		}
		var j protocol.Job
		e = s.DB.QueryRow(`SELECT id,pr,state,message,head FROM jobs WHERE repo=? AND actor=? AND token=?`, repo, actor, q.Token).Scan(&j.ID, &j.PR, &j.State, &j.Message, &j.Head)
		out.Job = &j
		select {
		case s.wake <- struct{}{}:
		default:
		}
	default:
		e = errors.New("unsupported review operation")
	}
	return out, e
}

func (s *Server) worker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		_, _ = s.DB.Exec(`UPDATE jobs SET state='queued' WHERE state='checking' AND (target_head != COALESCE((SELECT r.head FROM refs r JOIN prs p ON p.repo=r.repo AND p.target=r.name WHERE p.id=jobs.pr),'') OR source != COALESCE((SELECT p.head FROM prs p WHERE p.id=jobs.pr),''))`)
		for {
			if ctx.Err() != nil {
				return
			}
			var id int64
			var repo, actor, head string
			var pr int64
			e := s.DB.QueryRow(`SELECT id,repo,pr,actor,source FROM jobs WHERE state='queued' ORDER BY id LIMIT 1`).Scan(&id, &repo, &pr, &actor, &head)
			if e == sql.ErrNoRows {
				break
			}
			if e != nil {
				s.Log.Printf("read merge queue: %v", e)
				break
			}
			_, e = s.DB.Exec(`UPDATE jobs SET state='running' WHERE id=?`, id)
			if e != nil {
				break
			}
			merged, e := s.merge(repo, pr, actor, head, id)
			if errors.Is(e, errChecking) {
				continue
			}
			if e != nil {
				_, err := s.DB.Exec(`UPDATE jobs SET state='failed', message=? WHERE id=?`, e.Error(), id)
				if err != nil {
					s.Log.Printf("record merge failure: %v", err)
				}
			} else {
				_, err := s.DB.Exec(`UPDATE jobs SET state='merged',head=?,message='' WHERE id=?`, merged, id)
				if err != nil {
					s.Log.Printf("record merge completion: %v", err)
				}
			}
		}
	}
}

func (s *Server) merge(repo string, id int64, actor, expected string, jobID int64) (string, error) {
	p, e := s.getPR(repo, id)
	if e != nil {
		return "", e
	}
	if p.State == "merged" {
		return s.ref(repo, p.Target)
	}
	if p.State != "open" || p.Head != expected {
		return "", errors.New("source changed after enqueueing; review and enqueue again")
	}
	var repoName string
	if e = s.DB.QueryRow(`SELECT name FROM repositories WHERE id=?`, repo).Scan(&repoName); e != nil {
		return "", e
	}
	if _, e = s.permission(actor, repoName, 2); e != nil {
		return "", e
	}
	target, e := s.ref(repo, p.Target)
	if e != nil {
		return "", e
	}
	store := s.Store(repo)
	var candidate, candidateTarget string
	if e = s.DB.QueryRow(`SELECT head,target_head FROM jobs WHERE id=?`, jobID).Scan(&candidate, &candidateTarget); e != nil {
		return "", e
	}
	if candidate == "" || candidateTarget != target {
		base, e := store.MergeBase(p.Head, target)
		if e != nil {
			return "", e
		}
		bs, e := store.Snapshot(base)
		if e != nil {
			return "", e
		}
		left, e := store.Snapshot(target)
		if e != nil {
			return "", e
		}
		right, e := store.Snapshot(p.Head)
		if e != nil {
			return "", e
		}
		merged, e := diff.Snapshots(store, bs, left, right)
		if e != nil {
			return "", e
		}
		tree, e := store.WriteTree(merged)
		if e != nil {
			return "", e
		}
		candidate, e = store.PutJSON("commit", model.Commit{Tree: tree, Parents: []string{target, p.Head}, Author: actor, Time: now(), Message: fmt.Sprintf("Merge pull request #%d: %s", p.ID, p.Title)})
		if e != nil {
			return "", e
		}
	}
	if e = store.Flush(); e != nil {
		return "", e
	}
	proofs, e := s.validate(repo, candidate)
	if e != nil {
		return "", e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if e = persistProofs(tx, repo, proofs); e != nil {
		return "", e
	}
	var current, source, state string
	if e = tx.QueryRow(`SELECT head FROM refs WHERE repo=? AND name=?`, repo, p.Target).Scan(&current); e != nil {
		return "", e
	}
	if current != target {
		return "", errors.New("target changed during merge; enqueue again")
	}
	if e = tx.QueryRow(`SELECT head,state FROM prs WHERE repo=? AND id=?`, repo, p.ID).Scan(&source, &state); e != nil {
		return "", e
	}
	if source != p.Head || state != "open" {
		return "", errors.New("pull request changed during merge")
	}
	// An approval only counts while the reviewer still has write access.
	var approved int
	e = tx.QueryRow(`SELECT count(*) FROM approvals a WHERE a.pr=? AND a.head=? AND (a.actor=? OR EXISTS (SELECT 1 FROM grants g WHERE g.repo=? AND g.identity=a.actor AND g.role IN ('write','admin')))`, p.ID, p.Head, s.owner, repo).Scan(&approved)
	if e != nil {
		return "", e
	}
	if approved == 0 {
		return "", errors.New("latest source revision requires an approval from a current writer")
	}
	// Recheck the submitter's grant inside the publication transaction too.
	if actor != s.owner {
		var role string
		if e = tx.QueryRow(`SELECT role FROM grants WHERE repo=? AND identity=?`, repo, actor).Scan(&role); e != nil || rank(role) < 2 {
			return "", errors.New("merge submitter no longer has write access")
		}
	}
	rows, e := tx.Query(`SELECT name FROM required_checks WHERE repo=? ORDER BY name`, repo)
	if e != nil {
		return "", e
	}
	var names []string
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			rows.Close()
			return "", e
		}
		names = append(names, name)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return "", e
	}
	waiting := false
	for _, name := range names {
		var status string
		e = tx.QueryRow(`SELECT c.status FROM checks c WHERE c.job=? AND c.candidate=? AND c.name=? AND (c.actor=? OR EXISTS (SELECT 1 FROM grants g WHERE g.repo=? AND g.identity=c.actor AND g.role='admin'))`, jobID, candidate, name, s.owner, repo).Scan(&status)
		if e != nil && e != sql.ErrNoRows {
			return "", e
		}
		if status == "failure" {
			return "", fmt.Errorf("required check failed: %s", name)
		}
		if status != "success" {
			waiting = true
		}
	}
	if waiting {
		if _, e = tx.Exec(`UPDATE jobs SET state='checking',head=?,target_head=?,message='waiting for required checks' WHERE id=?`, candidate, target, jobID); e != nil {
			return "", e
		}
		if e = tx.Commit(); e != nil {
			return "", e
		}
		return candidate, errChecking
	}
	if _, e = tx.Exec(`UPDATE refs SET head=? WHERE repo=? AND name=? AND head=?`, candidate, repo, p.Target, target); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE prs SET state='merged' WHERE repo=? AND id=?`, repo, p.ID); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE jobs SET state='merged',head=?,message='' WHERE id=?`, candidate, jobID); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE prs SET head=? WHERE repo=? AND source=? AND state='open'`, candidate, repo, p.Target); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`INSERT OR IGNORE INTO revisions SELECT id,head,base FROM prs WHERE repo=? AND source=? AND state='open'`, repo, p.Target); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`INSERT INTO movements(repo,branch,old,head,actor,time) VALUES (?,?,?,?,?,?)`, repo, p.Target, target, candidate, actor, now()); e != nil {
		return "", e
	}
	return candidate, tx.Commit()
}
