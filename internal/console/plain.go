package console

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// The plain mode says what the console shows, in sentences, one line at a time: no box drawing, no colour, nothing
// redrawn. Screen readers read it as it comes, and scripts can follow it. murmur watch prints the sentences;
// murmur --plain also takes commands on its input.

// Watch prints what changes in the team as sentences until it is stopped. With commands, it also reads them from in.
func Watch(out io.Writer, in io.Reader) error {
	if err := ipc.EnsureDaemon(); err != nil {
		return err
	}
	var mu sync.Mutex
	var last, listed ipc.Snapshot // the newest snapshot, and the one the needs were last numbered from
	have := false
	if in != nil {
		go func() {
			sc := bufio.NewScanner(in)
			fmt.Fprintln(out, "Type help for the commands.")
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				mu.Lock()
				var snap ipc.Snapshot
				snap, listed = numbered(line, last, listed)
				mu.Unlock()
				reply, quit := command(snap, line, time.Now(), call)
				if reply != "" {
					fmt.Fprintln(out, reply)
				}
				if quit {
					os.Exit(0)
				}
			}
		}()
	}
	for {
		c, err := ipc.Dial()
		if err == nil {
			_ = json.NewEncoder(c).Encode(ipc.Request{Op: "sub"})
			r := bufio.NewReaderSize(c, 1<<20)
			for {
				data, err := r.ReadBytes('\n')
				if err != nil {
					break
				}
				var s ipc.Snapshot
				if json.Unmarshal(data, &s) != nil {
					continue
				}
				s = cleanSnap(s)
				mu.Lock()
				var lines []string
				if have {
					lines = changes(last, s, time.Now())
				} else {
					lines, listed = summary(s, time.Now()), s
				}
				last, have = s, true
				mu.Unlock()
				for _, l := range lines {
					fmt.Fprintln(out, l)
				}
			}
			c.Close()
		}
		fmt.Fprintln(out, "Lost the daemon; reconnecting.")
		time.Sleep(time.Second)
		_ = ipc.EnsureDaemon()
	}
}

func call(req ipc.Request) (ipc.Reply, error) {
	var r ipc.Reply
	err := ipc.Call(req, &r, 10*time.Second)
	return r, err
}

// summary is the team in sentences: who you are, what needs you, and every agent.
func summary(s ipc.Snapshot, now time.Time) []string {
	b := buildBoard(s, now, scopeAll)
	out := []string{fmt.Sprintf("murmur, as %s. %s. %d people online, %d %s.", s.Me.Handle, stateWord(s.State), b.people, b.agents, plural(b.agents, "agent", "agents"))}
	if s.Update != "" { // murmur update says how when Nix installed it
		out = append(out, "Murmur "+s.Update+" is available: run murmur update.")
	}
	if len(b.needs) == 0 {
		out = append(out, "Nothing needs you.")
	} else {
		out = append(out, fmt.Sprintf("%d %s you:", len(b.needs), plural(len(b.needs), "thing needs", "things need")))
		for i, n := range b.needs { // a held answer in full: y N shares only what was said
			out = append(out, fmt.Sprintf("  %d. %s, waiting %s.", i+1, needSentence(n), spoken(now.Sub(n.since))))
			out = append(out, asideDetail(n)...)
		}
	}
	for _, r := range b.all() {
		out = append(out, b.agentSentence(r, now))
	}
	for _, a := range s.Asides { // your questions by the way, and your agents', still on their way
		if a.Mine && !a.Final() {
			out = append(out, fmt.Sprintf("%s question by the way to %s (%s): %s", capital(whose(askedBy(a, s.Me.Handle))), a.To,
				asideWhere(a), oneLine(a.Question)))
		}
	}
	for _, d := range b.dissonant {
		out = append(out, fmt.Sprintf("Out of tune: %s and %s both change %s and never talked.", d.mine.addr, d.other, d.file))
	}
	return out
}

