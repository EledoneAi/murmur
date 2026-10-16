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
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/gitscan"
	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
	"github.com/EledoneAi/murmur/internal/setup"
	"github.com/EledoneAi/murmur/internal/update"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fsnotify/fsnotify"
)

// The daemon is the one long-lived murmur process on a laptop. It sleeps on file events and on its hub
// connection, and exits on its own ten minutes after the last agent session and console are gone.

const (
	idleExit   = 10 * time.Minute
	gitEvery   = 20 * time.Second // while a session works; once more whenever its status changes
	keepMsgs   = 1000
	sendWait   = 6 * time.Second
	historyMax = 2 << 20
)

var errUnauthorized = errors.New("unauthorized")

type Daemon struct {
	mu         sync.Mutex
	sessions   map[string]*session          // by harness.Key
	hints      map[string]string            // session key -> transcript path, from its start hook
	sockets    map[string]string            // session key -> where murmur pushes messages to it, from its start hook
	terms      map[string]map[string]string // session key -> where it runs, from its start hook's environment
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

	// notify shows a desktop notification. Tests leave it nil, which shows none.
	notify func(title, text string)

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
	cfg := config.Load()
	d := newDaemon(cfg.Handle)
	d.me.Name, d.ln, d.team.Agreements, d.notify = cfg.Name, ln, cfg.Agreements, desktop
	d.loadHistory()
	if d.w, err = fsnotify.NewWatcher(); err != nil {
		return err
	}
	for _, l := range listers() {
		_ = os.MkdirAll(l.Watch(), 0o700)
		if err := d.w.Add(l.Watch()); err != nil {
			return err
		}
	}
	d.scanAll()
	go d.watchLoop()
	go d.publishLoop()
	go d.tickLoop()
	go d.hubLoop()
	go setup.Reapply()
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
	return &Daemon{
		sessions: map[string]*session{}, hints: map[string]string{}, sockets: map[string]string{}, terms: map[string]map[string]string{}, watched: map[string]string{},
		seen: map[string]bool{}, handed: map[string]bool{}, held: map[string][]heldMsg{}, queued: map[string][]heldMsg{}, names: map[string]string{},
		outbox: map[string]outMsg{},
		failed: map[string]failure{}, repos: map[string]string{}, asks: map[string]protocol.Msg{},
		answers: map[string]string{}, done: map[string]bool{}, subs: map[chan struct{}]bool{}, acks: map[string]chan protocol.Frame{},
		checks: map[string]chan protocol.Frame{}, me: protocol.Person{Handle: handle}, state: "connecting", lastUse: time.Now(),
		kick: make(chan struct{}, 1),
	}
}

func (d *Daemon) shutdown() {
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
	for _, e := range list {
		if e.ID == "" {
			writing[e.PID] = true // half-written: the next event reads it again
			continue
		}
		live[harness.Key(e.Harness, e.ID)] = true
		d.listedLocked(e)
	}
	for k, s := range d.sessions {
		if s.harness == name && !live[k] && !writing[s.pid] {
			d.removeLocked(k)
		}
	}
}

