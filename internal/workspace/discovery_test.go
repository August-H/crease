package workspace

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindRejectsUninitializedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, query, message string
	}{
		{"missing configuration", `DELETE FROM config; DELETE FROM branches`, "crease init"},
		{"missing branch", `DELETE FROM branches`, "current branch \"main\" is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "project")
			w, err := Init(root)
			check(t, err)
			_, err = w.DB.Exec(tc.query)
			check(t, err)
			if _, err = w.Head(); err == nil || errors.Is(err, sql.ErrNoRows) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("unexpected metadata error: %v", err)
			}
			w.Close()
			found, err := Find(root)
			if found != nil {
				found.Close()
				t.Fatal("discovered an uninitialized repository")
			}
			if err == nil || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), root) {
				t.Fatalf("unexpected discovery error: %v", err)
			}
			// Failed discovery must release its lock so the metadata remains
			// available for recovery instead of stranding a workspace lock.
			w, err = open(root)
			check(t, err)
			w.Close()
		})
	}
}
