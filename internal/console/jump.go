package console

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/internal/ipc"
)

// g goes to the terminal where one of your agents runs, to answer it there: its tmux pane, iTerm2 session, WezTerm
// pane or kitty window. The console switches itself, from your environment; other terminals don't let a program
// do it, and the console says where the agent is instead.

var (
	tmuxPane  = regexp.MustCompile(`^%[0-9]+$`)
	itermID   = regexp.MustCompile(`^[0-9A-Fa-f-]{36}$`)
	paneID    = regexp.MustCompile(`^[0-9]+$`)
	appByTerm = map[string]string{"vscode": "VS Code", "Apple_Terminal": "Terminal", "ghostty": "Ghostty", "iTerm.app": "iTerm2",
		"WezTerm": "WezTerm", "kitty": "kitty"}
)

// jumpCmd is the command that switches to a terminal, as its session's hook saw it, or why there is none.
func jumpCmd(term map[string]string) (*exec.Cmd, error) {
	if pane := term["TMUX_PANE"]; tmuxPane.MatchString(pane) {
		args := []string{}
		if sock, _, _ := strings.Cut(term["TMUX"], ","); sock != "" {
			args = append(args, "-S", sock)
		}
		// switch-client fails when the console isn't in tmux; the pane's window becomes current anyway
		args = append(args, "select-window", "-t", pane, ";", "select-pane", "-t", pane, ";", "switch-client", "-t", pane)
		return exec.Command("tmux", args...), nil
	}
	if _, id, ok := strings.Cut(term["ITERM_SESSION_ID"], ":"); ok && itermID.MatchString(id) {
		script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if unique id of s is "%s" then
					select w
					select t
					select s
					activate
					return
				end if
			end repeat
		end repeat
	end repeat
end tell`, id)
		return exec.Command("osascript", "-e", script), nil
	}
	if pane := term["WEZTERM_PANE"]; paneID.MatchString(pane) {
		return exec.Command("wezterm", "cli", "activate-pane", "--pane-id", pane), nil
	}
	if win := term["KITTY_WINDOW_ID"]; paneID.MatchString(win) {
		args := []string{"@"}
		if to := term["KITTY_LISTEN_ON"]; to != "" {
			args = append(args, "--to", to)
		}
		return exec.Command("kitty", append(args, "focus-window", "--match", "id:"+win)...), nil
	}
	if app := appByTerm[term["TERM_PROGRAM"]]; app != "" {
		return nil, fmt.Errorf("it runs in %s, which doesn't let murmur switch to it", app)
	}
	return nil, fmt.Errorf("murmur doesn't know how to switch to its terminal")
}

// goTo asks the daemon where an agent runs, and switches there.
func (m *model) goTo(r *agentRef) tea.Cmd {
	name := r.a.Name
	return func() tea.Msg {
		if m.demo {
			return doneMsg{text: "In murmur demo there is no terminal to go to", err: true}
		}
		rep, err := m.call(ipc.Request{Op: "terminal", To: name}, 3*time.Second)
		if err != nil || !rep.OK {
			why := rep.Err
			if err != nil {
				why = err.Error()
			}
			return doneMsg{text: "Can't go to " + name + ": " + why, err: true}
		}
		var term map[string]string
		_ = json.Unmarshal([]byte(rep.Text), &term)
		cmd, err := jumpCmd(term)
		if err != nil {
			return doneMsg{text: "Can't go to " + name + ": " + err.Error(), err: true}
		}
		if out, err := cmd.CombinedOutput(); err != nil && cmd.Path != "tmux" && !strings.HasSuffix(cmd.Path, "/tmux") {
			return doneMsg{text: "Can't go to " + name + ": " + strings.TrimSpace(string(out)+" "+err.Error()), err: true}
		}
		return doneMsg{text: "Went to " + name + "'s terminal"}
	}
}
