package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"crease/internal/identity"
	"golang.org/x/crypto/ssh"
)

func (a *app) key(args []string) error {
	op := "show"
	if len(args) > 0 {
		op, args = args[0], args[1:]
	}
	switch op {
	case "init", "generate":
		f := a.flags("key init")
		file := f.String("file", "", "private-key file; defaults to Crease's identity directory")
		comment := f.String("comment", "Crease identity", "public-key comment")
		if err := parse(f, args); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("usage: crease key init [--file PRIVATE_KEY_FILE] [--comment TEXT]")
		}
		path := *file
		if path == "" {
			var err error
			path, err = identity.DefaultPath()
			if err != nil {
				return err
			}
		}
		path, err := identity.Path(path)
		if err != nil {
			return err
		}
		pub, err := identity.Generate(path, *comment)
		if err != nil {
			return err
		}
		if err = identity.Use(path); err != nil {
			return fmt.Errorf("key created at %s but could not save the default: %w", path, err)
		}
		fmt.Fprintf(a.out, "Created an unencrypted Ed25519 identity at %s\nPublic key: %s.pub\nFingerprint: %s\nRun crease key share to share your public key.\n", path, path, ssh.FingerprintSHA256(pub))
		return nil
	case "use":
		if len(args) != 1 {
			return errors.New("usage: crease key use PRIVATE_KEY_FILE")
		}
		if err := identity.Use(args[0]); err != nil {
			return err
		}
		fmt.Fprintln(a.out, "Default identity saved. Run crease key share to share its public key.")
		return nil
	case "show", "share", "copy", "fingerprint":
		if len(args) != 0 {
			return fmt.Errorf("usage: crease key %s; select a private key with crease --key FILE key %s", op, op)
		}
		path, err := identity.Resolve(a.opts.KeyFile)
		if err != nil {
			return err
		}
		pub, err := identity.Public(path)
		if err != nil {
			return err
		}
		public := ssh.MarshalAuthorizedKey(pub)
		fingerprint := ssh.FingerprintSHA256(pub)
		switch op {
		case "show":
			_, err = a.out.Write(public)
			return err
		case "fingerprint":
			fmt.Fprintln(a.out, fingerprint)
			return nil
		case "copy":
			if _, err = clipboard(false, public); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Copied public key to clipboard.\nFingerprint: %s\n", fingerprint)
		case "share":
			if _, err = a.out.Write(public); err != nil {
				return err
			}
			fmt.Fprintln(a.err, "Fingerprint:", fingerprint)
			if _, err = clipboard(false, public); err != nil {
				fmt.Fprintln(a.err, "Clipboard unavailable here; copy the public key printed above.")
			} else {
				fmt.Fprintln(a.err, "Public key copied to clipboard. Send it to the repository owner.")
			}
		}
		return nil
	default:
		return errors.New("usage: crease key [show|share|copy|fingerprint|init|use]")
	}
}

// accessKey accepts a public-key file, a quoted public key, stdin, an interactive
// paste, or the system clipboard. All paths share validation before any RPC.
func (a *app) accessKey(args []string, fromClipboard bool) (ssh.PublicKey, error) {
	if len(args) > 1 || fromClipboard && len(args) != 0 {
		return nil, errors.New("provide one public key, a public-key file, - for stdin, or --clipboard")
	}
	var raw []byte
	var err error
	switch {
	case fromClipboard:
		raw, err = clipboard(true, nil)
	case len(args) == 0:
		fmt.Fprint(a.err, "Paste the SSH public key, then press Enter: ")
		line, readErr := bufio.NewReader(io.LimitReader(a.in, identity.MaxPublicKey+1)).ReadString('\n')
		raw = []byte(line)
		if readErr != nil && readErr != io.EOF {
			err = readErr
		}
	case args[0] == "-":
		raw, err = io.ReadAll(io.LimitReader(a.in, identity.MaxPublicKey+1))
	default:
		value := strings.TrimSpace(args[0])
		if strings.HasPrefix(value, "ssh-") || strings.HasPrefix(value, "ecdsa-") || strings.HasPrefix(value, "sk-") || strings.Contains(value, "PRIVATE KEY") {
			raw = []byte(value)
		} else {
			file, e := os.Open(args[0])
			if e != nil {
				return nil, e
			}
			raw, err = io.ReadAll(io.LimitReader(file, identity.MaxPublicKey+1))
			file.Close()
		}
	}
	if err != nil {
		return nil, err
	}
	return identity.ParsePublic(raw)
}

func clipboardCommands(read bool) [][]string {
	switch runtime.GOOS {
	case "darwin":
		if read {
			return [][]string{{"pbpaste"}}
		}
		return [][]string{{"pbcopy"}}
	case "windows":
		if read {
			return [][]string{{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-Clipboard -Raw"}}
		}
		return [][]string{{"clip.exe"}}
	default:
		if read {
			return [][]string{{"wl-paste", "--no-newline"}, {"xclip", "-selection", "clipboard", "-o"}, {"xsel", "--clipboard", "--output"}}
		}
		return [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
	}
}

type clipboardBuffer struct {
	raw      []byte
	overflow bool
}

func (b *clipboardBuffer) Write(p []byte) (int, error) {
	n := len(p)
	space := identity.MaxPublicKey + 1 - len(b.raw)
	if n > space {
		b.overflow = true
		p = p[:space]
	}
	b.raw = append(b.raw, p...)
	return n, nil
}

func clipboard(read bool, public []byte) ([]byte, error) {
	for _, args := range clipboardCommands(read) {
		path, err := exec.LookPath(args[0])
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, path, args[1:]...)
		cmd.Stdin = strings.NewReader(string(public))
		cmd.Stderr = io.Discard
		var output clipboardBuffer
		if read {
			cmd.Stdout = &output
		} else {
			cmd.Stdout = io.Discard
		}
		err = cmd.Run()
		cancel()
		if err != nil {
			continue
		}
		if output.overflow || len(output.raw) > identity.MaxPublicKey {
			return nil, errors.New("clipboard does not contain a single SSH public key")
		}
		return output.raw, nil
	}
	if read {
		return nil, errors.New("clipboard unavailable; use crease access grant and paste the public key, or provide a .pub file")
	}
	return nil, errors.New("clipboard unavailable; use crease key show to print your public key")
}
