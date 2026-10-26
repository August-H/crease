package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestSelectedEncryptedKeyUsesOnlyMatchingAgentIdentity(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "test", []byte("test passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "encrypted")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, _, err := selectedKeyAuth(path); err == nil || !strings.Contains(err.Error(), "ssh-add") {
		t.Fatal("encrypted key without agent did not explain unlocking")
	}
	// Keep the Unix socket path short enough for macOS.
	root, err := os.MkdirTemp("", "cr-agent-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "agent")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	keyring := agent.NewKeyring()
	_, unrelated, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: unrelated}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _ = agent.ServeAgent(keyring, conn) }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", socket)
	if _, _, err := selectedKeyAuth(path); err == nil {
		t.Fatal("unrelated agent key was accepted for the selected identity")
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	signer, conn, err := selectedKeySigner(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if signer == nil || conn == nil {
		t.Fatal("matching encrypted identity unavailable")
	}
	// Exercise an actual signature request, beyond merely listing the key.
	pub, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	signature, err := signer.Sign(rand.Reader, []byte("crease identity test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Verify([]byte("crease identity test"), signature); err != nil {
		t.Fatal(err)
	}
}
