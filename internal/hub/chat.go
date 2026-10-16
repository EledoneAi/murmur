package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// chatClient posts to the team's chat. A chat that doesn't answer quickly misses the post: the hub never retries,
// so a slow chat can't get the same ping twice or hold up the jobs.
var chatClient = &http.Client{Timeout: 5 * time.Second}

// post sends text to the team's chat, in the format its kind expects.
func (h *Hub) post(ctx context.Context, text string) error {
	s := h.Settings()
	if s.ChatWebhook == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.ChatWebhook, bytes.NewReader(chatBody(s.ChatKind, text)))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		var resp *http.Response
		if resp, err = chatClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				err = fmt.Errorf("the chat answered %s", resp.Status)
			}
		}
	}
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) { // its message holds the webhook's address, which is a secret
			err = uerr.Err
		}
		h.log.Error("chat", "op", "post to the team's chat", "kind", s.ChatKind, "error", err.Error())
	}
	return err
}

var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// plain keeps text from laptops (names, repos, branches, file paths) from turning into mentions or links where a chat
// reads markup: <users/all> in Google Chat, [text](url) in Teams' cards.
var plain = strings.NewReplacer("<", "‹", ">", "›", "[", "⟦", "]", "⟧")

// chatBody is text as each chat's incoming webhooks take it.
func chatBody(kind, text string) []byte {
	switch kind {
	case "slack": // Slack reads <…> as links and mentions, so the text escapes them
		return mustJSON(map[string]string{"text": slackEscape.Replace(text)})
	case "discord": // Discord takes 2000 characters, and would ping anyone the text names with @
		return mustJSON(map[string]any{"content": string([]rune(text)[:min(len([]rune(text)), 2000)]),
			"allowed_mentions": map[string]any{"parse": []string{}}})
	case "teams": // an Adaptive Card, which both Teams Workflows and the older connectors take
		var blocks []map[string]any
		for line := range strings.SplitSeq(plain.Replace(text), "\n") {
			if line != "" {
				blocks = append(blocks, map[string]any{"type": "TextBlock", "text": line, "wrap": true})
			}
		}
		card := map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard",
			"version": "1.2", "body": blocks}
		return mustJSON(map[string]any{"type": "message",
			"attachments": []map[string]any{{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}}})
	}
	return mustJSON(map[string]string{"text": plain.Replace(text)}) // google-chat and generic
}

// working says whether t is in the team's working hours, when the hub may ping people.
func (s Settings) working(t time.Time) bool {
	t = t.In(zone(s.TimeZone))
	if s.Weekdays && (t.Weekday() == time.Saturday || t.Weekday() == time.Sunday) {
		return false
	}
	return t.Hour() >= s.WorkStart && t.Hour() < s.WorkEnd
}

const pingedKey = "chat/pinged" // agent address -> when the hub last pinged the chat about it, in unix ms

// pingWaits tells the team's chat about the agents that have waited WaitPingMinutes for their person, once per wait
// and only in working hours: a wait that started at night is pinged when the day starts, if it still holds.
func (h *Hub) pingWaits(ctx context.Context, now time.Time, s Settings, agents map[string]checkout) {
	if s.ChatWebhook == "" || s.WaitPingMinutes == 0 || !s.working(now) {
		return
	}
	var due []string
	for addr, w := range h.stats.waits {
		if !w.pinged && now.Sub(w.start) >= time.Duration(s.WaitPingMinutes)*time.Minute {
			due = append(due, addr)
		}
	}
	if len(due) == 0 {
		return
	}
	pinged := map[string]int64{}
	if err := getJSON(ctx, h.store, pingedKey, &pinged); err != nil {
		h.log.Error("database", "op", "read which waits were pinged", "error", err.Error())
		return
	}
	var lines []string
	slices.Sort(due)
	for _, addr := range due {
		w := h.stats.waits[addr]
		w.pinged = true
		if pinged[addr] >= w.status.UnixMilli() { // pinged by the instance that held the lease before
			continue
		}
		pinged[addr] = now.UnixMilli()
		lines = append(lines, h.pingLine(agents[addr], now.Sub(w.start)))
	}
	if len(lines) == 0 {
		return
	}
	maps.DeleteFunc(pinged, func(_ string, at int64) bool { return now.Sub(time.UnixMilli(at)) > 7*24*time.Hour })
	if err := h.store.Put(ctx, pingedKey, mustJSON(pinged)); err != nil {
		h.log.Error("database", "op", "remember which waits were pinged", "error", err.Error())
	}
	h.log.Info("waits pinged", "agents", len(lines))
	_ = h.post(ctx, strings.Join(lines, "\n"))
}

// pingLine says who waits, where, and for how long: "alice's agent main-1 (acme/api · login) has waited 15 min for
// them: https://hub.example.com".
func (h *Hub) pingLine(x checkout, d time.Duration) string {
	where := x.agent.Repo
	if x.agent.Branch != "" {
		where = strings.TrimPrefix(where+" · "+x.agent.Branch, " · ")
	}
	line := x.handle + "'s agent " + x.agent.Name
	if where != "" {
		line += " (" + where + ")"
	}
	line += " has waited " + minutes(int(d/time.Minute)) + " for them"
	if h.cfg.PublicURL != "" {
		line += ": " + h.cfg.PublicURL
	}
	return line
}

// minutes says how long m minutes is: "under a minute", "15 min", "2 h", "2 h 10 min".
func minutes(m int) string {
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}
