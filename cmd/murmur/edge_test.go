package main

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Options are checked where a command takes none but its own, wherever they come; free text never is.
func TestUnknownOptionEdges(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"who", "--json"}, "--json"},
		{[]string{"status", "--json", "extra"}, "extra"},
		{[]string{"update", "--check", "--auto"}, ""},
		{[]string{"update", "-check"}, "-check"},
		{[]string{"updates", "OFF"}, "OFF"},
		{[]string{"updates", "off"}, ""},
		{[]string{"notifications", "maybe"}, "maybe"},
		{[]string{"retitles", "off"}, ""},
		{[]string{"btw-answers", "all"}, ""},
		{[]string{"retitles", "never"}, "never"},
		{[]string{"join", "--"}, "--"},
		{[]string{"version", "--json"}, "--json"},
		{[]string{"", "extra"}, "extra"},
		{[]string{"mute", "bob", "-5m"}, ""},
		{[]string{"send", "bob", "--yes"}, ""},
		{[]string{"hook", "claude", "SessionStart"}, ""},
		{[]string{"bogus", "--yes"}, ""}, // an unknown command is refused as one
	} {
		if got := unknownOption(c.args[0], c.args); got != c.want {
			t.Errorf("%q: %q, want %q", c.args, got, c.want)
		}
	}
}

// join hands the hub on as it was typed, whatever it holds: setup checks it. A second one is an unknown option.
func TestHubArgEdges(t *testing.T) {
	for _, hub := range []string{"murmur.acme.com", "https://murmur.acme.com/", "murmur.acme.com/sub", "bob:pw@murmur.acme.com",
		"murmur acme.com", "http://127.0.0.1:8080"} {
		got, rest := hubArg([]string{"join", "--no-browser", hub, "--relogin"})
		if got != hub || !slices.Equal(rest, []string{"join", "--no-browser", "--relogin"}) {
			t.Errorf("%q: %q %q", hub, got, rest)
		}
	}
	if hub, rest := hubArg([]string{"join", "a.com", "b.com"}); hub != "a.com" || unknownOption("join", rest) != "b.com" {
		t.Errorf("two hubs: %q %q", hub, rest)
	}
	if hub, rest := hubArg([]string{"join", "--relogin", "--no-browser"}); hub != "" || len(rest) != 3 {
		t.Errorf("no hub: %q %q", hub, rest)
	}
}

// Exit codes, for scripts: 2 for what murmur doesn't understand, 1 for what it can't do, 0 otherwise. Only commands
// that never reach a daemon run here, each in a copy of this test with its own MURMUR_HOME.
func TestExitCodes(t *testing.T) {
	if args, ok := os.LookupEnv("MURMUR_TEST_ARGS"); ok {
		os.Args = append([]string{"murmur"}, strings.Split(args, "\x1f")...)
		main()
		os.Exit(0)
	}
	for _, c := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"bogus"}, 2, `unknown command "bogus"`},
		{[]string{"-v"}, 2, "unknown command"},
		{[]string{"status", "--yes"}, 2, `unknown option "--yes"`},
		{[]string{"join", "a.com", "b.com"}, 2, `unknown option "b.com"`},
		{[]string{"updates", "sometimes"}, 2, "unknown option"},
		{[]string{"send", "bob"}, 1, "usage: murmur send"},
		{[]string{"mute"}, 1, "usage: murmur mute"},
		{[]string{"mute", "bob", "2h", "extra"}, 1, "usage: murmur mute"},
		{[]string{"update"}, 1, "not in a team yet"},
		{[]string{"update", "--check"}, 1, "not in a team yet"},
		{[]string{"version"}, 0, "murmur "},
		{[]string{"help"}, 0, "murmur: your team's coding agents"},
		{[]string{"send", "--help"}, 0, "murmur: your team's coding agents"},
		{[]string{"updates", "ask"}, 0, "only when you run murmur update"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExitCodes$")
		cmd.Env = append(os.Environ(), "MURMUR_TEST_ARGS="+strings.Join(c.args, "\x1f"), "MURMUR_HOME="+t.TempDir(), "MURMUR_HUB=")
		out, err := cmd.CombinedOutput()
		code := 0
		if e := (*exec.ExitError)(nil); errors.As(err, &e) {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != c.code || !strings.Contains(string(out), c.says) {
			t.Errorf("murmur %s: exit %d, want %d, saying %q:\n%s", strings.Join(c.args, " "), code, c.code, c.says, out)
		}
	}
}
