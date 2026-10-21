package auth

import "testing"

// Only true, or "true" as some providers write it, counts as a verified email.
func TestOIDCVerifiedMeansTrue(t *testing.T) {
	iss := newIssuer(t)
	p := newProvider(t, Config{Kind: "oidc", Issuer: iss.URL})
	for _, v := range []any{1, "1", "True", "TRUE", "yes", "verified", map[string]any{"value": true}, []any{true}} {
		id, err := signIn(t, p, iss, map[string]any{"email": "ada@example.com", "email_verified": v}, "n0nce")
		if err != nil || id.Verified {
			t.Errorf("email_verified %#v: %+v, %v", v, id, err)
		}
	}
}

// With OIDC_TRUST_EMAIL, preferred_username stands in for a missing email only when it is a bare address.
func TestTrustEmailNeedsABareAddress(t *testing.T) {
	iss := newIssuer(t)
	p := newProvider(t, Config{Kind: "oidc", Issuer: iss.URL, TrustEmail: true})
	for _, name := range []string{"Ada <ada@example.com>", " ada@example.com", "ada@example.com ", "ada@", "@example.com",
		"ada@example.com, eve@example.com", "ada"} {
		if id, err := signIn(t, p, iss, map[string]any{"preferred_username": name}, "n0nce"); err == nil {
			t.Errorf("preferred_username %q: signed in as %+v", name, id)
		}
	}
}

// Only Google says that an account belongs to a Workspace: another provider's hd claim is a claim like any other.
func TestOnlyGoogleVouchesForADomain(t *testing.T) {
	iss := newIssuer(t)
	id, err := signIn(t, newProvider(t, Config{Kind: "oidc", Issuer: iss.URL}), iss,
		map[string]any{"email": "ada@example.com", "email_verified": true, "hd": "example.com"}, "n0nce")
	if err != nil || id.Workspace {
		t.Fatalf("an OIDC provider's hd: %+v, %v", id, err)
	}
	old := googleIssuer
	googleIssuer = iss.URL
	t.Cleanup(func() { googleIssuer = old })
	id, err = signIn(t, newProvider(t, Config{Kind: "google"}), iss,
		map[string]any{"email": "ada@example.com", "email_verified": true, "hd": "Other.example"}, "n0nce")
	if err != nil || !id.Workspace || id.Domain != "other.example" {
		t.Fatalf("a Workspace account at another domain than its email's: %+v, %v", id, err)
	}
}

// GitHub signs in with the account's primary email only: a verified secondary one doesn't stand in for it.
func TestGitHubNeedsAPrimaryEmail(t *testing.T) {
	gh := startGitHub(t, &gitHub{user: `{"login":"ada"}`, orgs: [][]string{{}},
		emails: `[{"email":"ada@example.com","primary":false,"verified":true},{"email":"ada@users.noreply.github.com","primary":false,"verified":true}]`})
	if id, err := gh.signIn(t, newProvider(t, Config{Kind: "github", GitHubURL: gh.URL}), "n0nce"); err == nil {
		t.Fatalf("signed in without a primary email: %+v", id)
	}
}

// GitHub's PKCE verifier comes from the sign-in's nonce and the client secret: the nonce passes through the browser,
// so seeing it is not enough to trade a code.
func TestGitHubVerifierNeedsTheSecret(t *testing.T) {
	p := newProvider(t, Config{Kind: "github"}).(*githubProvider)
	q := query(t, p.AuthURL("st", "n0nce"))
	if q.Get("code_challenge") == "" || q.Get("code_challenge") == "n0nce" || p.verifier("n0nce") == p.verifier("n0nce2") {
		t.Fatalf("challenge %q", q.Get("code_challenge"))
	}
	other := newProvider(t, Config{Kind: "github"}).(*githubProvider)
	other.oauth.ClientSecret = "another-secret"
	if other.verifier("n0nce") == p.verifier("n0nce") {
		t.Fatal("the verifier doesn't depend on the client secret")
	}
}

// Identity providers are reached over https, or over http on this computer only: not on a name that only starts
// like localhost, nor on an address that only looks like a loopback one.
func TestProvidersOnThisComputerOnly(t *testing.T) {
	for _, raw := range []string{"http://localhost.evil.example", "http://127.0.0.1.nip.io", "http://0.0.0.0:9", "http://[::]:9",
		"http://localhost@evil.example", "http://evil.example#@localhost", "http://192.168.1.2", "ftp://localhost", "https://",
		"localhost", ""} {
		if err := checkServer(raw, "OIDC_ISSUER"); err == nil {
			t.Errorf("%q was taken", raw)
		}
	}
	for _, raw := range []string{"https://sso.example", "http://localhost:9", "http://127.0.0.1:9", "http://[::1]:9"} {
		if err := checkServer(raw, "OIDC_ISSUER"); err != nil {
			t.Errorf("%q: %v", raw, err)
		}
	}
	if _, err := New(Config{Kind: "github", ClientID: "hub", ClientSecret: "s3cret", RedirectURL: callback, GitHubURL: "http://localhost.evil.example"}); err == nil {
		t.Error("a GitHub Enterprise Server over plain http elsewhere than this computer")
	}
}
