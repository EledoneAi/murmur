package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// reading is a transcript and what it read, as the daemon keeps them.
type reading struct {
	*Transcript
	harness.Activity
}

func (r *reading) Read() bool { return r.Transcript.Read(&r.Activity) }

// maxDirs is how many folders harness.Activity keeps.
const maxDirs = 200

func TestReadTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	long := `{"parentUuid":"x","type":"user","message":"` + strings.Repeat("a", 200<<10) + `"}` + "\n"
	first := `{"type":"ai-title","aiTitle":"Rerun billing on staging","sessionId":"s"}` + "\n" + long +
		`{"type":"pr-link","sessionId":"s","prNumber":1822,"prUrl":"https://github.com/acme/api/pull/1822"}` + "\n" +
		`{"type":"ai-title","aiTitle":"half writ` // still being written
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &reading{Transcript: &Transcript{Path: path}}
	if !s.Read() || s.Title != "Rerun billing on staging" || s.PR != 1822 {
		t.Fatalf("first read: title %q pr %d", s.Title, s.PR)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`ten","sessionId":"s"}` + "\n" + `{"type":"custom-title","customTitle":"Mine","sessionId":"s"}` + "\n")
	f.Close()
	if !s.Read() || s.aiTitle != "half written" || s.Title != "Mine" {
		t.Fatalf("second read: ai %q title %q", s.aiTitle, s.Title)
	}
	if s.Read() {
		t.Fatal("nothing new, yet changed")
	}
	at := time.Now().UTC().Format(time.RFC3339)
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"parentUuid":"p","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/x/client/tui.go","old_string":"\"type\":\"tool_use\""}}]},"type":"assistant","timestamp":"` + at + `"}` + "\n" +
		`{"parentUuid":"p","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./...","description":"Run the tests"}}]},"type":"assistant","timestamp":"` + at + `"}` + "\n" +
		`{"parentUuid":"p","message":{"role":"user","content":[{"type":"tool_result","content":"{\"type\":\"tool_use\"}"}]},"type":"user","timestamp":"` + at + `"}` + "\n")
	f.Close()
	if !s.Read() || s.Doing != "Running go" { // the command's name, not the description the model wrote
		t.Fatalf("tool calls: doing %q", s.Doing)
	}
	if s.Kinds != "ex" { // an edit, then a command
		t.Fatalf("kinds %q", s.Kinds)
	}
	for _, c := range []struct{ tool, in, want string }{
		{"TodoWrite", `{"todos":[{"status":"completed","activeForm":"Reading"},{"status":"in_progress","activeForm":"Fixing the x key"}]}`, "Updating its plan"},
		{"Bash", `{"command":"/usr/bin/gcloud run deploy --token=secret"}`, "Running gcloud"},
		{"Bash", `{"command":"API_KEY=sk-123 go test ./...","description":"Run tests against prod"}`, "Running go"},
		{"Grep", `{"pattern":"customer_name"}`, "Searching"},
		{"WebFetch", `{"url":"https://internal.example/report"}`, "Reading a web page"},
		{"Task", `{"description":"Audit the billing code"}`, "Delegating"},
	} {
		if got := doingOf(c.tool, []byte(c.in)); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.tool, c.in, got, c.want)
		}
	}
	if got := doingOf("mcp__claude_ai_Notion__notion-fetch", nil); got != "Notion · notion-fetch" {
		t.Errorf("mcp tool: %q", got)
	}
}

// answerLine is an assistant line as Claude Code writes it: one block of an answer, with the answer's usage.
func answerLine(block string, used int) string {
	return `{"parentUuid":"p","isSidechain":false,"message":{"model":"claude-x","role":"assistant","content":[` + block +
		`],"usage":{"input_tokens":2,"cache_creation_input_tokens":` + strconv.Itoa(used/2) + `,"cache_read_input_tokens":` +
		strconv.Itoa(used-used/2-2) + `,"output_tokens":9}},"type":"assistant","timestamp":"2026-10-16T09:00:00Z"}` + "\n"
}

func toolBlock(name, input string) string {
	return `{"type":"tool_use","id":"t","name":"` + name + `","input":` + input + `}`
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString(strings.Join(lines, ""))
}

// Kinds keeps the last 30 counted tool calls as letters; tools it doesn't count leave no letter.
func TestKinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	s := &reading{Transcript: &Transcript{Path: path}}
	var lines []string
	for _, c := range []struct{ tool, in string }{
		{"Read", `{"file_path":"a.go"}`}, {"Grep", `{"pattern":"x"}`}, {"Glob", `{"pattern":"*"}`}, {"LS", `{"path":"."}`},
		{"Edit", `{}`}, {"MultiEdit", `{}`}, {"Write", `{}`}, {"NotebookEdit", `{}`}, {"Bash", `{"command":"ls"}`},
		{"WebFetch", `{}`}, {"WebSearch", `{}`}, {"Task", `{}`}, {"Agent", `{}`}, {"mcp__murmur__send", `{}`},
		{"mcp__murmur__btw", `{}`}, {"TodoWrite", `{}`}, {"mcp__claude_ai_Notion__notion-fetch", `{}`}, {"mcp__murmur__who", `{}`},
	} {
		lines = append(lines, answerLine(toolBlock(c.tool, c.in), 1000))
	}
	appendLines(t, path, lines...)
	if !s.Read() || s.Kinds != "rrrreeeexwwssmm" {
		t.Fatalf("kinds %q", s.Kinds)
	}
	lines = lines[:0]
	for range 40 {
		lines = append(lines, answerLine(toolBlock("Read", `{}`), 1000))
	}
	appendLines(t, path, append(lines, answerLine(toolBlock("Edit", `{}`), 1000))...)
	if s.Read(); s.Kinds != strings.Repeat("r", 29)+"e" {
		t.Fatalf("kinds after 41 more: %q", s.Kinds)
	}
}

// The same command in 9 of the last 12 tool calls is a loop; 8 is not, nor is another tool repeated.
func TestLoop(t *testing.T) {
	poll, read := toolBlock("Bash", `{"command":"gh run view 42"}`), toolBlock("Read", `{"file_path":"a.go"}`)
	for _, c := range []struct {
		calls []string
		want  bool
	}{
		{slices.Repeat([]string{poll}, 9), true},
		{append(slices.Repeat([]string{poll}, 8), toolBlock("Bash", `{"command":"gh run view 43"}`)), false},
		{append(slices.Repeat([]string{poll}, 9), slices.Repeat([]string{read}, 3)...), true},
		{append(slices.Repeat([]string{poll}, 9), slices.Repeat([]string{read}, 4)...), false},
		{slices.Repeat([]string{read}, 12), false},
	} {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		for _, b := range c.calls {
			appendLines(t, path, answerLine(b, 1000))
		}
		s := &reading{Transcript: &Transcript{Path: path}}
		if s.Read(); s.Loop != c.want {
			t.Errorf("%d calls: loop %v, want %v", len(c.calls), s.Loop, c.want)
		}
	}
}

// Context is the latest answer's input, cache read and cache creation tokens, out of a million unless the model has
// a 200k window, or until the conversation goes past 200k; a compaction brings it down at once.
func TestContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	s := &reading{Transcript: &Transcript{Path: path}}
	text := `{"type":"text","text":"done"}`
	appendLines(t, path, answerLine(text, 50_000))
	if !s.Read() || s.Context != 5 {
		t.Fatalf("50k of a million: %d%%", s.Context)
	}
	appendLines(t, path, `{"parentUuid":"p","isSidechain":false,"attachment":{"type":"model","identity":{"modelId":"claude-haiku-5-5"},"text":"x"},"type":"attachment","uuid":"u"}`+"\n")
	if !s.Read() || s.Context != 25 {
		t.Fatalf("50k on a Haiku: %d%%", s.Context)
	}
	synthetic := `{"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API Error"}],"usage":{"input_tokens":0,"output_tokens":0}},"type":"assistant"}` + "\n"
	sidechain := strings.Replace(answerLine(text, 190_000), `"isSidechain":false`, `"isSidechain":true`, 1)
	appendLines(t, path, `{"attachment":{"type":"model","identity":{"modelId":"claude-sonnet-4-5"}},"type":"attachment"}`+"\n",
		synthetic, sidechain)
	if s.Read(); s.Context != 25 {
		t.Fatalf("on a 4.x model, after an API error and a subagent's answer: %d%%", s.Context)
	}
	appendLines(t, path, answerLine(text, 400_000))
	if s.Read(); s.Context != 40 {
		t.Fatalf("past 200k, so a million: %d%%", s.Context)
	}
	appendLines(t, path, `{"parentUuid":null,"logicalParentUuid":"l","isSidechain":false,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"manual","preTokens":400000,"postTokens":20000}}`+"\n")
	if s.Read(); s.Context != 2 {
		t.Fatalf("after compacting: %d%%", s.Context)
	}
}

// Tokens is the size of the conversation as its last request sent it, which alone is no change; Said is the first line
// of what the agent last said in its own words, clean and short.
func TestTokensAndSaid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	s := &reading{Transcript: &Transcript{Path: path}}
	text := func(s string) string {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
		return string(b)
	}
	appendLines(t, path, answerLine(text("\n  \nDone: the billing rerun passes.\nNext I'll open the PR."), 50_000))
	if !s.Read() || s.Said != "Done: the billing rerun passes." || s.Tokens != 50_000 {
		t.Fatalf("said %q, tokens %d", s.Said, s.Tokens)
	}
	appendLines(t, path, answerLine(text("Done: the billing rerun passes."), 50_400))
	if s.Read() || s.Tokens != 50_400 {
		t.Fatalf("only the tokens changed, yet changed; tokens %d", s.Tokens)
	}
	sidechain := strings.Replace(answerLine(text("A subagent's report"), 190_000), `"isSidechain":false`, `"isSidechain":true`, 1)
	synthetic := `{"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"No response requested."}],` +
		`"usage":{"input_tokens":0,"output_tokens":0}},"type":"assistant"}` + "\n"
	appendLines(t, path, answerLine(toolBlock("Bash", `{"command":"go test ./..."}`), 60_000), sidechain, synthetic,
		answerLine(text("  \n\t"), 60_000))
	if s.Read(); s.Said != "Done: the billing rerun passes." || s.Tokens != 60_000 {
		t.Fatalf("after a tool call, a subagent's text, Claude Code's note and an empty text: said %q, tokens %d", s.Said, s.Tokens)
	}
	appendLines(t, path, answerLine(text("\x1b[31mRed\x1b[0m\ttext "+strings.Repeat("é", 300)+"\nmore"), 61_000))
	if !s.Read() || s.Said != "Red text "+strings.Repeat("é", 191) {
		t.Fatalf("said %q", s.Said)
	}
	appendLines(t, path, `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary",`+
		`"compactMetadata":{"trigger":"auto","preTokens":61000,"postTokens":20000}}`+"\n")
	if s.Read(); s.Tokens != 20_000 {
		t.Fatalf("after compacting: tokens %d", s.Tokens)
	}
}

