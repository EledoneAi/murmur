package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/auth"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// Edge cases of the hub's web side: who may call what, sign-ins by provider, by email and by laptop, posts from
// other sites, the settings admins can save, and what laptops can make the team page show.

// cookieFor is a session cookie for email, signed by h for purpose and good for ttl.
func cookieFor(h *Hub, purpose, email string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: h.signedExp(purpose, email, ttl)}
}

// knock sends one request as a page's script would, with a cookie, a laptop's token or neither, and returns the
// status.
func knock(t *testing.T, hub, method, path string, cookie *http.Cookie, token string) int {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	req, _ := http.NewRequestWithContext(ctx, method, hub+path, body)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Each address takes the credential it is for: the pages' API a browser's session, never a laptop's token, and the
// laptops' addresses a token, never a session. A session past its time, signed for another purpose or with another
// key, or for someone the hub doesn't know, gets nowhere; nor does a revoked laptop.
func TestEachAddressTakesItsOwnCredential(t *testing.T) {
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		_ = st.PutDevice(ctx, store.Device{ID: "d-old", Email: "alice@x.com", TokenHash: hashToken("tok-old"), Created: time.Now(), Revoked: true})
	})
	elsewhere := &Hub{cfg: Config{Secret: []byte("another-hub-secret")}}
	session := cookieFor(tm.h, "session", "alice@x.com", time.Hour)
	bad := map[string]*http.Cookie{
		"no session":                 nil,
		"an expired session":         cookieFor(tm.h, "session", "alice@x.com", -time.Minute),
		"a sign-in state":            cookieFor(tm.h, "state", "alice@x.com", time.Hour),
		"another hub's session":      cookieFor(elsewhere, "session", "alice@x.com", time.Hour),
		"a session for someone else": cookieFor(tm.h, "session", "mallory@x.com", time.Hour),
		"a forged session":           {Name: cookieName, Value: base64.RawURLEncoding.EncodeToString([]byte("session|alice@x.com|9999999999")) + ".AAAA"},
	}
	pages := [][2]string{
		{"GET", "/api/me"}, {"GET", "/api/home"}, {"GET", "/api/team"}, {"GET", "/team/events"},
		{"POST", "/api/devices/revoke"}, {"POST", "/api/cli/login"}, {"POST", "/team/send"},
		{"GET", "/api/admin"}, {"GET", "/api/admin/people"}, {"GET", "/api/admin/laptops"}, {"GET", "/api/admin/settings"},
		{"GET", "/api/admin/events"}, {"POST", "/api/admin/people/join"}, {"POST", "/api/admin/people/promote"},
		{"POST", "/api/admin/people/demote"}, {"POST", "/api/admin/people/remove"}, {"POST", "/api/admin/people/restore"},
		{"POST", "/api/admin/laptops/revoke"}, {"POST", "/api/admin/settings"},
	}
	for _, p := range pages {
		for what, c := range bad {
			if got := knock(t, tm.url, p[0], p[1], c, ""); got != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d", p[0], p[1], what, got)
			}
		}
		// alice is an admin, and her laptop's token is no session
		if got := knock(t, tm.url, p[0], p[1], nil, "tok-alice"); got != http.StatusUnauthorized {
			t.Errorf("%s %s with a laptop's token: %d", p[0], p[1], got)
		}
		if got := knock(t, tm.url, p[0], p[1], session, ""); got == http.StatusUnauthorized || got == http.StatusForbidden {
			t.Errorf("%s %s with alice's session: %d", p[0], p[1], got)
		}
	}
	for _, p := range [][2]string{{"POST", "/api/revoke"}, {"GET", "/ws"}, {"GET", "/team/prs"}} {
		if got := knock(t, tm.url, p[0], p[1], nil, "tok-old"); got != http.StatusUnauthorized {
			t.Errorf("%s %s with a revoked laptop: %d", p[0], p[1], got)
		}
		if p[1] != "/team/prs" { // which a page reads too
			if got := knock(t, tm.url, p[0], p[1], session, ""); got != http.StatusUnauthorized {
				t.Errorf("%s %s with a browser's session: %d", p[0], p[1], got)
			}
		}
	}
	tm.h.mu.Lock()
	revoked := tm.h.devices["d-alice"].Revoked
	tm.h.mu.Unlock()
	if revoked {
		t.Fatal("a request without alice's laptop token revoked it")
	}
}

