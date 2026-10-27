package transport

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crease/internal/codec"
	"crease/internal/identity"
	"crease/internal/model"
	"crease/internal/protocol"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Remote struct{ Address, Repo, User string }
type Options struct {
	KeyFile, KnownHosts string
	TrustHost           bool
	In                  io.Reader
	Err                 io.Writer
}
type Client struct {
	client       *ssh.Client
	session      *ssh.Session
	in           io.WriteCloser
	out          io.Reader
	agent        net.Conn
	Repo         string
	conn         net.Conn
	Capabilities map[string]bool
	Stats        Traffic
}

// Traffic counts application records, excluding SSH framing and handshakes.
// A Client is used by one command at a time.
type Traffic struct{ Requests, SentRecords, ReceivedRecords, SentEncodedBytes, ReceivedEncodedBytes int64 }

func Parse(raw string) (Remote, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return Remote{}, e
	}
	if u.Scheme != "ssh" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return Remote{}, errors.New("remote must be ssh://host[:port]/repository")
	}
	repo := strings.TrimPrefix(u.Path, "/")
	if !model.ValidRef(repo) {
		return Remote{}, errors.New("invalid repository name")
	}
	port := u.Port()
	if port == "" {
		port = "2222"
	}
	user := "crease"
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return Remote{}, errors.New("passwords in remote URLs are unsupported")
		}
		user = u.User.Username()
	}
	return Remote{Address: net.JoinHostPort(u.Hostname(), port), Repo: repo, User: user}, nil
}