// listedLocked takes in a session as its harness lists it.
func (d *Daemon) listedLocked(e harness.Session) {
	key := harness.Key(e.Harness, e.ID)
	s := d.sessions[key]
	fresh := s == nil || s.started != e.Started || s.pid != e.PID
	if fresh {
		if s != nil {
			d.removeLocked(key)
		}
		s = &session{harness: e.Harness, id: e.ID, pid: e.PID, started: e.Started}
		d.sessions[key] = s
	}
	statusChanged := s.status != e.Status
	// the name is cleaned as the hub cleans it, so this laptop, the hub and every message use the same one
	s.name, s.cwd, s.sock, s.app, s.version = protocol.Clean(e.Name, false), e.Cwd, e.Socket, e.App, e.Version
	if sock := d.sockets[key]; sock != "" { // what the harness documents wins over its registry
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

// statusLocked records a session's new status. One that works again gets the notes it was spared while idle.
func (d *Daemon) statusLocked(s *session, status string, since int64) {
	s.status, s.since = status, since
	if status != protocol.StatusWaiting {
		s.needs = ""
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

func (d *Daemon) removeLocked(key string) {
	if s := d.sessions[key]; s != nil {
		if s.hooked {
			d.appendHistory(histLine{Gone: key})
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
	if s != nil && s.transcript == path && s.read(&s.act) {
		d.changed()
	}
}

func (d *Daemon) gitLocked(s *session) {
	if s.gitBusy || time.Since(s.gitAt) < 3*time.Second || s.cwd == "" {
		return
	}
	s.gitBusy = true
	cwd, dirs := s.cwd, slices.Clone(s.act.Dirs)
	go func() {
		r := gitscan.Read(cwd)
		also := d.alsoOf(dirs, r.Top, r.Repo)
		d.mu.Lock()
		defer d.mu.Unlock()
		s.gitBusy, s.gitAt = false, time.Now()
		if s.top != r.Top || s.repo != r.Repo || s.branch != r.Branch || !slices.Equal(s.files, r.Files) || s.st != r.State ||
			!slices.Equal(s.also, also) {
			s.top, s.repo, s.branch, s.tree, s.files, s.st, s.also = r.Top, r.Repo, r.Branch, r.Tree, r.Files, r.State, also
			d.changed()
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
		}
		d.mu.Lock()
		for k, s := range d.sessions {
			if !harness.Alive(s.pid) || (s.hooked && time.Since(s.heard) > hookedTTL) {
				d.removeLocked(k)
				continue
			}
			if s.transcript == "" {
				d.attachLocked(s)
			}
			if (s.status == protocol.StatusBusy || s.status == protocol.StatusWaiting) && time.Since(s.gitAt) > gitEvery {
				d.gitLocked(s)
			}
		}
		if len(d.sessions) > 0 || len(d.subs) > 0 {
			d.lastUse = time.Now()
		} else if time.Since(d.lastUse) > idleExit {
			log.Print("no agent session or console for a while: exiting")
			d.shutdown()
		}
		d.mu.Unlock()
	}
}

func (d *Daemon) localAgentsLocked(withFiles bool) []protocol.Agent {
	var out []protocol.Agent
	for _, s := range d.sessions {
		if s.name != "" {
			out = append(out, s.agent(withFiles))
		}
	}
	slices.SortFunc(out, func(a, b protocol.Agent) int { return strings.Compare(a.Name, b.Name) })
	return out
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
		if !inHistory[id] && f.Msg.ID != "" {
			snap.Outbox = append(snap.Outbox, f.Msg)
		}
	}
	snap.Failed = failed
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
	out <- protocol.Frame{T: protocol.FrameHello, Protocol: protocol.Version, Version: release.Version, Agents: agents}
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
		if update.Newer(f.Version) { // never an older one: a hub can't take laptops back to a version with a known bug
			d.available = f.Version
		}
		d.mu.Unlock()
		go d.maybeUpdate()
		d.changed()
	case protocol.FrameRoster:
		for i := range f.People { // the hub cleans what laptops send; this laptop does not rely on it
			f.People[i].Clean()
		}
		pairs := cleanPairs(f.Pairs)
		d.mu.Lock()
		d.people, d.pairs, d.state = f.People, pairs, "online"
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
		d.mu.Unlock()
		if ch == nil {
			d.lateAck(f) // its sender stopped waiting
			return
		}
		select {
		case ch <- f:
		default:
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

// failure is a message sent from this laptop that failed, and why, kept a day for the console.
type failure struct {
	Msg protocol.Msg // its sender in full
	Why string
	At  time.Time
}

func (d *Daemon) failLocked(m protocol.Msg, why string) {
	if d.failed == nil {
		d.failed = map[string]failure{}
	}
	d.failed[m.ID] = failure{d.fullLocked(m), why, time.Now()}
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
	d.mu.Unlock()
	if err := p.Push(to, protocol.Prompt(m)); err != nil {
		return false, "could not reach " + m.To + ": " + err.Error()
	}
	d.mu.Lock()
	d.handed[key] = true
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
	d.setState("refused by the hub: " + why)
	info, err := update.Check(cfg.Hub)
	if err != nil || !update.Newer(info.Version) {
		return
	}
	d.mu.Lock()
	d.available, d.forced = info.Version, true
	d.mu.Unlock()
	d.maybeUpdate()
}

// maybeUpdate starts `murmur update --auto` when the hub runs a newer version and both the team's policy and
// this laptop's allow it, or the hub refuses this version. It waits until none of this laptop's agents is working:
// the update restarts the daemon, and a message arriving then would wait for the new one.
func (d *Daemon) maybeUpdate() {
	cfg := config.Load()
	exe := config.Exe()
	d.mu.Lock()
	policy := d.team.Updates
	if cfg.Updates != "" {
		policy = cfg.Updates
	}
	ok := d.available != "" && (policy == "auto" || (d.forced && cfg.Updates != "off")) &&
		time.Since(d.updating) > time.Hour && !update.Skipped(d.available) && update.Managed(exe) == ""
	for _, s := range d.sessions {
		ok = ok && s.status != protocol.StatusBusy
	}
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
