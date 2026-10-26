// Package codec encodes bounded binary object records for packs and SSH streams.
// Object IDs still hash the original model payload, never this physical encoding.
package codec

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	"crease/internal/model"
)

const Header = 74 // ID[32], type, encoding, raw length[4], stored length[4], base[32]
const MaxRecord = Header + model.MaxObject + 8
const MaxDepth = 8

var ErrEnd = errors.New("end of binary object stream")

var writers = sync.Pool{New: func() any { w, _ := flate.NewWriter(io.Discard, flate.BestSpeed); return w }}

func compress(b []byte) []byte {
	var out bytes.Buffer
	w := writers.Get().(*flate.Writer)
	w.Reset(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	writers.Put(w)
	return out.Bytes()
}
func kind(s string) byte {
	switch s {
	case "blob":
		return 1
	case "tree":
		return 2
	case "commit":
		return 3
	}
	return 0
}
func Kind(b byte) string {
	switch b {
	case 1:
		return "blob"
	case 2:
		return "tree"
	case 3:
		return "commit"
	}
	return ""
}

func Encode(o model.Object, base *model.Object) ([]byte, error) {
	if e := o.Validate(); e != nil {
		return nil, e
	}
	encoding := byte(0)
	stored := o.Data
	var baseID string
	if len(o.Data) >= 128 {
		z := compress(o.Data)
		if len(z)+16 < len(stored) {
			stored = z
			encoding = 1
		}
	}
	if base != nil && base.Type == "blob" && o.Type == "blob" && base.ID() != o.ID() {
		prefix := 0
		for prefix < len(base.Data) && prefix < len(o.Data) && base.Data[prefix] == o.Data[prefix] {
			prefix++
		}
		suffix := 0
		for suffix < len(base.Data)-prefix && suffix < len(o.Data)-prefix && base.Data[len(base.Data)-1-suffix] == o.Data[len(o.Data)-1-suffix] {
			suffix++
		}
		patch := make([]byte, 8+len(o.Data)-prefix-suffix)
		binary.BigEndian.PutUint32(patch, uint32(prefix))
		binary.BigEndian.PutUint32(patch[4:], uint32(suffix))
		copy(patch[8:], o.Data[prefix:len(o.Data)-suffix])
		z := compress(patch)
		if len(z)+32 < len(stored) {
			stored = z
			encoding = 2
			baseID = base.ID()
		}
	}
	b := make([]byte, Header+len(stored))
	id, _ := hex.DecodeString(o.ID())
	copy(b, id)
	b[32] = kind(o.Type)
	b[33] = encoding
	binary.BigEndian.PutUint32(b[34:], uint32(len(o.Data)))
	binary.BigEndian.PutUint32(b[38:], uint32(len(stored)))
	if baseID != "" {
		raw, _ := hex.DecodeString(baseID)
		copy(b[42:], raw)
	}
	copy(b[Header:], stored)
	return b, nil
}
func Inspect(b []byte) (id, typ, base string, err error) {
	if len(b) < Header || len(b) > MaxRecord {
		return "", "", "", errors.New("invalid binary object length")
	}
	n := int(binary.BigEndian.Uint32(b[38:]))
	raw := int(binary.BigEndian.Uint32(b[34:]))
	typ = Kind(b[32])
	if n != len(b)-Header || raw > model.MaxObject || typ == "" || b[33] > 2 {
		return "", "", "", errors.New("invalid binary object header")
	}
	id = hex.EncodeToString(b[:32])
	if b[33] == 2 {
		if typ != "blob" {
			return "", "", "", errors.New("only blobs may use delta encoding")
		}
		base = hex.EncodeToString(b[42:74])
	} else if !bytes.Equal(b[42:74], make([]byte, 32)) {
		return "", "", "", errors.New("unexpected delta base")
	}
	return
}
func Decode(b []byte, get func(string) (model.Object, error)) (model.Object, error) {
	id, typ, base, e := Inspect(b)
	if e != nil {
		return model.Object{}, e
	}
	rawLen := int(binary.BigEndian.Uint32(b[34:]))
	data := b[Header:]
	if b[33] != 0 {
		r := flate.NewReader(bytes.NewReader(data))
		decoded, e := io.ReadAll(io.LimitReader(r, model.MaxObject+9))
		ce := r.Close()
		if e != nil {
			return model.Object{}, e
		}
		if ce != nil {
			return model.Object{}, ce
		}
		if len(decoded) > model.MaxObject+8 {
			return model.Object{}, errors.New("decompressed record exceeds limit")
		}
		data = decoded
	}
	if base != "" {
		if len(data) < 8 || get == nil {
			return model.Object{}, errors.New("delta base is unavailable")
		}
		o, e := get(base)
		if e != nil {
			return model.Object{}, e
		}
		if o.Type != "blob" {
			return model.Object{}, errors.New("delta base must be a blob")
		}
		prefix, suffix := int(binary.BigEndian.Uint32(data)), int(binary.BigEndian.Uint32(data[4:]))
		if prefix > len(o.Data) || suffix > len(o.Data)-prefix || prefix+suffix+len(data)-8 != rawLen {
			return model.Object{}, errors.New("invalid delta bounds")
		}
		out := make([]byte, 0, rawLen)
		out = append(out, o.Data[:prefix]...)
		out = append(out, data[8:]...)
		out = append(out, o.Data[len(o.Data)-suffix:]...)
		data = out
	}
	if len(data) != rawLen {
		return model.Object{}, errors.New("decoded object length mismatch")
	}
	o := model.Object{Type: typ, Data: data}
	if e = o.Validate(); e != nil {
		return o, e
	}
	if o.ID() != id {
		return o, errors.New("binary object integrity check failed")
	}
	return o, nil
}
func Write(w io.Writer, b []byte) error {
	if len(b) < Header || len(b) > MaxRecord {
		return errors.New("binary record exceeds limit")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if _, e := w.Write(h[:]); e != nil {
		return e
	}
	_, e := io.Copy(w, bytes.NewReader(b))
	return e
}

// A zero-length record ends a stream. A framed JSON response follows it.
func Read(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return nil, e
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 {
		return nil, ErrEnd
	}
	if n < Header || n > MaxRecord {
		return nil, errors.New("invalid binary record size")
	}
	b := make([]byte, n)
	_, e := io.ReadFull(r, b)
	return b, e
}
func End(w io.Writer) error { _, e := w.Write([]byte{0, 0, 0, 0}); return e }