// DEV_LOGIN signs people in by email only on a hub without an identity provider. With one, the team's list applies
// to everyone the provider signs in: a hub left with DEV_LOGIN=1 must not let in, as its admin, the first stranger
// with an account at the provider.
func TestDevLoginIsOffWithAProvider(t *testing.T) {
	idp := &fakeIdP{who: auth.Identity{Email: "eve@anywhere.example", Verified: true}}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), DevLogin: true, PublicURL: "http://localhost", Auth: idp, Seed: Defaults()},
		store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	resp, err = c.Get(srv.URL + "/auth/callback?" + url.Values{"state": {u.Query().Get("state")}, "code": {"good"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.mu.Lock()
	eve := h.users["eve@anywhere.example"]
	h.mu.Unlock()
	if resp.StatusCode != http.StatusForbidden || eve != nil {
		t.Fatalf("someone on no list signed in through the provider: %d, %+v", resp.StatusCode, eve)
	}
	if resp, err = c.PostForm(srv.URL+"/dev-login", url.Values{"email": {"eve@anywhere.example"}}); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sign-in by email next to a provider: %d", resp.StatusCode)
	}
}

// A hub with dev login serves the computer it runs on: it takes sign-ins by email from a browser that reached it as
// localhost or a loopback address only, not under another name, as a page rebinding its own name to 127.0.0.1 would.
func TestDevLoginServesThisComputerOnly(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost": true, "http://localhost:8080": true, "http://127.0.0.1:8080": true, "http://127.9.9.9": true,
		"http://[::1]:8080": true, "https://localhost": false, "http://localhost.evil.example": false,
		"http://127.0.0.1.nip.io": false, "http://0.0.0.0:8080": false, "http://[::]:8080": false,
		"http://localhost@evil.example": false, "http://evil.example#@localhost": false, "http://2130706433": false,
		"http://192.168.1.20:8080": false, "localhost:8080": false, "": false,
	} {
		if got := Local(raw); got != want {
			t.Errorf("Local(%q) = %v", raw, got)
		}
	}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), DevLogin: true, PublicURL: "http://localhost:8080"}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	for host, want := range map[string]bool{"localhost:8080": true, "127.0.0.1": true, "[::1]:8080": true, "evil.example": false,
		"localhost.evil.example": false, "127.0.0.1.nip.io:8080": false, "localhost@evil.example": false} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/dev-login", strings.NewReader("email=ann%40x.com"))
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if in := resp.StatusCode == http.StatusFound && len(resp.Cookies()) > 0; in != want {
			t.Errorf("dev login reached as %s: %d", host, resp.StatusCode)
		}
	}
}

// The team's lists match whole, verified addresses and whole domains, ignoring case only. Lookalikes, subdomains,
// a trailing dot or a second @ are other addresses.
func TestWhoMayJoinMatchesWholeAddresses(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "https://hub.example", Admins: []string{"boss@else.org"},
		Seed: seed(func(s *Settings) { s.AllowedEmails, s.AllowedOrgs = []string{"dan@gmail.com"}, []string{"acme"} })}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	verified := func(email string) auth.Identity { return auth.Identity{Email: email, Verified: true} }
	for _, c := range []struct {
		id   auth.Identity
		want bool
	}{
		{verified("Ann@X.com"), true},
		{verified("ann+murmur@x.com"), true},
		{verified("DAN@gmail.com"), true},
		{verified("boss@else.org"), true},
		{auth.Identity{Email: "gus@else.org", Verified: true, Orgs: []string{"ACME"}}, true},
		{verified("ann@x.com."), false},
		{verified("ann@sub.x.com"), false},
		{verified("ann@x.com.evil.example"), false},
		{verified("ann@x.com@evil.example"), false},
		{verified("ann@evil.example@x.com"), false},
		{verified("ann@х.com"), false}, // a Cyrillic х
		{verified("dan+x@gmail.com"), false},
		{verified("dan@gmail.com.evil.example"), false},
		{verified("x.com"), false},
		{verified(""), false},
		{auth.Identity{Email: "boss@else.org"}, false},
		{auth.Identity{Email: "ann@x.com"}, false},
		{auth.Identity{Email: "gus@else.org", Verified: true, Orgs: []string{"acme-labs", "not-acme"}}, false},
		{auth.Identity{Email: "ann@x.com", Verified: true, Workspace: true, Domain: "y.org"}, false},
	} {
		if err := h.admitted(c.id); (err == nil) != c.want {
			t.Errorf("%+v: %v", c.id, err)
		}
	}
}

