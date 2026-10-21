package daemon

import (
	"os/exec"
	"runtime"
	"strings"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// tell shows this person a desktop notification about message m, unless they turned notifications off. It shows
// who the message is from and its first line, never more: notifications show on locked screens and in screen shares.
func (d *Daemon) tell(who string, m protocol.Msg) {
	if d.notify == nil || config.Load().Notifications == "off" {
		return
	}
	first, _, _ := strings.Cut(strings.TrimSpace(m.Text), "\n")
	d.notify("Murmur: "+who, string([]rune(first)[:min(len([]rune(first)), 200)]))
}

// toMeFromOthers says whether m is for this person rather than one of their agents, from someone else.
func (d *Daemon) toMeFromOthers(m protocol.Msg) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	person, agent := protocol.Split(m.To)
	return agent == "" && person == d.me.Handle && m.From != protocol.System && protocol.PersonOf(m.From) != d.me.Handle
}

// desktop shows a notification with osascript on macOS, or notify-send on Linux when it is installed. Elsewhere it
// shows nothing.
func desktop(title, text string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// the texts go as arguments, never into the script, so nothing in them runs
		cmd = exec.Command("osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run", title, text)
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return
		}
		cmd = exec.Command(path, "--", markupSafe(title), markupSafe(text))
	default:
		return
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// markupSafe is text for notify-send, whose notification servers mostly read markup: what a teammate wrote shows as
// they wrote it, never as a link or an image.
var markupSafe = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
