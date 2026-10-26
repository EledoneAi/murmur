// Package ipc is the local socket between the daemon and the programs that talk to it: the console, the MCP server
// and one-shot commands.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

type Snapshot struct {
	Me     protocol.Person    `json:"me"`
	State  string             `json:"state"`
	People []protocol.Person  `json:"people"`
	Msgs   []protocol.Msg     `json:"msgs"`
	Pairs  []protocol.Overlap `json:"pairs,omitempty"` // this person's agents' overlaps right now
	// Messages sent from this laptop: those the hub hasn't confirmed yet, and those that failed, with why. Outbox
	// holds the ones the history doesn't, the hub never having echoed them, so the console can show their text.
	Sending []string          `json:"sending,omitempty"`
	Failed  map[string]string `json:"failed,omitempty"`
	Outbox  []protocol.Msg    `json:"outbox,omitempty"`
	Done    map[string]bool   `json:"done"`
	Asks    []protocol.Msg    `json:"asks,omitempty"` // messages a mute holds, waiting for this person
	Version string            `json:"version"`
	Update  string            `json:"update,omitempty"` // the team's version, when this laptop can update to it
	Mutes   []string          `json:"mutes,omitempty"`  // what this person muted, in force now
	// Asides are the questions asked by the way in the last day: this person's, and those asked of this person's
	// agents, oldest first. Said has every agent of this laptop's that its coding tool lets be asked by the way, by name,
	// with the first line of what it last said in its own words, "" until it has said something: an agent in it is on
	// this laptop, and its person can ask it here, without a hub. Said never leaves the laptop. BtwAnswers is this
	// laptop's setting for teammates' questions: approve, auto or off. CantAsk has this laptop's other agents, by name,
	// with why nobody can ask them by the way, in words their person acts on: "api-9a can't be asked by the way: its
	// Claude Code is older than 2.1.292; update Claude Code".
	Asides     []Aside           `json:"asides,omitempty"`
	Said       map[string]string `json:"said,omitempty"`
	BtwAnswers string            `json:"btw_answers,omitempty"`
	CantAsk    map[string]string `json:"cant_ask,omitempty"`
}

// Aside is a question asked by the way, as the console and plain mode show it.
type Aside struct {
	protocol.Btw
	// Mine says it was asked from this laptop, by this person or by one of their agents (From then names it); else a
	// teammate or a teammate's agent asked one of this person's agents.
	Mine  bool   `json:"mine,omitempty"`
	State string `json:"state"`
	Cold  int    `json:"cold,omitempty"` // on AsideCold: the conversation the copy would read, in tokens
}

// Where an aside stands.
const (
	AsideAsking   = "asking"   // on its way, or waiting on the agent's person (Note says which); Mine only
	AsideRunning  = "running"  // the copy is answering
	AsideHeld     = "held"     // answered: the answer waits for this person, who sends it (btw-approve) or drops it
	AsideCold     = "cold"     // the agent was idle long: the question waits for this person's go before the copy runs
	AsideAnswered = "answered" // final: Answer holds it
	AsideFailed   = "failed"   // final: Err says why, a person's decline included
)

// The settings for teammates' questions by the way, in config.Config.BtwAnswers and Snapshot.BtwAnswers. Empty is
// BtwApprove. Questions from this person, from any of their laptops, never wait.
const (
	BtwApprove = protocol.BtwApprove // each answer waits for this person
	BtwAuto    = protocol.BtwAuto    // answers go out on their own, except a cold question's
	BtwAll     = "all"               // every answer goes out on its own, a cold question's too
	BtwOff     = "off"               // teammates can't ask this person's agents
)

// ---------------------------------------------------------------- talking to the daemon

