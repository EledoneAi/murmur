package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/internal/auth"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/store"
)

// safeNext keeps a page to return to after sign-in only when it is a path on this hub. Browsers drop tabs and line
// breaks from addresses and read a backslash as a slash, so "/<tab>/evil.example" or "/\\evil.example" would
// otherwise send people to another site.
func safeNext(n string) string {
	u, err := url.Parse(n)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") ||
		strings.ContainsFunc(n, func(r rune) bool { return r < ' ' || r == 0x7f || r == '\\' }) {
		return "/"
	}
	return n
}

// devLoginOn says whether people may sign in by typing an email: only on a hub that serves this computer alone.
func (h *Hub) devLoginOn() bool { return h.cfg.DevLogin && Local(h.cfg.PublicURL) }

// Local says whether a hub's address is this computer only: http on localhost or a loopback address.
func Local(publicURL string) bool {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func (h *Hub) providerName() string {
	if h.cfg.Auth == nil {
		return "email (local)"
	}
	return h.cfg.Auth.Name()
}

func (h *Hub) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if h.cfg.Auth == nil {
		if !h.devLoginOn() {
			h.message(w, http.StatusServiceUnavailable, "No sign-in set up", "This hub has no identity provider. Its admin sets one up in its configuration.")
			return
		}
		http.Redirect(w, r, "/?next="+url.QueryEscape(next), http.StatusFound) // the front page signs in by email
		return
	}
	// The browser keeps a secret; the state and the nonce, which travel in addresses, only carry values derived
	// from it. Someone who sees a sign-in's state and code can't finish it in another browser.
	secret := protocol.RandomHex(16)
	http.SetCookie(w, &http.Cookie{Name: "sy_n", Value: secret, Path: "/auth", MaxAge: 600, HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, h.cfg.Auth.AuthURL(h.signedExp("state", derive("state", secret)+" "+next, 10*time.Minute), derive("nonce", secret)), http.StatusFound)
}

func derive(purpose, secret string) string {
	sum := sha256.Sum256([]byte(purpose + "\x00" + secret))
	return hex.EncodeToString(sum[:16])
}

func (h *Hub) callback(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.unsignExp("state", r.URL.Query().Get("state"))
	bound, next, _ := strings.Cut(raw, " ")
	c, err := r.Cookie("sy_n")
	if !ok || err != nil || derive("state", c.Value) != bound || h.cfg.Auth == nil {
		h.message(w, http.StatusBadRequest, "Sign-in expired", "This sign-in link expired. Start again from the murmur page.")
		return
	}
	ctx, done := context.WithTimeout(r.Context(), 15*time.Second)
	defer done()
	id, err := h.cfg.Auth.Exchange(ctx, r.URL.Query().Get("code"), derive("nonce", c.Value))
	if err != nil {
		h.log.Info("sign-in failed", "provider", h.cfg.Auth.Name(), "error", err.Error())
		h.message(w, http.StatusBadGateway, "Sign-in failed", h.cfg.Auth.Name()+" didn't confirm who you are. Try again.")
		return
	}
	h.signIn(w, r, id, next)
}

func (h *Hub) devLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	// a hub with dev login serves the computer it runs on: a browser that reached it by another name, from the
	// network, gets nothing (curl can still lie about the name: publish the port on 127.0.0.1 only)
	if !h.devLoginOn() || h.cfg.Auth != nil || !Local("http://"+r.Host) {
		http.Error(w, "not available", http.StatusForbidden)
		return
	}
	_, domain, _ := strings.Cut(email, "@")
	h.signIn(w, r, auth.Identity{Email: email, Verified: true, Domain: domain}, safeNext(r.FormValue("next")))
}

