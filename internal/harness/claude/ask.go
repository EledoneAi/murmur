package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/protocol"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// A question by the way is answered by a copy of the session: Claude Code resumes the conversation in a fork it never
// writes down, in the session's folder, with the session's own claude and sign-in, and blocks every tool the copy
// tries. The copy reads the conversation from the prompt cache the session keeps warm, and the session never sees it.

// maxTurns bounds the copy, for --max-turns: it answers in one turn, or in two when it tries a tool and the hook turns
// it back. Each turn reads the whole conversation again.
const maxTurns = "4"

// budget is the copy's spending cap, in US dollars, for --max-budget-usd, for a conversation of tokens. Claude Code
// checks the cap after a turn it has paid for, and then gives no answer: a cap below what one turn costs would throw
// away the turn that answers, and a cold copy of a large conversation costs more than a few dollars in one turn. So
// the cap only stops a runaway copy, past what maxTurns allows: one full cache write at 25 USD a million tokens (above
// any model's price today: 8 on Opus 5.5, 20 on Fable 5.1), plus four reads at a tenth of that, makes 35 USD a
// million tokens. 200k tokens get 7 USD; a small conversation gets the floor, 5 USD; one whose size isn't known gets
// what a million tokens would, 35 USD.
func budget(tokens int) string {
	const perMillion, floor = 25 + 4*2.5, 5.0
	if tokens <= 0 {
		tokens = 1_000_000
	}
	return strconv.FormatFloat(max(floor, perMillion*float64(tokens)/1e6), 'f', 2, 64)
}

// denyAll is the hook that turns back every tool call the copy tries, whatever the permission rules allow: a
// PreToolUse hook that exits with 2 refuses the call. Taking the tools away (--tools "") or denying them by rule would
// change what the copy sends and write the whole conversation to the cache again, at 15 to 25 times the price, and the
// copy would fake its tool calls in text. disableAllHooks would turn this hook off too. Each time it runs, it adds a
// line to the file tried, which Ask counts: a copy that used a tool the hook never saw used it for real.
func denyAll(tried string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{
		map[string]any{"type": "command", "command": "echo >> " + shellQuote(tried) + "; exit 2"}}}}})
	return b
}

// shellQuote quotes s for a POSIX shell, as one word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// errToolRan is why a copy's answer is dropped when it used a tool the hook didn't turn back: what the tool read
// may be in the answer.
var errToolRan = errors.New("the copy used a tool murmur's hook never turned back, so its answer was dropped: " +
	"your organisation's Claude Code settings may turn hooks off")

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errNoClaude says what to do when no claude can run the copy.
var errNoClaude = errors.New("can't find claude, Claude Code's command: put it on your PATH, or install it with " +
	"Claude Code's installer, which puts it in ~/.local/bin")

// AskVersion is the oldest Claude Code whose sessions murmur copies to answer questions by the way: the oldest a copy
// was checked with. A copy needs --fork-session with --session-id, --no-session-persistence, --setting-sources "",
// --permission-prompts, --max-turns, PreToolUse hooks in --settings, and the endings --resume writes for a turn it
// finds cut short ("[Tool call interrupted…]", "No response requested."), which the copy's prompt explains. Each night,
// make compat runs a copy with the newest Claude Code.
const AskVersion = "2.1.292"

// errOlder is why a session of an older Claude Code isn't copied.
var errOlder = errors.New("its Claude Code is older than " + AskVersion + "; update Claude Code")

// CanCopy says why session s isn't copied: its Claude Code is older than AskVersion, or doesn't say its version.
func (Claude) CanCopy(s harness.Session) error {
	if release.Older(s.Version, AskVersion) {
		return errOlder
	}
	return nil
}

