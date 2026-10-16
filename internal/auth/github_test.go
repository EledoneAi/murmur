package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitHub is a fake GitHub: an OAuth app's endpoints and the API under /api/v3, as on Enterprise Server. A sign-in
// code is "code" followed by the PKCE challenge of that sign-in, if any.
type gitHub struct {
	*httptest.Server
	user, emails string     // the JSON of /user and /user/emails
	orgs         [][]string // the pages of /user/orgs
}

func startGitHub(t *testing.T, gh *gitHub) *gitHub {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		challenge, ok := strings.CutPrefix(r.FormValue("code"), "code")
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if !ok || r.FormValue("client_id") != "hub" || r.FormValue("client_secret") != "s3cret" ||
			challenge != "" && challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			// GitHub reports a bad code with a 200, in the body.
			w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
			_, _ = w.Write([]byte("error=bad_verification_code"))
			return
		}
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		_, _ = w.Write([]byte("access_token=gho_token&scope=read%3Aorg%2Cread%3Auser%2Cuser%3Aemail&token_type=bearer"))
	})
	api := func(path string, body func(w http.ResponseWriter, r *http.Request) string) {
		mux.HandleFunc("GET /api/v3"+path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gho_token" {
				http.Error(w, `{"message":"Requires authentication"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body(w, r)))
		})
	}
	api("/user", func(http.ResponseWriter, *http.Request) string { return gh.user })
	api("/user/emails", func(http.ResponseWriter, *http.Request) string { return gh.emails })
	api("/user/orgs", func(w http.ResponseWriter, r *http.Request) string {
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n = max(n, 1)
		if n < len(gh.orgs) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/user/orgs?per_page=100&page=%d>; rel="next", <%s/api/v3/user/orgs?per_page=100&page=%d>; rel="last"`,
				gh.URL, n+1, gh.URL, len(gh.orgs)))
		}
		page := []map[string]string{}
		for _, login := range gh.orgs[n-1] {
			page = append(page, map[string]string{"login": login})
		}
		b, _ := json.Marshal(page)
		return string(b)
	})
	gh.Server = httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	return gh
}

// signIn runs a whole sign-in on gh with p, nonce being what the hub holds on both ends.
func (gh *gitHub) signIn(t *testing.T, p Provider, nonce string) (Identity, error) {
	t.Helper()
	q := query(t, p.AuthURL("st", nonce))
	if q.Get("scope") != "read:user user:email read:org" || q.Get("state") != "st" || q.Get("redirect_uri") != callback {
		t.Errorf("authorization URL query %v", q)
	}
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	return p.Exchange(ctx, "code"+q.Get("code_challenge"), nonce)
}

func TestGitHubSignIn(t *testing.T) {
	gh := startGitHub(t, &gitHub{
		user:   `{"login":"ada","name":"Ada Lovelace"}`,
		emails: `[{"email":"ada@users.noreply.github.com","primary":false,"verified":true},{"email":"Ada@Example.com","primary":true,"verified":true}]`,
		orgs:   [][]string{{"Analytical-Engines", "babbage"}, {"Royal-Society"}, {"difference"}},
	})
	p := newProvider(t, Config{Kind: "github", GitHubURL: gh.URL + "/"})
	if p.Name() != "GitHub" {
		t.Errorf("name %q, want GitHub", p.Name())
	}
	id, err := gh.signIn(t, p, "n0nce")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ada@example.com" || !id.Verified || id.Name != "Ada Lovelace" || id.Domain != "" ||
		!slices.Equal(id.Orgs, []string{"analytical-engines", "babbage", "royal-society", "difference"}) {
		t.Errorf("identity %+v", id)
	}
}

func TestGitHubUnverifiedPrimaryEmail(t *testing.T) {
	gh := startGitHub(t, &gitHub{
		user:   `{"login":"ada","name":null}`,
		emails: `[{"email":"ada@example.com","primary":true,"verified":false},{"email":"ada@elsewhere.example","primary":false,"verified":true}]`,
		orgs:   [][]string{{}},
	})
	id, err := gh.signIn(t, newProvider(t, Config{Kind: "github", GitHubURL: gh.URL}), "n0nce")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ada@example.com" || id.Verified || id.Name != "ada" || len(id.Orgs) != 0 {
		t.Errorf("identity %+v", id)
	}
}

func TestGitHubPKCE(t *testing.T) {
	gh := startGitHub(t, &gitHub{user: `{"login":"ada"}`, emails: `[{"email":"ada@example.com","primary":true,"verified":true}]`, orgs: [][]string{{}}})
	if q := query(t, newProvider(t, Config{Kind: "github", GitHubURL: gh.URL}).AuthURL("st", "n0nce")); q.Has("code_challenge") {
		t.Errorf("PKCE sent to Enterprise Server: %v", q)
	}
	// github.com, moved onto the fake.
	p := newProvider(t, Config{Kind: "github"}).(*githubProvider)
	p.oauth.Endpoint.AuthURL, p.oauth.Endpoint.TokenURL, p.api = gh.URL+"/login/oauth/authorize", gh.URL+"/login/oauth/access_token", gh.URL+"/api/v3"
	q := query(t, p.AuthURL("st", "n0nce"))
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 {
		t.Fatalf("no PKCE challenge for github.com: %v", q)
	}
	if id, err := gh.signIn(t, p, "n0nce"); err != nil || id.Email != "ada@example.com" {
		t.Errorf("sign-in with PKCE: %+v, %v", id, err)
	}
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if id, err := p.Exchange(ctx, "code"+q.Get("code_challenge"), "another"); err == nil {
		t.Errorf("a code from another sign-in was accepted: %+v", id)
	}
}

func TestNextPage(t *testing.T) {
	const api = "https://gh.example/api/v3"
	for _, tc := range []struct{ link, want string }{
		{"", ""},
		{`<https://gh.example/api/v3/user/orgs?page=2>; rel="next", <https://gh.example/api/v3/user/orgs?page=5>; rel="last"`, api + "/user/orgs?page=2"},
		{`<https://gh.example/api/v3/user/orgs?page=1>; rel="prev"`, ""},
		{`<https://elsewhere.example/api/v3/user/orgs?page=2>; rel="next"`, ""},
		{`<https://gh.example/api/v3.elsewhere.example/user/orgs?page=2>; rel="next"`, ""},
	} {
		if got := nextPage(tc.link, api); got != tc.want {
			t.Errorf("nextPage(%q) = %q, want %q", tc.link, got, tc.want)
		}
	}
}
