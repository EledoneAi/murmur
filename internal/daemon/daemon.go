// Package daemon is the one long-lived murmur process on a laptop.
package daemon

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/gitscan"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/EledoneAi/murmur/v2/internal/setup"
	"github.com/EledoneAi/murmur/v2/internal/update"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fsnotify/fsnotify"
)

// The daemon is the one long-lived murmur process on a laptop. It sleeps on file events and on its hub
// connection, and exits on its own ten minutes after the last agent session and console are gone, once no question
// asked from the laptop waits for its answer.

const (
	idleExit   = 10 * time.Minute
	gitEvery   = 20 * time.Second // while a session works; once more whenever its status changes
	keepMsgs   = 1000
	historyMax = 2 << 20
	sendWait   = 6 * time.Second // how long a send waits for the hub's answer
)

var errUnauthorized = errors.New("unauthorized")

type Daemon struct {
	mu         sync.Mutex
	wait       time.Duration                // a send's wait for the hub, when not sendWait: tests shorten it
	sessions   map[string]*session          // by harness.Key
	hints      map[string]string            // session key -> transcript path, from its start hook
	sockets    map[string]string            // procKey -> where murmur pushes messages to the session, from its start hook
	terms      map[string]map[string]string // procKey -> where the session runs, from its start hook's environment
	watched    map[string]string            // transcript path -> session key
	people     []protocol.Person
	pairs      []protocol.Overlap // this person's agents' overlaps, from the roster
	me         protocol.Person
	state      string
	history    []protocol.Msg
	seen       map[string]bool
	handed     map[string]bool      // message id, or "notice id/Claude session id": the agent, or that conversation, has it
	held       map[string][]heldMsg // agent name -> notes waiting while it is idle
	queued     map[string][]heldMsg // session key -> messages waiting for its next hook, when murmur can't push to it
	inflight   map[string]inflight  // token -> messages a hook's answer carries, until the hook printed them
	names      map[string]string    // session key -> the name murmur gave a session learned from hooks, from the history
	outbox     map[string]outMsg
	failed     map[string]failure      // messages sent from here that failed, by id
	repos      map[string]string       // folder -> the repo it is in, "" outside git: for the repos sessions touch
	asks       map[string]protocol.Msg // held by a mute, waiting for this person
	answers    map[string]string       // answerKey -> "go" or "declined", once its person answered
	mutes      []mute
	done       map[string]bool
	subs       map[chan struct{}]bool
	out        chan protocol.Frame
	acks       map[string]chan protocol.Frame
	checks     map[string]chan protocol.Frame // check id -> whoever waits for the hub's answer
	sentAgents []byte
	lastUse    time.Time
	team       protocol.Team // what the hub asks of laptops, from its welcome
	available  string        // the team's version, when it is newer than this one
	forced     bool          // the hub refuses this version: only an update brings this laptop back
	updating   time.Time     // when this daemon last started an update
	reportAt   time.Time     // when it last sent the hub its self-check

	// questions by the way (btw.go)
	asides     map[string]*aside   // by aside.key
	forks      map[string]bool     // the session ids of the copies running, which are no sessions of this laptop's
	lines      map[string]*line    // session key -> the questions at its copy
	slots      chan struct{}       // each copy running on this laptop holds one
	asideCh    chan struct{}       // closed when an aside changes, for whoever waits on one
	waiters    map[string]int      // aside id -> the btw-waits showing its answer, which then needs no notification
	btwAnswers string              // config.BtwAnswers: "" (approve), auto or off
	hubBtw     bool                // the hub's last welcome said it carries questions by the way
	solo       bool                // murmur solo: the hub is this daemon's own, with no teammates and no other laptop on it
	welcomed   bool                // a hub welcomed this daemon
	btwOut     chan protocol.Frame // the connection this person's open questions last went out on
	// askers finds the harnesses that answer questions by the way. Tests replace setup's; nil uses them.
	askers func(harness string) harness.Asker
	// stopping is done once the daemon stops (stopCopies): the copies running stop with it, and no other starts
	stopping   context.Context
	stopCopies context.CancelFunc

	// notify shows a desktop notification. Tests leave it nil, which shows none.
	notify func(title, text string)
	// notices are what senders hear when a session closes before it read their messages, sent once the daemon runs
	notices []ipc.Request
	running bool // past the start: notices go out as they come
	// reapply sets the laptop's harnesses up again after an update, once the daemon has reached the hub: an update
	// that never gets there is rolled back, and the older version must find the setup it knows. Tests leave it nil.
	reapply   func()
	reapplied bool
	lock      *os.File // held while the daemon runs: closing it, as the garbage collector would, lets another start

	kick chan struct{}
	w    *fsnotify.Watcher
	ln   net.Listener
	hist *os.File
}

func Run() error {
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(config.Dir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil // another daemon already runs
	}
	if os.Getenv("GOMAXPROCS") == "" {
		// it mostly waits: with a processor for every core, each wake-up woke idle threads too, a sixth of its CPU in
		// a busy team; two still answer a hook while a roster decodes
		runtime.GOMAXPROCS(2)
	}
	logPath := filepath.Join(config.Dir(), "daemon.log")
	if st, err := os.Stat(logPath); err == nil && st.Size() > 1<<20 {
		_ = os.Truncate(logPath, 0)
	}
	_ = os.Remove(config.SockPath())
	ln, err := net.Listen("unix", config.SockPath())
	if err != nil {
		return err
	}
	_ = os.Chmod(config.SockPath(), 0o600)
	if config.Load().Solo {
		if err := solo(context.Background()); err != nil {
			return fmt.Errorf("murmur solo's hub: %w", err)
		}
	}
	cfg := config.Load()
	config.UseDirs(cfg) // the tools' folders as join saw them, whoever started this daemon
	sweepCopies()
	d := newDaemon(cfg.Handle)
	d.me.Name, d.ln, d.team.Agreements, d.notify, d.reapply, d.lock = cfg.Name, ln, cfg.Agreements, desktop, setup.Reapply, lock
	if cfg.BtwAnswers != ipc.BtwApprove {
		d.btwAnswers = cfg.BtwAnswers
	}
	d.solo = cfg.Solo
	d.loadHistory()
	d.mu.Lock()
	d.running = true
	d.mu.Unlock()
	go d.sendNotices()
	if d.w, err = fsnotify.NewWatcher(); err != nil {
		return err
	}
	if err := watchListers(d.w); err != nil {
		return err
	}
	d.scanAll()
	go d.watchLoop()
	go d.publishLoop()
	go d.tickLoop()
	go d.hubLoop()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	go func() {
		log.Printf("stopped by %v", <-signals)
		d.shutdown()
	}()
	log.Printf("murmur %s started (pid %d)", release.Version, os.Getpid())
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go d.serve(c)
	}
}

