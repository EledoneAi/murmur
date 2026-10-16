package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

var assistantLine = []byte(`"type":"assistant"`)

// transcriptTypes are the short lines read for what they say, found near their start: titles, PR links, the model
// the conversation runs on, and compactions.
var transcriptTypes = [][]byte{[]byte(`"type":"ai-title"`), []byte(`"type":"custom-title"`), []byte(`"type":"pr-link"`),
	[]byte(`"type":"model"`), []byte(`"subtype":"compact_boundary"`)}

// readers keeps the buffers transcripts are read through: each session's is read again at every line Claude Code adds.
var readers = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 64<<10) }}

// recentCalls is how much of a long transcript's end its tool calls are read from, when it is read from its start,
// with the last answer before it.
// ponytail: Kinds keeps 30 calls and the loop hint 12, which 4 MB holds unless the answers are huge; the folders
// the calls named (Dirs, for the repos a session touched) lose what came before.
const recentCalls = 4 << 20

// Transcript reads one conversation's transcript as it grows.
type Transcript struct {
	Path   string
	Offset int64

	aiTitle string // the title Claude Code gives the conversation
	custom  string // the title its person set, which wins
	tokens  int    // the context the latest answer used
	model   string // the model, as Claude Code names it to the model itself
	large   bool   // the context window is known to hold a million tokens
}

// title is the conversation's title: its person's, or else Claude Code's.
func (t *Transcript) title() string {
	if t.custom != "" {
		return t.custom
	}
	return t.aiTitle
}

// Read reads what was appended since the last call into a. Titles and PR links are short lines that start with
// their type; tool calls and context sizes are assistant lines. Long lines (tool output, file contents) are skipped
// without being held in memory.
func (t *Transcript) Read(a *harness.Activity) bool {
	if t.Path == "" {
		return false
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < t.Offset {
		t.Offset = 0 // rewritten from scratch
	}
	if _, err := f.Seek(t.Offset, io.SeekStart); err != nil {
		return false
	}
	// read from its start, a long conversation's tool calls and context sizes are read from its last recentCalls
	// bytes only: decoding its answers was most of the cost, and the newest calls are what Kinds and Doing keep
	callsFrom := int64(0)
	if t.Offset == 0 && st != nil {
		callsFrom = st.Size() - recentCalls
	}
	r := readers.Get().(*bufio.Reader)
	r.Reset(f)
	defer func() {
		r.Reset(nil)
		readers.Put(r)
	}()
	changed := false
	pos := t.Offset
	// the last answer before callsFrom, decoded when the read gets past it: a long tool output can fill the window,
	// and the session would show no activity and no context until its next answer
	var before []byte
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			n := int64(len(line))
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				n += int64(len(line))
			}
			if err != nil {
				break // a long line still being written: read it again next time
			}
			pos += n
			continue
		}
		if err != nil {
			break // EOF, possibly mid-line: that line is read again next time
		}
		pos += int64(len(line))
		if len(before) > 0 && pos > callsFrom {
			changed = t.assistant(before, a) || changed
			before = before[:0]
		}
		if bytes.Contains(line, assistantLine) {
			if pos > callsFrom {
				changed = t.assistant(line, a) || changed
			} else {
				before = append(before[:0], line...)
			}
			continue
		}
		head := line[:min(len(line), 200)]
		if !slices.ContainsFunc(transcriptTypes, func(t []byte) bool { return bytes.Contains(head, t) }) {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			Subtype     string `json:"subtype"`
			AITitle     string `json:"aiTitle"`
			CustomTitle string `json:"customTitle"`
			Title       string `json:"title"`
			PRNumber    int    `json:"prNumber"`
			PRURL       string `json:"prUrl"`
			Attachment  struct {
				Type     string `json:"type"`
				Identity struct {
					ModelID string `json:"modelId"`
				} `json:"identity"`
			} `json:"attachment"`
			Compact struct {
				PreTokens  int `json:"preTokens"`
				PostTokens int `json:"postTokens"`
			} `json:"compactMetadata"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "ai-title":
			t.aiTitle = e.AITitle
		case "custom-title":
			t.custom = e.CustomTitle
			if t.custom == "" {
				t.custom = e.Title
			}
		case "pr-link":
			if e.PRNumber != 0 {
				changed = changed || a.PR != e.PRNumber
				a.PR, a.PRURL = e.PRNumber, e.PRURL
			}
		case "attachment": // Claude Code tells the model which it is, again when its person switches
			if id := e.Attachment.Identity.ModelID; e.Attachment.Type == "model" && id != "" && id != t.model {
				t.model, t.large = id, false
				changed = t.context(a, t.tokens) || changed
			}
		case "system":
			if e.Subtype == "compact_boundary" && e.Compact.PostTokens > 0 {
				t.large = t.large || e.Compact.PreTokens > smallWindow
				changed = t.context(a, e.Compact.PostTokens) || changed
			}
		}
		if title := t.title(); a.Title != title {
			a.Title, changed = title, true
		}
	}
	if len(before) > 0 { // nothing but lines too long to read came after it
		changed = t.assistant(before, a) || changed
	}
	t.Offset = pos
	return changed
}

// assistant reads an assistant line: how full the context window is, and its tool calls. Claude Code writes one line
// per block of an answer, each with the answer's usage.
func (t *Transcript) assistant(line []byte, a *harness.Activity) bool {
	var e struct {
		Type      string    `json:"type"`
		Sidechain bool      `json:"isSidechain"`
		Timestamp time.Time `json:"timestamp"`
		Message   struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Usage struct {
				Input         int `json:"input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
		return false
	}
	changed := false
	// a subagent's answers measure its own context; Claude Code's own notes (an API error) carry no usage
	if u := e.Message.Usage; !e.Sidechain && u.Input+u.CacheRead+u.CacheCreation > 0 {
		changed = t.context(a, u.Input+u.CacheRead+u.CacheCreation)
	}
	for _, c := range e.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		a.Called(callOf(c.Name, c.Input))
		changed = true
	}
	return changed
}

