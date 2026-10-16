package hub

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// The team page is the console's live view in a browser, for teammates in VS Code, the desktop app or on a phone:
// every person and their agents, the team's pull requests and the announcements to repos, and a form on each to
// send a message. Without JavaScript it shows the team as it was when it loaded; its script keeps it live from a
// stream of the team, which rides on the roster every laptop gets.

const (
	pageWait  = 3 * time.Second  // how long a page waits to hear that its message arrived; laptops answer at once
	heartbeat = 25 * time.Second // a quiet stream says it is still there this often, so proxies keep it open
)

// teamView is the team as one person sees it on the team page, and as its stream sends it.
type teamView struct {
	Me     string            `json:"me"`
	People []protocol.Person `json:"people"` // people with a waiting agent first, and their waiting agents first
	PRs    []pullRequest     `json:"prs"`    // oldest first
	Mine   []string          `json:"mine"`   // the viewer's running agents, which they can ask to review
	Repos  []string          `json:"repos"`  // the repos the team's agents work in, to announce to
	News   []newsItem        `json:"news"`   // the announcements still up, newest first
}

// pullRequest is a pull request one of the team's agents opened.
type pullRequest struct {
	Name   string `json:"name"` // acme/api#1822
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"` // the agent's conversation
	Person string `json:"person"`
	Agent  string `json:"agent"`
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	Since  int64  `json:"since,omitempty"` // the agent's last status change, unix ms
	Review string `json:"review"`          // the message asking an agent to review it
}

type newsItem struct {
	From  string `json:"from"`
	Repo  string `json:"repo"`
	Text  string `json:"text"`
	At    int64  `json:"at"`    // unix ms
	Until int64  `json:"until"` // unix ms, when it comes down
}

// member lets only the team's signed-in people through. A visitor who is signed out signs in first; the page's
// script, which can't, is told to reload.
func (h *Hub) member(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me := h.me(r)
		accept := r.Header.Get("Accept")
		switch {
		case me != nil:
			next(w, r, *me)
		case strings.Contains(accept, "application/json") || strings.Contains(accept, "text/event-stream"):
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "result": "You are signed out. Reload the page to sign in again."})
		default:
			back := r.URL.RequestURI()
			if r.Method != http.MethodGet { // a form sent after the sign-in expired: what it said is lost
				back = "/team"
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(back), http.StatusFound)
		}
	}
}

// teamFor is the team as a person sees it, or false once they are no longer on it.
func (h *Hub) teamFor(email string) (teamView, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.users[email]
	if u == nil || u.Disabled {
		return teamView{}, false
	}
	return h.teamLocked(u.Handle), true
}

func (h *Hub) teamLocked(me string) teamView {
	people := pageOrder(h.rosterLocked())
	return teamView{Me: me, People: people, PRs: pullRequests(people), Mine: mine(running(h.holdersLocked()), me),
		Repos: repos(people), News: h.newsLocked()}
}

// mine is what a person runs, as running lists it: an empty list rather than none.
func mine(running map[string][]string, me string) []string {
	if r := running[me]; r != nil {
		return r
	}
	return []string{}
}

// teamEvent is what the team pages' stream sends after a roster: the team, encoded once for every page, and the
// agents each person runs, the one part that differs between viewers.
type teamEvent struct {
	shared  []byte              // the people, pull requests, repos and announcements, as members of a JSON object
	running map[string][]string // by handle
}

func newTeamEvent(people []protocol.Person, holders []holder, news []newsItem) *teamEvent {
	people = slices.Clone(people) // laptops share the roster: the page orders its own copy
	for i := range people {
		people[i].Agents = slices.Clone(people[i].Agents)
	}
	people = pageOrder(people)
	v := mustJSON(struct {
		People []protocol.Person `json:"people"`
		PRs    []pullRequest     `json:"prs"`
		Repos  []string          `json:"repos"`
		News   []newsItem        `json:"news"`
	}{people, pullRequests(people), repos(people), news})
	return &teamEvent{shared: v[1 : len(v)-1], running: running(holders)}
}

