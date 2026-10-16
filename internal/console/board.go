package console

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/protocol"
)

// The board is the home screen read top to bottom: what needs you, oldest first; then each constellation, the agents
// in touch with each other; then the agents not in touch with anyone, with the overlaps between them; then the quiet
// ones, folded. It is built from one snapshot of the daemon, so it can be tested without a terminal.

const (
	touchWindow = 24 * time.Hour // agents that exchanged a message within it are in touch
	quietAfter  = 2 * time.Hour  // idle agents not in touch fold away after it
)

// agentRef is one agent on the board, with what the console derives about it.
type agentRef struct {
	a        protocol.Agent
	person   string // its person's handle
	addr     string // person/agent
	mine     bool
	online   bool // its laptop is connected
	checkout int  // 2 for a second checkout of the same branch by the same person, 3 for a third…
	st       state
}

func (r *agentRef) idleFor(now time.Time) time.Duration {
	if r.a.Since == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(r.a.Since))
}

// link is two members of a constellation that exchanged messages lately.
type link struct {
	a, b    int  // member indexes, a < b
	between bool // different people's agents: a bright line; one person's own: a dim one
	held    bool // a mute holds a message between them, waiting for you: dashed
	count   int
}

// group is a constellation: agents in touch with each other.
type group struct {
	key     string // stable across snapshots: its first member's address
	name    string
	repos   []string
	people  int
	members []*agentRef
	links   []link
	tune    tune
	needs   int // members that need you
	latest  int64
}

// tune is a constellation's harmony: whether agents changing the same file talk about it.
type tune struct {
	file string // the shared file, when there is one
	off  bool   // two members change it and never talked
}

// dissonance is two agents not in touch changing the same file: i introduces them.
type dissonance struct {
	mine  *agentRef
	other string // the other agent's address
	file  string
	more  int
}

type needKind int

const (
	needApprove  needKind = iota // one of your agents waits for you in its terminal
	needQuestion                 // a person or an agent asked you something
	needHeld                     // a mute holds a message for one of your agents
	needFailed                   // a message you sent didn't get through
)

// need is one line of Needs you.
type need struct {
	key   string
	kind  needKind
	who   string // the address it comes from: your agent, or whoever asked or sent
	to    string // for a held message: your agent it was for
	text  string
	why   string // for a failed message
	since time.Time
	ref   *agentRef
	msg   protocol.Msg
}

type board struct {
	me        string
	solo      bool // nobody else on the hub: no initials, one scope
	needs     []need
	large     bool     // more than largeTeam people: what is far from you folds into elsewhere
	elsewhere []*group // constellations folded to chords, what needs attention first
	elseLone  int      // agents not in touch, and quiet ones, left out of a large team's near view
	elseQuiet int
	groups    []*group
	lone      []*agentRef
	dissonant []dissonance
	quiet     []*agentRef
	people    int // online
	agents    int
	initials  map[string]string // handle -> its initial, two letters when two people share one
	colours   map[string]int    // handle -> its colour, as the hub numbered it; you and everyone with agents
}

// scope narrows the board: everyone, or your agents and those in touch with them.
type scope int

const (
	scopeTeam scope = iota
	scopeMine
	scopeAll  // the team with every fold opened
	scopeNear // in a large team: your agents, what they talk to, and anything in your repos
)

var scopeNames = [...]string{"Team", "Mine", "All", "Near me"}

// scopeKeys are the keys that select each scope, and scopeHot where that letter is in its name: the m of "me".
var (
	scopeKeys = [...]string{"t", "m", "a", "m"}
	scopeHot  = [...]int{0, 0, 0, 5}
)

// largeTeam is where the board starts folding what is far from you away, into Elsewhere.
const largeTeam = 10

// scopes are the scopes a team of this size offers, the default first.
func scopes(people int) []scope {
	switch {
	case people <= 1:
		return []scope{scopeMine}
	case people > largeTeam:
		return []scope{scopeNear, scopeTeam, scopeAll}
	}
	return []scope{scopeTeam, scopeMine, scopeAll}
}

