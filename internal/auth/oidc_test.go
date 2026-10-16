package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

const callback = "https://hub.example/auth/callback"

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// issuer is a fake OpenID provider: discovery and signing keys come from oidctest, and its token endpoint hands out
// the ID token the code stands for.
type issuer struct{ *httptest.Server }

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	keys := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: testKey().Public(), KeyID: "k1", Algorithm: oidc.RS256}}}
	mux := http.NewServeMux()
	mux.Handle("/", keys)
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		if id != "hub" || secret != "s3cret" || r.FormValue("grant_type") != "authorization_code" || r.FormValue("redirect_uri") != callback {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": r.FormValue("code")})
	})
	iss := &issuer{httptest.NewServer(mux)}
	t.Cleanup(iss.Close)
	keys.SetIssuer(iss.URL)
	return iss
}

// signIn plays the person signing in at the provider. It returns a code for an ID token made for the client and
// nonce in authURL, with claims over those.
func (iss *issuer) signIn(t *testing.T, authURL string, claims map[string]any) string {
	t.Helper()
	q := query(t, authURL)
	c := map[string]any{"iss": iss.URL, "sub": "42", "aud": q.Get("client_id"), "nonce": q.Get("nonce"),
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	maps.Copy(c, claims)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return oidctest.SignIDToken(testKey(), "k1", oidc.RS256, string(b))
}

func query(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func newProvider(t *testing.T, cfg Config) Provider {
	t.Helper()
	cfg.ClientID, cfg.ClientSecret, cfg.RedirectURL = "hub", "s3cret", callback
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// signIn runs a whole sign-in against iss: the hub's nonce goes out in the authorization URL, and wantNonce is the
// one the hub holds when the person comes back.
func signIn(t *testing.T, p Provider, iss *issuer, claims map[string]any, wantNonce string) (Identity, error) {
	t.Helper()
	code := iss.signIn(t, p.AuthURL("st", "n0nce"), claims)
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	return p.Exchange(ctx, code, wantNonce)
}

func TestOIDCSignIn(t *testing.T) {
	iss := newIssuer(t)
	p := newProvider(t, Config{Kind: "oidc", Issuer: iss.URL})
	if p.Name() != "SSO" {
		t.Errorf("name %q, want SSO", p.Name())
	}
	q := query(t, p.AuthURL("st", "n0nce"))
	if q.Get("scope") != "openid email profile" || q.Get("state") != "st" || q.Get("nonce") != "n0nce" ||
		q.Get("redirect_uri") != callback || q.Has("hd") {
		t.Errorf("authorization URL query %v", q)
	}
	id, err := signIn(t, p, iss, map[string]any{"email": "Ada@Example.com", "email_verified": true, "name": "Ada Lovelace"}, "n0nce")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ada@example.com" || id.Name != "Ada Lovelace" || !id.Verified || id.Domain != "" || id.Orgs != nil {
		t.Errorf("identity %+v", id)
	}
}

func TestOIDCRefuses(t *testing.T) {
	iss := newIssuer(t)
	p := newProvider(t, Config{Kind: "oidc", Issuer: iss.URL})
	email := map[string]any{"email": "ada@example.com", "email_verified": true}
	for name, tc := range map[string]struct {
		claims map[string]any
		nonce  string
	}{
		"wrong nonce":    {email, "another"},
		"no nonce":       {email, ""},
		"wrong audience": {map[string]any{"aud": "another-client", "email": "ada@example.com"}, "n0nce"},
		"wrong issuer":   {map[string]any{"iss": "https://elsewhere.example", "email": "ada@example.com"}, "n0nce"},
		"expired":        {map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "email": "ada@example.com"}, "n0nce"},
		"no email":       {map[string]any{"preferred_username": "ada@example.com"}, "n0nce"},
	} {
		if id, err := signIn(t, p, iss, tc.claims, tc.nonce); err == nil {
			t.Errorf("%s: signed in as %+v", name, id)
		}
	}
}

func TestOIDCUnverifiedEmail(t *testing.T) {
	iss := newIssuer(t)
	p := newProvider(t, Config{Kind: "oidc", Issuer: iss.URL})
	for _, verified := range []any{false, "false", nil} {
		id, err := signIn(t, p, iss, map[string]any{"email": "ada@example.com", "email_verified": verified}, "n0nce")
		if err != nil || id.Verified {
			t.Errorf("email_verified %v: %+v, %v", verified, id, err)
		}
	}
	id, err := signIn(t, p, iss, map[string]any{"email": "ada@example.com", "email_verified": "true"}, "n0nce")
	if err != nil || !id.Verified {
		t.Errorf(`email_verified "true": %+v, %v`, id, err)
	}
}

func TestOIDCTrustEmail(t *testing.T) {
	iss := newIssuer(t)
	p := newProvider(t, Config{Kind: "oidc", Issuer: iss.URL, TrustEmail: true, Name: "Microsoft"})
	if p.Name() != "Microsoft" {
		t.Errorf("name %q, want Microsoft", p.Name())
	}
	id, err := signIn(t, p, iss, map[string]any{"email": "Ada@Example.com"}, "n0nce")
	if err != nil || id.Email != "ada@example.com" || !id.Verified {
		t.Errorf("email without email_verified: %+v, %v", id, err)
	}
	id, err = signIn(t, p, iss, map[string]any{"preferred_username": "Ada@Example.com", "name": "Ada"}, "n0nce")
	if err != nil || id.Email != "ada@example.com" || !id.Verified {
		t.Errorf("preferred_username: %+v, %v", id, err)
	}
	if id, err := signIn(t, p, iss, map[string]any{"preferred_username": "ada"}, "n0nce"); err == nil {
		t.Errorf("preferred_username that isn't an address: signed in as %+v", id)
	}
}

func TestGoogle(t *testing.T) {
	iss := newIssuer(t)
	old := googleIssuer
	googleIssuer = iss.URL
	t.Cleanup(func() { googleIssuer = old })
	p := newProvider(t, Config{Kind: "google", Issuer: "https://ignored.example", HostedDomain: "example.com", TrustEmail: true})
	if p.Name() != "Google" {
		t.Errorf("name %q, want Google", p.Name())
	}
	if hd := query(t, p.AuthURL("st", "n0nce")).Get("hd"); hd != "example.com" {
		t.Errorf("hd %q, want example.com", hd)
	}
	id, err := signIn(t, p, iss, map[string]any{"email": "ada@example.com", "email_verified": true, "hd": "Example.com"}, "n0nce")
	if err != nil || id.Domain != "example.com" || !id.Verified {
		t.Errorf("Workspace account: %+v, %v", id, err)
	}
	id, err = signIn(t, p, iss, map[string]any{"email": "ada@elsewhere.example", "email_verified": false}, "n0nce")
	if err != nil || id.Domain != "" || id.Verified {
		t.Errorf("unverified account without hd, TrustEmail must not apply to Google: %+v, %v", id, err)
	}
}

func TestNewRefuses(t *testing.T) {
	iss := newIssuer(t)
	ok := Config{ClientID: "hub", ClientSecret: "s3cret", RedirectURL: callback}
	with := func(f func(*Config)) Config { c := ok; f(&c); return c }
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{with(func(c *Config) {}), "OIDC_ISSUER"},
		{with(func(c *Config) { c.Kind = "saml" }), `"saml"`},
		{with(func(c *Config) { c.Kind, c.Issuer, c.ClientID = "oidc", iss.URL, "" }), "OIDC_CLIENT_ID"},
		{with(func(c *Config) { c.Kind, c.Issuer, c.ClientSecret = "oidc", iss.URL, "" }), "OIDC_CLIENT_SECRET"},
		{with(func(c *Config) { c.Kind, c.Issuer, c.RedirectURL = "oidc", iss.URL, "/auth/callback" }), "PUBLIC_URL"},
		{with(func(c *Config) { c.Kind = "oidc" }), "OIDC_ISSUER"},
		{with(func(c *Config) { c.Kind, c.Issuer = "oidc", "http://sso.example" }), "OIDC_ISSUER"},
		{with(func(c *Config) { c.Kind, c.Issuer = "oidc", iss.URL+"/" }), `OIDC_ISSUER to exactly that`},
		{with(func(c *Config) { c.Kind, c.Issuer = "oidc", "http://127.0.0.1:1" }), "http://127.0.0.1:1"},
		{with(func(c *Config) { c.Kind, c.ClientID = "google", "" }), "GOOGLE_CLIENT_ID"},
		{with(func(c *Config) { c.Kind, c.ClientSecret = "github", "" }), "GITHUB_CLIENT_SECRET"},
		{with(func(c *Config) { c.Kind, c.GitHubURL = "github", "http://github.example" }), "GITHUB_URL"},
	} {
		p, err := New(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v, %v; want an error naming %s", tc.cfg, p, err, tc.want)
		}
	}
}
