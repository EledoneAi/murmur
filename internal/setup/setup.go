// Package setup connects a laptop: sign-in, the hooks and MCP server in each harness, and the checks of doctor.
package setup

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/harness/claude"
	"github.com/EledoneAi/murmur/v2/internal/harness/hookish"
	"github.com/EledoneAi/murmur/v2/internal/harness/opencode"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// Harnesses are the AI coding tools murmur connects, in the order join sets them up.
var Harnesses = []harness.Harness{claude.Claude{}, hookish.Codex, hookish.Cursor, opencode.OpenCode{}, hookish.Copilot, hookish.Gemini}

// Harness is the harness called name, or nil.
func Harness(name string) harness.Harness {
	for _, h := range Harnesses {
		if h.Name() == name {
			return h
		}
	}
	return nil
}

// ---------------------------------------------------------------- join

// color is off under NO_COLOR and when the output goes to a file or another program.
var color = func() bool {
	st, err := os.Stdout.Stat()
	return os.Getenv("NO_COLOR") == "" && err == nil && st.Mode()&os.ModeCharDevice != 0
}()

func step(ok bool, label, detail string) {
	mark := "✓"
	switch {
	case !ok && color:
		mark = "\033[33m!\033[0m"
	case !ok:
		mark = "!"
	case color:
		mark = "\033[32m✓\033[0m"
	}
	fmt.Printf("%s %-14s %s\n", mark, label, detail)
}

// steps prints a harness's checks under its name, and says whether they all passed.
func steps(h harness.Harness, checks []protocol.Check) bool {
	fmt.Println("  " + h.Title())
	ok := true
	for _, c := range checks {
		step(c.OK, "  "+c.Name, c.Detail)
		ok = ok && c.OK
	}
	return ok
}

func openBrowser(url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	_ = exec.Command(cmd, url).Start()
}

func postJSON(url string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out)
	}
	return resp.StatusCode, nil
}

