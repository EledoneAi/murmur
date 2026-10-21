package claude

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// The lines of a conversation as Claude Code 2.1 writes them, for where the turn stands: a user line's type comes
// before its message, an answer's after it, and what Claude Code says about a line comes after its message.

const lineEnd = `,"userType":"external","entrypoint":"cli","cwd":"/work/api","sessionId":"s","version":"2.1.292","gitBranch":"main"}` + "\n"

// answers is a line of an answer: one of its blocks, with the answer's stop reason ("" for a line written before
// the answer ended).
func answers(block, stop string) string {
	reason := "null"
	if stop != "" {
		reason = `"` + stop + `"`
	}
	return `{"parentUuid":"p","isSidechain":false,"message":{"model":"claude-opus-5-5","id":"msg_1","type":"message",` +
		`"role":"assistant","content":[` + block + `],"stop_reason":` + reason + `,"usage":{"input_tokens":2,` +
		`"cache_read_input_tokens":40000,"output_tokens":9}},"requestId":"req_1","type":"assistant","uuid":"a",` +
		`"timestamp":"2026-10-20T14:48:12.903Z"` + lineEnd
}

func calls(id, name, input string) string {
	return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}`
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// asked is a user line whose content is text, with what Claude Code writes after its message.
func asked(content, after string) string {
	return `{"parentUuid":"p","isSidechain":false,"promptId":"pr","type":"user","message":{"role":"user","content":` +
		quoted(content) + `}` + after + `,"uuid":"u","timestamp":"2026-10-20T14:48:10.581Z"` + lineEnd
}

// prompt is its person's prompt; pushed, a message handed to the session, as Claude Code writes the one murmur hands
// it.
const (
	prompt = `,"permissionMode":"default","promptSource":"cli","turnOrigin":"human","turnPosition":{"promptIndex":0,"turnIndex":2}`
	pushed = `,"isMeta":true,"permissionMode":"default","origin":{"kind":"peer","from":"unknown"},"promptSource":"system",` +
		`"turnOrigin":"peer","turnPosition":{"promptIndex":0,"turnIndex":4},"queueSkipAttachments":true`
)

// returns is a tool's result.
func returns(id, output string) string {
	return `{"parentUuid":"p","isSidechain":false,"promptId":"pr","type":"user","message":{"role":"user","content":[` +
		`{"tool_use_id":"` + id + `","type":"tool_result","content":` + quoted(output) + `}]},"uuid":"u",` +
		`"timestamp":"2026-10-20T14:48:12.913Z","toolUseResult":{"stdout":"ok"},"sourceToolAssistantUUID":"a"` + lineEnd
}

func subagents(line string) string {
	return strings.Replace(line, `"isSidechain":false`, `"isSidechain":true,"agentId":"a1b2"`, 1)
}

// Where the turn stands follows the last line that moves it: a tool call waits for its result (all of them, for calls
// side by side), a result is worked on, a prompt (its person's, or a message handed to the session) is being replied
// to, an answer that ends the turn ends it, and so does its person stopping it. A subagent's lines, Claude Code's own,
// compaction summaries, attachments and snapshots move nothing, nor does a line of an answer that hasn't ended. The
// turn moving is no change: nothing shows it.
func TestTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	s := &reading{Transcript: &Transcript{Path: path}}
	big := strings.Repeat("x", 200<<10)
	for _, step := range []struct {
		what       string
		lines      []string
		turn, call string
		quiet      bool // Read says nothing changed
	}{
		{"its person's prompt", []string{asked("Write the design doc for per-user rate limits", prompt)},
			harness.TurnReply, "", true},
		{"the thinking before a call", []string{answers(`{"type":"thinking","thinking":"","signature":"x"}`, "tool_use")},
			harness.TurnReply, "", false},
		{"a command", []string{answers(calls("toolu_1", "Bash", `{"command":"go test ./..."}`), "tool_use")},
			harness.TurnTool, "Running go", false},
		{"its output", []string{returns("toolu_1", "ok  \tgithub.com/acme/api\t1.2s")}, harness.TurnResult, "Running go", true},
		{"two reads side by side", []string{answers(calls("toolu_2", "Read", `{"file_path":"/work/api/a.go"}`), "tool_use"),
			answers(calls("toolu_3", "Read", `{"file_path":"/work/api/b.go"}`), "tool_use")}, harness.TurnTool, "Reading b.go", false},
		{"the second read's result first", []string{returns("toolu_3", "package api")}, harness.TurnTool, "Reading a.go", true},
		{"then the first's", []string{returns("toolu_2", "package api")}, harness.TurnResult, "Reading a.go", true},
		{"a subagent's prompt, call and result", []string{subagents(asked("Audit the limiter", prompt)),
			subagents(answers(calls("toolu_s", "Grep", `{"pattern":"limit"}`), "tool_use")), subagents(returns("toolu_s", "x.go"))},
			harness.TurnResult, "Reading a.go", false},
		{"Claude Code's own lines", []string{
			`{"parentUuid":"p","isSidechain":false,"attachment":{"type":"queued_command","prompt":"and the docs"},"type":"attachment","uuid":"u"}` + "\n",
			`{"type":"file-history-snapshot","messageId":"m","snapshot":{"messageId":"m","trackedFileBackups":{}},"isSnapshotUpdate":false}` + "\n",
			asked("Base directory for this skill: /work/.claude/skills/review", `,"isMeta":true`),
			`{"parentUuid":"p","isSidechain":false,"type":"system","subtype":"stop_hook_summary","content":"","level":"info"}` + "\n",
			`{"parentUuid":"p","isSidechain":false,"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text",` +
				`"text":"No response requested."}],"stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0}},"type":"assistant"}` + "\n",
			`{"type":"last-prompt","lastPrompt":"Write the design doc","sessionId":"s"}` + "\n"},
			harness.TurnResult, "Reading a.go", true},
		{"its answer", []string{answers(`{"type":"thinking","thinking":"","signature":"x"}`, "end_turn"),
			answers(`{"type":"text","text":"The doc is in docs/limits.md."}`, "end_turn")}, harness.TurnEnded, "", false},
		{"a local command and its output", []string{asked("<command-name>/model</command-name>\n<command-message>model</command-message>", ""),
			asked("<local-command-stdout>Set model to opus</local-command-stdout>", "")}, harness.TurnEnded, "", true},
		{"a message handed to the session", []string{asked("Another Claude session sent a message:\n[murmur] bob/api-9a: are admin keys exempt?", pushed)},
			harness.TurnReply, "", true},
		{"a write", []string{answers(calls("toolu_4", "Write", `{"file_path":"/work/api/docs/limits.md","content":"# Limits"}`), "tool_use")},
			harness.TurnTool, "Writing limits.md", false},
		{"its person stops it", []string{returns("toolu_4", "[Request interrupted by user for tool use]"),
			`{"parentUuid":"p","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":` +
				`"[Request interrupted by user for tool use]"}]},"uuid":"u","timestamp":"2026-10-20T14:49:00Z"}` + "\n"},
			harness.TurnEnded, "", true},
		{"a search, its result, and a compaction", []string{asked("Go on", prompt), answers(calls("toolu_5", "Grep", `{"pattern":"limit"}`), "tool_use"),
			returns("toolu_5", "limiter.go"),
			`{"parentUuid":null,"logicalParentUuid":"l","isSidechain":false,"type":"system","subtype":"compact_boundary",` +
				`"content":"Conversation compacted","compactMetadata":{"trigger":"auto","preTokens":400000,"postTokens":20000}}` + "\n",
			asked("This session is being continued from a previous conversation that ran out of context.",
				`,"isVisibleInTranscriptOnly":true,"isCompactSummary":true`)},
			harness.TurnResult, "Searching", false},
		{"a long output", []string{answers(calls("toolu_6", "Read", `{"file_path":"/work/api/big.go"}`), "tool_use"), returns("toolu_6", big)},
			harness.TurnResult, "Reading big.go", false},
		{"a long prompt", []string{answers(`{"type":"text","text":"Done."}`, "end_turn"), asked("Look at this log:\n"+big, prompt)},
			harness.TurnReply, "", false},
	} {
		appendLines(t, path, step.lines...)
		changed := s.Read()
		if s.Turn != step.turn || s.TurnCall != step.call {
			t.Fatalf("%s: turn %q on %q, want %q on %q", step.what, s.Turn, s.TurnCall, step.turn, step.call)
		}
		if step.quiet && changed {
			t.Errorf("%s: changed", step.what)
		}
	}
}

