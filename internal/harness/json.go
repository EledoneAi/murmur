package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ---------------------------------------------------------------- settings files, keys kept in order

// Harnesses keep their settings in JSON files that people edit too. murmur changes its own entries and leaves every
// other key where it was.

type field struct {
	k string
	v json.RawMessage
}

// Object is a JSON object whose keys keep their order.
type Object []field

func ParseObject(b []byte) (Object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o Object
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, field{t.(string), v})
	}
	return o, nil
}

func (o Object) Get(k string) json.RawMessage {
	for _, f := range o {
		if f.k == k {
			return f.v
		}
	}
	return nil
}

func (o Object) Set(k string, v json.RawMessage) Object {
	for i, f := range o {
		if f.k == k {
			o[i].v = v
			return o
		}
	}
	return append(o, field{k, v})
}

func (o Object) Del(k string) Object {
	return slices.DeleteFunc(o, func(f field) bool { return f.k == k })
}

func (o Object) Bytes() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.k)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// Sub returns the object stored at key k, or an empty one.
func (o Object) Sub(k string) Object {
	if v := o.Get(k); v != nil {
		if s, err := ParseObject(v); err == nil {
			return s
		}
	}
	return nil
}

// PutSub stores s at k, or removes k when s is empty.
func (o Object) PutSub(k string, s Object) Object {
	if len(s) == 0 {
		return o.Del(k)
	}
	return o.Set(k, s.Bytes())
}

// ReadObject reads a settings file, and returns what it held for WriteObject. A missing file is an empty object.
func ReadObject(path string) (Object, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	o, err := ParseObject(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, b, nil
}

// WriteObject saves o in place of the settings file that held b, keeping its key order and permissions. When murmur
// adds to it, it keeps a copy of the file as it was before murmur first changed it; when murmur takes its entries out
// and nothing is left, the file goes.
func WriteObject(path string, b []byte, o Object, adding bool) error {
	if len(o) == 0 && !adding {
		if b == nil {
			return nil
		}
		return os.Remove(path)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, o.Bytes(), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	if bytes.Equal(out.Bytes(), b) {
		return nil
	}
	if keep := path + ".before-murmur"; adding && len(b) > 2 {
		if _, err := os.Stat(keep); errors.Is(err, os.ErrNotExist) {
			_ = os.WriteFile(keep, b, 0o600)
		}
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".murmur-tmp"
	if err := os.WriteFile(tmp, out.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// OurGroup says whether a group of hooks in a Claude-style settings file, {matcher, hooks: [{command}]}, holds one of
// murmur's.
func OurGroup(group json.RawMessage) bool {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	_ = json.Unmarshal(group, &g)
	return slices.ContainsFunc(g.Hooks, func(h struct {
		Command string `json:"command"`
	}) bool {
		return Ours(h.Command)
	})
}

// HasOurs says whether any event in a hooks object holds one of murmur's hooks, in a Claude-style group or as a
// flat entry {command}.
func HasOurs(hooks Object) bool {
	for _, f := range hooks {
		var entries []json.RawMessage
		_ = json.Unmarshal(f.v, &entries)
		for _, e := range entries {
			var flat struct{ Command, Bash string }
			_ = json.Unmarshal(e, &flat)
			if OurGroup(e) || Ours(flat.Command) || Ours(flat.Bash) {
				return true
			}
		}
	}
	return false
}
