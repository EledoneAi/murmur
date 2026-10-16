// Package ipc is the local socket between the daemon and the programs that talk to it: the console, the MCP server
// and one-shot commands.
package ipc

import (
	"bufio"
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

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
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
}

// ---------------------------------------------------------------- talking to the daemon

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

// EnsureDaemon starts the background process unless it already runs. It returns once the daemon answers. A daemon
// older than this binary, still running after an upgrade the daemon didn't make itself (Homebrew's), is stopped,
// and this version's starts and sets the laptop up again.
func EnsureDaemon() error {
	var r Reply
	err := CallRunning(Request{Op: "status"}, &r, time.Second)
	switch {
	case err != nil:
		if c, derr := Dial(); derr == nil { // it answers, but slowly: leave it be
			c.Close()
			return nil
		}
	case release.Dev() || r.Version == "" || !release.Older(r.Version, release.Version):
		return nil
	default:
		_ = CallRunning(Request{Op: "stop"}, &Reply{}, 2*time.Second)
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
	c, err := Dial()
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	line, err := bufio.NewReaderSize(c, 1<<16).ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, out)
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

func Status() (string, bool, error) {
	r, err := Settled()
	if err != nil {
		return "", false, err
	}
	cfg := config.Load()
	parts := []string{"murmur " + release.Version, cfg.Handle, r.State, r.Text, "hub " + cfg.Hub}
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · "), r.State == "online", nil
}
