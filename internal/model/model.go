// Package model defines Crease's versioned, content-addressed object format.
package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const Version = 1
const MaxObject = 8 << 20

type Object struct {
	Type string `json:"type"`
	Data []byte `json:"data"`
}

type Entry struct {
	Name string `json:"name"`
	Mode string `json:"mode"` // file, exec, or tree; symlinks are rejected in v1.
	ID   string `json:"id"`
}
type Tree struct {
	Entries []Entry `json:"entries"`
}
type Commit struct {
	Tree    string   `json:"tree"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Time    string   `json:"time"` // UTC RFC3339Nano
	Message string   `json:"message"`
}

var objectID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var refName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

func ValidID(id string) bool { return objectID.MatchString(id) }
func ValidRef(s string) bool {
	return refName.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") && !strings.HasSuffix(s, "/")
}
func ValidPath(p string) bool {
	if p == "" || p == "." || !utf8.ValidString(p) || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00:") {
		return false
	}
	for _, r := range p {
		if r < 32 || r == 127 {
			return false
		}
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." || strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") || strings.EqualFold(s, ".crease") || strings.EqualFold(s, ".git") {
			return false
		}
		// Avoid Windows device paths even when a repository is created on Unix.
		base := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return false
		}
	}
	return true
}

func (o Object) ID() string {
	h := sha256.New()
	fmt.Fprintf(h, "crease %d %s %d\x00", Version, o.Type, len(o.Data))
	h.Write(o.Data)
	return hex.EncodeToString(h.Sum(nil))
}

func JSON(kind string, v any) (Object, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Object{}, err
	}
	o := Object{Type: kind, Data: b}
	return o, o.Validate()
}

func decodeCanonical(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, canonical) {
		return errors.New("noncanonical object encoding")
	}
	return nil
}

func (o Object) Tree() (Tree, error) { var t Tree; err := decodeCanonical(o.Data, &t); return t, err }
func (o Object) Commit() (Commit, error) {
	var c Commit
	err := decodeCanonical(o.Data, &c)
	return c, err
}

func (o Object) Validate() error {
	if len(o.Data) > MaxObject {
		return fmt.Errorf("object exceeds %d byte limit", MaxObject)
	}
	switch o.Type {
	case "blob":
		return nil
	case "tree":
		t, err := o.Tree()
		if err != nil {
			return err
		}
		last := ""
		folded := map[string]bool{}
		for _, e := range t.Entries {
			fold := cases.Fold().String(norm.NFC.String(e.Name))
			if !ValidPath(e.Name) || strings.Contains(e.Name, "/") || e.Name <= last || folded[fold] || !ValidID(e.ID) {
				return errors.New("invalid, unsorted, or case-colliding tree entry")
			}
			if e.Mode != "file" && e.Mode != "exec" && e.Mode != "tree" {
				return errors.New("unsupported file mode")
			}
			last = e.Name
			folded[fold] = true
		}
	case "commit":
		c, err := o.Commit()
		if err != nil {
			return err
		}
		if !ValidID(c.Tree) || len(c.Parents) > 2 || c.Author == "" || c.Message == "" || c.Time == "" {
			return errors.New("invalid commit")
		}
		for _, id := range c.Parents {
			if !ValidID(id) {
				return errors.New("invalid parent ID")
			}
		}
	default:
		return errors.New("unknown object type")
	}
	return nil
}

func (o Object) Links() ([]string, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	var ids []string
	switch o.Type {
	case "commit":
		c, _ := o.Commit()
		ids = append(ids, c.Tree)
		ids = append(ids, c.Parents...)
	case "tree":
		t, _ := o.Tree()
		for _, e := range t.Entries {
			ids = append(ids, e.ID)
		}
	}
	return ids, nil
}

func Sort(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}
