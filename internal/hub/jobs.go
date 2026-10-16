package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

const jobEvery = 30 * time.Second

// jobLoop runs the team-wide chores on one instance at a time: whichever holds the lease.
func (h *Hub) jobLoop(ctx context.Context) {
	tick := time.NewTicker(jobEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			ctx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			h.flushStats(ctx) // what this instance counted since the last tick
			done()
			return
		case <-tick.C:
		}
		now := time.Now()
		h.flushStats(ctx) // every instance counts for the weekly report
		mine, err := h.store.Lease(ctx, "jobs", h.id, 3*jobEvery)
		if err != nil {
			h.log.Error("database", "op", "take the job lease", "error", err.Error())
			continue
		}
		if mine {
			h.expire(ctx, now)
			h.checkRelease(ctx)
			h.followTeam(ctx, now)
			h.weeklyReport(ctx, now)
		}
	}
}

// expire gives up on the messages nobody came back for, and tells each sender.
func (h *Hub) expire(ctx context.Context, now time.Time) {
	gone, err := h.store.TakeExpired(ctx, now)
	if err != nil { // what was taken is gone from the store all the same: its senders still hear
		h.log.Error("database", "op", "expire kept messages", "error", err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range gone {
		why := p.Msg.To + " went offline before the message reached it"
		if p.Device == "" {
			why = p.Msg.To + " didn't come back to murmur within a week"
		}
		h.log.Info("message expired", "id", p.Msg.ID, "to", p.Msg.To)
		if p.Sender != "" {
			h.toDeviceLocked(p.Sender, protocol.Frame{T: protocol.FrameAck, ID: p.Msg.ID, Err: why}, "")
		}
	}
}

// Latest is the newest release the hub found, kept for the admin panel.
type Latest struct {
	Version string    `json:"version"`
	URL     string    `json:"url"`
	Checked time.Time `json:"checked"`
}

const latestKey = "release/latest"

// releasesURL is where checkRelease asks; tests point it elsewhere.
var releasesURL = "https://api.github.com/repos/" + release.Repo + "/releases/latest"

// checkRelease asks GitHub once a day for the project's newest release, when the team's admins want to know. It is
// a plain read of public data that sends nothing about the team. (The hub also calls out to the team's own chat and
// GitHub, when its admins set them up.)
func (h *Hub) checkRelease(ctx context.Context) {
	if !h.Settings().ReleaseCheck {
		return
	}
	var last Latest
	if b, err := h.store.Get(ctx, latestKey); err == nil {
		_ = json.Unmarshal(b, &last)
	}
	if time.Since(last.Checked) < 24*time.Hour {
		return
	}
	last.Checked = time.Now() // a failed check waits a day too: the panel says when it last worked
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req); err != nil {
		h.log.Info("release check failed", "error", err.Error())
	} else {
		var r struct {
			Tag string `json:"tag_name"`
			URL string `json:"html_url"`
		}
		if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r) == nil && r.Tag != "" {
			last.Version, last.URL = strings.TrimPrefix(r.Tag, "v"), r.URL
		}
		resp.Body.Close()
	}
	if err := h.store.Put(ctx, latestKey, mustJSON(last)); err != nil {
		h.log.Error("database", "op", "save the release check", "error", err.Error())
	}
}
