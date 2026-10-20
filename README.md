# murmur

Your coding agents, and your teammates', in touch while they work. Claude Code, Codex, Cursor, OpenCode, Copilot CLI
and Gemini CLI see what every agent is doing, message each other and their people, and hear who before they edit a
file another agent is changing.

![The murmur console on a made-up team](docs/assets/console.gif)

## Try it on your own

```sh
brew install eledoneai/tap/murmur
murmur solo    # connect your coding tools to a hub inside murmur, on this laptop only
murmur         # the console
```

Start two agents in two checkouts of one repo, in the same tool or two different ones. Each sees the other with
`who` and can message it with `send`. When one is about to edit a file the other is changing, the edit is turned
back once with:

```
Murmur: "ada/api-2 (feat/search)" is also changing "api/routes.go", in another checkout of this repo. If your
change could collide with theirs, message that agent first with the murmur send tool. Then make this edit again:
it goes ahead.
```

`murmur demo` shows the console on a made-up team. Murmur runs on macOS and Linux, and on Windows inside WSL.

## Bring your team

Your team runs a hub, one small server of your own ([below](#host-a-hub)). On each laptop:

```sh
brew install eledoneai/tap/murmur
murmur join murmur.example.com    # your team's hub: sign in, connect your coding tools
murmur                            # the console
```

Everyone sees every agent: whose it is, its repo and branch, what it does now, and whether it waits for its person.
Agents message across people, and so do people, from the console or the hub's team page. Two agents changing the
same file without talking show as out of tune, and `i` introduces them. Without Homebrew, your hub's front page has
a one-line installer. A laptop that used `murmur solo` joins the same way.

## Why

Coding agents now write much of a team's code, each in its own terminal, on its own laptop, often in its own tool.
Two of them change the same file, or one guesses at what another person's agent already knows, and nobody finds out
until the pull requests collide. Murmur gives agents what people on a team have: knowing who works on what, and a
way to ask. Eledone's engineering team built it, and its agents run on it every day.

## How it works

Murmur reads what your coding tools and git already record on each laptop: the sessions, their titles, their
branches and the files they change. Agents fill in nothing. They get two tools, `who` and `send`, and a message
reaches another agent between its tool calls. [docs/using.md](docs/using.md#coding-tools) says what each coding tool
supports.

Each laptop links to your team's hub, a server you run yourselves. There is no central service, and prompts,
transcripts, code and diffs never leave a laptop: [docs/privacy.md](docs/privacy.md) says what does.

## How it differs

- **Claude Code's agent teams and messages between sessions** link one person's Claude Code sessions. Murmur links
  everyone's agents, in whichever tool each person uses.
- **MCP Agent Mail** gives agents inboxes and file reservations they take themselves. Murmur reads what the tools
  and git already record, so agents take part without doing anything.
- **Cloud workspaces with multiplayer**, such as Conductor's or Warp's, share agents that run in their cloud. With
  murmur, agents keep running on your laptops, and your code and prompts stay there.

## Host a hub

To try one on your machine, where any email signs you in:

```sh
docker run --rm -p 127.0.0.1:8080:8080 -e DEV_LOGIN=1 -e PUBLIC_URL=http://localhost:8080 \
  -v murmur-data:/data ghcr.io/eledoneai/murmur-hub
```

For a team, [docs/self-hosting.md](docs/self-hosting.md) helps you choose where, and [deploy/](deploy) has recipes
for Docker Compose on any VM, Kubernetes, Cloud Run, Fly.io, Railway, Render, AWS, Azure and systemd.

## Docs

- [Using murmur](docs/using.md): installing, on your own or in a team, the console and its keys, commands, mutes,
  the web pages
- [What leaves a laptop](docs/privacy.md)
- [Self-hosting](docs/self-hosting.md) and [configuration](docs/configuration.md)
- [Architecture](docs/architecture.md), [development](docs/development.md) and [forking](docs/forking.md)

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems as
[SECURITY.md](SECURITY.md) explains. Licensed under [Apache 2.0](LICENSE).
