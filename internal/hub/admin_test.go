package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/store"
	"github.com/coder/websocket"
)

// team is a running hub where alice is an admin and bob is not, each with a laptop, both signed in to a browser.
type team struct {
	h          *Hub
	st         store.Store
	url        string
	alice, bob *http.Client
}

// newTeam starts a team's hub. before writes to the store before the hub starts, when the test needs more.
func newTeam(t *testing.T, cfg Config, before func(context.Context, store.Store)) *team {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemory()
	for _, u := range []store.User{{Email: "alice@x.com", Handle: "alice", Admin: true}, {Email: "bob@x.com", Handle: "bob"}} {
		_ = st.PutUser(ctx, u)
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + u.Handle, Email: u.Email, Host: u.Handle + "-mbp", TokenHash: hashToken("tok-" + u.Handle), Created: time.Now()})
	}
	if before != nil {
		before(ctx, st)
	}
	cfg.Secret, cfg.DevLogin, cfg.PublicURL = []byte("0123456789abcdef"), true, "http://localhost"
	h, err := newHub(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	tm := &team{h: h, st: st, url: srv.URL}
	tm.alice, _ = signedIn(t, srv.URL, "alice@x.com")
	tm.bob, _ = signedIn(t, srv.URL, "bob@x.com")
	return tm
}

// signedIn is a browser that signed in as email through the dev login, with the status the sign-in got. It keeps
// its cookie and doesn't follow redirects. With no email it stays signed out.
func signedIn(t *testing.T, hub, email string) (*http.Client, int) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if email == "" {
		return c, 0
	}
	resp, err := c.PostForm(hub+"/dev-login", url.Values{"email": {email}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return c, resp.StatusCode
}

func (tm *team) get(t *testing.T, c *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Get(tm.url + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// post sends a form and returns the status, where it redirects to, and the page.
func (tm *team) post(t *testing.T, c *http.Client, path string, form url.Values) (int, string, string) {
	t.Helper()
	resp, err := c.PostForm(tm.url+path, form)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func (tm *team) user(email string) store.User {
	tm.h.mu.Lock()
	defer tm.h.mu.Unlock()
	return *tm.h.users[email]
}

// hasEvent says whether the event log holds an event of this kind, by actor, about subject.
func (tm *team) hasEvent(kind, actor, subject string) bool {
	evs, _ := tm.st.Events(context.Background(), 100)
	return slices.ContainsFunc(evs, func(e store.Event) bool { return e.Kind == kind && e.Actor == actor && e.Subject == subject })
}

// call sends a request to the admin API as c, with body as JSON when there is one, and reads the answer into out
// when it is a success. It returns the status, and what the hub said was wrong when it isn't. Every answer must be
// JSON, and every failure must say why.
func (tm *team) call(t *testing.T, c *http.Client, method, path string, body, out any) (int, problem) {
	t.Helper()
	var sent io.Reader
	if body != nil {
		sent = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequest(method, tm.url+path, sent)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: %d with %q, not JSON:\n%s", method, path, resp.StatusCode, ct, b)
	}
	var p problem
	switch {
	case resp.StatusCode >= 400:
		if json.Unmarshal(b, &p) != nil || p.Error == "" {
			t.Fatalf("%s %s: %d without a reason: %s", method, path, resp.StatusCode, b)
		}
	case out != nil:
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, b)
		}
	}
	return resp.StatusCode, p
}

// handle is the body of an action on a person.
func handle(h string) map[string]string { return map[string]string{"handle": h} }

// peoplePage is what GET /api/admin/people answers.
type peoplePage struct {
	People    []personView `json:"people"`
	OnlyAdmin bool         `json:"only_admin"`
	Provider  string       `json:"provider"`
	Join      joinLists    `json:"join"`
}

// Only admins use the admin API: a browser that is signed out is told so, anyone else is refused, and neither
// changes anything.
func TestAdminPanelIsForAdmins(t *testing.T) {
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		_ = st.PutUser(ctx, store.User{Email: "carol@x.com", Handle: "carol", Admin: true}) // so alice may step down
	})
	visitor, _ := signedIn(t, tm.url, "")
	gets := []string{"/api/admin", "/api/admin/people", "/api/admin/laptops", "/api/admin/settings", "/api/admin/events"}
	posts := map[string]any{
		"/api/admin/people/join":    joinLists{AllowedDomains: "gmail.com"},
		"/api/admin/people/promote": handle("bob"),
		"/api/admin/people/demote":  handle("alice"),
		"/api/admin/people/remove":  handle("alice"),
		"/api/admin/people/restore": handle("alice"),
		"/api/admin/laptops/revoke": map[string]string{"id": "d-alice"},
		"/api/admin/settings":       map[string]int{"sends_per_minute": 999},
	}
	before := tm.h.Settings()
	try := func(method, path string, body any) {
		t.Helper()
		if status, p := tm.call(t, visitor, method, path, body, nil); status != http.StatusUnauthorized || !strings.Contains(p.Error, "signed out") {
			t.Errorf("signed out, %s %s: %d %q", method, path, status, p.Error)
		}
		if status, p := tm.call(t, tm.bob, method, path, body, nil); status != http.StatusForbidden || p.Error != "Only the team's admins can use the admin panel." {
			t.Errorf("not an admin, %s %s: %d %q", method, path, status, p.Error)
		}
	}
	for _, p := range gets {
		try(http.MethodGet, p, nil)
		if status, _ := tm.call(t, tm.alice, http.MethodGet, p, nil, nil); status != http.StatusOK {
			t.Errorf("admin, GET %s: %d", p, status)
		}
	}
	for p, body := range posts {
		try(http.MethodPost, p, body)
	}
	if !reflect.DeepEqual(tm.h.Settings(), before) {
		t.Fatal("settings changed")
	}
	tm.h.mu.Lock()
	revoked := tm.h.devices["d-alice"].Revoked
	tm.h.mu.Unlock()
	if alice, bob := tm.user("alice@x.com"), tm.user("bob@x.com"); !alice.Admin || alice.Disabled || bob.Admin || revoked {
		t.Fatalf("someone who isn't an admin changed the team: alice %+v, bob %+v, alice's laptop revoked %v", alice, bob, revoked)
	}
}

