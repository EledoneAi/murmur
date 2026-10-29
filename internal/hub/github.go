package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// When a pull request merges, the agents still changing the same files on other branches hear of it before they
// push, instead of at the conflict. GitHub sends the event; the hub reads the merged files from GitHub's API.

// prEvent is the part of GitHub's pull_request event the hub reads.
type prEvent struct {
	Action string `json:"action"`
	PR     struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Merged bool   `json:"merged"`
		Head   struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repo struct {
		FullName string `json:"full_name"`
		URL      string `json:"url"` // the repo in GitHub's API: on api.github.com, or on a GitHub Enterprise server
	} `json:"repository"`
}

// githubClient reads merged files from GitHub's API.
var githubClient = &http.Client{Timeout: 20 * time.Second}

// githubWebhook takes GitHub's events, signed with the team's webhook secret. It answers at once and works after, as
// GitHub gives a webhook ten seconds.
func (h *Hub) githubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := h.Settings().GitHubWebhookSecret
	if secret == "" {
		http.Error(w, "This hub takes no GitHub events until an admin sets a webhook secret.", http.StatusForbidden)
		return
	}
	// pull request events are a few kilobytes; GitHub allows 25 MB, but this hub reads nothing that big unsigned
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "The event is too large.", http.StatusRequestEntityTooLarge)
		return
	}
	if !signedBy(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		h.log.Info("github event refused", "why", "bad signature", "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "The signature doesn't match this hub's webhook secret.", http.StatusForbidden)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		_, _ = io.WriteString(w, "pong")
		return
	case "pull_request":
	default:
		w.WriteHeader(http.StatusAccepted) // only pull requests matter here
		return
	}
	var ev prEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "The event isn't JSON: set the webhook's content type to application/json.", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	if ev.Action == "closed" && ev.PR.Merged {
		go h.mergeNotices(ev)
	}
}

// signedBy checks GitHub's signature of body: an HMAC-SHA256 with the webhook secret, compared in constant time.
func signedBy(secret string, body []byte, header string) bool {
	sig, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(sig, m.Sum(nil))
}

// mergeNotices tells the agents a merged pull request concerns.
func (h *Hub) mergeNotices(ev prEvent) {
	ctx, done := context.WithTimeout(context.Background(), 2*time.Minute)
	defer done()
	s := h.Settings()
	files, err := mergedFiles(ctx, ev, s.GitHubToken, h.githubAPIHosts())
	if err != nil {
		h.log.Error("github", "op", "read a merged pull request's files", "repo", ev.Repo.FullName, "pr", ev.PR.Number, "error", err.Error())
		return
	}
	files = slices.DeleteFunc(files, s.Ignored)
	h.mu.Lock()
	sent := h.mergeNoticesLocked(ev, files)
	h.mu.Unlock()
	h.log.Info("pull request merged", "repo", ev.Repo.FullName, "pr", ev.PR.Number, "files", len(files), "notices", sent)
	if sent > 0 {
		h.count(weekOf(time.Now(), s.TimeZone), func(w *weekStats) { w.Merges += sent })
	}
}

// githubAPIHosts are where the hub sends its GitHub token: github.com's API, and the Enterprise Server the hub
// signs people in with, if any. An event names its repository's API address, and whoever holds the webhook secret
// writes events: the token goes nowhere else.
func (h *Hub) githubAPIHosts() []string {
	hosts := []string{"api.github.com"}
	if u, err := url.Parse(h.cfg.GitHubURL); err == nil && u.Host != "" {
		hosts = append(hosts, u.Host)
	}
	return hosts
}

// mergedFiles lists the files a pull request changed, the old names of renamed files included, page by page.
func mergedFiles(ctx context.Context, ev prEvent, token string, hosts []string) ([]string, error) {
	u, err := url.Parse(ev.Repo.URL)
	if err != nil || u.Scheme != "https" || !slices.Contains(hosts, u.Host) || ev.PR.Number <= 0 {
		return nil, fmt.Errorf("the event's repository API address isn't GitHub's: %q", ev.Repo.URL)
	}
	var files []string
	for page := 1; page <= 30; page++ { // GitHub lists at most 3000 files, 100 a page
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			ev.Repo.URL+"/pulls/"+strconv.Itoa(ev.PR.Number)+"/files?per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := githubClient.Do(req)
		if err != nil {
			return nil, err
		}
		var batch []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&batch)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub answered %s", resp.Status)
		}
		if err != nil {
			return nil, err
		}
		for _, f := range batch {
			files = append(files, protocol.Clean(f.Filename, false))
			if f.PreviousFilename != "" {
				files = append(files, protocol.Clean(f.PreviousFilename, false))
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return files, nil
}

// mergeNoticesLocked sends a notice to every online agent in the merged repo whose changes touch the merged files,
// on whichever instance its laptop is connected to. The pull request's own branch already has them.
func (h *Hub) mergeNoticesLocked(ev prEvent, files []string) int {
	merged := map[string]bool{}
	for _, f := range files {
		merged[f] = true
	}
	own := ""
	if strings.EqualFold(ev.PR.Head.Repo.FullName, ev.Repo.FullName) {
		own = ev.PR.Head.Ref
	}
	// the title is the pull request author's words: agents get only the number and the branch, as data
	base := protocol.Clean(protocol.Cut(ev.PR.Base.Ref, 200), false)
	repo := protocol.Clean(protocol.Cut(ev.Repo.FullName, 200), false)
	sent := 0
	for _, x := range h.holdersLocked() {
		if !x.online {
			continue
		}
		for _, a := range x.agents {
			if !strings.EqualFold(a.Repo, ev.Repo.FullName) || (own != "" && a.Branch == own) {
				continue
			}
			common := slices.DeleteFunc(slices.Clone(a.Files), func(f string) bool { return !merged[f] })
			if len(common) == 0 {
				continue
			}
			to := x.handle + "/" + a.Name
			m := protocol.Msg{
				ID: mergeID(ev, to), From: protocol.System, To: to, At: time.Now().UnixMilli(),
				Text: fmt.Sprintf("pull request #%d in %s merged into %q, changing %s. Rebase before you push.",
					ev.PR.Number, repo, base, fileList(common)),
			}
			h.toDeviceLocked(x.dev, protocol.Frame{T: protocol.FrameMsg, Msg: &m}, "", 0)
			h.toPeopleLocked([]string{x.handle}, protocol.Frame{T: protocol.FrameMsg, Msg: &m, Echo: true}, []string{x.dev}, 0)
			h.log.Info("merge notice", "id", m.ID, "to", to, "repo", ev.Repo.FullName, "pr", ev.PR.Number, "files", len(common))
			sent++
		}
	}
	return sent
}

// mergeID is the same for the same merge and agent, so a laptop drops the notice GitHub delivers again.
func mergeID(ev prEvent, to string) string {
	s := sha256.Sum256([]byte("merge\x00" + strings.ToLower(ev.Repo.FullName) + "\x00" + strconv.Itoa(ev.PR.Number) + "\x00" + to))
	return "gh-" + hex.EncodeToString(s[:8])
}
