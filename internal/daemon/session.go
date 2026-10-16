package daemon

import (
	"slices"
	"time"

	"github.com/EledoneAi/murmur/internal/gitscan"
	"github.com/EledoneAi/murmur/internal/harness"
	"github.com/EledoneAi/murmur/internal/protocol"
)

// A session is one agent session in a harness, read from what the harness writes anyway and from its hooks:
//   - Claude Code's ~/.claude/sessions/<pid>.json: name, folder, busy/idle/waiting, and the session's inbox socket
//   - the session's transcript: the title Claude Code gives the conversation, the PRs it opened, its tool calls and
//     how full its context is
//   - git, in the session's folder: repo, branch, files changed on the branch, the size of the change and where the
//     checkout stands
type session struct {
	harness string
	id      string
	pid     int
	name    string
	cwd     string
	app     string
	version string // the harness's version
	status  string
	since   int64
	sock    string
	started string    // the process start time the harness records, to tell a reused pid apart
	hooked  bool      // learned from its hooks, for a harness that keeps no registry of its sessions
	heard   time.Time // its last hook

	act        harness.Activity
	transcript string                       // its path, once found
	read       func(*harness.Activity) bool // reads what the transcript added since
	needs      string
	warned     map[string]bool // files whose first edit in this conversation was turned back for an overlap

	top, repo, branch, tree string
	files                   []string
	st                      gitscan.State
	also                    []string // other repos the conversation's tool calls touched
	gitAt                   time.Time
	gitBusy                 bool
}

func (s *session) key() string { return harness.Key(s.harness, s.id) }

// listed is the session as its harness knows it, for the harness to push a message to.
func (s *session) listed() harness.Session {
	return harness.Session{Harness: s.harness, ID: s.id, PID: s.pid, Name: s.name, Cwd: s.cwd, Status: s.status, App: s.app,
		Version: s.version, Socket: s.sock, Transcript: s.transcript}
}

func (s *session) agent(withFiles bool) protocol.Agent {
	a := protocol.Agent{Name: s.name, Repo: s.repo, Branch: s.branch, Title: s.act.Title, PR: s.act.PR, PRURL: s.act.PRURL,
		App: s.app, Status: s.status, Needs: s.needs, Since: s.since, Doing: s.act.Doing,
		Kinds: s.act.Kinds, Loop: s.act.Loop, Context: s.act.Context,
		Added: s.st.Added, Deleted: s.st.Deleted, Changed: s.st.Changed,
		Base: s.st.Base, Detached: s.st.Detached, Op: s.st.Op, Dir: s.st.Dir, Also: slices.Clone(s.also)}
	if withFiles {
		a.Tree, a.Files = s.tree, slices.Clone(s.files)
	}
	a.Clean() // titles, actions and branches come from the model and from git: this laptop's console prints them too
	return a
}
