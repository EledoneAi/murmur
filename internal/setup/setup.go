// Package setup connects a laptop: sign-in, Claude Code's hooks and MCP server, and the checks of doctor.
package setup

import (
	"bufio"
	"bytes"
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

	"github.com/EledoneAi/murmur/internal/claudecode"
	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/protocol"
	"github.com/EledoneAi/murmur/internal/release"
)

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
	fmt.Printf("%s %-12s %s\n", mark, label, detail)
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

// Join signs this laptop in to its team's hub and connects Claude Code. hub names the hub the first time, or a new
// one; after that the laptop remembers it. Without one, it asks for it on the terminal.
func Join(hub string, relogin, noBrowser bool) error {
	cfg := config.Load()
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
	exe := config.Exe()
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

	settings := filepath.Join(claudecode.Dir(), "settings.json")
	if err := claudecode.EditSettings(settings, exe, true); err != nil {
		return err
	}
	step(true, "Hooks", "SessionStart, PreToolUse (edits), Notification in "+config.Tildify(settings))
	step(true, "Permissions", "who, send allowed without a prompt")
	if note := claudecode.InboundNote(settings); note != "" {
		step(false, "Messages", note)
		if ask("  Let your sessions take messages from other sessions (crossSessionInbound: accept)?") {
			if err := claudecode.AcceptMessages(settings); err != nil {
				step(false, "Messages", err.Error())
			} else {
				step(true, "Messages", "agents take messages from other sessions")
			}
		}
	}

	if claude, err := exec.LookPath("claude"); err == nil {
		_ = exec.Command(claude, "mcp", "remove", "--scope", "user", "murmur").Run()
		if out, err := exec.Command(claude, "mcp", "add", "--scope", "user", "murmur", "--", exe, "mcp").CombinedOutput(); err != nil {
			step(false, "MCP server", strings.TrimSpace(string(out)))
		} else {
			step(true, "MCP server", "murmur (user scope): who, send")
		}
	} else {
		step(false, "MCP server", "claude not found on PATH; run: claude mcp add --scope user murmur -- "+exe+" mcp")
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
	fmt.Println("\nSessions that were already open receive messages now; restart them to give them the tools.")
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
	if s == "" || strings.Contains(s, "://") {
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
	_, more := Checks()
	ok := true
	for _, c := range append(checks, more...) {
		step(c.OK, c.Name, c.Detail)
		ok = ok && c.OK
	}
	return ok
}

// Checks looks at what join set up in Claude Code, and returns Claude Code's version too. Each failed check says
// how to fix it. The daemon sends these to the hub on the team's schedule, for its admins.
func Checks() (claudeCode string, checks []protocol.Check) {
	check := func(good bool, name, detail, fix string) {
		if !good {
			detail = fix
		}
		checks = append(checks, protocol.Check{Name: name, OK: good, Detail: detail})
	}
	settings := filepath.Join(claudecode.Dir(), "settings.json")
	hooks, perms, _ := claudecode.SetupState(settings)
	check(hooks, "Hooks", "SessionStart, PreToolUse (edits), Notification in "+config.Tildify(settings), "missing: run murmur join")
	check(perms, "Permissions", "who, send allowed", "missing: run murmur join")
	note := claudecode.InboundNote(settings)
	check(note == "", "Messages", "agents take messages from other sessions", note)
	claude, err := exec.LookPath("claude")
	if err != nil {
		check(false, "Claude Code", "", "claude is not on PATH")
		return "", checks
	}
	out, _ := exec.Command(claude, "--version").Output()
	v, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	check(!claudecode.TooOld(v), "Claude Code", v, v+": messages between sessions need "+claudecode.MinVersion+" or later; run claude update")
	check(exec.Command(claude, "mcp", "get", "murmur").Run() == nil, "MCP server", "murmur: who, send", "missing: run murmur join")
	return v, checks
}

func Reapply() {
	if b, _ := os.ReadFile(setupMarker()); release.Version == "dev" || string(b) == release.Version || config.Load().Token == "" {
		return
	}
	exe := config.Exe()
	if err := claudecode.EditSettings(filepath.Join(claudecode.Dir(), "settings.json"), exe, true); err != nil {
		log.Printf("setup for %s: %v", release.Version, err)
		return
	}
	if claude, err := exec.LookPath("claude"); err == nil && exec.Command(claude, "mcp", "get", "murmur").Run() != nil {
		if out, err := exec.Command(claude, "mcp", "add", "--scope", "user", "murmur", "--", exe, "mcp").CombinedOutput(); err != nil {
			log.Printf("setup for %s: mcp add: %s", release.Version, out)
		}
	}
	_ = os.WriteFile(setupMarker(), []byte(release.Version), 0o600)
	log.Printf("setup applied for %s", release.Version)
}

func Uninstall() error {
	if err := guardDev(); err != nil {
		return err
	}
	cfg := config.Load()
	if err := claudecode.EditSettings(filepath.Join(claudecode.Dir(), "settings.json"), config.Exe(), false); err != nil {
		return err
	}
	step(true, "Hooks", "removed")
	if claude, err := exec.LookPath("claude"); err == nil {
		_ = exec.Command(claude, "mcp", "remove", "--scope", "user", "murmur").Run()
		step(true, "MCP server", "removed")
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
	return nil
}

// hookWait bounds the hooks Claude Code waits for. Past it they print nothing, and the session or the edit goes on.
const hookWait = 800 * time.Millisecond

// Hook is what Claude Code runs on SessionStart, before an agent edits a file (PreToolUse), and on Notification.
// What it prints reaches the agent: the team brief as the session's context, an overlap as the reason an edit is
// turned back. It never holds a session up: on any failure, and after hookWait, it prints nothing.
func Hook(event string) {
	_, _ = os.Stdout.Write(hook(event, os.Stdin))
}

func hook(event string, stdin io.Reader) []byte {
	if event == "notify" { // Claude Code doesn't wait for this one
		_ = ipc.Call(hookRequest(event, stdin), nil, 2*time.Second)
		return nil
	}
	out := make(chan []byte, 1)
	go func() { out <- askDaemon(event, stdin) }()
	select {
	case b := <-out:
		return b
	case <-time.After(hookWait):
		return nil
	}
}

// askDaemon hands a hook's input to the daemon and turns its answer into what the hook prints.
func askDaemon(event string, stdin io.Reader) []byte {
	req, r := hookRequest(event, stdin), ipc.Reply{}
	switch event {
	case "start":
		if ipc.Call(req, &r, hookWait) == nil && r.Text != "" {
			return claudecode.StartContext(r.Text)
		}
	case "edit":
		// an edit never starts the daemon: without one, it goes ahead
		if ipc.CallRunning(req, &r, hookWait) == nil && r.Text != "" {
			return claudecode.Deny(r.Text)
		}
	}
	return nil
}

func hookRequest(event string, stdin io.Reader) ipc.Request {
	data, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	// Claude Code documents this variable for hooks: the session's inbox, where murmur hands it messages
	req := ipc.Request{Op: "hook", Event: event, Data: data, Socket: os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")}
	if event == "start" {
		for _, k := range terminalVars {
			if v := os.Getenv(k); v != "" {
				if req.Term == nil {
					req.Term = map[string]string{}
				}
				req.Term[k] = v
			}
		}
	}
	return req
}

// terminalVars say where a session runs, for the console's jump to it. The hook inherits them from the session.
var terminalVars = []string{"TMUX", "TMUX_PANE", "ITERM_SESSION_ID", "WEZTERM_PANE", "KITTY_WINDOW_ID", "KITTY_LISTEN_ON", "TERM_PROGRAM"}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// guardDev keeps a build from source off a developer's own Claude Code: join writes the running binary's path into
// Claude Code's settings, and `go run` builds into a folder Go deletes afterwards. Developers set CLAUDE_CONFIG_DIR
// and MURMUR_HOME to folders of their own (make laptop does), and then anything goes.
func guardDev() error {
	if !release.Dev() || (os.Getenv("CLAUDE_CONFIG_DIR") != "" && os.Getenv("MURMUR_HOME") != "") {
		return nil
	}
	return errors.New("this is a development build: it would point your own Claude Code at it. " +
		"Set CLAUDE_CONFIG_DIR and MURMUR_HOME to other folders first (make laptop does), or install a release")
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