func newDaemon(handle string) *Daemon {
	stopping, stopCopies := context.WithCancel(context.Background())
	return &Daemon{
		sessions: map[string]*session{}, hints: map[string]string{}, sockets: map[string]string{}, terms: map[string]map[string]string{}, watched: map[string]string{},
		seen: map[string]bool{}, handed: map[string]bool{}, held: map[string][]heldMsg{}, queued: map[string][]heldMsg{}, names: map[string]string{},
		outbox: map[string]outMsg{}, asides: map[string]*aside{},
		failed: map[string]failure{}, repos: map[string]string{}, asks: map[string]protocol.Msg{},
		answers: map[string]string{}, done: map[string]bool{}, subs: map[chan struct{}]bool{}, acks: map[string]chan protocol.Frame{},
		checks: map[string]chan protocol.Frame{}, me: protocol.Person{Handle: handle}, state: "connecting", lastUse: time.Now(),
		kick: make(chan struct{}, 1), stopping: stopping, stopCopies: stopCopies,
	}
}

// shutdown stops the daemon: as it is replaced by a newer murmur, uninstalled, stopped by a signal, or idle. The copies
// answering questions by the way stop first, with whatever they started: one left running would answer nobody, at its
// person's cost. Until then the socket stays, so whoever stops the daemon waits for them before starting another.
func (d *Daemon) shutdown() {
	d.haltCopies(copyHalt)
	_ = d.ln.Close()
	_ = os.Remove(config.SockPath())
	os.Exit(0)
}

func (d *Daemon) changed() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------- sessions

// listers are the harnesses that list their own live sessions, which the daemon watches.
func listers() map[string]harness.Lister {
	out := map[string]harness.Lister{}
	for _, h := range setup.Harnesses {
		if l, ok := h.(harness.Lister); ok {
			out[h.Name()] = l
		}
	}
	return out
}

// watchListers watches the folders the installed harnesses list their sessions in. One that isn't installed is left
// alone, since the folder made for it would make it look installed; once it is, join connects it and restarts the
// daemon.
func watchListers(w *fsnotify.Watcher) error {
	for _, h := range setup.Harnesses {
		l, ok := h.(harness.Lister)
		if !ok || !h.Installed() {
			continue
		}
		_ = os.MkdirAll(l.Watch(), 0o700)
		if err := w.Add(l.Watch()); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) scanAll() {
	for name, l := range listers() {
		d.scan(name, l)
	}
}

// scan reads the sessions a harness lists, and lets go of its sessions it no longer lists.
func (d *Daemon) scan(name string, l harness.Lister) {
	list := l.Sessions()
	d.mu.Lock()
	defer d.mu.Unlock()
	live, writing := map[string]bool{}, map[int]bool{}
	known := map[string]string{} // procKey -> key, of the sessions this harness already has
	for k, s := range d.sessions {
		if s.harness == name {
			known[procKey(s.harness, s.id, s.pid)] = k
		}
	}
	var listed []harness.Session
	for _, e := range list {
		if e.ID == "" {
			writing[e.PID] = true // half-written: what the daemon has for it stays, and the next event reads it again
			continue
		}
		if d.forks[e.ID] { // a copy answering a question by the way, registered as a session while it runs
			continue
		}
		listed = append(listed, e)
	}
	slices.SortFunc(listed, func(a, b harness.Session) int { return a.PID - b.PID })
	for _, e := range listed {
		// the same conversation open in two processes (claude --resume while it runs) shows as two agents. Each
		// keeps the key it has, so neither replaces the other, and a new one takes the plain key only when no
		// running process holds it
		key, ok := known[procKey(e.Harness, e.ID, e.PID)]
		if !ok {
			key = harness.Key(e.Harness, e.ID)
			if s := d.sessions[key]; live[key] || (s != nil && s.pid != e.PID && (writing[s.pid] || slices.ContainsFunc(listed,
				func(o harness.Session) bool { return o.PID == s.pid && o.ID == e.ID }))) {
				key += "@" + strconv.Itoa(e.PID)
			}
		}
		live[key] = true
		d.listedLocked(l, e, key)
	}
	for k, s := range d.sessions {
		if s.harness == name && !live[k] && !writing[s.pid] {
			d.removeLocked(k)
		}
	}
}

// procKey names a session in one process: two processes may hold the same conversation.
func procKey(harness, id string, pid int) string { return harness + "/" + id + "@" + strconv.Itoa(pid) }

// listedLocked takes in a session as harness l lists it, under key.
func (d *Daemon) listedLocked(l harness.Lister, e harness.Session, key string) {
	s := d.sessions[key]
	fresh := s == nil || s.started != e.Started || s.pid != e.PID
	if fresh {
		if s != nil {
			d.removeLocked(key)
		}
		// the harness's own record of the start time may be in its own format: what a harness checks is murmur's
		s = &session{harness: e.Harness, id: e.ID, pid: e.PID, started: e.Started, proc: harness.StartTime(e.PID)}
		if c, ok := l.(harness.StartChecker); ok && !c.SameStart(e.PID, e.Started) {
			s.proc = "" // the session's process is gone, and another has its PID: a copy takes nothing from that one
		}
		if key != harness.Key(e.Harness, e.ID) {
			s.k = key
		}
		d.sessions[key] = s
	}
	statusChanged := s.status != e.Status
	// the name is cleaned as the hub cleans it, so this laptop, the hub and every message use the same one. A session
	// its person didn't name is named after itself, never after a folder: it keeps its address wherever it works
	switch {
	case !e.Derived:
		s.name = protocol.Clean(e.Name, false) // "" for a session Claude Code doesn't name (claude -p): shown nowhere
	case s.name == "":
		s.name = d.autoNameLocked(e.Harness, e.ID, key)
	}
	s.cwd, s.sock, s.app, s.version = e.Cwd, e.Socket, e.App, e.Version
	if sock := d.sockets[procKey(e.Harness, e.ID, e.PID)]; sock != "" { // what the harness documents wins over its registry
		s.sock = sock
	}
	if statusChanged {
		d.statusLocked(s, e.Status, e.Since)
	}
	if s.transcript == "" {
		d.attachLocked(s)
	}
	if fresh || statusChanged {
		d.gitLocked(s)
	}
	d.changed()
}

// autoNameLocked names session key, whose harness gives it no name its person chose, after its id: claude-0905 for
// 0905d316-…, as its transcript is named. The name stays the session's wherever it works and across daemon restarts.
// A name another session on this laptop has takes more of the id.
// ponytail: only this laptop's sessions are checked, and only as a session comes: the same person's agent on another
// laptop with the same 4 hex digits (1 in 65536 a pair) hides one of the two in the roster, and the longer name may
// shorten at a daemon restart once the other session is gone. Keep names in the history if that ever bites.
func (d *Daemon) autoNameLocked(h, id, key string) string {
	short := strings.ReplaceAll(id, "-", "")
	for n := 4; ; n += 2 {
		name, taken := protocol.Clean(h+"-"+short[:min(n, len(short))], false), false
		for k, s := range d.sessions {
			taken = taken || (k != key && s.name == name)
		}
		if !taken || n >= len(short) {
			return name
		}
	}
}

// statusLocked records a session's new status. One that works again gets the notes it was spared while idle.
func (d *Daemon) statusLocked(s *session, status string, since int64) {
	s.status, s.since = status, since
	if status != protocol.StatusWaiting {
		s.needs = ""
	}
	if status == protocol.StatusIdle {
		s.ended = time.Now() // its title is checked once git has read where the turn left it
	}
	if status == protocol.StatusBusy && len(d.held[s.name]) > 0 {
		held, name := d.unholdLocked(s.name), s.name
		go func() {
			for _, m := range held {
				d.handOver(m, name)
			}
		}()
	}
}

// retitleEvery is the least time between two checks of a session's title that its person's prompts call for.
const retitleEvery = 10 * time.Minute

// retitleAsk is what a copy is asked about the title showing: whether it still says what the session works on, or
// else what does.
func retitleAsk(showing string) string {
	if showing == "" {
		return "Murmur shows your teammates and their agents a title for this conversation, so they know what you work " +
			"on and whom to ask. Title what you are working on now: six words at most, in the conversation's language, " +
			"and nothing else in your answer."
	}
	return fmt.Sprintf("Murmur shows your teammates and their agents this conversation titled %q, so they know what you "+
		"work on and whom to ask. If that still says what you are working on now, answer KEEP. If your focus has moved "+
		"on, answer with a title for what you work on now: six words at most, in the conversation's language. Answer "+
		"KEEP or the title, and nothing else.", showing)
}

// retitleLocked checks, once git has read where a turn ended, whether the session's title still says what it works
// on, when its work may have moved on: at once when it works in another repo or on another branch than at the last
// check, and after a turn its person started, once retitleEvery has passed since. Claude Code titles a conversation
// once, from its first prompt: a session its person turns to other things would show its first task to the agents
// deciding whom to ask. A copy of the conversation answers KEEP or a new title, from the prompt cache the turn just
// warmed, at the session's person's cost, unless they turned it off (murmur retitles off). A title its person set
// stays.
func (d *Daemon) retitleLocked(s *session) {
	moved := !strings.EqualFold(s.repo, s.titleRepo) || s.branch != s.titleBranch
	prompted := s.act.Prompts > s.titlePrompts && time.Since(s.titleAt) >= retitleEvery
	if !moved && !prompted {
		return
	}
	s.titleRepo, s.titleBranch, s.titlePrompts, s.titleAt = s.repo, s.branch, s.act.Prompts, time.Now()
	if s.act.Custom || !copies(d.askerOf(s.harness)) || d.unaskable(s) != "" || config.Load().Retitles == "off" {
		return
	}
	key, name, old, showing := s.key(), s.name, s.act.Title, s.title()
	go func() {
		ans, err := d.copyOf(name, "murmur", retitleAsk(showing))
		if err != nil {
			log.Printf("retitling %s: %v", name, err)
			return
		}
		title := titleFrom(ans.Text)
		d.mu.Lock()
		defer d.mu.Unlock()
		if s := d.sessions[key]; s != nil && s.act.Title == old && title != "" && !strings.EqualFold(title, "keep") {
			s.newTitle, s.oldTitle = title, old
			d.changed()
		}
	}()
}

// titleFrom is a copy's answer as a title: its first line, without the quotes, emphasis or full stop a model may add.
func titleFrom(answer string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(answer), "\n")
	return shortTitle(strings.Trim(strings.TrimSpace(line), "\"'*_`#. "))
}

