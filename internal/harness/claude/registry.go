package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// ---------------------------------------------------------------- registry

type Entry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	StatusAt  int64  `json:"statusUpdatedAt"`
	Socket    string `json:"messagingSocketPath"`
	ProcStart string `json:"procStart"`
	Entry     string `json:"entrypoint"`
	Version   string `json:"version"` // Claude Code's version in that session
}

// AppOf names where a session runs. The VS Code and Desktop apps run the same Claude Code, so their sessions
// register, get an inbox and pick up the hooks and MCP server exactly like a terminal session.
func AppOf(entrypoint string) string {
	switch {
	case entrypoint == "claude-vscode":
		return "vscode"
	case entrypoint == "claude-desktop":
		return "desktop"
	case strings.HasPrefix(entrypoint, "sdk"):
		return "sdk"
	}
	return ""
}

// Dir is Claude Code's configuration folder: CLAUDE_CONFIG_DIR, or ~/.claude.
func Dir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(config.Home(), ".claude")
}

func RegistryDir() string { return filepath.Join(Dir(), "sessions") }

// PIDOf turns ".../sessions/12345.json" into 12345.
func PIDOf(path string) (int, bool) {
	base := filepath.Base(path)
	n, ok := strings.CutSuffix(base, ".json")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(n)
	return pid, err == nil && pid > 0
}

func ReadEntry(path string) (Entry, error) {
	var e Entry
	b, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	if e.PID == 0 || e.SessionID == "" {
		return e, errors.New("incomplete entry")
	}
	return e, nil
}

func (Claude) Watch() string { return RegistryDir() }

// Sessions lists the live sessions in Claude Code's registry: one file per process, which Claude Code rewrites as
// the session's status changes.
func (Claude) Sessions() []harness.Session {
	files, _ := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	var out []harness.Session
	for _, f := range files {
		pid, ok := PIDOf(f)
		if !ok {
			continue
		}
		e, err := ReadEntry(f)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			out = append(out, harness.Session{Harness: Name, PID: pid}) // half-written: read again on its next event
		case harness.Alive(e.PID) && e.Kind != "daemon":
			out = append(out, harness.Session{Harness: Name, ID: e.SessionID, PID: pid, Started: e.ProcStart, Name: e.Name,
				Cwd: e.Cwd, Status: e.Status, Since: e.StatusAt, App: AppOf(e.Entry), Version: e.Version, Socket: e.Socket})
		}
	}
	return out
}

// ---------------------------------------------------------------- transcript

func FindTranscript(id string) string {
	m, _ := filepath.Glob(filepath.Join(Dir(), "projects", "*", id+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// Open finds a session's transcript: the path its start hook named, or the one Claude Code keeps under its id.
func (Claude) Open(s harness.Session) (string, func(*harness.Activity) bool) {
	p := s.Transcript
	if p == "" {
		p = FindTranscript(s.ID)
	}
	if _, err := os.Stat(p); p == "" || err != nil {
		return "", nil // Claude Code writes it with the first message, after the start hook named it
	}
	t := &Transcript{Path: p}
	return p, t.Read
}
