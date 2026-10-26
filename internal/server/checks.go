package server

import (
	"errors"

	"crease/internal/model"
	"crease/internal/protocol"
)

var errChecking = errors.New("waiting for required checks")

func (s *Server) checks(repo, actor string, q protocol.Request) (protocol.Response, error) {
	var out protocol.Response
	switch q.Op {
	case "policy_set":
		if len(q.Names) > 32 {
			return out, errors.New("at most 32 required checks are supported")
		}
		for _, n := range q.Names {
			if !model.ValidRef(n) {
				return out, errors.New("invalid check name")
			}
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return out, e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`DELETE FROM required_checks WHERE repo=?`, repo); e != nil {
			return out, e
		}
		for _, n := range q.Names {
			if _, e = tx.Exec(`INSERT OR IGNORE INTO required_checks VALUES (?,?)`, repo, n); e != nil {
				return out, e
			}
		}
		if _, e = tx.Exec(`UPDATE jobs SET state='queued' WHERE repo=? AND state='checking'`, repo); e != nil {
			return out, e
		}
		return out, tx.Commit()
	case "policy_get":
		rows, e := s.DB.Query(`SELECT name FROM required_checks WHERE repo=? ORDER BY name`, repo)
		if e != nil {
			return out, e
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if e = rows.Scan(&n); e != nil {
				return out, e
			}
			out.Names = append(out.Names, n)
		}
		return out, rows.Err()
	case "check_report":
		if !model.ValidID(q.Candidate) || !model.ValidRef(q.Name) || (q.Status != "success" && q.Status != "failure" && q.Status != "pending") {
			return out, errors.New("check requires a candidate ID, name, and pending, success, or failure status")
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return out, e
		}
		defer tx.Rollback()
		var head, state string
		if e = tx.QueryRow(`SELECT head,state FROM jobs WHERE repo=? AND id=?`, repo, q.ID).Scan(&head, &state); e != nil {
			return out, e
		}
		if head != q.Candidate || (state != "checking" && state != "queued" && state != "running") {
			return out, errors.New("job no longer awaits checks for this candidate")
		}
		if _, e = tx.Exec(`INSERT INTO checks VALUES (?,?,?,?,?) ON CONFLICT(job,candidate,name) DO UPDATE SET status=excluded.status,actor=excluded.actor`, q.ID, q.Candidate, q.Name, q.Status, actor); e != nil {
			return out, e
		}
		if _, e = tx.Exec(`UPDATE jobs SET state='queued' WHERE id=?`, q.ID); e != nil {
			return out, e
		}
		if e = tx.Commit(); e != nil {
			return out, e
		}
		select {
		case s.wake <- struct{}{}:
		default:
		}
		return out, nil
	case "check_list":
		var exists int
		if e := s.DB.QueryRow(`SELECT count(*) FROM jobs WHERE repo=? AND id=?`, repo, q.ID).Scan(&exists); e != nil {
			return out, e
		}
		if exists == 0 {
			return out, errors.New("job not found")
		}
		rows, e := s.DB.Query(`SELECT name,status,candidate FROM checks WHERE job=? ORDER BY name`, q.ID)
		if e != nil {
			return out, e
		}
		defer rows.Close()
		for rows.Next() {
			var c protocol.Check
			if e = rows.Scan(&c.Name, &c.Status, &c.Candidate); e != nil {
				return out, e
			}
			out.Checks = append(out.Checks, c)
		}
		return out, rows.Err()
	}
	return out, errors.New("unsupported check operation")
}
