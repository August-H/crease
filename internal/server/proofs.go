package server

import (
	"crease/internal/objectstore"
	"database/sql"
)

func (s *Server) proof(repo, id string) (objectstore.Proof, bool, error) {
	var p objectstore.Proof
	e := s.DB.QueryRow(`SELECT kind,height FROM graph_proofs WHERE repo=? AND id=?`, repo, id).Scan(&p.Kind, &p.Height)
	if e == sql.ErrNoRows {
		return p, false, nil
	}
	return p, e == nil, e
}
func (s *Server) validate(repo, head string) (map[string]objectstore.Proof, error) {
	return s.Store(repo).Validate(head, func(id string) (objectstore.Proof, bool, error) { return s.proof(repo, id) })
}
func persistProofs(tx *sql.Tx, repo string, proofs map[string]objectstore.Proof) error {
	if len(proofs) == 0 {
		return nil
	}
	stmt, e := tx.Prepare(`INSERT OR IGNORE INTO graph_proofs VALUES (?,?,?,?)`)
	if e != nil {
		return e
	}
	defer stmt.Close()
	for id, p := range proofs {
		if _, e = stmt.Exec(repo, id, p.Kind, p.Height); e != nil {
			return e
		}
	}
	return nil
}