// An admin who empties every list keeps the admins in: the panel's admins can still sign in, and nobody else.
func TestEmptiedListsKeepTheAdmins(t *testing.T) {
	st := store.NewMemory()
	_ = st.PutUser(context.Background(), store.User{Email: "alice@x.com", Handle: "alice", Admin: true})
	_ = st.PutUser(context.Background(), store.User{Email: "bob@x.com", Handle: "bob"})
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "https://hub.example"}, st)
	if err != nil {
		t.Fatal(err)
	}
	s := h.Settings()
	s.AllowedDomains, s.AllowedEmails, s.AllowedOrgs = nil, nil, nil
	if err := h.SaveSettings(context.Background(), s, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := h.admitted(auth.Identity{Email: "alice@x.com", Verified: true}); err != nil {
		t.Fatalf("the admin can't sign in: %v", err)
	}
	if h.admitted(auth.Identity{Email: "bob@x.com", Verified: true}) == nil {
		t.Fatal("bob signs in with no list naming him")
	}
}

// Handles are lower-case letters, digits and inner dashes, each person's own, and never murmur, whatever the email.
func TestHandlesFromOddEmails(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	valid := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	seen := map[string]string{}
	for _, email := range []string{"murmur@x.com", "murmur@y.org", "murmur.bot@x.com", "-murmur-@x.com", "mur.mur@x.com",
		"../x@x.com", "x@x.com", "a/b@x.com", "日本@x.com", "中文@x.com", "--@x.com", "admin@x.com", "o'neil@x.com", "@x.com"} {
		u, err := h.ensureUser(context.Background(), email, "")
		if err != nil {
			t.Fatal(err)
		}
		if !valid.MatchString(u.Handle) || u.Handle == protocol.System || seen[u.Handle] != "" {
			t.Errorf("%s got the handle %q (also %q's)", email, u.Handle, seen[u.Handle])
		}
		seen[u.Handle] = email
	}
}

