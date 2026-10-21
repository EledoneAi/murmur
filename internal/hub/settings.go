package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/store"
)

// Settings are what a team tunes while its hub runs. They live in the store; environment variables only seed them
// the first time a hub starts, and admins change them in the admin panel without a restart.
type Settings struct {
	// Who may join: verified emails in these domains, these exact emails, members of these GitHub organisations.
	// With none set, only the admins named at startup can sign in.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	AllowedEmails  []string `json:"allowed_emails,omitempty"`
	AllowedOrgs    []string `json:"allowed_orgs,omitempty"`

	TokenDays int `json:"token_days"` // a laptop signs in again this long after its sign-in; 0 for never

	SendsPerMinute   int `json:"sends_per_minute"`   // messages one sender (a person or an agent) can send per minute
	LoopMax          int `json:"loop_max"`           // messages two agents exchange in LoopMinutes before a person must step in
	LoopMinutes      int `json:"loop_minutes"`       //
	MaxAgents        int `json:"max_agents"`         // sessions one laptop can report
	MaxLaptops       int `json:"max_laptops"`        // laptops one person can have signed in
	DownloadsPerHour int `json:"downloads_per_hour"` // binary downloads one address can start per hour

	// IgnoredPaths keep files out of overlaps for the whole team, on top of each repo's .gitattributes:
	// "traces/**" covers a folder, "*.lock" any file with that name, "docs/*.md" files right in docs.
	IgnoredPaths []string `json:"ignored_paths,omitempty"`

	UpdatePolicy string `json:"update_policy"`         // auto: laptops update when idle; notify: their people decide; off
	MinVersion   string `json:"min_version,omitempty"` // laptops older than this are refused until they update
	CheckHours   int    `json:"check_hours"`           // how often laptops check their own setup and report; 0 for never
	ReleaseCheck bool   `json:"release_check"`         // look for new murmur releases once a day, for the admins

	// Agreements is how the team asks its agents to work together, in plain text: every agent receives it at the
	// start of each session.
	Agreements string `json:"agreements,omitempty"`

	// Chat is where the hub posts for the team: pings for agents left waiting, the weekly report. ChatKind says the
	// format: slack, google-chat, discord, teams, or generic (a JSON body {"text": …}).
	ChatWebhook string `json:"chat_webhook,omitempty"`
	ChatKind    string `json:"chat_kind,omitempty"`
	// WaitPingMinutes is how long an agent waits for its person before the team's chat hears of it; 0 for never.
	WaitPingMinutes int `json:"wait_ping_minutes"`
	// Working hours, in TimeZone, when the hub pings people: Monday to Friday only when Weekdays is on.
	WorkStart int    `json:"work_start"`
	WorkEnd   int    `json:"work_end"`
	Weekdays  bool   `json:"weekdays"`
	TimeZone  string `json:"time_zone"`
	// WeeklyReport posts the team's week to the chat on Monday at WorkStart.
	WeeklyReport bool `json:"weekly_report"`

	AnnounceHours int `json:"announce_hours"` // how long an announcement to a repo stays up

	// Btw lets people and their agents ask an agent a question by the way, which a throwaway copy of its conversation
	// answers on its person's laptop, without the agent seeing it. BtwPerHour is how many one person and their agents
	// can ask in an hour, each also counted in SendsPerMinute, and in LoopMax when an agent asks an agent.
	Btw        bool `json:"btw"`
	BtwPerHour int  `json:"btw_per_hour"`

	// GitHub tells agents when a pull request merges over their unpushed work: GitHub sends pull request events
	// signed with WebhookSecret to /github/webhook, and the hub reads changed files with Token (read-only).
	GitHubWebhookSecret string `json:"github_webhook_secret,omitempty"`
	GitHubToken         string `json:"github_token,omitempty"`

	Updated   time.Time `json:"updated,omitzero"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Defaults are the settings of a new hub, before its environment and its admins change them.
func Defaults() Settings {
	return Settings{TokenDays: 90, SendsPerMinute: 20, LoopMax: 12, LoopMinutes: 10, MaxAgents: 64,
		MaxLaptops: 10, DownloadsPerHour: 120, UpdatePolicy: "auto", CheckHours: 24, ReleaseCheck: true, WaitPingMinutes: 15,
		WorkStart: 9, WorkEnd: 19, Weekdays: true, TimeZone: "UTC", WeeklyReport: true, AnnounceHours: 24, Btw: true, BtwPerHour: 30}
}

const settingsKey = "settings"

// settingError says what is wrong with one setting, which it names as the store and the admin panel do.
type settingError struct{ field, text string }

func (e settingError) Error() string { return e.text }

// Valid says what is wrong with s, if anything, and names the setting at fault.
func (s Settings) Valid() error {
	for _, c := range []struct {
		field, name string
		v, lo, hi   int
	}{
		{"token_days", "days a laptop stays signed in", s.TokenDays, 0, 3650},
		{"sends_per_minute", "messages per minute", s.SendsPerMinute, 1, 1000},
		{"btw_per_hour", "questions by the way per hour", s.BtwPerHour, 1, 1000},
		{"loop_max", "messages between two agents", s.LoopMax, 1, 1000},
		{"loop_minutes", "loop window", s.LoopMinutes, 1, 1440},
		{"max_agents", "agents per laptop", s.MaxAgents, 1, 1000},
		{"max_laptops", "laptops per person", s.MaxLaptops, 1, 1000},
		{"downloads_per_hour", "downloads per hour", s.DownloadsPerHour, 1, 100000},
		{"check_hours", "hours between checks", s.CheckHours, 0, 24 * 30},
		{"wait_ping_minutes", "minutes before a wait is pinged", s.WaitPingMinutes, 0, 24 * 60},
		{"work_start", "start of the working day", s.WorkStart, 0, 23},
		{"work_end", "end of the working day", s.WorkEnd, 1, 24},
		{"announce_hours", "hours an announcement stays up", s.AnnounceHours, 1, 24 * 30},
	} {
		if c.v < c.lo || c.v > c.hi {
			return settingError{c.field, fmt.Sprintf("%s must be between %d and %d", c.name, c.lo, c.hi)}
		}
	}
	if s.WorkEnd <= s.WorkStart {
		return settingError{"work_end", "the working day must end after it starts"}
	}
	if _, err := time.LoadLocation(s.TimeZone); err != nil {
		return settingError{"time_zone", fmt.Sprintf("time zone %q: use a name like Europe/Paris or UTC", s.TimeZone)}
	}
	if s.ChatWebhook != "" && !strings.HasPrefix(s.ChatWebhook, "https://") {
		return settingError{"chat_webhook", "the chat webhook must be an https:// address"}
	}
	if !slices.Contains([]string{"", "slack", "google-chat", "discord", "teams", "generic"}, s.ChatKind) {
		return settingError{"chat_kind", fmt.Sprintf("chat kind %q: use slack, google-chat, discord, teams or generic", s.ChatKind)}
	}
	if len(s.Agreements) > 4000 {
		return settingError{"agreements", "the agreements are longer than 4000 characters: keep them to one page"}
	}
	if !slices.Contains([]string{"auto", "notify", "off"}, s.UpdatePolicy) {
		return settingError{"update_policy", fmt.Sprintf("update policy %q: use auto, notify or off", s.UpdatePolicy)}
	}
	if s.MinVersion != "" && !semver.MatchString(s.MinVersion) {
		return settingError{"min_version", fmt.Sprintf("oldest accepted version %q: write it like 1.4.0", s.MinVersion)}
	}
	for _, p := range s.IgnoredPaths {
		if _, err := path.Match(strings.TrimSuffix(p, "/**"), ""); err != nil {
			return settingError{"ignored_paths", fmt.Sprintf("ignored path %q: %v", p, err)}
		}
	}
	for _, d := range s.AllowedDomains {
		if strings.Contains(d, "@") {
			return settingError{"allowed_domains", fmt.Sprintf("allowed domain %q: write only what comes after the @, like example.com", d)}
		}
	}
	for _, e := range s.AllowedEmails {
		if !strings.Contains(e, "@") {
			return settingError{"allowed_emails", fmt.Sprintf("allowed email %q: write the whole address", e)}
		}
	}
	return nil
}

// loadSettings reads the team's settings, or stores the seed the first time.
func (h *Hub) loadSettings(ctx context.Context) error {
	s := Defaults()
	b, err := h.store.Get(ctx, settingsKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s = h.cfg.Seed
		if err := s.Valid(); err != nil {
			return fmt.Errorf("settings from the environment: %w", err)
		}
		if err := h.store.Put(ctx, settingsKey, mustJSON(s)); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("settings in the store: %w", err)
		}
	}
	h.mu.Lock()
	h.settings = s
	h.teamChangedLocked()
	h.mu.Unlock()
	return nil
}

var semver = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// Ignored says whether the team keeps file f out of overlaps.
func (s Settings) Ignored(f string) bool {
	for _, p := range s.IgnoredPaths {
		if dir, ok := strings.CutSuffix(p, "/**"); ok {
			if strings.HasPrefix(f, dir+"/") {
				return true
			}
			continue
		}
		pat, target := p, f
		if !strings.Contains(p, "/") { // a bare name matches in any folder
			target = path.Base(f)
		}
		if ok, _ := path.Match(pat, target); ok {
			return true
		}
	}
	return false
}

// SaveSettings stores new settings, uses them on this instance at once and tells the others: an instance doesn't
// act on its own events.
func (h *Hub) SaveSettings(ctx context.Context, s Settings, by string) error {
	if err := s.Valid(); err != nil {
		return err
	}
	s.AllowedDomains, s.AllowedEmails, s.AllowedOrgs = lowerList(s.AllowedDomains), lowerList(s.AllowedEmails), lowerList(s.AllowedOrgs)
	s.MinVersion = strings.TrimPrefix(s.MinVersion, "v")
	s.Updated, s.UpdatedBy = time.Now(), by
	if err := h.store.Put(ctx, settingsKey, mustJSON(s)); err != nil {
		return err
	}
	h.mu.Lock()
	old := h.settings
	h.settings = s
	h.teamChangedLocked()
	h.mu.Unlock()
	h.event(ctx, "settings changed", by, "", changedFields(old, s))
	h.changed(ctx, "settings")
	return nil
}

// changedFields names the settings that differ between old and s, as the store names them, for the event log.
func changedFields(old, s Settings) string {
	old.Updated, old.UpdatedBy, s.Updated, s.UpdatedBy = time.Time{}, "", time.Time{}, ""
	var a, b map[string]any
	_ = json.Unmarshal(mustJSON(old), &a)
	_ = json.Unmarshal(mustJSON(s), &b)
	var out []string
	for k := range b {
		if !reflect.DeepEqual(a[k], b[k]) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok { // a list emptied, which the JSON leaves out
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

func (h *Hub) Settings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

// lowerList trims, lowercases, sorts and dedups a list an admin typed.
func lowerList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, strings.TrimPrefix(s, "@"))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SplitList reads a comma- or space-separated list, as environment variables and form fields hold them.
func SplitList(s string) []string {
	return lowerList(strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }))
}
