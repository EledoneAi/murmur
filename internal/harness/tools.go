package harness

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// Harnesses name their tools differently, and none of the names collide: Claude Code's Edit, Codex's apply_patch,
// Gemini CLI's replace, OpenCode's edit. One table reads them all.
var toolKinds = map[string]byte{}

func init() {
	for kind, names := range map[byte][]string{
		protocol.KindRead: {"Read", "Grep", "Glob", "LS", "NotebookRead", "LSP", "read_file", "read_many_files", "grep_search",
			"search_file_content", "glob", "list_directory", "view", "read", "grep", "rg", "lsp"},
		protocol.KindEdit: {"Edit", "MultiEdit", "Write", "NotebookEdit", "apply_patch", "write_file", "replace", "edit", "create",
			"write", "str_replace_editor", "Delete"},
		protocol.KindRun:      {"Bash", "Shell", "run_shell_command", "bash", "shell", "exec_command", "powershell"},
		protocol.KindWeb:      {"WebFetch", "WebSearch", "web_fetch", "google_web_search", "web_search", "webfetch", "websearch"},
		protocol.KindSubagent: {"Task", "Agent", "Subagent", "spawn_agent", "invoke_agent", "task"},
		protocol.KindMessage:  {"mcp__murmur__send", "mcp_murmur_send", "murmur-send", "murmur_send", "MCP:send"},
	} {
		for _, n := range names {
			toolKinds[n] = kind
		}
	}
}

// toolInput is what murmur reads of a tool call's input, under the names the harnesses give it.
type toolInput struct {
	Command      json.RawMessage `json:"command"` // a string; some harnesses send an array
	FilePath     string          `json:"file_path"`
	FilePathCase string          `json:"filePath"`
	Path         string          `json:"path"`
	AbsPath      string          `json:"absolute_path"`
	NotebookPath string          `json:"notebook_path"`
	DirPath      string          `json:"dir_path"`
	Patch        string          `json:"patchText"`
	Input        string          `json:"input"`
}

func (in toolInput) command() string {
	var s string
	if json.Unmarshal(in.Command, &s) == nil {
		return s
	}
	var argv []string
	_ = json.Unmarshal(in.Command, &argv)
	return strings.Join(argv, " ")
}

func (in toolInput) file() string {
	for _, p := range []string{in.FilePath, in.FilePathCase, in.AbsPath, in.NotebookPath, in.Path} {
		if p != "" {
			return p
		}
	}
	return ""
}

// readers are the tools that read one file; the other KindRead tools search.
var readers = map[string]bool{"Read": true, "NotebookRead": true, "read_file": true, "view": true, "read": true}

// CallOf reads a tool call: what it does, in murmur's words, and the files it would change, absolute, when it is an
// edit. input is the tool's arguments as JSON; cwd makes relative paths absolute.
func CallOf(cwd, tool string, input json.RawMessage) (Call, []string) {
	var in toolInput
	_ = json.Unmarshal(input, &in)
	c := Call{Kind: toolKinds[tool], Doing: tool}
	var edits []string
	switch c.Kind {
	case protocol.KindRun:
		cmd := in.command()
		c.Doing, c.Shell = Running(cmd), Shell(cmd)
	case protocol.KindEdit:
		if tool == "apply_patch" || in.Patch != "" {
			edits = PatchFiles(cwd, firstOf(in.Patch, in.Input, in.command()))
		} else if f := in.file(); f != "" {
			edits = []string{Abs(cwd, f)}
		}
		c.Doing = "Editing"
		if len(edits) > 0 {
			c.Doing += " " + filepath.Base(edits[0])
		}
		if len(edits) > 1 {
			c.Doing += " and more"
		}
		c.Dirs = Dirs(cwd, edits...)
	case protocol.KindRead:
		c.Doing = "Searching"
		if f := in.file(); readers[tool] && f != "" {
			c.Doing = "Reading " + filepath.Base(f)
		}
		c.Dirs = Dirs(cwd, in.file())
		if in.DirPath != "" {
			c.Dirs = append(c.Dirs, Abs(cwd, in.DirPath))
		}
	case protocol.KindWeb:
		c.Doing = "Reading a web page"
		if strings.Contains(strings.ToLower(tool), "search") {
			c.Doing = "Searching the web"
		}
	case protocol.KindSubagent:
		c.Doing = "Delegating"
	case protocol.KindMessage:
		c.Doing = "murmur · send"
	}
	switch {
	case tool == "TodoWrite" || tool == "update_plan" || tool == "write_todos" || tool == "todowrite":
		c.Doing = "Updating its plan"
	case c.Kind == 0 && strings.HasPrefix(tool, "mcp__"):
		if srv, name, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__"); ok {
			c.Doing = srv + " · " + name
		}
	}
	return c, edits
}

func firstOf(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// PatchFiles are the files a patch in Codex's format adds, changes, deletes or moves to, absolute.
func PatchFiles(cwd, patch string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(patch))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, head := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if p, ok := strings.CutPrefix(line, head); ok && strings.TrimSpace(p) != "" {
				out = append(out, Abs(cwd, strings.TrimSpace(p)))
			}
		}
	}
	return out
}

// Version runs `command --version` and returns the version it prints: the last word of its first line that starts
// with a digit, as in "codex-cli 0.163.0" or "1.0.98". "" when the command isn't on PATH.
func Version(command string) string {
	path, err := exec.LookPath(command)
	if err != nil {
		return ""
	}
	out, _ := exec.Command(path, "--version").Output()
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	v := ""
	for _, w := range strings.Fields(first) {
		if w = strings.TrimPrefix(w, "v"); w != "" && w[0] >= '0' && w[0] <= '9' {
			v = w
		}
	}
	return v
}