// The context window holds a million tokens, as current models' do, unless the model is a Haiku or from before the
// million-token windows (Claude 3, the 4.x family) and Claude Code doesn't name it with "[1m]". A conversation that
// went past 200k has a million whatever its name says.
// ponytail: a pattern on model ids; a model with a small window and a new name reads too empty until it is added.
const (
	smallWindow = 200_000
	largeWindow = 1_000_000
)

var smallModels = regexp.MustCompile(`haiku|claude-3|claude-[a-z]+-4([-.]|$)`)

func windowOf(model string) int {
	model = strings.ToLower(model)
	if smallModels.MatchString(model) && !strings.HasSuffix(model, "[1m]") {
		return smallWindow
	}
	return largeWindow
}

// context takes the tokens the conversation now uses, and says whether its percentage changed.
func (t *Transcript) context(a *harness.Activity, tokens int) bool {
	t.tokens = tokens
	t.large = t.large || tokens > smallWindow
	window := windowOf(t.model)
	if t.large {
		window = largeWindow
	}
	pct := min((tokens*100+window/2)/window, 100)
	if pct == a.Context {
		return false
	}
	a.Context = pct
	return true
}

// callOf reads one of Claude Code's tool calls. Commands are kept only as fingerprints.
func callOf(tool string, input json.RawMessage) harness.Call {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(input, &in)
	c := harness.Call{Kind: kindOf(tool), Doing: doingOf(tool, input)}
	if tool == "Bash" {
		c.Shell = harness.Shell(in.Command)
	}
	c.Dirs = []string{filepath.Dir(in.FilePath), filepath.Dir(in.NotebookPath), filepath.Clean(in.Path)}
	return c
}

// kindOf is the letter a tool call shows as in Kinds, or 0 for tools Kinds leaves out (plans, other MCP servers).
func kindOf(tool string) byte {
	switch tool {
	case "Read", "Grep", "Glob", "LS", "NotebookRead", "LSP":
		return protocol.KindRead
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return protocol.KindEdit
	case "Bash":
		return protocol.KindRun
	case "WebFetch", "WebSearch":
		return protocol.KindWeb
	case "Task", "Agent":
		return protocol.KindSubagent
	case "mcp__murmur__send":
		return protocol.KindMessage
	}
	return 0
}

// doingOf says what a tool call does in a few words: the tool, and the file it works on or the command it runs.
// Nothing the model wrote leaves the laptop (command descriptions, search patterns, to-do text, web addresses), and
// never a command line or file contents.
func doingOf(tool string, input json.RawMessage) string {
	var in struct {
		Command, Skill string
		FilePath       string `json:"file_path"`
		NotebookPath   string `json:"notebook_path"`
	}
	_ = json.Unmarshal(input, &in)
	file := filepath.Base(in.FilePath + in.NotebookPath)
	switch tool {
	case "Bash":
		return harness.Running(in.Command)
	case "Read":
		return "Reading " + file
	case "Edit", "MultiEdit", "NotebookEdit":
		return "Editing " + file
	case "Write":
		return "Writing " + file
	case "Grep", "Glob":
		return "Searching"
	case "Agent", "Task":
		return "Delegating"
	case "WebSearch":
		return "Searching the web"
	case "WebFetch":
		return "Reading a web page"
	case "Skill":
		return "Using " + in.Skill
	case "TodoWrite":
		return "Updating its plan"
	}
	if srv, name, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__"); ok && strings.HasPrefix(tool, "mcp__") {
		return strings.TrimPrefix(srv, "claude_ai_") + " · " + name
	}
	return tool
}