// A laptop's sign-in code works once, for the laptop that holds its verifier, until it expires; the browser is only
// ever sent back to 127.0.0.1, on a port that isn't a system one.
func TestLaptopSignInCodes(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	const verifier, state = "the-verifier-only-this-laptop-holds-0123456789", "state-0123456789abcdef"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	start := func(port, challenge, state string) (int, map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"port": port, "challenge": challenge, "state": state, "host": "ann-mbp"})
		resp, err := tm.alice.Post(tm.url+"/api/cli/login", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	trade := func(code, verifier string) (int, map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
		resp, err := http.Post(tm.url+"/cli/token", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	status, out := start("5555", challenge, state)
	if back := regexp.MustCompile(`^http://127\.0\.0\.1:5555/callback\?code=[0-9a-f]{32}&state=` + state + `$`); status != http.StatusOK || !back.MatchString(out["redirect"]) {
		t.Fatalf("the browser goes back to %q (%d)", out["redirect"], status)
	}
	for _, port := range []string{"80", "1023", "65536", "-1", "0x15b3", " 5555", "5555 ", "5555/x", "localhost.evil.example:5555", "5555@evil.example", "1e4", ""} {
		if status, out := start(port, challenge, state); status != http.StatusBadRequest || out["redirect"] != "" || out["code"] != "" {
			t.Errorf("port %q: %d %v", port, status, out)
		}
	}
	for _, c := range [][2]string{{"short", state}, {challenge[:42] + "=", state}, {challenge[:42] + "/", state}, {challenge, "short"},
		{challenge, "state with spaces 0123"}, {challenge, strings.Repeat("s", 65)}} {
		if status, _ := start("5555", c[0], c[1]); status != http.StatusBadRequest {
			t.Errorf("challenge %q, state %q: %d", c[0], c[1], status)
		}
	}

	// another laptop's verifier spends the code: it then fails for its own laptop too
	back, _ := url.Parse(out["redirect"])
	code := back.Query().Get("code")
	if status, _ := trade(code, "another-laptops-verifier-0123456789abcdefghij"); status != http.StatusBadRequest {
		t.Fatalf("another verifier: %d", status)
	}
	if status, _ := trade(code, verifier); status != http.StatusBadRequest {
		t.Fatalf("a code spent by another laptop worked: %d", status)
	}
	// a code works once, for the person who said yes in the browser
	_, shown := start("0", challenge, state)
	if status, got := trade(shown["code"], verifier); status != http.StatusOK || got["email"] != "alice@x.com" || got["token"] == "" {
		t.Fatalf("the first trade: %d %v", status, got)
	}
	if status, _ := trade(shown["code"], verifier); status != http.StatusBadRequest {
		t.Fatalf("a code worked twice: %d", status)
	}
	for _, c := range [][2]string{{"", verifier}, {"", ""}, {"nonexistent", verifier}} {
		if status, _ := trade(c[0], c[1]); status != http.StatusBadRequest {
			t.Errorf("code %q: %d", c[0], status)
		}
	}
	// codes expire
	ctx := context.Background()
	_ = tm.st.PutCode(ctx, store.Code{ID: "expired", Email: "alice@x.com", Challenge: challenge, Expires: time.Now().Add(-time.Millisecond)})
	_ = tm.st.PutCode(ctx, store.Code{ID: "fresh", Email: "alice@x.com", Challenge: challenge, Expires: time.Now().Add(time.Minute)})
	if status, _ := trade("expired", verifier); status != http.StatusBadRequest {
		t.Fatalf("an expired code: %d", status)
	}
	if status, _ := trade("fresh", verifier); status != http.StatusOK {
		t.Fatalf("a code before it expires: %d", status)
	}
	// a code for someone removed since the browser said yes
	_ = tm.st.PutCode(ctx, store.Code{ID: "bobs", Email: "bob@x.com", Challenge: challenge, Expires: time.Now().Add(time.Minute)})
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/people/remove", handle("bob"), nil); status != http.StatusOK {
		t.Fatal(status)
	}
	if status, _ := trade("bobs", verifier); status != http.StatusForbidden {
		t.Fatalf("a removed person's code: %d", status)
	}
}

// Whatever page a sign-in was asked to come back to, it lands on this hub.
func TestSignInComesBackToThisHub(t *testing.T) {
	idp := &fakeIdP{who: auth.Identity{Email: "ann@x.com", Verified: true}}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "http://hub", Auth: idp}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	for next, want := range map[string]string{
		"/team": "/team", "/cli/login?port=5555&state=s": "/cli/login?port=5555&state=s", "https://evil.example/": "/",
		"//evil.example/": "/", "/\\evil.example": "/", "/\t/evil.example": "/", "/\r\n/evil.example": "/", "javascript:alert(1)": "/",
		"/.//evil.example": "/evil.example", "/%2F%2Fevil.example": "/%2F%2Fevil.example", "evil.example": "/", " /team": "/",
		"http:/evil.example": "/", "/@evil.example": "/@evil.example",
	} {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(srv.URL + "/login?next=" + url.QueryEscape(next))
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(resp.Header.Get("Location"))
		resp, err = c.Get(srv.URL + "/auth/callback?" + url.Values{"state": {u.Query().Get("state")}, "code": {"good"}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || got != want {
			t.Errorf("next %q: %d to %q, want %q", next, resp.StatusCode, got, want)
		}
	}
}

// Every post the pages make is refused from another site, whatever the browser says about where it comes from, and
// changes nothing.
func TestPagePostsFromAnotherSite(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	bob := connect(t, tm.url, "tok-bob", protocol.Agent{Name: "b1", Status: protocol.StatusIdle})
	const form = "application/x-www-form-urlencoded"
	posts := [][3]string{
		{"/team/send", form, "to=bob%2Fb1&text=hi"},
		{"/logout", form, ""},
		{"/dev-login", form, "email=eve%40x.com"},
		{"/api/devices/revoke", "text/plain", `{"id":"d-alice"}`},
		{"/api/cli/login", "text/plain", `{"port":"5555","challenge":"` + strings.Repeat("c", 43) + `","state":"state-0123456789abcdef"}`},
		{"/api/admin/people/promote", "application/json", `{"handle":"bob"}`},
		{"/api/admin/settings", "application/json", `{"sends_per_minute":999}`},
	}
	s := tm.h.Settings()
	for _, from := range [][2]string{{"Origin", "https://evil.example"}, {"Origin", "null"}, {"Origin", "http://localhost:8080"},
		{"Origin", "https://localhost"}, {"Origin", "http://localhost.evil.example"}, {"Referer", "https://evil.example/page"},
		{"Referer", "http://localhost.evil.example/"}} {
		for _, p := range posts {
			req, _ := http.NewRequest(http.MethodPost, tm.url+p[0], strings.NewReader(p[2]))
			req.Header.Set("Content-Type", p[1])
			req.Header.Set("Accept", "application/json")
			req.Header.Set(from[0], from[1])
			resp, err := tm.alice.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s with %s %s: %d", p[0], from[0], from[1], resp.StatusCode)
			}
		}
	}
	tm.h.mu.Lock()
	revoked := tm.h.devices["d-alice"].Revoked
	tm.h.mu.Unlock()
	if revoked || tm.user("bob@x.com").Admin || !reflect.DeepEqual(tm.h.Settings(), s) {
		t.Fatal("a post from another site changed the team")
	}
	if status, _ := tm.get(t, tm.alice, "/api/me"); status != http.StatusOK {
		t.Fatal("a post from another site signed alice out")
	}
	bob.quiet(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg })
}