func buildBoard(s ipc.Snapshot, now time.Time, sc scope) *board {
	b := &board{me: s.Me.Handle, initials: map[string]string{}, colours: map[string]int{}}
	b.assignPeople(s.Me, s.People)

	// every agent, and second checkouts
	byAddr := map[string]*agentRef{}
	var all []*agentRef
	type place struct{ person, repo, branch string }
	checkouts := map[place][]*agentRef{}
	for _, p := range s.People {
		if p.Online {
			b.people++
		}
		for _, a := range p.Agents {
			r := &agentRef{a: a, person: p.Handle, addr: p.Handle + "/" + a.Name, mine: p.Handle == s.Me.Handle, online: p.Online}
			r.st = agentState(a, r.mine)
			if !p.Online {
				r.st = gone
			}
			byAddr[r.addr] = r
			all = append(all, r)
			if a.Branch != "" && a.Tree != "" {
				k := place{p.Handle, a.Repo, a.Branch}
				checkouts[k] = append(checkouts[k], r)
			}
		}
	}
	for _, rs := range checkouts {
		trees := map[string]int{}
		slices.SortFunc(rs, func(x, y *agentRef) int { return cmp.Compare(x.a.Name, y.a.Name) })
		for _, r := range rs {
			if _, ok := trees[r.a.Tree]; !ok {
				trees[r.a.Tree] = len(trees) + 1
			}
			if n := trees[r.a.Tree]; n > 1 {
				r.checkout = n
			}
		}
	}
	b.agents = len(all)
	b.solo = len(b.colours) <= 1

	// links, from the messages this laptop saw lately between agents still running
	counts, held := map[pair]int{}, map[pair]bool{}
	latest := map[string]int64{}
	add := func(m protocol.Msg, isHeld bool) {
		if now.Sub(time.UnixMilli(m.At)) > touchWindow || byAddr[m.From] == nil || byAddr[m.To] == nil || m.From == m.To {
			return
		}
		k := pair{min(m.From, m.To), max(m.From, m.To)}
		counts[k]++
		held[k] = held[k] || isHeld
		latest[m.From], latest[m.To] = max(latest[m.From], m.At), max(latest[m.To], m.At)
	}
	for _, m := range s.Msgs {
		add(m, false)
	}
	for _, m := range s.Asks {
		add(m, true)
	}

	// constellations: the connected agents, by union-find over the links
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			parent[x] = find(p)
			return parent[x]
		}
		return x
	}
	for k := range counts {
		ra, rb := find(k.a), find(k.b)
		if ra != rb {
			parent[max(ra, rb)] = min(ra, rb)
		}
	}
	members := map[string][]*agentRef{}
	for _, r := range all {
		if _, linked := latest[r.addr]; linked {
			root := find(r.addr)
			members[root] = append(members[root], r)
		}
	}

	// overlaps: in tune inside a constellation, a dissonance between agents not in touch
	inGroup := map[string]string{}
	for root, ms := range members {
		for _, r := range ms {
			inGroup[r.addr] = root
		}
	}
	groupTune := map[string]tune{}
	for _, o := range s.Pairs {
		mine := s.Me.Handle + "/" + o.Mine
		if byAddr[mine] == nil || len(o.Files) == 0 {
			continue
		}
		file := o.Files[0]
		g1, g2 := inGroup[mine], inGroup[o.Other]
		switch {
		case g1 != "" && g1 == g2:
			talked := counts[pair{min(mine, o.Other), max(mine, o.Other)}] > 0
			t := groupTune[g1]
			if !t.off { // an off note wins over an in-tune one
				t = tune{file: file, off: !talked}
			}
			groupTune[g1] = t
		default:
			b.dissonant = append(b.dissonant, dissonance{mine: byAddr[mine], other: o.Other, file: file, more: len(o.Files) - 1 + o.More})
		}
	}

	for root, ms := range members {
		g := b.group(root, ms, counts, held, latest)
		g.tune = groupTune[root]
		b.groups = append(b.groups, g)
	}
	slices.SortFunc(b.groups, func(x, y *group) int {
		return cmp.Or(-cmp.Compare(x.needs, y.needs), cmp.Compare(rankMine(y.members), rankMine(x.members)),
			-cmp.Compare(x.latest, y.latest), cmp.Compare(x.key, y.key))
	})

	// the rest: not in touch, or quiet
	for _, r := range all {
		if _, linked := latest[r.addr]; linked {
			continue
		}
		if (r.st == idle || r.st == gone) && r.idleFor(now) > quietAfter && sc != scopeAll {
			b.quiet = append(b.quiet, r)
		} else {
			b.lone = append(b.lone, r)
		}
	}
	slices.SortFunc(b.lone, func(x, y *agentRef) int {
		return cmp.Or(cmp.Compare(rank(x), rank(y)), cmp.Compare(x.addr, y.addr))
	})
	slices.SortFunc(b.dissonant, func(x, y dissonance) int {
		return cmp.Or(cmp.Compare(x.mine.addr, y.mine.addr), cmp.Compare(x.other, y.other))
	})

	if sc == scopeMine {
		b.groups = slices.DeleteFunc(b.groups, func(g *group) bool { return rankMine(g.members) == 0 })
		b.lone = slices.DeleteFunc(b.lone, func(r *agentRef) bool { return !r.mine })
		b.quiet = slices.DeleteFunc(b.quiet, func(r *agentRef) bool { return !r.mine })
	}

	if b.large = len(b.colours) > largeTeam; b.large && sc != scopeAll {
		b.foldFar(sc)
	}
	b.needs = buildNeeds(s, byAddr, now)
	return b
}

