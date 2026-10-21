package claude

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Claude Code applies its managed settings, the ones an organisation puts on a laptop, to a copy too: --setting-sources
// "" leaves them on. Two of them would let a copy use its tools: disableAllHooks turns off every hook, the one that
// turns the copy's tool calls back included, and allowManagedHooksOnly runs only the hooks managed settings name. On a
// laptop whose managed settings say either, no copy runs, and its agents can't be asked by the way.

// errHooksOff is why no copy runs on a laptop whose managed settings turn hooks off.
var errHooksOff = errors.New("your organisation's Claude Code settings turn hooks off, so murmur can't keep a copy from using tools")

// managedDirs are the folders Claude Code reads managed settings from, managed-settings.json then the .json files of
// managed-settings.d: the system's, as Claude Code 2.1 has them on macOS and Linux, and one the environment names.
// Tests replace it.
var managedDirs = func() []string {
	dirs := []string{"/etc/claude-code"}
	if runtime.GOOS == "darwin" {
		dirs = []string{"/Library/Application Support/ClaudeCode"}
	}
	if d := os.Getenv("CLAUDE_CODE_MANAGED_SETTINGS_PATH"); d != "" {
		dirs = append(dirs, d)
	}
	return dirs
}

// managedPlists are the configuration profiles a Mac's device management gives Claude Code its managed settings in:
// the user's, then the device's. Tests replace it.
var managedPlists = func() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	const name = "com.anthropic.claudecode.plist"
	var out []string
	if u, err := user.Current(); err == nil && u.Username != "" {
		out = append(out, filepath.Join("/Library/Managed Preferences", u.Username, name))
	}
	return append(out, filepath.Join("/Library/Managed Preferences", name))
}

// remoteSettings are where Claude Code keeps the managed settings an organisation's admins set on claude.ai, as it last
// fetched them: in its folder, or where the environment says.
func remoteSettings() []string {
	out := []string{filepath.Join(Dir(), "remote-settings.json")}
	if p := os.Getenv("CLAUDE_CODE_REMOTE_SETTINGS_PATH"); p != "" {
		out = append(out, p)
	}
	return out
}

// managed is what the managed settings said of hooks lately, and when.
var managed struct {
	sync.Mutex
	at  time.Time
	off bool
}

// hooksOff says whether Claude Code's managed settings turn hooks off on this laptop, as they said it within maxAge.
func hooksOff(maxAge time.Duration) bool {
	managed.Lock()
	defer managed.Unlock()
	if managed.at.IsZero() || time.Since(managed.at) >= maxAge {
		managed.off, managed.at = readHooksOff(), time.Now()
	}
	return managed.off
}

// readHooksOff reads every source of managed settings for disableAllHooks or allowManagedHooksOnly. A source that
// can't be read sets nothing, for Claude Code too.
func readHooksOff() bool {
	var docs []string
	for _, dir := range managedDirs() {
		docs = append(docs, filepath.Join(dir, "managed-settings.json"))
		entries, _ := os.ReadDir(filepath.Join(dir, "managed-settings.d"))
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") && (e.Type().IsRegular() || e.Type()&fs.ModeSymlink != 0) {
				docs = append(docs, filepath.Join(dir, "managed-settings.d", name))
			}
		}
	}
	for _, p := range append(docs, remoteSettings()...) {
		if b, err := os.ReadFile(p); err == nil && turnsHooksOff(b) {
			return true
		}
	}
	for _, p := range managedPlists() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// as Claude Code reads it: plutil turns a profile, XML or binary, into the JSON of its settings
		if b, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", "--", p).Output(); err == nil && turnsHooksOff(b) {
			return true
		}
	}
	return false
}

// hooksOffSaid finds either setting in a document that isn't JSON as Go reads it: one with comments, say.
var hooksOffSaid = regexp.MustCompile(`"(disableAllHooks|allowManagedHooksOnly)"\s*:\s*true\b`)

// turnsHooksOff says whether a settings document says disableAllHooks or allowManagedHooksOnly.
func turnsHooksOff(doc []byte) bool {
	var s map[string]json.RawMessage
	if json.Unmarshal(doc, &s) != nil {
		return hooksOffSaid.Match(doc)
	}
	return slices.ContainsFunc([]string{"disableAllHooks", "allowManagedHooksOnly"}, func(k string) bool { return string(s[k]) == "true" })
}

// CanAsk says why a copy can't run on this laptop, as the managed settings said it within the last minute, or nil: the
// daemon asks whenever it tells the hub about its agents. Ask reads them again.
func (Claude) CanAsk() error {
	if hooksOff(time.Minute) {
		return errHooksOff
	}
	return nil
}
