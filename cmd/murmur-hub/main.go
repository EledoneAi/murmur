// Command murmur-hub is a team's murmur hub. It is configured with environment variables; docs/configuration.md
// lists them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EledoneAi/murmur/internal/auth"
	"github.com/EledoneAi/murmur/internal/bus"
	"github.com/EledoneAi/murmur/internal/hub"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
	"github.com/EledoneAi/murmur/internal/store"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("murmur-hub", release.Version)
		return
	}
	log := logger(os.Getenv("LOG_FORMAT"))
	if err := run(log); err != nil {
		log.Error("stopped", "error", err.Error())
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	port := env("PORT", "8080")
	dev := os.Getenv("DEV_LOGIN") == "1"
	cfg := hub.Config{
		PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:"+port), "/"),
		Version:   release.Version,
		Secret:    []byte(os.Getenv("SESSION_SECRET")),
		DevLogin:  dev,
		Admins:    hub.SplitList(os.Getenv("ADMINS")),
		Instance:  os.Getenv("INSTANCE_ID"),
		// TRUST_PROXY=1: the hub runs behind a proxy that appends the client's address to X-Forwarded-For
		TrustProxy: os.Getenv("TRUST_PROXY") == "1",
		GitHubURL:  os.Getenv("GITHUB_URL"),
		Log:        log,
	}
	if len(cfg.Secret) < 16 {
		if !dev {
			return errors.New("SESSION_SECRET must be set, 16 characters or more")
		}
		cfg.Secret = []byte(protocol.RandomHex(32)) // a local hub's sign-ins last until it restarts
	}
	seed, err := seedSettings()
	if err != nil {
		return err
	}
	cfg.Seed = seed
	if cfg.Auth, err = provider(cfg.PublicURL + "/auth/callback"); err != nil {
		return err
	}
	switch {
	case cfg.Auth == nil && !dev:
		return errors.New("no sign-in: set OIDC_ISSUER, GOOGLE_CLIENT_ID or GITHUB_CLIENT_ID (see docs/self-hosting.md), or DEV_LOGIN=1 for a local run")
	case dev && !hub.Local(cfg.PublicURL):
		return errors.New("DEV_LOGIN=1 lets anyone sign in by typing an email: it only runs with PUBLIC_URL on http://localhost")
	}
	if d := env("DIST_DIR", "/usr/share/murmur/dist"); dirExists(d) {
		cfg.Dist = os.DirFS(d)
	} else {
		log.Warn("no client binaries: laptops can't install or update from this hub", "dist_dir", d)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	storeURL := env("STORE", "memory://")
	if strings.HasPrefix(storeURL, "memory:") {
		log.Warn("STORE is memory://: people and laptops are forgotten when the hub stops")
	}
	octx, done := context.WithTimeout(ctx, 30*time.Second)
	st, err := store.Open(octx, storeURL)
	if err != nil {
		done()
		return fmt.Errorf("STORE: %w", err)
	}
	defer st.Close()
	b, err := bus.Open(octx, env("BUS", "local://"))
	done()
	if err != nil {
		return fmt.Errorf("BUS: %w", err)
	}
	defer b.Close()

	h, err := hub.New(ctx, cfg, st, b)
	if err != nil {
		return err
	}
	// No read or write timeout: laptops keep one WebSocket open for hours, and team pages a stream.
	srv := &http.Server{Addr: ":" + port, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		// stop taking connections first, so laptops dropped next reconnect to another instance, not to this one
		log.Info("shutting down")
		sctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(sctx)
		h.Close()
	}()
	log.Info("murmur hub", "version", release.Version, "port", port, "url", cfg.PublicURL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped // the store and the bus close once laptops are let go and counts saved
	return nil
}

// provider picks the identity provider from the environment: OIDC, Google or GitHub, or none.
func provider(redirect string) (auth.Provider, error) {
	var cfg auth.Config
	switch {
	case os.Getenv("OIDC_ISSUER") != "":
		cfg = auth.Config{Kind: "oidc", Issuer: os.Getenv("OIDC_ISSUER"), ClientID: os.Getenv("OIDC_CLIENT_ID"),
			ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"), Name: os.Getenv("OIDC_NAME"), TrustEmail: os.Getenv("OIDC_TRUST_EMAIL") == "1"}
	case os.Getenv("GOOGLE_CLIENT_ID") != "":
		cfg = auth.Config{Kind: "google", ClientID: os.Getenv("GOOGLE_CLIENT_ID"), ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET")}
		if ds := hub.SplitList(os.Getenv("ALLOWED_DOMAINS")); len(ds) == 1 {
			cfg.HostedDomain = ds[0]
		}
	case os.Getenv("GITHUB_CLIENT_ID") != "":
		cfg = auth.Config{Kind: "github", ClientID: os.Getenv("GITHUB_CLIENT_ID"), ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
			GitHubURL: os.Getenv("GITHUB_URL")}
	default:
		return nil, nil
	}
	cfg.RedirectURL = redirect
	return auth.New(cfg)
}

// seedSettings are the settings a new hub starts with: the defaults, changed by the environment. Once a hub has
// stored its settings, admins change them in the admin panel and these variables no longer apply.
func seedSettings() (hub.Settings, error) {
	s := hub.Defaults()
	s.AllowedDomains = hub.SplitList(os.Getenv("ALLOWED_DOMAINS"))
	s.AllowedEmails = hub.SplitList(os.Getenv("ALLOWED_EMAILS"))
	s.AllowedOrgs = hub.SplitList(os.Getenv("ALLOWED_GITHUB_ORGS"))
	if v := os.Getenv("TOKEN_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s, fmt.Errorf("TOKEN_DAYS: %q is not a number of days", v)
		}
		s.TokenDays = n
	}
	return s, s.Valid()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// logger writes one line per event: JSON, text for a terminal, or JSON with the field names Cloud Logging reads.
func logger(format string) *slog.Logger {
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, nil))
	case "gcp":
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		}}))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}