// foldFar moves what is far from you into elsewhere: the constellations without your agents, and near you only, the
// agents outside your repos.
func (b *board) foldFar(sc scope) {
	repos := map[string]bool{}
	for _, r := range b.all() {
		if r.mine && r.a.Repo != "" {
			repos[r.a.Repo] = true
		}
	}
	near := func(r *agentRef) bool { return r.mine || (sc == scopeNear && repos[r.a.Repo]) }
	keep := b.groups[:0]
	for _, g := range b.groups {
		if slices.ContainsFunc(g.members, near) {
			keep = append(keep, g)
		} else {
			b.elsewhere = append(b.elsewhere, g)
		}
	}
	b.groups = keep
	slices.SortStableFunc(b.elsewhere, func(x, y *group) int {
		attention := func(g *group) int {
			n := 0
			for _, r := range g.members {
				if r.st.waiting() {
					n++
				}
			}
			if g.tune.off {
				n += 10
			}
			return n
		}
		return cmp.Compare(attention(y), attention(x))
	})
	if sc != scopeNear {
		return
	}
	lone := b.lone[:0]
	for _, r := range b.lone {
		if near(r) {
			lone = append(lone, r)
		} else {
			b.elseLone++
		}
	}
	b.lone = lone
	quiet := b.quiet[:0]
	for _, r := range b.quiet {
		if r.mine {
			quiet = append(quiet, r)
		} else {
			b.elseQuiet++
		}
	}
	b.quiet = quiet
}

// filterBy narrows the board to what matches the filter: agents by name, person, repo, branch or title, and the
// constellations they are in, whole. What needs you stays.
func (b *board) filterBy(f string) {
	f = strings.ToLower(strings.TrimSpace(f))
	if f == "" {
		return
	}
	hit := func(r *agentRef) bool {
		return strings.Contains(strings.ToLower(r.addr+" "+r.a.Repo+" "+r.a.Branch+" "+r.a.Title), f)
	}
	b.groups = slices.DeleteFunc(b.groups, func(g *group) bool { return !slices.ContainsFunc(g.members, hit) })
	b.lone = slices.DeleteFunc(b.lone, func(r *agentRef) bool { return !hit(r) })
	b.quiet = slices.DeleteFunc(b.quiet, func(r *agentRef) bool { return !hit(r) })
	b.dissonant = slices.DeleteFunc(b.dissonant, func(d dissonance) bool {
		return !hit(d.mine) && !strings.Contains(strings.ToLower(d.other+" "+d.file), f)
	})
}

// rank orders agents: yours first, those that need someone first among them, then working ones.
func rank(r *agentRef) int {
	n := int(r.st)
	if !r.mine {
		n += 10
	}
	return n
}

func rankMine(ms []*agentRef) int {
	n := 0
	for _, r := range ms {
		if r.mine {
			n++
		}
	}
	return n
}

