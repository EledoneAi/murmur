# Architecture

Murmur links a team's Claude Code agents. Each person runs a small program on their laptop, and the team runs one hub. There's no central service: whoever deploys a hub owns it, and laptops talk only to their hub.

```
 laptop                                         hub (one or more instances)
┌──────────────────────────────────────┐        ┌──────────────────────────────┐
│ Claude Code sessions                 │        │ core: presence, routing,     │
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
| `internal/claudecode` | Everything murmur reads or writes in Claude Code: session registry, transcripts, inbox socket, settings.json, hook payloads |
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
- Loop limits between two agents are counted per instance. Agents connected to different instances can exchange up to twice the limit before it trips.

### Configuration

Environment variables only cover what is needed to start: where to listen, the store, the bus, the identity provider, the first admins and the public URL. The allowlist and `TOKEN_DAYS` only seed a new store. Everything a team tunes later lives in the store and is changed in the admin panel without a restart: who may join, sign-in lifetime, limits, ignored paths, update policy and oldest accepted version, laptop checks, the release check, agreements, the team's chat and GitHub merge notices.

## The laptop

The daemon is the only long-lived process. The console, the MCP server, one-shot commands and Claude Code's hooks talk to it over a unix socket. It reads Claude Code's own files and never asks agents to fill anything in. Mutes live in its history on the laptop.

Claude Code waits for two hooks, each bounded well under a second:
- **Session start:** the daemon briefs the session: who else works in its repo, the files its checkout shares with another (it asks the hub with `check`) and messages waiting for it, in five lines at most, then the team's agreements, which the daemon keeps from the hub's welcome. The MCP server's instructions carry the agreements too.
- **Before an edit:** the daemon asks the hub whether another checkout of the repo changes the file, for at most half a second, and the first such edit in a conversation is turned back once with the other agent's name.

Any failure prints nothing and lets the session go on.

`murmur init` signs a laptop in with OAuth for native apps and PKCE: it opens the hub's `/cli/login`, and the browser comes back to a one-shot listener on 127.0.0.1 with a code the laptop trades for its token. Over SSH, or with `--no-browser`, it prints a link to `/cli/login` with port 0 instead: the page shows the code, and its person pastes it into the terminal. Only the laptop that started the sign-in can trade the code.

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
