package hookish

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// ---------------------------------------------------------------- Codex's MCP server, in config.toml

// murmur registers its MCP server in Codex's config.toml itself, as one block at the end of the file between two
// comment lines, the second carrying a digest of what is between them. It replaces or removes that block only when
// it is exactly what murmur wrote, and reads nothing else in the file but a search for another server called murmur.
// Anything else (a block someone changed, a server murmur didn't write) it leaves alone, and says what to do by hand.
// `codex mcp add` isn't used: Codex rewrites every MCP server's table when it runs, losing its person's comments.

const (
	blockStart = "# murmur's MCP server, which murmur join writes and murmur uninstall removes as written: change it and murmur leaves it alone"
	blockEnd   = "# end of murmur's block, "
)

func (s *Spec) tomlPath() string { return filepath.Join(s.dir(), s.tomlFile) }

// tomlQuote writes s as a TOML basic string.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexServer is murmur's server, as config.toml's tables: the command, and its two tools approved, so Codex runs
// them without asking. Codex starts MCP servers with a bare environment, so a MURMUR_HOME of the person's own goes
// with it.
func codexServer(exe string) string {
	var b strings.Builder
	b.WriteString("[mcp_servers.murmur]\ncommand = " + tomlQuote(exe) + "\nargs = [\"mcp\", \"codex\"]\n")
	if os.Getenv("MURMUR_HOME") != "" {
		b.WriteString("\n[mcp_servers.murmur.env]\nMURMUR_HOME = " + tomlQuote(config.Dir()) + "\n")
	}
	b.WriteString("\n[mcp_servers.murmur.tools.who]\napproval_mode = \"approve\"\n\n[mcp_servers.murmur.tools.send]\napproval_mode = \"approve\"\n")
	return b.String()
}

func digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:12])
}

// codexBlock is murmur's block, markers included.
func codexBlock(exe string) string {
	body := codexServer(exe)
	return blockStart + "\n" + body + blockEnd + digest(body) + "\n"
}

// otherServer finds a server called murmur in config.toml that isn't in murmur's block: a table, dotted keys or an
// inline table. A false alarm only sends the person to do it by hand.
var otherServer = regexp.MustCompile(`(?m)^\s*(\[\s*mcp_servers\s*\.\s*"?murmur"?\s*[\].]|mcp_servers\s*\.\s*"?murmur"?\s*[.=]|"?murmur"?\s*=\s*\{)`)

// block is where murmur's block is in text: its first and last byte, as written (intact) or not. found says the
// file has a marker of murmur's; other says it has a server called murmur outside the block.
type block struct {
	start, end   int
	found, whole bool
	other        bool
}

func findBlock(text string) block {
	var b block
	i := lineAt(text, 0, blockStart)
	if i < 0 {
		b.found = strings.Contains(text, blockEnd) // the end of a block whose start someone took out
		b.other = otherServer.MatchString(text)
		return b
	}
	b.found = true
	body := i + len(blockStart) + 1
	j := lineAt(text, body, blockEnd)
	if j < 0 {
		b.other = otherServer.MatchString(text[:i])
		return b
	}
	end := len(text)
	if k := strings.IndexByte(text[j:], '\n'); k >= 0 {
		end = j + k + 1
	}
	b.start, b.end = i, end
	b.whole = strings.TrimSuffix(text[j:end], "\n") == blockEnd+digest(text[body:j])
	b.other = otherServer.MatchString(text[:i] + text[end:])
	return b
}

// lineAt is where the first line at or after from that starts with prefix begins, or -1.
func lineAt(text string, from int, prefix string) int {
	for i := from; i < len(text); {
		if (i == 0 || text[i-1] == '\n') && strings.HasPrefix(text[i:], prefix) {
			return i
		}
		k := strings.IndexByte(text[i:], '\n')
		if k < 0 {
			return -1
		}
		i += k + 1
	}
	return -1
}

// byHandTOML is what to do in config.toml when murmur won't change it.
func byHandTOML(path, exe string, add bool, why string) error {
	if add {
		return fmt.Errorf("%s %s: give it this server by hand, or take that out and run murmur join again:\n%s", config.Tildify(path), why, codexServer(exe))
	}
	return fmt.Errorf("%s %s: take murmur's MCP server out of it by hand", config.Tildify(path), why)
}

// codexMCP registers (or with add=false removes) murmur's MCP server in Codex's config.toml.
func codexMCP(s *Spec, exe string, add bool) error {
	path := s.tomlPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if !add {
			return nil
		}
		return harness.WriteAtomic(path, []byte(codexBlock(exe)), 0o600)
	} else if err != nil {
		return err
	}
	text := string(b)
	blk := findBlock(text)
	switch {
	case blk.found && !blk.whole:
		return byHandTOML(path, exe, add, "has murmur's block, changed since murmur wrote it")
	case blk.other:
		return byHandTOML(path, exe, add, "has an MCP server called murmur that murmur didn't write")
	case !blk.found && !add:
		return nil
	}
	var out string
	switch {
	case blk.found && add: // in its place
		out = text[:blk.start] + codexBlock(exe) + text[blk.end:]
	case blk.found: // with the blank line murmur put before it
		before := text[:blk.start]
		if strings.HasSuffix(before, "\n\n") {
			before = before[:len(before)-1]
		}
		out = before + text[blk.end:]
	default:
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		out = text + codexBlock(exe)
	}
	if out == string(b) {
		return nil
	}
	if _, err := os.Stat(path + ".before-murmur"); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(path+".before-murmur", b, 0o600)
	}
	return harness.WriteAtomic(path, []byte(out), 0o600)
}

// codexMCPConnected says whether config.toml has murmur's block, or any server called murmur.
func codexMCPConnected(s *Spec) bool {
	b, _ := os.ReadFile(s.tomlPath())
	blk := findBlock(string(b))
	return blk.found || blk.other
}

// inlineHooks finds hooks kept in config.toml: [[hooks.SessionStart]] and the like. hooks.state, where Codex keeps
// which hooks it trusts, isn't one.
var inlineHooks = regexp.MustCompile(`(?m)^\s*\[\[?\s*hooks\s*\.\s*(SessionStart|SessionEnd|UserPromptSubmit|PreToolUse|PermissionRequest|PostToolUse|PreCompact|PostCompact|SubagentStart|SubagentStop|Stop|Interrupt)\b`)

// codexInline says whether the person keeps Codex hooks inline in config.toml, besides murmur's in hooks.json.
func codexInline(s *Spec) bool {
	b, _ := os.ReadFile(s.tomlPath())
	return inlineHooks.Match(b)
}

// ---------------------------------------------------------------- Codex's thread names

// codexTitle is the name its person gave a Codex thread with /rename, from session_index.jsonl, where the newest line
// for an id wins. Codex gives threads no title of its own.
func codexTitle(s *Spec, id string) string {
	f, err := os.Open(filepath.Join(s.dir(), "session_index.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 1<<20 {
		_, _ = f.Seek(-1<<20, io.SeekEnd)
	}
	title := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(id)) {
			continue
		}
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID == id {
			title = e.Name
		}
	}
	return title
}
