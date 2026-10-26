package syncer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"crease/internal/protocol"
	"crease/internal/server"
	"crease/internal/syncer"
	"crease/internal/transport"
	"crease/internal/workspace"
	"golang.org/x/crypto/ssh"
)

type fixture struct {
	root, url string
	options   transport.Options
}

func check(t testing.TB, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func setup(t testing.TB) fixture {
	t.Helper()
	root := t.TempDir()
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	check(t, e)
	block, e := ssh.MarshalPrivateKey(priv, "")
	check(t, e)
	key, e := ssh.NewPublicKey(pub)
	check(t, e)
	private, public := filepath.Join(root, "key"), filepath.Join(root, "key.pub")
	check(t, os.WriteFile(private, pem.EncodeToMemory(block), 0600))
	check(t, os.WriteFile(public, ssh.MarshalAuthorizedKey(key), 0600))
	s, e := server.Open(filepath.Join(root, "server"), public)
	check(t, e)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	check(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); check(t, <-done); s.Close() })
	f := fixture{root: root, url: "ssh://" + ln.Addr().String() + "/bench", options: transport.Options{KeyFile: private, KnownHosts: filepath.Join(root, "known_hosts"), TrustHost: true, Err: io.Discard}}
	c, e := transport.Dial(f.url, f.options)
	check(t, e)
	defer c.Close()
	_, e = c.Call(protocol.Request{Op: "create"})
	check(t, e)
	w, e := workspace.Init(filepath.Join(root, "source"))
	check(t, e)
	defer w.Close()
	check(t, w.SetConfig("remote", f.url))
	file := filepath.Join(w.Root, "file")
	check(t, os.WriteFile(file, []byte("initial"), 0644))
	check(t, w.Add([]string{"file"}, w.Root))
	for i := 0; i < 50; i++ {
		check(t, os.WriteFile(file, []byte(fmt.Sprintf("revision %d\n", i)), 0644))
		_, e = w.Commit(fmt.Sprintf("commit %d", i), "benchmark", nil)
		check(t, e)
	}
	check(t, syncer.Push(c, w))
	return f
}
func receive(t testing.TB, f fixture, legacy bool, name string) (transport.Traffic, string) {
	t.Helper()
	dir, e := os.MkdirTemp(f.root, name+"-")
	check(t, e)
	w, e := workspace.Init(dir)
	check(t, e)
	defer w.Close()
	check(t, w.SetConfig("remote", f.url))
	c, e := transport.Dial(f.url, f.options)
	check(t, e)
	defer c.Close()
	if legacy {
		c.Capabilities = map[string]bool{}
	}
	c.Stats = transport.Traffic{}
	head, e := syncer.Fetch(c, w, "main", false)
	check(t, e)
	return c.Stats, head
}

func TestStreamingAvoidsPerGenerationRequests(t *testing.T) {
	f := setup(t)
	fast, _ := receive(t, f, false, "fast")
	slow, _ := receive(t, f, true, "legacy")
	if fast.Requests > 3 {
		t.Fatalf("streaming used %d requests", fast.Requests)
	}
	if slow.Requests < 50 {
		t.Fatalf("legacy fixture did not exercise dependent requests: %d", slow.Requests)
	}
	t.Logf("50-commit fetch: binary %d RPCs, JSON %d RPCs", fast.Requests, slow.Requests)
}

func TestLargeFileDeltasWorkInBothDirections(t *testing.T) {
	f := setup(t)
	source, e := workspace.Find(filepath.Join(f.root, "source"))
	check(t, e)
	defer source.Close()
	writer, e := transport.Dial(f.url, f.options)
	check(t, e)
	defer writer.Close()
	data := make([]byte, 1<<20)
	_, e = rand.Read(data)
	check(t, e)
	file := filepath.Join(source.Root, "file")
	check(t, os.WriteFile(file, data, 0644))
	_, e = source.Commit("large baseline", "test", nil)
	check(t, e)
	check(t, syncer.Push(writer, source))
	target, e := workspace.Init(filepath.Join(f.root, "delta-target"))
	check(t, e)
	defer target.Close()
	check(t, target.SetConfig("remote", f.url))
	reader, e := transport.Dial(f.url, f.options)
	check(t, e)
	defer reader.Close()
	base, e := syncer.Fetch(reader, target, "main", false)
	check(t, e)
	check(t, target.Checkout("main", base, true, false, nil))
	copy(data[512000:], []byte("edited bytes"))
	check(t, os.WriteFile(file, data, 0644))
	_, e = source.Commit("small edit", "test", nil)
	check(t, e)
	writer.Stats = transport.Traffic{}
	check(t, syncer.Push(writer, source))
	if writer.Stats.SentEncodedBytes > 4096 {
		t.Fatalf("small edit uploaded %d binary bytes", writer.Stats.SentEncodedBytes)
	}
	reader.Stats = transport.Traffic{}
	head, e := syncer.Fetch(reader, target, "main", false)
	check(t, e)
	if reader.Stats.ReceivedEncodedBytes > 4096 {
		t.Fatalf("small edit downloaded %d binary bytes", reader.Stats.ReceivedEncodedBytes)
	}
	snapshot, e := target.Store.Snapshot(head)
	check(t, e)
	object, e := target.Store.Verify(snapshot["file"].ID)
	check(t, e)
	if !bytes.Equal(object.Data, data) {
		t.Fatal("delta reconstruction changed contents")
	}
	check(t, target.Checkout("main", head, true, false, nil))
}

// This isolates history transfer, object persistence, and SSH setup. It does
// not materialize working files or simulate a wide-area network.
func BenchmarkHistoryTransfer(b *testing.B) {
	f := setup(b)
	for _, legacy := range []bool{false, true} {
		name := "binary"
		if legacy {
			name = "legacy-json"
		}
		b.Run(name, func(b *testing.B) {
			b.ResetTimer()
			var calls, wire int64
			for i := 0; i < b.N; i++ {
				stats, _ := receive(b, f, legacy, fmt.Sprintf("%s-%d", name, i))
				calls += stats.Requests
				wire += stats.ReceivedEncodedBytes
			}
			b.ReportMetric(float64(calls)/float64(b.N), "rpc/op")
			if !legacy {
				b.ReportMetric(float64(wire)/float64(b.N), "binary-bytes/op")
			}
		})
	}
}