// Behind https, every cookie the hub sets is Secure and HttpOnly, and the sign-in and session cookies are
// SameSite=Lax, whatever the request in front of the proxy looked like.
func TestCookieFlags(t *testing.T) {
	idp := &fakeIdP{who: auth.Identity{Email: "ann@x.com", Verified: true}}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "https://hub.example", Auth: idp}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler()) // plain http, as behind a proxy
	defer srv.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	one := func(resp *http.Response) *http.Cookie {
		t.Helper()
		resp.Body.Close()
		if cs := resp.Cookies(); len(cs) == 1 {
			return cs[0]
		}
		t.Fatalf("cookies: %v", resp.Cookies())
		return nil
	}
	resp, err := c.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	n := one(resp)
	if n.Name != "sy_n" || !n.Secure || !n.HttpOnly || n.SameSite != http.SameSiteLaxMode || n.Path != "/auth" || n.MaxAge != 600 {
		t.Errorf("sign-in cookie: %+v", n)
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?"+url.Values{"state": {u.Query().Get("state")}, "code": {"good"}}.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: "sy_n", Value: n.Value})
	if resp, err = c.Do(req); err != nil {
		t.Fatal(err)
	}
	s := one(resp)
	if s.Name != cookieName || !s.Secure || !s.HttpOnly || s.SameSite != http.SameSiteLaxMode || s.Path != "/" {
		t.Errorf("session cookie: %+v", s)
	}
	if resp, err = c.Post(srv.URL+"/logout", "application/x-www-form-urlencoded", nil); err != nil {
		t.Fatal(err)
	}
	if out := one(resp); out.Name != cookieName || out.MaxAge >= 0 || !out.Secure || !out.HttpOnly {
		t.Errorf("sign-out cookie: %+v", out)
	}
}

