package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// The hub refuses to start with DEV_LOGIN on an address others reach, without a long SESSION_SECRET or a way to
// sign in, and with starting settings out of bounds.
func TestRunRefuses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const secret = "0123456789abcdef"
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"DEV_LOGIN": "1", "PUBLIC_URL": "https://murmur.example.com"}, "DEV_LOGIN"},
		{map[string]string{"DEV_LOGIN": "1", "PUBLIC_URL": "http://murmur.example.com"}, "DEV_LOGIN"},
		{map[string]string{"DEV_LOGIN": "1", "PUBLIC_URL": "http://localhost.evil.example:8080"}, "DEV_LOGIN"},
		{map[string]string{"DEV_LOGIN": "1", "PUBLIC_URL": "http://0.0.0.0:8080"}, "DEV_LOGIN"},
		{map[string]string{"DEV_LOGIN": "1", "PUBLIC_URL": "http://192.168.1.20:8080"}, "DEV_LOGIN"},
		{map[string]string{"DEV_LOGIN": "true", "PUBLIC_URL": "http://localhost:8080"}, "SESSION_SECRET"},
		{map[string]string{"SESSION_SECRET": "short"}, "SESSION_SECRET"},
		{map[string]string{"SESSION_SECRET": secret}, "no sign-in"},
		{map[string]string{"SESSION_SECRET": secret, "TOKEN_DAYS": "-1"}, "days a laptop stays signed in"},
		{map[string]string{"SESSION_SECRET": secret, "TOKEN_DAYS": "ninety"}, "TOKEN_DAYS"},
		{map[string]string{"SESSION_SECRET": secret, "ALLOWED_EMAILS": "nobody"}, "allowed email"},
		{map[string]string{"SESSION_SECRET": secret, "ALLOWED_DOMAINS": "ann@x.com"}, "allowed domain"},
	} {
		for _, k := range []string{"PORT", "PUBLIC_URL", "SESSION_SECRET", "DEV_LOGIN", "OIDC_ISSUER", "GOOGLE_CLIENT_ID", "GITHUB_CLIENT_ID",
			"TOKEN_DAYS", "ALLOWED_DOMAINS", "ALLOWED_EMAILS", "ALLOWED_GITHUB_ORGS", "ADMINS", "STORE", "BUS", "DIST_DIR"} {
			t.Setenv(k, c.env[k])
		}
		if err := run(log); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error naming %s", c.env, err, c.want)
		}
	}
}

// A hub with dev sign-in listens on this computer only: anyone who reaches it signs in as anyone, and its Host check
// alone doesn't stop a request from the network that names localhost. HOST says otherwise, as the image does.
func TestDevHubListensLocally(t *testing.T) {
	for _, c := range []struct {
		host string
		dev  bool
		want string
	}{{"", false, ":8080"}, {"", true, "127.0.0.1:8080"}, {"0.0.0.0", true, "0.0.0.0:8080"}, {"::1", true, "[::1]:8080"}} {
		t.Setenv("HOST", c.host)
		if got := listenAddr("8080", c.dev); got != c.want {
			t.Errorf("HOST=%q, dev %v: listens on %s, want %s", c.host, c.dev, got, c.want)
		}
	}
}