func (d *Daemon) removeLocked(key string) {
	if s := d.sessions[key]; s != nil {
		if s.hooked {
			d.appendHistory(histLine{Gone: key})
			d.unreadLocked(key)
		}
		d.unwatchLocked(s)
		delete(d.hints, key)
		delete(d.sessions, key)
		d.changed()
	}
}

// attachLocked finds the session's transcript, for a harness whose transcripts murmur reads.
func (d *Daemon) attachLocked(s *session) {
	r, ok := setup.Harness(s.harness).(harness.Reader)
	if !ok {
		return
	}
	path, read := r.Open(harness.Session{Harness: s.harness, ID: s.id, Transcript: d.hints[s.key()]})
	if path == "" {
		return // the harness writes it with the first message, after the start hook named it; the tick tries again
	}
	s.transcript, s.read = path, read
	d.watched[path] = s.key()
	_ = d.w.Add(path)
	s.read(&s.act)
	d.followLocked(s, s.act.Cwd, "")
}

func (d *Daemon) unwatchLocked(s *session) {
	if s.transcript != "" {
		_ = d.w.Remove(s.transcript)
		delete(d.watched, s.transcript)
	}
}

func (d *Daemon) onTranscript(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[d.watched[path]]
	if s == nil || s.transcript != path {
		return
	}
	if s.read(&s.act) {
		d.changed()
	}
	d.followLocked(s, s.act.Cwd, "")
}

// followLocked follows a session to where it works now: the folder its harness says it works in, cwd, which a cd or a
// worktree moves, and the file it is about to edit, file, in a repo it reaches by path. Only a git checkout takes it:
// a scratch file, or a look at a folder outside git, leaves it where it works. cwd moves it only when it changed, and
// file only out of the checkout it works in, so editing deep in its own tree doesn't move it to that folder.
func (d *Daemon) followLocked(s *session, cwd, file string) {
	to := ""
	if cwd != "" && cwd != s.wd {
		s.wd = cwd
		if gitscan.TopOf(cwd) != "" {
			to = cwd
		}
	}
	if top := gitscan.TopOf(file); top != "" {
		if _, in := inCheckout(s.top, file); !in {
			to = top
		}
	}
	if to == "" || to == s.dir() {
		return
	}
	s.at, s.gitAt = to, time.Time{} // read at once, not behind other checkouts
	d.gitLocked(s)
}

