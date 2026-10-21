package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/store"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func deliver(t *testing.T, url, event, signature string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/github/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// GitHub's events are taken only with the signature of the team's secret, and never when the hub has none.
func TestGitHubWebhookSignature(t *testing.T) {
	h, err := newHub(t, Config{Secret: []byte("0123456789abcdef"), Seed: seed(func(s *Settings) { s.GitHubWebhookSecret = "s3cret" })}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	body := []byte(`{"action":"closed","pull_request":{"number":7,"merged":false},"repository":{"full_name":"acme/api"}}`)
	for _, c := range []struct {
		event, signature string
		want             int
	}{
		{"ping", sign("s3cret", body), http.StatusOK},
		{"pull_request", sign("s3cret", body), http.StatusAccepted},
		{"push", sign("s3cret", body), http.StatusAccepted},
		{"pull_request", sign("other", body), http.StatusForbidden},
		{"pull_request", sign("s3cret", append(body, ' ')), http.StatusForbidden},
		{"pull_request", strings.TrimPrefix(sign("s3cret", body), "sha256="), http.StatusForbidden},
		{"pull_request", "", http.StatusForbidden},
	} {
		if got := deliver(t, srv.URL, c.event, c.signature, body); got != c.want {
			t.Errorf("%s signed %q: %d, want %d", c.event, c.signature, got, c.want)
		}
	}
	if got := deliver(t, srv.URL, "pull_request", sign("s3cret", []byte("payload=%7B%7D")), []byte("payload=%7B%7D")); got != http.StatusBadRequest {
		t.Errorf("a form-encoded event: %d", got)
	}

	open, err := newHub(t, Config{Secret: []byte("0123456789abcdef")}, store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	srvOpen := httptest.NewServer(open.Handler())
	defer srvOpen.Close()
	if got := deliver(t, srvOpen.URL, "pull_request", sign("", body), body); got != http.StatusForbidden {
		t.Errorf("a hub without a secret took an event: %d", got)
	}
}

// fakeGitHub serves one pull request's files, two pages of them, to the hub's token only.
func fakeGitHub(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var pages []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/acme/api/pulls/7/files" || r.Header.Get("Authorization") != "Bearer ghtok" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()
		type file struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename,omitempty"`
		}
		var files []file
		if page == "1" {
			files = append(files, file{Filename: "api/routes.go"})
			for i := range 99 {
				files = append(files, file{Filename: fmt.Sprintf("gen/f%02d.go", i)})
			}
		} else {
			files = []file{{Filename: "x.go"}, {Filename: "go.sum"}, {Filename: "new/name.go", PreviousFilename: "old/name.go"}}
		}
		_ = json.NewEncoder(w).Encode(files)
	}))
	t.Cleanup(srv.Close)
	old := githubClient // the hub sends its token only over https, to GitHub's hosts: this one stands in for an Enterprise Server
	githubClient = srv.Client()
	t.Cleanup(func() { githubClient = old })
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(pages) }
}

