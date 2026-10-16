// Package opencode is murmur's adapter for OpenCode, whose hooks are plugins: murmur's runs inside OpenCode, reports
// its sessions through `murmur hook opencode <event>`, gives agents who and send as tools of its own, and hands a
// session the messages murmur sends to its socket, even while the session is idle.
package opencode

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

const Name = "opencode"

// MinVersion is the oldest OpenCode whose plugin API murmur's plugin was written against.
const MinVersion = "1.19.0"

//go:embed plugin.js
var plugin string

type OpenCode struct{}

func (OpenCode) Name() string       { return Name }
func (OpenCode) Title() string      { return "OpenCode" }
func (OpenCode) SessionVar() string { return "" } // the plugin's tools name their session

// Dir is the folder OpenCode loads plugins from that murmur writes to: OPENCODE_CONFIG_DIR, which OpenCode loads
// after its own, or its global folder.
func Dir() string {
	if d := os.Getenv("OPENCODE_CONFIG_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "opencode")
	}
	return filepath.Join(config.Home(), ".config", "opencode")
}

func pluginPath() string { return filepath.Join(Dir(), "plugins", "murmur.js") }

func (OpenCode) Installed() bool {
	if _, err := exec.LookPath("opencode"); err == nil {
		return true
	}
	_, err := os.Stat(Dir())
	return err == nil
}

// marker is how murmur knows its plugin: a file without it is someone else's, which join never replaces.
const marker = "murmur join writes it"

// ours says whether path is murmur's plugin: a file, not a link, that carries the marker.
func ours(path string) bool {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), marker)
}

// Connected says whether murmur's plugin is in place, as join wrote it.
func (OpenCode) Connected() bool { return ours(pluginPath()) }

// Holds says the same: the plugin is all murmur adds to OpenCode.
func (o OpenCode) Holds() bool { return o.Connected() }

// Source is the plugin as join writes it, for this binary.
func Source(exe string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	sockets := filepath.Dir(config.SockPath()) // a short folder only this person can open
	return strings.NewReplacer(`"__MURMUR__"`, q(exe), `"__SOCKETS__"`, q(sockets)).Replace(plugin)
}

func (o OpenCode) Connect(exe string, _ func(string) bool) []protocol.Check {
	if err := harness.Guard("OPENCODE_CONFIG_DIR"); err != nil {
		return harness.Guarded(err)
	}
	path := pluginPath()
	if _, err := os.Lstat(path); err == nil && !ours(path) {
		return []protocol.Check{{Name: "Plugin", Detail: config.Tildify(path) + " is not murmur's: move it away, then run murmur join again"}}
	}
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = harness.ReplaceFile(path, []byte(Source(exe)), 0o644) // never through a link
	}
	if err != nil {
		return []protocol.Check{{Name: "Plugin", Detail: err.Error()}}
	}
	return append([]protocol.Check{{Name: "Plugin", OK: true, Detail: config.Tildify(path) + ": hooks, who and send"}}, version()...)
}

// Disconnect removes murmur's plugin, and nothing that isn't.
func (o OpenCode) Disconnect(string) error {
	if err := harness.Guard("OPENCODE_CONFIG_DIR"); err != nil {
		return err
	}
	if !ours(pluginPath()) {
		return nil
	}
	return os.Remove(pluginPath())
}

func (o OpenCode) Checks() []protocol.Check {
	return append([]protocol.Check{harness.Check(o.Connected(), "Plugin", config.Tildify(pluginPath())+": hooks, who and send",
		"missing: run murmur join")}, version()...)
}

func version() []protocol.Check {
	v := harness.Version("opencode")
	if v == "" {
		return nil
	}
	return []protocol.Check{harness.Check(!release.Older(v, MinVersion), "Version", v, v+": murmur's plugin needs "+MinVersion+" or later")}
}

// input is what the plugin sends a hook: its session, OpenCode's process, the socket it listens on, and the tool call.
type input struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	PID       int             `json:"pid"`
	Socket    string          `json:"socket"`
	Title     string          `json:"title"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Needs     string          `json:"needs"`
}

// Parse reads what the plugin sends. The event is murmur's own kind.
func (OpenCode) Parse(event string, stdin []byte) (harness.Event, error) {
	var in input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return harness.Event{}, err
	}
	ev := harness.Event{Kind: event, Harness: Name, SessionID: in.SessionID, PID: in.PID, Cwd: in.Cwd, Socket: in.Socket,
		Title: in.Title, Tool: in.Tool, Needs: in.Needs}
	if event == harness.BeforeTool { // the plugin has no after-tool hook: the call counts as it starts
		c, edits := harness.CallOf(in.Cwd, in.Tool, in.Args)
		ev.Call, ev.Paths = &c, edits
	}
	return ev, nil
}

// Answer is plain text, which the plugin reads: the brief on start, the reason an edit is turned back.
func (OpenCode) Answer(ev harness.Event, text string) []byte {
	if ev.Kind == harness.Start || ev.Kind == harness.BeforeTool {
		return []byte(text)
	}
	return nil
}

// Push hands a message to a session through the plugin, which prompts it: at once when it is idle, after its current
// step when it works. The prompt reaches the model as its person's would, so the message comes in an envelope that
// says where it comes from. The plugin answers whether OpenCode took it.
func (OpenCode) Push(s harness.Session, text string) error {
	if s.Socket == "" {
		return errors.New("its OpenCode has no murmur plugin listening")
	}
	c, err := net.DialTimeout("unix", s.Socket, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(harness.Marshal(map[string]string{"session": s.ID, "text": harness.Envelope(text)})); err != nil {
		return err
	}
	if u, ok := c.(*net.UnixConn); ok {
		_ = u.CloseWrite() // the end of the message
	}
	answer, err := bufio.NewReader(io.LimitReader(c, 4096)).ReadString('\n')
	if answer = strings.TrimSpace(answer); answer == "ok" {
		return nil
	} else if answer != "" {
		return errors.New("OpenCode said: " + strings.TrimPrefix(answer, "error: "))
	}
	return err
}
