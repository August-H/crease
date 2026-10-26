package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFramingLimits(t *testing.T) {
	var b bytes.Buffer
	if e := Write(&b, Request{Version: 1, Op: "refs", Repo: "private"}); e != nil {
		t.Fatal(e)
	}
	var q Request
	if e := Read(&b, &q); e != nil || q.Repo != "private" {
		t.Fatalf("round trip: %v", e)
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], MaxFrame+1)
	if e := Read(bytes.NewReader(h[:]), &q); e == nil {
		t.Fatal("oversized frame accepted")
	}
	binary.BigEndian.PutUint32(h[:], 10)
	if e := Read(bytes.NewReader(append(h[:], []byte("{}")...)), &q); e == nil {
		t.Fatal("truncated frame accepted")
	}
}
func FuzzRead(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, '{', '}'})
	f.Fuzz(func(t *testing.T, b []byte) { var q Request; _ = Read(bytes.NewReader(b), &q) })
}