// event is the team as one person sees it: teamView, as JSON, in parts that pages share.
func (e *teamEvent) event(me string) net.Buffers {
	b := append(append([]byte(`{"me":`), mustJSON(me)...), `,"mine":`...)
	return net.Buffers{append(append(b, mustJSON(mine(e.running, me))...), ','), e.shared, []byte("}")}
}

// pageOrder puts people with a waiting agent first, and their waiting agents first, and keeps only links a page
// may show.
func pageOrder(people []protocol.Person) []protocol.Person {
	for i := range people {
		p := &people[i]
		slices.SortStableFunc(p.Agents, func(a, b protocol.Agent) int {
			if waitingFor(a) && waitingFor(b) {
				return cmp.Compare(a.Since, b.Since) // the longest wait first
			}
			return last(!waitingFor(a), !waitingFor(b))
		})
		for j := range p.Agents {
			a := &p.Agents[j]
			a.PRURL = webLink(a.PRURL)
		}
	}
	slices.SortStableFunc(people, func(a, b protocol.Person) int {
		return cmp.Or(last(!slices.ContainsFunc(a.Agents, waitingFor), !slices.ContainsFunc(b.Agents, waitingFor)),
			last(len(a.Agents) == 0, len(b.Agents) == 0))
	})
	return people
}

// pullRequests lists the pull requests of the team's agents, oldest first by their agent's last change, each once.
func pullRequests(people []protocol.Person) []pullRequest {
	out := []pullRequest{}
	for _, p := range people {
		for _, a := range p.Agents {
			if a.PR != 0 || a.PRURL != "" {
				out = append(out, pullFrom(p.Handle, a))
			}
		}
	}
	slices.SortStableFunc(out, func(a, b pullRequest) int { return cmp.Compare(a.Since, b.Since) })
	seen := map[string]bool{}
	return slices.DeleteFunc(out, func(p pullRequest) bool { // two sessions on one branch share a pull request
		k := strings.ToLower(cmp.Or(p.URL, p.Name))
		dup := seen[k]
		seen[k] = true
		return dup
	})
}

