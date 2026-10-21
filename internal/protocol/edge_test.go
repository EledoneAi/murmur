package protocol

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Whatever bytes arrive, Clean hands back valid UTF-8: a lone byte from 0x80 to 0x9f is a C1 control, CSI and OSC
// among them, to a terminal that doesn't read UTF-8.
func TestCleanAlwaysValidUTF8(t *testing.T) {
	for _, s := range []string{"\xff", "ok\x9b2J", "\x9d0;title\x9c", "caf\xc3", "a\x01\xff", "\xed\xa0\x80"} {
		for _, lines := range []bool{false, true} {
			if got := Clean(s, lines); !utf8.ValidString(got) {
				t.Errorf("Clean(%q, %v) = %q", s, lines, got)
			}
		}
	}
}

// A run of variation selectors can carry a hidden text, one byte each: putting a zero-width joiner or an invisible
// combining mark between them doesn't keep them.
func TestCleanSelectorRunsBetweenJoiners(t *testing.T) {
	for _, glue := range []string{"\u200d", "\u200c", "\u034f"} {
		s := "ok" + strings.Repeat(glue+"\U000E0100", 40)
		if got := Clean(s, false); strings.Count(got, "\U000E0100") > 1 {
			t.Errorf("Clean kept %d selectors joined by %q", strings.Count(got, "\U000E0100"), glue)
		}
	}
	// emoji keep theirs: a selector before a joiner, a keycap, a heart
	for _, s := range []string{"❤️\u200d\U0001F525", "#️⃣", "\U0001F441️\u200d\U0001F5E8️"} {
		if got := Clean(s, false); got != s {
			t.Errorf("Clean(%q) = %q, want it whole", s, got)
		}
	}
}

// What the hub reads into every console: NUL, escapes, bidi overrides and zero-width characters go, from agents'
// fields and their files, and each field ends up within its length, never through a character.
func TestAgentCleanEveryField(t *testing.T) {
	dirty := "a\x00\x1b[2J\u202e\u200b\x1b]0;t\x07b"
	a := Agent{Name: dirty, Repo: dirty, Branch: dirty, Title: dirty, PRURL: dirty, App: dirty, Status: dirty, Needs: dirty,
		Doing: dirty, Tree: dirty, Base: dirty, Detached: dirty, Op: dirty, Dir: dirty, Files: []string{dirty}, Also: []string{dirty},
		Kinds: "rexz\x1bm", Context: 900, Added: -1, Deleted: -2, Changed: -3}
	a.Clean()
	for name, f := range map[string]string{"name": a.Name, "repo": a.Repo, "branch": a.Branch, "title": a.Title, "pr_url": a.PRURL,
		"app": a.App, "status": a.Status, "needs": a.Needs, "doing": a.Doing, "tree": a.Tree, "base": a.Base, "detached": a.Detached,
		"op": a.Op, "dir": a.Dir, "file": a.Files[0], "also": a.Also[0]} {
		if f != "ab" {
			t.Errorf("%s = %q, want %q", name, f, "ab")
		}
	}
	if a.Kinds != "rexm" || a.Context != 100 || a.Added != 0 || a.Deleted != 0 || a.Changed != 0 {
		t.Errorf("kinds %q, context %d, lines %d %d %d", a.Kinds, a.Context, a.Added, a.Deleted, a.Changed)
	}

	long := Agent{Name: strings.Repeat("é", 500), Title: strings.Repeat("葛", 500), Also: make([]string, 50)}
	long.Clean()
	if n := utf8.RuneCountInString(long.Name); n != 80 || !utf8.ValidString(long.Name) {
		t.Errorf("a long name kept %d characters", n)
	}
	if n := utf8.RuneCountInString(long.Title); n != 200 {
		t.Errorf("a long title kept %d characters", n)
	}
	if len(long.Also) != 5 {
		t.Errorf("%d other repos kept", len(long.Also))
	}
}

// Every address form names a person, lower-cased and trimmed, and an agent, trimmed.
func TestSplitAddresses(t *testing.T) {
	for addr, want := range map[string][2]string{
		"bob": {"bob", ""}, " Bob ": {"bob", ""}, "BOB / main-1 ": {"bob", "main-1"}, "bob/": {"bob", ""}, "/main-1": {"", "main-1"},
		"": {"", ""}, "bob/a/b": {"bob", "a/b"}, "murmur": {"murmur", ""},
	} {
		if p, a := Split(addr); p != want[0] || a != want[1] {
			t.Errorf("Split(%q) = %q, %q; want %q, %q", addr, p, a, want[0], want[1])
		}
	}
}

// Cut never leaves half a character, and leaves short text alone.
func TestCut(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{{"aé", 2, "a"}, {"aé", 3, "aé"}, {"abc", 0, ""}, {"\U0001F469\U0001F4BB", 5, "\U0001F469"}, {"", 4, ""}} {
		if got := Cut(c.in, c.n); got != c.want {
			t.Errorf("Cut(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
