// Package protocol is what crosses the WebSocket between a laptop and the hub, plus the few rules both sides share.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Agent is one Claude Code session as the team sees it. Everything in it is read from what Claude Code and git
// already record; agents never fill any of it in.
type Agent struct {
	Name   string   `json:"name"`
	Repo   string   `json:"repo,omitempty"`   // owner/name from the git origin, or the folder name outside git
	Branch string   `json:"branch,omitempty"` //
	Title  string   `json:"title,omitempty"`  // the title Claude Code keeps for the conversation
	PR     int      `json:"pr,omitempty"`     //
	PRURL  string   `json:"pr_url,omitempty"` //
	App    string   `json:"app,omitempty"`    // vscode, desktop or sdk; empty in a terminal
	Status string   `json:"status"`           // busy, idle, waiting or shell, as Claude Code reports it
	Needs  string   `json:"needs,omitempty"`  // permission or input, when it waits for its person
	Since  int64    `json:"since,omitempty"`  // unix ms of the last status change
	Doing  string   `json:"doing,omitempty"`  // the last tool call in a few words: "Editing tui.go"
	Tree   string   `json:"tree,omitempty"`   // checkout id: sessions sharing one checkout always overlap
	Files  []string `json:"files,omitempty"`  // changed on the branch; only the hub sees them, for overlaps

	Kinds   string `json:"kinds,omitempty"`   // the last 30 tool calls, oldest first, one letter each (see Kind*)
	Loop    bool   `json:"loop,omitempty"`    // the same command in 9 of the last 12 calls: often a loop
	Context int    `json:"context,omitempty"` // how full the conversation's context window is, in percent
	Added   int    `json:"added,omitempty"`   // lines added on the branch and in the tree, against the merge base
	Deleted int    `json:"deleted,omitempty"` // lines deleted, likewise
	Changed int    `json:"changed,omitempty"` // files changed, likewise
	// Where the checkout is, beyond Repo and Branch: the nearest local branch it is stacked on, the short commit
	// when HEAD is detached, a rebase or merge in progress ("rebasing 3/7", "merging"), and the folder the session
	// runs in under the checkout's top, for a package in a monorepo.
	Base     string   `json:"base,omitempty"`
	Detached string   `json:"detached,omitempty"`
	Op       string   `json:"op,omitempty"`
	Dir      string   `json:"dir,omitempty"`
	Also     []string `json:"also,omitempty"` // other repos the session touched, owner/name like Repo
}

// The kinds of tool call in Agent.Kinds.
const (
	KindRead     = 'r' // Read, Grep, Glob, LS and the like
	KindEdit     = 'e' // Edit, MultiEdit, Write, NotebookEdit
	KindRun      = 'x' // Bash
	KindWeb      = 'w' // WebFetch, WebSearch
	KindSubagent = 's' // Task, Agent
	KindMessage  = 'm' // murmur's send
)

// Overlap is one of a person's agents changing the same files as another checkout's agent.
type Overlap struct {
	Mine  string   `json:"mine"`  // the person's agent, by name
	Other string   `json:"other"` // the other agent's address
	Files []string `json:"files"` // the first three shared files
	More  int      `json:"more,omitempty"`
}

// LaptopReadLimit is the largest frame a laptop reads from its hub. The hub keeps the team list under it.
const LaptopReadLimit = 32 << 20

// HubReadLimit is the largest frame a hub reads from a laptop, which closes the connection of a laptop that sends
// more: a laptop keeps its hello and agents frames, files included, under it.
const HubReadLimit = 1 << 20

type Person struct {
	Handle string  `json:"handle"`
	Name   string  `json:"name,omitempty"`
	Online bool    `json:"online"`
	Colour int     `json:"colour"` // which of the eight person colours is theirs, the same on every screen
	Agents []Agent `json:"agents,omitempty"`
}

// Colours is how many person colours there are. The hub gives them in the order people joined, so the first eight
// people never share one.
const Colours = 8

// Msg is one message. Addresses are "bob/main-1" for an agent, "bob" for a person, "repo:acme/api" for every agent
// working in a repo, "murmur" for notices.
type Msg struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
	Text string `json:"text"`
	At   int64  `json:"at"`
	// Announce is the repo of an announcement, on the copy the hub sends each agent working in it. To is that
	// agent's address, so laptops deliver it as any message.
	Announce string `json:"announce,omitempty"`
}

// Version is the protocol a laptop speaks, sent in its hello. A hub refuses versions it doesn't speak.
const Version = 1

// Frame types. Laptop to hub: hello, agents, send, delivered, report, check. Hub to laptop: welcome, roster, msg, ack,
// checked.
const (
	FrameHello     = "hello"
	FrameAgents    = "agents"
	FrameSend      = "send"
	FrameDelivered = "delivered"
	FrameReport    = "report"
	FrameCheck     = "check"   // laptop to hub: does another checkout change these files? (ID, Repo, Tree, Files)
	FrameChecked   = "checked" // hub to laptop: the answer, as overlap lines (ID, Overlaps)
	FrameWelcome   = "welcome"
	FrameRoster    = "roster"
	FrameMsg       = "msg"
	FrameAck       = "ack"
)