// A Claude Code that doesn't mark its prompts with where they came from: any user line of text is one, but its own
// lines and compaction summaries.
func TestTurnUnmarked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	s := &reading{Transcript: &Transcript{Path: path}}
	appendLines(t, path, answers(`{"type":"text","text":"Done."}`, "end_turn"), asked("Caveat: The messages below were generated by the user", `,"isMeta":true`),
		asked("This session is being continued from a previous conversation.", `,"isCompactSummary":true`))
	if s.Read(); s.Turn != harness.TurnEnded {
		t.Fatalf("after Claude Code's own lines: %q", s.Turn)
	}
	appendLines(t, path, asked("Now the docs", ""))
	if s.Read(); s.Turn != harness.TurnReply {
		t.Fatalf("after a prompt: %q", s.Turn)
	}
}

// A long transcript read from its start takes its last answer from before its last recentCalls bytes: the results
// after that answer still count, as they came after it.
func TestTurnFromALongTranscript(t *testing.T) {
	pad := `{"type":"file-history-snapshot","messageId":"m","snapshot":{"pad":"` + strings.Repeat("p", 60_000) + `"}}` + "\n"
	padding := make([]string, recentCalls/len(pad)+2)
	for i := range padding {
		padding[i] = pad
	}
	read, grep := calls("toolu_1", "Read", `{"file_path":"/work/api/a.go"}`), calls("toolu_2", "Grep", `{"pattern":"limit"}`)
	for _, c := range []struct {
		what       string
		lines      []string
		turn, call string
	}{
		{"a result", []string{asked("Go on", prompt), answers(read, "tool_use"), returns("toolu_1", "package api")},
			harness.TurnResult, "Reading a.go"},
		{"one result of two", []string{asked("Go on", prompt), answers(read, "tool_use"), answers(grep, "tool_use"),
			returns("toolu_1", "package api")}, harness.TurnTool, "Searching"},
		{"no result yet", []string{asked("Go on", prompt), answers(grep, "tool_use")}, harness.TurnTool, "Searching"},
	} {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		appendLines(t, path, append(c.lines, padding...)...)
		s := &reading{Transcript: &Transcript{Path: path}}
		if s.Read(); s.Turn != c.turn || s.TurnCall != c.call {
			t.Errorf("%s: turn %q on %q, want %q on %q", c.what, s.Turn, s.TurnCall, c.turn, c.call)
		}
	}
}