// admitted says whether an identity may join the team, and why not when it can't.
func (h *Hub) admitted(id auth.Identity) error {
	email := strings.ToLower(id.Email)
	if !id.Verified || !strings.Contains(email, "@") {
		return errors.New("your identity provider didn't confirm this email address")
	}
	s := h.Settings()
	if slices.Contains(h.cfg.Admins, email) || (h.devLoginOn() && len(s.AllowedDomains)+len(s.AllowedEmails)+len(s.AllowedOrgs) == 0) {
		return nil // a hub on localhost with no list serves its developer alone
	}
	_, domain, _ := strings.Cut(email, "@")
	byDomain := slices.Contains(s.AllowedDomains, domain) && (!id.Workspace || id.Domain == domain)
	switch {
	case slices.Contains(s.AllowedEmails, email), byDomain:
		return nil
	case slices.ContainsFunc(id.Orgs, func(o string) bool { return slices.Contains(s.AllowedOrgs, strings.ToLower(o)) }):
		return nil
	}
	h.mu.Lock()
	u := h.users[email]
	h.mu.Unlock()
	if u != nil && u.Admin { // an admin made in the panel, whose domain the team no longer lists
		return nil
	}
	return fmt.Errorf("%s isn't on this team's list. Ask an admin to add you", email)
}

func (h *Hub) signIn(w http.ResponseWriter, r *http.Request, id auth.Identity, next string) {
	email := strings.ToLower(id.Email)
	if err := h.admitted(id); err != nil {
		_, domain, _ := strings.Cut(email, "@")
		h.log.Info("sign-in refused", "domain", domain, "verified", id.Verified)
		h.message(w, http.StatusForbidden, "Not on this team", err.Error())
		return
	}
	u, err := h.ensureUser(r.Context(), email, id.Name)
	if err != nil {
		h.log.Error("database", "op", "save person", "error", err.Error())
		h.message(w, http.StatusInternalServerError, "Try again", "Your account could not be saved.")
		return
	}
	if u.Disabled {
		h.log.Info("sign-in refused", "person", u.Handle, "why", "removed by an admin")
		h.message(w, http.StatusForbidden, "Removed", "An admin removed you from this team. Ask them if this is a mistake.")
		return
	}
	h.event(r.Context(), "signed in", u.Handle, "", "")
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: h.signedExp("session", email, 30*24*time.Hour), Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

var nonHandle = regexp.MustCompile(`[^a-z0-9-]+`)

// ensureUser registers a person the first time they sign in, and keeps their name and admin role up to date. Their
// handle is the first part of their email ("alice" for alice.martin@), or more when someone already has it.
// The store is written first, so a failed write changes nothing.
func (h *Hub) ensureUser(ctx context.Context, email, name string) (store.User, error) {
	name = protocol.Clean(name, false)
	h.mu.Lock()
	// on a hub on localhost with no admins at all, the first person in is its admin
	admin := slices.Contains(h.cfg.Admins, email) || (h.devLoginOn() && len(h.cfg.Admins) == 0 && !h.anyAdminLocked())
	if old := h.users[email]; old != nil {
		u := *old
		h.mu.Unlock()
		changed := (name != "" && u.Name != name) || (admin && !u.Admin)
		if !changed {
			return u, nil
		}
		if name != "" {
			u.Name = name
		}
		u.Admin = u.Admin || admin
		return u, h.putUser(ctx, u)
	}
	local, _, _ := strings.Cut(email, "@")
	first, _, _ := strings.Cut(local, ".")
	// "murmur" signs murmur's own notices
	taken := func(handle string) bool { return handle == "" || handle == protocol.System || h.handles[handle] != nil }
	handle := strings.Trim(nonHandle.ReplaceAllString(first, ""), "-")
	if taken(handle) {
		handle = strings.Trim(nonHandle.ReplaceAllString(strings.ReplaceAll(local, ".", "-"), ""), "-")
	}
	for i := 2; taken(handle); i++ {
		handle = fmt.Sprintf("%s%d", strings.Trim(nonHandle.ReplaceAllString(first, ""), "-"), i)
	}
	h.mu.Unlock()
	// ponytail: two instances can give one handle to two people signing up in the same second; unique handles in
	// the store if teams ever sign up that fast
	u := store.User{Email: email, Handle: handle, Name: name, Admin: admin, Created: time.Now()}
	return u, h.putUser(ctx, u)
}

// putUser saves a person, then updates this instance and tells the others.
func (h *Hub) putUser(ctx context.Context, u store.User) error {
	if err := h.store.PutUser(ctx, u); err != nil {
		return err
	}
	h.mu.Lock()
	if old := h.users[u.Email]; old != nil && old.Handle != u.Handle {
		delete(h.handles, old.Handle)
	}
	h.users[u.Email], h.handles[u.Handle] = &u, &u
	h.mu.Unlock()
	h.markDirty()
	h.changed(ctx, "people")
	return nil
}

// putDevice saves a laptop, then updates this instance and tells the others.
func (h *Hub) putDevice(ctx context.Context, d store.Device) error {
	if err := h.store.PutDevice(ctx, d); err != nil {
		return err
	}
	h.mu.Lock()
	h.devices[d.ID], h.tokens[d.TokenHash] = &d, &d
	if l := h.links[d.ID]; l != nil && !h.allowedLocked(d.ID) {
		l.cancel()
	}
	h.mu.Unlock()
	h.changed(ctx, "laptops")
	return nil
}

func (h *Hub) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.secure()})
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---------------------------------------------------------------- laptops

