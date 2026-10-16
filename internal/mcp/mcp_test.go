package mcp

import (
	"encoding/json"
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

// Codex names the session in each tool call's metadata: that wins over what the environment says, and a call without
// it keeps the environment's.
func TestSessionOf(t *testing.T) {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal([]byte(`{"name":"send","arguments":{"to":"bob","text":"hi"},"_meta":{"callId":"call_1",
		"x-codex-turn-metadata":{"session_id":"019a-thread","thread_id":"019a-thread","turn_id":"t1","model":"m"}}}`), &p)
	if got := sessionOf(p.Meta, "from-env"); got != "019a-thread" {
		t.Fatalf("with metadata: %q", got)
	}
	if got := sessionOf(nil, "from-env"); got != "from-env" {
		t.Fatalf("without: %q", got)
	}
}