func (d *Daemon) gitLocked(s *session) {
	if s.dir() == "" {
		return
	}
	if s.gitBusy || time.Since(s.gitAt) < 3*time.Second {
		s.gitAgain = true // else a branch switched just before the turn ended would show until the next turn
		return
	}
	s.gitBusy, s.gitAgain = true, false
	cwd, dirs, from := s.dir(), slices.Clone(s.act.Dirs), time.Now()
	read := gitscan.Read
	if s.gitAt.IsZero() { // the session's start brief waits for its first read: not behind another checkout's
		read = gitscan.ReadNow
	}
	go func() {
		r := read(cwd)
		also := d.alsoOf(dirs, r.Top, r.Repo)
		d.mu.Lock()
		defer d.mu.Unlock()
		s.gitBusy, s.gitAt = false, time.Now()
		if cwd != s.dir() { // it moved while git read where it was
			s.gitAgain = true
			return
		}
		if s.top != r.Top || s.repo != r.Repo || s.branch != r.Branch || !slices.Equal(s.files, r.Files) || s.st != r.State ||
			!slices.Equal(s.also, also) {
			s.top, s.repo, s.branch, s.tree, s.files, s.st, s.also = r.Top, r.Repo, r.Branch, r.Tree, r.Files, r.State, also
			d.changed()
		}
		if s.titleAt.IsZero() { // the title showing is the one the session came with, here
			s.titleRepo, s.titleBranch, s.titlePrompts, s.titleAt = s.repo, s.branch, s.act.Prompts, time.Now()
		}
		if !s.ended.IsZero() && from.After(s.ended) { // read where the turn left it
			s.ended = time.Time{}
			d.retitleLocked(s)
		}
	}()
}

const maxAlso = 5

