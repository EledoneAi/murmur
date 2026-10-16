package mcp

import (
	"strings"
	"testing"
)

// Agents read the team's agreements in the server's instructions, and the plain instructions when the daemon
// doesn't run or the team has none.
func TestInstructions(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	if instructions() != mcpInstructions {
		t.Fatal("instructions without a daemon")
	}
	if withAgreements(" \n") != mcpInstructions {
		t.Fatal("instructions for a team without agreements")
	}
	if got := withAgreements("Keep pull requests small.\n"); !strings.HasPrefix(got, mcpInstructions) || !strings.HasSuffix(got, "follows:\nKeep pull requests small.") {
		t.Fatalf("with agreements: %q", got)
	}
}
