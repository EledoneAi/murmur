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
	var last ipc.Snapshot
	have := false
	if in != nil {
		go func() {
			sc := bufio.NewScanner(in)
			fmt.Fprintln(out, "Type help for the commands.")
			for sc.Scan() {
				mu.Lock()
				snap := last
				mu.Unlock()
				reply, quit := command(snap, strings.TrimSpace(sc.Text()), time.Now(), call)
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
				mu.Lock()
				var lines []string
				if have {
					lines = changes(last, s, time.Now())
				} else {
					lines = summary(s, time.Now())
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
	out := []string{fmt.Sprintf("murmur, as %s. %s. %d people online, %d agents.", s.Me.Handle, stateWord(s.State), b.people, b.agents)}
	if len(b.needs) == 0 {
		out = append(out, "Nothing needs you.")
	} else {
		out = append(out, fmt.Sprintf("%d %s you:", len(b.needs), plural(len(b.needs), "thing needs", "things need")))
		for i, n := range b.needs {
			out = append(out, fmt.Sprintf("  %d. %s, waiting %s.", i+1, needSentence(n), spoken(now.Sub(n.since))))
		}
	}
	for _, r := range b.all() {
		out = append(out, agentSentence(r, now))
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
	ob, nb := buildBoard(old, at, scopeAll), buildBoard(now, at, scopeAll)
	before := map[string]need{}
	for _, n := range ob.needs {
		before[n.key] = n
	}
	after := map[string]bool{}
	for _, n := range nb.needs {
		after[n.key] = true
		if _, ok := before[n.key]; !ok {
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
	for _, x := range now.Msgs {
		if !seen[x.ID] && x.From != now.Me.Handle {
			out = append(out, fmt.Sprintf("Message from %s to %s: %s", x.From, x.To, firstLine(x.Text)))
		}
	}
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
			out = append(out, "New: "+agentSentence(r, at))
		case o.st != r.st && r.mine:
			out = append(out, agentSentence(r, at))
		}
	}
	for addr := range oldAgents {
		if !newAgents[addr] {
			out = append(out, addr+" ended.")
		}
	}
	return out
}

func agentSentence(r *agentRef, now time.Time) string {
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
	return s + "."
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
  y N, x N               let held message N through, or drop it
  reply N TEXT           answer need N
  done N                 mark question N done
  send TO TEXT           message an agent (bob/main-1), a person (bob) or a repo (repo:acme/api)
  mute WHO [1h|24h]      hold messages from WHO for a while, or until you unmute
  quit`

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
		}
		return strings.Join(lines, "\n"), false
	case "who":
		var lines []string
		for _, r := range b.all() {
			lines = append(lines, agentSentence(r, now))
		}
		return strings.Join(lines, "\n"), false
	case "y", "x":
		n, ok := nth(rest)
		if !ok || n.kind != needHeld {
			return "Say the number of a held message, from needs.", false
		}
		if verb == "y" {
			return do(ipc.Request{Op: "approve", ID: n.msg.ID}, "Let through to "+n.to+"."), false
		}
		return do(ipc.Request{Op: "decline", ID: n.msg.ID}, "Dropped; "+n.who+" is told."), false
	case "done":
		n, ok := nth(rest)
		if !ok || n.kind == needApprove || n.kind == needHeld {
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
