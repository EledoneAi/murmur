// Package auth signs people in to a hub through their team's identity provider. It only says who someone is; the
// hub decides whether they get in (allowed domains, allowed emails, GitHub organisations, removed people).
package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
)

// Identity is who signed in, as the provider vouches for it.
type Identity struct {
	Email    string // lower case
	Name     string
	Verified bool   // the provider checked that this person owns Email
	Domain   string // the organisation's domain when the provider knows it (Google Workspace's hd), else ""
	// Workspace says the provider vouches for Domain: a Google account outside any Workspace has none, even when its
	// email is at a company's domain, so the hub admits it by domain only when Domain is that domain.
	Workspace bool
	Orgs      []string // GitHub organisations the person belongs to, by login in lower case; empty for other providers
}

type Provider interface {
	// Name is shown on the sign-in button: "Sign in with Google".
	Name() string
	// AuthURL is where the browser goes to sign in. state comes back on the callback; nonce is bound to the ID
	// token where the provider supports it.
	AuthURL(state, nonce string) string
	// Exchange trades the callback's code for the person's identity.
	Exchange(ctx context.Context, code, nonce string) (Identity, error)
}

// Config says which identity provider a hub uses. The hub fills it from its environment.
type Config struct {
	Kind         string // "oidc", "google" or "github"
	Issuer       string // OIDC: the issuer URL, exactly as the provider writes it in ID tokens
	ClientID     string
	ClientSecret string
	RedirectURL  string // the hub's /auth/callback
	Name         string // the sign-in button's label, when the default doesn't fit
	HostedDomain string // Google: the Workspace domain to offer accounts from. Only a hint: the hub checks Identity.Domain
	// TrustEmail counts every email from an OIDC provider as verified, and lets preferred_username stand in for a
	// missing email when it is an address. It is for providers that never send email_verified but only issue
	// addresses the organisation owns, such as Microsoft Entra.
	TrustEmail bool
	GitHubURL  string // GitHub Enterprise Server, such as https://github.example.com; github.com when empty
}

// New returns the provider cfg describes. Its errors name the environment variable an admin should set. An OIDC
// provider is discovered here, so New fails while its issuer can't be reached.
func New(cfg Config) (Provider, error) {
	switch cfg.Kind {
	case "oidc":
		if err := check(cfg, "OIDC"); err != nil {
			return nil, err
		}
		if err := checkServer(cfg.Issuer, "OIDC_ISSUER"); err != nil {
			return nil, err
		}
		return newOIDC(cfg)
	case "google":
		if err := check(cfg, "GOOGLE"); err != nil {
			return nil, err
		}
		// Google sends email_verified itself, and some Google accounts have addresses nobody checked.
		cfg.Issuer, cfg.TrustEmail = googleIssuer, false
		if cfg.Name == "" {
			cfg.Name = "Google"
		}
		return newOIDC(cfg)
	case "github":
		if err := check(cfg, "GITHUB"); err != nil {
			return nil, err
		}
		return newGitHub(cfg)
	case "":
		return nil, errors.New("no sign-in provider: set OIDC_ISSUER, GOOGLE_CLIENT_ID or GITHUB_CLIENT_ID")
	}
	return nil, fmt.Errorf("unknown sign-in provider %q: use oidc, google or github", cfg.Kind)
}

// check validates what every provider needs. env is the prefix of the provider's variables.
func check(cfg Config, env string) error {
	if cfg.ClientID == "" {
		return fmt.Errorf("%s_CLIENT_ID is not set: use the client ID the identity provider gave the hub", env)
	}
	if cfg.ClientSecret == "" {
		return fmt.Errorf("%s_CLIENT_SECRET is not set: use the client secret the identity provider gave the hub", env)
	}
	if u, err := url.Parse(cfg.RedirectURL); err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("the sign-in callback %q is not a full URL: set PUBLIC_URL to the hub's address", cfg.RedirectURL)
	}
	return nil
}

// checkServer refuses an identity provider's address unless it is https: the hub believes what the provider says
// about people. Plain http is only for a provider on the same machine, in development.
func checkServer(raw, env string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not a URL", env, raw)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return fmt.Errorf("%s %q must start with https://", env, raw)
	}
	return nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
