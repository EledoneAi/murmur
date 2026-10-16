package harness

import "strings"

// shells are what a harness may run a hook command or an MCP server through.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

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
