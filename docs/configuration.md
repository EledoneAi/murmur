# Configuration

Murmur reads environment variables only. The hub reads what it needs to start: where to listen, its address,
the store, the bus, the identity provider and the first admins. Everything a team tunes later lives in the store
and is changed in the hub's admin panel, without a restart: see [Admin settings](#admin-settings).

## The hub

### Basics

| Variable | Default | What it does |
|---|---|---|
| `PORT` | `8080` | The port the hub listens on. |
| `PUBLIC_URL` | `http://localhost:$PORT` | The address people and laptops use, such as `https://murmur.example.com`. Set it everywhere but on your own machine. Sign-in redirects and the install line use it, and laptops installed from that line keep it as their hub's address. |
| `SESSION_SECRET` | none | The key the hub signs its cookies with: 16 characters or more, random (`openssl rand -base64 32`). Required unless `DEV_LOGIN=1`. Changing it signs everyone out of the web pages. |
| `LOG_FORMAT` | `json` | `json` writes one JSON object per line; `text` writes plain lines for reading in a terminal; `gcp` writes JSON with the field names Google Cloud Logging understands. |
| `DIST_DIR` | `/usr/share/murmur/dist` | The folder of laptop binaries the hub serves to the install script and to laptops updating themselves. It holds `murmur-darwin-amd64`, `murmur-darwin-arm64`, `murmur-linux-amd64`, `murmur-linux-arm64`, `checksums.txt` and `checksums.txt.sig`. The image carries the ones of its own release. Without the folder the hub starts, but laptops can't install or update from it. A hub built with a release key refuses to start when the folder's `checksums.txt.sig` doesn't verify against that key. |
| `INSTANCE_ID` | random at each start | Optional: this instance's name on the bus, in the job lease and in the logs. If you set it, give each instance its own. |
| `TRUST_PROXY` | unset | `1` when the hub runs behind a proxy or load balancer that appends the client's address to `X-Forwarded-For` (Caddy, nginx, Kubernetes ingress, Cloud Run, Fly). The hub then limits downloads per client address, read from the last hop. Without a proxy, leave it unset: anyone can write that header. |
| `GOMEMLIMIT` | 80% of the container's memory limit | Go's soft memory limit, such as `400MiB`: the hub collects garbage harder as it nears it rather than outgrow its container. In a container (cgroup v2), the hub sets it from the container's limit itself. |

### Store and bus

| Variable | Default | What it does |
|---|---|---|
| `STORE` | `sqlite:///data/murmur.db` in the image, `memory://` otherwise | Where the hub keeps people, laptops, waiting messages and settings. One of: `memory://` (nothing survives a restart), `sqlite:///path/file.db` (`sqlite://file.db` is relative to the working folder; the file and its folder are created), `postgres://user:password@host:5432/database` or `postgresql://...`, `firestore://PROJECT` or `firestore://PROJECT/DATABASE`. Set it yourself when you run the binary without the image. |
| `BUS` | `local://` | How hub instances pass events to each other. `local://` keeps them inside one instance. To run more than one instance, use `postgres://...` (or `postgresql://...`) or `redis://...` (`rediss://...` for TLS). Hubs that share one Postgres or Redis need a channel each: add `?channel=NAME` (default `murmur`). The Postgres bus needs a session of its own: point it at the server or a session pooler, not a transaction pooler. |

More than one instance needs a networked bus and a networked store: not `sqlite://`, not `memory://`.

