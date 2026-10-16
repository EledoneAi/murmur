package hookish

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/EledoneAi/murmur/internal/harness"
)

// ---------------------------------------------------------------- Codex's config.toml

// murmur changes config.toml only in tables of its own, and keeps everything else as its person wrote it: comments,
// blank lines, multi-line strings. A file murmur changes is copied first, once, to config.toml.before-murmur.

func (s *Spec) tomlPath() string { return filepath.Join(s.dir(), s.tomlFile) }

// tomlBlock is one table of a TOML file: its header's name, "" for the lines before the first, whether it is an
// element of an array of tables ([[name]]), and its lines, the header first.
type tomlBlock struct {
	name  string
	array bool
	lines []string
}

var tomlHeader = regexp.MustCompile(`^\s*(\[\[?)\s*([^\[\]]+?)\s*\]\]?\s*(#.*)?$`)

// tomlBlocks splits a TOML file into its tables. A line inside a multi-line string never starts one.
func tomlBlocks(text string) []tomlBlock {
	bs := []tomlBlock{{}}
	quote := "" // the multi-line string a line is in, if any
	for _, line := range strings.Split(text, "\n") {
		if quote == "" {
			if m := tomlHeader.FindStringSubmatch(line); m != nil {
				bs = append(bs, tomlBlock{name: m[2], array: m[1] == "[["})
			}
		}
		b := &bs[len(bs)-1]
		b.lines = append(b.lines, line)
		for _, q := range []string{`"""`, `'''`} {
			if (quote == "" || quote == q) && strings.Count(line, q)%2 == 1 {
				if quote == q {
					quote = ""
				} else {
					quote = q
				}
			}
		}
	}
	return bs
}

func joinBlocks(bs []tomlBlock) string {
	var lines []string
	for _, b := range bs {
		lines = append(lines, b.lines...)
	}
	return strings.Join(lines, "\n")
}

func tomlBlank(line string) bool   { return strings.TrimSpace(line) == "" }
func tomlComment(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "#") }

// without drops the blocks drop names. Consecutive tables of murmur's go as one: the blank line put before them goes
// too, and the blank lines and comments after the last stay, for they introduce the next table. Comments between
// them stay as well.
func without(bs []tomlBlock, drop func(i int) bool) ([]tomlBlock, bool) {
	var out []tomlBlock
	found := false
	for i, b := range bs {
		if !drop(i) {
			out = append(out, b)
			continue
		}
		found = true
		if n := len(out); n > 0 && (i == 0 || !drop(i-1)) {
			if last := &out[n-1]; len(last.lines) > 0 && tomlBlank(last.lines[len(last.lines)-1]) {
				last.lines = last.lines[:len(last.lines)-1]
			}
		}
		tail := trailing(b)
		if i+1 < len(bs) && drop(i+1) {
			tail = slices.DeleteFunc(slices.Clone(tail), tomlBlank)
		}
		if len(tail) > 0 {
			out = append(out, tomlBlock{lines: tail})
		}
	}
	return out, found
}

// trailing are the blank lines and comments at the end of a table, after its last key.
func trailing(b tomlBlock) []string {
	end := len(b.lines)
	for end > 1 && (tomlBlank(b.lines[end-1]) || tomlComment(b.lines[end-1])) {
		end--
	}
	return b.lines[end:]
}

// editTOML rewrites a TOML file with change, which returns the new text and whether it changed it. It never creates
// the file, and keeps a copy of it as it was before murmur first changed it.
func editTOML(path string, change func(text string) (string, bool)) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	text, changed := change(string(b))
	if strings.HasSuffix(string(b), "\n") {
		text = strings.TrimRight(text, "\n") + "\n"
	}
	if !changed || text == string(b) {
		return nil
	}
	if _, err := os.Stat(path + ".before-murmur"); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(path+".before-murmur", b, 0o600)
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return harness.WriteAtomic(path, []byte(text), mode)
}

// dropTables takes out the tables under prefix, [prefix] and [prefix.…].
func dropTables(path, prefix string) error {
	return editTOML(path, func(text string) (string, bool) {
		bs := tomlBlocks(text)
		bs, found := without(bs, func(i int) bool {
			return bs[i].name == prefix || strings.HasPrefix(bs[i].name, prefix+".")
		})
		return joinBlocks(bs), found
	})
}

// codexEvents are the events Codex hooks, as config.toml names their tables: [[hooks.SessionStart]]…
var codexEvents = regexp.MustCompile(`^hooks\.(SessionStart|SessionEnd|UserPromptSubmit|PreToolUse|PermissionRequest|PostToolUse|` +
	`PreCompact|PostCompact|SubagentStart|SubagentStop|Stop|Interrupt)(\.hooks)?$`)

// tomlHasHooks says whether the person keeps hooks inline in config.toml. hooks.state, where Codex keeps which hooks
// are trusted, isn't one.
func tomlHasHooks(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, blk := range tomlBlocks(string(b)) {
		if codexEvents.MatchString(blk.name) {
			return true
		}
	}
	return false
}

var tomlCommand = regexp.MustCompile(`^\s*command\s*=\s*("(?:[^"\\]|\\.)*"|'[^']*')\s*(#.*)?$`)

// tomlString reads a TOML string: basic, with escapes, or literal.
func tomlString(v string) string {
	if strings.HasPrefix(v, "'") {
		return strings.Trim(v, "'")
	}
	s, _ := strconv.Unquote(v)
	return s
}

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
			b.WriteString(`\u` + strconv.FormatInt(int64(0x10000+r), 16)[1:])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlElements finds, for one event, the blocks of each element of its array: [[hooks.E]] and the
