package main

import "testing"

// murmur alone opens the console: no arguments is not an unknown option, nor a crash.
func TestUnknownOption(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{{nil, ""}, {[]string{"--plain"}, ""}, {[]string{"status", "--json"}, ""}, {[]string{"status", "--yes"}, "--yes"},
		{[]string{"uninstall", "-y"}, ""}, {[]string{"send", "bob", "hi"}, ""}} {
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