// Ask answers a question by the way from a copy of session s. The copy runs as long as ctx allows: when ctx is done,
// it is killed with everything it started, and once it is done, so is what it started and left running. A session
// whose Claude Code is older than AskVersion isn't copied; the daemon asks CanCopy first, which refuses one that
// doesn't say its version too.
func (Claude) Ask(ctx context.Context, s harness.Session, q harness.Question) (harness.Answer, error) {
	if !uuidPattern.MatchString(s.ID) || !uuidPattern.MatchString(q.Fork) {
		return harness.Answer{}, fmt.Errorf("can't copy session %q under %q: both must be UUIDs", s.ID, q.Fork)
	}
	if s.Version != "" && release.Older(s.Version, AskVersion) {
		return harness.Answer{}, errOlder
	}
	if hooksOff(0) {
		return harness.Answer{}, errHooksOff
	}
	// the session's own process gives the claude and the provider settings, while it is still the session's: a PID the
	// system gave another process since names a process that started at another time, and nothing is taken from it
	var command, session []string
	known := false
	if s.PID > 0 && s.Started != "" && harness.StartTime(s.PID) == s.Started {
		command = sessionClaude(s.PID)
		session, known = harness.Environ(s.PID, provider)
	}
	if command == nil {
		command = installedClaude()
	}
	if command == nil {
		return harness.Answer{}, errNoClaude
	}
	mine := userSettings()
	env := copyEnv(os.Environ(), session, known)
	tried := filepath.Join(config.Dir(), "btw-"+q.Fork+".tried")
	settingsFile, err := writeSettings(q.Fork, copySettings(mine, tried))
	if err != nil {
		return harness.Answer{}, err
	}
	defer os.Remove(settingsFile)
	defer os.Remove(tried)

	args := append(slices.Clone(command[1:]), "-p", "--resume", s.ID, "--fork-session", "--session-id", q.Fork,
		"--no-session-persistence", "--setting-sources", "", "--settings", settingsFile,
		"--permission-mode", "dontAsk", "--permission-prompts", "none", "--output-format", "json",
		"--max-turns", maxTurns, "--max-budget-usd", budget(q.Tokens))
	cmd := exec.CommandContext(ctx, command[0], args...)
	cmd.Dir, cmd.Env = s.Cwd, env
	cmd.Stdin = strings.NewReader(framed(q))
	stdout, stderr := &head{max: 1 << 20}, &head{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// the copy may start processes of its own: they go with it, when it is stopped and when it is done
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err = cmd.Run()
	took := time.Since(start)
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Claude Code may leave an empty folder for the copy's environment: Remove takes only an empty one
	_ = os.Remove(filepath.Join(Dir(), "session-env", q.Fork))
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return harness.Answer{Took: took}, fmt.Errorf("the copy took too long to answer: %w", ctx.Err())
	case err != nil && ctx.Err() != nil:
		return harness.Answer{Took: took}, fmt.Errorf("the copy was stopped: %w", ctx.Err())
	case errors.Is(err, exec.ErrWaitDelay):
		err = nil // the copy is done, and something it started still holds its output: what it printed is all there
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return harness.Answer{Took: took}, fmt.Errorf("can't run %s: %w", command[0], err)
	}
	code := 0
	if exit != nil {
		code = exit.ExitCode()
	}
	b, _ := os.ReadFile(tried)
	a, err := result(stdout.b, stderr.b, code, cloud(env, mine.Env), bytes.Count(b, []byte("\n")))
	a.Took = took
	return a, err
}

// framed is the copy's prompt: what it is, then the question. Claude Code ignores --append-system-prompt on --resume,
// so the copy learns it from its user turn. A copy of a busy session finds its last turn cut short by Claude Code, with
// a tool call "interrupted" or a reply "not requested": the copy hears what the session is doing meanwhile, so it
// neither takes that for something that happened nor, asked what it is doing, describes it. The prompt starts with
// murmur's words, where Claude Code would read a command (/, !, #), and what others wrote in it mentions no file
// (unmentioned).
func framed(q harness.Question) string {
	now := ""
	if q.State != "" {
		now = " Right now it is " + unmentioned(q.State) + "."
	}
	return "By the way: " + unmentioned(q.From) + " asks you a question through murmur. You are a throwaway copy of this " +
		"conversation: the session you were copied from never sees this, and goes on meanwhile." + now + " If the " +
		"conversation ends in a \"[Tool call interrupted…]\" result or \"No response requested.\", that comes from making " +
		"this copy, not from what happened: that work is still going on. Asked what you are doing, describe what it is " +
		"doing right now (such as the reply it is still writing to the last message), not how this copy ends. Tools are " +
		"off here: answer in a few sentences from what you already know, and don't quote code, file contents or " +
		"secrets.\n\nQuestion: " + unmentioned(q.Text)
}

