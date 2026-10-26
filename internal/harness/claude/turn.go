package claude

import (
	"bytes"
	"slices"

	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// Where the agent's turn stands (harness.Activity.Turn), from the lines of its transcript that move it: an answer, a
// tool's result, a prompt, its person stopping it. Claude Code writes an answer down once the model has written it,
// and a prompt or a result at once: the last of them says what the model does now. A subagent's lines, Claude Code's
// own (its notes, a local command and its output, a skill's text), compaction summaries, attachments and the other
// kinds of lines don't move it.

// The ways a user line moves the turn. A line of text may be a prompt or one of Claude Code's own: its end says.
const (
	noMove      byte = iota
	promptMove       // a prompt: the model writes its reply
	resultMove       // a tool's result: the model works on it
	stopMove         // its person stopped the turn
	maybePrompt      // a line of text, which prompted reads
)

type move struct {
	kind byte
	id   string // the tool call a result answers
}

// pending is a tool call whose result hasn't come, and what it does, in Doing's words.
type pending struct{ id, doing string }

type turn struct {
	at     string    // harness.Turn*
	call   string    // the tool call it is about, in Doing's words
	calls  []pending // the last answer's tool calls whose results haven't come
	answer bool      // the last line that moved the turn is an answer's: the next answer line is of the same answer
	marked bool      // this Claude Code marks the prompts the model answers (turnOrigin)
	asked  int       // the prompts its person sent: harness.Activity.Prompts
}

// The marks of the lines that move the turn, as Claude Code writes them: in a JSON string, a quote is escaped, so
// none of these can come from what anyone wrote.
var (
	messageKey     = []byte(`"message":`)
	userType       = []byte(`"type":"user",`)
	sidechain      = []byte(`"isSidechain":true`)
	toolResult     = []byte(`"type":"tool_result"`)
	toolUseID      = []byte(`"tool_use_id":"`)
	interruption   = []byte(`:"[Request interrupted`)
	turnOrigin     = []byte(`"turnOrigin":"`)
	humanOrigin    = []byte(`"turnOrigin":"human"`)
	metaLine       = []byte(`"isMeta":true`)
	compactSummary = []byte(`"isCompactSummary":true`)
	transcriptOnly = []byte(`"isVisibleInTranscriptOnly":true`)
)

// How much of a user line's start says what it is, and how much of a long line's end prompted reads.
const (
	userHead = 1 << 10
	userTail = 4 << 10
)

// userOf reads the start of a line: whether it is a user line, and how it moves the turn. Claude Code writes a user
// line's type just before its message, with nothing nested before them, and the first block of its content next: a
// tool's result names the call it answers there. An answer's line starts with its message, which names no user.
func userOf(line []byte) (move, bool) {
	head := line[:min(len(line), userHead)]
	i := bytes.Index(head, messageKey)
	if i < 1 || !bytes.HasSuffix(head[:i], userType) || bytes.IndexByte(head[1:i], '{') >= 0 {
		return move{}, false
	}
	content := head[i:]
	switch {
	case bytes.Contains(head[:i], sidechain):
		return move{}, true // a subagent's
	case bytes.Contains(content, toolUseID) || bytes.Contains(content, toolResult):
		m := move{kind: resultMove}
		if _, id, ok := bytes.Cut(content, toolUseID); ok {
			if j := bytes.IndexByte(id, '"'); j >= 0 {
				m.id = string(id[:j])
			}
		}
		return m, true
	case bytes.Contains(content, interruption):
		return move{kind: stopMove}, true
	}
	return move{kind: maybePrompt}, true
}

// prompted reads the end of a line of text, where Claude Code says what it is. The prompt the model answers says where
// it came from (turnOrigin: its person, a message handed to the session, a task's notification); the lines the model
// isn't asked to answer don't (a local command and its output, a skill's text, a caveat). Before Claude Code marked
// prompts, a prompt was any line of text but its own (isMeta), and any may be its person's. A compaction's summary is
// never one.
func (t *turn) prompted(end []byte) move {
	switch {
	case bytes.Contains(end, compactSummary) || bytes.Contains(end, transcriptOnly):
		return move{}
	case bytes.Contains(end, turnOrigin):
		t.marked = true
		if bytes.Contains(end, humanOrigin) {
			t.asked++
		}
		return move{kind: promptMove}
	case t.marked || bytes.Contains(end, metaLine):
		return move{}
	}
	t.asked++
	return move{kind: promptMove}
}

// move moves the turn with a user line. A result takes its call off those still running; one whose call is unknown
// (an answer too long to read made it) names none.
func (t *turn) move(m move) {
	switch m.kind {
	case promptMove:
		t.at, t.call, t.calls, t.answer = harness.TurnReply, "", t.calls[:0], false
	case stopMove:
		t.at, t.call, t.calls, t.answer = harness.TurnEnded, "", t.calls[:0], false
	case resultMove:
		t.answer, t.call = false, ""
		if i := slices.IndexFunc(t.calls, func(c pending) bool { return c.id == m.id }); i >= 0 {
			t.call = t.calls[i].doing // before Delete clears it
			t.calls = slices.Delete(t.calls, i, i+1)
		} else if m.id == "" {
			t.calls = t.calls[:0]
		}
		t.at = harness.TurnResult
		if len(t.calls) > 0 { // the answer called tools side by side: the model waits for them all
			t.at, t.call = harness.TurnTool, t.calls[len(t.calls)-1].doing
		}
	}
}

// answering starts reading a line of an answer of the main conversation: the first line of a new answer comes once
// every result the last one waited for has come.
func (t *turn) answering() {
	if !t.answer {
		t.calls, t.answer = t.calls[:0], true
	}
}

// maxCalls is how many calls of an answer the turn waits for at most: more would be the calls of many answers, in a
// transcript whose results it can't read.
const maxCalls = 64

// called adds a call of the answer being read to those waiting for their result.
func (t *turn) called(id, doing string) {
	if len(t.calls) == maxCalls {
		t.calls = slices.Delete(t.calls, 0, 1)
	}
	t.calls = append(t.calls, pending{id, doing})
}

// answered moves the turn with a line of an answer, whose tool calls answering's caller added to calls. A line of an
// answer that calls no tool and doesn't say it ended the turn, such as the thinking before a call, leaves the turn
// where it was.
func (t *turn) answered(ended bool) {
	switch {
	case len(t.calls) > 0:
		t.at, t.call = harness.TurnTool, t.calls[len(t.calls)-1].doing
	case ended:
		t.at, t.call = harness.TurnEnded, ""
	}
}
