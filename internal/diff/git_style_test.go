package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/objectstore"
)

func TestGitPatchMetadataAndApplication(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable for independent patch validation")
	}
	for _, tc := range []struct {
		name, path, before, after, oldMode, newMode, header string
	}{
		{"added", "new file", "", "new\n", "", "file", "new file mode 100644"},
		{"deleted", "old file", "old\n", "", "file", "", "deleted file mode 100644"},
		{"executable", "program", "same\n", "same\n", "file", "exec", "old mode 100644\nnew mode 100755"},
		{"empty added", "empty", "", "", "", "file", "new file mode 100644"},
		{"empty deleted", "empty", "", "", "file", "", "deleted file mode 100644"},
		{"unicode", "café.go", "old\n", "new\n", "file", "file", `"a/caf\303\251.go"`},
		{"quote", `a"b`, "old\n", "new\n", "file", "file", `"a/a\"b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := objectstore.Snapshot{}, objectstore.Snapshot{}
			oldID, newID := strings.Repeat("a", 64), strings.Repeat("b", 64)
			if tc.before == tc.after {
				newID = oldID
			}
			if tc.oldMode != "" {
				left[tc.path] = objectstore.File{ID: oldID, Mode: tc.oldMode}
			}
			if tc.newMode != "" {
				right[tc.path] = objectstore.File{ID: newID, Mode: tc.newMode}
			}
			text, err := UnifiedWith(left, right, func(_ string, _ objectstore.File, after bool) ([]byte, error) {
				if after {
					return []byte(tc.after), nil
				}
				return []byte(tc.before), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(text, "diff --git ") || !strings.Contains(text, tc.header) {
				t.Fatalf("bad patch:\n%s", text)
			}
			root := t.TempDir()
			if tc.oldMode != "" {
				if err := os.WriteFile(filepath.Join(root, tc.path), []byte(tc.before), 0644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(git, "apply", "-")
			cmd.Dir = root
			cmd.Stdin = strings.NewReader(text)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git apply rejected patch: %v\n%s\n%s", err, output, text)
			}
			if tc.newMode == "" {
				if _, err := os.Stat(filepath.Join(root, tc.path)); !os.IsNotExist(err) {
					t.Fatal("deleted file remains")
				}
				return
			}
			data, err := os.ReadFile(filepath.Join(root, tc.path))
			if err != nil || string(data) != tc.after {
				t.Fatalf("wrong patched contents: %q %v", data, err)
			}
			st, err := os.Stat(filepath.Join(root, tc.path))
			if err != nil {
				t.Fatal(err)
			}
			if (st.Mode()&0111 != 0) != (tc.newMode == "exec") {
				t.Fatalf("wrong patched mode: %v", st.Mode())
			}
		})
	}
}

func TestDiffStatsAndBinaryFormatting(t *testing.T) {
	left := objectstore.Snapshot{"text": {ID: "a", Mode: "file"}, "binary": {ID: "c", Mode: "file"}}
	right := objectstore.Snapshot{"text": {ID: "b", Mode: "file"}, "binary": {ID: "d", Mode: "file"}}
	read := func(p string, _ objectstore.File, after bool) ([]byte, error) {
		if p == "binary" {
			if after {
				return []byte("\x00new"), nil
			}
			return []byte("\x00old"), nil
		}
		if after {
			return []byte("same\nnew\nextra\n"), nil
		}
		return []byte("same\nold\n"), nil
	}
	stats, err := StatsWith(left, right, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 || !stats[0].Binary || stats[1].Insertions != 2 || stats[1].Deletions != 1 {
		t.Fatalf("incorrect stats: %+v", stats)
	}
	text, err := UnifiedWith(left, right, read)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Binary files a/binary and b/binary differ") {
		t.Fatalf("binary patch: %s", text)
	}
}
