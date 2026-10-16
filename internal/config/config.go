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
)

type Config struct {
	Hub    string `json:"hub"`
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
	if d := os.Getenv("MURMUR_HOME"); d != "" {
		return d
	}
	return filepath.Join(Home(), ".murmur")
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

// ErrNoHub is returned when this laptop doesn't know its team's hub yet.
var ErrNoHub = errors.New("no hub set: run the install line from your team's hub, or murmur init --hub URL")

func Load() Config {
	c := Config{}
	if b, err := os.ReadFile(filepath.Join(Dir(), "config.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if h := os.Getenv("MURMUR_HUB"); h != "" {
		c.Hub = h
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
