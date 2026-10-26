// Package identity manages local SSH identities and their shareable public keys.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crease/internal/objectstore"
	"golang.org/x/crypto/ssh"
)

const MaxPublicKey = 16 << 10

func Directory() (string, error) {
	if root := os.Getenv("CREASE_HOME"); root != "" {
		return filepath.Abs(root)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".crease"), nil
}

func Path(path string) (string, error) {
	if strings.Contains(path, "PRIVATE KEY") {
		return "", errors.New("provide a private-key file path, not the private key contents")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

func DefaultPath() (string, error) {
	root, err := Directory()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "id_ed25519"), nil
}

// Configured returns a deliberately selected identity, if there is one.
// Explicit flags take precedence over the environment and saved preference.
func Configured(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("CREASE_KEY")
	}
	if explicit != "" {
		return Path(explicit)
	}
	root, err := Directory()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(root, "identity.json"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		Key string `json:"key"`
	}
	if len(raw) > 16<<10 || json.Unmarshal(raw, &config) != nil || !filepath.IsAbs(config.Key) {
		return "", errors.New("invalid saved identity; use crease key use PRIVATE_KEY_FILE")
	}
	return Path(config.Key)
}

func Candidates() ([]string, error) {
	managed, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return []string{managed, filepath.Join(home, ".ssh", "crease"), filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_ecdsa"), filepath.Join(home, ".ssh", "id_rsa")}, nil
}

func Resolve(explicit string) (string, error) {
	path, err := Configured(explicit)
	if err != nil || path != "" {
		return path, err
	}
	paths, err := Candidates()
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", errors.New("no SSH identity found; run crease key init or crease key use PRIVATE_KEY_FILE")
}

func ParsePublic(raw []byte) (ssh.PublicKey, error) {
	if len(raw) > MaxPublicKey || strings.Contains(string(raw), "PRIVATE KEY") {
		return nil, errors.New("expected one SSH public key, not a private key or multiple keys")
	}
	pub, _, options, rest, err := ssh.ParseAuthorizedKey(raw)
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("expected one SSH public key without authorized_keys options")
	}
	return pub, nil
}

// Public derives the public key from the selected private key. It never returns
// private bytes and cannot be confused by a stale .pub sidecar file.
func Public(path string) (ssh.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err == nil {
		return signer.PublicKey(), nil
	}
	var encrypted *ssh.PassphraseMissingError
	if errors.As(err, &encrypted) {
		if encrypted.PublicKey != nil {
			return encrypted.PublicKey, nil
		}
		sidecar, readErr := os.ReadFile(path + ".pub")
		if readErr != nil {
			return nil, errors.New("encrypted identity needs its .pub file; export it with ssh-keygen -y")
		}
		return ParsePublic(sidecar)
	}
	return nil, errors.New("not a supported SSH private key; use crease key use PRIVATE_KEY_FILE")
}

func Use(path string) error {
	abs, err := Path(path)
	if err != nil {
		return err
	}
	if _, err = Public(abs); err != nil {
		return err
	}
	root, err := Directory()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Key string `json:"key"`
	}{abs})
	if err != nil {
		return err
	}
	return objectstore.Atomic(filepath.Join(root, "identity.json"), append(raw, '\n'), 0600)
}

func Generate(path, comment string) (ssh.PublicKey, error) {
	if strings.ContainsAny(comment, "\r\n") {
		return nil, errors.New("key comment must be a single line")
	}
	if strings.HasSuffix(path, ".pub") {
		return nil, errors.New("choose a private-key filename without the .pub suffix")
	}
	for _, file := range []string{path, path + ".pub"} {
		if _, err := os.Lstat(file); !os.IsNotExist(err) {
			return nil, fmt.Errorf("key path already exists or cannot be accessed: %s", file)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return nil, err
	}
	pub, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		return nil, err
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		public += " " + comment
	}
	if err = exclusive(path, pem.EncodeToMemory(block), 0600); err != nil {
		return nil, err
	}
	if err = exclusive(path+".pub", []byte(public+"\n"), 0644); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	if err = objectstore.SyncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return pub, nil
}

func exclusive(path string, raw []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
