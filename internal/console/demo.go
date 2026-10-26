package console

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// RunDemo opens the console on a made-up team that keeps working: no hub, no daemon, no Claude Code needed. It is
// how murmur demo shows the console, how the README's recording is made, and where console changes are tried first.
func RunDemo() error {
	if err := terminal(); err != nil {
		return err
	}
	oneProcessor()
	m := newModel()
	m.demo = true
	go func() {
		start := time.Now()
		for i := 0; ; i++ {
			m.sub <- snapMsg(demoAt(start, i))
			time.Sleep(2 * time.Second)
		}
	}()
	_, err := tea.NewProgram(m).Run()
	return err
}

// demoAt is the demo team at step i, two seconds apart: agents keep calling tools, the fuses burn, a message comes
// and goes, and now and then an agent stops to ask for a permission.
func demoAt(start time.Time, i int) ipc.Snapshot {
	now := start.Add(time.Duration(i) * 2 * time.Second)
	s := demoSnapshot(now)
	for p := range s.People {
		for a := range s.People[p].Agents {
			ag := &s.People[p].Agents[a]
			if ag.Status == protocol.StatusBusy && ag.Kinds != "" {
				k := []rune(ag.Kinds)
				shift := (i + a + p) % len(k)
				ag.Kinds = string(append(k[shift:], k[:shift]...))
			}
		}
	}
	if i%20 >= 12 { // bob's reviewer asks for a permission now and then
		b := &s.People[1].Agents[0]
		b.Status, b.Needs, b.Since = protocol.StatusWaiting, "permission", start.Add(time.Duration(i-i%20+12)*2*time.Second).UnixMilli()
	}
	if i%15 >= 5 {
		s.Msgs = append(s.Msgs, protocol.Msg{ID: fmt.Sprintf("live-%d", i/15), From: "carol/billing-2", To: "alice/api-9b",
			Text: "I split shadow and live in certifier.go. Who calls Rerun()?", At: now.Add(-time.Duration(i%15-5) * 2 * time.Second).UnixMilli()})
	}
	return s
}

// demoNow is when the golden tests look at the demo team.
var demoNow = time.Date(2026, 10, 16, 14, 32, 0, 0, time.Local)