// alsoOf names the other repos a session touched, from the folders its tool calls named: the maxAlso it touched most
// recently, in name order, leaving out its own repo. Each folder outside the session's checkout is looked up in git
// once.
func (d *Daemon) alsoOf(dirs []string, top, repo string) []string {
	var out []string
	for i := len(dirs) - 1; i >= 0 && len(out) < maxAlso; i-- {
		dir := dirs[i]
		if top != "" && (dir == top || strings.HasPrefix(dir, top+string(filepath.Separator))) {
			continue
		}
		d.mu.Lock()
		name, ok := d.repos[dir]
		d.mu.Unlock()
		if !ok {
			name = gitscan.RepoOf(dir)
			d.mu.Lock()
			if d.repos == nil || len(d.repos) > 5000 {
				d.repos = map[string]string{}
			}
			d.repos[dir] = name
			d.mu.Unlock()
		}
		if name != "" && !strings.EqualFold(name, repo) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (d *Daemon) watchLoop() {
	dirs := map[string]string{} // a folder a harness lists its sessions in -> the harness
	for name, l := range listers() {
		dirs[filepath.Clean(l.Watch())] = name
	}
	for {
		select {
		case ev, ok := <-d.w.Events:
			if !ok {
				return
			}
			if name, ok := dirs[filepath.Dir(ev.Name)]; ok {
				d.scan(name, listers()[name])
			} else if ev.Has(fsnotify.Write) {
				d.onTranscript(ev.Name)
			}
		case err, ok := <-d.w.Errors:
			if !ok {
				return
			}
			log.Printf("watch: %v", err)
		}
	}
}

func (d *Daemon) tickLoop() {
	tick := time.NewTicker(5 * time.Second)
	n := 0
	for range tick.C {
		n++
		if n%12 == 0 {
			d.scanAll() // safety net for a missed file event
			go d.maybeUpdate()
			go d.maybeReport()
		}
		if n%24 == 0 {
			d.mu.Lock()
			out := d.out
			d.pruneLocked()
			d.mu.Unlock()
			if out != nil {
				go d.resend(out, 2*time.Minute)
			}
			go d.btwTick(askAgain)
		}
		d.mu.Lock()
		for k, s := range d.sessions {
			if !harness.Alive(s.pid) || (s.hooked && (!s.running() || time.Since(s.heard) > hookedTTL)) {
				d.removeLocked(k)
				continue
			}
			if s.transcript == "" {
				d.attachLocked(s)
			}
			if s.gitAgain || (s.status == protocol.StatusBusy || s.status == protocol.StatusWaiting) && time.Since(s.gitAt) > gitEvery {
				d.gitLocked(s)
			}
		}
		idle := d.idleLocked()
		d.mu.Unlock()
		if idle {
			log.Print("no agent session, console or question for a while: exiting")
			d.shutdown()
		}
	}
}

// idleLocked says whether the daemon has had nothing to do for idleExit: no agent session, no console, and no
// question asked from this laptop that waits for its answer, which only this daemon would hear.
func (d *Daemon) idleLocked() bool {
	if len(d.sessions) > 0 || len(d.subs) > 0 || d.askingLocked() {
		d.lastUse = time.Now()
		return false
	}
	return time.Since(d.lastUse) > idleExit
}

func (d *Daemon) localAgentsLocked(withFiles bool) []protocol.Agent {
	var out []protocol.Agent
	for _, s := range d.sessions {
		if s.name != "" {
			a := s.agent(withFiles)
			a.Btw = d.btwOfLocked(s)
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b protocol.Agent) int { return strings.Compare(a.Name, b.Name) })
	if withFiles {
		fitFiles(out, protocol.HubReadLimit-64<<10) // the rest of the frame takes far less
	}
	return out
}

// fitFiles drops the changed files of the agents that changed longest ago until the agents, encoded, fit in budget
// bytes: the hub closes a connection that sends a bigger frame, and the laptop would reconnect for nothing forever.
// Their overlaps go unseen meanwhile.
func fitFiles(agents []protocol.Agent, budget int) {
	b, _ := json.Marshal(agents)
	size := len(b)
	byAge := make([]int, len(agents))
	for i := range byAge {
		byAge[i] = i
	}
	slices.SortFunc(byAge, func(i, j int) int { return cmp.Compare(agents[i].Since, agents[j].Since) })
	for _, i := range byAge {
		if size <= budget {
			return
		}
		f, _ := json.Marshal(agents[i].Files)
		size -= len(f)
		agents[i].Files = nil
	}
}

// byIDLocked finds a session by its id, in any harness.
func (d *Daemon) byIDLocked(id string) *session {
	for _, s := range d.sessions {
		if s.id == id {
			return s
		}
	}
	return nil
}

// byPIDLocked finds a session that the harness process pid runs.
func (d *Daemon) byPIDLocked(pid int) *session {
	for _, s := range d.sessions {
		if s.pid == pid {
			return s
		}
	}
	return nil
}

// ---------------------------------------------------------------- publishing

func (d *Daemon) publishLoop() {
	for range d.kick {
		time.Sleep(30 * time.Millisecond) // one update for a burst of file events
		d.mu.Lock()
		if d.out != nil {
			agents := d.localAgentsLocked(true)
			if b, _ := json.Marshal(agents); !bytes.Equal(b, d.sentAgents) {
				select {
				case d.out <- protocol.Frame{T: protocol.FrameAgents, Agents: agents}:
					d.sentAgents = b
				default:
				}
			}
		}
		for ch := range d.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		d.mu.Unlock()
	}
}

func (d *Daemon) snapshotLocked() ipc.Snapshot {
	mine := d.localAgentsLocked(false)
	me := d.me
	var people []protocol.Person
	found := false
	for _, p := range d.people {
		if p.Handle == me.Handle {
			found = true
			agents := slices.Clone(mine)
			for _, a := range p.Agents { // the same person's agents on another laptop
				if !slices.ContainsFunc(mine, func(m protocol.Agent) bool { return m.Name == a.Name }) {
					agents = append(agents, a)
				}
			}
			p.Agents, p.Online = agents, true
		}
		people = append(people, p)
	}
	if !found {
		people = append([]protocol.Person{{Handle: me.Handle, Name: me.Name, Online: d.out != nil, Agents: mine}}, people...)
	}
	msgs := d.history
	if len(msgs) > 300 {
		msgs = msgs[len(msgs)-300:]
	}
	done := map[string]bool{}
	for _, m := range msgs {
		if d.done[m.ID] {
			done[m.ID] = true
		}
	}
	asks := slices.Collect(maps.Values(d.asks))
	slices.SortFunc(asks, func(a, b protocol.Msg) int { return int(a.At - b.At) })
	sending := slices.SortedFunc(maps.Values(d.outbox), func(a, b outMsg) int {
		return cmp.Or(a.At.Compare(b.At), strings.Compare(a.Msg.ID, b.Msg.ID))
	})
	var failed map[string]string
	inHistory := map[string]bool{}
	for _, m := range msgs {
		inHistory[m.ID] = true
	}
	snap := ipc.Snapshot{Me: me, State: d.state, People: people, Msgs: slices.Clone(msgs), Pairs: d.pairs,
		Done: done, Asks: asks, Update: d.available, Mutes: d.muteListLocked(), Version: release.Version}
	for _, o := range sending {
		snap.Sending = append(snap.Sending, o.Msg.ID)
		if !inHistory[o.Msg.ID] {
			snap.Outbox = append(snap.Outbox, d.fullLocked(o.Msg))
		}
	}
	for id, f := range d.failed {
		if failed == nil {
			failed = map[string]string{}
		}
		failed[id] = f.Why
		if d.done[id] { // a refused message never reaches the history, and its person can mark it done all the same
			done[id] = true
		}
		if !inHistory[id] && f.Msg.ID != "" {
			snap.Outbox = append(snap.Outbox, f.Msg)
		}
	}
	snap.Failed = failed
	snap.Asides, snap.BtwAnswers = d.asidesLocked(), cmp.Or(d.btwAnswers, ipc.BtwApprove)
	for _, s := range d.sessions { // the console takes an agent here for one on this laptop, which it can ask
		switch why := d.unaskable(s); {
		case s.name == "":
		case why != "":
			if snap.CantAsk == nil {
				snap.CantAsk = map[string]string{}
			}
			snap.CantAsk[s.name] = why
		default:
			if snap.Said == nil {
				snap.Said = map[string]string{}
			}
			snap.Said[s.name] = protocol.Clean(protocol.Cut(s.act.Said, 400), false)
		}
	}
	slices.SortFunc(snap.Outbox, func(a, b protocol.Msg) int { return cmp.Or(cmp.Compare(a.At, b.At), strings.Compare(a.ID, b.ID)) })
	return snap
}

// ---------------------------------------------------------------- hub

func (d *Daemon) setState(s string) {
	d.mu.Lock()
	changed := d.state != s
	d.state = s
	d.mu.Unlock()
	if changed {
		d.changed()
	}
}

func (d *Daemon) hubLoop() {
	backoff := 500 * time.Millisecond
	for {
		cfg := config.Load()
		if cfg.Token == "" {
			d.setState("not signed in: run murmur join")
			time.Sleep(20 * time.Second)
			continue
		}
		start := time.Now()
		err := d.connect(cfg)
		if errors.Is(err, errUnauthorized) {
			d.setState("signed out: run murmur join")
			time.Sleep(time.Minute)
			continue
		}
		// a hub closes with policy violation only to refuse this laptop's protocol or version. Try again later means
		// this laptop sent too much at once: it reconnects as after any drop, and is not refused
		var closed websocket.CloseError
		if errors.As(err, &closed) && closed.Code == websocket.StatusPolicyViolation {
			d.refused(cfg, closed.Reason)
			time.Sleep(time.Minute)
			continue
		}
		d.setState("reconnecting")
		if time.Since(start) > 30*time.Second {
			backoff = 500 * time.Millisecond
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

func wsURL(hub string) string {
	if scheme, rest, ok := strings.Cut(hub, "://"); ok { // MURMUR_HUB may say HTTPS://
		hub = strings.ToLower(scheme) + "://" + rest
	}
	if rest, ok := strings.CutPrefix(hub, "https://"); ok {
		return "wss://" + rest + "/ws"
	}
	return "ws://" + strings.TrimPrefix(hub, "http://") + "/ws"
}

func (d *Daemon) connect(cfg config.Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dctx, ddone := context.WithTimeout(ctx, 15*time.Second)
	c, resp, err := websocket.Dial(dctx, wsURL(cfg.Hub), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Token}}})
	ddone()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errUnauthorized
		}
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.LaptopReadLimit)
	out := make(chan protocol.Frame, 256)
	d.mu.Lock()
	agents := d.localAgentsLocked(true)
	d.sentAgents, _ = json.Marshal(agents)
	// hello first: the hub reads nothing before it, and nothing else reaches out before d.out names it
	out <- protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: release.Version, Agents: agents,
		Can: []string{protocol.CanBtw}}
	d.out = out // "online" once the team list arrives: until then who would show an empty team
	d.mu.Unlock()
	d.resend(out, 0)
	d.changed()
	defer func() {
		d.mu.Lock()
		if d.out == out {
			d.out = nil
		}
		d.mu.Unlock()
		d.changed()
	}()

	go func() {
		tick := time.NewTicker(25 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-out:
				wctx, done := context.WithTimeout(ctx, 10*time.Second)
				err := wsjson.Write(wctx, c, f)
				done()
				if err != nil {
					cancel()
					return
				}
			case <-tick.C:
				pctx, done := context.WithTimeout(ctx, 20*time.Second)
				err := c.Ping(pctx)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		var f protocol.Frame
		if err := wsjson.Read(ctx, c, &f); err != nil {
			return err
		}
		d.onFrame(f)
	}
}

func (d *Daemon) toHub(f protocol.Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.out != nil {
		select {
		case d.out <- f:
		default:
		}
	}
}

// check asks the hub which agents change these files in other checkouts of repo, and waits at most wait for the
// answer. No hub, or no answer in time, counts as none: a check must never hold an agent up.
func (d *Daemon) check(repo, tree string, files []string, wait time.Duration) []string {
	files = checkable(files)
	timeout := time.After(wait)
	id, answer := protocol.RandomHex(8), make(chan protocol.Frame, 1)
	d.mu.Lock()
	out := d.out
	if out == nil {
		d.mu.Unlock()
		return nil
	}
	d.checks[id] = answer
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.checks, id)
		d.mu.Unlock()
	}()
	select {
	case out <- protocol.Frame{T: protocol.FrameCheck, ID: id, Repo: repo, Tree: tree, Files: files}:
	case <-timeout:
		return nil
	}
	select {
	case f := <-answer:
		return f.Overlaps
	case <-timeout:
		return nil
	}
}

