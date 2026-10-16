package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/protocol"
)

var assistantLine = []byte(`"type":"assistant"`)

// transcriptTypes are the short lines read for what they say, found near their start: titles, PR links, the model
// the conversation runs on, and compactions.
var transcriptTypes = [][]byte{[]byte(`"type":"ai-title"`), []byte(`"type":"custom-title"`), []byte(`"type":"pr-link"`),
	[]byte(`"type":"model"`), []byte(`"subtype":"compact_boundary"`)}

// Transcript is what murmur reads from one conversation's transcript, as it grows.
type Transcript struct {
	Path    string
	Offset  int64
	AITitle string // the title Claude Code gives the conversation
	Custom  string // the title its person set, which wins
	PR      int
	PRURL   string
	Doing   string   // the last tool call in a few words
	Kinds   string   // the kinds of the last tool calls, oldest first, one letter each (see protocol.Kind*)
	Loop    bool     // the same command in most of the last tool calls
	Context int      // how full the context window is, in percent
	Dirs    []string // folders the tool calls named by absolute path, the most recent last

	recent []uint64 // the last tool calls, for Loop: a fingerprint of a Bash command, 0 for any other tool
	tokens int      // the context the latest answer used
	model  string   // the model, as Claude Code names it to the model itself
	large  bool     // the context window is known to hold a million tokens
}

// Title is the conversation's title: its person's, or else Claude Code's.
func (t *Transcript) Title() string {
	if t.Custom != "" {
		return t.Custom
	}
	return t.AITitle
}

// Read reads what was appended since the last call. Titles and PR links are short lines that start with
// their type; tool calls and context sizes are assistant lines. Long lines (tool output, file contents) are skipped
// without being held in memory.
func (s *Transcript) Read() bool {
	if s.Path == "" {
		return false
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() < s.Offset {
		s.Offset = 0 // rewritten from scratch
	}
	if _, err := f.Seek(s.Offset, io.SeekStart); err != nil {
		return false
	}
	r := bufio.NewReaderSize(f, 64<<10)
	changed := false
	pos := s.Offset
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
		if bytes.Contains(line, assistantLine) {
			changed = s.assistant(line) || changed
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
			changed = changed || s.AITitle != e.AITitle
			s.AITitle = e.AITitle
		case "custom-title":
			t := e.CustomTitle
			if t == "" {
				t = e.Title
			}
			changed = changed || s.Custom != t
			s.Custom = t
		case "pr-link":
			if e.PRNumber != 0 {
				changed = changed || s.PR != e.PRNumber
				s.PR, s.PRURL = e.PRNumber, e.PRURL
			}
		case "attachment": // Claude Code tells the model which it is, again when its person switches
			if id := e.Attachment.Identity.ModelID; e.Attachment.Type == "model" && id != "" && id != s.model {
				s.model, s.large = id, false
				changed = s.context(s.tokens) || changed
			}
		case "system":
			if e.Subtype == "compact_boundary" && e.Compact.PostTokens > 0 {
				s.large = s.large || e.Compact.PreTokens > smallWindow
				changed = s.context(e.Compact.PostTokens) || changed
			}
		}
	}
	s.Offset = pos
	return changed
}

const (
	maxKinds = 30  // tool calls in Kinds
	loopOf   = 12  // Loop looks at the last loopOf tool calls
	loopMin  = 9   // and needs one command in at least loopMin of them
	maxDirs  = 200 // folders in Dirs: an early look into another repo is forgotten after this many others
)

// assistant reads an assistant line: how full the context window is, and its tool calls. Claude Code writes one line
// per block of an answer, each with the answer's usage.
func (s *Transcript) assistant(line []byte) bool {
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
		changed = s.context(u.Input + u.CacheRead + u.CacheCreation)
	}
	for _, c := range e.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		s.Doing = doingOf(c.Name, c.Input)
		s.called(c.Name, c.Input)
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
func (s *Transcript) context(tokens int) bool {
	s.tokens = tokens
	s.large = s.large || tokens > smallWindow
	window := windowOf(s.model)
	if s.large {
		window = largeWindow
	}
	pct := min((tokens*100+window/2)/window, 100)
	if pct == s.Context {
		return false
	}
	s.Context = pct
	return true
}

// called counts one tool call: its kind, its command for the loop hint, and the folder it names. Commands are kept
// only as fingerprints.
func (s *Transcript) called(tool string, input json.RawMessage) {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(input, &in)
	if k := kindOf(tool); k != 0 {
		s.Kinds += string(k)
		s.Kinds = s.Kinds[max(len(s.Kinds)-maxKinds, 0):]
	}
	var cmd uint64
	if tool == "Bash" {
		h := fnv.New64a()
		_, _ = h.Write([]byte(in.Command))
		cmd = h.Sum64() | 1 // never 0, which stands for every other tool
	}
	s.recent = append(s.recent, cmd)
	s.recent = s.recent[max(len(s.recent)-loopOf, 0):]
	s.Loop = looping(s.recent)
	for _, dir := range []string{filepath.Dir(in.FilePath), filepath.Dir(in.NotebookPath), filepath.Clean(in.Path)} {
		if filepath.IsAbs(dir) {
			s.touch(dir)
		}
	}
}

// touch moves dir to the end of Dirs, which keeps the maxDirs folders named most recently.
func (s *Transcript) touch(dir string) {
	if i := slices.Index(s.Dirs, dir); i >= 0 {
		s.Dirs = slices.Delete(s.Dirs, i, i+1)
	} else if len(s.Dirs) >= maxDirs {
		s.Dirs = slices.Delete(s.Dirs, 0, 1)
	}
	s.Dirs = append(s.Dirs, dir)
}

// looping says whether one command makes up at least loopMin of these calls.
func looping(calls []uint64) bool {
	n := map[uint64]int{}
	for _, c := range calls {
		if c != 0 {
			if n[c]++; n[c] >= loopMin {
				return true
			}
		}
	}
	return false
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
		for _, f := range strings.Fields(in.Command) {
			if !strings.Contains(f, "=") { // skip VAR=value before the command
				return "Running " + filepath.Base(f)
			}
		}
		return "Running a command"
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
