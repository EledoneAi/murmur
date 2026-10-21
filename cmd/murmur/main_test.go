package main

import (
	"strings"
	"testing"
)

// murmur alone opens the console: no arguments is not an unknown option, nor a crash.
func TestUnknownOption(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{{nil, ""}, {[]string{"--plain"}, ""}, {[]string{"status", "--json"}, ""}, {[]string{"status", "--yes"}, "--yes"},
		{[]string{"uninstall", "-y"}, ""}, {[]string{"send", "bob", "hi"}, ""}, {[]string{"btw", "bob/b1", "--why?"}, ""},
		{[]string{"btw-answers", "auto"}, ""}, {[]string{"btw-answers", "sometimes"}, "sometimes"}} {
		cmd := ""
		if len(c.args) > 0 {
			cmd = c.args[0]
		}
		if got := unknownOption(cmd, c.args); got != c.want {
			t.Errorf("%v: %q, want %q", c.args, got, c.want)
		}
	}
}

// join takes its hub before or after its options.
func TestHubArg(t *testing.T) {
	for _, args := range [][]string{{"join", "hub.acme.com", "--relogin"}, {"join", "--relogin", "hub.acme.com"}} {
		if hub, rest := hubArg(args); hub != "hub.acme.com" || len(rest) != 2 || rest[1] != "--relogin" {
			t.Errorf("%v: %q %v", args, hub, rest)
		}
	}
	if hub, rest := hubArg([]string{"join"}); hub != "" || len(rest) != 1 {
		t.Errorf("join alone: %q %v", hub, rest)
	}
}

// murmur btw takes an agent and a question, in one argument or several, and prints the answer, or where the question
// stands when the answer is late.
func TestBtwArgs(t *testing.T) {
	if to, q, err := btwArgs([]string{"btw", "bob/b1", "what", "are", "you", "stuck", "on?"}); err != nil || to != "bob/b1" ||
		q != "what are you stuck on?" {
		t.Fatalf("%q %q %v", to, q, err)
	}
	for _, args := range [][]string{{"btw"}, {"btw", "bob/b1"}, {"btw", "bob/b1", " "}, {"btw", "", "hi"}} {
		if _, _, err := btwArgs(args); err == nil || !strings.HasPrefix(err.Error(), "usage: murmur btw AGENT QUESTION") {
			t.Errorf("%q: %v", args, err)
		}
	}
	if got := btwSays("bob/b1", "feat/x", ""); got != "feat/x" {
		t.Fatalf("answered: %q", got)
	}
	if got := btwSays("bob/b1", "", "answered; waits for bob to share it"); got !=
		"No answer from bob/b1 yet (answered; waits for bob to share it). The console (murmur) shows it when it comes." {
		t.Fatalf("late: %q", got)
	}
	for _, s := range []string{"approve", "auto", "off"} {
		if btwAnswersSaid[s] == "" {
			t.Errorf("btw-answers %s says nothing", s)
		}
	}
}

// A copy murmur runs to answer a question by the way loads no hooks of murmur's, and if it did, they'd do nothing.
func TestCopiesHooksDoNothing(t *testing.T) {
	var ran []string
	defer func(h func(string, string)) { hookFor = h }(hookFor)
	hookFor = func(h, event string) { ran = append(ran, h+" "+event) }
	t.Setenv("MURMUR_BTW", "1")
	hook([]string{"hook", "claude", "start"})
	t.Setenv("MURMUR_BTW", "")
	hook([]string{"hook", "claude", "stop"})
	if strings.Join(ran, ",") != "claude stop" {
		t.Fatalf("hooks ran: %v", ran)
	}
}