The `<version>-litestream` image also reads `LITESTREAM_REPLICA_URL`, where it copies the sqlite store as it changes,
and the credentials that storage needs: [deploy/litestream](../deploy/litestream).
[self-hosting.md](self-hosting.md#choose-a-store-and-a-bus) helps you choose.

### Sign-in

Configure one identity provider. When several are set, the hub uses the first of `OIDC_ISSUER`,
`GOOGLE_CLIENT_ID` and `GITHUB_CLIENT_ID`; with none, it doesn't start unless `DEV_LOGIN=1`. The redirect URL to
register with the provider is `PUBLIC_URL` followed by `/auth/callback`.
[self-hosting.md](self-hosting.md#set-up-sign-in) shows each provider step by step.

| Variable | What it does |
|---|---|
| `OIDC_ISSUER` | The issuer URL of an OpenID Connect provider (Microsoft Entra, Okta, Keycloak, Authentik, GitLab, Google and others). It must be `https://`, or `http://` on localhost. |
| `OIDC_CLIENT_ID` | The client ID the provider gave the hub. |
| `OIDC_CLIENT_SECRET` | The client secret the provider gave the hub. |
| `OIDC_NAME` | The provider's name on the sign-in button: "Sign in with *name*". |
| `OIDC_TRUST_EMAIL` | `1` counts every email address the provider sends as verified, and lets `preferred_username` stand in for a missing email when it is an address. Microsoft Entra needs it. Use it only with a provider whose addresses your organization controls. |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | Sign in with Google. When `ALLOWED_DOMAINS` holds exactly one domain, Google's sign-in page offers only accounts of that domain (its `hd` hint); the hub still checks each address. |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | Sign in with GitHub, through a GitHub OAuth app. |
| `GITHUB_URL` | The address of a GitHub Enterprise Server, such as `https://github.example.com`, for sign-in and for merge notices. Leave it unset for github.com. |
| `DEV_LOGIN` | `1` lets anyone sign in by typing an email address, when no identity provider is set. The hub refuses to start with it unless `PUBLIC_URL` is `http://localhost` or a loopback address such as `http://127.0.0.1:8080`, so it can't be left on by mistake on a hub others reach. While there are no `ADMINS` and no admin yet, the first person to sign in becomes an admin; with no allowlist, anyone can sign in. For local development. |

### Who may join

The allowlist variables and `TOKEN_DAYS` set the starting values: they are read the first time a hub starts, with
an empty store. After that the hub keeps its settings in its store, and admins change them in the admin panel;
changing these variables then has no effect. `ADMINS` is read at every start: whoever it names can sign in even
when no list names them, and becomes an admin the next time they do (it never takes admin rights away; the admin
panel does). Lists are separated by commas.

With an empty allowlist, only the people in `ADMINS` and the admins made in the panel can sign in.

| Variable | What it does |
|---|---|
| `ALLOWED_DOMAINS` | Email domains whose people may join, such as `example.com,example.org`. |
| `ALLOWED_EMAILS` | Single email addresses that may join, for people outside those domains. |
| `ALLOWED_GITHUB_ORGS` | GitHub organizations whose members may join, with GitHub sign-in. |
| `ADMINS` | Email addresses of the first admins. |
| `TOKEN_DAYS` | Days a laptop stays signed in before its person signs it in again: 0 to 3650, `90` by default, `0` for never. |

### Version

The version is set when the binaries are built, not at run time:

```sh
go build -ldflags "-X github.com/EledoneAi/murmur/v2/internal/release.Version=1.2.3" ./cmd/murmur-hub
go build -ldflags "-X github.com/EledoneAi/murmur/v2/internal/release.Version=1.2.3" ./cmd/murmur
```

Releases set it to the tag without its `v`. A build without it is version `dev`, and laptops built that way never
update themselves. The release key is built in the same way, as `internal/release.Key`; see
[development.md](development.md#the-release-key).

## Admin settings

Admins change these in the admin panel: who may join on `/admin/people`, the rest on `/admin/settings`. Changes
apply at once on every instance.

| Setting | Default | What it does |
|---|---|---|
| Days a laptop stays signed in | 90 | Seeded from `TOKEN_DAYS`. 0 keeps laptops signed in until someone revokes them. |
| Messages per minute | 20 | What one person or one agent can send. |
| Messages between two agents | 12 in 10 minutes | Past that, a person must step in. |
| Agents per laptop | 64 | Agent sessions one laptop can report. |
| Laptops per person | 10 | Laptops one person can have signed in at once. |
| Downloads per hour | 120 | Installs and updates one network address can start. |
| Ignored paths | none | Files that never count as an overlap, on top of each repo's `.gitattributes`: `traces/**` covers a folder, `*.lock` any file with that name, `docs/*.md` the files right in `docs`. |
| Update policy | `auto` | `auto`: laptops update themselves when none of their agents is busy. `notify`: the console says a version is ready, and people run `murmur update`. `off`: laptops stay on their version until their people update them. |
| Oldest accepted version | none | The hub refuses older laptops; they update themselves unless their person chose `murmur updates off`. |
| Release check | on | Once a day the hub asks `https://api.github.com/repos/EledoneAi/murmur/releases/latest` (`internal/release.Repo`) for the newest release, and the overview shows it. It sends nothing about the team. |
| Team agreements | none | How your agents should work together, up to 4000 characters. Every agent reads them at the start of each session. |
| Hours an announcement stays up | 24 | Announcements to a repo reach its running agents and stay on the team page this long. |
| Chat webhook and chat app | none | An `https://` incoming webhook, for Slack, Google Chat, Discord, Microsoft Teams, or anything that takes `{"text": …}`. See [self-hosting.md](self-hosting.md#the-teams-chat). |
| Minutes before a wait is posted | 15 | How long an agent waits for its person before the chat hears of it. 0 never posts waits. |
| Working day | 9 to 19, Monday to Friday | Hours in the team's time zone. Nothing is posted to the chat outside them. |
| Time zone | `UTC` | A name like `Europe/Paris`. Working hours and the weekly report follow it: set it to your team's. |
| Weekly report | on | Posted to the chat on Monday at the start of the working day. Team totals only. |
| GitHub webhook secret and read-only token | none | For merge notices: see [self-hosting.md](self-hosting.md#github-merge-notices). The page never shows them again. |
| Hours between laptop checks | 24 | Each laptop checks its own setup this often and reports it to the admins. 0 turns checks off. |

## The laptop

| Variable | Default | What it does |
|---|---|---|
| `MURMUR_HUB` | the hub saved at sign-in | The team's hub, such as `https://murmur.example.com`. `murmur join` saves the hub it joins. Set this to point a laptop at another hub, as `make laptop` does for a test laptop. |
| `MURMUR_HOME` | `~/.murmur` | Where murmur keeps its files on the laptop: config and sign-in token, the daemon's socket and log, message history. Point it elsewhere to keep a test laptop apart from your real one. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | Claude Code's own setting for its folder. Murmur follows it: it reads Claude Code's sessions there and adds its hooks to `settings.json` there. |
| `CODEX_HOME` | `~/.codex` | Codex's own setting for its folder, where murmur adds its hooks. |
| `CURSOR_CONFIG_DIR` | `~/.cursor` | Cursor CLI's setting for its folder, where murmur adds its hooks and MCP server. |
| `OPENCODE_CONFIG_DIR` | `~/.config/opencode` | A folder OpenCode loads plugins from besides its own, where murmur then writes its plugin. Without it, murmur follows `XDG_CONFIG_HOME`. |
| `COPILOT_HOME` | `~/.copilot` | Copilot CLI's own setting for its folder, where murmur adds its hooks and MCP server. |
| `GEMINI_CLI_HOME` | your home folder | Gemini CLI's own setting for the folder that holds `.gemini`, where murmur adds its hooks and MCP server. |
| `MURMUR_THEME` | the terminal's background | The console's colours: `dark`, `light`, `ansi` (the terminal's 16 colours) or `contrast`. |
| `MURMUR_MOTION` | on | `0` stops the console's animations. They are off over SSH anyway. |
| `MURMUR_GLYPHS` | from the locale | `ascii` draws the console with ASCII only; `unicode` forces the symbols. |
| `MURMUR_BELL` | unset | `1` rings the terminal bell when one of your agents starts waiting for you. |
| `NO_COLOR` | unset | Any value turns colors off in the console, `join` and `doctor`. |

Laptops install with the lines their hub's front page shows:

```sh
brew install eledoneai/tap/murmur
murmur join https://<your hub>
```

Without Homebrew, `curl -fsSL https://<your hub>/install.sh | sh` downloads the binary from the hub and runs
`murmur join`.