// repos lists the repos the team's agents work in, each once, as the first agent spells it.
func repos(people []protocol.Person) []string {
	out := []string{}
	for _, p := range people {
		for _, a := range p.Agents {
			if a.Repo != "" && !slices.ContainsFunc(out, func(r string) bool { return strings.EqualFold(r, a.Repo) }) {
				out = append(out, a.Repo)
			}
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return out
}

// running lists the agents each person runs now, on laptops that are online, by handle.
func running(holders []holder) map[string][]string {
	out := map[string][]string{}
	for _, x := range holders {
		if x.online {
			for _, a := range x.agents {
				out[x.handle] = append(out[x.handle], a.Name)
			}
		}
	}
	for handle, names := range out {
		slices.Sort(names)
		out[handle] = slices.Compact(names)
	}
	return out
}

// newsLocked lists the announcements still up, newest first.
func (h *Hub) newsLocked() []newsItem {
	out := []newsItem{}
	up := time.Duration(h.settings.AnnounceHours) * time.Hour
	for _, a := range h.news {
		if until := a.At.Add(up); time.Now().Before(until) {
			out = append(out, newsItem{From: a.From, Repo: a.Repo, Text: a.Text, At: a.At.UnixMilli(), Until: until.UnixMilli()})
		}
	}
	return out
}

// prURL is what a pull request link must look like before the page links it or asks an agent to review it: its
// laptop wrote it, and the review message goes out in the viewer's name.
var prURL = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/([0-9]+)$`)

func pullFrom(handle string, a protocol.Agent) pullRequest {
	p := pullRequest{Number: a.PR, Title: a.Title, Person: handle, Agent: a.Name, Repo: a.Repo, Branch: a.Branch, Since: a.Since}
	if a.PR != 0 {
		p.Name = fmt.Sprintf("%s#%d", a.Repo, a.PR)
	}
	if m := prURL.FindStringSubmatch(a.PRURL); m != nil && m[2] == strconv.Itoa(a.PR) {
		p.URL = a.PRURL
		p.Review = fmt.Sprintf("Please review %s with gh pr view and gh pr diff, and send me what you find.", p.URL)
	}
	return p
}

// webLink keeps a link from a laptop only when it is a web address: a page must never link to javascript:.
func webLink(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(s, " \"'<>") {
		return ""
	}
	return s
}

// ---------------------------------------------------------------- what the page says about an agent; team.js says
// the same

// teamEvents streams the team to the page's script: once at once, then whenever the roster changes, on any instance.
func (h *Hub) teamEvents(w http.ResponseWriter, r *http.Request, me store.User) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx would hold the events back
	changed := make(chan struct{}, 1)
	changed <- struct{}{}
	h.mu.Lock()
	h.streams[changed] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.streams, changed)
		h.mu.Unlock()
	}()
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-beat.C:
			_, err = io.WriteString(w, ": still here\n\n")
		case <-changed:
			b, ok := h.teamNow(me.Email)
			if !ok { // removed from the team since the page opened
				return
			}
			b = append(append(net.Buffers{[]byte("data: ")}, b...), []byte("\n\n"))
			_, err = b.WriteTo(w)
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// teamNow is the team as a person sees it now, as JSON (which escapes <, > and &: what laptops wrote stays data), or
// false once they are no longer on it. It comes from the last roster, encoded once for every page open, or is
// worked out for this page when there is none.
func (h *Hub) teamNow(email string) (net.Buffers, bool) {
	e := h.teamPage.Load()
	h.mu.Lock()
	u := h.users[email]
	if u == nil || u.Disabled {
		h.mu.Unlock()
		return nil, false
	}
	handle := u.Handle
	var v teamView
	if e == nil {
		v = h.teamLocked(handle)
	}
	h.mu.Unlock()
	if e != nil {
		return e.event(handle), true
	}
	return net.Buffers{mustJSON(v)}, true
}

// teamPRs lists the team's pull requests, oldest first, for a signed-in browser or a laptop.
func (h *Hub) teamPRs(w http.ResponseWriter, r *http.Request) {
	email := ""
	if u := h.me(r); u != nil {
		email = u.Email
	} else if _, u, ok := h.bearer(r); ok {
		email = u.Email
	}
	v, ok := h.teamFor(email)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in to see the team's pull requests"})
		return
	}
	writeJSON(w, http.StatusOK, v.PRs)
}

// teamSend sends a message from the team page, as the person signed in, and says what became of it.
func (h *Hub) teamSend(w http.ResponseWriter, r *http.Request, me store.User) {
	to, text := strings.TrimSpace(r.FormValue("to")), r.FormValue("text")
	a := h.sendFromPage(r.Context(), me, to, text)
	status := http.StatusOK
	if !a.OK {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"ok": a.OK, "result": outcome(a)})
}

// sendFromPage sends a message from a person on the team page through the same checks and limits as one from their
// laptop, and waits a moment for the answer: the page has no laptop to hear it later.
func (h *Hub) sendFromPage(ctx context.Context, me store.User, to, text string) protocol.Frame {
	m := protocol.Msg{ID: "web-" + protocol.RandomHex(8), To: to, Text: text}
	answer := make(chan protocol.Frame, 1)
	h.mu.Lock()
	h.pages[m.ID] = answer
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pages, m.ID)
		h.mu.Unlock()
	}()
	h.send(sender{handle: me.Handle, answer: func(f protocol.Frame) {
		select {
		case answer <- f:
		default:
		}
	}}, m)
	select {
	case a := <-answer:
		return a
	case <-time.After(pageWait):
		return protocol.Frame{OK: true, Note: "sent; no answer yet from " + to}
	case <-ctx.Done():
		return protocol.Frame{Err: "the page stopped waiting"}
	}
}

// outcome says what became of a message sent from the team page, in a sentence.
func outcome(a protocol.Frame) string {
	switch {
	case !a.OK:
		return "Not sent: " + cmp.Or(a.Err, "refused") + "."
	case a.Note == "":
		return "Sent."
	}
	r, n := utf8.DecodeRuneInString(a.Note)
	return string(unicode.ToUpper(r)) + a.Note[n:] + "."
}
