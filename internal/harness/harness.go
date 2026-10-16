// Package harness is what murmur needs from an AI coding tool on a laptop, a harness such as Claude Code: where its
// sessions are, what they do, how to reach them, and the hooks and MCP server that join sets up in it. Each harness
// has an adapter in a package of its own; the daemon, setup and the MCP server only see this interface.
package harness

import (
	"errors"
	"hash/fnv"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/EledoneAi/murmur/internal/protocol"
)

// Harness is one AI coding tool.
type Harness interface {
	Name() string    // "claude", "codex"…: in hook commands, session keys and the agent's App tag
	Title() string   // "Claude Code", "Codex"…: what people read
	Installed() bool // its command or its configuration folder is on this laptop
	Connected() bool // murmur's hooks are in its settings, as join left them
	// Connect adds murmur's hooks, MCP server and tool permissions, for join and after an update. ask puts a question
	// to the person at the terminal, and is nil when nobody is there.
	Connect(exe string, ask func(question string) bool) []protocol.Check
	Disconnect(exe string) error // undoes exactly what Connect added
	Checks() []protocol.Check    // what Connect set up, for doctor and the laptop's report
	// Parse reads what a hook gets on stdin, for the event its command names. It runs in the hook's own process, so it
	// may read the environment the harness gives its hooks.
	Parse(event string, stdin []byte) (Event, error)
	// Answer is what the hook prints for murmur's text: the brief on start, a refusal on before-tool, messages waiting
	// for the session on after-tool, stop or prompt. Empty text prints nothing.
	Answer(ev Event, text string) []byte
}

// The optional capabilities, found by type assertion.

// Lister is a harness that keeps its own registry of live sessions (Claude Code). The daemon lists its sessions again
// whenever something in Watch changes. A session whose entry is being written comes with its PID only: the daemon
// keeps what it had for it.
type Lister interface {
	Watch() string
	Sessions() []Session
}

// Pusher is a harness murmur can hand a message to while its session runs (Claude Code's inbox socket).
type Pusher interface {
	Push(s Session, text string) error
}

// Reader is a harness whose transcripts are worth reading for the title, pull requests, tool calls and context use
// (Claude Code). Open finds session s's transcript and returns its path, "" until the harness has written it, and a
// function that reads what was added since its last call into a, saying whether a changed.
type Reader interface {
	Open(s Session) (path string, read func(a *Activity) bool)
}

// Event kinds: what a hook reports, in murmur's words.
const (
	Start      = "start"       // a session starts, or resumes: murmur briefs it
	Prompt     = "prompt"      // its person sent a prompt: it works
	BeforeTool = "before-tool" // it is about to call a tool: murmur may turn an edit back
	AfterTool  = "after-tool"  // a tool call finished
	Stop       = "stop"        // its turn ended: it waits for its person
	Needs      = "needs"       // it waits for its person's approval or answer
	End        = "end"         // the session closed
)

// Event is one hook call, as the hook's process reads it and hands it to the daemon.
type Event struct {
	Kind       string            `json:"kind"`
	Harness    string            `json:"harness"`
	SessionID  string            `json:"session"`
	PID        int               `json:"pid,omitempty"` // the harness's process, which runs the session
	Cwd        string            `json:"cwd,omitempty"`
	Transcript string            `json:"transcript,omitempty"`
	Socket     string            `json:"socket,omitempty"` // where murmur pushes messages to the session, if the harness has one
	Term       map[string]string `json:"term,omitempty"`   // where the session runs, on start
	Call       *Call             `json:"call,omitempty"`   // the tool call, on before-tool
	Paths      []string          `json:"paths,omitempty"`  // the files an edit would change, absolute, on before-tool
	Needs      string            `json:"needs,omitempty"`  // permission or input, on needs
	Version    string            `json:"version,omitempty"`
}

// Key names a session across harnesses.
func Key(harness, id string) string { return harness + "/" + id }

// Session is one live session as its harness lists it.
type Session struct {
	Harness    string
	ID         string
	PID        int
	Started    string // when its process started, as the harness records it: tells a reused PID apart
	Name       string // the harness's name for it; the daemon names the sessions of harnesses that don't
	Cwd        string
	Status     string // protocol.Status*
	Since      int64  // unix ms of the last status change
	App        string // where it runs, when that isn't a terminal (vscode, desktop, sdk)
	Version    string // the harness's version
	Socket     string
	Transcript string
}