// Request is one call to the daemon. The ops for questions by the way:
//   - "btw": To (an agent's address) and Text, under an ID the caller chose (protocol.RandomHex(8)). It returns once
//     the question is on its way, or refused with Err. The answer shows in Snapshot.Asides. From the MCP server's btw
//     tool (or OpenCode's murmur_btw), it carries Harness, PID and Session as send does, and the question is then asked
//     as that agent, not as this laptop's person, and a final answer (or Err) that no btw-wait returned reaches the
//     agent as a murmur message.
//   - "btw-wait": ID. It returns once the aside is final, or after about two minutes: Text is the answer, Err why
//     there is none, and Note where the question stands while it isn't final. From the btw tool, it carries Harness,
//     PID and Session too, and returns after 45 seconds at most: the tool stops waiting at 50 (mcp.btwWait), under the
//     coding tools' time limit for a tool call, and tells the agent the Note. A caller that closes the connection stops
//     waiting (a tool call its coding tool cancelled): a final answer it didn't read reaches the agent as a message.
//   - "btw-approve", "btw-decline": ID, of an aside AsideHeld (send the answer, or drop it) or AsideCold (run the
//     copy, or decline the question) for this person.
//   - "btw-answers": Text is BtwApprove, BtwAuto, BtwAll or BtwOff, saved as this laptop's setting.
type Request struct {
	Op      string         `json:"op"`
	Hook    *harness.Event `json:"hook,omitempty"`    // what a harness's hook reports
	Harness string         `json:"harness,omitempty"` // the harness of the agent that sends, from the MCP server
	PID     int            `json:"pid,omitempty"`     // its process
	Session string         `json:"session,omitempty"` // and its session id, when the harness gives the MCP server one
	To      string         `json:"to,omitempty"`
	Text    string         `json:"text,omitempty"`
	ID      string         `json:"id,omitempty"`
	IDs     []string       `json:"ids,omitempty"`
	Version string         `json:"version,omitempty"` // on stop: the version of the daemon meant, which refuses if it isn't
}

type Reply struct {
	OK      bool   `json:"ok"`
	Note    string `json:"note,omitempty"`
	Err     string `json:"err,omitempty"`
	Text    string `json:"text,omitempty"`
	State   string `json:"state,omitempty"`
	Version string `json:"version,omitempty"` // the daemon's own, on status
	Update  string `json:"update,omitempty"`  // on status: the team's version, when this laptop can update to it
	// Ack, on a hook's reply, says the reply carries messages the daemon keeps until the hook says it printed them.
	Ack bool `json:"ack,omitempty"`
}

func Dial() (net.Conn, error) { return net.DialTimeout("unix", config.SockPath(), time.Second) }

// self is the binary this program started from. On Linux the running binary follows a rename: once an update moves
// it aside to murmur.prev, os.Executable names the old version, which must not start again.
var self = config.Exe()

// replaced says this process already replaced an older daemon once: after an update that rolled back, the binary on
// disk is the older one again, and replacing its daemon again would only start it again.
var replaced bool

// EnsureDaemon starts the background process unless it already runs. It returns once the daemon answers. A daemon
// older than this binary, still running after an upgrade the daemon didn't make itself (Homebrew's), is stopped,
// and this version's starts and sets the laptop up again: once per process, and never for a version that failed to
// update on this laptop lately.
func EnsureDaemon() error {
	var r Reply
	err := CallRunning(Request{Op: "status"}, &r, time.Second)
	switch {
	case err != nil:
		if c, derr := Dial(); derr == nil { // it answers, but slowly: leave it be
			c.Close()
			return nil
		}
	case release.Dev() || r.Version == "" || !release.Before(r.Version, release.Version) || replaced ||
		config.UpdateSkipped(release.Version):
		return nil
	default:
		replaced = true
		// the stop names the version seen: a daemon another process started meanwhile stays
		_ = CallRunning(Request{Op: "stop", Version: r.Version}, &Reply{}, 2*time.Second)
		for range 100 { // until it has let go of its socket
			c, err := Dial()
			if err != nil {
				break
			}
			c.Close()
			time.Sleep(20 * time.Millisecond)
		}
	}
	return start(self)
}

// start starts a daemon; tests replace it.
var start = StartDaemon

// StartDaemon runs exe as the daemon and waits up to three seconds for its socket. When a daemon already runs, the
// new one exits at once and that one answers.
func StartDaemon(exe string) error {
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(config.Dir(), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	failed := errors.New("the murmur daemon did not start; see " + filepath.Join(config.Dir(), "daemon.log"))
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for range 150 {
		if c, err := Dial(); err == nil {
			c.Close()
			return nil
		}
		select {
		case err := <-exited:
			if err == nil {
				continue // another daemon has the lock and is starting
			}
			return failed
		case <-time.After(20 * time.Millisecond):
		}
	}
	return failed
}

// Call sends one Request and reads one Reply, starting the daemon if it doesn't run. A nil out skips the Reply.
func Call(req Request, out any, timeout time.Duration) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	return CallRunning(req, out, timeout)
}