// Dirs keeps the folders of absolute paths the tool calls named, the most recent last, and nothing relative.
func TestDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendLines(t, path,
		answerLine(toolBlock("Read", `{"file_path":"/work/api/server.go"}`), 1000),
		answerLine(toolBlock("Grep", `{"pattern":"x","path":"/work/web/src/"}`), 1000),
		answerLine(toolBlock("NotebookEdit", `{"notebook_path":"/work/lab/a.ipynb"}`), 1000),
		answerLine(toolBlock("Edit", `{"file_path":"relative/x.go"}`), 1000),
		answerLine(toolBlock("Write", `{"file_path":"/work/api/client.go","content":"package api"}`), 1000))
	s := &reading{Transcript: &Transcript{Path: path}}
	if s.Read(); strings.Join(s.Dirs, ",") != "/work/web/src,/work/lab,/work/api" {
		t.Fatalf("dirs: %q", s.Dirs)
	}
	var lines []string
	for i := range maxDirs {
		lines = append(lines, answerLine(toolBlock("Read", `{"file_path":"/work/api/pkg`+strconv.Itoa(i)+`/a.go"}`), 1000))
	}
	appendLines(t, path, lines...)
	if s.Read(); len(s.Dirs) != maxDirs || slices.Contains(s.Dirs, "/work/web/src") {
		t.Fatalf("%d dirs, the oldest kept: %v", len(s.Dirs), slices.Contains(s.Dirs, "/work/web/src"))
	}
}

