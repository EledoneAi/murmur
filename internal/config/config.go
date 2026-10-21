// Package config is where murmur keeps its files on a laptop, and the laptop's settings.
package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Hub string `json:"hub"`
	// Solo is set by murmur solo: the daemon runs a hub of its own at Hub, on this laptop only, and signs its person
	// in to it whenever it starts.
	Solo   bool   `json:"solo,omitempty"`
	Token  string `json:"token,omitempty"`
	Email  string `json:"email,omitempty"`
	Handle string `json:"handle,omitempty"`
	Name   string `json:"name,omitempty"`
	// Updates is this laptop's own update preference: "" follows the team's policy, "ask" waits for
	// `murmur update`, "off" never updates on its own.
	Updates string `json:"updates,omitempty"`
	// Notifications is "off" when this laptop's person doesn't want desktop notifications.
	Notifications string `json:"notifications,omitempty"`
	// Agreements are the team's, as its hub last sent them: sessions that start before the daemon reaches the hub
	// get them too.
	Agreements string `json:"agreements,omitempty"`
	// Dirs are the coding tools' folder variables as join saw them, "" for unset: the daemon uses these, whatever
	// environment the process that started it had (Codex clears its MCP servers').
	Dirs map[string]string `json:"dirs,omitempty"`
}

// DirVars are the variables that name the coding tools' folders.
var DirVars = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "CURSOR_CONFIG_DIR", "OPENCODE_CONFIG_DIR", "XDG_CONFIG_HOME",
	"COPILOT_HOME", "GEMINI_CLI_HOME"}

// UseDirs sets the coding tools' folder variables as join saw them, when it recorded them.
func UseDirs(c Config) {
	for k, v := range c.Dirs {
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
}

func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		h = os.Getenv("HOME")
	}
	return h
}

// Dir holds the config, the daemon's socket, its log and the message history: nothing else is written anywhere
// except the hook and MCP entries in Claude Code's own settings.
func Dir() string {
	d := os.Getenv("MURMUR_HOME")
	switch {
	case d == "":
		return filepath.Join(Home(), ".murmur")
	case filepath.IsAbs(d):
		return d
	}
	// not absolute, such as "~/murmur" in a settings file no shell expands: from the home folder, the same for the
	// hooks, the daemon and the MCP server, which each run in a folder of their own
	if d == "~" || strings.HasPrefix(d, "~/") {
		d = d[1:]
	}
	return filepath.Join(Home(), d)
}

// SockPath is the daemon's socket. Unix socket paths are capped near 104 bytes, so a long MURMUR_HOME falls
// back to a short path in a folder only this user can open.
func SockPath() string {
	p := filepath.Join(Dir(), "sock")
	if len(p) < 100 {
		return p
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("murmur-%d", os.Getuid()))
	_ = os.Mkdir(dir, 0o700)
	if !private(dir) {
		return p // too long to bind, so the daemon fails loudly instead of listening where others can reach it
	}
	sum := sha256.Sum256([]byte(Dir()))
	return filepath.Join(dir, fmt.Sprintf("%x.sock", sum[:4]))
}

// private says whether dir is a real folder owned by this user that nobody else can enter.
func private(dir string) bool {
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}

// updateSkipped names a version that failed to update on this laptop: the daemon doesn't try it again for an hour,
// and a newer binary doesn't replace the daemon that came back from it.
func updateSkipped() string { return filepath.Join(Dir(), "update-skipped") }

const skipFor = time.Hour

// SkipUpdate records that version v failed on this laptop.
func SkipUpdate(v string) { _ = os.WriteFile(updateSkipped(), []byte(v), 0o600) }

// FailedUpdate names a version that failed on this laptop in the last hour, for status and the console, or "".
func FailedUpdate() string {
	st, err := os.Stat(updateSkipped())
	if err != nil || time.Since(st.ModTime()) > skipFor {
		return ""
	}
	b, _ := os.ReadFile(updateSkipped())
	return string(b)
}

// UpdateSkipped says whether version v failed on this laptop lately.
func UpdateSkipped(v string) bool { return v != "" && FailedUpdate() == v }

// ErrNoHub is returned when this laptop doesn't know its team's hub yet.
var ErrNoHub = errors.New("not in a team yet: run murmur join with your team's hub, such as murmur join murmur.example.com")

func Load() Config {
	c := Config{}
	if b, err := os.ReadFile(filepath.Join(Dir(), "config.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if h := os.Getenv("MURMUR_HUB"); h != "" && !c.Solo {
		c.Hub = strings.TrimRight(h, "/") // murmur adds its paths after it
	}
	return c
}

func Save(c Config) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := filepath.Join(Dir(), ".config.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(Dir(), "config.json"))
}

// Exe is the running binary, with symlinks resolved: what hooks, the MCP server and updates point at. A package
// manager keeps each version in its own folder and links the current one on PATH, so for those the link on PATH is
// what stays valid after an upgrade.
func Exe() string {
	exe, err := os.Executable()
	if err != nil {
		return "murmur"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if !versioned(exe) {
		return exe
	}
	if onPath, err := exec.LookPath("murmur"); err == nil {
		if r, err := filepath.EvalSymlinks(onPath); err == nil && r == exe {
			return onPath
		}
	}
	return exe
}

// versioned says whether a binary sits in a package manager's folder for one version.
func versioned(exe string) bool {
	for _, p := range []string{"/Cellar/", "/Caskroom/", "/nix/store/"} {
		if strings.Contains(exe, p) {
			return true
		}
	}
	return false
}

// Tildify shortens a path under the home folder to ~/…, for messages people read.
func Tildify(p string) string {
	if rest, ok := strings.CutPrefix(p, Home()); ok {
		return "~" + rest
	}
	return p
}