// The admin API takes JSON only, up to the hub's cap, and names what it can't read.
func TestAdminTakesJSON(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	raw := func(path, contentType, body string) (int, problem) {
		t.Helper()
		resp, err := tm.alice.Post(tm.url+path, contentType, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var p problem
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || p.Error == "" {
			t.Fatalf("POST %s %q: %d without a reason", path, body, resp.StatusCode)
		}
		return resp.StatusCode, p
	}
	if status, _ := raw("/api/admin/people/promote", "application/x-www-form-urlencoded", "handle=bob"); status != http.StatusUnsupportedMediaType {
		t.Errorf("a form: %d", status)
	}
	if status, _ := raw("/api/admin/people/promote", "application/json", `{"handle":`); status != http.StatusBadRequest {
		t.Errorf("broken JSON: %d", status)
	}
	if status, p := raw("/api/admin/people/promote", "application/json; charset=utf-8", `{"handle":3}`); status != http.StatusBadRequest || p.Field != "handle" {
		t.Errorf("a number for a handle: %d %+v", status, p)
	}
	if status, p := raw("/api/admin/people/promote", "application/json", `{"who":"bob"}`); status != http.StatusBadRequest || p.Field != "who" {
		t.Errorf("an unknown field: %d %+v", status, p)
	}
	big := `{"agreements":"` + strings.Repeat("a", 300<<10) + `"}`
	if status, _ := raw("/api/admin/settings", "application/json", big); status != http.StatusRequestEntityTooLarge {
		t.Errorf("300 KB: %d", status)
	}
	if tm.user("bob@x.com").Admin || tm.h.Settings().Agreements != "" {
		t.Fatal("a request the hub couldn't read changed the team")
	}
}

// An admin reads and changes the settings: this hub uses them at once, and the API and the event log say who
// changed what. A setting left out keeps its value. Settings that aren't valid are refused with the reason and the
// field at fault, and change nothing.
func TestAdminChangesSettings(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	var view settingsView
	if status, _ := tm.call(t, tm.alice, http.MethodGet, "/api/admin/settings", nil, &view); status != http.StatusOK ||
		view.Values != valuesOf(tm.h.Settings()) || view.Values.SendsPerMinute != Defaults().SendsPerMinute ||
		!time.Time(view.Updated).IsZero() || view.UpdatedBy != "" {
		t.Fatalf("settings before any change: %d %+v", status, view)
	}
	var saved struct {
		OK bool `json:"ok"`
		settingsView
	}
	status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{"sends_per_minute": 35,
		"update_policy": "notify", "min_version": "v1.4.0", "ignored_paths": "traces/**\r\n\r\n Vendor/*.Go \r\n", "release_check": false}, &saved)
	if status != http.StatusOK || !saved.OK {
		t.Fatalf("save: %d", status)
	}
	s := tm.h.Settings()
	if s.SendsPerMinute != 35 || s.UpdatePolicy != "notify" || s.MinVersion != "1.4.0" || s.ReleaseCheck ||
		!slices.Equal(s.IgnoredPaths, []string{"traces/**", "Vendor/*.Go"}) || !slices.Equal(s.AllowedDomains, []string{"x.com"}) ||
		s.LoopMax != Defaults().LoopMax || s.TimeZone != "UTC" || s.UpdatedBy != "alice" {
		t.Fatalf("settings: %+v", s)
	}
	if saved.Values != valuesOf(s) || saved.Values.IgnoredPaths != "traces/**\nVendor/*.Go" || saved.UpdatedBy != "alice" ||
		time.Since(time.Time(saved.Updated)) > time.Minute {
		t.Fatalf("the answer doesn't show the saved settings and who saved them: %+v", saved.settingsView)
	}
	if tm.call(t, tm.alice, http.MethodGet, "/api/admin/settings", nil, &view); view.UpdatedBy != "alice" || view.Values != saved.Values {
		t.Fatalf("settings after the change: %+v", view)
	}
	evs, _ := tm.st.Events(context.Background(), 1)
	if len(evs) != 1 || evs[0].Kind != "settings changed" || evs[0].Detail != "ignored_paths, min_version, release_check, sends_per_minute, update_policy" {
		t.Fatalf("event: %+v", evs)
	}

	for _, c := range []struct {
		field string
		value any
		want  string
	}{
		{"sends_per_minute", 0, "Messages per minute must be between 1 and 1000."},
		{"token_days", 4000, "Days a laptop stays signed in must be between 0 and 3650."},
		{"loop_max", "twelve", "whole number"},
		{"loop_max", 12.5, "whole number"},
		{"min_version", "latest", `Oldest accepted version "latest": write it like 1.4.0.`},
		{"ignored_paths", "src/[", `Ignored path "src/["`},
		{"update_policy", "sometimes", "Update policy"},
		{"work_end", 8, "The working day must end after it starts."},
		{"time_zone", "Mars/Olympus", "Time zone"},
		{"chat_kind", "irc", "Chat kind"},
		{"agreements", strings.Repeat("a", 4001), "longer than 4000 characters"},
		{"weekdays", "yes", "true or false"},
		{"allowed_domains", "y.org", "doesn't know"}, // who may join is on the people page
		{"github_secret_set", false, "doesn't know"},
	} {
		status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{c.field: c.value}, nil)
		if status != http.StatusBadRequest || p.Field != c.field || !strings.Contains(p.Error, c.want) {
			t.Errorf("%s=%v: %d %+v", c.field, c.value, status, p)
		}
		if !reflect.DeepEqual(tm.h.Settings(), s) {
			t.Fatalf("%s=%v changed the settings", c.field, c.value)
		}
	}
}

