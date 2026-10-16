package hub

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
	"github.com/EledoneAi/murmur/internal/store"
)

// The hub's web pages are an Astro app in web/, built into webdist and served from the binary. The pages are static:
// they ask the hub for what they show, as JSON, and the team page follows its stream. Rebuild them with make web.

//go:embed all:webdist
var webFiles embed.FS

var webdist, _ = fs.Sub(webFiles, "webdist")

// page serves one of the app's pages, such as "team/index.html".
func (h *Hub) page(file string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { h.servePage(w, http.StatusOK, file) }
}

func (h *Hub) servePage(w http.ResponseWriter, status int, file string) {
	h.servePageWith(w, status, file, "")
}

// servePageWith serves a page with tags added to its head: what the hub tells it, already escaped.
func (h *Hub) servePageWith(w http.ResponseWriter, status int, file, head string) {
	b, err := fs.ReadFile(webdist, file)
	if err != nil {
		h.log.Error("page", "file", file, "error", err.Error())
		http.Error(w, "this hub was built without its web pages: run make web", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.WriteHeader(status)
	if head != "" {
		b = bytes.Replace(b, []byte("</head>"), []byte(head+"</head>"), 1)
	}
	_, _ = w.Write(b)
}

// assets serves the app's scripts, styles and fonts. Their names carry a hash of their content, so they never change.
func (h *Hub) assets() http.Handler {
	files := http.FileServerFS(webdist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_astro/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}

// notFound is the app's page for an address the hub doesn't know.
func (h *Hub) notFound(w http.ResponseWriter, _ *http.Request) {
	h.servePage(w, http.StatusNotFound, "404.html")
}

// message answers with the notice page, which shows one title and one sentence: the hub writes them into the page,
// escaped, so nothing in an address can make it say anything else.
func (h *Hub) message(w http.ResponseWriter, status int, title, text string) {
	h.servePageWith(w, status, "notice/index.html", `<meta name="murmur-title" content="`+html.EscapeString(title)+
		`"><meta name="murmur-text" content="`+html.EscapeString(text)+`">`)
}

// apiMe is who is signed in: 401, with how to sign in, for a signed-out browser.
func (h *Hub) apiMe(w http.ResponseWriter, r *http.Request) {
	u := h.me(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"provider": h.providerName(), "dev": h.devLoginOn() && h.cfg.Auth == nil})
		return
	}
	h.mu.Lock()
	colour := h.coloursLocked()[u.Email]
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"handle": u.Handle, "name": u.Name, "email": u.Email, "admin": u.Admin,
		"colour": colour, "version": h.cfg.Version})
}

// apiHome is the front page of someone signed in: the install line and their laptops.
func (h *Hub) apiHome(w http.ResponseWriter, _ *http.Request, me store.User) {
	type device struct {
		ID       string     `json:"id"`
		Host     string     `json:"host"`
		Online   bool       `json:"online"`
		Created  time.Time  `json:"created"`
		LastSeen *time.Time `json:"last_seen,omitempty"`
	}
	h.mu.Lock()
	devs := []device{}
	online := h.onlineLocked()
	for _, d := range h.devices {
		if d.Email == me.Email && !d.Revoked {
			x := device{ID: d.ID, Host: d.Host, Online: online[d.ID], Created: d.Created}
			if !d.LastSeen.IsZero() {
				seen := d.LastSeen
				x.LastSeen = &seen
			}
			devs = append(devs, x)
		}
	}
	h.mu.Unlock()
	slices.SortFunc(devs, func(a, b device) int { return b.Created.Compare(a.Created) })
	writeJSON(w, http.StatusOK, map[string]any{"brew": "brew install " + release.Tap, "join": "murmur join " + h.cfg.PublicURL,
		"install": "curl -fsSL " + h.cfg.PublicURL + "/install.sh | sh", "devices": devs})
}

// apiTeam is the team as the team page shows it, once; the page then follows /team/events.
func (h *Hub) apiTeam(w http.ResponseWriter, _ *http.Request, me store.User) {
	v, ok := h.teamFor(me.Email)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "You are no longer on this team."})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// readJSON reads a page's JSON request into v, at most 256 KB, and answers 400 itself when it can't.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil { // sameOrigin caps posts at 256 KB
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The hub couldn't read this request."})
		return false
	}
	return true
}

// apiRevokeMine revokes one of your own laptops.
func (h *Hub) apiRevokeMine(w http.ResponseWriter, r *http.Request, me store.User) {
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	h.mu.Lock()
	d := h.devices[req.ID]
	mine := d != nil && d.Email == me.Email && !d.Revoked
	h.mu.Unlock()
	if !mine {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This laptop is unknown or already revoked."})
		return
	}
	if err := h.revoke(r.Context(), req.ID, me); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The hub couldn't save this. Try again."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// cliLoginPage is where a laptop's murmur join sends its person: the page that asks them to connect it.
func (h *Hub) cliLoginPage(w http.ResponseWriter, r *http.Request) {
	if h.me(r) == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	h.servePage(w, http.StatusOK, "connect/index.html")
}

// apiCliLogin connects a laptop once its person said yes. The laptop gets its sign-in through the browser, when it
// listens on a port, or its person pastes a code: port 0 is a laptop the browser can't reach, such as one over SSH,
// and only the laptop that started can use the code (PKCE).
func (h *Hub) apiCliLogin(w http.ResponseWriter, r *http.Request, me store.User) {
	var req struct {
		Port      string `json:"port"`
		Challenge string `json:"challenge"`
		State     string `json:"state"`
		Host      string `json:"host"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	port, err := strconv.Atoi(req.Port)
	if err != nil || (port != 0 && (port < 1024 || port > 65535)) || !challengeRe.MatchString(req.Challenge) || !stateRe.MatchString(req.State) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "This sign-in link is incomplete. Run murmur join again."})
		return
	}
	host := protocol.Clean(protocol.Cut(req.Host, 80), false)
	code := store.Code{ID: protocol.RandomHex(16), Email: me.Email, Host: host, Challenge: req.Challenge, Expires: time.Now().Add(codeTTL)}
	if err := h.store.PutCode(r.Context(), code); err != nil {
		h.log.Error("database", "op", "save a sign-in code", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "This laptop's sign-in could not be started. Try again."})
		return
	}
	if port == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"code": code.ID, "phrase": Phrase(req.State), "host": host})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": fmt.Sprintf("http://127.0.0.1:%d/callback?%s", port,
		url.Values{"code": {code.ID}, "state": {req.State}}.Encode())})
}

// adminPages serves the admin panel's pages. They are static: what they show comes from /api/admin, for admins only.
func (h *Hub) adminPages(mux *http.ServeMux) {
	for _, p := range []string{"", "/people", "/laptops", "/settings", "/events"} {
		file := "admin" + p + "/index.html"
		mux.HandleFunc("GET /admin"+p, h.member(func(w http.ResponseWriter, _ *http.Request, _ store.User) { h.servePage(w, http.StatusOK, file) }))
	}
}
