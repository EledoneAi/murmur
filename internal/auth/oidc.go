package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// googleIssuer is a variable so that tests can stand in for Google.
var googleIssuer = "https://accounts.google.com"

// oidcProvider signs people in with OpenID Connect: Google, Microsoft Entra, Okta, Keycloak, Authentik, GitLab,
// Auth0 and the like.
type oidcProvider struct {
	name       string
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	hd         string
	trustEmail bool
	google     bool // Google's issuer: Domain is a Workspace's, when the account belongs to one
}

// claims are what the hub reads from an ID token.
type claims struct {
	Email             string `json:"email"`
	EmailVerified     any    `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	HD                string `json:"hd"`
}

func newOIDC(cfg Config) (*oidcProvider, error) {
	ctx, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	// The provider keeps this client to fetch signing keys later, outside any request, so it needs its own timeout.
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, &http.Client{Timeout: 15 * time.Second}), cfg.Issuer)
	if mismatch := (*oidc.IssuerMismatchError)(nil); errors.As(err, &mismatch) {
		return nil, fmt.Errorf("the identity provider calls itself %q: set OIDC_ISSUER to exactly that", mismatch.Discovered)
	}
	if err != nil {
		return nil, fmt.Errorf("discovering the identity provider at %s: %w", cfg.Issuer, err)
	}
	name := cfg.Name
	if name == "" {
		name = "SSO"
	}
	return &oidcProvider{
		name: name,
		oauth: &oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier:   p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		hd:         cfg.HostedDomain,
		trustEmail: cfg.TrustEmail,
		google:     cfg.Issuer == googleIssuer,
	}, nil
}

func (o *oidcProvider) Name() string { return o.name }

// AuthURL doesn't add PKCE: the nonce in the ID token already ties the code to this sign-in.
func (o *oidcProvider) AuthURL(state, nonce string) string {
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce)}
	if o.hd != "" {
		opts = append(opts, oauth2.SetAuthURLParam("hd", o.hd))
	}
	return o.oauth.AuthCodeURL(state, opts...)
}

func (o *oidcProvider) Exchange(ctx context.Context, code, nonce string) (Identity, error) {
	tok, err := o.oauth.Exchange(ctx, code)
	if err != nil {
		return Identity{}, fmt.Errorf("exchanging the sign-in code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, errors.New("the identity provider sent no ID token")
	}
	id, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("checking the ID token: %w", err)
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return Identity{}, errors.New("the ID token belongs to another sign-in: its nonce doesn't match")
	}
	var c claims
	if err := id.Claims(&c); err != nil {
		return Identity{}, fmt.Errorf("reading the ID token: %w", err)
	}
	return o.identity(c)
}

func (o *oidcProvider) identity(c claims) (Identity, error) {
	email := c.Email
	// Entra may only give the sign-in name, which is usually the person's address. Only TrustEmail says it is one
	// the organisation owns.
	if email == "" && o.trustEmail && isEmail(c.PreferredUsername) {
		email = c.PreferredUsername
	}
	if email == "" {
		return Identity{}, errors.New("the ID token has no email: grant the hub the email scope, or set OIDC_TRUST_EMAIL if sign-in names are addresses")
	}
	return Identity{
		Email: strings.ToLower(email),
		Name:  c.Name,
		// Some providers, such as AWS Cognito, send email_verified as a string.
		Verified:  o.trustEmail || c.EmailVerified == true || c.EmailVerified == "true",
		Domain:    strings.ToLower(c.HD),
		Workspace: o.google,
	}, nil
}

func isEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}
