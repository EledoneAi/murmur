// Command murmur is the laptop side of murmur. Run it with no arguments for the console.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/console"
	"github.com/EledoneAi/murmur/v2/internal/daemon"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/mcp"
	"github.com/EledoneAi/murmur/v2/internal/release"
	"github.com/EledoneAi/murmur/v2/internal/setup"
	"github.com/EledoneAi/murmur/v2/internal/update"
)

const usage = `murmur: your team's coding agents, in one console.

  murmur                open the console (--plain: in sentences, for screen readers, with commands)
  murmur demo           open the console on a made-up team, no hub needed
  murmur watch          print what changes in the team, in sentences
  murmur join HUB       join your team's hub (murmur.example.com): sign in and connect the coding tools on this
                        laptop (Claude Code, Codex, Cursor, OpenCode, Copilot CLI, Gemini CLI), once per laptop;
                        --relogin signs in again, --no-browser signs in with a code, as over SSH
  murmur solo           use murmur on your own, with no team and no hub to run: the agents on this laptop see
                        and message each other, whatever coding tool each runs in; join moves it to a team
  murmur who            list people and their agents
  murmur send TO TEXT   message an agent (bob/main-1), a person (bob), or every agent in a repo (repo:acme/api)
  murmur btw AGENT QUESTION
                        ask an agent (bob/main-1, or your own main-1) something by the way: a copy of its
                        conversation answers, and the agent never sees the question
  murmur btw-answers [approve|auto|off]
                        how teammates' questions to your agents are answered: each answer waits for you to share
                        it, answers go on their own, or teammates can't ask
  murmur mute WHO [30m|2h|tomorrow]
                        hold messages for your agents from a person (bob), an agent (bob/main-1), repo:acme/api,
                        branch:feat/x or group:bob/main-1 (that agent and those it talks with); held ones wait in
                        the console
  murmur unmute WHO     let them through again
  murmur mutes          list what you muted
  murmur status         show whether the daemon is connected (--json: everything the console knows)
  murmur doctor         check the setup and say what to fix
  murmur update         update to your team's version now (--check only says whether there is one)
  murmur updates [team|ask|off]
                        how this laptop updates: as the team's admins chose, only when you run update, or never
  murmur notifications [on|off]
                        desktop notifications when someone messages you or a mute holds a message for your agents
  murmur retitles [on|off]
                        a new title for an agent's conversation once its focus moves on, from a copy of it, at
                        your cost
  murmur version        print this laptop's version
  murmur uninstall      undo join and sign this laptop out (asks first; --yes skips the question)
`

// options lists what each command accepts. send, btw, hook, tool, mute and unmute take free text, so they are not
// checked.
var options = map[string][]string{
	"": nil, "--plain": nil, "demo": nil, "watch": nil, "join": {"--relogin", "--no-browser"}, "solo": nil, "uninstall": {"--yes", "-y"}, "daemon": nil, "mcp": harnesses(),
	"who": nil, "status": {"--json"}, "doctor": nil, "version": nil, "--version": nil, "update": {"--check", "--auto"},
	"updates": {"team", "ask", "off"}, "notifications": {"on", "off"}, "retitles": {"on", "off"}, "mutes": nil, "btw-answers": {"approve", "auto", "off"},
}

// harnesses are the names of the coding tools murmur connects: the MCP server is started for one of them.
func harnesses() []string {
	var names []string
	for _, h := range setup.Harnesses {
		names = append(names, h.Name())
	}
	return names
}

// hubArg takes the hub out of join's arguments: the one that isn't an option, wherever it comes.
func hubArg(args []string) (string, []string) {
	if i := slices.IndexFunc(args[1:], func(a string) bool { return !strings.HasPrefix(a, "-") }); i >= 0 {
		return args[i+1], slices.Delete(slices.Clone(args), i+1, i+2)
	}
	return "", args
}

