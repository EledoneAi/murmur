package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"

	"github.com/EledoneAi/murmur/internal/auth"
	"github.com/EledoneAi/murmur/internal/store"
)

// fakeIdP signs in whoever it's told to, once the nonce matches the one sent with the sign-in.
type fakeIdP struct {
	who   auth.Identity
	nonce string
}

func (p *fakeIdP) Name() string { return "Fake" }

func (p *fakeIdP) AuthURL(state, nonce string) string {
	p.nonce = nonce
	return "https://idp.example/auth?" + url.Values{"state": {state}}.Encode()
}

func (p *fakeIdP) Exchange(_ context.Context, code, nonce string) (auth.Identity, error) {
	if code != "good" || nonce != p.nonce {
		return auth.Identity{}, errors.New("bad code or nonce")
	}
	return p.who, nil
}

func TestProviderSignIn(t *testing.T) {
	idp := &fakeIdP{}
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "http://hub", Auth: idp, Admins: []string{"boss@else.org"},
		Seed: seed(func(s *Settings) { s.AllowedOrgs = []string{"acme"} })}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	browser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	start := func(c *http.Client) string {
		resp, err := c.Get(srv.URL + "/login")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(resp.Header.Get("Location"))
		return u.Query().Get("state")
	}
	finish := func(c *http.Client, state string) int {
		resp, err := c.Get(srv.URL + "/auth/callback?" + url.Values{"state": {state}, "code": {"good"}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	for _, c := range []struct {
		name string
		who  auth.Identity
		want int
	}{
		{"team domain", auth.Identity{Email: "ann@x.com", Verified: true}, http.StatusFound},
		{"unverified", auth.Identity{Email: "eve@x.com"}, http.StatusForbidden},
		{"other domain", auth.Identity{Email: "eve@else.org", Verified: true}, http.StatusForbidden},
		{"GitHub organisation", auth.Identity{Email: "gus@else.org", Verified: true, Orgs: []string{"ACME"}}, http.StatusFound},
		{"first admin", auth.Identity{Email: "boss@else.org", Verified: true}, http.StatusFound},
	} {
		idp.who = c.who
		b := browser()
		if got := finish(b, start(b)); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	h.mu.Lock()
	boss := h.users["boss@else.org"]
	h.mu.Unlock()
	if boss == nil || !boss.Admin {
		t.Fatal("ADMINS didn't make an admin")
	}

	// a state and code seen elsewhere can't finish the sign-in in another browser, even with the nonce as cookie
	idp.who = auth.Identity{Email: "ann@x.com", Verified: true}
	state := start(browser())
	thief := browser()
	u, _ := url.Parse(srv.URL + "/auth")
	thief.Jar.SetCookies(u, []*http.Cookie{{Name: "sy_n", Value: idp.nonce}})
	if got := finish(thief, state); got != http.StatusBadRequest {
		t.Fatalf("replayed sign-in: %d", got)
	}
}

// A laptop no browser can reach (over SSH) asks for port 0: the page shows the code, and only the laptop that
// holds the PKCE verifier can trade it for a token.
func TestLaptopSignInByCode(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), DevLogin: true, PublicURL: "http://localhost"}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if _, err := c.PostForm(srv.URL+"/dev-login", url.Values{"email": {"ann@x.com"}}); err != nil {
		t.Fatal(err)
	}
	verifier := "the-verifier-only-the-remote-laptop-has-0123"
	sum := sha256.Sum256([]byte(verifier))
	req, _ := json.Marshal(map[string]string{"port": "0", "host": "devbox", "state": "remote-state-0123456789",
		"challenge": base64.RawURLEncoding.EncodeToString(sum[:])})
	resp, err := c.Post(srv.URL+"/api/cli/login", "application/json", bytes.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	var shown struct{ Code, Phrase string }
	_ = json.NewDecoder(resp.Body).Decode(&shown)
	if resp.StatusCode != http.StatusOK || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(shown.Code) || shown.Phrase != Phrase("remote-state-0123456789") {
		t.Fatalf("no code shown: %d %+v", resp.StatusCode, shown)
	}
	trade := func(verifier string) int {
		b, _ := json.Marshal(map[string]string{"code": shown.Code, "verifier": verifier})
		resp, err := http.Post(srv.URL+"/cli/token", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if trade(verifier) != http.StatusOK {
		t.Fatal("the laptop that asked couldn't use its code")
	}
	if trade(verifier) == http.StatusOK {
		t.Fatal("a code worked twice")
	}
}

// With Google, a team's domain admits only accounts of that Workspace: a personal Google account whose email is at
// the company's domain is not the company's.
func TestGoogleDomainNeedsTheWorkspace(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), PublicURL: "https://hub.example"}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	in := auth.Identity{Email: "ann@x.com", Verified: true, Workspace: true, Domain: "x.com"}
	if err := h.admitted(in); err != nil {
		t.Fatalf("a Workspace account: %v", err)
	}
	in.Domain = ""
	if h.admitted(in) == nil {
		t.Fatal("a personal Google account at the team's domain got in")
	}
	in.Workspace = false // another provider, which doesn't say
	if err := h.admitted(in); err != nil {
		t.Fatalf("an OIDC account at the team's domain: %v", err)
	}
}
