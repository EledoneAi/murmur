package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ourStart = `{"matcher":"","hooks":[{"type":"command","command":"/opt/bin/murmur hook claude start","timeout":5}]}`

// A file that isn't a JSON object, or isn't there as a file, is never read as one: an empty object would have join
// write over it.
func TestReadObjectRefuses(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"null": "null", "array": `[{"a":1}]`, "string": `"x"`, "number": "1",
		"bom": "\xef\xbb\xbf{\"a\":1}", "garbage": "\x00\x01\xff", "half": `{"a": `} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, []byte(content), 0o600)
		if o, _, err := ReadObject(path); err == nil {
			t.Errorf("%s: read as %s", name, o.Bytes())
		}
	}
	if _, _, err := ReadObject(dir); err == nil {
		t.Error("a folder read as a settings file")
	}
}

// A file that holds more than one object, or one cut short, is read for what it says but never rewritten: writing
// it would drop what comes after the first object.
func TestWriteObjectRefusesWhatItCannotKeep(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"two": `{"a":1}{"b":2}`, "trailing": `{"a":1} x`, "cut": `{"a":1`} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, []byte(content), 0o600)
		o, b, err := ReadObject(path)
		if err != nil {
			continue
		}
		if err := WriteObject(path, b, o.Set("m", json.RawMessage(`1`)), true); err == nil {
			t.Errorf("%s: rewritten", name)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("%s: changed to %s", name, got)
		}
	}
}

// A key written twice is read as the harnesses read it, the last one winning: murmur edits that one.
func TestDuplicateKeys(t *testing.T) {
	o, err := ParseObject([]byte(`{"hooks": {"a": 1}, "model": "x", "hooks": {"b": 2}}`))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]json.RawMessage
	_ = json.Unmarshal(o.Bytes(), &want)
	if got := string(o.Get("hooks")); got != string(want["hooks"]) {
		t.Fatalf("Get reads %s, the harness %s", got, want["hooks"])
	}
	o = o.Set("hooks", json.RawMessage(`{"c":3}`))
	_ = json.Unmarshal(o.Bytes(), &want)
	if string(want["hooks"]) != `{"c":3}` {
		t.Fatalf("Set changed the copy the harness doesn't read: %s", o.Bytes())
	}
}

// A settings file with Windows line ends keeps them: uninstall leaves it as it was.
func TestCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	orig := "{\r\n  \"model\": \"x\",\r\n  \"n\": [\r\n    1\r\n  ]\r\n}\r\n"
	_ = os.WriteFile(path, []byte(orig), 0o600)
	o, b, err := ReadObject(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteObject(path, b, o.Set("m", Marshal([]string{"y"})), true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if bytes.Count(got, []byte("\n")) != bytes.Count(got, []byte("\r\n")) {
		t.Fatalf("line ends changed:\n%q", got)
	}
	o, b, _ = ReadObject(path)
	if err := WriteObject(path, b, o.Del("m"), false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Fatalf("uninstall left\n%q\nwant\n%q", got, orig)
	}
}

// A settings file its person made read-only stays as it is: murmur says it can't change it.
func TestReadOnlySettings(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes any file")
	}
	path := filepath.Join(t.TempDir(), "s.json")
	_ = os.WriteFile(path, []byte("{\n  \"a\": 1\n}\n"), 0o444)
	o, b, err := ReadObject(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteObject(path, b, o.Set("m", json.RawMessage(`1`)), true); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("wrote a read-only file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "{\n  \"a\": 1\n}\n" {
		t.Fatalf("changed to %s", got)
	}
}

// A link to a link to the settings file stays a link, and so does a relative one: murmur writes the file at the end.
func TestLinkChain(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	_ = os.MkdirAll(filepath.Dir(real), 0o755)
	_ = os.WriteFile(real, []byte("{\n  \"a\": 1\n}\n"), 0o600)
	mid := filepath.Join(dir, "mid.json")
	_ = os.Symlink(filepath.Join("dotfiles", "settings.json"), mid)
	path := filepath.Join(dir, "tool", "settings.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.Symlink(mid, path)
	o, b, err := ReadObject(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteObject(path, b, o.Set("m", json.RawMessage(`1`)), true); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{path, mid} {
		if st, err := os.Lstat(l); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a link", l)
		}
	}
	if got, _ := os.ReadFile(real); !strings.Contains(string(got), `"m": 1`) {
		t.Fatalf("the file at the end: %s", got)
	}
	if st, _ := os.Stat(real); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

// A person's hook in the same group as murmur's, as Claude Code's /hooks adds one under a matcher that is there
// already, is theirs: joining again and uninstalling keep it.
func TestReplaceKeepsTheirHookInOurGroup(t *testing.T) {
	mixed := json.RawMessage(`{"matcher":"","hooks":[{"type":"command","command":"/opt/bin/murmur hook claude start","timeout":5},{"type":"command","command":"my-own start"}]}`)
	other := json.RawMessage(`{"hooks":[{"type":"command","command":"after"}]}`)
	entry := json.RawMessage(ourStart)
	out, had := Replace([]json.RawMessage{mixed, other}, entry)
	if !had || !bytes.Contains(Marshal(out), []byte("my-own start")) || !bytes.Contains(Marshal(out), []byte("after")) {
		t.Fatalf("join: %s", Marshal(out))
	}
	again, _ := Replace(out, entry)
	if string(Marshal(again)) != string(Marshal(out)) {
		t.Fatalf("join again: %s, was %s", Marshal(again), Marshal(out))
	}
	gone, _ := Replace(out, nil)
	if s := string(Marshal(gone)); strings.Contains(s, "murmur hook") || !strings.Contains(s, "my-own start") || !strings.Contains(s, "after") {
		t.Fatalf("uninstall: %s", s)
	}
}

// A process that runs is alive; 0 and -1, which kill takes for this process group and for every process, are not.
func TestAliveNamesOneProcess(t *testing.T) {
	if !Alive(os.Getpid()) || Alive(0) || Alive(-1) {
		t.Fatalf("self %v, 0 %v, -1 %v", Alive(os.Getpid()), Alive(0), Alive(-1))
	}
}

// murmur's entries written twice, by hand or by a merge of dotfiles, become one in the first one's place.
func TestReplaceOursTwice(t *testing.T) {
	mine := json.RawMessage(`{"hooks":[{"type":"command","command":"mine"}]}`)
	old := json.RawMessage(`{"matcher":"","hooks":[{"type":"command","command":"/usr/local/bin/murmur hook start"}]}`)
	entry := json.RawMessage(ourStart)
	out, _ := Replace([]json.RawMessage{old, mine, entry, old}, entry)
	if got := string(Marshal(out)); got != string(Marshal([]json.RawMessage{entry, mine})) {
		t.Fatalf("join: %s", got)
	}
	if out, _ := Replace([]json.RawMessage{old, mine, entry}, nil); string(Marshal(out)) != string(Marshal([]json.RawMessage{mine})) {
		t.Fatalf("uninstall: %s", Marshal(out))
	}
}
