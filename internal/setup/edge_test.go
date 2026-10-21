package setup

import (
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/harness/claude"
	"github.com/EledoneAi/murmur/v2/internal/harness/hookish"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
)

// A hook's input as large as a tool call makes it: a file of a few megabytes written at once, a command's long
// output after it ran. The hook still reads its session and answers.
func TestLargeHookInput(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	big := strings.Repeat("x", 3<<20)
	fakeDaemon(t, &ipc.Reply{OK: true, Text: "bob/b1 is also changing x.go"})
	write := `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/work/app/x.go","content":"` + big + `"}}`
	if out := run(claude.Claude{}, "edit", write); string(out) != string(claude.Deny("bob/b1 is also changing x.go")) {
		t.Fatalf("a large write: %.200s", out)
	}
	after := `{"session_id":"c1","cwd":"/work/app","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"cat big.log"},"tool_response":"` + big + `"}`
	if out := run(hookish.Codex, "after-tool", after); !strings.Contains(string(out), "bob/b1") {
		t.Fatalf("a long output: %.200s", out)
	}
}

// A hub's address as people paste it: a scheme in capitals is the same address, and one murmur can't add its paths
// to, or would show a password from, is refused before anything signs in.
func TestHubAddressAsPasted(t *testing.T) {
	for in, want := range map[string]string{"HTTPS://Hub.Acme.com": "https://Hub.Acme.com", "Https://hub.acme.com/": "https://hub.acme.com",
		"  hub.acme.com:8443/ ": "https://hub.acme.com:8443", "https://hub.acme.com/murmur/": "https://hub.acme.com/murmur"} {
		if got := hubAddress(in); got != want || checkHub(got) != nil {
			t.Errorf("hubAddress(%q) = %q (%v), want %q", in, got, checkHub(got), want)
		}
	}
	for _, bad := range []string{"https://hub.acme.com/?next=/", "https://hub.acme.com/#/team", "https://hub.acme.com/#",
		"https://me:secret@hub.acme.com", "https://me@hub.acme.com", "https://me:secret@hub.acme.com/?next=/"} {
		err := checkHub(hubAddress(bad))
		if err == nil {
			t.Errorf("%s was accepted", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the refusal shows the password: %v", bad, err)
		}
	}
	// a join line that reads as the team's hub and signs in to another
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("MURMUR_HUB", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := Join("hub.acme.com@evil.example", false, true); err == nil || !strings.Contains(err.Error(), "the hub at evil.example") {
		t.Fatalf("join hub.acme.com@evil.example: %v", err)
	}
}
