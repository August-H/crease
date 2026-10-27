package syncer

import (
	"errors"
	"time"

	"crease/internal/codec"
	"crease/internal/model"
	"crease/internal/protocol"
	"crease/internal/transport"
	"crease/internal/workspace"
)

// CloneTimings contains foreground wall times. File reception and the checkout
// pipeline overlap, so these measurements must not be added together.
type CloneTimings struct {
	Mode               string  `json:"mode"`
	AuthenticationMS   float64 `json:"authentication_ms"`
	InitializeMS       float64 `json:"initialize_ms"`
	MetadataMS         float64 `json:"metadata_ms"`
	FallbackFetchMS    float64 `json:"fallback_fetch_ms"`
	PrepareMS          float64 `json:"prepare_ms"`
	FilesMS            float64 `json:"files_ms"`
	CheckoutWaitMS     float64 `json:"checkout_wait_ms"`
	FileSyncMS         float64 `json:"file_sync_ms"`
	CheckoutPipelineMS float64 `json:"checkout_pipeline_ms"`
	PublishMS          float64 `json:"publish_ms"`
	CleanupMS          float64 `json:"cleanup_ms"`
	TotalMS            float64 `json:"total_ms"`
	Requests           int64   `json:"requests"`
	Records            int64   `json:"records"`
	EncodedBytes       int64   `json:"encoded_bytes"`
	Workers            int     `json:"workers"`
	PeakCheckoutBytes  int     `json:"peak_checkout_bytes"`
}