func Dial(raw string, opts Options) (*Client, error) {
	r, e := Parse(raw)
	if e != nil {
		return nil, e
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Err == nil {
		opts.Err = os.Stderr
	}
	root, e := identity.Directory()
	if e != nil {
		return nil, e
	}
	opts.KeyFile, e = identity.Configured(opts.KeyFile)
	if e != nil {
		return nil, e
	}
	if opts.KnownHosts == "" {
		opts.KnownHosts = os.Getenv("CREASE_KNOWN_HOSTS")
	}
	if opts.KnownHosts == "" {
		opts.KnownHosts = filepath.Join(root, "known_hosts")
	}
	if os.Getenv("CREASE_TRUST_HOST") == "1" {
		opts.TrustHost = true
	}
	var auth []ssh.AuthMethod
	var agentConn net.Conn
	if opts.KeyFile != "" {
		method, conn, err := selectedKeyAuth(opts.KeyFile)
		if err != nil {
			return nil, err
		}
		agentConn = conn
		auth = append(auth, method)
	} else {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			agentConn, _ = net.DialTimeout("unix", sock, 5*time.Second)
			if agentConn != nil {
				auth = append(auth, ssh.PublicKeysCallback(agent.NewClient(agentConn).Signers))
			}
		}
		paths, err := identity.Candidates()
		if err != nil {
			if agentConn != nil {
				agentConn.Close()
			}
			return nil, err
		}
		for _, path := range paths {
			if b, e := os.ReadFile(path); e == nil {
				if signer, e := ssh.ParsePrivateKey(b); e == nil {
					auth = append(auth, ssh.PublicKeys(signer))
				}
			}
		}
	}
	fail := func(err error) (*Client, error) {
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, err
	}
	if len(auth) == 0 {
		return fail(errors.New("no usable SSH key; run crease key init, crease key use FILE, or load a key into ssh-agent"))
	}
	if e = os.MkdirAll(filepath.Dir(opts.KnownHosts), 0700); e != nil {
		return fail(e)
	}
	f, e := os.OpenFile(opts.KnownHosts, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return fail(e)
	}
	f.Close()
	check, e := knownhosts.New(opts.KnownHosts)
	if e != nil {
		return fail(e)
	}
	hostCallback := func(host string, addr net.Addr, key ssh.PublicKey) error {
		e := check(host, addr, key)
		if e == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(e, &ke) || len(ke.Want) > 0 {
			return fmt.Errorf("server host key changed or is invalid: %w", e)
		}
		fmt.Fprintf(opts.Err, "Server %s has host key %s\n", host, ssh.FingerprintSHA256(key))
		if !opts.TrustHost {
			fmt.Fprint(opts.Err, "Verify this fingerprint with the server owner. Trust this host? [y/N] ")
			line, _ := bufio.NewReader(opts.In).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				return errors.New("server host key was not trusted")
			}
		}
		f, e := os.OpenFile(opts.KnownHosts, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		if _, e = fmt.Fprintln(f, knownhosts.Line([]string{host}, key)); e != nil {
			return e
		}
		return f.Sync()
	}
	conn, e := net.DialTimeout("tcp", r.Address, 10*time.Second)
	if e != nil {
		return fail(e)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	sc, chans, reqs, e := ssh.NewClientConn(conn, r.Address, &ssh.ClientConfig{User: r.User, Auth: auth, HostKeyCallback: hostCallback, Timeout: 10 * time.Second})
	if e != nil {
		conn.Close()
		return fail(e)
	}
	client := ssh.NewClient(sc, chans, reqs)
	session, e := client.NewSession()
	if e != nil {
		client.Close()
		return fail(e)
	}
	in, e := session.StdinPipe()
	if e != nil {
		session.Close()
		client.Close()
		return fail(e)
	}
	out, e := session.StdoutPipe()
	if e != nil {
		session.Close()
		client.Close()
		return fail(e)
	}
	session.Stderr = opts.Err
	if e = session.Start("crease-rpc-v1"); e != nil {
		session.Close()
		client.Close()
		return fail(e)
	}
	c := &Client{client: client, session: session, in: in, out: out, agent: agentConn, Repo: r.Repo, conn: conn, Capabilities: map[string]bool{}}
	// Legacy servers return a normal unsupported-operation error and remain usable.
	if res, e := c.Call(protocol.Request{Op: "capabilities"}); e == nil {
		for _, cap := range res.Capabilities {
			c.Capabilities[cap] = true
		}
	}
	return c, nil
}
func (c *Client) Receive(q protocol.Request, accept func([]byte) error) (protocol.Response, error) {
	return c.ReceiveStart(q, nil, accept)
}

// ReceiveStart exposes the opening stream response before consuming records.
// Clone uses it to bind metadata and current contents to one pinned head.
func (c *Client) ReceiveStart(q protocol.Request, start func(protocol.Response) error, accept func([]byte) error) (protocol.Response, error) {
	res, e := c.Call(q)
	if e != nil {
		return res, e
	}
	if !res.Stream {
		return res, errors.New("server did not start a binary stream")
	}
	if start != nil {
		if e = start(res); e != nil {
			c.Close()
			return res, e
		}
	}
	for {
		c.conn.SetDeadline(time.Now().Add(2 * time.Minute))
		b, e := codec.Read(c.out)
		if e == codec.ErrEnd {
			break
		}
		if e != nil {
			return res, e
		}
		c.Stats.ReceivedRecords++
		c.Stats.ReceivedEncodedBytes += int64(len(b) + 4)
		if e = accept(b); e != nil {
			c.Close()
			return res, e
		}
	}
	res = protocol.Response{}
	if e = protocol.Read(c.out, &res); e != nil {
		return res, e
	}
	if res.Version != protocol.Version {
		return res, errors.New("unsupported stream response version")
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}
func (c *Client) Send(q protocol.Request, next func() ([]byte, error)) (protocol.Response, error) {
	res, e := c.Call(q)
	if e != nil {
		return res, e
	}
	if !res.Stream {
		return res, errors.New("server did not accept a binary stream")
	}
	for {
		b, e := next()
		if e == io.EOF {
			break
		}
		if e != nil {
			c.Close()
			return res, e
		}
		c.conn.SetDeadline(time.Now().Add(2 * time.Minute))
		c.Stats.SentRecords++
		c.Stats.SentEncodedBytes += int64(len(b) + 4)
		if e = codec.Write(c.in, b); e != nil {
			return res, e
		}
	}
	if e = codec.End(c.in); e != nil {
		return res, e
	}
	res = protocol.Response{}
	if e = protocol.Read(c.out, &res); e != nil {
		return res, e
	}
	if res.Version != protocol.Version {
		return res, errors.New("unsupported stream response version")
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}
func (c *Client) Close() {
	c.session.Close()
	c.client.Close()
	_ = c.session.Wait()
	if c.agent != nil {
		c.agent.Close()
	}
}
func (c *Client) Call(req protocol.Request) (protocol.Response, error) {
	c.Stats.Requests++
	req.Version = protocol.Version
	req.Repo = c.Repo
	c.conn.SetDeadline(time.Now().Add(2 * time.Minute))
	var res protocol.Response
	if e := protocol.Write(c.in, req); e != nil {
		return res, e
	}
	if e := protocol.Read(c.out, &res); e != nil {
		return res, e
	}
	if res.Version != protocol.Version {
		return res, errors.New("unsupported server protocol version")
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}
