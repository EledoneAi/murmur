package hub

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// The admin panel shows a team's admins everything the team tunes, and lets them change it from a browser without a
// restart. The hub serves it as JSON under /api/admin, to signed-in admins only. The panel's pages ask before an
// action that signs someone out or takes rights away; the hub keeps its rules whether they asked or not.

// adminRoutes serves the admin panel's API.
func (h *Hub) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin", h.admin(h.adminHome))
	mux.HandleFunc("GET /api/admin/people", h.admin(h.adminPeople))
	mux.HandleFunc("POST /api/admin/people/join", h.admin(h.joinPost))
	mux.HandleFunc("POST /api/admin/people/promote", h.admin(h.promote))
	mux.HandleFunc("POST /api/admin/people/demote", h.admin(h.demote))
	mux.HandleFunc("POST /api/admin/people/remove", h.admin(h.removePost))
	mux.HandleFunc("POST /api/admin/people/restore", h.admin(h.restorePost))
	mux.HandleFunc("GET /api/admin/laptops", h.admin(h.adminLaptops))
	mux.HandleFunc("POST /api/admin/laptops/revoke", h.admin(h.revokeLaptop))
	mux.HandleFunc("GET /api/admin/settings", h.admin(h.adminSettings))
	mux.HandleFunc("POST /api/admin/settings", h.admin(h.settingsPost))
	mux.HandleFunc("GET /api/admin/events", h.admin(h.adminEvents))
}

// admin lets only admins through. Anyone else gets an error the page can show: 401 when they are signed out, so the
// page sends them to sign in, and 403 when they aren't an admin.
func (h *Hub) admin(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store") // nothing the panel shows should outlive the page
		me := h.me(r)
		switch {
		case me == nil:
			refuse(w, http.StatusUnauthorized, "You are signed out. Sign in again to use the admin panel.", "")
		case !me.Admin:
			refuse(w, http.StatusForbidden, "Only the team's admins can use the admin panel.", "")
		default:
			next(w, r, *me)
		}
	}
}

// problem is what the admin API answers when it refuses or fails: a sentence for a person, and the field at fault
// when one is.
type problem struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

func refuse(w http.ResponseWriter, status int, text, field string) {
	writeJSON(w, status, problem{Error: text, Field: field})
}

// failed tells the admin the hub couldn't read or save something, and logs why.
func (h *Hub) failed(w http.ResponseWriter, op string, err error) {
	h.log.Error("database", "op", op, "error", err.Error())
	refuse(w, http.StatusInternalServerError, "The hub couldn't reach its database. Try again: it is safe to repeat.", "")
}

// readBody reads a request's JSON object into v, which holds already what anything the request leaves out keeps.
// When it can't, it answers the request itself and says why: the body isn't JSON or is too large, or it holds a
// field v doesn't have or a value of the wrong kind.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if t, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); t != "application/json" {
		refuse(w, http.StatusUnsupportedMediaType, "Send this as JSON, with the Content-Type application/json.", "")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)) // as large as any post to the hub
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		return true
	}
	var big *http.MaxBytesError
	var wrong *json.UnmarshalTypeError
	unknown, isUnknown := strings.CutPrefix(err.Error(), "json: unknown field ")
	switch {
	case errors.As(err, &big):
		refuse(w, http.StatusRequestEntityTooLarge, "This is too large: the hub reads up to 256 KB.", "")
	case errors.As(err, &wrong) && wrong.Field != "":
		text := map[reflect.Kind]string{reflect.Int: "Write a whole number, like 20.", reflect.Bool: "This must be true or false.",
			reflect.String: "This must be text."}[wrong.Type.Kind()]
		refuse(w, http.StatusBadRequest, cmp.Or(text, "This value has the wrong kind."), wrong.Field)
	case isUnknown:
		field := strings.Trim(unknown, `"`)
		refuse(w, http.StatusBadRequest, "The hub doesn't know "+field+" here.", field)
	default:
		refuse(w, http.StatusBadRequest, "The hub couldn't read this: send one JSON object.", "")
	}
	return false
}

