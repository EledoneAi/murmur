package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------- settings files, keys kept in order

// Harnesses keep their settings in JSON files that people edit too. murmur changes its own entries and leaves every
// other key where it was, written as it was: no HTML escaping, the file's own indentation.

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

// Get and Set take a key written twice as the harnesses' JSON readers do: the last one counts.
func (o Object) Get(k string) json.RawMessage {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].k == k {
			return o[i].v
		}
	}
	return nil
}

func (o Object) Set(k string, v json.RawMessage) Object {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].k == k {
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
		b.Write(Marshal(f.k))
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

// Marshal is json.Marshal without HTML escaping: a person's "a && b" stays as they wrote it.
func Marshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// ErrComments is a settings file murmur can read but not write without losing its person's comments.
var ErrComments = errors.New("it has comments, which rewriting it would lose")

// ReadObject reads a settings file, and returns what it held for WriteObject. A missing file is an empty object.
// Comments and trailing commas, which some harnesses allow, are read past.
func ReadObject(path string) (Object, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	o, err := ParseObject(b)
	if err != nil {
		if o, err2 := ParseObject(StripComments(b)); err2 == nil {
			return o, b, nil
		}
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, b, nil
}

// WriteObject saves o in place of the settings file that held b, keeping its key order, its indentation and its
// permissions. When murmur adds to it, it keeps a copy of the file as it was before murmur first changed it; when
// murmur takes its entries out and nothing is left, the file goes. It never rewrites a file with comments.
func WriteObject(path string, b []byte, o Object, adding bool) error {
	if b != nil && !json.Valid(b) {
		return ErrComments
	}
	if len(o) == 0 && !adding {
		if b == nil {
			return nil
		}
		if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSymlink == 0 {
			return os.Remove(path)
		}
		// a link the person keeps (their dotfiles) stays a link, to an empty object
	}
	var out bytes.Buffer
	if err := json.Indent(&out, o.Bytes(), "", indentOf(b)); err != nil {
		return err
	}
	if b == nil || bytes.HasSuffix(b, []byte("\n")) {
		out.WriteByte('\n')
	}
	text := out.Bytes()
	if bytes.Contains(b, []byte("\r\n")) { // Windows line ends stay
		text = bytes.ReplaceAll(text, []byte("\n"), []byte("\r\n"))
	}
	if bytes.Equal(text, b) {
		return nil
	}
	if keep := path + ".before-murmur"; adding && len(b) > 2 {
		if _, err := os.Stat(keep); errors.Is(err, os.ErrNotExist) {
			_ = os.WriteFile(keep, b, 0o600)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return WriteAtomic(path, text, 0o644)
}

// WriteAtomic replaces the file at path with b, keeping its permissions (mode for a new one): a reader sees the old
// file or the new one, never half of it. A link at path, such as a settings file a person keeps with their dotfiles,
// stays a link: its target is replaced. A file this user can't write stays as it is, though replacing it would work.
func WriteAtomic(path string, b []byte, mode os.FileMode) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
		if unix.Access(path, unix.W_OK) != nil {
			return &os.PathError{Op: "write", Path: path, Err: os.ErrPermission}
		}
	}
	return ReplaceFile(path, b, mode)
}

// ReplaceFile replaces whatever is at path, a link included, with a file holding b, atomically.
func ReplaceFile(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".murmur-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// indentOf is one level of a JSON file's indentation, from its first indented line: two spaces for a new file.
func indentOf(b []byte) string {
	for _, line := range bytes.Split(b, []byte("\n"))[1:] {
		if t := bytes.TrimLeft(line, " \t"); len(t) > 0 && len(t) < len(line) {
			return string(line[:len(line)-len(t)])
		}
	}
	return "  "
}

// StripComments turns JSON with comments and trailing commas into JSON.
func StripComments(b []byte) []byte {
	var out []byte
	inString, escaped := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inString:
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			if i < len(b) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && (b[i] != '*' || b[i+1] != '/') {
				i++
			}
			i++
		case c == ']' || c == '}':
			// a comma before the closing bracket, past any blanks, goes
			j := len(out) - 1
			for j >= 0 && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
				j--
			}
			if j >= 0 && out[j] == ',' {
				out = append(out[:j], out[j+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// OurGroup says whether a group of hooks in a Claude-style settings file, {matcher, hooks: [{command}]}, holds one of
// murmur's.
func OurGroup(group json.RawMessage) bool { return groupHas(group, Ours) }

// CurrentGroup says whether a group of hooks holds one of murmur's in this version's form.
func CurrentGroup(group json.RawMessage) bool { return groupHas(group, Current) }

// groupHas says whether a group of hooks, or a flat entry {command} or {bash}, has a command that is.
func groupHas(entry json.RawMessage, is func(string) bool) bool {
	var g struct {
		Command, Bash string
		Hooks         []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	_ = json.Unmarshal(entry, &g)
	if is(g.Command) || is(g.Bash) {
		return true
	}
	for _, h := range g.Hooks {
		if is(h.Command) {
			return true
		}
	}
	return false
}

// HasOurs says whether any event in a hooks object holds one of murmur's hooks, in a Claude-style group or as a
// flat entry.
func HasOurs(hooks Object) bool { return hasEntry(hooks, Ours) }

// HasOld says whether any event in a hooks object holds a hook of murmur's in 1.0.1's form, which runs nothing now.
func HasOld(hooks Object) bool {
	return hasEntry(hooks, func(c string) bool { return Ours(c) && !Current(c) })
}

func hasEntry(hooks Object, is func(string) bool) bool {
	for _, f := range hooks {
		var entries []json.RawMessage
		_ = json.Unmarshal(f.v, &entries)
		if slices.ContainsFunc(entries, func(e json.RawMessage) bool { return groupHas(e, is) }) {
			return true
		}
	}
	return false
}

// Replace puts murmur's entry where murmur's was in an event's list, and drops any other of murmur's: harnesses
// such as Codex trust hooks by their place, so the person's hooks after murmur's keep theirs. With entry nil it only
// drops them. A hook of the person's in a group with murmur's stays, in what is left of that group. It says whether
// the list held any of murmur's.
func Replace(entries []json.RawMessage, entry json.RawMessage) ([]json.RawMessage, bool) {
	at := slices.IndexFunc(entries, OurGroup)
	if at < 0 {
		if entry != nil {
			entries = append(entries, entry)
		}
		return entries, false
	}
	out := slices.Clone(entries[:at])
	if entry != nil {
		out = append(out, entry)
	}
	for _, e := range entries[at:] {
		if e = withoutOurs(e); e != nil {
			out = append(out, e)
		}
	}
	return out, true
}

// withoutOurs is a group of hooks without murmur's, or nil when nothing else is in it: Claude Code's /hooks adds a
// person's hook to the group whose matcher they pick, murmur's too.
func withoutOurs(group json.RawMessage) json.RawMessage {
	if !OurGroup(group) {
		return group
	}
	g, err := ParseObject(group)
	if err != nil {
		return nil
	}
	var hooks []json.RawMessage
	_ = json.Unmarshal(g.Get("hooks"), &hooks)
	theirs := slices.DeleteFunc(hooks, func(h json.RawMessage) bool { return groupHas(h, Ours) })
	if len(theirs) == 0 {
		return nil
	}
	return g.Set("hooks", Marshal(theirs)).Bytes()
}
