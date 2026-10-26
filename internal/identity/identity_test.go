package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerationSelectionAndPublicExport(t *testing.T) {
	t.Setenv("CREASE_HOME", t.TempDir())
	t.Setenv("CREASE_KEY", "")
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := Generate(path, "teammate")
	if err != nil {
		t.Fatal(err)
	}
	private, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions: %v", st.Mode())
	}
	if _, err = Generate(path, "again"); err == nil {
		t.Fatal("existing identity overwritten")
	}
	stillPrivate, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(private, stillPrivate) {
		t.Fatal("existing private key changed")
	}
	if err := Use(path); err != nil {
		t.Fatal(err)
	}
	selected, err := Resolve("")
	if err != nil || selected != path {
		t.Fatalf("default identity: %s %v", selected, err)
	}
	actual, err := Public(path)
	if err != nil || !bytes.Equal(pub.Marshal(), actual.Marshal()) {
		t.Fatal("public export differs from private identity")
	}
	// Export is derived from the private identity, not an unrelated sidecar.
	if err := os.WriteFile(path+".pub", []byte("stale public key\n"), 0644); err != nil {
		t.Fatal(err)
	}
	actual, err = Public(path)
	if err != nil || !bytes.Equal(pub.Marshal(), actual.Marshal()) {
		t.Fatal("stale sidecar changed shared identity")
	}
	other := filepath.Join(t.TempDir(), "other")
	if _, err := Generate(other, "other"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREASE_KEY", other)
	selected, err = Resolve("")
	if err != nil || selected != other {
		t.Fatal("environment did not override saved preference")
	}
	selected, err = Resolve(path)
	if err != nil || selected != path {
		t.Fatal("explicit identity did not override environment")
	}
}

func TestEncryptedPublicExport(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "encrypted test key", []byte("test passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "encrypted")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	pub, err := Public(path)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub.Marshal(), expected.Marshal()) {
		t.Fatal("wrong encrypted identity public key")
	}
}

func TestPublicValidationAndNoClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	pub, err := Generate(path, "")
	if err != nil {
		t.Fatal(err)
	}
	public := ssh.MarshalAuthorizedKey(pub)
	private, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{private, append(append([]byte{}, public...), public...), []byte("command=\"false\" " + string(public)), []byte(strings.Repeat("x", MaxPublicKey+1)), []byte("not a key")} {
		if _, err := ParsePublic(raw); err == nil {
			t.Fatal("unsafe or malformed public key accepted")
		}
	}
	if _, err := ParsePublic(public); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.pub"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(filepath.Join(root, "existing"), ""); err == nil {
		t.Fatal("existing public key clobbered")
	}
	if _, err := os.Stat(filepath.Join(root, "existing")); !os.IsNotExist(err) {
		t.Fatal("private key created despite existing sidecar")
	}
	if err := os.Symlink(path, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(filepath.Join(root, "symlink"), ""); err == nil {
		t.Fatal("existing symlink followed")
	}
}

func TestMalformedSavedIdentityFailsClosed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CREASE_HOME", root)
	t.Setenv("CREASE_KEY", "")
	if err := os.WriteFile(filepath.Join(root, "identity.json"), []byte(`{"key":"relative/path"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Configured(""); err == nil {
		t.Fatal("invalid saved identity silently ignored")
	}
}