// unknownOption is the first option a command doesn't take, or "". Commands that take free text aren't checked.
func unknownOption(cmd string, args []string) string {
	allowed, ok := options[cmd]
	if !ok || len(args) < 2 {
		return ""
	}
	for _, a := range args[1:] {
		if !slices.Contains(allowed, a) {
			return a
		}
	}
	return ""
}

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") { // never run a command just to show its help
		fmt.Print(usage)
		return
	}
	hub := ""
	if cmd == "join" {
		hub, args = hubArg(args)
	}
	if bad := unknownOption(cmd, args); bad != "" {
		fmt.Fprintf(os.Stderr, "murmur %s: unknown option %q\n\n%s", cmd, bad, usage)
		os.Exit(2)
	}
	var err error
	switch cmd {
	case "":
		if cfg := config.Load(); cfg.Token == "" && !cfg.Solo { // not in a team yet: join one first, asking for its hub
			if err = setup.Join("", false, false); err != nil {
				break
			}
		}
		err = console.Run()
	case "--plain":
		err = console.Watch(os.Stdout, os.Stdin)
	case "watch":
		err = console.Watch(os.Stdout, nil)
	case "demo":
		err = console.RunDemo()
	case "join":
		err = setup.Join(hub, slices.Contains(args, "--relogin"), slices.Contains(args, "--no-browser"))
	case "solo":
		err = setup.Solo()
	case "uninstall":
		if !slices.Contains(args, "--yes") && !slices.Contains(args, "-y") && !confirm() {
			fmt.Fprintln(os.Stderr, "murmur: nothing removed")
			os.Exit(1)
		}
		err = setup.Uninstall()
	case "daemon":
		err = daemon.Run()
	case "hook":
		hook(args)
	case "mcp":
		if inCopy() { // no MCP server of murmur's speaks for a copy
			break
		}
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		err = mcp.Serve(name)
	case "tool":
		if len(args) > 2 && !inCopy() {
			mcp.Tool(args[1], args[2], os.Stdin)
		}
	case "who":
		var text string
		var online bool
		if text, online, err = ipc.Who(); err == nil {
			fmt.Println(text)
			if !online {
				os.Exit(1)
			}
		}
	case "send":
		if len(args) < 3 {
			err = fmt.Errorf("usage: murmur send TO TEXT")
			break
		}
		var note string
		note, err = ipc.Send(args[1], strings.Join(args[2:], " "))
		if err == nil {
			fmt.Println("Sent to " + args[1] + ". " + note)
		}
	case "btw":
		var to, question, answer, note string
		if to, question, err = btwArgs(args); err != nil {
			break
		}
		if answer, note, err = ipc.Btw(to, question); err == nil {
			fmt.Println(btwSays(to, answer, note))
		}
	case "btw-answers":
		err = setBtwAnswers(args[1:])
	case "status":
		if slices.Contains(args, "--json") {
			err = console.Status(os.Stdout)
			break
		}
		var text string
		var online bool
		if text, online, err = ipc.Status(); err == nil {
			fmt.Println(text)
			if !online {
				os.Exit(1)
			}
		}
	case "doctor":
		if !setup.Doctor() {
			os.Exit(1)
		}
	case "mute", "unmute":
		if len(args) < 2 || len(args) > 3 {
			err = fmt.Errorf("usage: murmur %s WHO [30m|2h|tomorrow]", cmd)
			break
		}
		var r ipc.Reply
		req := ipc.Request{Op: cmd, To: args[1]}
		if len(args) == 3 {
			req.Text = args[2]
		}
		if err = ipc.Call(req, &r, 5*time.Second); err == nil {
			if !r.OK {
				err = errors.New(r.Err)
			} else {
				fmt.Println(strings.ToUpper(r.Text[:1]) + r.Text[1:] + ".")
			}
		}
	case "mutes":
		var snap ipc.Snapshot
		if err = ipc.Call(ipc.Request{Op: "snapshot"}, &snap, 5*time.Second); err == nil {
			if len(snap.Mutes) == 0 {
				fmt.Println("Nothing muted.")
			}
			for _, m := range snap.Mutes {
				fmt.Println(m)
			}
		}
	case "update":
		err = runUpdate(slices.Contains(args, "--check"), slices.Contains(args, "--auto"))
	case "updates":
		err = setUpdates(args[1:])
	case "notifications":
		err = setNotifications(args[1:])
	case "retitles":
		err = setRetitles(args[1:])
	case "version", "--version":
		fmt.Println("murmur", release.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "murmur: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "murmur:", err)
		os.Exit(1)
	}
}

// confirm asks before uninstalling. Without a terminal to ask on, the answer is no: scripts pass --yes.
func confirm() bool {
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintln(os.Stderr, "murmur: uninstall needs a yes; run murmur uninstall --yes")
		return false
	}
	fmt.Printf("This removes murmur's hooks and MCP server from your coding tools, signs this laptop out and deletes %s.\nContinue? [y/N] ", config.Dir())
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func runUpdate(check, auto bool) error {
	cfg := config.Load()
	if !check {
		return update.Run(cfg, auto, func(s string) { fmt.Println(s) })
	}
	if cfg.Hub == "" {
		return config.ErrNoHub
	}
	info, err := update.Check(cfg.Hub)
	if err != nil {
		return err
	}
	switch {
	case update.Newer(info.Version):
		fmt.Printf("Murmur %s is available (this laptop runs %s): run murmur update\n", info.Version, release.Version)
	default:
		fmt.Printf("Murmur %s is up to date (the team runs %s)\n", release.Version, info.Version)
	}
	return nil
}

