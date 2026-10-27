package cli_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/cli"
	"crease/internal/syncer"
)

func TestCloneTimingsAreOptInAndSeparateFromOutput(t *testing.T) {
	f := fixtureServer(t)
	source := filepath.Join(f.root, "source")
	f.run(t, f.root, f.owner, "create", f.url, source)
	put(t, source, "file", "clone fixture\n")
	f.run(t, source, f.owner, "add", ".")
	f.run(t, source, f.owner, "commit", "-m", "fixture")
	f.run(t, source, f.owner, "push")
	t.Chdir(f.root)
	for _, timed := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		dir := filepath.Join(f.root, "ordinary")
		args := []string{"--key", f.owner.priv, "--known-hosts", f.known, "clone", "--quiet", f.url}
		if timed {
			dir = filepath.Join(f.root, "timed")
			args = append(args, "--timings")
		}
		args = append(args, dir)
		must(t, cli.Run(args, strings.NewReader(""), &stdout, &stderr, "test"))
		if stdout.Len() != 0 {
			t.Fatalf("quiet clone wrote to stdout: %q", stdout.String())
		}
		if !timed {
			if stderr.Len() != 0 {
				t.Fatalf("ordinary clone emitted profiling output: %q", stderr.String())
			}
			continue
		}
		var timings syncer.CloneTimings
		must(t, json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(stderr.String()), "clone timings: ")), &timings))
		if timings.Mode != "clone-stream-v1" || timings.Requests != 1 || timings.TotalMS <= 0 || timings.Workers != 1 {
			t.Fatalf("invalid clone timing data: %+v", timings)
		}
	}
}
