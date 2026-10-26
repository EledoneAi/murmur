package daemon

import (
	"cmp"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/gitscan"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// A session is one agent session in a harness, read from what the harness writes anyway and from its hooks:
//   - Claude Code's ~/.claude/sessions/<pid>.json: the name its person gave it, if any, the folder it started in,
//     busy/idle/waiting, and the session's inbox socket
//   - the session's transcript: the title Claude Code gives the conversation, the PRs it opened, its tool calls and
//     how full its context is
//   - git, in the folder the session works in now (dir): repo, branch, files changed on the branch, the size of the
//     change and where the checkout stands
type session struct {
	k       string // its key, when it isn't harness.Key(harness, id): the same conversation open in a second process
	harness string
	id      string
	pid     int
	name    string
	cwd     string // the folder it started in, as its harness lists it: a copy answering by the way runs there
	at      string // the folder it works in now, once it moved out of cwd (followLocked)
	wd      string // the folder its harness last said it works in, so only a change moves it
	app     string
	version string // the harness's version
	status  string
	since   int64
	sock    string
	started string    // the process start time the harness records, to tell a reused pid apart
	proc    string    // its process's start time as harness.StartTime read it when the daemon took the session in
	hooked  bool      // learned from its hooks, for a harness that keeps no registry of its sessions
	heard   time.Time // its last hook

	act        harness.Activity
	transcript string                       // its path, once found
	read       func(*harness.Activity) bool // reads what the transcript added since
	needs      string
	warned     map[string]bool // files whose first edit in this conversation was turned back for an overlap

	newTitle string // a copy's title for the conversation, since its focus moved on (retitleLocked)
	oldTitle string // the harness's title it replaced, which shows again once the harness changes it
	// what the title showing was last checked against: the repo and branch, the prompts its person had sent, and when
	titleRepo, titleBranch string
	titlePrompts           int
	titleAt                time.Time
	ended                  time.Time // when its last turn ended, until git has read the checkout since

	top, repo, branch, tree string
	files                   []string
	st                      gitscan.State
	also                    []string // other repos the conversation's tool calls touched
	gitAt                   time.Time
	gitBusy                 bool
	gitAgain                bool // a read was asked for while one ran, or just after: the next tick reads
}

func (s *session) key() string {
	if s.k != "" {
		return s.k
	}
	return harness.Key(s.harness, s.id)
}

// dir is the folder the session works in now: where it moved, or where it started.
func (s *session) dir() string { return cmp.Or(s.at, s.cwd) }

// title is the conversation's title: a copy's since the session's focus moved on, until its harness or its person
// titles it again.
func (s *session) title() string {
	if s.newTitle != "" && s.act.Title == s.oldTitle && !s.act.Custom {
		return s.newTitle
	}
	return s.act.Title
}

// listed is the session as its harness knows it, for the harness to push a message to.
func (s *session) listed() harness.Session {
	return harness.Session{Harness: s.harness, ID: s.id, PID: s.pid, Started: s.proc, Name: s.name, Cwd: s.cwd, Status: s.status,
		App: s.app, Version: s.version, Socket: s.sock, Transcript: s.transcript}
}

func (s *session) agent(withFiles bool) protocol.Agent {
	a := protocol.Agent{Name: s.name, Repo: s.repo, Branch: s.branch, Title: s.title(),
		App: s.app, Status: s.status, Needs: s.needs, Since: s.since, Doing: s.act.Doing,
		Kinds: s.act.Kinds, Loop: s.act.Loop, Context: s.act.Context,
		Added: s.st.Added, Deleted: s.st.Deleted, Changed: s.st.Changed,
		Base: s.st.Base, Detached: s.st.Detached, Op: s.st.Op, Dir: s.st.Dir, Also: slices.Clone(s.also)}
	// the conversation's last pull request, while it works in that PR's repo, known by its name: a fork's origin has
	// another owner than the repo its pull requests go to
	if s.act.PRRepo == "" || strings.EqualFold(path.Base(s.act.PRRepo), path.Base(s.repo)) {
		a.PR, a.PRURL = s.act.PR, s.act.PRURL
	}
	if withFiles {
		a.Tree, a.Files = s.tree, slices.Clone(s.files)
	}
	a.Clean() // titles, actions and branches come from the model and from git: this laptop's console prints them too
	return a
}
