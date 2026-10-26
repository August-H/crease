package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/cli"
	keyidentity "crease/internal/identity"
	"golang.org/x/crypto/ssh"
)

func TestSharedKeyGrantsAndSavedIdentity(t *testing.T) {
	f := fixtureServer(t)
	project := filepath.Join(f.root, "owner-workspace")
	f.run(t, f.root, f.owner, "create", f.url, project)
	put(t, project, "file", "base\n")
	f.run(t, project, f.owner, "add", ".")
	f.run(t, project, f.owner, "commit", "-m", "base")
	f.run(t, project, f.owner, "push")
	t.Setenv("CREASE_HOME", filepath.Join(f.root, "teammate-settings"))
	t.Setenv("CREASE_KEY", "")
	command := func(dir, input string, owner bool, args ...string) (string, error) {
		t.Helper()
		t.Chdir(dir)
		opts := []string{"--known-hosts", f.known}
		if owner {
			opts = append(opts, "--key", f.owner.priv)
		}
		var out, stderr bytes.Buffer
		err := cli.Run(append(opts, args...), strings.NewReader(input), &out, &stderr, "test")
		return out.String() + stderr.String(), err
	}
	if _, err := command(f.root, "", false, "key", "init"); err != nil {
		t.Fatal(err)
	}
	public, err := command(f.root, "", false, "key", "show")
	must(t, err)
	pub, err := keyidentity.ParsePublic([]byte(public))
	must(t, err)
	fingerprint := ssh.FingerprintSHA256(pub)
	if output, err := command(project, "", true, "access", "grant", strings.TrimSpace(public), "--role", "write"); err != nil || !strings.Contains(output, "Granted write access to "+fingerprint) {
		t.Fatalf("quoted key grant: %v %s", err, output)
	}
	clone := filepath.Join(f.root, "teammate-clone")
	if _, err := command(f.root, "", false, "clone", f.url, clone); err != nil {
		t.Fatalf("saved identity was not used for clone: %v", err)
	}
	put(t, clone, "file", "teammate edit\n")
	if _, err := command(clone, "", false, "commit", "-m", "teammate"); err != nil {
		t.Fatal(err)
	}
	if _, err := command(clone, "", false, "push"); err != nil {
		t.Fatalf("saved identity was not used for push: %v", err)
	}
	if output, err := command(project, public, true, "access", "revoke", "-"); err != nil || !strings.Contains(output, "Revoked access for "+fingerprint) {
		t.Fatalf("piped revoke: %v %s", err, output)
	}
	if _, err := command(clone, "", false, "fetch"); err == nil {
		t.Fatal("revoked key still fetched")
	}
	if output, err := command(project, public, true, "access", "grant"); err != nil || !strings.Contains(output, "Paste the SSH public key") || !strings.Contains(output, "Granted read access") {
		t.Fatalf("interactive grant: %v %s", err, output)
	}
	if _, err := command(clone, "", false, "fetch"); err != nil {
		t.Fatal(err)
	}
	if _, err := command(clone, "", false, "push"); err == nil {
		t.Fatal("default read access permitted a push")
	}
	private, err := os.ReadFile(f.reader.priv)
	must(t, err)
	for _, input := range []string{string(private), public + public} {
		output, err := command(project, input, true, "access", "grant", "-", "--role", "admin")
		if err == nil || strings.Contains(output, "Granted") || strings.Contains(output, "PRIVATE KEY") {
			t.Fatalf("invalid key grant: %v %s", err, output)
		}
	}
	output, err := command(project, "", true, "access", "grant", string(private), "--role", "admin")
	if err == nil || strings.Contains(output+err.Error(), string(private)) || strings.Contains(output, "Granted") {
		t.Fatal("pasted private key was accepted or echoed")
	}
	output, err = command(clone, public, false, "access", "grant", "-", "--role", "admin")
	if err == nil || strings.Contains(output, "Granted") {
		t.Fatalf("unauthorized grant claimed success: %v %s", err, output)
	}
}