// pair is two agents' addresses, the smaller first.
type pair struct{ a, b string }

func (b *board) group(root string, ms []*agentRef, counts map[pair]int, held map[pair]bool, latest map[string]int64) *group {
	slices.SortFunc(ms, func(x, y *agentRef) int { // yours first, then by person, so one person's agents sit together
		return cmp.Or(boolRank(y.mine)-boolRank(x.mine), cmp.Compare(b.colours[x.person], b.colours[y.person]),
			cmp.Compare(x.person, y.person), cmp.Compare(x.a.Name, y.a.Name))
	})
	g := &group{key: root, members: ms}
	idx := map[string]int{}
	people, repos := map[string]bool{}, map[string]bool{}
	for i, r := range ms {
		idx[r.addr] = i
		people[r.person] = true
		if r.a.Repo != "" {
			repos[r.a.Repo] = true
		}
		for _, also := range r.a.Also {
			repos[also] = true
		}
		if r.st == needsYou {
			g.needs++
		}
		g.latest = max(g.latest, latest[r.addr])
	}
	for k, n := range counts {
		i, ok1 := idx[k.a]
		j, ok2 := idx[k.b]
		if !ok1 || !ok2 {
			continue
		}
		g.links = append(g.links, link{a: min(i, j), b: max(i, j), between: ms[i].person != ms[j].person, held: held[k], count: n})
	}
	slices.SortFunc(g.links, func(x, y link) int { return cmp.Or(cmp.Compare(x.a, y.a), cmp.Compare(x.b, y.b)) })
	g.people = len(people)
	g.repos = shortRepos(slices.Sorted(maps.Keys(repos)))
	g.name = groupName(ms)
	return g
}

// groupName names a constellation from what its agents share: a branch, else the repo, else the first title.
func groupName(ms []*agentRef) string {
	branches := map[string]int{}
	for _, r := range ms {
		if b := r.a.Branch; b != "" && b != "main" && b != "master" {
			branches[b]++
		}
	}
	best, n := "", 0
	for b, c := range branches {
		if c > n || (c == n && b < best) {
			best, n = b, c
		}
	}
	if n > 1 {
		return humanize(path.Base(best))
	}
	var names []string
	for b := range branches {
		names = append(names, path.Base(b))
	}
	if shared := sharedStem(names); len(names) > 1 && len(shared) >= 3 {
		return humanize(shared)
	}
	if best != "" {
		return humanize(path.Base(best))
	}
	if r := ms[0].a.Repo; r != "" {
		return humanize(path.Base(r))
	}
	return ms[0].a.Name
}

// sharedStem is the start the names share, up to a separator: billing for billing-ocr and billing-v2.
func sharedStem(names []string) string {
	if len(names) == 0 {
		return ""
	}
	stem := names[0]
	for _, n := range names[1:] {
		i := 0
		for i < len(stem) && i < len(n) && stem[i] == n[i] {
			i++
		}
		stem = stem[:i]
	}
	if i := strings.LastIndexAny(stem, "-_/"); i >= 0 && len(stem) < len(names[0]) {
		stem = stem[:i]
	}
	return stem
}

// afterShared is what base adds to the start it shares with branch: v2 for feat/billing-v2 under feat/billing-ocr.
func afterShared(branch, base string) string {
	i := 0
	for i < len(branch) && i < len(base) && branch[i] == base[i] {
		i++
	}
	cut := strings.LastIndexAny(base[:i], "-_/")
	if rest := base[cut+1:]; rest != "" {
		return rest
	}
	return path.Base(base)
}

// humanize turns a branch or repo name into words: billing-v2 is "Billing v2".
func humanize(s string) string {
	s = strings.NewReplacer("-", " ", "_", " ").Replace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// shortRepos drops the owner from repo names, unless two repos share a name.
func shortRepos(repos []string) []string {
	seen := map[string]int{}
	for _, r := range repos {
		seen[path.Base(r)]++
	}
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r
		if seen[path.Base(r)] == 1 {
			out[i] = path.Base(r)
		}
	}
	return out
}

