package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crease/internal/identity"
	"golang.org/x/crypto/ssh"
)

func mockClipboard(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	data := filepath.Join(t.TempDir(), "clipboard")
	t.Setenv("PATH", bin)
	t.Setenv("CREASE_TEST_CLIPBOARD", data)
	write := clipboardCommands(false)[0][0]
	read := clipboardCommands(true)[0][0]
	if err := os.WriteFile(filepath.Join(bin, write), []byte("#!/bin/sh\n/bin/cat > \"$CREASE_TEST_CLIPBOARD\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, read), []byte("#!/bin/sh\n/bin/cat \"$CREASE_TEST_CLIPBOARD\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPublicKeySharingAndClipboardValidation(t *testing.T) {
	t.Setenv("CREASE_HOME", t.TempDir())
	t.Setenv("CREASE_KEY", "")
	clip := mockClipboard(t)
	var out, stderr bytes.Buffer
	a := &app{out: &out, err: &stderr, in: strings.NewReader("")}
	if err := a.key([]string{"init"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Created an unencrypted Ed25519 identity") {
		t.Fatalf("generation response: %s", out.String())
	}
	path, err := identity.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := identity.Public(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := ssh.MarshalAuthorizedKey(pub)
	for _, op := range []string{"show", "copy", "share", "fingerprint"} {
		out.Reset()
		stderr.Reset()
		if err := a.key([]string{op}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String()+stderr.String(), "PRIVATE KEY") {
			t.Fatal("private material printed")
		}
		if op == "show" || op == "share" {
			if !bytes.Equal(out.Bytes(), expected) {
				t.Fatalf("public output contains extra text: %q", out.String())
			}
		}
		if op == "copy" || op == "share" {
			actual, err := os.ReadFile(clip)
			if err != nil || !bytes.Equal(actual, expected) {
				t.Fatal("clipboard did not contain only the public key")
			}
		}
	}
	parsed, err := a.accessKey(nil, true)
	if err != nil || !bytes.Equal(parsed.Marshal(), pub.Marshal()) {
		t.Fatalf("clipboard import: %v", err)
	}
	private, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{private, append(append([]byte{}, expected...), expected...), bytes.Repeat([]byte("x"), identity.MaxPublicKey+2)} {
		if err := os.WriteFile(clip, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.accessKey(nil, true); err == nil {
			t.Fatal("invalid clipboard key accepted")
		}
	}
}

func TestHeadlessSharingAndPublicKeyInputs(t *testing.T) {
	t.Setenv("CREASE_HOME", t.TempDir())
	t.Setenv("CREASE_KEY", "")
	t.Setenv("PATH", t.TempDir())
	path, err := identity.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := identity.Generate(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Use(path); err != nil {
		t.Fatal(err)
	}
	public := string(ssh.MarshalAuthorizedKey(pub))
	var out, stderr bytes.Buffer
	a := &app{out: &out, err: &stderr, in: strings.NewReader(public)}
	if err := a.key([]string{"share"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != public || !strings.Contains(stderr.String(), "Clipboard unavailable") {
		t.Fatal("headless sharing did not print a usable public key")
	}
	if err := a.key([]string{"copy"}); err == nil {
		t.Fatal("copy claimed success without a clipboard")
	}
	for _, input := range [][]string{{public}, {path + ".pub"}, {"-"}, nil} {
		a.in = strings.NewReader(public)
		actual, err := a.accessKey(input, false)
		if err != nil || !bytes.Equal(actual.Marshal(), pub.Marshal()) {
			t.Fatalf("key import %v: %v", input, err)
		}
	}
	a.in = strings.NewReader(public + public)
	if _, err := a.accessKey([]string{"-"}, false); err == nil {
		t.Fatal("multiple piped keys accepted")
	}
	if _, err := a.accessKey([]string{public}, true); err == nil {
		t.Fatal("conflicting key sources accepted")
	}
}