// signIn connects this laptop with OAuth for native apps: a one-shot listener on 127.0.0.1 receives the
// browser after sign-in, and the code it brings is traded, with the PKCE verifier, for this laptop's token.
func signIn(cfg config.Config, noBrowser bool) (config.Config, error) {
	if noBrowser {
		return signInByCode(cfg)
	}
	verifier, state := protocol.RandomHex(32), protocol.RandomHex(16)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return cfg, err
	}
	type result struct {
		cfg config.Config
		err error
	}
	done := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state { // only the sign-in this join started
			http.NotFound(w, r)
			return
		}
		var got struct{ Token, Email, Handle, Name, Error string }
		status, err := postJSON(cfg.Hub+"/cli/token", map[string]string{"code": r.URL.Query().Get("code"), "verifier": verifier}, &got)
		res := result{cfg: cfg}
		switch {
		case err != nil:
			res.err = fmt.Errorf("cannot reach murmur: %w", err)
		case status != http.StatusOK:
			res.err = errors.New(got.Error)
		default:
			res.cfg.Token, res.cfg.Email, res.cfg.Handle, res.cfg.Name = got.Token, got.Email, got.Handle, got.Name
			res.err = config.Save(res.cfg)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		title, text := "Connected", "This laptop is on murmur as "+got.Handle+". You can close this tab."
		if res.err != nil {
			title, text = "Not connected", res.err.Error()
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Murmur</title><body style="font:16px system-ui;margin:15vh auto;max-width:34em;padding:0 20px"><h1>%s</h1><p>%s</p>`,
			html.EscapeString(title), html.EscapeString(text))
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // the page reaches the browser before join stops this server
		}
		select {
		case done <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = srv.Shutdown(ctx)
	}()
	login := cfg.Hub + "/cli/login?" + url.Values{
		"port": {strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)}, "challenge": {challenge}, "state": {state}, "host": {hostname()},
	}.Encode()
	fmt.Println("  Sign in to " + cfg.Hub + " in the browser that just opened.")
	fmt.Println("  If none opened: " + login)
	openBrowser(login)
	select {
	case r := <-done:
		return r.cfg, r.err
	case <-time.After(10 * time.Minute):
		return cfg, errors.New("no sign-in within 10 minutes; run murmur join again")
	}
}

// Join signs this laptop in to its team's hub and connects the coding tools installed on it, leaving the others alone.
// hub names the hub the first time, or a new one; after that the laptop remembers it. Without one, it asks for it on
// the terminal.
func Join(hub string, relogin, noBrowser bool) error {
	cfg := config.Load()
	if cfg.Solo { // its own hub signs nobody in: join asks for the team's
		cfg.Hub, cfg.Token, cfg.Solo = "", "", false
	}
	if hub == "" && cfg.Hub == "" {
		hub = askLine("Your team's hub (its address, such as murmur.example.com): ")
	}
	if hub = hubAddress(hub); hub != "" && hub != cfg.Hub {
		cfg.Hub, cfg.Token, relogin = hub, "", true // a token is only good on the hub that gave it
	}
	if cfg.Hub == "" {
		return config.ErrNoHub
	}
	if err := checkHub(cfg.Hub); err != nil {
		return err
	}
	if err := guardDev(); err != nil {
		return err
	}
	fmt.Println("Murmur")
	if cfg.Token != "" && !relogin && !TokenAccepted(cfg) {
		step(false, "Signed out", "the hub no longer accepts this laptop: signing in again")
		relogin = true
	}
	if cfg.Token == "" || relogin {
		var err error
		// over SSH, a browser on the person's own computer can't reach this machine's 127.0.0.1
		if cfg, err = signIn(cfg, noBrowser || os.Getenv("SSH_CONNECTION") != ""); err != nil {
			return err
		}
	}
	step(true, "Signed in", fmt.Sprintf("%s (%s)", cfg.Handle, cfg.Email))
	return connect(cfg)
}

// Solo sets this laptop up on its own, with no team and no hub to run: its daemon runs a hub of its own on
// 127.0.0.1, which only this laptop reaches, and signs its person in under their login name. murmur join moves the
// laptop to a team's hub.
func Solo() error {
	cfg := config.Load()
	if cfg.Token != "" && !cfg.Solo {
		return fmt.Errorf("this laptop is in the team at %s: murmur uninstall first, to use murmur on its own", cfg.Hub)
	}
	if err := guardDev(); err != nil {
		return err
	}
	if !cfg.Solo {
		cfg.Hub = "" // the daemon picks a free port
	}
	// the handle comes from the email: the login name, as the terminal knows its person
	cfg.Solo, cfg.Token, cfg.Email = true, "", strings.ToLower(cmp.Or(os.Getenv("USER"), "me"))+"@localhost"
	if out, err := exec.Command("git", "config", "--get", "user.name").Output(); err == nil {
		cfg.Name = strings.TrimSpace(string(out))
	}
	fmt.Println("Murmur")
	step(true, "On your own", "a hub of this laptop's own, which only it reaches: murmur join moves it to a team")
	return connect(cfg)
}

// connect connects each coding tool installed here, saves the config and starts the daemon on it.
func connect(cfg config.Config) error {
	exe := config.Exe()
	found := false
	for _, h := range Harnesses { // each one installed here; the others are left alone
		if h.Installed() {
			found = true
			steps(h, h.Connect(exe, ask))
		}
	}
	if !found {
		step(false, "Coding tools", "none found: install one of "+titles()+", then run murmur join again")
	}
	cfg.Dirs = map[string]string{} // where the tools are, for a daemon that a tool with a bare environment starts
	for _, k := range config.DirVars {
		cfg.Dirs[k] = os.Getenv(k)
	}
	if err := config.Save(cfg); err != nil {
		return err
	}

	_ = os.WriteFile(setupMarker(), []byte(release.Version), 0o600)

	_ = ipc.Call(ipc.Request{Op: "stop"}, &ipc.Reply{}, 2*time.Second) // pick up the new token and binary
	time.Sleep(100 * time.Millisecond)
	if err := ipc.EnsureDaemon(); err != nil {
		return err
	}
	var st ipc.Reply
	for range 30 { // wait briefly for the hub, so the summary is true
		if ipc.Call(ipc.Request{Op: "status"}, &st, 2*time.Second) == nil && st.State == "online" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	step(st.State == "online", "Daemon", st.State+", "+st.Text)
	fmt.Println("\nSessions that were already open receive messages now; restart them to give them the tools and hooks.")
	if m, _ := filepath.Glob(filepath.Join(config.Home(), ".vscode*", "extensions", "anthropic.claude-code-*")); len(m) > 0 {
		fmt.Println("VS Code: new Claude conversations are connected; for open ones, run Developer: Reload Window.")
	}
	fmt.Println("Open the console with: murmur")
	return nil
}

func setupMarker() string { return filepath.Join(config.Dir(), "setup-version") }

// hubAddress is a hub's address as people type it, with its scheme: https://, or http:// for this machine.
func hubAddress(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if scheme, rest, ok := strings.Cut(s, "://"); ok { // HTTPS:// as some type it: the daemon makes wss:// of https://
		return strings.ToLower(scheme) + "://" + rest
	}
	if s == "" {
		return s
	}
	if host, _, err := net.SplitHostPort(s); loopback(s) || (err == nil && loopback(host)) {
		return "http://" + s
	}
	return "https://" + s
}

// checkHub refuses what isn't a hub's address, and a hub that would get this laptop's sign-in and send it updates
// in clear: http:// is for a hub on this machine only.
func checkHub(hub string) error {
	u, err := url.Parse(hub)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return fmt.Errorf("%q is not a hub address: it starts with https://", hub)
	case u.User != nil: // hub.acme.com@evil.example is evil.example; a password would be shown and saved
		return fmt.Errorf("%s is the hub at %s, with a user name before the @: give the hub's address alone", u.Redacted(), u.Host)
	case strings.ContainsAny(hub, "?#"): // a page's address, copied from the browser: murmur adds its paths after it
		return fmt.Errorf("%q is a page's address: use the hub's own, %s://%s", hub, u.Scheme, u.Host)
	case u.Scheme == "http" && !loopback(u.Hostname()):
		return fmt.Errorf("%s is not encrypted: this laptop's sign-in and its updates would cross the network in clear. "+
			"Use the hub's https:// address; http:// is only for a hub on this machine", hub)
	}
	return nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// TokenAccepted asks the hub whether this laptop's sign-in still works: a revoked or unknown token gets 401 on /ws.
// Any other answer, or none, keeps the token, so join never asks for a sign-in only because the network is down.
func TokenAccepted(cfg config.Config) bool {
	req, err := http.NewRequest(http.MethodGet, cfg.Hub+"/ws", nil)
	if err != nil {
		return true
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return true
	}
	resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized
}

// Doctor checks what join set up, one line per check, and says how to fix what is wrong. It returns false when
// something needs fixing.
func Doctor() bool {
	fmt.Println("Murmur " + release.Version)
	cfg := config.Load()
	checks := []protocol.Check{{Name: "Signed in", OK: cfg.Token != "", Detail: cfg.Handle + " on " + cfg.Hub}}
	if !checks[0].OK {
		checks[0].Detail = "no: run murmur join"
	}
	if st, err := ipc.Settled(); err != nil {
		checks = append(checks, protocol.Check{Name: "Daemon", Detail: err.Error()})
	} else if st.State == "online" {
		checks = append(checks, protocol.Check{Name: "Daemon", OK: true, Detail: st.State + ", " + st.Text})
	} else {
		checks = append(checks, protocol.Check{Name: "Daemon", Detail: st.State + "; see " + config.Tildify(filepath.Join(config.Dir(), "daemon.log"))})
	}
	ok := true
	for _, c := range checks {
		step(c.OK, c.Name, c.Detail)
		ok = ok && c.OK
	}
	found := false
	for _, h := range Harnesses {
		switch {
		case h.Connected():
			ok = steps(h, h.Checks()) && ok
		case h.Installed():
			step(false, h.Title(), "installed, not connected: run murmur join")
			ok = false
		default:
			continue
		}
		found = true
	}
	if !found {
		step(false, "Coding tools", "none found: install one of "+titles()+", then run murmur join")
		ok = false
	}
	return ok
}

// titles lists the harnesses murmur connects, for people.
func titles() string {
	var t []string
	for _, h := range Harnesses {
		t = append(t, h.Title())
	}
	return strings.Join(t[:len(t)-1], ", ") + " or " + t[len(t)-1]
}

// Checks looks at what join set up in each harness, and returns Claude Code's version too. Each failed check says
// how to fix it, and names its harness. The daemon sends these to the hub on the team's schedule, for its admins.
func Checks() (claudeCode string, checks []protocol.Check) {
	for _, h := range Harnesses {
		switch {
		case h.Connected():
			for _, c := range h.Checks() {
				c.Name = h.Title() + ": " + c.Name
				checks = append(checks, c)
			}
		case h.Installed():
			checks = append(checks, protocol.Check{Name: h.Title(), Detail: "installed, not connected: run murmur join"})
		}
	}
	if (claude.Claude{}).Installed() {
		claudeCode = claude.Version()
	}
	return claudeCode, checks
}

// Reapply sets the connected harnesses up again, once, when the daemon starts on a version other than the one that
// last set this laptop up: their hooks and MCP servers become what this version needs, as a fresh join writes them.
// A harness counts as connected when its settings hold murmur's hooks, whichever version wrote them, so a laptop
// that joined with an older version is set up again, and a harness its person disconnected stays as it is. It runs
// only on a laptop that joined a hub, and a development build only where its guards let it (guardDev, harness.Guard).
func Reapply() { reapply(config.Exe()) }

func reapply(exe string) {
	if b, _ := os.ReadFile(setupMarker()); string(b) == release.Version || config.Load().Token == "" || guardDev() != nil {
		return
	}
	var again, failed []string
	for _, h := range Harnesses {
		if !h.Connected() {
			continue
		}
		checks := h.Connect(exe, nil)
		if len(checks) == 1 && checks[0].Name == harness.Skipped {
			continue
		}
		again = append(again, h.Title())
		for _, c := range checks {
			if !c.OK && !notes[c.Name] {
				failed = append(failed, h.Title()+" "+strings.ToLower(c.Name)+": "+c.Detail)
			}
		}
	}
	if len(again) > 0 {
		line := "set up again for " + release.Version + ": " + strings.Join(again, ", ")
		if len(failed) > 0 {
			line += "; failed, tried again at the next start: " + strings.Join(failed, "; ")
		}
		log.Print(line)
	}
	if len(failed) == 0 { // a harness that failed is set up again at the next start
		_ = os.WriteFile(setupMarker(), []byte(release.Version), 0o600)
	}
}

// notes are the checks that say something to the person rather than fail: Claude Code's messages setting, a step
// such as trusting Codex's hooks, Gemini CLI's folder trust, an old version.
var notes = map[string]bool{"Messages": true, "Next": true, "Folders": true, "Version": true}

func Uninstall() error {
	if err := guardDev(); err != nil {
		return err
	}
	cfg := config.Load()
	var failed []string
	for _, h := range Harnesses { // each removes only its own entries, from the tools that hold any
		if !h.Holds() {
			continue
		}
		switch err := h.Disconnect(config.Exe()); {
		case errors.Is(err, harness.ErrDev):
			step(false, h.Title(), err.Error())
		case err != nil: // the rest goes on: sign-out matters more than one file
			step(false, h.Title(), err.Error())
			failed = append(failed, h.Title())
		default:
			step(true, h.Title(), "murmur's hooks and MCP server removed")
		}
	}
	if c, err := ipc.Dial(); err == nil {
		_ = json.NewEncoder(c).Encode(ipc.Request{Op: "stop"})
		c.Close()
	}
	if cfg.Token != "" {
		req, _ := http.NewRequest(http.MethodPost, cfg.Hub+"/api/revoke", nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		if resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err == nil {
			resp.Body.Close()
		}
		step(true, "Laptop", "signed out")
	}
	_ = os.RemoveAll(config.Dir())
	step(true, "Data", "removed "+config.Tildify(config.Dir()))
	fmt.Printf("\nThe binary stays at %s; delete it with: rm -f %s %s\n", config.Exe(), config.Exe(), config.Exe()+".prev")
	if len(failed) > 0 {
		return fmt.Errorf("murmur's entries are still in %s: see above", strings.Join(failed, ", "))
	}
	return nil
}

// hookWait bounds the hooks a harness waits for. Past it they print nothing, and the session or the edit goes on.
const hookWait = 800 * time.Millisecond

// Hook is what a harness runs as `murmur hook <harness> <event>`: when a session starts, before an agent edits a
// file, and when it waits for its person. What it prints reaches the agent: the team brief as the session's context,
// an overlap as the reason an edit is turned back. It never holds a session up: on any failure, and after hookWait,
// it prints nothing.
func Hook(name, event string) {
	hook(Harness(name), event, os.Stdin, os.Stdout)
}

// hook answers one hook on out. Whatever goes wrong inside it, the hook prints at most what the harness reads as no
// objection, and exits 0: a harness may refuse a tool when its hook fails.
func hook(h harness.Harness, event string, stdin io.Reader, out io.Writer) {
	defer func() { _ = recover() }()
	// a tool call's input and output come whole: a file written at once, a command's output, megabytes of it
	data, _ := io.ReadAll(io.LimitReader(stdin, 64<<20))
	var cursor struct {
		Version string `json:"cursor_version"`
	}
	if h != nil && h.Name() == claude.Name && json.Unmarshal(data, &cursor) == nil && cursor.Version != "" {
		// Cursor runs the hooks in ~/.claude/settings.json too, with its own input, which names its version, and its
		// model reads only some of what Claude Code's answers say. Cursor's own murmur hooks speak for its sessions;
		// without them, these do.
		if hookish.Cursor.Connected() {
			return
		}
		h = hookish.Cursor
	}
	ev, ok := hookEvent(h, event, data)
	if !ok {
		return
	}
	req := ipc.Request{Op: "hook", Hook: &ev}
	if ev.Kind == harness.Start || ev.Kind == harness.Prompt || ev.Kind == harness.Needs {
		_ = ipc.EnsureDaemon() // before the wait below starts counting: a tool call never starts the daemon
	}
	if ev.Kind == harness.Needs { // the harness doesn't wait for this one
		_ = ipc.CallRunning(req, nil, 2*time.Second)
		return
	}
	type answer struct {
		b    []byte
		done func(printed bool)
	}
	res := make(chan answer, 1)
	go func() {
		defer func() {
			if recover() != nil {
				res <- answer{b: h.Answer(ev, "")}
			}
		}()
		r, done, err := ipc.Hook(req, hookWait)
		if err != nil {
			res <- answer{b: h.Answer(ev, "")} // what a harness answers without murmur's word
			return
		}
		res <- answer{h.Answer(ev, r.Text), done}
	}()
	select {
	case a := <-res:
		_, err := out.Write(a.b)
		if a.done != nil {
			a.done(err == nil) // the messages it carried leave the daemon only now
		}
	case <-time.After(hookWait):
		_, _ = out.Write(h.Answer(ev, "")) // anything the daemon answers later stays with it
	}
}

// hookEvent reads a hook's input, and says whether it is one murmur acts on.
func hookEvent(h harness.Harness, event string, data []byte) (harness.Event, bool) {
	if h == nil {
		return harness.Event{}, false
	}
	ev, err := h.Parse(event, data)
	if err != nil || ev.Kind == "" || ev.SessionID == "" {
		return ev, false
	}
	if ev.PID == 0 { // how the daemon tells when the session is gone, and which process's session it is
		ev.PID = harness.Process(os.Getppid())
	}
	if ev.Kind == harness.Start {
		for _, k := range terminalVars {
			if v := os.Getenv(k); v != "" {
				if ev.Term == nil {
					ev.Term = map[string]string{}
				}
				ev.Term[k] = v
			}
		}
	}
	return ev, true
}

// terminalVars say where a session runs, for the console's jump to it: its tmux pane, iTerm2 session, WezTerm pane or
// kitty window, and the terminal program. The hook inherits them from the session.
var terminalVars = []string{"TMUX", "TMUX_PANE", "ITERM_SESSION_ID", "WEZTERM_PANE", "KITTY_WINDOW_ID", "KITTY_LISTEN_ON", "TERM_PROGRAM"}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// guardDev keeps a build from source off a developer's own murmur: it would sign in over their laptop's. Developers
// set MURMUR_HOME to a folder of their own (make laptop does). Each harness has the same guard on its own folder (see
// harness.Guard).
func guardDev() error {
	if !release.Dev() || os.Getenv("MURMUR_HOME") != "" {
		return nil
	}
	return errors.New("this is a development build: set MURMUR_HOME, and the folder variable of each coding tool, " +
		"to folders of your own first (make laptop does), or install a release")
}

// terminal is the person's terminal, or nil without one. Their answers come from it, not from stdin: the install
// line pipes the script into sh, so join's stdin is the download.
func terminal() *os.File {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return nil
	}
	return f
}

// askLine asks for a line on the terminal. Without a terminal the answer is empty.
func askLine(question string) string {
	tty := terminal()
	if tty == nil {
		return ""
	}
	defer tty.Close()
	fmt.Print(question)
	answer, _ := bufio.NewReader(tty).ReadString('\n')
	return strings.TrimSpace(answer)
}

// ask asks a yes-or-no question on the terminal. Without a terminal the answer is no.
func ask(question string) bool {
	answer := strings.ToLower(askLine(question + " [y/N] "))
	return answer == "y" || answer == "yes"
}

// phrase is what the hub's code page shows for a sign-in, from its state: the first four bytes of its SHA-256, as
// "1A2B-3C4D". A person who sees the same on the page knows its code is for this sign-in, not one someone else
// started and sent them.
func phrase(state string) string {
	sum := sha256.Sum256([]byte(state))
	p := strings.ToUpper(hex.EncodeToString(sum[:4]))
	return p[:4] + "-" + p[4:]
}

// signInByCode signs in a laptop that no browser can reach: its person opens the link anywhere, signs in, and pastes
// the code the page shows. The code is useless without this laptop's PKCE verifier.
func signInByCode(cfg config.Config) (config.Config, error) {
	verifier, state := protocol.RandomHex(32), protocol.RandomHex(16)
	sum := sha256.Sum256([]byte(verifier))
	login := cfg.Hub + "/cli/login?" + url.Values{"port": {"0"}, "challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"state": {state}, "host": {hostname()}}.Encode()
	fmt.Println("  Open this link in a browser, sign in, and paste the code it shows:")
	fmt.Println("  " + login)
	fmt.Println("  Check that the page shows " + phrase(state) + ".")
	fmt.Print("  Code: ")
	in := io.Reader(os.Stdin)
	if tty := terminal(); tty != nil {
		defer tty.Close()
		in = tty
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return cfg, errors.New("no code read: run murmur join again")
	}
	var got struct{ Token, Email, Handle, Name, Error string }
	status, err := postJSON(cfg.Hub+"/cli/token", map[string]string{"code": strings.TrimSpace(line), "verifier": verifier}, &got)
	switch {
	case err != nil:
		return cfg, fmt.Errorf("cannot reach murmur: %w", err)
	case status != http.StatusOK:
		return cfg, errors.New(got.Error)
	}
	cfg.Token, cfg.Email, cfg.Handle, cfg.Name = got.Token, got.Email, got.Handle, got.Name
	return cfg, config.Save(cfg)
}