// [[hooks.E.hooks]] after it. It returns the first and last block of each, and whether it is murmur's.
func tomlElements(bs []tomlBlock, event string) (spans [][2]int, ours []bool) {
	for i := 0; i < len(bs); i++ {
		if !bs[i].array || bs[i].name != "hooks."+event {
			continue
		}
		j, mine := i, false
		for j+1 < len(bs) && strings.HasPrefix(bs[j+1].name, "hooks."+event+".") {
			j++
		}
		for _, b := range bs[i : j+1] {
			for _, l := range b.lines {
				if m := tomlCommand.FindStringSubmatch(l); m != nil && harness.Ours(tomlString(m[1])) {
					mine = true
				}
			}
		}
		spans, ours = append(spans, [2]int{i, j}), append(ours, mine)
		i = j
	}
	return spans, ours
}

func tomlHasOurHooks(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	bs := tomlBlocks(string(b))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "PermissionRequest", "SessionEnd"} {
		if _, ours := tomlElements(bs, ev); slices.Contains(ours, true) {
			return true
		}
	}
	return false
}

// tomlElement is murmur's element for one event, as config.toml writes it.
func (s *Spec) tomlElement(exe string, h hook) []string {
	head := []string{"[[hooks." + h.event + "]]"}
	if h.matcher != "" {
		head = append(head, "matcher = "+tomlQuote(h.matcher))
	}
	body := []string{"[[hooks." + h.event + ".hooks]]", `type = "command"`, "command = " + tomlQuote(s.commandFor(exe, h)),
		"timeout = " + strconv.Itoa(s.timeout(h))}
	if h.async {
		body = append(body, "async = true")
	}
	return append(append(head, ""), body...)
}

// editTOMLHooks puts murmur's hooks inline in config.toml (or with add=false takes them out): an element of murmur's
// is rewritten in its place, which keeps the trust Codex gave the person's hooks after it, and a new one goes at the
// end.
func editTOMLHooks(s *Spec, exe string, add bool) error {
	return editTOML(s.tomlPath(), func(text string) (string, bool) {
		bs := tomlBlocks(text)
		replace, gone, drop := map[int][]string{}, map[int]bool{}, map[int]bool{}
		changed := false
		var tail []string
		for _, h := range s.hooks {
			spans, ours := tomlElements(bs, h.event)
			kept := false
			for k, o := range ours {
				if !o {
					continue
				}
				changed = true
				first, last := spans[k][0], spans[k][1]
				if add && !kept { // in its place, before the comments that introduce the next table
					kept = true
					replace[first] = append(s.tomlElement(exe, h), trailing(bs[last])...)
					for i := first + 1; i <= last; i++ {
						gone[i] = true
					}
					continue
				}
				for i := first; i <= last; i++ {
					drop[i] = true
				}
			}
			if add && !kept {
				tail = append(append(tail, ""), s.tomlElement(exe, h)...)
			}
		}
		var mid []tomlBlock
		var dropped []bool
		for i, b := range bs {
			switch {
			case replace[i] != nil:
				mid, dropped = append(mid, tomlBlock{name: b.name, array: true, lines: replace[i]}), append(dropped, false)
			case !gone[i]:
				mid, dropped = append(mid, b), append(dropped, drop[i])
			}
		}
		mid, _ = without(mid, func(i int) bool { return dropped[i] })
		out := joinBlocks(mid)
		if len(tail) > 0 {
			out = strings.TrimRight(out, "\n") + "\n" + strings.Join(tail, "\n")
			changed = true
		}
		return out, changed
	})
}

// ---------------------------------------------------------------- Codex's MCP server

// codexApprovals let Codex run murmur's two tools without asking. `codex mcp add` writes the server's table and
// drops these, so they follow it each time; TOML takes a table's sub-tables anywhere in the file.
const codexApprovals = `
[mcp_servers.murmur.tools.who]
approval_mode = "approve"

[mcp_servers.murmur.tools.send]
approval_mode = "approve"
`

// codexMCP registers (or with add=false removes) murmur's MCP server in Codex. Codex starts MCP servers with a bare
// environment, so a MURMUR_HOME of the person's own goes with it. Removing it takes murmur's tables out of
// config.toml even without codex on PATH, and touches the file only when they are there.
func codexMCP(s *Spec, exe string, add bool) error {
	toml := s.tomlPath()
	codex, lerr := exec.LookPath("codex")
	if lerr == nil && codexMCPRegistered(codex) {
		_ = exec.Command(codex, "mcp", "remove", "murmur").Run()
	}
	if err := dropTables(toml, "mcp_servers.murmur"); err != nil || !add {
		return err
	}
	if lerr != nil {
		return errors.New("codex not found on PATH; run: codex mcp add murmur -- " + exe + " mcp codex")
	}
	args := []string{"mcp", "add", "murmur"}
	if home := os.Getenv("MURMUR_HOME"); home != "" {
		args = append(args, "--env", "MURMUR_HOME="+home)
	}
	if out, err := exec.Command(codex, append(args, "--", exe, "mcp", "codex")...).CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return editTOML(toml, func(text string) (string, bool) {
		return strings.TrimRight(text, "\n") + "\n" + codexApprovals, true
	})
}

func codexMCPRegistered(codex string) bool {
	return exec.Command(codex, "mcp", "get", "murmur").Run() == nil
}

func codexMCPConnected(s *Spec) bool {
	if codex, err := exec.LookPath("codex"); err == nil && codexMCPRegistered(codex) {
		return true
	}
	b, _ := os.ReadFile(s.tomlPath())
	for _, blk := range tomlBlocks(string(b)) {
		if blk.name == "mcp_servers.murmur" || strings.HasPrefix(blk.name, "mcp_servers.murmur.") {
			return true
		}
	}
	return false
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