// unmentioned keeps Claude Code from attaching a file a prompt mentions: it reads "@" followed by a path, anywhere in
// the prompt, before any tool runs, so the hook that turns tools back never sees it. An "@" followed by a space
// mentions nothing.
func unmentioned(s string) string { return strings.ReplaceAll(s, "@", "@ ") }

// ---------------------------------------------------------------- the claude that runs the copy

// sessionClaude is the command that runs the claude a session's process runs, pid, once Ask knows the process is still
// the session's; nil when it can't be told. The copy runs the session's own version: the desktop app and the editors
// bundle a claude of their own, and a daemon the desktop app started has no claude on its PATH. Tests replace it.
var sessionClaude = func(pid int) []string {
	return commandOf(harness.Executable(pid), harness.Args(pid))
}

// commandOf is the command that runs exe again as the program it ran with argv: exe itself, or, when exe is a
// JavaScript runtime, as for Claude Code installed with npm, exe with the script it ran. It is nil for a runtime whose
// script argv doesn't name by its absolute path: one that set its process title over its arguments, as Node does on
// Linux.
func commandOf(exe string, argv []string) []string {
	if exe == "" {
		return nil
	}
	command := []string{exe}
	switch filepath.Base(exe) {
	case "node", "nodejs", "bun":
		i := 1
		for i < len(argv) && strings.HasPrefix(argv[i], "-") { // the runtime's own options
			i++
		}
		if i >= len(argv) || !filepath.IsAbs(argv[i]) {
			return nil
		}
		command = append(command, argv[i])
	}
	return command
}

// installs are where Claude Code's installers put claude, after the PATH. Tests replace them.
var installs = func() []string {
	home := config.Home()
	return []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude", filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude")}
}

// installedClaude is the claude a copy runs when its session's process can't say which it runs. Tests replace it.
var installedClaude = findClaude

// findClaude is the claude on the PATH, or else where an installer puts it.
func findClaude() []string {
	if p, err := exec.LookPath("claude"); err == nil {
		return []string{p}
	}
	for _, p := range installs() {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return []string{p}
		}
	}
	return nil
}

// ---------------------------------------------------------------- what the copy signs in with

// providerNames, with providerPrefixes, are the variables that say where and how Claude Code signs in: the provider,
// keys and tokens, the cloud's region, profile and project, the proxy and its certificates. They are all the copy takes
// from the session's environment and from the env of its person's settings: a session's environment also holds its
// own messaging token.
var providerNames = func() map[string]bool {
	m := map[string]bool{"CLAUDE_CODE_SKIP_AWS_CRED_CACHE": true, "CLAUDE_CODE_OAUTH_TOKEN": true}
	for _, p := range []string{"BEDROCK", "VERTEX", "FOUNDRY", "MANTLE", "ANTHROPIC_AWS", "ANTHROPIC_GOOGLE_CLOUD"} {
		m["CLAUDE_CODE_USE_"+p], m["CLAUDE_CODE_SKIP_"+p+"_AUTH"] = true, true
	}
	for _, n := range []string{"CLAUDE_CODE_USE_GATEWAY",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_BEARER_TOKEN_BEDROCK", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
		"CLOUD_ML_REGION", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT", "CLOUDSDK_CONFIG",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "NODE_EXTRA_CA_CERTS"} {
		m[n] = true
	}
	return m
}()

var providerPrefixes = []string{"ANTHROPIC_BEDROCK_", "ANTHROPIC_VERTEX_", "ANTHROPIC_FOUNDRY_", "VERTEX_REGION_"}

// provider says whether a variable is one the copy takes from its session (providerNames).
func provider(name string) bool {
	if providerNames[name] {
		return true
	}
	for _, p := range providerPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return strings.HasPrefix(name, "ANTHROPIC_DEFAULT_") && strings.HasSuffix(name, "_MODEL")
}

