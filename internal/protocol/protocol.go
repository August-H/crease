// Package protocol defines v1 RPC framing carried inside an authenticated SSH channel.
package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"crease/internal/model"
)

const Version = 1
const MaxFrame = 16 << 20
const MaxBatch = 128

type Request struct {
	Version   int               `json:"version"`
	Op        string            `json:"op"`
	Repo      string            `json:"repo"`
	Branch    string            `json:"branch,omitempty"`
	Head      string            `json:"head,omitempty"`
	Expected  string            `json:"expected,omitempty"`
	IDs       []string          `json:"ids,omitempty"`
	Objects   []model.Object    `json:"objects,omitempty"`
	PublicKey string            `json:"public_key,omitempty"`
	Role      string            `json:"role,omitempty"`
	ID        int64             `json:"id,omitempty"`
	Title     string            `json:"title,omitempty"`
	Body      string            `json:"body,omitempty"`
	Source    string            `json:"source,omitempty"`
	Target    string            `json:"target,omitempty"`
	Token     string            `json:"token,omitempty"`
	Cursor    int64             `json:"cursor,omitempty"`
	Limit     int               `json:"limit,omitempty"`
	Name      string            `json:"name,omitempty"`
	Names     []string          `json:"names,omitempty"`
	Status    string            `json:"status,omitempty"`
	Candidate string            `json:"candidate,omitempty"`
	Haves     []string          `json:"haves,omitempty"`
	Paths     []string          `json:"paths,omitempty"`
	Full      bool              `json:"full,omitempty"`
	Bases     map[string]string `json:"bases,omitempty"`
}
type Grant struct {
	Fingerprint string `json:"fingerprint"`
	Role        string `json:"role"`
}
type PR struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	State   string `json:"state"`
	Author  string `json:"author"`
	Created string `json:"created"`
}
type Comment struct {
	ID      int64  `json:"id"`
	Author  string `json:"author"`
	Head    string `json:"head"`
	Body    string `json:"body"`
	Created string `json:"created"`
}
type Job struct {
	ID      int64  `json:"id"`
	PR      int64  `json:"pr"`
	State   string `json:"state"`
	Message string `json:"message"`
	Head    string `json:"head"`
}
type Check struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Candidate string `json:"candidate"`
}
type Response struct {
	Version      int                     `json:"version"`
	Error        string                  `json:"error,omitempty"`
	Refs         map[string]string       `json:"refs,omitempty"`
	Head         string                  `json:"head,omitempty"`
	Default      string                  `json:"default,omitempty"`
	IDs          []string                `json:"ids,omitempty"`
	Objects      map[string]model.Object `json:"objects,omitempty"`
	Grants       []Grant                 `json:"grants,omitempty"`
	PR           *PR                     `json:"pr,omitempty"`
	PRs          []PR                    `json:"prs,omitempty"`
	Comments     []Comment               `json:"comments,omitempty"`
	Job          *Job                    `json:"job,omitempty"`
	Text         string                  `json:"text,omitempty"`
	Names        []string                `json:"names,omitempty"`
	Checks       []Check                 `json:"checks,omitempty"`
	Capabilities []string                `json:"capabilities,omitempty"`
	Stream       bool                    `json:"stream,omitempty"`
	Records      int64                   `json:"records,omitempty"`
	RawBytes     int64                   `json:"raw_bytes,omitempty"`
}

func Write(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > MaxFrame {
		return errors.New("protocol frame exceeds limit")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if _, e = w.Write(h[:]); e != nil {
		return e
	}
	_, e = io.Copy(w, bytes.NewReader(b))
	return e
}
func Read(r io.Reader, v any) error {
	var h [4]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return e
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxFrame {
		return errors.New("invalid frame length")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(r, b); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing protocol data")
	}
	return nil
}