func (d *Daemon) onFrame(f protocol.Frame) {
	switch f.T {
	case protocol.FrameWelcome:
		if f.Me == nil {
			return
		}
		f.Me.Clean()
		// a welcome comes again whenever the team's settings change, and the person may have changed this laptop's
		// own settings since the connection began: the config is read again, not saved over
		cfg := config.Load()
		agreements := cfg.Agreements
		if f.Team != nil {
			f.Team.Agreements = protocol.Clean(f.Team.Agreements, true) // every agent reads them
			agreements = f.Team.Agreements
		}
		d.mu.Lock()
		d.me = *f.Me
		d.mu.Unlock()
		if cfg.Handle != f.Me.Handle || cfg.Name != f.Me.Name || cfg.Agreements != agreements {
			cfg.Handle, cfg.Name, cfg.Agreements = f.Me.Handle, f.Me.Name, agreements
			_ = config.Save(cfg)
		}
		d.mu.Lock()
		if f.Team != nil {
			d.team = *f.Team
		}
		d.available, d.forced = "", false
		// the console and murmur status print it
		f.Version = protocol.Clean(protocol.Cut(f.Version, 40), false)
		if update.Newer(f.Version) { // never an older one: a hub can't take laptops back to a version with a known bug
			d.available = f.Version
		}
		can := slices.Contains(f.Can, protocol.CanBtw)
		d.hubBtw, d.welcomed = can, true // the agents published next say whether teammates can ask them
		d.mu.Unlock()
		d.btwWelcomed(can)
		go d.maybeUpdate()
		d.changed()
	case protocol.FrameRoster:
		for i := range f.People { // the hub cleans what laptops send; this laptop does not rely on it
			f.People[i].Clean()
		}
		pairs := cleanPairs(f.Pairs)
		d.mu.Lock()
		d.people, d.pairs, d.state = f.People, pairs, "online"
		if d.reapply != nil && !d.reapplied {
			d.reapplied = true
			go d.reapply()
		}
		d.mu.Unlock()
		d.changed()
	case protocol.FrameMsg:
		if f.Msg != nil {
			m := *f.Msg
			m.From, m.To, m.Text = protocol.Clean(m.From, false), protocol.Clean(m.To, false), protocol.Clean(m.Text, true)
			d.onMsg(m, f.Echo)
		}
	case protocol.FrameAck:
		f.Note, f.Err = protocol.Clean(f.Note, false), protocol.Clean(f.Err, false) // shown to the sender and its agent
		d.mu.Lock()
		ch := d.acks[f.ID]
		if ch != nil {
			select {
			case ch <- f: // under the lock: a sender that stops waiting finds it there
			default:
			}
		}
		d.mu.Unlock()
		if ch == nil {
			d.lateAck(f) // its sender stopped waiting
		}
	case protocol.FrameBtw:
		if f.Btw != nil {
			d.onBtw(*f.Btw)
		}
	case protocol.FrameBtwAnswer:
		if f.Btw != nil {
			d.onBtwAnswer(*f.Btw)
		}
	case protocol.FrameChecked:
		for i := range f.Overlaps { // they reach an agent
			f.Overlaps[i] = protocol.Clean(f.Overlaps[i], false)
		}
		d.mu.Lock()
		ch := d.checks[f.ID]
		d.mu.Unlock()
		if ch != nil {
			select {
			case ch <- f:
			default:
			}
		}
	}
}

// A check names at most maxCheck files, maxCheckBytes of paths in all: a frame stays far under what the hub reads.
const (
	maxCheck      = 500
	maxCheckBytes = 256 << 10
)

// checkable are the first files a check can name.
func checkable(files []string) []string {
	size := 0
	for i, f := range files {
		if size += len(f) + 3; i >= maxCheck || size > maxCheckBytes {
			return files[:i]
		}
	}
	return files
}

// cleanPairs cleans the overlaps a roster brings, which this laptop's console prints.
func cleanPairs(in []protocol.Overlap) []protocol.Overlap {
	var pairs []protocol.Overlap
	for _, p := range in[:min(len(in), 50)] {
		p.Mine, p.Other = protocol.Clean(protocol.Cut(p.Mine, 400), false), protocol.Clean(protocol.Cut(p.Other, 400), false)
		p.Files = slices.Clone(p.Files[:min(len(p.Files), 3)])
		for i := range p.Files {
			p.Files[i] = protocol.Clean(protocol.Cut(p.Files[i], 1024), false)
		}
		p.More = max(p.More, 0)
		pairs = append(pairs, p)
	}
	return pairs
}

// failure is a message sent from this laptop that failed, and why, kept a day for the console, also across restarts.
type failure struct {
	Msg protocol.Msg `json:"msg"` // its sender in full
	Why string       `json:"why"`
	At  time.Time    `json:"at"`
}

func (d *Daemon) failLocked(m protocol.Msg, why string) {
	if d.failed == nil {
		d.failed = map[string]failure{}
	}
	f := failure{d.fullLocked(m), why, time.Now()}
	d.failed[m.ID] = f
	d.appendHistory(histLine{Failed: &f})
	d.changed()
}

// fullLocked names a message's sender in full, as the hub does: the person, or person/agent. The outbox names only
// the sending agent, or nobody for the person.
func (d *Daemon) fullLocked(m protocol.Msg) protocol.Msg {
	switch {
	case m.From == "":
		m.From = d.me.Handle
	case !strings.Contains(m.From, "/"):
		m.From = d.me.Handle + "/" + m.From
	}
	return m
}

// outMsg is a message sent from this laptop that the hub hasn't confirmed. It goes again, under the same id, each
// time the daemon connects, until the hub answers: whoever already has it drops the copy.
type outMsg struct {
	Msg protocol.Msg `json:"msg"` // From is the sending session's name, empty for the person
	At  time.Time    `json:"at"`
}

const (
	outboxTTL = 24 * time.Hour
	resendMax = 50 // messages one resend sends: a hub closes a connection that sends too much at once
)

