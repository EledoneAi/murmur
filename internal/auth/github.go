package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
)

// githubProvider signs people in with a GitHub OAuth app, on github.com or GitHub Enterprise Server. GitHub isn't an
// OpenID provider: who someone is comes from its API, and there is no ID token to carry a nonce.
type githubProvider struct {
	name  string
	oauth *oauth2.Config
	api   string
	pkce  bool
}

func newGitHub(cfg Config) (*githubProvider, error) {
	base, api, pkce := "https://github.com", "https://api.github.com", true
	if u := strings.TrimSuffix(cfg.GitHubURL, "/"); u != "" && u != base {
		if err := checkServer(u, "GITHUB_URL"); err != nil {
			return nil, err
		}
		// Enterprise Server doesn't support PKCE yet (its 3.22 documentation says so).
		base, api, pkce = u, u+"/api/v3", false
	}
	name := cfg.Name
	if name == "" {
		name = "GitHub"
	}
	return &githubProvider{
		name: name,
		oauth: &oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Scopes: []string{"read:user", "user:email", "read:org"},
			Endpoint: oauth2.Endpoint{
				AuthURL: base + "/login/oauth/authorize", TokenURL: base + "/login/oauth/access_token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		api:  api,
		pkce: pkce,
	}, nil
}

func (g *githubProvider) Name() string { return g.name }

func (g *githubProvider) AuthURL(state, nonce string) string {
	if !g.pkce {
		return g.oauth.AuthCodeURL(state)
	}
	return g.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(g.verifier(nonce)))
}

func (g *githubProvider) Exchange(ctx context.Context, code, nonce string) (Identity, error) {
	var opts []oauth2.AuthCodeOption
	if g.pkce {
		opts = append(opts, oauth2.VerifierOption(g.verifier(nonce)))
	}
	tok, err := g.oauth.Exchange(ctx, code, opts...)
	if err != nil {
		return Identity{}, fmt.Errorf("exchanging the sign-in code: %w", err)
	}
	c := g.oauth.Client(ctx, tok)
	var user struct{ Login, Name string }
	if _, err := g.get(ctx, c, g.api+"/user", &user); err != nil {
		return Identity{}, err
	}
	var emails []struct {
		Email             string
		Primary, Verified bool
	}
	if _, err := g.get(ctx, c, g.api+"/user/emails?per_page=100", &emails); err != nil {
		return Identity{}, err
	}
	orgs, err := g.orgs(ctx, c)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{Name: user.Name, Orgs: orgs}
	if id.Name == "" {
		id.Name = user.Login
	}
	for _, e := range emails {
		if e.Primary {
			id.Email, id.Verified = strings.ToLower(e.Email), e.Verified
		}
	}
	if id.Email == "" {
		return Identity{}, errors.New("GitHub gave no primary email for this account")
	}
	return id, nil
}

// verifier is the PKCE secret of one sign-in. Only the nonce goes from AuthURL to Exchange, so the verifier is
// derived from it, keyed with the client secret: the nonce passes through the browser, the verifier must not.
func (g *githubProvider) verifier(nonce string) string {
	m := hmac.New(sha256.New, []byte(g.oauth.ClientSecret))
	m.Write([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (g *githubProvider) orgs(ctx context.Context, c *http.Client) ([]string, error) {
	var logins []string
	for url := g.api + "/user/orgs?per_page=100"; url != ""; {
		var page []struct{ Login string }
		next, err := g.get(ctx, c, url, &page)
		if err != nil {
			return nil, err
		}
		for _, o := range page {
			logins = append(logins, strings.ToLower(o.Login))
		}
		url = next
	}
	return logins, nil
}

// get reads one GitHub API response into v and returns the address of the next page, if there is one.
func (g *githubProvider) get(ctx context.Context, c *http.Client, url string, v any) (next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %s to %s", resp.Status, url)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return "", fmt.Errorf("reading GitHub's answer to %s: %w", url, err)
	}
	return nextPage(resp.Header.Get("Link"), g.api), nil
}

// nextPage finds the rel="next" address in a GitHub Link header. The token goes with every request, so only
// addresses on the API itself count.
func nextPage(link, api string) string {
	for part := range strings.SplitSeq(link, ",") {
		u, rel, _ := strings.Cut(part, ";")
		u = strings.Trim(strings.TrimSpace(u), "<>")
		if strings.TrimSpace(rel) == `rel="next"` && strings.HasPrefix(u, api+"/") {
			return u
		}
	}
	return ""
}