// assignPeople gives you and each person with agents their colour and an initial. The colour is the one the hub
// numbered them with, in the order they joined, so it is the same on every screen; two people on the board who
// share an initial get two letters.
func (b *board) assignPeople(me protocol.Person, people []protocol.Person) {
	b.colours[me.Handle] = me.Colour
	for _, p := range people {
		if p.Handle != me.Handle && len(p.Agents) > 0 {
			b.colours[p.Handle] = p.Colour
		}
	}
	first := map[string]int{}
	for h := range b.colours {
		if h != "" {
			first[strings.ToUpper(string([]rune(h)[0]))]++
		}
	}
	for h := range b.colours {
		if h == "" {
			continue
		}
		r := []rune(h)
		in := strings.ToUpper(string(r[0]))
		if first[in] > 1 && len(r) > 1 {
			in += string(r[1])
		}
		b.initials[h] = in
	}
}

// buildNeeds lists what needs you, oldest first: your agents waiting for you, questions to you, messages a mute
// holds for your agents, and messages you sent that didn't get through.
func buildNeeds(s ipc.Snapshot, byAddr map[string]*agentRef, now time.Time) []need {
	var out []need
	for _, r := range byAddr {
		if r.st == needsYou {
			out = append(out, need{key: "need:agent:" + r.addr, kind: needApprove, who: r.addr, text: waitReason(r.a),
				since: time.UnixMilli(r.a.Since), ref: r})
		}
	}
	me := s.Me.Handle
	for i, m := range s.Msgs {
		if m.To != me || m.From == me || s.Done[m.ID] {
			continue
		}
		answered := slices.ContainsFunc(s.Msgs[i+1:], func(y protocol.Msg) bool { return y.From == me && y.To == m.From })
		if !answered {
			out = append(out, need{key: "need:msg:" + m.ID, kind: needQuestion, who: m.From, text: firstLine(m.Text),
				since: time.UnixMilli(m.At), ref: byAddr[m.From], msg: m})
		}
	}
	for _, m := range s.Asks {
		out = append(out, need{key: "need:held:" + m.ID, kind: needHeld, who: m.From, to: m.To, text: firstLine(m.Text),
			since: time.UnixMilli(m.At), ref: byAddr[m.From], msg: m})
	}
	for _, m := range append(slices.Clone(s.Msgs), s.Outbox...) {
		if why, ok := s.Failed[m.ID]; ok && !s.Done[m.ID] {
			out = append(out, need{key: "need:failed:" + m.ID, kind: needFailed, who: m.To, text: firstLine(m.Text), why: why,
				since: time.UnixMilli(m.At), ref: byAddr[m.To], msg: m})
		}
	}
	slices.SortFunc(out, func(x, y need) int { return cmp.Or(x.since.Compare(y.since), cmp.Compare(x.key, y.key)) })
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// place says where an agent works, in the row's few cells: its branch, a second checkout, the branch it's stacked
// on, a detached commit, a rebase in progress, or a folder outside git.
func (st styles) place(g glyphs, r *agentRef, w int) string {
	a := r.a
	if a.Tree == "" && a.Repo != "" && a.Branch == "" {
		return st.dim.Render(fit(g.folder+" "+a.Repo, w, g))
	}
	var tail, plain string
	if r.checkout > 1 {
		sup := superscript(r.checkout)
		tail += st.lilac.Render(sup)
		plain += sup
	}
	if a.Op != "" {
		tail += st.fg.Render(" " + a.Op)
		plain += " " + a.Op
	} else if a.Base != "" && a.Base != "main" && a.Base != "master" {
		s := " " + g.stacked + " " + afterShared(a.Branch, a.Base)
		tail += st.lilac.Render(s)
		plain += s
	}
	head := a.Branch
	if a.Detached != "" {
		head = g.detached + " " + a.Detached
	}
	room := w - cellWidth(plain)
	if room < 4 {
		return st.dim.Render(fit(head, w, g))
	}
	return st.dim.Render(shortBranch(head, room, g)) + tail
}

func superscript(n int) string {
	digits := []rune("⁰¹²³⁴⁵⁶⁷⁸⁹")
	var b strings.Builder
	for _, c := range fmt.Sprint(n) {
		b.WriteRune(digits[c-'0'])
	}
	return b.String()
}