// demoSnapshot is a made-up team at a given time: what murmur demo shows and the golden tests check. Its people have
// the colours a hub would give them, in the order they joined: alice 0, bob 1, carol 2, dave 3, erin 4.
func demoSnapshot(now time.Time) ipc.Snapshot {
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	msg := func(id, from, to, text string, d time.Duration) protocol.Msg {
		return protocol.Msg{ID: id, From: from, To: to, Text: text, At: ago(d)}
	}
	return ipc.Snapshot{
		Me: protocol.Person{Handle: "alice"}, State: "online", Version: "demo",
		People: []protocol.Person{
			{Handle: "alice", Online: true, Agents: []protocol.Agent{
				{Name: "api-9b", Repo: "acme/api", Branch: "feat/billing-v2", Tree: "t-9b", Title: "Rerun billing on staging", PR: 1822,
					PRURL: "https://github.com/acme/api/pull/1822", Status: protocol.StatusBusy, Since: ago(12 * time.Minute),
					Doing: "Running go test ./billing/ -run Rerun", Kinds: "rrrxeerxxxexrrmexxxerxxxmxrexx", Added: 214, Deleted: 38, Changed: 6, Context: 78,
					Btw: protocol.BtwApprove},
				{Name: "api-1a", Repo: "acme/api", Branch: "feat/billing-ocr", Base: "feat/billing-v2", Tree: "t-1a",
					Title: "Read invoices with OCR", Status: protocol.StatusWaiting, Needs: "permission", Since: ago(3*time.Minute + 12*time.Second),
					Doing: "Running go test ./ocr/...", Kinds: "rreeeererxx", Added: 51, Deleted: 4, Changed: 3, Context: 31, Btw: protocol.BtwApprove},
				{Name: "web-4c", Repo: "acme/web", Branch: "feat/search-ui", Tree: "t-4c", Title: "Search filters and empty states",
					Status: protocol.StatusBusy, Since: ago(25 * time.Minute), Doing: "Editing src/search/Filters.tsx",
					Kinds: "eerreeeereeerreeeereeeerreeere", Added: 96, Deleted: 12, Changed: 4, Context: 44, PR: 1840,
					PRURL: "https://github.com/acme/web/pull/1840", Btw: protocol.BtwApprove},
				{Name: "infra-3", Repo: "acme/infra", Branch: "chore/tf-providers", Tree: "t-i3", Title: "Upgrade the Terraform providers",
					Status: protocol.StatusBusy, Since: ago(8 * time.Minute), Doing: "Running terraform plan", Kinds: "rrxrrexrrexxrrx", Context: 22,
					Btw: protocol.BtwApprove},
				{Name: "notes", Repo: "notes", Title: "Draft the weekly plan", Status: protocol.StatusBusy, Since: ago(5 * time.Minute),
					Doing: "Fetching the release calendar", Kinds: "rrwwrrrerrewrrre"},
				{Name: "docs-2", Repo: "acme/docs", Branch: "main", Tree: "t-d2", Title: "Proofread the API guide", Status: protocol.StatusIdle,
					Since: ago(3 * time.Hour), Btw: protocol.BtwApprove},
				{Name: "api-2f", Repo: "acme/api", Branch: "feat/billing-v2", Tree: "t-2f", Title: "Billing fixtures for the rerun",
					Status: protocol.StatusIdle, Since: ago(40 * time.Minute), Btw: protocol.BtwApprove},
			}},
			{Handle: "bob", Online: true, Colour: 1, Agents: []protocol.Agent{
				{Name: "main-1", Repo: "acme/api", Branch: "main", Tree: "t-b1", Title: "Review the billing config", Status: protocol.StatusBusy,
					Since: ago(4 * time.Minute), Doing: "Reading billing/config.go", Kinds: "rrwrrrrerrrwrrrrrr", Context: 12, Btw: protocol.BtwApprove},
			}},
			{Handle: "carol", Online: true, Colour: 2, Agents: []protocol.Agent{
				{Name: "billing-2", Repo: "acme/api", Branch: "feat/billing-certifier", Tree: "t-c2", Title: "Split the certifier",
					Status: protocol.StatusWaiting, Needs: "input", Since: ago(8*time.Minute + 40*time.Second), Kinds: "eeereerrem",
					Btw: protocol.BtwAuto},
				{Name: "web-2", Repo: "acme/web", Branch: "feat/search-api", Tree: "t-w2", Title: "Search endpoint pagination",
					Status: protocol.StatusBusy, Since: ago(15 * time.Minute), Kinds: "rreeexxeeerreexxe", Doing: "Editing api/search.ts"},
			}},
			{Handle: "dave", Online: true, Colour: 3, Agents: []protocol.Agent{
				{Name: "infra-1", Repo: "acme/infra", Branch: "pilot/terraform-6", Tree: "t-d1", Title: "Pilot Terraform 6",
					Status: protocol.StatusIdle, Since: ago(time.Hour), Btw: protocol.BtwMine},
				{Name: "ingest-1", Repo: "acme/data", Branch: "queue-v2", Tree: "t-g1", Title: "Move the rerun queue", Status: protocol.StatusBusy,
					Since: ago(9 * time.Minute), Doing: "Running make migrate", Kinds: "xxxxrxxxxxxx", Loop: true, Btw: protocol.BtwMine},
			}},
			{Handle: "erin", Colour: 4},
		},
		Pairs: []protocol.Overlap{
			{Mine: "api-9b", Other: "carol/billing-2", Files: []string{"billing/certifier.go"}},
			{Mine: "infra-3", Other: "dave/infra-1", Files: []string{"main.tf"}},
		},
		Msgs: []protocol.Msg{
			msg("m1", "alice/api-9b", "alice/api-1a", "I'm moving the OCR call behind the certifier flag. Rebase after my next push.", 8*time.Minute),
			msg("m2", "carol/billing-2", "alice/api-9b", "I split shadow and live in certifier.go. Who calls Rerun()?", 3*time.Minute),
			msg("m3", "alice/api-9b", "carol/billing-2", "billing/rerun.go:88. Keep an alias until #1831 merges?", 2*time.Minute),
			msg("m4", "alice/web-4c", "carol/web-2", "The filters need a total count. Can the endpoint return it?", 20*time.Minute),
			msg("m5", "carol/web-2", "alice/web-4c", "Yes: total comes back with every page from now on.", 14*time.Minute),
			msg("m6", "dave", "alice", "ok to merge #1827 before v2?", time.Minute+5*time.Second),
		},
		Asks: []protocol.Msg{
			msg("h1", "bob/main-1", "alice/api-9b", "Staging uses the shadow certifier too; keep the alias until Friday.", 40*time.Second),
		},
		Mutes: []string{"person bob until Fri 18:00"},
		// carol's web-2 asked alice's web-4c something by the way, with murmur's btw tool, and alice shared the answer;
		// alice asked bob's reviewer, and has its answer; carol asked alice's api-9b, whose answer waits for alice to
		// share it. alice's notes runs in Codex, which can't copy a conversation, and so does carol's web-2, which can
		// still ask: nobody can ask them, and alice's laptop says why of hers. dave turned teammates' questions off: only
		// he and his agents can ask his.
		Asides: []ipc.Aside{
			{Btw: protocol.Btw{ID: "w7", From: "carol/web-2", To: "alice/web-4c", Question: "Which query params do the filters send? I'll validate them on the endpoint.",
				Answer: "status, owner, from and to, all optional; from and to are ISO dates. Sorting stays newest first for now.",
				Tokens: 36_900, At: ago(10 * time.Minute)}, State: ipc.AsideAnswered},
			{Btw: protocol.Btw{ID: "w1", From: "alice", To: "bob/main-1", Question: "What's left in the billing config review?",
				Answer: "Two things: the shadow flag still defaults to on, and nothing reads the retry limit. I'm checking certifier.go next.",
				Tokens: 41_200, At: ago(6 * time.Minute)}, Mine: true, State: ipc.AsideAnswered},
			{Btw: protocol.Btw{ID: "w2", From: "carol", To: "alice/api-9b", Question: "Does rerun.go still call the old certifier?",
				Answer: "Yes, once: billing/rerun.go:88 calls Rerun() through the alias. It stays until #1831 merges.",
				Tokens: 52_800, At: ago(90 * time.Second)}, State: ipc.AsideHeld},
		},
		Said: map[string]string{ // what alice's agents last said, on this laptop
			"api-9b":  "The rerun passes locally; staging next, once carol's split lands.",
			"api-1a":  "The OCR fixtures changed, so I'll run the OCR tests before the rebase.",
			"web-4c":  "Filters work; the empty state waits for the total count from carol's endpoint.",
			"infra-3": "Planning with the new providers; no changes to apply yet.",
			"docs-2":  "The API guide reads well; two examples still use the v1 endpoints.",
			"api-2f":  "Fixtures for the rerun are in billing/testdata/rerun.",
		},
		CantAsk:    map[string]string{"notes": "notes runs in Codex, which can't be asked by the way"},
		BtwAnswers: ipc.BtwApprove,
	}
}
