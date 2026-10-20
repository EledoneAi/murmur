# Architecture

Murmur links a team's coding agents: Claude Code, Codex, Cursor, OpenCode, Copilot CLI and Gemini CLI. Each person runs a small program on their laptop, and the team runs one hub. There's no central service: whoever deploys a hub owns it, and laptops talk only to their hub.

```
 laptop                                         hub (one or more instances)
┌──────────────────────────────────────┐        ┌──────────────────────────────┐
│ coding tools' sessions               │        │ core: presence, routing,     │
│   │ hooks, session files, inbox      │        │       overlaps, limits       │
│   ▼                                  │  wss   │ web: sign-in, team, admin    │
│ daemon ── ipc socket ── console      │◀──────▶│ dist: install.sh, binaries   │
│   ▲                    └─ mcp (who,  │        ├──────────────┬───────────────┤
│   │                       send)      │        │ store        │ bus           │
│   └── update (verifies, swaps, rolls │        │ sqlite · pg  │ local · pg ·  │
│       back)                          │        │ firestore    │ redis         │
└──────────────────────────────────────┘        └──────────────┴───────────────┘
```

## Packages

| Package | What it holds |
|---|---|
| `cmd/murmur` | Laptop CLI: argument parsing only |
| `cmd/murmur-hub` | Hub server: reads config, picks the store, bus and auth, then serves |
| `internal/protocol` | What crosses the WebSocket: frames, typed constants, the protocol version, addresses, `Clean` |
| `internal/release` | Versions (semver), the release key and signature check, and `Repo`, where hubs look for new releases |
| `internal/transport` | The hub's side of one WebSocket connection: read loop, writer, pings. The daemon keeps its own connection |
| `internal/harness` | What murmur needs from an AI coding tool (a harness): the `Harness` interface, with the optional `Lister`, `Pusher` and `Reader`; hook events; a session's activity; the order-preserving editor for settings files |
| `internal/harness/claude` | Everything murmur reads or writes in Claude Code: session registry, transcripts, inbox socket, settings.json, hook payloads |
| `internal/harness/hookish` | The harnesses whose hooks copied Claude Code's, one row of a table each: Codex, Cursor, Copilot CLI, Gemini CLI |
| `internal/harness/opencode` | OpenCode, through a plugin (`plugin.js`, built into the binary) |
| `internal/gitscan` | Repo, branch and changed files of a checkout, the size of its change and where it stands (base branch, rebase or merge, folder) |
| `internal/ipc` | The local socket between the daemon and its clients: requests, replies, snapshots, and a client |
| `internal/daemon` | The laptop process: sessions, hub link, messages, mutes, outbox, history, ipc server |
| `internal/update` | Laptop self-update: check, download, verify, swap, health check, rollback |
| `internal/mcp` | The stdio MCP server agents use (`who`, `send`) |
| `internal/console` | The terminal console on Bubble Tea v2: the board built from a snapshot (`board.go`), its signs (`vocab.go`), the links' gutter, the inspector, thread, sheets, motion, and the plain mode |
| `internal/setup` | `init`, `doctor`, `uninstall`, laptop sign-in |
| `internal/hub` | The hub: presence across instances (`presence.go`), connections (`links.go`), routing and limits (`route.go`), overlaps, settings, background jobs, and its web side: sign-in and dev login (`signin.go`), the JSON the pages ask for (`/api/...`, the admin panel's under `/api/admin`), the team page's stream, downloads, and the pages themselves, which `web/` builds into `webdist/` and the binary embeds |
| `internal/auth` | Identity providers: generic OIDC (Microsoft Entra, Okta, Keycloak, Authentik, GitLab…), Google (OIDC with the `hd` hint) and GitHub OAuth |
| `internal/store` | Durable state behind one interface: `memory`, `sqlite`, `postgres`, `firestore`, plus `storetest` (contract tests) |
| `internal/bus` | Events between hub instances: `local`, `postgres` (LISTEN/NOTIFY), `redis` |
| `web` | The hub's web pages: an Astro app with Svelte components and Tailwind, which `make web` builds into `internal/hub/webdist` |

Only `cmd/` builds binaries. The other Go packages are `internal/`, so nothing outside this repo can depend on them.

## The hub

### State
- **Durable state** is in the store:
  - people, laptops and their token hashes, with each laptop's version, last sighting and last self-check
  - messages kept for someone offline
  - one-time sign-in codes
  - admin settings
  - announcements to repos, while they are up
  - the admin event log: the newest 10,000 events, none older than 90 days, never message text
  - the newest release the release check found
  - the job lease
  - the weekly counts, team totals only: each instance adds its own under its own key, and the report sums them
  - which waits the chat was pinged about, the collisions counted today, and the last weekly report posted
- **Live state** is in memory:
  - the WebSocket links an instance holds
  - the roster, rebuilt from presence events; overlaps are read from it when a laptop checks files or a roster goes out
  - the team pages open on the instance, each streaming the roster
  - rate-limit windows

Mutes are not on the hub: each laptop keeps its own.

A restart loses nothing that matters: laptops reconnect, say hello again, and resend what is in their outbox.

### Instances and the bus
- Every instance publishes events to the bus and acts on the events that concern its own links.
- Event kinds:
  - **presence:** a laptop's agents; **alive:** the laptops an instance still holds; **sync:** a new instance asks
    the others for theirs
  - **frame:** a frame for one laptop or for every laptop of one person, held by another instance
  - **changed:** a person, laptop, setting or announcement changed in the store, so reload it
  - **answer:** the answer to a message sent from the team page, for the instance where the page waits
- With one instance the bus is in memory, and the code path is the same.
- Each instance says which laptops it holds every 10 s. A laptop no instance has named for 35 s is offline, which covers instances that die without saying goodbye.
- Background jobs run every 30 s on whichever instance holds the job lease in the store: expiring kept messages, the release check (once a day), following the team (waits and collisions, counted for the weekly report, and chat pings about agents left waiting), and the weekly report. Holding the lease is the only form of leadership. The hub schedules nothing on laptops: each one checks its own setup every `check_hours` and reports it.
- GitHub posts pull request events to `/github/webhook`, signed with the team's secret. When one merges, the instance that took the event reads its files from GitHub's API and tells the agents changing them, wherever they are connected.
Ceilings, on purpose:
- Every instance sees every event. A team is a few hundred laptops at most, so that's fine. Partition by person if teams ever get much bigger.
- Every laptop gets the whole team in each roster, so rosters cost the team's size times its laptops. An instance encodes each roster once and hands out at most 128 MB of them a second, in turn: laptops that just said hello first and at twice that, the team pages as one more laptop. A laptop that gets a message gets the roster that has its sender as it was first. The consoles of a big, busy team catch up every few seconds, and its messages keep their speed. Rosters that carry only who changed would lift this ceiling; they need a new protocol.
- Loop limits between two agents are counted per instance. Agents connected to different instances can exchange up to twice the limit before it trips.

### Configuration

Environment variables only cover what is needed to start: where to listen, the store, the bus, the identity provider, the first admins and the public URL. The allowlist and `TOKEN_DAYS` only seed a new store. Everything a team tunes later lives in the store and is changed in the admin panel without a restart: who may join, sign-in lifetime, limits, ignored paths, update policy and oldest accepted version, laptop checks, the release check, agreements, the team's chat and GitHub merge notices.

## The laptop

The daemon is the only long-lived process. The console, the MCP server, one-shot commands and the coding tools' hooks talk to it over a unix socket. It reads what the tools record and never asks agents to fill anything in. Mutes live in its history on the laptop.

### Harnesses

Each coding tool, a harness, is behind `harness.Harness`: connect and disconnect for join and uninstall, checks for doctor, and reading a hook's input and writing its answer. Hooks run `murmur hook <harness> <event>`, which hands the daemon a `harness.Event` in murmur's words (start, prompt, before-tool, after-tool, stop, needs, end) and prints the daemon's answer in the tool's own dialect. Three optional capabilities, found by type assertion, say what more a tool offers:

- **`Lister`**: the tool keeps a registry of its live sessions, which the daemon watches (Claude Code's `~/.claude/sessions`). Without one, the daemon learns each session from its hooks: the first names it (`codex-1`), a prompt or a tool call means it works, the end of a turn that it waits, and it goes when it ends, when its process does, or after 12 hours without a hook. A hook finds its tool's process by walking up past the shell that runs it. Sessions learned this way are kept in the history, so a restarted daemon knows them again.
- **`Pusher`**: murmur can hand a running session a message (Claude Code's inbox, OpenCode's plugin). Without it, messages wait in a queue per session and leave with the answer to its next after-tool, stop or prompt hook. An idle agent gets them only when its person prompts it again; its sender is told.
- **`Reader`**: the tool's transcripts are worth reading for the title, pull requests, tool calls and context use (Claude Code). Without one, tool calls come from the tool's hooks, through one table of every tool's tool names.

Sessions are keyed by harness and session id. The console, the hub and the protocol don't know harnesses: an agent's `App` says which tool it runs in. The MCP server knows which session it speaks for from its parent process, the session id a tool gives it in its environment or in a tool call's metadata, or, failing those, the busy session of its tool.

The tools that wait for hooks wait for these, each bounded well under a second:
- **Session start:** the daemon briefs the session: who else works in its repo, the files its checkout shares with another (it asks the hub with `check`) and messages waiting for it, in five lines at most, then the team's agreements, which the daemon keeps from the hub's welcome. The MCP server's instructions carry the agreements too.
- **Before an edit:** the daemon asks the hub whether another checkout of the repo changes the file, for at most half a second, and the first such edit in a conversation is turned back once with the other agent's name.

Any failure prints nothing and lets the session go on.

`murmur solo` sets a laptop up with no team: its daemon runs `internal/hub` itself, on 127.0.0.1, with the memory store, the local bus and no identity provider, so no page signs anyone in. At each start the daemon signs its person in with `Hub.Solo`, under their login name, and saves the token, which only that run's hub accepts. It keeps its port while the port is free. The laptop binary is built with the `laptop` tag, which leaves out the server-only stores and buses (`internal/store/laptop.go`, `internal/bus/laptop.go`).

`murmur join` signs a laptop in with OAuth for native apps and PKCE: it opens the hub's `/cli/login`, and the browser comes back to a one-shot listener on 127.0.0.1 with a code the laptop trades for its token. Over SSH, or with `--no-browser`, it prints a link to `/cli/login` with port 0 instead: the page shows the code, and its person pastes it into the terminal. Only the laptop that started the sign-in can trade the code.

## Updates

- **We** publish signed releases and nothing else: binaries, hub images, and `checksums.txt` signed into `checksums.txt.sig` with the project key. The release workflow builds the public key into every binary and stops when it is missing.
- **The admin** chooses when the team moves, by upgrading the hub. Each hub image carries the client binaries of its own version, so a hub needs no internet access to serve updates. The admin panel can check for new releases once a day, and the admin can switch that off.
- **The hub** tells each laptop the team version, the oldest version it accepts, and the team's update policy: `auto`, `notify` or `off`. A laptop older than the oldest accepted version is refused, with the command to run.
- **The user** can run `murmur update` at any time, and set their own laptop to `ask` or `off`. A refused laptop updates itself whatever the policy, unless its person chose `off`: then it stays refused until they run `murmur update`.
- **Verification:** laptops verify `checksums.txt.sig` against the key built into the binary, then the checksum of what they download. A hub can't update laptops to code the project didn't sign. A binary built without a key checks the checksum only. The install script checks the binary against the hub's `checksums.txt`, not the signature: a first install trusts the hub over HTTPS. A hub built with a key refuses to start when its `DIST_DIR` holds binaries the key didn't sign. A team that builds from source builds in its own key.
- **Applying an update:** when an update is due and none of the laptop's agents is busy, the daemon starts `murmur update --auto` in a child process, at most once an hour. The child downloads and checks the binary, replaces the running one in place and keeps the old one as `murmur.prev`, then restarts the daemon and waits up to a minute for it to be online on the new version. If it isn't, the child puts `murmur.prev` back and restarts the old daemon, and automatic updates skip that version for an hour; `murmur update` run by hand tries it again. Laptops never move to an older version because a hub says so.
- **Package managers:** binaries that Homebrew or Nix installed (under `/opt/homebrew`, `/usr/local/Cellar`, `/usr/local/Caskroom`, `/home/linuxbrew` or `/nix/store`) never replace themselves. They say which command to run. Their hooks and MCP server point at the package manager's link on `PATH`, which stays valid across upgrades.

## Protocol

Frames are JSON objects over one WebSocket per laptop. `hello` carries the protocol version. A hub refuses protocol versions it doesn't speak, with a message that says what to update. There is no compatibility with releases from before 1.0.