// A long transcript read from its start keeps its title and pull request from anywhere, and takes its tool calls and
// context from its last recentCalls bytes; read as it grows, every call counts.
func TestLongTranscriptFromItsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	early := answerLine(toolBlock("Read", `{"file_path":"/work/old/a.go","pad":"`+strings.Repeat("p", 50_000)+`"}`), 900_000)
	lines := []string{`{"type":"ai-title","aiTitle":"Rerun billing on staging","sessionId":"s"}` + "\n",
		`{"type":"pr-link","sessionId":"s","prNumber":1822,"prUrl":"https://github.com/acme/api/pull/1822"}` + "\n"}
	for range recentCalls/len(early) + 10 {
		lines = append(lines, early)
	}
	lines = append(lines, answerLine(toolBlock("Bash", `{"command":"go test ./..."}`), 300_000))
	appendLines(t, path, lines...)
	s := &reading{Transcript: &Transcript{Path: path}}
	s.Read()
	if s.Title != "Rerun billing on staging" || s.PR != 1822 || s.Doing != "Running go" || s.Context != 30 {
		t.Fatalf("title %q pr %d doing %q context %d", s.Title, s.PR, s.Doing, s.Context)
	}
	if len(s.Kinds) != 30 || !strings.HasSuffix(s.Kinds, "rx") || !slices.Contains(s.Dirs, "/work/old") {
		t.Fatalf("kinds %q, dirs %v", s.Kinds, s.Dirs)
	}
	appendLines(t, path, early, early)
	if s.Read(); !strings.HasSuffix(s.Kinds, "xrr") {
		t.Fatalf("calls added later: kinds %q", s.Kinds)
	}
}

