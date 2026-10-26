package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type failedWatch struct{ fail *atomic.Bool }

func (w failedWatch) Drain() ([]Event, error) {
	if w.fail.Swap(false) {
		return nil, errors.New("simulated overflow or lost watch")
	}
	return nil, nil
}
func (failedWatch) Refresh(string) error { return nil }
func (failedWatch) Close()               {}
func (failedWatch) Name() string         { return "test" }
func TestLostEventsInvalidateClientCursors(t *testing.T) {
	root := t.TempDir()
	if e := os.Mkdir(filepath.Join(root, ".crease"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fail atomic.Bool
	done := make(chan error, 1)
	go func() { done <- run(ctx, root, func(string) (watcher, error) { return failedWatch{fail: &fail}, nil }) }()
	deadline := time.Now().Add(5 * time.Second)
	var first Response
	for {
		var e error
		first, e = Query(root, "changes", "", 0)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(e)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !first.Full {
		t.Fatal("new monitor did not require a full scan")
	}
	stable, e := Query(root, "changes", first.Epoch, first.Seq)
	if e != nil || stable.Full {
		t.Fatalf("valid cursor rejected: %v", e)
	}
	fail.Store(true)
	reset, e := Query(root, "changes", first.Epoch, first.Seq)
	if e != nil || !reset.Full || reset.Epoch == first.Epoch {
		t.Fatalf("lost watch reused a client cursor: %+v %v", reset, e)
	}
	if _, e = Query(root, "stop", "", 0); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