// resend sends the unconfirmed messages at least minAge old again, the resendMax oldest, and gives up on those older
// than a day. It runs on every connection, and every few minutes while connected: a message can be lost after the
// hub forwarded it, when the recipient's laptop drops before it answers. Whoever already has it drops the copy.
func (d *Daemon) resend(out chan protocol.Frame, minAge time.Duration) {
	d.mu.Lock()
	var again, expired []outMsg
	for id, o := range d.outbox {
		switch age := time.Since(o.At); {
		case age > outboxTTL:
			delete(d.outbox, id)
			d.appendHistory(histLine{Sent: id})
			d.failLocked(o.Msg, "murmur couldn't get it there for a day")
			expired = append(expired, o)
		case age >= minAge && d.acks[id] == nil: // not while its sender still waits for the first answer
			again = append(again, o)
		}
	}
	d.mu.Unlock()
	slices.SortFunc(again, func(a, b outMsg) int { return a.At.Compare(b.At) })
	again = again[:min(len(again), resendMax)] // the rest go next time
	for i := range again {
		select {
		case out <- protocol.Frame{T: protocol.FrameSend, Msg: &again[i].Msg}:
		default: // the rest goes with the next connection
		}
	}
	for _, o := range expired {
		d.tellSender(o.Msg, "not delivered to "+o.Msg.To+": murmur couldn't get it there for a day")
	}
}

// lateAck settles a message whose sender stopped waiting for the answer, and tells the sender if it failed. A failure
// can also come after the hub kept the message for later (a store that failed, or a week without its recipient),
// when the message has left the outbox: the history has it then.
func (d *Daemon) lateAck(f protocol.Frame) {
	d.mu.Lock()
	o, ok := d.outbox[f.ID]
	if ok && f.Err == protocol.TooMany { // a resent copy in a burst: it goes again with the outbox's next try
		d.mu.Unlock()
		return
	}
	if ok {
		delete(d.outbox, f.ID)
		d.appendHistory(histLine{Sent: f.ID})
		d.changed()
	} else if m, mine := d.sentLocked(f.ID); mine {
		o, ok = outMsg{Msg: m}, true
	}
	if ok && !f.OK {
		d.failLocked(o.Msg, f.Err)
	}
	d.mu.Unlock()
	if ok && !f.OK {
		d.tellSender(o.Msg, "not delivered to "+o.Msg.To+": "+f.Err)
	}
}

// sentLocked finds a message this laptop sent in the history, where the hub's copy names its sender in full, and
// names the sender as the outbox does: the sending agent, or "" for the person.
func (d *Daemon) sentLocked(id string) (protocol.Msg, bool) {
	for _, m := range slices.Backward(d.history) {
		if m.ID == id {
			person, agent := protocol.Split(m.From)
			m.From = agent
			return m, person == d.me.Handle
		}
	}
	return protocol.Msg{}, false
}

// tellSender says that a message failed to whoever sent it: the sending agent in its session, the person in the
// console.
func (d *Daemon) tellSender(m protocol.Msg, why string) {
	d.mu.Lock()
	to := d.me.Handle
	d.mu.Unlock()
	if m.From != "" {
		to += "/" + m.From
	}
	note := protocol.Msg{ID: "nd-" + m.ID, From: protocol.System, To: to, Text: why, At: time.Now().UnixMilli()}
	d.record(note)
	if m.From != "" {
		d.handOver(note, m.From)
	}
}

func (d *Daemon) onMsg(m protocol.Msg, echo bool) {
	// recorded first: what the daemon remembers of a message, such as whom it was handed to, lasts while the history
	// has it
	fresh := d.record(m)
	if !echo {
		ok, why, note := true, "", ""
		if _, agent := protocol.Split(m.To); agent != "" {
			switch d.approval(m) {
			case "ask":
				note = d.ask(m)
			case "declined":
				ok, why = false, "its person declined it"
			default:
				var text string
				if ok, text = d.handOver(m, agent); ok {
					note = text
				} else {
					why = text
				}
			}
		}
		d.toHub(protocol.Frame{T: protocol.FrameDelivered, ID: m.ID, OK: ok, Err: why, Note: note})
	}
	if fresh && !echo && d.toMeFromOthers(m) {
		d.tell(m.From, m)
	}
}

// approval says what happens to a message for one of this person's agents: it goes through ("go"), waits for this
// person because a mute holds it ("ask"), or was declined. Murmur's own notices go through: they are already held
// while an agent is idle.
func (d *Daemon) approval(m protocol.Msg) string {
	key := answerKey(m)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case m.From == protocol.System:
		return "go"
	case d.answers[key] != "":
		return d.answers[key]
	}
	if _, held := d.mutedLocked(m); held {
		return "ask"
	}
	return "go"
}

// answerKey names the message a verdict is for: its id and a digest of who sent it, to whom and what it says. The
// sending laptop chooses the id, so a verdict on the id alone would also pass other text, or the same text to another
// agent, sent later under it.
func answerKey(m protocol.Msg) string {
	sum := sha256.Sum256([]byte(m.From + "\x00" + m.To + "\x00" + m.Text))
	return m.ID + "/" + hex.EncodeToString(sum[:8])
}

// ask holds a message for this person, who hears of it from a desktop notification the first time it arrives.
func (d *Daemon) ask(m protocol.Msg) string {
	d.mu.Lock()
	_, again := d.asks[m.ID]
	if !again {
		d.asks[m.ID] = m
		d.appendHistory(histLine{Ask: &m})
		d.changed()
	}
	note := "held: " + d.me.Handle + " muted messages like it for now and sees it in their console"
	d.mu.Unlock()
	if !again {
		d.tell(m.From+", held by a mute", m)
	}
	return note
}

// answer passes a held message on, or drops it. A message whose agent has closed since is not passed on: this person
// hears it from the error, its sender from a note.
func (d *Daemon) answer(op, id string) error {
	d.mu.Lock()
	m, ok := d.asks[id]
	if !ok {
		d.mu.Unlock()
		return errors.New("no message is held under that id")
	}
	var pass []protocol.Msg
	settle := func(m protocol.Msg, verdict string) {
		delete(d.asks, m.ID)
		d.answers[answerKey(m)] = verdict
		d.appendHistory(histLine{Answer: answerKey(m), Verdict: verdict})
		if verdict == "go" {
			pass = append(pass, m)
		}
	}
	switch op {
	case "approve":
		settle(m, "go")
	case "decline":
		settle(m, "declined")
	default:
		d.mu.Unlock()
		return errors.New("unknown answer " + op)
	}
	me := d.me.Handle
	d.mu.Unlock()
	d.changed()
	var lost []string
	for _, m := range pass {
		_, agent := protocol.Split(m.To)
		if ok, why := d.handOver(m, agent); !ok {
			lost = append(lost, why)
			go d.send(ipc.Request{To: m.From, Text: me + " passed your message on, but " + why + "."})
		}
	}
	if op == "decline" { // the sending agent hears it from the person, so it doesn't wait for an answer
		go d.send(ipc.Request{To: m.From, Text: me + " did not pass your message on to " + m.To + "."})
	}
	if len(lost) > 0 {
		return errors.New("passed on, but " + strings.Join(lost, "; "))
	}
	return nil
}