// A transcript whose last answer came before a tool output longer than recentCalls still shows that answer, read from
// its start: its call, and the context it used. The output is in many lines, or in one too long to read.
func TestLastAnswerBeforeALongOutput(t *testing.T) {
	answer := answerLine(toolBlock("Bash", `{"command":"go test ./..."}`), 300_000)
	output := func(n int) string {
		return `{"parentUuid":"p","message":{"role":"user","content":[{"type":"tool_result","content":"` + strings.Repeat("o", n) +
			`"}]},"type":"user"}` + "\n"
	}
	many := slices.Repeat([]string{output(60_000)}, recentCalls/60_000+10)
	for name, lines := range map[string][]string{"many lines": many, "one line": {output(recentCalls + 1000)}} {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		appendLines(t, path, append([]string{answer}, lines...)...)
		s := &reading{Transcript: &Transcript{Path: path}}
		if s.Read(); s.Doing != "Running go" || s.Kinds != "x" || s.Context != 30 {
			t.Fatalf("%s: doing %q kinds %q context %d", name, s.Doing, s.Kinds, s.Context)
		}
	}
}

func TestWindowOf(t *testing.T) {
	for model, want := range map[string]int{"claude-opus-5-5": 1_000_000, "claude-fable-5-1": 1_000_000, "": 1_000_000,
		"claude-haiku-5-5": 200_000, "claude-sonnet-4-5": 200_000, "claude-sonnet-4-5[1m]": 1_000_000, "claude-3-7-sonnet": 200_000,
		"claude-opus-4-1-20250805": 200_000, "claude-opus-4": 200_000} {
		if got := windowOf(model); got != want {
			t.Errorf("%q: %d, want %d", model, got, want)
		}
	}
}
