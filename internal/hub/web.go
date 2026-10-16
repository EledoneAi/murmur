package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/store"
)

// The web side is small on purpose: sign in, connect a laptop, see your laptops, the team page and the admin panel.
// The console in the terminal is where people use murmur; the team page shows its live view in a browser.

const cookieName = "murmur"

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	// the pages, the app's files and what the pages ask for
	mux.HandleFunc("GET /{$}", h.page("index.html"))
	mux.HandleFunc("GET /team", h.member(func(w http.ResponseWriter, r *http.Request, _ store.User) {
		h.servePage(w, http.StatusOK, "team/index.html")
	}))
	mux.Handle("GET /_astro/", h.assets())
	mux.Handle("GET /favicon.svg", h.assets())
	mux.HandleFunc("GET /api/me", h.apiMe)
	mux.HandleFunc("GET /api/home", h.member(h.apiHome))
	mux.HandleFunc("GET /api/team", h.member(h.apiTeam))
	mux.HandleFunc("POST /api/devices/revoke", h.member(h.apiRevokeMine))
	// sign-in, of people and of laptops
	mux.HandleFunc("GET /login", h.login)
	mux.HandleFunc("POST /dev-login", h.devLogin)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.HandleFunc("POST /logout", h.logout)
	mux.HandleFunc("GET /cli/login", h.cliLoginPage)
	mux.HandleFunc("POST /api/cli/login", h.member(h.apiCliLogin))
	mux.HandleFunc("POST /cli/token", h.cliToken)
	mux.HandleFunc("POST /api/revoke", h.revokeSelf)
	// the team page's stream, its messages, and the pull requests for laptops
	mux.HandleFunc("GET /team/events", h.member(h.teamEvents))
	mux.HandleFunc("GET /team/prs", h.teamPRs)
	mux.HandleFunc("POST /team/send", h.member(h.teamSend))
	h.adminRoutes(mux)
	h.adminPages(mux)
	mux.HandleFunc("GET /install.sh", h.install)
	mux.HandleFunc("GET /dl/{os}/{arch}", h.download)
	mux.HandleFunc("GET /dl/version", h.version)
	mux.HandleFunc("GET /dl/{file}", h.distFile)
	mux.HandleFunc("GET /ws", h.serveWS)
	mux.HandleFunc("POST /github/webhook", h.githubWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /", h.notFound)
	return h.sameOrigin(mux)
}

// sameOrigin refuses form posts from other sites: the pages sign people in with a cookie, and SameSite=Lax alone
// leaves older browsers and some redirects open. Laptops post JSON with a bearer token and send no Origin.
func (h *Hub) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Path != "/github/webhook" { // which reads its own, larger, events
				r.Body = http.MaxBytesReader(w, r.Body, 256<<10) // no form or laptop request comes near it
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				if ref, err := url.Parse(r.Header.Get("Referer")); err == nil && ref.Host != "" {
					origin = ref.Scheme + "://" + ref.Host
				}
			}
			if origin != "" && origin != h.origin() {
				http.Error(w, "This form must be sent from "+h.origin()+".", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) origin() string {
	u, err := url.Parse(h.cfg.PublicURL)
	if err != nil {
		return h.cfg.PublicURL
	}
	return u.Scheme + "://" + u.Host
}

// ---------------------------------------------------------------- signed values

func (h *Hub) sign(v string) string {
	m := hmac.New(sha256.New, h.cfg.Secret)
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString([]byte(v)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *Hub) unsign(s string) (string, bool) {
	a, b, ok := strings.Cut(s, ".")
	if !ok {
		return "", false
	}
	v, err1 := base64.RawURLEncoding.DecodeString(a)
	sig, err2 := base64.RawURLEncoding.DecodeString(b)
	if err1 != nil || err2 != nil {
		return "", false
	}
	m := hmac.New(sha256.New, h.cfg.Secret)
	m.Write(v)
	return string(v), hmac.Equal(sig, m.Sum(nil))
}

// signedExp is "purpose|value|unix expiry", signed. The purpose keeps a value signed for one use, such as a sign-in
// state, from working as another, such as a session cookie.
func (h *Hub) signedExp(purpose, v string, ttl time.Duration) string {
	return h.sign(purpose + "|" + v + "|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10))
}

func (h *Hub) unsignExp(purpose, s string) (string, bool) {
	raw, ok := h.unsign(s)
	rest, ok2 := strings.CutPrefix(raw, purpose+"|")
	i := strings.LastIndex(rest, "|") // the value may hold a | itself, such as a page to return to
	if !ok || !ok2 || i < 0 {
		return "", false
	}
	t, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil || time.Now().Unix() > t {
		return "", false
	}
	return rest[:i], true
}

func (h *Hub) secure() bool { return strings.HasPrefix(h.cfg.PublicURL, "https://") }

// me is the person signed in to the browser making r, if they may still use the hub.
func (h *Hub) me(r *http.Request) *store.User {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	email, ok := h.unsignExp("session", c.Value)
	if !ok {
		return nil
	}
	h.mu.Lock()
	_, known := h.users[email]
	h.mu.Unlock()
	if !known { // signed up on another instance a moment ago
		h.reloadSoon(r.Context())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if u := h.users[email]; u != nil && !u.Disabled {
		cp := *u
		return &cp
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