// handOver gives a message to its agent's session once, even when a copy comes again after the agent started a new
// conversation (an outbox sends a message again when its answer was lost). Murmur's notices go to each conversation
// once: one that comes again under the same id doesn't prompt a conversation that has it, while one started since
// gets it. Only a delivery the session took counts: one that failed is tried again when the message comes back, and
// an echo meant for the console never counts. It says whether the message went, and a note for its sender when it
// did, or why when it didn't.
func (d *Daemon) handOver(m protocol.Msg, agent string) (bool, string) {
	d.mu.Lock()
	if m.From != protocol.System && d.handed[m.ID] { // a copy of one it took, maybe before its session ended
		d.mu.Unlock()
		return true, ""
	}
	var target *session
	for _, s := range d.sessions {
		if s.name == agent {
			target = s
		}
	}
	var p harness.Pusher
	if target != nil {
		p, _ = setup.Harness(target.harness).(harness.Pusher)
	}
	if target == nil || (p != nil && target.sock == "") {
		d.mu.Unlock()
		return false, m.To + " is no longer running"
	}
	to, key := target.listed(), m.ID
	if m.From == protocol.System {
		key += "/" + target.id
	}
	if d.handed[key] {
		d.mu.Unlock()
		return true, ""
	}
	// Murmur's own notes (a failed send, for one) and announcements to a repo wait while the agent is idle:
	// handing one over would start a turn, counted like a prompt, often at night with nobody there to act on it
	if (m.From == protocol.System || m.Announce != "") && target.status == protocol.StatusIdle {
		d.holdLocked(agent, m)
		d.mu.Unlock()
		return true, ""
	}
	if p == nil { // the harness takes messages only with its hooks' answers
		d.queueLocked(target, m)
		d.handed[key] = true
		d.appendHistory(histLine{Handed: key})
		note := ""
		if target.status != protocol.StatusBusy {
			note = "waiting: " + m.To + " runs in " + titleOf(target.harness) + ", which takes messages only while its agent works; " +
				"it gets this once its person prompts it again"
		}
		d.mu.Unlock()
		return true, note
	}
	d.handed[key] = true // taken before the push: a copy arriving meanwhile isn't pushed again
	d.mu.Unlock()
	if err := p.Push(to, protocol.Prompt(m)); err != nil {
		d.mu.Lock()
		delete(d.handed, key) // tried again when the message comes back
		d.mu.Unlock()
		return false, "could not reach " + m.To + ": " + err.Error()
	}
	d.mu.Lock()
	d.appendHistory(histLine{Handed: key})
	d.mu.Unlock()
	return true, ""
}

// heldMsg is a note of murmur's or an announcement, kept while its agent is idle. It is kept by the agent's
// name, which a restarted daemon finds its sessions by again, and in the history file: the hub was told it arrived.
type heldMsg struct {
	Agent string       `json:"agent"`
	Msg   protocol.Msg `json:"msg"`
	At    time.Time    `json:"at"`
}

const heldTTL = 24 * time.Hour // a note held longer is out of date

func (d *Daemon) holdLocked(agent string, m protocol.Msg) {
	if slices.ContainsFunc(d.held[agent], func(h heldMsg) bool { return h.Msg.ID == m.ID }) {
		return
	}
	h := heldMsg{Agent: agent, Msg: m, At: time.Now()}
	d.held[agent] = append(d.held[agent], h)
	d.appendHistory(histLine{Held: &h})
}

// unholdLocked takes what is held for agent, leaving out what is out of date.
func (d *Daemon) unholdLocked(agent string) []protocol.Msg {
	var out []protocol.Msg
	for _, h := range d.held[agent] {
		d.appendHistory(histLine{Unheld: h.Msg.ID})
		if time.Since(h.At) < heldTTL {
			out = append(out, h.Msg)
		}
	}
	delete(d.held, agent)
	return out
}

// refused handles a hub that won't take this laptop's version: it learns the team's version and updates to it,
// unless this laptop's person turned updates off.
func (d *Daemon) refused(cfg config.Config, why string) {
	d.setState("refused by the hub: " + protocol.Clean(why, false)) // the console and murmur status print it
	info, err := update.Check(cfg.Hub)
	info.Version = protocol.Clean(protocol.Cut(info.Version, 40), false)
	if err != nil || !update.Newer(info.Version) {
		return
	}
	d.mu.Lock()
	d.available, d.forced = info.Version, true
	d.mu.Unlock()
	d.maybeUpdate()
}

// maybeUpdate starts `murmur update --auto` when the hub runs a newer version and both the team's policy and
// this laptop's allow it, or the hub refuses this version. It waits until none of this laptop's agents is working
// and no copy answers a question: the update restarts the daemon, and a message arriving then would wait for the new
// one.
func (d *Daemon) maybeUpdate() {
	cfg := config.Load()
	exe := config.Exe()
	d.mu.Lock()
	policy := d.team.Updates
	if cfg.Updates != "" {
		policy = cfg.Updates
	}
	ok := d.available != "" && (policy == "auto" || (d.forced && cfg.Updates != "off")) &&
		time.Since(d.updating) > time.Hour && !config.UpdateSkipped(d.available) && update.Managed(exe) == ""
	for _, s := range d.sessions {
		ok = ok && s.status != protocol.StatusBusy
	}
	ok = ok && len(d.forks) == 0 // nor while a copy answers a question by the way: it would stop with the daemon
	if !ok {
		d.mu.Unlock()
		return
	}
	d.updating = time.Now()
	v := d.available
	d.mu.Unlock()
	log.Printf("updating to %s", v)
	logf, err := os.OpenFile(filepath.Join(config.Dir(), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer logf.Close()
	cmd := exec.Command(exe, "update", "--auto")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // it outlives this daemon, which it restarts
	if err := cmd.Start(); err != nil {
		log.Printf("update to %s: %v", v, err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// maybeReport sends the hub this laptop's self-check on the team's schedule, for its admins.
func (d *Daemon) maybeReport() {
	d.mu.Lock()
	hours := d.team.CheckHours
	due := hours > 0 && d.out != nil && time.Since(d.reportAt) > time.Duration(hours)*time.Hour
	if due {
		d.reportAt = time.Now()
	}
	d.mu.Unlock()
	if !due {
		return
	}
	cc, checks := setup.Checks()
	r := protocol.Report{At: time.Now().UnixMilli(), OK: true, OS: runtime.GOOS + "/" + runtime.GOARCH, ClaudeCode: cc, Checks: checks}
	for _, c := range checks {
		r.OK = r.OK && c.OK
	}
	d.toHub(protocol.Frame{T: protocol.FrameReport, Report: &r})
}

// titleOf names a harness for people.
func titleOf(name string) string {
	if h := setup.Harness(name); h != nil {
		return h.Title()
	}
	return name
}

func (d *Daemon) muteListLocked() []string {
	var out []string
	for _, m := range d.mutesLocked() {
		out = append(out, m.String())
	}
	return out
}
