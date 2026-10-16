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
	Op      string          `json:"op"`
	Event   string          `json:"event,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	PID     int             `json:"pid,omitempty"`
	Session string          `json:"session,omitempty"`
	To      string          `json:"to,omitempty"`
	Text    string          `json:"text,omitempty"`
	ID      string          `json:"id,omitempty"`
	IDs     []string        `json:"ids,omitempty"`
	Socket  string          `json:"socket,omitempty"` // a session's inbox, from its SessionStart hook
	// Term is where a session runs, from its SessionStart hook's environment: its tmux pane, iTerm2 session, WezTerm
	// pane or kitty window, and the terminal program, so the console can switch to it.
	Term map[string]string `json:"term,omitempty"`
}

type Reply struct {
	OK      bool   `json:"ok"`
	Note    string `json:"note,omitempty"`
	Err     string `json:"err,omitempty"`
	Text    string `json:"text,omitempty"`
	State   string `json:"state,omitempty"`
	Version string `json:"version,omitempty"` // the daemon's own, on status
	Update  string `json:"update,omitempty"`  // on status: the team's version, when this laptop can update to it
}

func Dial() (net.Conn, error) { return net.DialTimeout("unix", config.SockPath(), time.Second) }

// self is the binary this program started from. On Linux the running binary follows a rename: once an update moves
// it aside to murmur.prev, os.Executable names the old version, which must not start again.
var self = config.Exe()

// EnsureDaemon starts the background process unless it already runs. It returns once the daemon answers.
func EnsureDaemon() error {
	if c, err := Dial(); err == nil {
		c.Close()
		return nil
	}
	return StartDaemon(self)
}

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
