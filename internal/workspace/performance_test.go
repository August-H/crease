package workspace

import "testing"

func TestCommitDoesNotRewriteUnchangedTracking(t *testing.T) {
	w := initWorkspace(t)
	for _, p := range []string{"a", "b", "c"} {
		write(t, w, p, "first")
	}
	check(t, w.Add([]string{"."}, w.Root))
	commit(t, w, "first")
	write(t, w, "a", "second")
	var before, after int
	check(t, w.DB.QueryRow(`SELECT total_changes()`).Scan(&before))
	commit(t, w, "second")
	check(t, w.DB.QueryRow(`SELECT total_changes()`).Scan(&after))
	if after-before != 2 {
		t.Fatalf("single-file commit wrote %d rows; expected ref and movement only", after-before)
	}
}
