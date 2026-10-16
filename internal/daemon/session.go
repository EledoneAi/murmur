package daemon

import (
	"slices"
	"time"

	"github.com/EledoneAi/murmur/internal/claudecode"
	"github.com/EledoneAi/murmur/internal/gitscan"
	"github.com/EledoneAi/murmur/internal/protocol"
)

// A session is one running Claude Code process, read from what Claude Code writes anyway:
//   - ~/.claude/sessions/<pid>.json: name, folder, busy/idle/waiting, and the session's inbox socket
//   - the session's transcript: the title Claude Code gives the conversation, the PRs it opened, its tool calls and
//     how full its context is
//   - git, in the session's folder: repo, branch, files changed on the branch, the size of the change and where the
//     checkout stands
type session struct {
	pid     int
	id      string
	name    string
	cwd     string
	app     string
	cc      string // Claude Code's version
	status  string
	since   int64
	sock    string
	started string // the process start time Claude Code records, to tell a reused pid apart

	tr     claudecode.Transcript
	needs  string
	warned map[string]bool // files whose first edit in this conversation was turned back for an overlap

	top, repo, branch, tree string
	files                   []string
	st                      gitscan.State
	also                    []string // other repos the conversation's tool calls touched
	gitAt                   time.Time
	gitBusy                 bool
}

func (s *session) agent(withFiles bool) protocol.Agent {
	a := protocol.Agent{Name: s.name, Repo: s.repo, Branch: s.branch, Title: s.tr.Title(), PR: s.tr.PR, PRURL: s.tr.PRURL,
		App: s.app, Status: s.status, Needs: s.needs, Since: s.since, Doing: s.tr.Doing,
		Kinds: s.tr.Kinds, Loop: s.tr.Loop, Context: s.tr.Context,
		Added: s.st.Added, Deleted: s.st.Deleted, Changed: s.st.Changed,
		Base: s.st.Base, Detached: s.st.Detached, Op: s.st.Op, Dir: s.st.Dir, Also: slices.Clone(s.also)}
	if withFiles {
		a.Tree, a.Files = s.tree, slices.Clone(s.files)
	}
	a.Clean() // titles, actions and branches come from the model and from git: this laptop's console prints them too
	return a
}