// changes says what changed between two snapshots: the connection, what needs you, messages, agents.
func changes(old, now ipc.Snapshot, at time.Time) []string {
	var out []string
	if old.State != now.State {
		out = append(out, stateWord(now.State)+".")
	}
	if now.Update != "" && now.Update != old.Update {
		out = append(out, "Murmur "+now.Update+" is available: run murmur update.")
	}
	ob, nb := buildBoard(old, at, scopeAll), buildBoard(now, at, scopeAll)
	before := map[string]need{}
	for _, n := range ob.needs {
		before[n.key] = n
	}
	after := map[string]bool{}
	for _, n := range nb.needs {
		after[n.key] = true
		if _, ok := before[n.key]; !ok && n.kind != needAsideHeld && n.kind != needAsideCold { // asides say it below
			out = append(out, "Needs you: "+needSentence(n)+".")
		}
	}
	for _, n := range ob.needs {
		if !after[n.key] && n.kind == needApprove {
			out = append(out, n.who+" no longer needs you.")
		}
	}
	seen := map[string]bool{}
	for _, x := range old.Msgs {
		seen[x.ID] = true
	}
	for _, x := range now.Msgs { // an answer by the way delivered as a message is said with its question
		if !seen[x.ID] && x.From != now.Me.Handle && !nb.delivered[x.ID] {
			out = append(out, fmt.Sprintf("Message from %s to %s: %s", x.From, x.To, firstLine(x.Text)))
		}
	}
	out = append(out, asideChanges(old, now, nb)...)
	oldAgents := map[string]*agentRef{}
	for _, r := range ob.all() {
		oldAgents[r.addr] = r
	}
	newAgents := map[string]bool{}
	for _, r := range nb.all() {
		newAgents[r.addr] = true
		o := oldAgents[r.addr]
		switch {
		case o == nil:
			out = append(out, "New: "+nb.agentSentence(r, at))
		case o.st != r.st && r.mine:
			out = append(out, nb.agentSentence(r, at))
		}
	}
	for addr := range oldAgents {
		if !newAgents[addr] {
			out = append(out, addr+" ended.")
		}
	}
	return out
}

// agentSentence says an agent in a sentence: what it does, where, on what, and for your agents on this laptop, what it
// last said in its own words, or why nobody can ask it by the way.
func (b *board) agentSentence(r *agentRef, now time.Time) string {
	where := r.a.Repo
	if r.a.Branch != "" {
		where += " on " + r.a.Branch
	}
	s := fmt.Sprintf("%s is %s", r.addr, r.st.word())
	if r.st.waiting() || r.st == idle {
		s += " for " + spoken(r.idleFor(now))
	}
	if where != "" {
		s += ", in " + where
	}
	if r.a.Title != "" {
		s += ": " + r.a.Title
	}
	s += "."
	if t := b.said[r.a.Name]; r.mine && t != "" {
		s += " It last said: " + t
	}
	if why := b.cantAskHere(r); why != "" {
		s += " " + sentence(why)
	}
	return s
}

func stateWord(state string) string {
	switch state {
	case "online":
		return "Connected"
	case "":
		return "Connecting"
	}
	return strings.ToUpper(state[:1]) + state[1:]
}

// spoken is a duration as people say it: 3 minutes, 2 hours.
func spoken(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d %s", int(d.Minutes()), plural(int(d.Minutes()), "minute", "minutes"))
	}
	return fmt.Sprintf("%d %s", int(d.Hours()), plural(int(d.Hours()), "hour", "hours"))
}

const plainHelp = `Commands:
  needs                  what needs you, numbered
  who                    every agent
  y N, x N               let held message N through, or drop it; for a question by the way, share its answer
                         or drop it, or let a cold one run or decline it
  reply N TEXT           answer need N
  done N                 mark question N done
  send TO TEXT           message an agent (bob/main-1), a person (bob) or a repo (repo:acme/api)
  btw AGENT QUESTION     ask an agent (bob/main-1, or one of yours by its name) by the way: a copy of its
                         conversation answers, and the agent never sees the question
  mute WHO [1h|24h]      hold messages from WHO for a while, or until you unmute
  quit`

// numbered is the snapshot a command counts needs in, and the one the needs are numbered from after it: a command
// that names a need by number counts in the list as it was last printed, so a need coming or going since doesn't
// shift the number onto another.
func numbered(line string, last, listed ipc.Snapshot) (snap, nowListed ipc.Snapshot) {
	switch verb, _, _ := strings.Cut(line, " "); verb {
	case "needs":
		return last, last
	case "y", "x", "done", "reply":
		return listed, listed
	}
	return last, listed
}