// Hook sends a hook's request to a daemon that already runs, and reads its reply. When the reply carries messages
// (Ack), the daemon keeps them until done(true) says the hook printed them; done(false), or a hook that never says,
// leaves them waiting.
func Hook(req Request, timeout time.Duration) (Reply, func(printed bool), error) {
	var r Reply
	c, err := Dial()
	if err != nil {
		return r, nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	line, err := []byte(nil), json.NewEncoder(c).Encode(req)
	if err == nil {
		line, err = bufio.NewReaderSize(c, 1<<16).ReadBytes('\n')
	}
	if err == nil {
		err = json.Unmarshal(line, &r)
	}
	if err != nil || !r.Ack {
		c.Close()
		return r, func(bool) {}, err
	}
	return r, func(printed bool) {
		if printed {
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, _ = c.Write([]byte("printed\n"))
		}
		c.Close()
	}, nil
}

// CallRunning is Call for a daemon that already runs: it never starts one.
func CallRunning(req Request, out any, timeout time.Duration) error {
	return CallRunningContext(context.Background(), req, out, timeout)
}

// CallRunningContext is CallRunning that gives up as soon as ctx is done, with ctx's error: it closes its connection,
// and the daemon sees whoever called is gone (an agent's btw tool whose call was cancelled, for one).
func CallRunningContext(ctx context.Context, req Request, out any, timeout time.Duration) error {
	c, err := Dial()
	if err != nil {
		return err
	}
	defer c.Close()
	defer context.AfterFunc(ctx, func() { c.Close() })()
	_ = c.SetDeadline(time.Now().Add(timeout))
	err = json.NewEncoder(c).Encode(req)
	if err == nil && out != nil {
		var line []byte
		if line, err = bufio.NewReaderSize(c, 1<<16).ReadBytes('\n'); err == nil {
			err = json.Unmarshal(line, out)
		}
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ---------------------------------------------------------------- one-shot commands

// settled asks the daemon for its state, giving a daemon that just started up to five seconds to reach the hub and
// get the team list. It exits ten minutes after the last session, so the command that starts it again would
// otherwise see "connecting". A daemon that lost the hub ("reconnecting") answers at once.
func Settled() (Reply, error) {
	var r Reply
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if err := Call(Request{Op: "status"}, &r, 2*time.Second); err != nil {
			return r, err
		}
		if r.State != "connecting" || time.Now().After(deadline) {
			return r, nil
		}
	}
}

// Who lists the team, and says whether this laptop is online: offline, the list may be out of date.
func Who() (string, bool, error) {
	st, err := Settled()
	if err != nil {
		return "", false, err
	}
	var r Reply
	if err := Call(Request{Op: "who"}, &r, 5*time.Second); err != nil {
		return "", false, err
	}
	return r.Text, st.State == "online", nil
}

// Send messages someone as the person at this laptop.
func Send(to, text string) (string, error) {
	if _, err := Settled(); err != nil {
		return "", err
	}
	var r Reply
	if err := Call(Request{Op: "send", To: to, Text: text}, &r, 10*time.Second); err != nil {
		return "", err
	}
	if !r.OK {
		return "", errors.New(r.Err)
	}
	return r.Note, nil
}

// Btw asks an agent something by the way, as the person at this laptop, and waits about two minutes at most for the
// answer. It returns the answer, or where the question stands when none came in time, or why there is none.
func Btw(to, question string) (answer, note string, err error) {
	if _, err := Settled(); err != nil { // a daemon that just started learns whether its hub carries questions
		return "", "", err
	}
	id := protocol.RandomHex(8)
	var r Reply
	if err := Call(Request{Op: "btw", ID: id, To: to, Text: question}, &r, 10*time.Second); err != nil {
		return "", "", err
	}
	if !r.OK {
		return "", "", errors.New(r.Err)
	}
	r = Reply{}
	if err := Call(Request{Op: "btw-wait", ID: id}, &r, 3*time.Minute); err != nil {
		return "", "", err
	}
	if !r.OK {
		return "", "", errors.New(r.Err)
	}
	return r.Text, r.Note, nil
}

// SetBtwAnswers saves how this laptop answers teammates' questions by the way: BtwApprove, BtwAuto, BtwAll or BtwOff.
func SetBtwAnswers(setting string) error {
	var r Reply
	if err := Call(Request{Op: "btw-answers", Text: setting}, &r, 5*time.Second); err != nil {
		return err
	}
	if !r.OK {
		return errors.New(r.Err)
	}
	return nil
}

func Status() (string, bool, error) {
	r, err := Settled()
	if err != nil {
		return "", false, err
	}
	cfg := config.Load()
	parts := []string{"murmur " + release.Version, cfg.Handle, r.State, r.Text, "hub " + cfg.Hub}
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · "), r.State == "online", nil
}
