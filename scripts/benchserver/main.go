// Command benchserver runs disposable loopback SSH endpoints for the stress suite.
// The Git endpoint is a benchmark adapter, not a deployable hosting service.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"crease/internal/server"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: benchserver EXISTING_DISPOSABLE_DIRECTORY")
	}
	root := os.Args[1]
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(private, "disposable benchmark identity")
	if err != nil {
		return err
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"identity": pem.EncodeToMemory(block), "identity.pub": ssh.MarshalAuthorizedKey(key),
	} {
		f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	}
	s, err := server.Open(filepath.Join(root, "crease-server"), filepath.Join(root, "identity.pub"))
	if err != nil {
		return err
	}
	defer s.Close()
	crease, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer crease.Close()
	git, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer git.Close()
	hosts := knownhosts.Line([]string{crease.Addr().String()}, s.Signer.PublicKey()) + "\n" +
		knownhosts.Line([]string{git.Addr().String()}, s.Signer.PublicKey()) + "\n"
	if err := os.WriteFile(filepath.Join(root, "known_hosts"), []byte(hosts), 0600); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- s.Serve(ctx, crease) }()
	go func() { done <- serveGit(ctx, git, s.Signer, key, filepath.Join(root, "git-remote.git")) }()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"crease_url": "ssh://crease@" + crease.Addr().String() + "/benchmark",
		"git_url":    "ssh://git@" + git.Addr().String() + "/benchmark.git",
	}); err != nil {
		cancel()
		<-done
		<-done
		return err
	}
	err = <-done
	cancel()
	return errors.Join(err, <-done)
}

func serveGit(ctx context.Context, listener net.Listener, host ssh.Signer, identity ssh.PublicKey, repo string) error {
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), identity.Marshal()) {
			return nil, errors.New("unknown benchmark identity")
		}
		return &ssh.Permissions{}, nil
	}}
	config.AddHostKey(host)
	var wg sync.WaitGroup
	go func() { <-ctx.Done(); listener.Close() }()
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			conn.SetDeadline(time.Now().Add(3 * time.Minute))
			sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
			if err != nil {
				return
			}
			defer sshConn.Close()
			go ssh.DiscardRequests(requests)
			for incoming := range channels {
				if incoming.ChannelType() != "session" {
					incoming.Reject(ssh.UnknownChannelType, "sessions only")
					continue
				}
				channel, requests, err := incoming.Accept()
				if err != nil {
					continue
				}
				gitSession(ctx, channel, requests, repo)
			}
		}()
	}
}

func gitSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, repo string) {
	defer channel.Close()
	protocol := ""
	for request := range requests {
		if request.Type == "env" {
			var value struct{ Name, Value string }
			ok := ssh.Unmarshal(request.Payload, &value) == nil && value.Name == "GIT_PROTOCOL" && len(value.Value) < 256
			if ok {
				protocol = value.Value
			}
			request.Reply(ok, nil)
			continue
		}
		var value struct{ Command string }
		if request.Type != "exec" || ssh.Unmarshal(request.Payload, &value) != nil {
			request.Reply(false, nil)
			continue
		}
		// Exact allowlist: never execute a shell or use client-supplied paths.
		command := ""
		for _, name := range []string{"git-upload-pack", "git-receive-pack"} {
			if value.Command == name+" '/benchmark.git'" {
				command = name
			}
		}
		if command == "" {
			request.Reply(false, nil)
			return
		}
		cmd := exec.CommandContext(ctx, command, repo)
		cmd.Env = append(os.Environ(), "GIT_PROTOCOL="+protocol)
		cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
		input, err := cmd.StdinPipe()
		if err != nil || cmd.Start() != nil {
			if input != nil {
				input.Close()
			}
			request.Reply(false, nil)
			return
		}
		request.Reply(true, nil)
		go func() { io.Copy(input, channel); input.Close() }()
		err = cmd.Wait()
		status := uint32(0)
		if err != nil {
			status = 1
		}
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}