// Admins change who may join on the people page. A list left out stays; a list with a mistake is refused, naming
// it, and changes nothing.
func TestAdminChangesWhoMayJoin(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	if _, status := signedIn(t, tm.url, "carol@y.org"); status != http.StatusForbidden {
		t.Fatalf("carol signed in before her domain was allowed: %d", status)
	}
	var saved struct {
		OK   bool      `json:"ok"`
		Join joinLists `json:"join"`
	}
	sent := joinLists{AllowedDomains: "x.com\r\n@Y.org", AllowedEmails: "dan@gmail.com"}
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/join", sent, &saved); status != http.StatusOK || !saved.OK ||
		saved.Join != (joinLists{AllowedDomains: "x.com\ny.org", AllowedEmails: "dan@gmail.com"}) {
		t.Fatalf("save: %d %+v", status, saved)
	}
	for _, email := range []string{"carol@y.org", "dan@gmail.com"} {
		if _, status := signedIn(t, tm.url, email); status != http.StatusFound {
			t.Errorf("%s can't sign in: %d", email, status)
		}
	}
	s := tm.h.Settings()
	if !slices.Equal(s.AllowedDomains, []string{"x.com", "y.org"}) || !slices.Equal(s.AllowedEmails, []string{"dan@gmail.com"}) ||
		s.SendsPerMinute != Defaults().SendsPerMinute || !tm.hasEvent("settings changed", "alice", "") {
		t.Fatalf("settings: %+v", s)
	}
	var page peoplePage
	if tm.call(t, tm.alice, http.MethodGet, "/api/admin/people", nil, &page); page.Join != saved.Join {
		t.Fatalf("the people page shows %+v", page.Join)
	}
	for field, sent := range map[string]joinLists{"allowed_emails": {AllowedEmails: "erin"}, "allowed_domains": {AllowedDomains: "erin@y.org"}} {
		status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/join", sent, nil)
		if status != http.StatusBadRequest || p.Field != field || !strings.Contains(p.Error, `"erin`) {
			t.Errorf("a mistake in %s: %d %+v", field, status, p)
		}
	}
	if !reflect.DeepEqual(tm.h.Settings(), s) {
		t.Fatal("a refused list changed the settings")
	}
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/join", map[string]string{"allowed_orgs": "Acme"}, nil); status != http.StatusOK ||
		!slices.Equal(tm.h.Settings().AllowedOrgs, []string{"acme"}) || !slices.Equal(tm.h.Settings().AllowedDomains, s.AllowedDomains) {
		t.Fatalf("one list: %d %+v", status, tm.h.Settings())
	}
}

