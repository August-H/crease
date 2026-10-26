package codec

import (
	"bytes"
	"crease/internal/model"
	"encoding/binary"
	"io"
	"testing"
)

func TestBinaryCompressionAndDelta(t *testing.T) {
	base := model.Object{Type: "blob", Data: bytes.Repeat([]byte("a deterministic source line\n"), 10000)}
	next := model.Object{Type: "blob", Data: bytes.Clone(base.Data)}
	copy(next.Data[1000:], "changed")
	raw, e := Encode(base, nil)
	if e != nil {
		t.Fatal(e)
	}
	if raw[33] != 1 {
		t.Fatal("compressible object was not compressed")
	}
	delta, e := Encode(next, &base)
	if e != nil {
		t.Fatal(e)
	}
	if delta[33] != 2 || len(delta) >= len(raw) {
		t.Fatal("similar version did not use a smaller delta")
	}
	got, e := Decode(delta, func(id string) (model.Object, error) {
		if id != base.ID() {
			t.Fatal("wrong base")
		}
		return base, nil
	})
	if e != nil || got.ID() != next.ID() {
		t.Fatalf("round trip: %v", e)
	}
	delta[len(delta)-1] ^= 0xff
	if _, e = Decode(delta, func(string) (model.Object, error) { return base, nil }); e == nil {
		t.Fatal("corrupt delta accepted")
	}
	bad := bytes.Clone(raw)
	binary.BigEndian.PutUint32(bad[34:], model.MaxObject+1)
	if _, e = Decode(bad, nil); e == nil {
		t.Fatal("oversized decoded length accepted")
	}
}
func TestStreamEndIsDifferentFromTruncation(t *testing.T) {
	if _, e := Read(bytes.NewReader(nil)); e != io.EOF {
		t.Fatalf("unexpected transport EOF: %v", e)
	}
	if _, e := Read(bytes.NewReader([]byte{0, 0, 0, 0})); e != ErrEnd {
		t.Fatalf("stream terminator not recognized: %v", e)
	}
	if _, e := Read(bytes.NewReader([]byte{0, 0})); e != io.ErrUnexpectedEOF {
		t.Fatalf("truncated header accepted: %v", e)
	}
}
func FuzzRecord(f *testing.F) {
	r, _ := Encode(model.Object{Type: "blob", Data: []byte("seed")}, nil)
	f.Add(r)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Decode(b, nil) })
}