// command runs one line typed in the plain mode and says what happened.
func command(s ipc.Snapshot, line string, now time.Time, call func(ipc.Request) (ipc.Reply, error)) (reply string, quit bool) {
	b := buildBoard(s, now, scopeAll)
	verb, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	nth := func(arg string) (need, bool) {
		i, err := strconv.Atoi(arg)
		if err != nil || i < 1 || i > len(b.needs) {
			return need{}, false
		}
		return b.needs[i-1], true
	}
	do := func(req ipc.Request, ok string) string {
		r, err := call(req)
		switch {
		case err != nil:
			return "Not done: " + err.Error()
		case !r.OK:
			return "Not done: " + r.Err
		}
		return ok
	}
	switch verb {
	case "":
		return "", false
	case "quit", "exit", "q":
		return "", true
	case "help", "?":
		return plainHelp, false
	case "needs":
		if len(b.needs) == 0 {
			return "Nothing needs you.", false
		}
		var lines []string
		for i, n := range b.needs {
			lines = append(lines, fmt.Sprintf("%d. %s, waiting %s.", i+1, needSentence(n), spoken(now.Sub(n.since))))
			lines = append(lines, asideDetail(n)...)
		}
		return strings.Join(lines, "\n"), false
	case "who":
		var lines []string
		for _, r := range b.all() {
			lines = append(lines, b.agentSentence(r, now))
		}
		return strings.Join(lines, "\n"), false
	case "y", "x":
		n, ok := nth(rest)
		switch {
		case ok && (n.kind == needAsideHeld || n.kind == needAsideCold):
			req, ok := settleRequest(n.btw, verb == "y")
			return do(req, ok+"."), false
		case !ok || n.kind != needHeld:
			return "Say the number of a held message, or of a question by the way that waits for you, from needs.", false
		case verb == "y":
			return do(ipc.Request{Op: "approve", ID: n.msg.ID}, "Let through to "+n.to+"."), false
		}
		return do(ipc.Request{Op: "decline", ID: n.msg.ID}, "Dropped; "+n.who+" is told."), false
	case "btw":
		to, text, _ := strings.Cut(rest, " ")
		if text = strings.TrimSpace(text); to == "" || text == "" {
			return "Say which agent to ask (bob/main-1), then the question.", false
		}
		if !strings.Contains(to, "/") { // one of yours, by its name
			to = s.Me.Handle + "/" + to
		}
		r := b.find(to)
		switch {
		case r == nil:
			return to + " isn't running. Type who for the agents.", false
		case !b.askable(r):
			return "Not asked: " + sentence(b.whyNot(r)), false
		case len([]rune(text)) > protocol.MaxBtwQuestion:
			return fmt.Sprintf("Not asked: a question by the way holds %d characters at most.", protocol.MaxBtwQuestion), false
		}
		return do(ipc.Request{Op: "btw", ID: protocol.RandomHex(8), To: to, Text: text}, "Asked "+to+" by the way; the answer comes here."), false
	case "done":
		n, ok := nth(rest)
		if !ok || n.kind == needApprove || n.kind == needHeld || n.kind == needAsideHeld || n.kind == needAsideCold {
			return "Say the number of a question or a failed message, from needs.", false
		}
		return do(ipc.Request{Op: "done", ID: n.msg.ID}, "Marked done."), false
	case "reply":
		arg, text, _ := strings.Cut(rest, " ")
		n, ok := nth(arg)
		if !ok || strings.TrimSpace(text) == "" {
			return "Say the number of a need, then your answer.", false
		}
		return do(ipc.Request{Op: "send", To: n.who, Text: text}, "Sent to "+n.who+"."), false
	case "send":
		to, text, _ := strings.Cut(rest, " ")
		if to == "" || strings.TrimSpace(text) == "" {
			return "Say whom to send to, then the message.", false
		}
		return do(ipc.Request{Op: "send", To: to, Text: text}, "Sent to "+to+"."), false
	case "mute":
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "Say whom to mute.", false
		}
		howLong := ""
		if len(fields) > 1 {
			howLong = fields[1]
		}
		return do(ipc.Request{Op: "mute", To: fields[0], Text: howLong}, "Muted "+fields[0]+"."), false
	}
	return fmt.Sprintf("%q isn't a command. Type help.", verb), false
}