// The people page lists everyone, removed people last, with their role, whether ADMINS names them, their laptops
// and colour, and who is looking.
func TestAdminPeople(t *testing.T) {
	tm := newTeam(t, Config{Admins: []string{"alice@x.com"}}, func(ctx context.Context, st store.Store) {
		_ = st.PutUser(ctx, store.User{Email: "carol@x.com", Handle: "carol", Name: "Carol Ray", Disabled: true})
		_ = st.PutDevice(ctx, store.Device{ID: "d-bob", Email: "bob@x.com", Host: "bob-mbp", TokenHash: hashToken("tok-bob"),
			Created: time.Now(), LastSeen: time.Now().Add(-time.Hour)})
		_ = st.PutDevice(ctx, store.Device{ID: "d-bob2", Email: "bob@x.com", TokenHash: hashToken("tok-bob2"), Created: time.Now(), Revoked: true})
	})
	connect(t, tm.url, "tok-alice")
	var page peoplePage
	if status, _ := tm.call(t, tm.alice, http.MethodGet, "/api/admin/people", nil, &page); status != http.StatusOK || len(page.People) != 3 {
		t.Fatalf("%d %+v", status, page)
	}
	alice, bob, carol := page.People[0], page.People[1], page.People[2]
	if alice.Handle != "alice" || !alice.Admin || !alice.InAdmins || !alice.You || alice.Laptops != 1 || !alice.Online || alice.Colour != 0 {
		t.Errorf("alice: %+v", alice)
	}
	if bob.Handle != "bob" || bob.Email != "bob@x.com" || bob.Admin || bob.InAdmins || bob.You || bob.Laptops != 1 || bob.Online ||
		time.Since(time.Time(bob.LastSeen)).Round(time.Hour) != time.Hour || bob.Colour != 1 {
		t.Errorf("bob: %+v", bob)
	}
	if carol.Handle != "carol" || carol.Name != "Carol Ray" || !carol.Removed || carol.Laptops != 0 || !time.Time(carol.LastSeen).IsZero() || carol.Colour != 2 {
		t.Errorf("carol: %+v", carol)
	}
	if !page.OnlyAdmin || page.Provider != "email (local)" || page.Join.AllowedDomains != "x.com" {
		t.Errorf("page: %+v", page)
	}
}

// Admins make others admins and take the rights away, their own included, but the team always keeps an admin.
func TestAdminRights(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	var page peoplePage
	var done personDone

	// alice is the only admin: she can't step down
	if tm.call(t, tm.alice, http.MethodGet, "/api/admin/people", nil, &page); !page.OnlyAdmin {
		t.Fatal("the people page doesn't say alice is the only admin")
	}
	if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/demote", handle("alice"), nil); status != http.StatusBadRequest ||
		p.Error != "alice is the team's only admin. Make someone else an admin first." || !tm.user("alice@x.com").Admin {
		t.Fatalf("the only admin stepped down: %d %+v", status, p)
	}
	// she makes bob an admin, and he can use the panel at once; asking twice changes nothing more
	for range 2 {
		if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/promote", handle("bob"), &done); status != http.StatusOK ||
			done != (personDone{OK: true, Handle: "bob", Admin: true}) {
			t.Fatalf("promote: %d %+v", status, done)
		}
	}
	if status, _ := tm.call(t, tm.bob, http.MethodGet, "/api/admin", nil, nil); status != http.StatusOK || !tm.hasEvent("made admin", "alice", "bob") {
		t.Fatalf("bob after being made admin: %d", status)
	}
	evs, _ := tm.st.Events(context.Background(), 100)
	if n := len(slices.DeleteFunc(evs, func(e store.Event) bool { return e.Kind != "made admin" })); n != 1 {
		t.Fatalf("%d events for one promotion", n)
	}
	if tm.call(t, tm.bob, http.MethodGet, "/api/admin/people", nil, &page); page.OnlyAdmin {
		t.Fatal("the people page says there is one admin")
	}
	// bob takes alice's rights away
	if status, _ := tm.call(t, tm.bob, http.MethodPost, "/api/admin/people/demote", handle("alice"), &done); status != http.StatusOK ||
		done != (personDone{OK: true, Handle: "alice"}) || tm.user("alice@x.com").Admin {
		t.Fatalf("demote: %d %+v", status, done)
	}
	if status, _ := tm.call(t, tm.alice, http.MethodGet, "/api/admin", nil, nil); status != http.StatusForbidden || !tm.hasEvent("admin rights removed", "bob", "alice") {
		t.Fatalf("alice after losing her rights: %d", status)
	}
	// now bob is the last admin
	if status, _ := tm.call(t, tm.bob, http.MethodPost, "/api/admin/people/demote", handle("bob"), nil); status != http.StatusBadRequest || !tm.user("bob@x.com").Admin {
		t.Fatalf("the last admin stepped down: %d", status)
	}
	if status, p := tm.call(t, tm.bob, http.MethodPost, "/api/admin/people/promote", handle("zed"), nil); status != http.StatusNotFound || p.Field != "handle" {
		t.Fatalf("someone who isn't on the team: %d %+v", status, p)
	}
	// with another admin, he can step down, and the answer says it was him
	tm.call(t, tm.bob, http.MethodPost, "/api/admin/people/promote", handle("alice"), nil)
	if status, _ := tm.call(t, tm.bob, http.MethodPost, "/api/admin/people/demote", handle("bob"), &done); status != http.StatusOK ||
		done != (personDone{OK: true, Handle: "bob", You: true}) || tm.user("bob@x.com").Admin {
		t.Fatalf("stepping down: %d %+v", status, done)
	}
	if status, _ := tm.call(t, tm.bob, http.MethodGet, "/api/admin", nil, nil); status != http.StatusForbidden {
		t.Fatalf("bob after stepping down: %d", status)
	}
}

