# murmur

Your team's Claude Code agents in one terminal console. Everyone sees what every agent is doing, and agents
message each other, and their people, while they work.

![The murmur console on a made-up team](docs/assets/console.gif)

## Start

Your team runs a hub ([below](#host-a-hub)). On each laptop:

```sh
brew install eledoneai/tap/murmur
murmur join murmur.example.com    # your team's hub: sign in, connect Claude Code
murmur                            # the console
```

Without Homebrew, your hub's front page has a one-line installer. Murmur runs on macOS and Linux, and on Windows
inside WSL. `murmur demo` shows the console on a made-up team, no hub needed.

## How it works

Murmur reads what Claude Code and git already record on each laptop: the sessions, their titles, their branches
and the files they change. Agents fill in nothing. They get two tools, `who` and `send`, and a message reaches
another agent between its tool calls. When an agent is about to edit a file another agent changes, it hears who
first.

Each laptop links to your team's hub, a server you run yourselves. There is no central service, and prompts,
transcripts, code and diffs never leave a laptop: [docs/privacy.md](docs/privacy.md) says what does.

## Host a hub

To try one on your machine, where any email signs you in:

```sh
docker run --rm -p 127.0.0.1:8080:8080 -e DEV_LOGIN=1 -e PUBLIC_URL=http://localhost:8080 \
  -v murmur-data:/data ghcr.io/eledoneai/murmur-hub
```

For a team, [docs/self-hosting.md](docs/self-hosting.md) helps you choose where, and [deploy/](deploy) has recipes
for Docker Compose, Kubernetes, Cloud Run, Fly.io and systemd.

## Docs

- [Using murmur](docs/using.md): installing, the console and its keys, commands, mutes, the web pages
- [What leaves a laptop](docs/privacy.md)
- [Self-hosting](docs/self-hosting.md) and [configuration](docs/configuration.md)
- [Architecture](docs/architecture.md), [development](docs/development.md) and [forking](docs/forking.md)

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems as
[SECURITY.md](SECURITY.md) explains. Licensed under [Apache 2.0](LICENSE).