// setUpdates shows or changes how this laptop updates.
func setUpdates(args []string) error {
	cfg := config.Load()
	if len(args) == 1 {
		cfg.Updates = map[string]string{"team": "", "ask": "ask", "off": "off"}[args[0]]
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	fmt.Println(map[string]string{
		"":    "This laptop updates as the team's admins chose.",
		"ask": "This laptop updates only when you run murmur update.",
		"off": "This laptop never updates on its own. If the hub stops accepting its version, run murmur update.",
	}[cfg.Updates])
	return nil
}

// setRetitles shows or changes whether copies of this laptop's agents' conversations title them again.
func setRetitles(args []string) error {
	cfg := config.Load()
	if len(args) == 1 {
		cfg.Retitles = map[string]string{"on": "", "off": "off"}[args[0]]
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	if cfg.Retitles == "off" {
		fmt.Println("Your agents keep the titles their coding tool or you gave their conversations.")
	} else {
		fmt.Println("When an agent's focus moves on, a copy of its conversation gives it a new title, at your cost: a few cents a check, every 10 minutes at most.")
	}
	return nil
}

// setNotifications shows or changes whether this laptop shows desktop notifications.
func setNotifications(args []string) error {
	cfg := config.Load()
	if len(args) == 1 {
		cfg.Notifications = map[string]string{"on": "", "off": "off"}[args[0]]
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	if cfg.Notifications == "off" {
		fmt.Println("This laptop shows no desktop notifications. Messages wait in the console (murmur).")
	} else {
		fmt.Println("This laptop shows a desktop notification when someone messages you, and when a mute holds a message for your agents.")
	}
	return nil
}

// hookFor answers a coding tool's hook; tests replace it.
var hookFor = setup.Hook

// inCopy says this process runs for a copy of a session, which murmur runs to answer a question by the way: no
// session of murmur's, whatever settings it loaded. Its hooks, MCP server and tools do nothing.
func inCopy() bool { return os.Getenv("MURMUR_BTW") != "" }

// hook answers a coding tool's hook, except a copy's.
func hook(args []string) {
	switch {
	case inCopy():
	case len(args) > 2:
		hookFor(args[1], args[2])
	default:
		// an older version's hook, which names no harness: it answers nothing, but this version's daemon, once it
		// runs, sets the laptop's hooks up again
		_ = ipc.EnsureDaemon()
	}
}

// btwArgs reads murmur btw AGENT QUESTION: the question may come in one argument or several.
func btwArgs(args []string) (to, question string, err error) {
	if len(args) < 3 || strings.TrimSpace(args[1]) == "" || strings.TrimSpace(strings.Join(args[2:], "")) == "" {
		return "", "", errors.New("usage: murmur btw AGENT QUESTION, like murmur btw bob/main-1 what are you stuck on?")
	}
	return args[1], strings.Join(args[2:], " "), nil
}

// btwSays is what murmur btw prints: the answer, or where the question stands when the answer is late.
func btwSays(to, answer, note string) string {
	if answer != "" {
		return answer
	}
	return fmt.Sprintf("No answer from %s yet (%s). The console (murmur) shows it when it comes.", to, note)
}

// btwAnswersSaid says what each setting for teammates' questions by the way means.
var btwAnswersSaid = map[string]string{
	"approve": "Teammates can ask your agents by the way. Each answer waits for you to share it, in the console.",
	"auto": "Teammates can ask your agents by the way, and the answers go to them on their own. A question to an agent " +
		"idle for over an hour still waits for you.",
	"off": "Teammates can't ask your agents by the way. You still can.",
}

// setBtwAnswers shows or changes how this laptop answers teammates' questions by the way.
func setBtwAnswers(args []string) error {
	setting := config.Load().BtwAnswers
	if len(args) == 1 {
		if err := ipc.SetBtwAnswers(args[0]); err != nil {
			return err
		}
		setting = args[0]
	}
	if setting == "" {
		setting = "approve"
	}
	fmt.Println(btwAnswersSaid[setting])
	return nil
}