// An admin removes a person: their laptop is dropped and refused, their kept messages go, their sign-in and web
// session stop working, and nobody can make them an admin. Letting them back in allows a new sign-in; the old
// laptop stays revoked. Nobody removes themselves.
func TestRemoveAPerson(t *testing.T) {
	ctx := context.Background()
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		_ = st.AddPending(ctx, store.Pending{Msg: protocol.Msg{ID: "p1", From: "alice", To: "bob", Text: "for later"}, Email: "bob@x.com",
			Created: time.Now(), Expires: time.Now().Add(time.Hour)})
	})
	laptop := connect(t, tm.url, "tok-bob")
	if _, me := tm.get(t, tm.alice, "/api/me"); !strings.Contains(me, `"admin":true`) {
		t.Fatal("the pages don't send admins to the panel")
	}
	if _, me := tm.get(t, tm.bob, "/api/me"); !strings.Contains(me, `"admin":false`) {
		t.Fatal("the pages offer the panel to someone who isn't an admin")
	}
	if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/remove", handle("alice"), nil); status != http.StatusBadRequest ||
		p.Error != "You can't remove yourself. Another admin can." || tm.user("alice@x.com").Disabled {
		t.Fatalf("alice removed herself: %d %+v", status, p)
	}
	var done personDone
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/remove", handle("bob"), &done); status != http.StatusOK ||
		done != (personDone{OK: true, Handle: "bob", Removed: true}) {
		t.Fatalf("remove: %d %+v", status, done)
	}
	for open := true; open; {
		select {
		case _, open = <-laptop.frames:
		case <-time.After(3 * time.Second):
			t.Fatal("bob's laptop stayed connected")
		}
	}
	if _, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(tm.url, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-bob"}}}); err == nil {
		t.Fatal("bob's laptop still gets in")
	}
	if p, _ := tm.st.Pending(ctx, "bob@x.com"); len(p) != 0 {
		t.Fatal("messages kept for bob stayed")
	}
	if status, _ := tm.get(t, tm.bob, "/api/me"); status != http.StatusUnauthorized {
		t.Fatal("bob's web session still works")
	}
	if status, _ := tm.call(t, tm.bob, http.MethodGet, "/api/admin", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("bob's web session still works: %d", status)
	}
	if _, status := signedIn(t, tm.url, "bob@x.com"); status != http.StatusForbidden {
		t.Fatalf("bob signed in again: %d", status)
	}
	var page peoplePage
	tm.call(t, tm.alice, http.MethodGet, "/api/admin/people", nil, &page)
	if i := slices.IndexFunc(page.People, func(p personView) bool { return p.Handle == "bob" }); i < 0 || !page.People[i].Removed ||
		page.People[i].Laptops != 0 || !tm.hasEvent("person removed", "alice", "bob") {
		t.Fatalf("the admin doesn't see who was removed: %+v", page.People)
	}
	if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/promote", handle("bob"), nil); status != http.StatusBadRequest ||
		!strings.Contains(p.Error, "Let them back in first") || tm.user("bob@x.com").Admin {
		t.Fatalf("a removed person was made admin: %d %+v", status, p)
	}
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/restore", handle("bob"), &done); status != http.StatusOK ||
		done != (personDone{OK: true, Handle: "bob"}) || !tm.hasEvent("person restored", "alice", "bob") {
		t.Fatalf("restore: %d %+v", status, done)
	}
	if _, status := signedIn(t, tm.url, "bob@x.com"); status != http.StatusFound {
		t.Fatalf("bob can't sign in after being let back in: %d", status)
	}
	if _, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(tm.url, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer tok-bob"}}}); err == nil {
		t.Fatal("bob's old laptop got back in")
	}
}

