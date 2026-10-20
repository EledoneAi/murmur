package daemon

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/hub"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

// solo runs the hub of a laptop on its own (murmur solo) inside this daemon: in memory, on 127.0.0.1 only, with no
// sign-in page. The daemon signs its person in at each start and saves the token, which only this run's hub accepts.
// It keeps the port solo chose while it is free, and saves another when it isn't.
func solo(ctx context.Context) error {
	cfg := config.Load()
	addr := "127.0.0.1:0"
	if u, err := url.Parse(cfg.Hub); err == nil && u.Hostname() == "127.0.0.1" {
		addr = u.Host
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	seed := hub.Defaults()
	seed.ReleaseCheck = false // nothing leaves the laptop: it updates the way it was installed
	h, err := hub.New(ctx, hub.Config{PublicURL: "http://" + ln.Addr().String(), Version: release.Version,
		Secret: []byte(protocol.RandomHex(32)), Seed: seed,
		Log: slog.New(slog.NewTextHandler(log.Writer(), &slog.HandlerOptions{Level: slog.LevelWarn}))},
		store.NewMemory(), bus.NewLocal())
	if err != nil {
		_ = ln.Close()
		return err
	}
	host, _ := os.Hostname()
	u, token, err := h.Solo(ctx, cfg.Email, cfg.Name, host)
	if err != nil {
		_ = ln.Close()
		return err
	}
	cfg.Hub, cfg.Token, cfg.Handle = "http://"+ln.Addr().String(), token, u.Handle
	if err := config.Save(cfg); err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		h.Close()
	}()
	return nil
}