// stamp is a time as the admin API sends it: RFC 3339 in UTC, to the second, or null for a time that never came.
type stamp time.Time

func (t stamp) MarshalJSON() ([]byte, error) {
	if time.Time(t).IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(time.Time(t).UTC().Format(time.RFC3339))
}

// UnmarshalJSON reads a stamp back, as the tests do.
func (t *stamp) UnmarshalJSON(b []byte) error {
	var at time.Time // null leaves it zero
	err := json.Unmarshal(b, &at)
	*t = stamp(at)
	return err
}

// sentence turns a reason that reads after "Not saved:", like the settings' own, into a sentence of its own.
func sentence(reason string) string {
	if reason == "" {
		return reason
	}
	return strings.ToUpper(reason[:1]) + reason[1:] + "."
}

// last sorts the items it is true for after the others.
func last(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// ---------------------------------------------------------------- overview

// overview is the panel's first page.
type overview struct {
	Version    string         `json:"version"`
	Latest     *releaseView   `json:"latest"` // what the release check found last; nil until it ran
	Newer      bool           `json:"newer"`  // Latest is newer than this hub
	Team       int            `json:"team"`   // people on the team, removed people aside
	Removed    int            `json:"removed"`
	Admins     []string       `json:"admins"`
	SignedIn   int            `json:"signed_in"` // laptops neither revoked nor expired
	Online     int            `json:"online"`
	Week       int            `json:"seen_this_week"`
	Versions   []versionCount `json:"versions"`
	Failing    []laptopView   `json:"failing"`  // the laptops whose last check failed
	Reported   bool           `json:"reported"` // whether any laptop sent a check
	CheckHours int            `json:"check_hours"`
}

type releaseView struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Checked stamp  `json:"checked"`
}

// versionCount is how many signed-in laptops run a version, and why it needs a look, if it does.
type versionCount struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
	Behind  string `json:"behind"`
}

func (h *Hub) adminHome(w http.ResponseWriter, r *http.Request, _ store.User) {
	laptops, err := h.laptopRows(r.Context())
	if err != nil {
		h.failed(w, "read laptops", err)
		return
	}
	o := overview{Version: h.cfg.Version, Admins: []string{}, Versions: []versionCount{}, Failing: []laptopView{},
		CheckHours: h.Settings().CheckHours}
	h.mu.Lock()
	for _, u := range h.users {
		if u.Disabled {
			o.Removed++
			continue
		}
		o.Team++
		if u.Admin {
			o.Admins = append(o.Admins, u.Handle)
		}
	}
	h.mu.Unlock()
	slices.Sort(o.Admins)
	for _, l := range laptops {
		if l.Revoked || l.Expired {
			continue
		}
		o.SignedIn++
		if l.Online {
			o.Online++
		}
		if l.Online || time.Since(l.LastSeen) < 7*24*time.Hour {
			o.Week++
		}
		if i := slices.IndexFunc(o.Versions, func(v versionCount) bool { return v.Version == l.Version }); i >= 0 {
			o.Versions[i].Count++
		} else {
			o.Versions = append(o.Versions, versionCount{l.Version, 1, l.Behind})
		}
		if l.Check != nil {
			o.Reported = true
			if !l.Check.OK {
				o.Failing = append(o.Failing, l.view())
			}
		}
	}
	slices.SortFunc(o.Versions, func(a, b versionCount) int { return newestFirst(a.Version, b.Version) })
	if latest, newer := h.latest(r.Context()); latest != nil {
		o.Latest, o.Newer = &releaseView{latest.Version, latest.URL, stamp(latest.Checked)}, newer
	}
	writeJSON(w, http.StatusOK, o)
}