// A merged pull request reaches the agents changing its files in that repo, on either instance: not the pull
// request's own branch, not other repos, not files the team ignores.
func TestMergeNotices(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	for _, h := range []string{"alice", "bob"} {
		_ = st.PutUser(ctx, store.User{Email: h + "@x.com", Handle: h})
		_ = st.PutDevice(ctx, store.Device{ID: "d-" + h, Email: h + "@x.com", TokenHash: hashToken("tok-" + h), Created: time.Now()})
	}
	b := &fanout{}
	gh, pages := fakeGitHub(t)
	start := func(name string) (*Hub, *httptest.Server) {
		h, err := New(ctx, Config{Secret: []byte("0123456789abcdef"), Instance: name, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			GitHubURL: gh.URL,
			Seed: seed(func(s *Settings) {
				s.GitHubWebhookSecret, s.GitHubToken, s.IgnoredPaths = "s3cret", "ghtok", []string{"go.sum"}
			})},
			st, b.open())
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h.Handler())
		t.Cleanup(func() { srv.Close(); h.Close() })
		return h, srv
	}
	one, srvOne := start("one")
	_, srvTwo := start("two")
	alice := connect(t, srvOne.URL, "tok-alice",
		protocol.Agent{Name: "a1", Repo: "Acme/API", Branch: "feature-a", Tree: "ta1", Files: []string{"api/routes.go", "x.go", "y.go"}},
		protocol.Agent{Name: "a2", Repo: "acme/api", Branch: "docs", Tree: "ta2", Files: []string{"README.md"}},
		protocol.Agent{Name: "a3", Repo: "acme/api", Branch: "deps", Tree: "ta3", Files: []string{"go.sum"}})
	bob := connect(t, srvTwo.URL, "tok-bob",
		protocol.Agent{Name: "b1", Repo: "acme/api", Branch: "feature-b", Tree: "tb1", Files: []string{"old/name.go"}},
		protocol.Agent{Name: "b2", Repo: "acme/api", Branch: "fix-routes", Tree: "tb2", Files: []string{"api/routes.go"}},
		protocol.Agent{Name: "b3", Repo: "acme/web", Branch: "main", Tree: "tb3", Files: []string{"api/routes.go"}})
	alice.expect(func(f protocol.Frame) bool {
		i := slices.IndexFunc(f.People, func(p protocol.Person) bool { return p.Handle == "bob" })
		return f.T == protocol.FrameRoster && i >= 0 && len(f.People[i].Agents) == 3
	})

	ev := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{"number": 7, "title": "Fix routes", "merged": true,
			"head": map[string]any{"ref": "fix-routes", "repo": map[string]any{"full_name": "acme/api"}},
			"base": map[string]any{"ref": "main"}},
		"repository": map[string]any{"full_name": "acme/api", "url": gh.URL + "/api/v3/repos/acme/api"}, // GitHub Enterprise
	}
	body := mustJSON(ev)
	if got := deliver(t, srvOne.URL, "pull_request", sign("s3cret", body), body); got != http.StatusAccepted {
		t.Fatalf("webhook: %d", got)
	}
	notice := func(f protocol.Frame) bool {
		return f.T == protocol.FrameMsg && !f.Echo && f.Msg.From == protocol.System && strings.HasPrefix(f.Msg.ID, "gh-")
	}
	m := alice.expect(notice)
	if m.Msg.To != "alice/a1" || m.Msg.Text != "pull request #7 in acme/api merged into \"main\", changing api/routes.go, x.go. Rebase before you push." {
		t.Fatalf("alice's notice: %+v", m.Msg)
	}
	if m := bob.expect(notice); m.Msg.To != "bob/b1" || !strings.Contains(m.Msg.Text, "changing old/name.go.") {
		t.Fatalf("bob's notice: %+v", m.Msg)
	}
	time.Sleep(200 * time.Millisecond)
	for _, l := range []*laptop{alice, bob} {
		for more := true; more; {
			select {
			case f := <-l.frames:
				l.backlog = append(l.backlog, f)
			default:
				more = false
			}
		}
		for _, f := range l.backlog {
			if notice(f) {
				t.Errorf("a notice for %s", f.Msg.To)
			}
		}
	}
	if got := pages(); !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("pages read: %v", got)
	}
	one.stats.mu.Lock()
	merges := one.stats.fresh[weekOf(time.Now(), "UTC")].Merges
	one.stats.mu.Unlock()
	if merges != 2 {
		t.Fatalf("merge notices counted: %d", merges)
	}

	// a laptop can't send under a notice's id, which would mark the notice as delivered before it comes
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: mergeID(prEvent{}, "bob/b1"), From: "a1", To: "bob/b1", Text: "hi"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && strings.HasPrefix(f.ID, "gh-") }); a.OK {
		t.Fatal("a laptop sent a message under a merge notice's id")
	}
	// nor under a late answer by the way's, which a console would show as an answer and not as the message it is
	alice.send(protocol.Frame{T: protocol.FrameSend, Msg: &protocol.Msg{ID: "bw-q1", From: "a1", To: "bob/b1", Text: "hi"}})
	if a := alice.expect(func(f protocol.Frame) bool { return f.T == protocol.FrameAck && f.ID == "bw-q1" }); a.OK {
		t.Fatal("a laptop sent a message under a late answer's id")
	}
}