// network says whether a provider variable is one of the network's: how to reach the provider, not who signs in.
func network(name string) bool {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS":
		return true
	}
	return false
}

// sessionVar says whether a variable is one Claude Code sets for the commands a session runs, which a daemon one of
// its hooks started inherits: the copy would take itself for a command of that session, and get its messaging token.
func sessionVar(name string) bool {
	switch name {
	case "CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT", "CLAUDE_PROJECT_DIR", "CLAUDE_ENV_FILE", "AI_AGENT":
		return true
	}
	rest, ok := strings.CutPrefix(name, "CLAUDE_CODE_")
	return ok && (rest == "ENTRYPOINT" || rest == "EXECPATH" || rest == "SSE_PORT" || strings.HasPrefix(rest, "MESSAGING_") ||
		strings.Contains(rest, "SESSION"))
}

// copyEnv is the copy's environment: the daemon's own, without what a session set for its commands, with the provider
// variables of the session's environment when it could be read (known), in place of the daemon's: the copy signs in as
// the session does. The daemon's own came from whichever session's hook started it: when the session's can't be read,
// the copy keeps none of them but the network's (its proxy, its certificates), so it never signs in with another
// session's key, and signs in as Claude Code would with none (the keychain, its person's settings, managed settings).
// Claude Code's keychain sign-in needs USER. MURMUR_BTW tells murmur's hooks and MCP server, should any run, that they
// run in a copy. CLAUDE_CONFIG_DIR stays the daemon's: it is where the daemon found the session.
func copyEnv(daemon, session []string, known bool) []string {
	var env []string
	has := map[string]bool{}
	for _, kv := range daemon {
		name, value, _ := strings.Cut(kv, "=")
		if sessionVar(name) || provider(name) && (known || !network(name)) || name == "MURMUR_BTW" {
			continue
		}
		env = append(env, kv)
		has[name] = has[name] || value != ""
	}
	env = append(env, session...)
	if !has["USER"] || !has["LOGNAME"] {
		if u, err := user.Current(); err == nil && u.Username != "" {
			if !has["USER"] {
				env = append(env, "USER="+u.Username)
			}
			if !has["LOGNAME"] {
				env = append(env, "LOGNAME="+u.Username)
			}
		}
	}
	return append(env, "MURMUR_BTW=1")
}

// settingsOfMine is what the copy takes from its person's settings.json: --setting-sources "" keeps the copy from
// reading it, with its hooks, permissions and MCP servers, and these are how Claude Code signs in. Never
// awsAuthRefresh or gcpAuthRefresh, which open a browser to sign in, and never the project's settings: a command of a
// repo's, run through --settings, would skip Claude Code's question whether to trust its folder.
type settingsOfMine struct {
	Env                 map[string]json.RawMessage
	APIKeyHelper        json.RawMessage
	AWSCredentialExport json.RawMessage
}

func userSettings() settingsOfMine {
	var s settingsOfMine
	var raw map[string]json.RawMessage
	b, err := os.ReadFile(settings())
	if err != nil || json.Unmarshal(b, &raw) != nil {
		return s
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(raw["env"], &env) == nil {
		for name, v := range env {
			if provider(name) {
				if s.Env == nil {
					s.Env = map[string]json.RawMessage{}
				}
				s.Env[name] = v
			}
		}
	}
	s.APIKeyHelper, s.AWSCredentialExport = setting(raw["apiKeyHelper"]), setting(raw["awsCredentialExport"])
	return s
}

// setting is a value of settings.json, or nil when it is missing or null.
func setting(v json.RawMessage) json.RawMessage {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}

// writeSettings writes the copy's --settings to a file of murmur's own, which only this user can read, for the copy
// fork, and returns its path: on the command line, the keys of its person's settings would show to every user of the
// laptop. The caller removes it once the copy is done. One a daemon that stopped meanwhile left is removed as the next
// daemon starts (SweepCopies), or, when its copy still ran then, with the first copy's an hour later, and so is what its
// hook wrote.
func writeSettings(fork string, settings []byte) (string, error) {
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("can't write the copy's settings: %w", err)
	}
	sweep(func(_ string, at time.Time) bool { return time.Since(at) > time.Hour })
	path := filepath.Join(dir, "btw-"+fork+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("can't write the copy's settings: %w", err)
	}
	_, err = f.Write(settings)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("can't write the copy's settings: %w", err)
	}
	return path, nil
}