func Milliseconds(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

// Clone receives one pinned snapshot into a fresh workspace, overlapping
// authenticated object ingestion with bounded, durable checkout staging.
func Clone(c *transport.Client, w *workspace.Workspace, branch string, full bool) (timings CloneTimings, err error) {
	traffic := c.Stats
	defer func() {
		timings.Requests = c.Stats.Requests - traffic.Requests
		timings.Records = c.Stats.ReceivedRecords - traffic.ReceivedRecords
		timings.EncodedBytes = c.Stats.ReceivedEncodedBytes - traffic.ReceivedEncodedBytes
	}()
	if !c.Capabilities["clone-stream-v1"] {
		timings.Mode = "fetch-fallback"
		start := time.Now()
		head, e := Fetch(c, w, branch, full)
		timings.FallbackFetchMS = Milliseconds(start)
		if e != nil {
			return timings, e
		}
		start = time.Now()
		stage, e := w.PrepareClone(branch, head, w.Sparse())
		timings.PrepareMS = Milliseconds(start)
		if e != nil {
			return timings, e
		}
		defer func() {
			start := time.Now()
			stage.Close()
			timings.CleanupMS = Milliseconds(start)
		}()
		timings.Workers = stage.Workers
		start = time.Now()
		snapshot, e := w.Store.Snapshot(head)
		if e != nil {
			return timings, e
		}
		seen := map[string]bool{}
		for p, file := range snapshot {
			if !workspace.Selected(p, w.Sparse()) || seen[file.ID] {
				continue
			}
			o, e := w.Store.Get(file.ID)
			if e != nil {
				return timings, e
			}
			if o.Type != "blob" {
				return timings, errors.New("expected checkout blob")
			}
			if e = stage.AddVerified(file.ID, o.Data); e != nil {
				return timings, e
			}
			seen[file.ID] = true
		}
		if e = stage.Wait(); e != nil {
			return timings, e
		}
		timings.CheckoutPipelineMS = Milliseconds(start)
		timings.PeakCheckoutBytes = stage.PeakBytes()
		timings.FileSyncMS = stage.SyncMS
		start = time.Now()
		err = stage.Publish()
		timings.PublishMS = Milliseconds(start)
		return timings, err
	}
	return cloneStream(c, w, branch, full)
}

func cloneStream(c *transport.Client, w *workspace.Workspace, branch string, full bool) (timings CloneTimings, err error) {
	return cloneFromStream(w, branch, full, c.ReceiveStart)
}

type cloneReader func(protocol.Request, func(protocol.Response) error, func([]byte) error) (protocol.Response, error)

func cloneFromStream(w *workspace.Workspace, branch string, full bool, receive cloneReader) (timings CloneTimings, err error) {
	timings.Mode = "clone-stream-v1"
	var header protocol.Response
	var stage *workspace.CloneCheckout
	defer func() {
		if stage != nil {
			start := time.Now()
			stage.Close()
			timings.CleanupMS = Milliseconds(start)
		}
	}()
	metadata := map[string]bool{}
	allowed := map[string]bool{}
	started := time.Now()
	var pipeline, files time.Time
	prepare := func() error {
		if stage != nil {
			return nil
		}
		timings.MetadataMS = Milliseconds(started)
		start := time.Now()
		if e := w.Store.Walk(header.Head, nil, false, func(string, model.Object) error { return nil }); e != nil {
			return e
		}
		var e error
		stage, e = w.PrepareClone(branch, header.Head, w.Sparse())
		if e != nil {
			return e
		}
		timings.Workers = stage.Workers
		if full {
			ids, e := Inventory(w.Store, header.Head)
			if e != nil {
				return e
			}
			for _, id := range ids {
				if !metadata[id] {
					allowed[id] = true
				}
			}
		} else {
			snapshot, e := w.Store.Snapshot(header.Head)
			if e != nil {
				return e
			}
			for p, file := range snapshot {
				if workspace.Selected(p, w.Sparse()) {
					allowed[file.ID] = true
				}
			}
		}
		timings.PrepareMS = Milliseconds(start)
		pipeline, files = time.Now(), time.Now()
		return nil
	}
	_, err = receive(protocol.Request{Op: "clone_stream", Branch: branch, Full: full, Paths: w.Sparse()},
		func(response protocol.Response) error {
			head, ok := response.Refs[branch]
			if !ok || head != response.Head || (head != "" && !model.ValidID(head)) {
				return errors.New("invalid pinned clone head")
			}
			for name, id := range response.Refs {
				if !model.ValidRef(name) || (id != "" && !model.ValidID(id)) {
					return errors.New("invalid clone reference")
				}
			}
			header = response
			return nil
		}, func(record []byte) error {
			id, kind, base, e := codec.Inspect(record)
			if e != nil {
				return e
			}
			if base != "" {
				return errors.New("clone stream requires independent records")
			}
			if kind != "blob" {
				if stage != nil || metadata[id] || len(metadata) >= 2_000_000 {
					return errors.New("invalid clone metadata order or count")
				}
				metadata[id] = true
				_, e = w.Store.ImportObject(record)
				return e
			}
			if e = prepare(); e != nil {
				return e
			}
			if !allowed[id] {
				return errors.New("unexpected or duplicate clone blob")
			}
			o, e := w.Store.ImportObject(record)
			if e != nil {
				return e
			}
			delete(allowed, id)
			if stage.Wants(id) {
				return stage.AddVerified(id, o.Data)
			}
			return nil
		})
	if err != nil {
		return timings, err
	}
	if err = prepare(); err != nil {
		return timings, err
	}
	timings.FilesMS = Milliseconds(files)
	if len(allowed) != 0 {
		return timings, errors.New("clone stream omitted file contents")
	}
	started = time.Now()
	if err = stage.Wait(); err != nil {
		return timings, err
	}
	timings.CheckoutWaitMS = Milliseconds(started)
	timings.CheckoutPipelineMS = Milliseconds(pipeline)
	timings.PeakCheckoutBytes = stage.PeakBytes()
	timings.FileSyncMS = stage.SyncMS
	started = time.Now()
	if err = w.Store.Flush(); err != nil {
		return timings, err
	}
	tx, err := w.DB.Begin()
	if err != nil {
		return timings, err
	}
	defer tx.Rollback()
	for name, head := range header.Refs {
		if _, err = tx.Exec(`INSERT INTO remote_refs VALUES (?,?)`, name, head); err != nil {
			return timings, err
		}
	}
	if header.Head != "" {
		if _, err = tx.Exec(`INSERT INTO fetched_heads VALUES (?,?)`, header.Head, boolInt(full)); err != nil {
			return timings, err
		}
	}
	if err = tx.Commit(); err != nil {
		return timings, err
	}
	err = stage.Publish()
	timings.PublishMS = Milliseconds(started)
	return timings, err
}