// Browsers send their origin in lower case, without the scheme's own port: a PUBLIC_URL written otherwise still takes
// the posts of the hub's own pages, and only theirs.
func TestOriginAsBrowsersWriteIt(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "http://LocalHost:80"}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	for origin, want := range map[string]int{"http://localhost": http.StatusFound, "http://localhost:8080": http.StatusForbidden,
		"https://localhost": http.StatusForbidden, "http://evil.example": http.StatusForbidden} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/logout", nil)
		req.Header.Set("Origin", origin)
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("a post from %s: %d, want %d", origin, resp.StatusCode, want)
		}
	}
}

// Every post is read up to 256 KB and GitHub's events up to 2 MB: anything larger is refused and does nothing.
func TestPostsAreCapped(t *testing.T) {
	tm := newTeam(t, Config{Seed: seed(func(s *Settings) { s.GitHubWebhookSecret = "s3cret" })}, nil)
	alice := connect(t, tm.url, "tok-alice", protocol.Agent{Name: "a1", Status: protocol.StatusIdle})
	pad := strings.Repeat("a", 300<<10)
	if status, out := sendFrom(t, tm.bob, tm.url, "alice/a1", pad); status == http.StatusOK || out["ok"] == true {
		t.Errorf("300 KB from the team page: %d %v", status, out)
	}
	for path, body := range map[string]string{
		"/api/devices/revoke": `{"id":"d-bob","pad":"` + pad + `"}`,
		"/cli/token":          `{"code":"x","verifier":"` + pad + `"}`,
		"/api/cli/login":      `{"port":"0","host":"` + pad + `"}`,
	} {
		resp, err := tm.bob.Post(tm.url+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("300 KB to %s: %d", path, resp.StatusCode)
		}
	}
	big := []byte(`{"action":"closed","pad":"` + strings.Repeat("a", 3<<20) + `"}`)
	if got := deliver(t, tm.url, "pull_request", sign("s3cret", big), big); got != http.StatusRequestEntityTooLarge {
		t.Errorf("a 3 MB event: %d", got)
	}
	tm.h.mu.Lock()
	revoked := tm.h.devices["d-bob"].Revoked
	tm.h.mu.Unlock()
	if revoked {
		t.Fatal("a post over the cap revoked a laptop")
	}
	alice.quiet(func(f protocol.Frame) bool { return f.T == protocol.FrameMsg })
}

// The hub sends its GitHub token only over https to GitHub's API hosts, whatever address a signed event names.
func TestMergedFilesStayOnGitHub(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { asked.Add(1); _, _ = io.WriteString(w, "[]") }))
	defer srv.Close()
	old := githubClient
	githubClient = srv.Client()
	t.Cleanup(func() { githubClient = old })
	host := strings.TrimPrefix(srv.URL, "https://")
	for _, repo := range []string{"https://" + host + "/repos/acme/api", "http://api.github.com/repos/acme/api",
		"https://api.github.com.evil.example/repos/acme/api", "https://api.github.com@" + host + "/repos/acme/api",
		"https://api.github.com:443/repos/acme/api", "//api.github.com/repos/acme/api", "api.github.com/repos/acme/api", ""} {
		var ev prEvent
		ev.Repo.URL, ev.PR.Number = repo, 7
		if _, err := mergedFiles(context.Background(), ev, "ghtok", []string{"api.github.com"}); err == nil {
			t.Errorf("the token went to %q", repo)
		}
	}
	var ev prEvent
	ev.Repo.URL, ev.PR.Number = "https://"+host+"/repos/acme/api", 0
	if _, err := mergedFiles(context.Background(), ev, "ghtok", []string{host}); err == nil {
		t.Error("a pull request without a number")
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("%d requests left the hub", n)
	}
}

