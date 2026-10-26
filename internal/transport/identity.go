package transport

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"crease/internal/identity"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// selectedKeyAuth uses exactly the selected identity. Encrypted private keys
// can be signed by ssh-agent without exposing or asking for their passphrase.
func selectedKeyAuth(path string) (ssh.AuthMethod, net.Conn, error) {
	signer, conn, err := selectedKeySigner(path)
	if err != nil {
		return nil, nil, err
	}
	return ssh.PublicKeys(signer), conn, nil
}

func selectedKeySigner(path string) (ssh.Signer, net.Conn, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err == nil {
		return signer, nil, nil
	}
	var encrypted *ssh.PassphraseMissingError
	if !errors.As(err, &encrypted) {
		return nil, nil, errors.New("invalid selected private SSH key")
	}
	pub, err := identity.Public(path)
	if err != nil {
		return nil, nil, err
	}
	sock := os.Getenv("SSH_AUTH_SOCK")
	locked := func() error { return fmt.Errorf("selected identity is encrypted; load it with ssh-add %q", path) }
	if sock == "" {
		return nil, nil, locked()
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, nil, locked()
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		conn.Close()
		return nil, nil, locked()
	}
	for _, signer := range signers {
		if bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
			return signer, conn, nil
		}
	}
	conn.Close()
	return nil, nil, locked()
}
