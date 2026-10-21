package hookish

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// A config.toml as people write it, with what trips a reader that goes line by line: comments and strings holding
// quotes and brackets, keys with brackets, arrays of strings on their own lines.
const codexConfig = `# Codex settings. """ and ''' in a comment
model = "gpt-5" # ''' again
notify = ["python3", "/x/notify.py"]
doc = 'a single """ in a literal'

[profiles."fast [beta]"]
model = "mini"

[mcp_servers.docs]
command = "docs"
args = [
  "--flag",
  "[mcp_servers.murmur]",
]
description = """
see [mcp_servers.murmur], not a table
"""
`

// parseTOML reads a file as Codex would, with a real TOML parser.
func parseTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	var v map[string]any
	if _, err := toml.DecodeFile(path, &v); err != nil {
		t.Fatalf("%s is not valid TOML: %v\n%s", path, err, read(t, path))
	}
	return v
}

func murmurServer(v map[string]any) map[string]any {
	servers, _ := v["mcp_servers"].(map[string]any)
	m, _ := servers["murmur"].(map[string]any)
	return m
}

// murmur's MCP server goes in a block of its own at the end of config.toml, which murmur replaces in its place and
// removes only as it wrote it. Everything else in the file stays as its person wrote it, and the file stays valid
// TOML. No codex command is needed: sandbox leaves PATH empty.
func TestCodexConfig(t *testing.T) {
	sandbox(t)
	t.Setenv("MURMUR_HOME", "/home/me/.murmur")
	path := filepath.Join(Codex.dir(), "config.toml")
	_ = os.WriteFile(path, []byte(codexConfig), 0o600)
	parseTOML(t, path)
	if err := codexMCP(Codex, "/opt/murmur", true); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	m := murmurServer(parseTOML(t, path))
	tools, _ := m["tools"].(map[string]any)
	who, _ := tools["who"].(map[string]any)
	btw, _ := tools["btw"].(map[string]any)
	env, _ := m["env"].(map[string]any)
	if !strings.HasPrefix(got, codexConfig+"\n") || m["command"] != "/opt/murmur" || who["approval_mode"] != "approve" || btw["approval_mode"] != "approve" ||
		env["MURMUR_HOME"] != "/home/me/.murmur" || !Codex.mcpConnected() {
		t.Fatalf("config.toml:\n%s", got)
	}
	if read(t, path+".before-murmur") != codexConfig {
		t.Fatal("no copy of the file as it was")
	}

	// the person adds a table after murmur's block; joining again rewrites the block where it is
	mine := "\n[profiles.slow]\nmodel = \"big\"\n"
	_ = os.WriteFile(path, []byte(got+mine), 0o600)
	if err := codexMCP(Codex, "/usr/local/bin/murmur", true); err != nil {
		t.Fatal(err)
	}
	if m := murmurServer(parseTOML(t, path)); m["command"] != "/usr/local/bin/murmur" || !strings.HasSuffix(read(t, path), mine) {
		t.Fatalf("joined again:\n%s", read(t, path))
	}
	if err := codexMCP(Codex, "", false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != codexConfig+mine {
		t.Fatalf("after uninstall:\n%q\nwant\n%q", got, codexConfig+mine)
	}
	parseTOML(t, path)

	// a block someone changed, or a server called murmur that murmur didn't write: murmur leaves the file alone
	if err := codexMCP(Codex, "/opt/murmur", true); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(read(t, path), `approval_mode = "approve"`, `approval_mode = "prompt"`, 1)
	theirs := codexConfig + "\n[mcp_servers.murmur]\ncommand = \"/old/murmur\"\nargs = [\"mcp\"]\n"
	for _, text := range []string{changed, theirs} {
		_ = os.WriteFile(path, []byte(text), 0o600)
		for _, add := range []bool{true, false} {
			if err := codexMCP(Codex, "/opt/murmur", add); err == nil || !strings.Contains(err.Error(), "by hand") || read(t, path) != text {
				t.Fatalf("add %v on\n%s\n: %v", add, text, err)
			}
		}
	}

	// what looks like a server called murmur, even inside a string, is never edited around: by hand
	lookalike := codexConfig + "notes = \"\"\"\n[mcp_servers.murmur]\n\"\"\"\n"
	_ = os.WriteFile(path, []byte(lookalike), 0o600)
	if err := codexMCP(Codex, "/opt/murmur", true); err == nil || read(t, path) != lookalike {
		t.Fatalf("a lookalike in a string: %v", err)
	}

	// no config.toml yet: murmur's block is the file
	os.Remove(path)
	if err := codexMCP(Codex, "/opt/murmur", true); err != nil {
		t.Fatal(err)
	}
	parseTOML(t, path)
	if err := codexMCP(Codex, "", false); err != nil || read(t, path) != "" {
		t.Fatalf("uninstall of a file murmur created: %v, %q", err, read(t, path))
	}
}

// A settings file the person keeps as a link, to their dotfiles, stays a link: murmur writes its target.
func TestLinkedSettings(t *testing.T) {
	sandbox(t)
	dotfiles := filepath.Join(t.TempDir(), "codex-config.toml")
	_ = os.WriteFile(dotfiles, []byte("model = \"gpt\"\n"), 0o600)
	path := filepath.Join(Codex.dir(), "config.toml")
	_ = os.Symlink(dotfiles, path)
	gemini := filepath.Join(t.TempDir(), "gemini-settings.json")
	_ = os.WriteFile(gemini, []byte("{\n  \"theme\": \"Dracula\"\n}\n"), 0o644)
	_ = os.MkdirAll(filepath.Dir(Gemini.hooksPath()), 0o755)
	_ = os.Symlink(gemini, Gemini.hooksPath())
	Codex.Connect("/opt/murmur", nil)
	Gemini.Connect("/opt/murmur", nil)
	for link, target := range map[string]string{path: dotfiles, Gemini.hooksPath(): gemini} {
		if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a link", link)
		}
		if !strings.Contains(read(t, target), "murmur") {
			t.Fatalf("%s wasn't written", target)
		}
	}
	if err := Codex.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	if err := Gemini.Disconnect(""); err != nil {
		t.Fatal(err)
	}
	if read(t, dotfiles) != "model = \"gpt\"\n" || read(t, gemini) != "{\n  \"theme\": \"Dracula\"\n}\n" {
		t.Fatalf("after uninstall: %q %q", read(t, dotfiles), read(t, gemini))
	}
}

// Codex hooks always go in hooks.json; a person who keeps theirs inline in config.toml hears that Codex will warn.
func TestCodexInlineHooksNote(t *testing.T) {
	sandbox(t)
	path := filepath.Join(Codex.dir(), "config.toml")
	_ = os.WriteFile(path, []byte("[[hooks.PreToolUse]]\nmatcher = \"^Bash$\"\n\n[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = 'policy.py'\n"), 0o600)
	checks := Codex.Connect("/opt/murmur", nil)
	if !checks[0].OK || !strings.Contains(checks[0].Detail, "hooks.json") ||
		!slices.ContainsFunc(checks, func(c protocol.Check) bool { return strings.Contains(c.Detail, "Codex warns that both") }) {
		t.Fatalf("connect: %+v", checks)
	}
	parseTOML(t, path)
}
