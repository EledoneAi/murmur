package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestClean(t *testing.T) {
	for _, c := range []struct {
		in    string
		lines bool
		want  string
	}{
		{"Fix the flaky test", false, "Fix the flaky test"},
		{"\x1b]52;c;cm0gLXJmIH4=\x07Deploy \x1b[31mnow\x1b[0m", false, "Deploy now"}, // clipboard write, colors
		{"\x1b]8;;https://evil.example\x1b\\docs\x1b]8;;\x1b\\", false, "docs"},      // hidden link
		{"bell\a, backspace\b, C1 \u009b2J, bidi \u202egnp.exe", false, "bell, backspace, C1 , bidi gnp.exe"},
		{"title\x1b]0;x\u009c and the rest\nline 2", true, "title and the rest\nline 2"},                                  // OSC ended by an 8-bit ST
		{"before \x1b]8;;http://x\nline2\nline3", true, "before \nline2\nline3"},                                          // unterminated: only its line
		{"file\u200e.go \u200f\u061c ma\u200bin \u2060\ufeffad\u00admin", false, "file\u200e.go \u200f\u061c main admin"}, // marks stay; zero-width, BOM, soft hyphen go
		{"fix\U000E0049\U000E0047\U000E004E\U000E004F\U000E0052\U000E0045 tests", false, "fix tests"},                     // tag characters
		{"one\u2028two\u2029three", true, "one\ntwo\nthree"},
		{"one\u2028two", false, "one two"},
		{"\U0001F469\u200d\U0001F4BB \u2764\ufe0f", false, "\U0001F469\u200d\U0001F4BB \u2764\ufe0f"},                   // emoji stay whole
		{"ok\ufe00\ufe01\ufe02\ufe03\ufe04\ufe05\ufe06\ufe07", false, "ok\ufe00"},                                       // a run of selectors can carry text
		{"\u845b\U000E0100 \u2764\ufe0f", false, "\u845b\U000E0100 \u2764\ufe0f"},                                       // one selector after a character stays
		{"\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645", false, "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645"}, // Persian needs the non-joiner
		{"\u0600123 a\u3164b", false, "\u0600123 ab"},                                                                   // Arabic number sign stays, Hangul filler goes
		{"build ok\x1b]0;title\u2028do not merge\nnext", true, "build ok\ndo not merge\nnext"},                          // an OSC ends at any line break
		{"a\x1b]0;t\rb", false, "ab"},
		{"first line\n\tsecond\r", true, "first line\n\tsecond"},
		{"one\nline\tfield", false, "one line field"},
	} {
		if got := Clean(c.in, c.lines); got != c.want {
			t.Errorf("Clean(%q, %v) = %q, want %q", c.in, c.lines, got, c.want)
		}
	}
}

// A message's footer has its agent answer the sender with send. A late answer by the way names as its sender the agent
// asked, which never saw the question: it gets no such footer, and says itself how to follow up.
func TestPromptLateBtw(t *testing.T) {
	m := Msg{ID: "m1", From: "bob/api-9a", Text: "Is the invoicer live?"}
	if p := Prompt(m); !strings.HasSuffix(p, "\n\n(From bob's agent api-9a, through murmur. Answer with the murmur `send` tool, to \"bob/api-9a\".)") {
		t.Fatalf("a message: %q", p)
	}
	m.ID, m.Text = LateBtw+"q1", "Answer to your question by the way (\"live?\"):\nYes.\n\n(From a copy of bob/api-9a's conversation.)"
	if p := Prompt(m); p != "[murmur] bob/api-9a: Answer to your question by the way (\"live?\"):\nYes.\n\n(From a copy of bob/api-9a's conversation.)" {
		t.Fatalf("a late answer: %q", p)
	}
}

// A topic and a last message change what the footer asks of the agent: keep the topic, answer only if it needs more.
func TestPromptTopicAndLast(t *testing.T) {
	m := Msg{ID: "m1", From: "bob/api-9a", Text: "Freeze lifted.", Topic: "Release freeze", Last: true}
	want := "\n\n(From bob's agent api-9a, through murmur, on \"Release freeze\". It is the sender's last message on this: they expect " +
		"no answer. If you need more, answer with the murmur `send` tool, to \"bob/api-9a\", topic \"Release freeze\" unless the subject moved.)"
	if p := Prompt(m); !strings.HasSuffix(p, want) {
		t.Fatalf("%q", p)
	}
}

// Two agents are in touch until an hour after their latest message, or at once when it was its sender's last; a
// reply after a last message brings them back.
func TestInTouch(t *testing.T) {
	now := time.UnixMilli(10 * 3600_000)
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	in := InTouch([]Msg{
		{From: "a/1", To: "b/1", At: at(50 * time.Minute)},
		{From: "b/1", To: "a/1", At: at(40 * time.Minute), Last: true}, // settled
		{From: "a/1", To: "c/1", At: at(30 * time.Minute), Last: true},
		{From: "c/1", To: "a/1", At: at(20 * time.Minute)}, // asked again
		{From: "a/1", To: "d/1", At: at(61 * time.Minute)}, // went quiet
		{From: "e/1", To: "a/1", At: at(5 * time.Minute)},
	}, now)
	if len(in) != 2 || !in[[2]string{"a/1", "c/1"}] || !in[[2]string{"a/1", "e/1"}] {
		t.Fatalf("in touch: %v", in)
	}
}