// What Claude Code reports a session doing.
const (
	StatusBusy    = "busy"
	StatusIdle    = "idle"
	StatusWaiting = "waiting"
	StatusShell   = "shell"
)

type Frame struct {
	T        string    `json:"t"`
	Protocol int       `json:"p,omitempty"` // on hello
	Version  string    `json:"v,omitempty"` // the laptop's release on hello, the team's on welcome
	Host     string    `json:"host,omitempty"`
	Me       *Person   `json:"me,omitempty"`
	Agents   []Agent   `json:"agents,omitempty"`
	People   []Person  `json:"people,omitempty"`
	Msg      *Msg      `json:"msg,omitempty"`
	Overlaps []string  `json:"overlaps,omitempty"` // on checked: the answer, one line per agent
	Pairs    []Overlap `json:"pairs,omitempty"`    // on a roster: its person's agents' overlaps right now
	Report   *Report   `json:"report,omitempty"`   // a laptop's self-check
	Team     *Team     `json:"team,omitempty"`     // on welcome: what the team asks of laptops
	Repo     string    `json:"repo,omitempty"`     // on check
	Tree     string    `json:"tree,omitempty"`     // on check: the asking checkout, whose own agents don't count
	Files    []string  `json:"files,omitempty"`    // on check: paths in Repo
	Echo     bool      `json:"echo,omitempty"`     // a copy for the people involved to see, not for delivery
	ID       string    `json:"id,omitempty"`
	OK       bool      `json:"ok,omitempty"`
	Note     string    `json:"note,omitempty"`
	Err      string    `json:"err,omitempty"`
}

const System = "murmur"

// Team is what a hub asks of its laptops, sent on welcome.
type Team struct {
	Updates    string `json:"updates"`               // auto, notify or off
	MinVersion string `json:"min_version,omitempty"` // older laptops are refused
	CheckHours int    `json:"check_hours,omitempty"` // how often to send a Report; 0 for never
	Agreements string `json:"agreements,omitempty"`  // how the team asks agents to work together, for every session
}

// Report is a laptop's check of its own setup, as `murmur doctor` runs it, for the team's admins.
type Report struct {
	At         int64   `json:"at"` // unix ms
	OK         bool    `json:"ok"`
	OS         string  `json:"os"` // darwin/arm64
	ClaudeCode string  `json:"claude_code,omitempty"`
	Checks     []Check `json:"checks"`
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MaxText caps one message. Claude Code accepts far more; people and agents should not need it.
const MaxText = 8000

// TooMany is the hub's answer to a message past its sender's limit for the minute. A laptop sends a message its
// outbox resent again later; a fresh one fails, so its agent hears to group them.
const TooMany = "too many messages in the last minute; group them into one"

// RepoAddress is how an announcement addresses every agent working in a repo: "repo:acme/api".
const RepoAddress = "repo:"

// Split turns "bob/main-1" into ("bob", "main-1") and "bob" into ("bob", "").
func Split(addr string) (person, agent string) {
	person, agent, _ = strings.Cut(strings.TrimSpace(addr), "/")
	return strings.ToLower(strings.TrimSpace(person)), strings.TrimSpace(agent)
}

// Person of an address, for "bob/main-1" or "bob".
func PersonOf(addr string) string {
	p, _ := Split(addr)
	return p
}

// Prompt is the text a Claude Code session receives. The first line is what its person sees in the preview,
// so it names the sender and starts the message.
func Prompt(m Msg) string {
	first, rest, _ := strings.Cut(strings.TrimSpace(m.Text), "\n")
	var b strings.Builder
	if m.From == System {
		fmt.Fprintf(&b, "[murmur] %s", first)
	} else {
		fmt.Fprintf(&b, "[murmur] %s: %s", m.From, first)
	}
	if rest != "" {
		b.WriteByte('\n')
		b.WriteString(rest)
	}
	if m.From != System {
		who := m.From
		if p, a := Split(m.From); a != "" {
			who = p + "'s agent " + a
		}
		fmt.Fprintf(&b, "\n\n(From %s, through murmur. Answer with the murmur `send` tool, to %q.)", who, m.From)
	}
	return b.String()
}

// escapeSeq matches the terminal escape sequences people would otherwise see as junk once their escape character is
// gone: CSI (colors, cursor moves, clearing the screen) and OSC (clipboard writes, hyperlinks, window titles), in their
// 7-bit and 8-bit forms. An OSC ends at its terminator or at the end of its line, so an unterminated one can't
// swallow the rest of a message.
var escapeSeq = regexp.MustCompile(`(?:\x1b\[|\x{9b})[0-?]*[ -/]*[@-~]|(?:\x1b\]|\x{9d})[^\x07\x1b\x{9c}\n\r\x{2028}\x{2029}]*(?:\x07|\x1b\\|\x{9c})?`)

// Clean makes text from another laptop safe to print and to hand to an agent. It removes terminal escape
// sequences, every other control character, and the invisible characters that make text read differently than it
// is or hide words from people but not from agents. Bytes that aren't UTF-8 become U+FFFD. Message text keeps its
// line breaks and tabs (lines true); one-line fields turn them into spaces.
func Clean(s string, lines bool) string {
	if !strings.ContainsFunc(s, func(r rune) bool {
		return r < ' ' || r == '\u2028' || r == '\u2029' || r == utf8.RuneError || unsafeRune(r) || selector(r)
	}) {
		return s
	}
	s = escapeSeq.ReplaceAllString(s, "")
	var b strings.Builder
	afterBase := false // the last character kept can take a variation selector: not a selector, a joiner or a mark
	for _, r := range s {
		switch {
		case r == '\n' || r == '\u2028' || r == '\u2029' || r == '\t':
			if !lines {
				r = ' '
			} else if r != '\t' {
				r = '\n'
			}
		case r < ' ' || unsafeRune(r):
			continue
		case selector(r) && !afterBase:
			continue // one variation selector right after a character, as emoji and some kanji use; a run hides data
		}
		afterBase = !selector(r) && !unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me)
		b.WriteRune(r)
	}
	return b.String()
}