// latest reads what the release check found last, if it ever ran, and whether that release is newer than this hub.
func (h *Hub) latest(ctx context.Context) (*Latest, bool) {
	b, err := h.store.Get(ctx, latestKey)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.log.Error("database", "op", "read the latest release", "error", err.Error())
		}
		return nil, false
	}
	var l Latest
	if json.Unmarshal(b, &l) != nil || !semver.MatchString(l.Version) {
		return nil, false
	}
	return &l, semver.MatchString(h.cfg.Version) && release.Before(h.cfg.Version, l.Version)
}

// newestFirst orders versions from the newest to the oldest. Those that aren't numbers, such as dev builds and
// laptops that never said which they run, come last.
func newestFirst(a, b string) int {
	na, nb := semver.MatchString(a), semver.MatchString(b)
	switch {
	case na != nb:
		return last(!na, !nb)
	case !na:
		return strings.Compare(a, b)
	case release.Before(b, a):
		return -1
	case release.Before(a, b):
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- laptops

// laptopRow is one laptop as the admin panel counts it.
type laptopRow struct {
	store.Device
	Handle  string           // its person's
	Online  bool             //
	Expired bool             // signed in longer ago than the team allows
	Behind  string           // why its version needs a look, if it does
	Check   *protocol.Report // its last self-check, nil until it sends one
	Failing []protocol.Check // the checks that failed in it
}

// laptopView is one laptop as the admin API shows it. Its token never leaves the hub.
type laptopView struct {
	ID       string     `json:"id"`
	Handle   string     `json:"handle"`
	Email    string     `json:"email"`
	Host     string     `json:"host"`
	Version  string     `json:"version"`
	Behind   string     `json:"behind"`
	Created  stamp      `json:"created"`
	LastSeen stamp      `json:"last_seen"`
	Online   bool       `json:"online"`
	Revoked  bool       `json:"revoked"`
	Expired  bool       `json:"expired"`
	Check    *checkView `json:"check"`
}

// checkView is a laptop's last self-check: whether it passed, when, and the checks that failed.
type checkView struct {
	OK      bool          `json:"ok"`
	At      stamp         `json:"at"`
	Failing []failedCheck `json:"failing"`
}

type failedCheck struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func (l laptopRow) view() laptopView {
	v := laptopView{ID: l.ID, Handle: l.Handle, Email: l.Email, Host: l.Host, Version: l.Version, Behind: l.Behind,
		Created: stamp(l.Created), LastSeen: stamp(l.LastSeen), Online: l.Online, Revoked: l.Revoked, Expired: l.Expired}
	if l.Check != nil {
		v.Check = &checkView{OK: l.Check.OK, At: stamp(l.ReportAt), Failing: []failedCheck{}}
		for _, c := range l.Failing {
			v.Check.Failing = append(v.Check.Failing, failedCheck{c.Name, c.Detail})
		}
	}
	return v
}

// laptopRows lists every laptop, revoked ones last. It reads them from the store, where laptops record their
// version, when they were last seen and their self-checks, without telling the other instances.
func (h *Hub) laptopRows(ctx context.Context) ([]laptopRow, error) {
	ds, err := h.store.Devices(ctx)
	if err != nil {
		return nil, err
	}
	s := h.Settings()
	h.mu.Lock()
	online := h.onlineLocked()
	handles := map[string]string{}
	for _, u := range h.users {
		handles[u.Email] = u.Handle
	}
	h.mu.Unlock()
	rows := make([]laptopRow, 0, len(ds))
	for _, d := range ds {
		l := laptopRow{Device: d, Handle: cmp.Or(handles[d.Email], d.Email), Online: online[d.ID] && !d.Revoked,
			Expired: !d.Revoked && s.expired(d), Behind: h.behind(d.Version, s)}
		if json.Unmarshal(d.Report, &l.Check) == nil && l.Check != nil {
			for _, c := range l.Check.Checks {
				if !c.OK {
					l.Failing = append(l.Failing, c)
				}
			}
			if l.ReportAt.IsZero() {
				l.ReportAt = time.UnixMilli(l.Check.At)
			}
		}
		rows = append(rows, l)
	}
	slices.SortFunc(rows, func(a, b laptopRow) int {
		return cmp.Or(last(a.Revoked, b.Revoked), strings.Compare(a.Handle, b.Handle), b.LastSeen.Compare(a.LastSeen))
	})
	return rows, nil
}

// behind says why a laptop's version needs a look, if it does: the hub refuses it, or has a newer one for it.
func (h *Hub) behind(v string, s Settings) string {
	switch {
	case v == "": // it never said hello
		return ""
	case s.MinVersion != "" && release.Before(v, s.MinVersion):
		return "older than " + s.MinVersion + ", the oldest accepted"
	case semver.MatchString(h.cfg.Version) && release.Before(v, h.cfg.Version):
		return "older than this hub's " + strings.TrimPrefix(h.cfg.Version, "v")
	}
	return ""
}

func (h *Hub) adminLaptops(w http.ResponseWriter, r *http.Request, _ store.User) {
	rows, err := h.laptopRows(r.Context())
	if err != nil {
		h.failed(w, "read laptops", err)
		return
	}
	laptops := make([]laptopView, 0, len(rows))
	for _, l := range rows {
		laptops = append(laptops, l.view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"laptops": laptops})
}

// revokeLaptop revokes any laptop: it disconnects at once and can't connect again.
func (h *Hub) revokeLaptop(w http.ResponseWriter, r *http.Request, me store.User) {
	var sent struct {
		ID string `json:"id"`
	}
	if !readBody(w, r, &sent) {
		return
	}
	d, _, ok := h.findLaptop(sent.ID)
	if !ok {
		refuse(w, http.StatusNotFound, "This laptop is unknown or already revoked.", "id")
		return
	}
	if err := h.revoke(r.Context(), d.ID, me); err != nil {
		refuse(w, http.StatusInternalServerError, "The hub couldn't save this. Try again.", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": d.ID})
}

// findLaptop finds a laptop that isn't revoked, and its person's handle.
func (h *Hub) findLaptop(id string) (store.Device, string, bool) {
	var d store.Device
	h.mu.Lock()
	defer h.mu.Unlock()
	found := h.devices[id]
	if found == nil || found.Revoked {
		return d, "", false
	}
	d = *found
	if u := h.users[d.Email]; u != nil {
		return d, u.Handle, true
	}
	return d, d.Email, true
}

// ---------------------------------------------------------------- people

// personView is one person as the people page shows them.
type personView struct {
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Admin    bool   `json:"admin"`
	InAdmins bool   `json:"in_admins_env"` // listed in ADMINS, so made an admin again at each sign-in
	Removed  bool   `json:"removed"`
	Laptops  int    `json:"laptops"` // signed in: neither revoked nor expired
	Online   bool   `json:"online"`
	LastSeen stamp  `json:"last_seen"` // on any of their laptops
	Colour   int    `json:"colour"`
	You      bool   `json:"you"`
}

// joinLists are who may join, one entry per line, as the people page shows them and an admin sends them back.
type joinLists struct {
	AllowedDomains string `json:"allowed_domains"`
	AllowedEmails  string `json:"allowed_emails"`
	AllowedOrgs    string `json:"allowed_orgs"`
}

func joinOf(s Settings) joinLists {
	return joinLists{strings.Join(s.AllowedDomains, "\n"), strings.Join(s.AllowedEmails, "\n"), strings.Join(s.AllowedOrgs, "\n")}
}

// adminPeople shows everyone, removed people last, and who may join.
func (h *Hub) adminPeople(w http.ResponseWriter, r *http.Request, me store.User) {
	laptops, err := h.laptopRows(r.Context())
	if err != nil {
		h.failed(w, "read laptops", err)
		return
	}
	byEmail := map[string]*personView{}
	admins := 0
	h.mu.Lock()
	colours := h.coloursLocked()
	for _, u := range h.users {
		byEmail[u.Email] = &personView{Handle: u.Handle, Name: u.Name, Email: u.Email, Admin: u.Admin,
			InAdmins: slices.Contains(h.cfg.Admins, u.Email), Removed: u.Disabled, Colour: colours[u.Email], You: u.Email == me.Email}
		if u.Admin && !u.Disabled {
			admins++
		}
	}
	h.mu.Unlock()
	for _, l := range laptops {
		p := byEmail[l.Email]
		if p == nil {
			continue
		}
		if !l.Revoked && !l.Expired {
			p.Laptops++
			p.Online = p.Online || l.Online
		}
		if l.LastSeen.After(time.Time(p.LastSeen)) {
			p.LastSeen = stamp(l.LastSeen)
		}
	}
	people := make([]personView, 0, len(byEmail))
	for _, p := range byEmail {
		people = append(people, *p)
	}
	slices.SortFunc(people, func(a, b personView) int {
		return cmp.Or(last(a.Removed, b.Removed), strings.Compare(a.Handle, b.Handle))
	})
	writeJSON(w, http.StatusOK, map[string]any{"people": people, "only_admin": admins <= 1, "provider": h.providerName(),
		"join": joinOf(h.Settings())})
}

// joinPost changes who may join: email domains, single addresses and GitHub organisations. A list left out stays as
// it is.
func (h *Hub) joinPost(w http.ResponseWriter, r *http.Request, me store.User) {
	s := h.Settings()
	sent := joinOf(s)
	if !readBody(w, r, &sent) {
		return
	}
	s.AllowedDomains, s.AllowedEmails, s.AllowedOrgs = SplitList(sent.AllowedDomains), SplitList(sent.AllowedEmails), SplitList(sent.AllowedOrgs)
	if !h.saveSettings(w, r, s, me) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "join": joinOf(h.Settings())})
}

// person finds whom an admin action names, or says there is nobody by that name.
func (h *Hub) person(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	var sent struct {
		Handle string `json:"handle"`
	}
	if !readBody(w, r, &sent) {
		return store.User{}, false
	}
	var p store.User
	h.mu.Lock()
	found := h.handles[sent.Handle]
	if found != nil {
		p = *found
	}
	h.mu.Unlock()
	if found == nil {
		refuse(w, http.StatusNotFound, "Nobody on this team goes by that name.", "handle")
		return p, false
	}
	return p, true
}

// personDone answers an action on a person with where they stand now. A page that took its own admin rights away
// leaves the panel.
type personDone struct {
	OK      bool   `json:"ok"`
	Handle  string `json:"handle"`
	Admin   bool   `json:"admin"`
	Removed bool   `json:"removed"`
	You     bool   `json:"you"`
}

func done(p, me store.User) personDone {
	return personDone{OK: true, Handle: p.Handle, Admin: p.Admin, Removed: p.Disabled, You: p.Email == me.Email}
}

func (h *Hub) promote(w http.ResponseWriter, r *http.Request, me store.User) {
	h.setAdmin(w, r, me, true)
}

func (h *Hub) demote(w http.ResponseWriter, r *http.Request, me store.User) {
	h.setAdmin(w, r, me, false)
}

// onlyAdmin says whether p is the last admin who can sign in: taking their rights away would leave nobody to manage
// the team until someone edits the hub's configuration.
func (h *Hub) onlyAdmin(p store.User) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range h.users {
		if u.Admin && !u.Disabled && u.Email != p.Email {
			return false
		}
	}
	return true
}

// setAdmin gives someone admin rights or takes them away, their own included, never from the last admin. Someone
// in ADMINS becomes an admin again at their next sign-in.
func (h *Hub) setAdmin(w http.ResponseWriter, r *http.Request, me store.User, admin bool) {
	p, ok := h.person(w, r)
	if !ok {
		return
	}
	switch {
	case p.Admin == admin: // sent twice
	case admin && p.Disabled:
		refuse(w, http.StatusBadRequest, p.Handle+" was removed from the team. Let them back in first.", "")
		return
	case !admin && h.onlyAdmin(p):
		refuse(w, http.StatusBadRequest, p.Handle+" is the team's only admin. Make someone else an admin first.", "")
		return
	default:
		// ponytail: two admins taking each other's rights at the same moment can both succeed; whoever is in ADMINS
		// becomes an admin again at their next sign-in
		p.Admin = admin
		if err := h.putUser(r.Context(), p); err != nil {
			h.failed(w, "change who is an admin", err)
			return
		}
		kind := "made admin"
		if !admin {
			kind = "admin rights removed"
		}
		h.event(r.Context(), kind, me.Handle, p.Handle, "")
	}
	writeJSON(w, http.StatusOK, done(p, me))
}

func (h *Hub) removePost(w http.ResponseWriter, r *http.Request, me store.User) {
	h.setRemoved(w, r, me, true)
}

func (h *Hub) restorePost(w http.ResponseWriter, r *http.Request, me store.User) {
	h.setRemoved(w, r, me, false)
}

// setRemoved removes a person, or lets them back in. Removing revokes all their laptops and drops their
// connections on every instance, deletes the messages kept for them, takes them off the team, and refuses their
// sign-ins and their browser. Their account with the identity provider is not touched. Restoring lets them sign in
// again; their old laptops stay revoked. Nobody removes themselves, so the admin removing someone stays.
func (h *Hub) setRemoved(w http.ResponseWriter, r *http.Request, me store.User, removed bool) {
	person, ok := h.person(w, r)
	if !ok {
		return
	}
	if person.Email == me.Email {
		refuse(w, http.StatusBadRequest, "You can't remove yourself. Another admin can.", "")
		return
	}
	var laptops []store.Device
	h.mu.Lock()
	for _, d := range h.devices {
		if removed && d.Email == person.Email && !d.Revoked {
			laptops = append(laptops, *d)
		}
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	person.Disabled = removed
	err := h.putUser(ctx, person)
	for _, d := range laptops {
		d.Revoked = true
		if e := h.putDevice(ctx, d); e != nil {
			err = e
		}
	}
	if removed {
		kept, e := h.store.Pending(ctx, person.Email)
		for _, p := range kept {
			if e == nil {
				e = h.store.DropPending(ctx, p.Msg.ID)
			}
		}
		if e != nil {
			err = e
		}
	}
	if err != nil {
		h.log.Error("database", "op", "remove or restore a person", "person", person.Handle, "error", err.Error())
		refuse(w, http.StatusInternalServerError, "Part of this didn't save. Try again: it is safe to repeat.", "")
		return
	}
	event := "person restored"
	if removed {
		event = "person removed"
	}
	h.event(ctx, event, me.Handle, person.Handle, "")
	writeJSON(w, http.StatusOK, done(person, me))
}

// ---------------------------------------------------------------- settings

// settingsValues are the team's settings as the admin panel shows and changes them, by the names of the settings
// form: boxes of one entry per line are text with a line each. Who may join is on the people page.
type settingsValues struct {
	TokenDays        int    `json:"token_days"`
	SendsPerMinute   int    `json:"sends_per_minute"`
	LoopMax          int    `json:"loop_max"`
	LoopMinutes      int    `json:"loop_minutes"`
	MaxAgents        int    `json:"max_agents"`
	MaxLaptops       int    `json:"max_laptops"`
	DownloadsPerHour int    `json:"downloads_per_hour"`
	IgnoredPaths     string `json:"ignored_paths"`
	UpdatePolicy     string `json:"update_policy"`
	MinVersion       string `json:"min_version"`
	ReleaseCheck     bool   `json:"release_check"`
	Agreements       string `json:"agreements"`
	AnnounceHours    int    `json:"announce_hours"`
	ChatWebhook      string `json:"chat_webhook"`
	ChatKind         string `json:"chat_kind"`
	WaitPingMinutes  int    `json:"wait_ping_minutes"`
	WorkStart        int    `json:"work_start"`
	WorkEnd          int    `json:"work_end"`
	TimeZone         string `json:"time_zone"`
	Weekdays         bool   `json:"weekdays"`
	WeeklyReport     bool   `json:"weekly_report"`
	CheckHours       int    `json:"check_hours"`
}

func valuesOf(s Settings) settingsValues {
	return settingsValues{TokenDays: s.TokenDays, SendsPerMinute: s.SendsPerMinute, LoopMax: s.LoopMax,
		LoopMinutes: s.LoopMinutes, MaxAgents: s.MaxAgents, MaxLaptops: s.MaxLaptops, DownloadsPerHour: s.DownloadsPerHour,
		IgnoredPaths: strings.Join(s.IgnoredPaths, "\n"), UpdatePolicy: s.UpdatePolicy, MinVersion: s.MinVersion,
		ReleaseCheck: s.ReleaseCheck, Agreements: s.Agreements, AnnounceHours: s.AnnounceHours, ChatWebhook: s.ChatWebhook,
		ChatKind: s.ChatKind, WaitPingMinutes: s.WaitPingMinutes, WorkStart: s.WorkStart, WorkEnd: s.WorkEnd,
		TimeZone: s.TimeZone, Weekdays: s.Weekdays, WeeklyReport: s.WeeklyReport, CheckHours: s.CheckHours}
}

// settingsView is the settings page: the values, whether GitHub's secret and token are set, which never leave the
// hub, and who changed the settings last, and when.
type settingsView struct {
	Values          settingsValues `json:"settings"`
	GitHubSecretSet bool           `json:"github_secret_set"`
	GitHubTokenSet  bool           `json:"github_token_set"`
	Updated         stamp          `json:"updated"`
	UpdatedBy       string         `json:"updated_by"`
}

func viewSettings(s Settings) settingsView {
	return settingsView{Values: valuesOf(s), GitHubSecretSet: s.GitHubWebhookSecret != "", GitHubTokenSet: s.GitHubToken != "",
		Updated: stamp(s.Updated), UpdatedBy: s.UpdatedBy}
}

// settingsSent is what an admin sends to change the settings: the values, of which those left out stay as they
// are, and a new GitHub secret or token to replace the stored one, or forget to clear it. Left empty, they stay.
type settingsSent struct {
	settingsValues
	GitHubSecret       string `json:"github_secret"`
	GitHubToken        string `json:"github_token"`
	ForgetGitHubSecret bool   `json:"forget_github_secret"`
	ForgetGitHubToken  bool   `json:"forget_github_token"`
}

// over is s changed as sent, tidied the way the settings form always did.
func (f settingsSent) over(s Settings) Settings {
	v := f.settingsValues
	s.TokenDays, s.SendsPerMinute, s.LoopMax, s.LoopMinutes = v.TokenDays, v.SendsPerMinute, v.LoopMax, v.LoopMinutes
	s.MaxAgents, s.MaxLaptops, s.DownloadsPerHour, s.CheckHours = v.MaxAgents, v.MaxLaptops, v.DownloadsPerHour, v.CheckHours
	s.WaitPingMinutes, s.WorkStart, s.WorkEnd, s.AnnounceHours = v.WaitPingMinutes, v.WorkStart, v.WorkEnd, v.AnnounceHours
	s.IgnoredPaths = lines(v.IgnoredPaths)
	s.UpdatePolicy = v.UpdatePolicy
	s.MinVersion = strings.TrimSpace(v.MinVersion)
	s.ReleaseCheck = v.ReleaseCheck
	s.Agreements = strings.TrimSpace(strings.ReplaceAll(v.Agreements, "\r\n", "\n"))
	s.ChatWebhook, s.ChatKind = strings.TrimSpace(v.ChatWebhook), v.ChatKind
	s.TimeZone = strings.TrimSpace(v.TimeZone)
	s.Weekdays, s.WeeklyReport = v.Weekdays, v.WeeklyReport
	for _, x := range []struct {
		v      *string
		typed  string
		forget bool
	}{{&s.GitHubWebhookSecret, f.GitHubSecret, f.ForgetGitHubSecret}, {&s.GitHubToken, f.GitHubToken, f.ForgetGitHubToken}} {
		switch {
		case x.forget:
			*x.v = ""
		case strings.TrimSpace(x.typed) != "":
			*x.v = strings.TrimSpace(x.typed)
		}
	}
	return s
}

func (h *Hub) adminSettings(w http.ResponseWriter, _ *http.Request, _ store.User) {
	writeJSON(w, http.StatusOK, viewSettings(h.Settings()))
}

func (h *Hub) settingsPost(w http.ResponseWriter, r *http.Request, me store.User) {
	s := h.Settings()
	sent := settingsSent{settingsValues: valuesOf(s)}
	if !readBody(w, r, &sent) {
		return
	}
	// laptops update to this hub's version, so an oldest accepted version past it would keep every one out. A hub
	// rolled back below the stored one still saves the other settings.
	next := sent.over(s)
	if v := strings.TrimPrefix(next.MinVersion, "v"); v != s.MinVersion && semver.MatchString(v) && semver.MatchString(h.cfg.Version) &&
		release.Before(h.cfg.Version, v) {
		refuse(w, http.StatusBadRequest, "The oldest accepted version can't be newer than this hub's "+strings.TrimPrefix(h.cfg.Version, "v")+
			": laptops update to the hub's version.", "min_version")
		return
	}
	if !h.saveSettings(w, r, next, me) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
		settingsView
	}{true, viewSettings(h.Settings())})
}

