package hub

import (
	"fmt"
	"slices"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// check answers a laptop that asks whether other checkouts of a repo change some files: before one of its agents
// edits a file, and when a session starts. It follows the console's rules (shared), for agents on every instance,
// with one line per agent: "bob/main-1 (feat/x) is also changing api/routes.go". Agents hear of an overlap only
// this way, when it matters to what they do; the hub pushes them nothing.
func (h *Hub) check(l *link, f protocol.Frame) {
	asking := protocol.Agent{Repo: f.Repo, Tree: f.Tree, Files: f.Files[:min(len(f.Files), 300)]}
	asking.Clean() // before taking the lock, like agent fields
	var lines []string
	for _, o := range h.overlapIndex().others(checkout{l.handle, asking}, asking.Files) {
		lines = append(lines, fmt.Sprintf("%s%s is also changing %s", o.with.addr(), onBranch(o.with.agent), fileList(o.files)))
	}
	slices.Sort(lines)
	if len(lines) > 0 {
		h.log.Info("overlap reported", "person", l.handle, "repo", asking.Repo, "agents", len(lines))
	}
	l.send(protocol.Frame{T: protocol.FrameChecked, ID: protocol.Clean(protocol.Cut(f.ID, 64), false), Overlaps: lines})
}

// onBranch is " (feat/x)" for an agent on a known branch.
func onBranch(a protocol.Agent) string {
	if a.Branch == "" {
		return ""
	}
	return " (" + a.Branch + ")"
}
