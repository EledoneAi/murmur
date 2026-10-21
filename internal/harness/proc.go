package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// shells are what a harness may run a hook command or an MCP server through.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "fish": true,
	"tcsh": true, "csh": true, "nu": true, "pwsh": true, "xonsh": true, "elvish": true, "yash": true}

// Process is the harness process that runs a hook or an MCP server whose parent is pid: pid itself, or, when a shell
// runs the command, the first process above it that isn't a shell. Hooks don't know their session's process
// otherwise, and the daemon needs it to tell when the session is gone and which session an MCP server speaks for.
func Process(pid int) int {
	for p := pid; p > 1; {
		up, name, ok := parent(p)
		if !ok || !shells[strings.TrimPrefix(name, "-")] {
			return p
		}
		p = up
	}
	return pid
}

// StartTime is when process pid started, as the system counts it, or "" when it doesn't run: with the pid, it names
// one process, and a pid the system gave another process since doesn't match.
func StartTime(pid int) string { return started(pid) }

// StartedAt is when process pid started, by the clock, or the zero time when it doesn't run: to compare with a
// harness's record of it.
func StartedAt(pid int) time.Time {
	if pid <= 0 {
		return time.Time{}
	}
	return startedAt(pid)
}

// Executable is the program process pid runs, to run it again: the path it was started with, where the system keeps
// it, because a package manager's link (/opt/homebrew/bin/claude) outlives the version it pointed to; else the file
// the process runs. It is "" when neither is an absolute path to a file that is still there.
func Executable(pid int) string {
	if pid <= 0 {
		return ""
	}
	for _, p := range executables(pid) {
		if st, err := os.Stat(p); err == nil && filepath.IsAbs(p) && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// Args are the arguments process pid was started with, its program's name first, or nil when they can't be read.
func Args(pid int) []string {
	if pid <= 0 {
		return nil
	}
	return args(pid)
}

// Environ is the environment process pid was started with, as NAME=value, kept to the names keep accepts. ok is false
// when it can't be read. The rest is wiped as soon as it is parsed: a coding tool's session holds tokens of its own
// in its environment, which murmur must not keep.
func Environ(pid int, keep func(name string) bool) (env []string, ok bool) {
	if pid <= 0 {
		return nil, false
	}
	ok = environ(pid, func(b []byte) {
		for v := range bytes.SplitSeq(b, []byte{0}) {
			if name, _, found := bytes.Cut(v, []byte("=")); found && len(name) > 0 && keep(string(name)) {
				env = append(env, string(v))
			}
		}
	})
	return env, ok
}