// saveSettings saves s for the team, or answers why it can't, naming the setting at fault.
func (h *Hub) saveSettings(w http.ResponseWriter, r *http.Request, s Settings, me store.User) bool {
	if err := s.Valid(); err != nil {
		var bad settingError
		errors.As(err, &bad)
		refuse(w, http.StatusBadRequest, sentence(err.Error()), bad.field)
		return false
	}
	if err := h.SaveSettings(r.Context(), s, me.Handle); err != nil {
		h.failed(w, "save settings", err)
		return false
	}
	return true
}

// lines reads a text box that holds one entry per line. Paths keep their case and may hold spaces, so this is not
// SplitList.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------- events

const eventsPerPage = 200

// eventView is one event in the log.
type eventView struct {
	At      stamp  `json:"at"`
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// adminEvents shows the event log, newest first, a page at a time; skip counts the newer events left out. Newer and
// older are the skip of the pages around this one, when there are.
func (h *Hub) adminEvents(w http.ResponseWriter, r *http.Request, _ store.User) {
	skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
	skip = min(max(skip, 0), 10000) // the store reads every newer event too
	evs, err := h.store.Events(r.Context(), skip+eventsPerPage+1)
	if err != nil {
		h.failed(w, "read events", err)
		return
	}
	var page struct {
		Events []eventView `json:"events"`
		Skip   int         `json:"skip"`
		Newer  *int        `json:"newer"`
		Older  *int        `json:"older"`
	}
	page.Events, page.Skip = []eventView{}, skip
	for _, e := range evs[min(skip, len(evs)):min(skip+eventsPerPage, len(evs))] {
		page.Events = append(page.Events, eventView{stamp(e.At), e.Kind, e.Actor, e.Subject, e.Detail})
	}
	if skip > 0 {
		newer := max(skip-eventsPerPage, 0)
		page.Newer = &newer
	}
	if len(evs) > skip+eventsPerPage {
		older := skip + eventsPerPage
		page.Older = &older
	}
	writeJSON(w, http.StatusOK, page)
}