// Settings out of bounds are refused, naming the setting, and leave the team as it was.
func TestSettingsOutOfBounds(t *testing.T) {
	tm := newTeam(t, Config{Version: "2.0.3"}, nil)
	before := tm.h.Settings()
	for _, c := range []struct {
		field string
		value any
	}{
		{"token_days", -1}, {"sends_per_minute", -5}, {"loop_max", 0}, {"loop_minutes", 0}, {"max_agents", 0}, {"max_laptops", 0},
		{"max_laptops", 1 << 40}, {"downloads_per_hour", 0}, {"check_hours", -1}, {"wait_ping_minutes", -1}, {"work_start", -1},
		{"work_end", 25}, {"announce_hours", 0}, {"token_days", json.Number("99999999999999999999")}, {"loop_max", json.Number("1e30")},
		{"min_version", "1.4"}, {"min_version", "1.4.0-rc.1"}, {"min_version", "1.4.0.0"}, {"min_version", "v"},
		{"min_version", "20.0.3"}, {"min_version", "v2.0.4"}, {"min_version", "2.1.0"},
		{"ignored_paths", "["}, {"ignored_paths", "ok/**\n\\"}, {"ignored_paths", "a/[/**"},
		{"time_zone", "../../../etc/passwd"}, {"chat_webhook", "javascript:alert(1)"}, {"chat_webhook", "http://169.254.169.254/"},
		{"chat_kind", "Slack"}, {"update_policy", ""}, {"update_policy", "AUTO"},
	} {
		status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{c.field: c.value}, nil)
		if status != http.StatusBadRequest || p.Field != c.field {
			t.Errorf("%s=%v: %d %+v", c.field, c.value, status, p)
		}
		if !reflect.DeepEqual(tm.h.Settings(), before) {
			t.Fatalf("%s=%v changed the settings", c.field, c.value)
		}
	}
	for _, c := range []struct {
		field string
		value any
		check func(Settings) bool
	}{
		{"token_days", 0, func(s Settings) bool { return s.TokenDays == 0 }},
		{"agreements", "", func(s Settings) bool { return s.Agreements == "" }},
		{"min_version", "v2.0.3", func(s Settings) bool { return s.MinVersion == "2.0.3" }},
		{"min_version", " 1.9.9 ", func(s Settings) bool { return s.MinVersion == "1.9.9" }},
		{"min_version", "", func(s Settings) bool { return s.MinVersion == "" }},
	} {
		if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{c.field: c.value}, nil); status != http.StatusOK ||
			!c.check(tm.h.Settings()) {
			t.Errorf("%s=%v: %d %+v", c.field, c.value, status, p)
		}
	}
}

// The oldest accepted version can't be newer than the hub: laptops update to the hub's own version, so none could
// connect until an admin noticed. A hub rolled back below the version stored keeps taking its other settings.
func TestOldestVersionNotPastTheHub(t *testing.T) {
	tm := newTeam(t, Config{Version: "2.0.3", Seed: seed(func(s *Settings) { s.MinVersion = "2.1.0" })}, nil)
	if status, p := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{"min_version": "20.0.3"}, nil); status != http.StatusBadRequest ||
		p.Field != "min_version" || !strings.Contains(p.Error, "2.0.3") || tm.h.Settings().MinVersion != "2.1.0" {
		t.Fatalf("an oldest version past the hub's: %d %+v", status, p)
	}
	if status, _ := tm.call(t, tm.alice, http.MethodPost, "/api/admin/settings", map[string]any{"sends_per_minute": 30}, nil); status != http.StatusOK ||
		tm.h.Settings().SendsPerMinute != 30 {
		t.Fatalf("another setting, on a hub rolled back: %d", status)
	}
	dev := newTeam(t, Config{Version: "dev"}, nil) // a build without a version can't tell
	if status, _ := dev.call(t, dev.alice, http.MethodPost, "/api/admin/settings", map[string]any{"min_version": "20.0.3"}, nil); status != http.StatusOK {
		t.Fatalf("a dev hub: %d", status)
	}
}

