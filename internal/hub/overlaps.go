package hub

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// checkout is one agent as the overlap index holds it.
type checkout struct {
	handle string
	agent  protocol.Agent
}

func (c checkout) addr() string { return c.handle + "/" + c.agent.Name }

// key names a checkout for overlaps: its tree when the laptop says it, else the agent, so six agents in one checkout
// count once.
func (c checkout) key() string { return cmp.Or(c.agent.Tree, c.addr()) }

// perFile caps the checkouts the index keeps for one file. A file thirty checkouts change at once is not news to any
// of them, and the cap keeps one person's made-up agents from making every roster quadratic.
const perFile = 32

// overlapIndex maps each file an online agent changes, by repo, to the checkouts changing it. The hub builds it
// once per roster push, outside its lock, and answers the consoles' overlaps, laptops' checks and the weekly counts
// from it.
type overlapIndex struct {
	files map[string][]*checkout // lower-case repo + "\x00" + path
	mine  map[string][]*checkout // a person's online agents, by handle
}

func newIndex(holders []holder, s Settings) *overlapIndex {
	x := &overlapIndex{files: map[string][]*checkout{}, mine: map[string][]*checkout{}}
	for _, h := range holders {
		if !h.online {
			continue
		}
		for _, a := range h.agents {
			c := &checkout{h.handle, a} // one for all its files
			x.mine[h.handle] = append(x.mine[h.handle], c)
			if a.Repo == "" {
				continue
			}
			repo := strings.ToLower(a.Repo) + "\x00"
			for _, f := range a.Files {
				k := repo + f
				if len(x.files[k]) < perFile && !s.Ignored(f) {
					x.files[k] = append(x.files[k], c)
				}
			}
		}
	}
	return x
}

// others lists, for a checkout and its files, the other checkouts of the repo changing them: other checkout ->
// the files in common, in the order given.
func (x *overlapIndex) others(asking checkout, files []string) map[string]*overlap {
	out := map[string]*overlap{}
	repo := strings.ToLower(asking.agent.Repo)
	if repo == "" {
		return out
	}
	mine := asking.key()
	for _, f := range files {
		for _, c := range x.files[repo+"\x00"+f] {
			k := c.key()
			if k == mine || (c.handle == asking.handle && c.agent.Name == asking.agent.Name) {
				continue
			}
			o := out[k]
			if o == nil {
				o = &overlap{with: c}
				out[k] = o
			}
			if n := len(o.files); n > 0 && o.files[n-1] == f {
				continue // another agent of that checkout: the file counts once
			}
			o.files = append(o.files, f)
		}
	}
	return out
}

type overlap struct {
	with  *checkout
	files []string
}

// maxPairs caps one person's overlaps on a roster.
const maxPairs = 50

// current lists the overlaps one person's agents have right now, one per pair of checkouts. It rides on that
// person's roster, so the console drops an overlap as soon as either side pushes or reverts the files. Agents learn
// of an overlap from check instead, when they are about to edit one of the files.
func (x *overlapIndex) current(handle string) []protocol.Overlap {
	// pair of checkouts, this person's first -> its overlap; the smallest by the agents and files it names wins, so it
	// stays put between rosters
	best := map[[2]string]protocol.Overlap{}
	for _, a := range x.mine[handle] {
		for k, o := range x.others(*a, a.agent.Files) {
			shown := o.files[:min(len(o.files), 3)]
			p := protocol.Overlap{Mine: a.agent.Name, Other: o.with.addr(), Files: shown, More: len(o.files) - len(shown)}
			pair := [2]string{a.key(), k}
			if old, ok := best[pair]; !ok || smaller(p, old) < 0 {
				best[pair] = p
			}
		}
	}
	out := slices.SortedFunc(maps.Values(best), smaller)
	return out[:min(len(out), maxPairs)]
}

// smaller orders overlaps by the agents they name, then their files.
func smaller(a, b protocol.Overlap) int {
	return cmp.Or(strings.Compare(a.Mine, b.Mine), strings.Compare(a.Other, b.Other), slices.Compare(a.Files, b.Files))
}

func fileList(files []string) string {
	shown := files[:min(len(files), 3)]
	list := strings.Join(shown, ", ")
	if more := len(files) - len(shown); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return list
}