// An admin sees every laptop, whether it is online and how its last self-check went, never its token, and revokes
// one: it disconnects at once and can't come back.
func TestAdminRevokesALaptop(t *testing.T) {
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		report := mustJSON(protocol.Report{At: time.Now().UnixMilli(), Checks: []protocol.Check{
			{Name: "hooks", Detail: "no murmur hook in settings.json"}, {Name: "socket", OK: true}}})
		_ = st.PutDevice(ctx, store.Device{ID: "d-bob", Email: "bob@x.com", Host: "bob-mbp", TokenHash: hashToken("tok-bob"),
			Created: time.Now(), Report: report, ReportAt: time.Now()})
	})
	laptop := connect(t, tm.url, "tok-bob")
	var page struct {
		Laptops []laptopView `json:"laptops"`
	}
	find := func(id string) laptopView {
		t.Helper()
		page.Laptops = nil
		tm.call(t, tm.alice, http.MethodGet, "/api/admin/laptops", nil, &page)
		i := slices.IndexFunc(page.Laptops, func(l laptopView) bool { return l.ID == id })
		if i < 0 {
			t.Fatalf("no %s in %+v", id, page.Laptops)
		}
		return page.Laptops[i]
	}
	bob := find("d-bob")
	if bob.Host != "bob-mbp" || bob.Handle != "bob" || !bob.Online || bob.Revoked || bob.Check == nil || bob.Check.OK ||
		!slices.Equal(bob.Check.Failing, []failedCheck{{"hooks", "no murmur hook in settings.json"}}) || time.Since(time.Time(bob.Check.At)) > time.Minute {
		t.Errorf("bob's laptop: %+v %+v", bob, bob.Check)
	}
	if alice := find("d-alice"); alice.Check != nil || alice.Online || alice.Host != "alice-mbp" || !time.Time(alice.LastSeen).IsZero() {
		t.Errorf("alice's laptop: %+v", alice)
	}
	if _, body := tm.get(t, tm.alice, "/api/admin/laptops"); strings.Contains(body, hashToken("tok-bob")) || strings.Contains(body, "token") {
		t.Error("the laptops page shows a token")
	}
	var done struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/laptops/revoke", map[string]string{"id": "d-bob"}, &done); status != http.StatusOK ||
		!done.OK || done.ID != "d-bob" {
		t.Fatalf("revoke: %d %+v", status, done)
	}
	for open := true; open; {
		select {
		case _, open = <-laptop.frames:
		case <-time.After(3 * time.Second):
			t.Fatal("the revoked laptop stayed connected")
		}
	}
	if bob := find("d-bob"); !bob.Revoked || bob.Online || page.Laptops[len(page.Laptops)-1].ID != "d-bob" || !tm.hasEvent("laptop revoked", "alice", "d-bob") {
		t.Fatalf("the laptops page doesn't show the laptop revoked, last: %+v", page.Laptops)
	}
	if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/laptops/revoke", map[string]string{"id": "d-bob"}, nil); status != http.StatusNotFound || p.Field != "id" {
		t.Fatalf("a revoked laptop can be revoked again: %d %+v", status, p)
	}
}