// The team page tells people's agents, pull requests and announcements apart by their names, and stops drawing on
// two of the same (Svelte's keyed lists throw). The hub sends each name once: a person's two laptops can both run
// b1, two sessions can share a pull request, one knowing its link and the other not, and two announcements to a
// repo can come in the same millisecond.
func TestTeamPageNamesEachOnce(t *testing.T) {
	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	tm := newTeam(t, Config{}, func(ctx context.Context, st store.Store) {
		_ = st.PutDevice(ctx, store.Device{ID: "d-bob2", Email: "bob@x.com", TokenHash: hashToken("tok-bob2"), Created: time.Now()})
		_ = st.Put(ctx, newsKey, mustJSON([]announcement{
			{ID: "n1", From: "alice", Repo: "org/app", Text: "main is frozen", At: at},
			{ID: "n2", From: "bob", Repo: "org/app", Text: "staging is down", At: at},
			{ID: "n3", From: "bob", Repo: "org/web", Text: "deploys are paused", At: at},
		}))
	})
	connect(t, tm.url, "tok-bob", protocol.Agent{Name: "b1", Repo: "org/app", PR: 5, PRURL: "https://github.com/org/app/pull/5",
		Status: protocol.StatusBusy})
	connect(t, tm.url, "tok-bob2", protocol.Agent{Name: "b1", Repo: "org/app", PR: 5, Status: protocol.StatusWaiting, Needs: "permission"})
	connect(t, tm.url, "tok-alice", protocol.Agent{Name: "a1", Repo: "org/app", PR: 5, Status: protocol.StatusIdle})
	check := func(v teamView) {
		t.Helper()
		keys := map[string]bool{}
		once := func(k string) {
			t.Helper()
			if keys[k] {
				t.Errorf("the page gets %s twice", k)
			}
			keys[k] = true
		}
		for _, p := range v.People {
			once("person " + p.Handle)
			for _, a := range p.Agents {
				once("agent " + p.Handle + "/" + a.Name)
			}
		}
		for _, pr := range v.PRs {
			once("pull request " + pr.Name)
		}
		for _, n := range v.News {
			once(fmt.Sprintf("announcement %d%s", n.At, n.Repo))
		}
		if len(v.News) != 3 {
			t.Errorf("announcements: %+v", v.News)
		}
	}
	s := openStream(t, tm.bob, tm.url)
	v, _ := s.expect(func(v teamView) bool {
		i := slices.IndexFunc(v.People, func(p protocol.Person) bool { return p.Handle == "alice" })
		return i >= 0 && len(v.People[i].Agents) == 1 && slices.ContainsFunc(v.People, func(p protocol.Person) bool {
			return p.Handle == "bob" && slices.ContainsFunc(p.Agents, func(a protocol.Agent) bool { return a.Needs != "" })
		})
	})
	check(v)
	if a, _ := agentIn(v, "bob/b1"); a.Needs != "permission" {
		t.Errorf("the page shows the b1 that doesn't wait over the one that does: %+v", a)
	}
	_, body := tm.get(t, tm.bob, "/api/team")
	v = teamView{}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	check(v)
}

// The install script and the front page name the hub by its PUBLIC_URL, never by what a request says it reached.
func TestAddressesComeFromPublicURL(t *testing.T) {
	tm := newTeam(t, Config{}, nil)
	for path, c := range map[string]*http.Cookie{"/install.sh": nil, "/api/home": cookieFor(tm.h, "session", "bob@x.com", time.Hour)} {
		req, _ := http.NewRequest(http.MethodGet, tm.url+path, nil)
		if c != nil {
			req.AddCookie(c)
		}
		req.Host = "evil.example"
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Forwarded", "host=evil.example;proto=https")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "evil") || !strings.Contains(string(b), "http://localhost") {
			t.Errorf("%s: %s", path, b)
		}
	}
}
