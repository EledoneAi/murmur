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

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/console"
	"github.com/EledoneAi/murmur/internal/daemon"
	"github.com/EledoneAi/murmur/internal/ipc"
	"github.com/EledoneAi/murmur/internal/mcp"
	"github.com/EledoneAi/murmur/internal/release"
	"github.com/EledoneAi/murmur/internal/setup"
	"github.com/EledoneAi/murmur/internal/update"
)

const usage = `murmur: your team's Claude Code agents, in one console.

  murmur                open the console (--plain: in sentences, for screen readers, with commands)
  murmur demo           open the console on a made-up team, no hub needed
  murmur watch          print what changes in the team, in sentences
  murmur init           sign in and connect Claude Code (once per laptop; --hub URL names your team's hub,
                        --relogin signs in again, --no-browser signs in with a code, as over SSH)
  murmur who            list people and their agents
  murmur send TO TEXT   message an agent (bob/main-1), a person (bob), or every agent in a repo (repo:acme/api)
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
  murmur version        print this laptop's version
  murmur uninstall      undo init and sign this laptop out (asks first; --yes skips the question)
`

// options lists what each command accepts. send, hook, mute and unmute take free text, so they are not checked.
var options = map[string][]string{
	"": nil, "--plain": nil, "demo": nil, "watch": nil, "init": {"--relogin", "--hub", "--no-browser"}, "uninstall": {"--yes", "-y"}, "daemon": nil, "mcp": nil,
	"who": nil, "status": {"--json"}, "doctor": nil, "version": nil, "--version": nil, "update": {"--check", "--auto"},
	"updates": {"team", "ask", "off"}, "notifications": {"on", "off"}, "mutes": nil,
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
	if i := slices.Index(args, "--hub"); cmd == "init" && i > 0 {
		if i+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "murmur init: --hub needs the hub's address\n")
			os.Exit(2)
		}
		hub = args[i+1]
		args = slices.Delete(args, i+1, i+2)
	}
	if bad := unknownOption(cmd, args); bad != "" {
		fmt.Fprintf(os.Stderr, "murmur %s: unknown option %q\n\n%s", cmd, bad, usage)
		os.Exit(2)
	}
	var err error
	switch cmd {
	case "":
		err = console.Run()
	case "--plain":
		err = console.Watch(os.Stdout, os.Stdin)
	case "watch":
		err = console.Watch(os.Stdout, nil)
	case "demo":
		err = console.RunDemo()
	case "init":
		err = setup.Init(hub, slices.Contains(args, "--relogin"), slices.Contains(args, "--no-browser"))
	case "uninstall":
		if !slices.Contains(args, "--yes") && !slices.Contains(args, "-y") && !confirm() {
			fmt.Fprintln(os.Stderr, "murmur: nothing removed")
			os.Exit(1)
		}
		err = setup.Uninstall()
	case "daemon":
		err = daemon.Run()
	case "hook":
		if len(args) > 1 {
			setup.Hook(args[1])
		}
	case "mcp":
		err = mcp.Serve()
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
	fmt.Printf("This removes murmur's hooks and MCP server from Claude Code, signs this laptop out and deletes %s.\nContinue? [y/N] ", config.Dir())
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