// The overview counts people and laptops, flags old versions and failing checks, and shows a newer release once
// the release check found one.
func TestAdminOverview(t *testing.T) {
	failing := mustJSON(protocol.Report{At: time.Now().UnixMilli(), Checks: []protocol.Check{{Name: "claude code", Detail: "not found"}}})
	tm := newTeam(t, Config{Version: "1.2.0", Seed: seed(func(s *Settings) { s.MinVersion = "1.1.0" })}, func(ctx context.Context, st store.Store) {
		_ = st.PutUser(ctx, store.User{Email: "carol@x.com", Handle: "carol", Disabled: true})
		_ = st.PutDevice(ctx, store.Device{ID: "d-alice", Email: "alice@x.com", Host: "alice-mbp", TokenHash: hashToken("tok-alice"),
			Created: time.Now(), LastSeen: time.Now().Add(-time.Hour), Version: "1.2.0"})
		_ = st.PutDevice(ctx, store.Device{ID: "d-bob", Email: "bob@x.com", Host: "bob-mbp", TokenHash: hashToken("tok-bob"),
			Created: time.Now(), LastSeen: time.Now().Add(-30 * 24 * time.Hour), Version: "1.0.3", Report: failing})
		_ = st.PutDevice(ctx, store.Device{ID: "d-old", Email: "bob@x.com", TokenHash: hashToken("tok-old"), Created: time.Now(), Revoked: true, Version: "0.9.0"})
	})
	var o overview
	if status, _ := tm.call(t, tm.alice, http.MethodGet, "/api/admin", nil, &o); status != http.StatusOK {
		t.Fatal(status)
	}
	if o.Version != "1.2.0" || o.Team != 2 || o.Removed != 1 || !slices.Equal(o.Admins, []string{"alice"}) || o.SignedIn != 2 ||
		o.Online != 0 || o.Week != 1 || !o.Reported || o.CheckHours != 24 || o.Latest != nil || o.Newer {
		t.Errorf("overview: %+v", o)
	}
	if want := []versionCount{{"1.2.0", 1, ""}, {"1.0.3", 1, "older than 1.1.0, the oldest accepted"}}; !slices.Equal(o.Versions, want) {
		t.Errorf("versions %+v, want %+v", o.Versions, want)
	}
	if len(o.Failing) != 1 || o.Failing[0].Handle != "bob" || o.Failing[0].Host != "bob-mbp" ||
		!slices.Equal(o.Failing[0].Check.Failing, []failedCheck{{"claude code", "not found"}}) {
		t.Errorf("failing: %+v", o.Failing)
	}
	_ = tm.st.Put(context.Background(), latestKey, []byte(`{"version":"1.3.0","url":"https://example.com/releases/1.3.0","checked":"`+time.Now().Format(time.RFC3339)+`"}`))
	o = overview{}
	if tm.call(t, tm.alice, http.MethodGet, "/api/admin", nil, &o); o.Latest == nil || o.Latest.Version != "1.3.0" ||
		o.Latest.URL != "https://example.com/releases/1.3.0" || time.Since(time.Time(o.Latest.Checked)) > time.Minute || !o.Newer {
		t.Errorf("the overview doesn't show the newer release: %+v", o.Latest)
	}
}

// A post from another site never reaches the admin API, even with an admin's cookie.
func TestAdminFormsFromAnotherSite(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	post := func(path string, body any, header, value string) int {
		req, _ := http.NewRequest(http.MethodPost, tm.url+path, bytes.NewReader(mustJSON(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(header, value)
		resp, err := tm.alice.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	s := tm.h.Settings()
	for _, h := range [][2]string{{"Origin", "https://evil.example"}, {"Referer", "https://evil.example/page"}} {
		if status := post("/api/admin/settings", map[string]int{"sends_per_minute": 999}, h[0], h[1]); status != http.StatusForbidden {
			t.Errorf("settings sent with %s %s: %d", h[0], h[1], status)
		}
		if status := post("/api/admin/people/promote", handle("bob"), h[0], h[1]); status != http.StatusForbidden {
			t.Errorf("promotion sent with %s %s: %d", h[0], h[1], status)
		}
	}
	if !reflect.DeepEqual(tm.h.Settings(), s) || tm.user("bob@x.com").Admin {
		t.Fatal("a post from another site changed the team")
	}
	if status := post("/api/admin/people/promote", handle("bob"), "Origin", "http://localhost"); status != http.StatusOK || !tm.user("bob@x.com").Admin {
		t.Fatalf("the hub's own page: %d", status)
	}
}

// The event log shows the newest events first, 200 at a time, with the skip of the pages around, and times in UTC.
func TestAdminEventLog(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	for i := range 250 {
		_ = tm.st.AddEvent(context.Background(), store.Event{At: time.Now(), Kind: "test", Subject: fmt.Sprintf("e%03d", i)})
	}
	type page struct {
		Events []eventView `json:"events"`
		Skip   int         `json:"skip"`
		Newer  *int        `json:"newer"`
		Older  *int        `json:"older"`
	}
	var first, second page
	tm.call(t, tm.alice, http.MethodGet, "/api/admin/events", nil, &first)
	if len(first.Events) != 200 || first.Events[0].Subject != "e249" || first.Events[199].Subject != "e050" || first.Skip != 0 ||
		first.Newer != nil || first.Older == nil || *first.Older != 200 {
		t.Fatalf("the first page isn't the 200 newest events, newest first, with the way to older ones: %d events, skip %d", len(first.Events), first.Skip)
	}
	tm.call(t, tm.alice, http.MethodGet, "/api/admin/events?skip=200", nil, &second)
	if len(second.Events) != 52 || second.Events[0].Subject != "e049" || second.Events[49].Subject != "e000" ||
		second.Events[50].Kind != "signed in" || second.Skip != 200 || second.Older != nil || second.Newer == nil || *second.Newer != 0 {
		t.Fatalf("the second page isn't the older events: %+v", second)
	}
	_, body := tm.get(t, tm.alice, "/api/admin/events?skip=249")
	if !regexp.MustCompile(`"at":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ"`).MatchString(body) {
		t.Fatalf("times aren't RFC 3339 in UTC: %s", body)
	}
}

// The settings never show GitHub's secret and token: left out or empty they stay, a new value replaces them, and
// forgetting clears them. The chat and the agreements are saved as typed.
func TestAdminSecretsAndChat(t *testing.T) {
	tm := newTeam(t, Config{Seed: seed(func(s *Settings) { s.GitHubWebhookSecret, s.GitHubToken = "old-secret", "old-token" })}, nil)
	var view settingsView
	if _, body := tm.get(t, tm.alice, "/api/admin/settings"); strings.Contains(body, "old-") {
		t.Fatal("the settings show a stored secret")
	}
	if tm.call(t, tm.alice, http.MethodGet, "/api/admin/settings", nil, &view); !view.GitHubSecretSet || !view.GitHubTokenSet {
		t.Fatalf("the settings don't say the secret and token are set: %+v", view)
	}
	var saved struct {
		OK bool `json:"ok"`
		settingsView
	}
	status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{"chat_webhook": " https://chat.example/hook ",
		"chat_kind": "slack", "agreements": "Message the agent changing a file before you change it.\r\nAsk a person only for decisions.\n",
		"github_secret": "", "github_token": "  "}, &saved)
	s := tm.h.Settings()
	if status != http.StatusOK || s.GitHubWebhookSecret != "old-secret" || s.GitHubToken != "old-token" || s.ChatWebhook != "https://chat.example/hook" ||
		s.ChatKind != "slack" || s.Agreements != "Message the agent changing a file before you change it.\nAsk a person only for decisions." {
		t.Fatalf("kept: %d %+v", status, s)
	}
	if !saved.GitHubSecretSet || saved.Values.ChatKind != "slack" {
		t.Fatalf("answer: %+v", saved)
	}
	tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{"github_token": "new-token", "forget_github_secret": true}, &saved)
	if s := tm.h.Settings(); s.GitHubWebhookSecret != "" || s.GitHubToken != "new-token" || saved.GitHubSecretSet || !saved.GitHubTokenSet {
		t.Fatalf("changed: %+v, answered %+v", s, saved)
	}
	s = tm.h.Settings()
	if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]string{"chat_webhook": "http://chat.example/hook"}, nil); status != http.StatusBadRequest ||
		p.Field != "chat_webhook" || p.Error != "The chat webhook must be an https:// address." || !reflect.DeepEqual(tm.h.Settings(), s) {
		t.Fatalf("a webhook over plain http: %d %+v", status, p)
	}
}