// SweepCopies removes the settings of the copies that no longer run, and what their hook wrote: as the daemon starts,
// those a daemon that stopped before its copies were done left, sign-in included. A copy that still runs (one a daemon
// killed outright left) is a session in Claude Code's registry, and keeps its own until it is done.
func (Claude) SweepCopies() {
	running := map[string]bool{}
	for _, s := range (Claude{}).Sessions() {
		running[s.ID] = true
	}
	sweep(func(fork string, _ time.Time) bool { return !running[fork] })
}

// sweep removes the files copies leave in murmur's folder, their settings and their hook's count, that left says are
// left over, from the copy's fork and when the file was last written.
func sweep(left func(fork string, at time.Time) bool) {
	dir := config.Dir()
	settings, _ := filepath.Glob(filepath.Join(dir, "btw-*.json"))
	tried, _ := filepath.Glob(filepath.Join(dir, "btw-*.tried"))
	for _, p := range append(settings, tried...) {
		fork, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(p), "btw-"), ".")
		if st, err := os.Stat(p); err == nil && left(fork, st.ModTime()) {
			_ = os.Remove(p)
		}
	}
}

// copySettings is the copy's --settings: the hook that turns every tool call back, counting them in tried, and what
// it takes from its person's settings.
func copySettings(mine settingsOfMine, tried string) []byte {
	s := map[string]any{"hooks": denyAll(tried)}
	if len(mine.Env) > 0 {
		s["env"] = mine.Env
	}
	if mine.APIKeyHelper != nil {
		s["apiKeyHelper"] = mine.APIKeyHelper
	}
	if mine.AWSCredentialExport != nil {
		s["awsCredentialExport"] = mine.AWSCredentialExport
	}
	b, _ := json.Marshal(s)
	return b
}

// The clouds the copy may sign in with, as its environment says. The model's id doesn't tell: Vertex's are bare.
const (
	cloudAWS    = "aws"
	cloudGoogle = "google"
)

// cloud is the cloud the copy signs in with: env, then the env of its person's settings, which Claude Code applies on
// top; "" for Anthropic's own sign-in.
func cloud(env []string, settingsEnv map[string]json.RawMessage) string {
	vars := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		vars[name] = value
	}
	for name, v := range settingsEnv {
		var s string
		if json.Unmarshal(v, &s) == nil {
			vars[name] = s
		}
	}
	on := func(name string) bool {
		switch strings.ToLower(strings.TrimSpace(vars[name])) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	switch {
	case on("CLAUDE_CODE_USE_BEDROCK") || on("CLAUDE_CODE_USE_ANTHROPIC_AWS"):
		return cloudAWS
	case on("CLAUDE_CODE_USE_VERTEX") || on("CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD"):
		return cloudGoogle
	}
	return ""
}

// ---------------------------------------------------------------- what the copy said