// asideChanges says what changed in the questions asked by the way: yours and your agents', asked, on their way and
// answered; and teammates' questions to your agents, or their agents', with the need numbers y and x take when they
// wait for you.
func asideChanges(old, now ipc.Snapshot, nb *board) []string {
	before := map[string]ipc.Aside{}
	for _, a := range old.Asides {
		before[a.ID] = a
	}
	var out []string
	for _, a := range now.Asides {
		o, seen := before[a.ID]
		if seen && o.State == a.State && o.Note == a.Note {
			continue
		}
		q, who := oneLine(a.Question), askedBy(a, now.Me.Handle) // you, your main-1, carol, carol's agent web-2
		switch {
		case a.Mine && a.Final():
			out = append(out, answerSentence(a)+".")
		case a.Mine && !seen:
			out = append(out, fmt.Sprintf("%s asked %s by the way: %s", capital(who), a.To, q))
			if a.Note != "" || a.State == ipc.AsideRunning {
				out = append(out, fmt.Sprintf("%s question by the way to %s: %s.", capital(whose(who)), a.To, asideWhere(a)))
			}
		case a.Mine:
			out = append(out, fmt.Sprintf("%s question by the way to %s: %s.", capital(whose(who)), a.To, asideWhere(a)))
		case waits(a):
			n := slices.IndexFunc(nb.needs, func(n need) bool { return n.key == "need:btw:"+a.ID }) + 1
			if a.State == ipc.AsideHeld {
				out = append(out, fmt.Sprintf("%s asked your %s by the way; the answer waits for you: y %d shares it, x %d drops it.",
					capital(who), agentName(a.To), n, n))
			} else {
				out = append(out, fmt.Sprintf("%s asked your %s by the way, idle long; its copy would read %s tokens: y %d lets it run, x %d declines it.",
					capital(who), agentName(a.To), tokens(a.Cold), n, n))
			}
			out = append(out, asideDetail(need{kind: needAsideHeld, btw: a})...)
		case a.State == ipc.AsideAnswered:
			out = append(out, fmt.Sprintf("Your %s answered %s by the way: %s", agentName(a.To), who, oneLine(a.Answer)))
		case a.State == ipc.AsideFailed:
			out = append(out, fmt.Sprintf("%s asked your %s by the way and got no answer: %s.", capital(who), agentName(a.To), a.Err))
		case !seen:
			out = append(out, fmt.Sprintf("%s asked your %s by the way: %s", capital(who), agentName(a.To), q))
		}
	}
	return out
}

// asideWhere says where your question by the way stands while it has no answer: the note of the agent's laptop or of
// the hub (it waits on the agent's person, or for a laptop), being answered, or on its way.
func asideWhere(a ipc.Aside) string {
	switch {
	case a.Note != "":
		return a.Note
	case a.State == ipc.AsideRunning:
		return "being answered by a copy of its conversation"
	}
	return "on its way"
}

// asideDetail is the question by the way of a need, and the answer that waits for you, whole, each on a line of its
// own: the plain mode says an answer whole wherever it gives the number y shares it by.
func asideDetail(n need) []string {
	if n.kind != needAsideHeld && n.kind != needAsideCold {
		return nil
	}
	out := []string{"   Question: " + oneLine(n.btw.Question)}
	if n.btw.Answer != "" {
		out = append(out, "   Answer: "+oneLine(n.btw.Answer))
	}
	return out
}

// oneLine is text on one line, for the plain mode: its lines joined, its spaces collapsed.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// sentence ends what s says with a full stop, on one line, unless it ends with one.
func sentence(s string) string { return strings.TrimSuffix(oneLine(s), ".") + "." }

// Status prints the daemon's snapshot as JSON, for scripts.
func Status(out io.Writer) error {
	if err := ipc.EnsureDaemon(); err != nil {
		return err
	}
	var s ipc.Snapshot
	if err := ipc.Call(ipc.Request{Op: "snapshot"}, &s, 5*time.Second); err != nil {
		return err
	}
	slices.SortFunc(s.People, func(a, b protocol.Person) int { return strings.Compare(a.Handle, b.Handle) })
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}
