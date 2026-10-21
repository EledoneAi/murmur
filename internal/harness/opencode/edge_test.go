package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// The plugin names the binary in a JavaScript string: a path with quotes, backslashes, a line end, JavaScript's line
// separators or a replacement pattern stays that one string.
func TestSourceQuotesTheBinary(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	exe := "/Users/it's \"me\"/a\\b/$&/ \n</script>/murmur"
	for _, line := range strings.Split(Source(exe), "\n") {
		lit, ok := strings.CutPrefix(line, "const MURMUR = ")
		if !ok {
			continue
		}
		var got string
		if err := json.Unmarshal([]byte(lit), &got); err != nil || got != exe || strings.ContainsRune(lit, ' ') {
			t.Fatalf("%q reads as %q, %v", lit, got, err)
		}
		return
	}
	t.Fatal("no MURMUR line: the binary's path broke the plugin's lines")
}

// What the plugin sends with odd values, and arguments of a tool murmur doesn't know, is read without failing; a
// field of the wrong type is no event.
func TestParseOddities(t *testing.T) {
	ev, err := OpenCode{}.Parse(harness.BeforeTool, []byte(`{"session_id":"ses 1\n","cwd":"/r/a b","pid":-1,"tool":"new_tool","args":"not an object"}`))
	if err != nil || ev.SessionID != "ses 1\n" || ev.Call == nil || ev.Call.Doing != "new_tool" || len(ev.Paths) != 0 {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = OpenCode{}.Parse(harness.BeforeTool, []byte(`{"session_id":"s","cwd":"/r","tool":"write","args":{"filePath":"../x/a.go"}}`))
	if err != nil || len(ev.Paths) != 1 || ev.Paths[0] != "/x/a.go" {
		t.Fatalf("%+v %v", ev.Paths, err)
	}
	if _, err := (OpenCode{}).Parse(harness.Start, []byte(`{"session_id":"s","pid":"12"}`)); err == nil {
		t.Fatal("a pid as a string read as an event")
	}
}