// Laptops sign in with OAuth for native apps (RFC 8252): `murmur init` listens on 127.0.0.1 and opens
// /cli/login; the person signs in (nothing at all when already signed in here); the browser is sent back to the
// laptop's own 127.0.0.1 with a one-time code; the laptop trades that code and the PKCE verifier it never sent for
// its token. A link that someone else started can't put a token on their laptop. Codes are in the store, so the
// browser and the laptop may reach different instances.

var challengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

var stateRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func (h *Hub) cliToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	sum := sha256.Sum256([]byte(req.Verifier))
	c, err := h.store.TakeCode(r.Context(), req.Code) // one try per code
	if err != nil || base64.RawURLEncoding.EncodeToString(sum[:]) != c.Challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this sign-in expired or was not started on this laptop; run murmur init again"})
		return
	}
	h.mu.Lock()
	u := h.users[c.Email]
	laptops := 0
	for _, d := range h.devices {
		if d.Email == c.Email && h.allowedLocked(d.ID) {
			laptops++
		}
	}
	h.mu.Unlock()
	if u == nil || u.Disabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you are no longer on this team"})
		return
	}
	if max := h.Settings().MaxLaptops; laptops >= max {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("you have %d laptops signed in, the most this team allows: revoke one on %s first", laptops, h.cfg.PublicURL)})
		return
	}
	token := protocol.RandomHex(32)
	d := store.Device{ID: protocol.RandomHex(8), Email: c.Email, Host: c.Host, TokenHash: hashToken(token), Created: time.Now()}
	if err := h.putDevice(r.Context(), d); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save this laptop; try again"})
		return
	}
	h.event(r.Context(), "laptop signed in", u.Handle, d.ID, d.Host)
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "email": u.Email, "handle": u.Handle, "name": u.Name})
}

// revoke signs a laptop out, when it belongs to email or by is an admin. It logs a failure to save it, and returns
// it for the pages that can say so.
func (h *Hub) revoke(ctx context.Context, id string, by store.User) error {
	h.mu.Lock()
	d := h.devices[id]
	h.mu.Unlock()
	if d == nil || (d.Email != by.Email && !by.Admin) {
		return nil
	}
	cp := *d
	cp.Revoked = true
	if err := h.putDevice(ctx, cp); err != nil {
		h.log.Error("database", "op", "save a revoked laptop", "laptop", id, "error", err.Error())
		return err
	}
	h.event(ctx, "laptop revoked", by.Handle, id, cp.Host)
	return nil
}

func (h *Hub) revokeSelf(w http.ResponseWriter, r *http.Request) {
	d, u, ok := h.bearer(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_ = h.revoke(r.Context(), d.ID, u)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) anyAdminLocked() bool {
	for _, u := range h.users {
		if u.Admin && !u.Disabled {
			return true
		}
	}
	return false
}

// Phrase is what both the terminal and the page show during a sign-in by code, so its person can tell the page
// belongs to the sign-in they started, and not to a link someone sent them.
func Phrase(state string) string {
	sum := sha256.Sum256([]byte(state))
	p := strings.ToUpper(hex.EncodeToString(sum[:4]))
	return p[:4] + "-" + p[4:]
}