// Anyone can revoke their own laptops from the front page, and nobody else's from there.
func TestRevokeOwnLaptop(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	if _, home := tm.get(t, tm.bob, "/api/home"); !strings.Contains(home, `"id":"d-bob"`) || !strings.Contains(home, `"host":"bob-mbp"`) ||
		strings.Contains(home, "d-alice") {
		t.Fatalf("bob's laptops: %s", home)
	}
	revoke := func(c *http.Client, id string) int {
		req, _ := http.NewRequest(http.MethodPost, tm.url+"/api/devices/revoke", strings.NewReader(`{"id":"`+id+`"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := revoke(tm.alice, "d-bob"); status != http.StatusNotFound {
		t.Fatalf("alice revoked bob's laptop from her front page: %d", status)
	}
	if status := revoke(tm.bob, "d-bob"); status != http.StatusOK {
		t.Fatalf("revoke: %d", status)
	}
	tm.h.mu.Lock()
	revoked := tm.h.devices["d-bob"].Revoked
	tm.h.mu.Unlock()
	if !revoked {
		t.Fatal("bob's laptop wasn't revoked")
	}
}

// A notice is the notice page with its title and sentence written in, escaped, under the status it is for.
func TestNoticePage(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	status, page := tm.get(t, tm.alice, "/auth/callback?state=<b>forged</b>")
	if status != http.StatusBadRequest || !strings.Contains(page, `<meta name="murmur-title" content="Sign-in expired">`) ||
		!strings.Contains(page, `name="murmur-text" content="This sign-in link expired.`) || strings.Contains(page, "<b>forged") {
		t.Fatalf("%d:\n%s", status, page)
	}
}

// The panel's pages are for signed-in people: anyone else signs in first. What they show comes from the API, for
// admins only.
func TestAdminPages(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	visitor, _ := signedIn(t, tm.url, "")
	for _, p := range []string{"/admin", "/admin/people", "/admin/laptops", "/admin/settings", "/admin/events"} {
		if status, page := tm.get(t, tm.alice, p); status != http.StatusOK || !strings.Contains(page, "· murmur</title>") {
			t.Errorf("%s for an admin: %d", p, status)
		}
		resp, err := visitor.Get(tm.url + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
			t.Errorf("%s signed out: %d", p, resp.StatusCode)
		}
	}
}
