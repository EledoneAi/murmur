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