// result reads what claude -p --output-format json printed, with its exit code, into the answer, or into an error that
// says what went wrong and, when the copy couldn't sign in, what its person can do. turnedBack is how many tool calls
// the hook turned back. Each turn of the copy's after its first follows a tool call: a copy with more turns than that
// used a tool the hook never saw, and its answer is dropped. Claude Code's permission_denials doesn't tell: it leaves
// out the calls a hook refuses.
func result(stdout, stderr []byte, code int, via string, turnedBack int) (harness.Answer, error) {
	var r struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
		Turns   int    `json:"num_turns"`
		// total_cost_usd and modelUsage aren't the copy's: on a resumed conversation they count the session's too
		Usage struct {
			Input         int `json:"input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	parsed := json.Unmarshal(lastLine(stdout), &r) == nil
	a := harness.Answer{Tokens: max(r.Usage.Input+r.Usage.CacheCreation+r.Usage.CacheRead, 0)}
	if parsed && !r.IsError && code == 0 {
		if r.Turns > turnedBack+1 {
			return a, errToolRan
		}
		if a.Text = answerText(r.Result); a.Text == "" {
			return a, errors.New("the copy answered nothing")
		}
		return a, nil
	}
	switch r.Subtype {
	case "error_max_budget_usd":
		return a, errors.New("the copy reached its spending cap before it answered")
	case "error_max_turns":
		return a, errors.New("the copy kept trying tools, and stopped before it answered")
	}
	if err := signIn(r.Result+"\n"+string(stderr), via); err != nil {
		return a, err
	}
	switch {
	case parsed && r.IsError:
		why := firstLine(r.Result)
		if why == "" {
			why = firstLine(string(stderr))
		}
		if why == "" {
			why = r.Subtype
		}
		return a, fmt.Errorf("the copy failed: %s", why)
	case code != 0:
		why := firstLine(string(stderr))
		if why == "" {
			why = "no message"
		}
		if code < 0 {
			return a, fmt.Errorf("claude was killed: %s", why)
		}
		return a, fmt.Errorf("claude exited with status %d: %s", code, why)
	}
	return a, errors.New("the copy's answer can't be read")
}

// lastLine is the last line of what claude printed: its JSON follows whatever it may have printed before.
func lastLine(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return b[i+1:]
	}
	return b
}

// answerText is the copy's answer as it goes out: clean, and cut to what a question's answer may hold.
func answerText(s string) string {
	s = strings.TrimSpace(protocol.Clean(s, true))
	return strings.TrimSpace(cutRunes(s, protocol.MaxBtwAnswer))
}

// cutRunes cuts s to n characters.
func cutRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// firstLine is the first line of s that holds anything, clean and cut short.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			return cutRunes(protocol.Clean(line, false), 200)
		}
	}
	return ""
}

// The ways a copy fails to sign in, as Claude Code, the AWS SDK and Google's auth library say them.
var (
	notLoggedIn = regexp.MustCompile(`(?i)not logged in|run /login|invalid api key`)
	awsSignIn   = regexp.MustCompile(`(?i)expiredtoken|security token included in the request is (expired|invalid)|` +
		`unrecognizedclientexception|sso session .{0,40}(expired|invalid)|token .{0,20}expired.{0,20}refresh failed|` +
		`could not load credentials from any providers|credentialsprovidererror|aws sso login`)
	googleSignIn = regexp.MustCompile(`(?i)could not load the default credentials|application.default credentials|` +
		`reauthentication is (needed|required)|gcloud auth`)
	authFailed = regexp.MustCompile(`(?i)\b40[13]\b|authentication_error|permission_error|unauthori[sz]ed|forbidden|` +
		`(oauth|access|bearer) token .{0,20}(expired|revoked|invalid)`)
)

// What a person does when the copy of their agent couldn't sign in.
const (
	fixLogin  = "log in to Claude Code (/login in a session)"
	fixAWS    = "run `aws sso login` (or refresh your AWS credentials)"
	fixGoogle = "run `gcloud auth application-default login`"
)

// signIn is the error of a copy that couldn't sign in, from what it printed, or nil when it failed otherwise. A failure
// that doesn't say which credentials failed is taken for those of the cloud the copy signs in with, via.
func signIn(text, via string) error {
	fix := ""
	switch {
	case notLoggedIn.MatchString(text):
		fix = fixLogin
	case awsSignIn.MatchString(text):
		fix = fixAWS
	case googleSignIn.MatchString(text):
		fix = fixGoogle
	case !authFailed.MatchString(text):
		return nil
	case via == cloudAWS:
		fix = fixAWS
	case via == cloudGoogle:
		fix = fixGoogle
	default:
		fix = fixLogin
	}
	return fmt.Errorf("%w: %s", harness.ErrSignIn, fix)
}

// head keeps the first max bytes written to it and drops the rest: a copy that prints too much can't fill memory.
type head struct {
	b   []byte
	max int
}

func (h *head) Write(p []byte) (int, error) {
	h.b = append(h.b, p[:min(len(p), h.max-len(h.b))]...)
	return len(p), nil
}