// unsafeRune is DEL; the C1 controls (U+0080 to U+009F), which some terminals read as escape sequences; the Hangul
// fillers, letters that show nothing; and the format characters (Unicode category Cf: bidi embeddings, overrides and
// isolates, zero-width spaces, the byte order mark, soft hyphens, and the tag characters people can't see but agents
// read), except the few that real text needs.
func unsafeRune(r rune) bool {
	switch {
	case r == 0x7f || (r >= 0x80 && r <= 0x9f), r == 0x3164 || r == 0x115f || r == 0x1160 || r == 0xffa0:
		return true
	case unicode.Is(unicode.Cf, r):
		return !neededFormat(r)
	}
	return false
}

// neededFormat are the format characters that writing needs: the zero-width non-joiner and joiner (Persian and
// Indic scripts, emoji), the left-to-right, right-to-left and Arabic letter marks, and the Arabic marks that go
// before numbers.
func neededFormat(r rune) bool {
	switch r {
	case 0x200c, 0x200d, 0x200e, 0x200f, 0x061c, 0x06dd, 0x070f, 0x0890, 0x0891, 0x08e2, 0x110bd, 0x110cd:
		return true
	}
	return r >= 0x0600 && r <= 0x0605
}

// selector is a variation selector, which picks how the character before it is drawn.
func selector(r rune) bool {
	return (r >= 0xfe00 && r <= 0xfe0f) || (r >= 0xe0100 && r <= 0xe01ef) || (r >= 0x180b && r <= 0x180d) || r == 0x180f
}

// Clean cleans every text field of an agent described by another laptop, and cuts each to a length no real agent
// needs, so one laptop can't fill everyone's console or the hub's memory.
func (a *Agent) Clean() {
	for _, f := range []struct {
		s   *string
		max int
	}{{&a.Name, 80}, {&a.Repo, 200}, {&a.Branch, 200}, {&a.Title, 200}, {&a.PRURL, 300}, {&a.App, 20}, {&a.Status, 20},
		{&a.Needs, 20}, {&a.Doing, 200}, {&a.Tree, 64}, {&a.Base, 200}, {&a.Detached, 40}, {&a.Op, 40}, {&a.Dir, 200}} {
		*f.s = Clean(Cut(*f.s, f.max*4), false)
		*f.s = string([]rune(*f.s)[:min(len([]rune(*f.s)), f.max)])
	}
	for i := range a.Files {
		a.Files[i] = Clean(Cut(a.Files[i], 1024), false)
	}
	a.Kinds = strings.Map(func(r rune) rune {
		if strings.ContainsRune(kinds, r) {
			return r
		}
		return -1
	}, Cut(a.Kinds, 30))
	a.Also = a.Also[:min(len(a.Also), 5)]
	for i := range a.Also {
		a.Also[i] = Clean(Cut(a.Also[i], 200), false)
	}
	a.Context = min(max(a.Context, 0), 100)
	a.Added, a.Deleted, a.Changed = max(a.Added, 0), max(a.Deleted, 0), max(a.Changed, 0)
}

const kinds = string(KindRead) + string(KindEdit) + string(KindRun) + string(KindWeb) + string(KindSubagent) + string(KindMessage)

// Cut keeps at most n bytes of s, never half a character.
func Cut(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}

// Clean cleans a person and their agents.
func (p *Person) Clean() {
	p.Handle, p.Name = Clean(p.Handle, false), Clean(p.Name, false)
	p.Colour = ((p.Colour % Colours) + Colours) % Colours
	for i := range p.Agents {
		p.Agents[i].Clean()
	}
}

// RandomHex is n random bytes, as hex: message ids, tokens, sign-in codes.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