// Call is one tool call in murmur's words.
type Call struct {
	Kind  byte     `json:"kind,omitempty"`  // protocol.Kind*, 0 for a tool Kinds leaves out (plans, other MCP servers)
	Doing string   `json:"doing,omitempty"` // what it does in a few words: "Editing tui.go"
	Shell uint64   `json:"shell,omitempty"` // a fingerprint of the shell command it runs, for the loop hint; 0 for other tools
	Dirs  []string `json:"dirs,omitempty"`  // folders it names by absolute path
}

// Activity is what a session's transcript and tool calls say about it.
type Activity struct {
	Title   string
	PR      int
	PRURL   string
	Doing   string   // the last tool call in a few words
	Kinds   string   // the kinds of the last tool calls, oldest first, one letter each (see protocol.Kind*)
	Loop    bool     // the same command in most of the last tool calls
	Context int      // how full the context window is, in percent
	Dirs    []string // folders the tool calls named by absolute path, the most recent last

	recent []uint64 // the last tool calls, for Loop: Call.Shell
}

const (
	maxKinds = 30  // tool calls in Kinds
	loopOf   = 12  // Loop looks at the last loopOf tool calls
	loopMin  = 9   // and needs one command in at least loopMin of them
	maxDirs  = 200 // folders in Dirs: an early look into another repo is forgotten after this many others
)

// Called counts one tool call: its kind, its command for the loop hint, and the folders it names.
func (a *Activity) Called(c Call) {
	a.Doing = c.Doing
	if c.Kind != 0 {
		a.Kinds += string(c.Kind)
		a.Kinds = a.Kinds[max(len(a.Kinds)-maxKinds, 0):]
	}
	a.recent = append(a.recent, c.Shell)
	a.recent = a.recent[max(len(a.recent)-loopOf, 0):]
	a.Loop = looping(a.recent)
	for _, dir := range c.Dirs {
		if filepath.IsAbs(dir) {
			a.touch(dir)
		}
	}
}

// touch moves dir to the end of Dirs, which keeps the maxDirs folders named most recently.
func (a *Activity) touch(dir string) {
	if i := slices.Index(a.Dirs, dir); i >= 0 {
		a.Dirs = slices.Delete(a.Dirs, i, i+1)
	} else if len(a.Dirs) >= maxDirs {
		a.Dirs = slices.Delete(a.Dirs, 0, 1)
	}
	a.Dirs = append(a.Dirs, dir)
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

// Shell is the fingerprint of a shell command, for Call.Shell: commands never leave the hook as text.
func Shell(command string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(command))
	return h.Sum64() | 1 // never 0, which stands for every other tool
}

// Running says what a shell command runs, without its arguments: "Running go".
func Running(command string) string {
	for _, f := range strings.Fields(command) {
		if !strings.Contains(f, "=") { // skip VAR=value before the command
			return "Running " + filepath.Base(f)
		}
	}
	return "Running a command"
}

// Dirs are the folders of these paths, made absolute against cwd, for Call.Dirs.
func Dirs(cwd string, paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		out = append(out, filepath.Dir(p))
	}
	return out
}

// Abs is path made absolute against cwd, or "" for an empty path.
func Abs(cwd, path string) string {
	if path != "" && !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	return path
}

// Alive says whether process pid runs.
func Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ShellQuote quotes s for a hook command, when it needs it.
func ShellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"\\$`!*?&;|<>()[]{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Ours says whether a hook command is one of murmur's: it runs murmur with hook as its first argument. Join and
// uninstall find their own entries by it, whichever version of murmur wrote them.
func Ours(command string) bool {
	return strings.Contains(command, "murmur") && strings.Contains(command, " hook ")
}

// Check is a protocol.Check whose detail is fix when it fails.
func Check(ok bool, name, detail, fix string) protocol.Check {
	if !ok {
		detail = fix
	}
	return protocol.Check{Name: name, OK: ok, Detail: detail}
}
