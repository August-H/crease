package metadata

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open serializes database writers through one connection. Slow file and network
// operations must happen outside transactions. No database is shared over NFS.
func Open(file string, schema string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(file)}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	err = db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		_, err = db.Exec(`INSERT INTO schema_version VALUES (1)`)
		version = 1
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	if version != 1 {
		db.Close()
		return nil, fmt.Errorf("unsupported database version %d", version)
	}
	// Schema definitions are additive. Apply each definition once, rather
	// than running every CREATE statement on every short-lived CLI connection.
	sum := sha256.Sum256([]byte(schema))
	hash := hex.EncodeToString(sum[:])
	var installed string
	err = db.QueryRow(`SELECT hash FROM schema_definitions WHERE hash=?`, hash).Scan(&installed)
	if err != nil {
		if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_definitions(hash TEXT PRIMARY KEY)`); err != nil {
			db.Close()
			return nil, err
		}
		if _, err = db.Exec(schema); err != nil {
			db.Close()
			return nil, err
		}
		if _, err = db.Exec(`INSERT OR IGNORE INTO schema_definitions VALUES (?)`, hash); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = os.Chmod(file, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
