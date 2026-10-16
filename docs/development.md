# Developing murmur

[CONTRIBUTING.md](../CONTRIBUTING.md) covers what you need installed and how pull requests work.
[architecture.md](architecture.md) explains how the code is laid out. This page is the day-to-day: running a hub
and a laptop from source, the tests, and how a release is made.

Run `make` to list every target.

## A hub and a laptop on your machine

Everything below stays inside the repository, in `.dev/` and `bin/`. Your own `~/.claude` and `~/.murmur`
are never touched, so you can keep using murmur with your team while you work on it.

### The hub: `make dev`

```sh
make dev
```

1. Cross-builds the laptop binary for macOS and Linux on amd64 and arm64 into `.dev/dist`, with
   `checksums.txt`. These are what the hub serves to the install script and to laptops updating themselves.
2. Builds `bin/murmur-hub` and runs it with:

   ```sh
   DEV_LOGIN=1 PORT=8080 PUBLIC_URL=http://localhost:8080 STORE=sqlite://$PWD/.dev/hub.db \
     DIST_DIR=$PWD/.dev/dist LOG_FORMAT=text
   ```

Open http://localhost:8080 and sign in by typing any email: `DEV_LOGIN=1` needs no identity provider. The hub
keeps its data in `.dev/hub.db` between runs; delete the file to start over.

### A laptop: `make laptop`

In a second terminal:

```sh
make laptop
```

It builds `bin/murmur` and opens a shell where:

| Variable | Value |
|---|---|
| `MURMUR_HOME` | `.dev/home` |
| `CLAUDE_CONFIG_DIR` | `.dev/claude` |
| `MURMUR_HUB` | `http://localhost:8080` |
| `PATH` | `bin/` first |

In that shell:

```sh
murmur join    # signs this test laptop in to the dev hub, in your browser
claude         # Claude Code, with its settings in .dev/claude; it may ask you to log in the first time
murmur         # the console, in another tab of the same shell
```

Type `exit` to leave the shell. To use the same settings in a shell you already have:

```sh
eval "$(make -s laptop)"
```

For a second test laptop, start from that and point it elsewhere:

```sh
eval "$(make -s laptop)"
export MURMUR_HOME=$PWD/.dev/home2 CLAUDE_CONFIG_DIR=$PWD/.dev/claude2
```

Builds from source are version `dev` and never update themselves, so the test laptop keeps running the binary
you just built. Rebuild it with `make laptop`; its daemon picks up the new binary the next time it starts.

### The console without a hub: `make demo`

```sh
make demo
```

Opens the console on made-up teammates and agents (`murmur demo`). Use it to work on the console's look and keys.

The console's tests compare each screen with files in `internal/console/testdata`: the home screen at 60, 84, 110
and 160 columns, in plain text (`.txt`) and with its colours in both themes (`.ansi`), and every other screen. After
a change on purpose, rewrite them and read the diff:

```sh
go test ./internal/console -update
git diff internal/console/testdata
```

To look at a `.ansi` file as it draws, print it in a terminal: `cat internal/console/testdata/home-110-dark.ansi`.

### The web pages: `make web-dev` and `pnpm demo`

The hub's pages live in [web/](../web): an Astro app with Svelte components, which `make web` builds into
`internal/hub/webdist`. The hub embeds that folder, so a plain `go build` needs no Node; commit the build with the
sources (CI checks they match). The pages ask the hub for JSON and follow its team stream; `web/src/lib/api.ts`
has the types, and `web/src/lib/admin.ts` those of the admin panel's `/api/admin`.

```sh
make dev                  # terminal 1: the hub on localhost:8080
make web-dev              # terminal 2: the pages with hot reload on localhost:4321, against that hub
cd web && pnpm demo       # or the pages alone, on a made-up team, with no hub at all
```

Node and pnpm are needed only for this. The pages are styled with Tailwind; `web/src/styles/murmur.css` holds the
colour tokens for light and dark, the two faces and a few shared classes. Keep them quiet: text on the page rather
than in boxes, IBM Plex Sans for prose and monospace only for names, branches and data, teal only for what needs
you, and a person's colour only on their initial.

### The Compose recipe: `make compose-up`

`make compose-up` builds the hub image from source (`make image`) and starts
[deploy/compose/docker-compose.yml](../deploy/compose/docker-compose.yml) on http://localhost, with Caddy in
front and dev sign-in. It needs port 80 free. `make compose-down` stops it and keeps its volume.

## Tests

```sh
make test       # go test ./...
make race       # go test -race ./..., as CI runs it on Linux and macOS
make lint       # golangci-lint with .golangci.yml: go vet, staticcheck, errcheck and formatting
make vuln       # govulncheck
make fmt        # gofmt -s on every package
```

`make lint` and `make vuln` run their tools through `go run` at a pinned or latest version, so there is nothing
to install first.

### Tests that need Postgres, Redis or Firestore

The store and bus tests that need a real service skip themselves unless their variable is set:

| Variable | Example value |
|---|---|
| `STORE_TEST_POSTGRES` | `postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable` |
| `BUS_TEST_POSTGRES` | `postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable` |
| `BUS_TEST_REDIS` | `redis://localhost:16379` |
| `FIRESTORE_EMULATOR_HOST` | `localhost:18086` |

The Makefile starts the services in Docker and sets the variables:

```sh
make services        # Postgres, Redis and the Firestore emulator
make integration     # go test ./internal/store/... ./internal/bus/... against them
make services-down   # removes the containers
```

Or start them yourself:

```sh
docker run -d --name murmur-test-postgres -e POSTGRES_PASSWORD=postgres -p 15432:5432 postgres:18
docker run -d --name murmur-test-redis -p 16379:6379 redis:8
docker run -d --name murmur-test-firestore -p 18086:8086 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
  gcloud emulators firestore start --host-port=0.0.0.0:8086
```

The Firestore emulator takes several seconds to start; it answers `Ok` on http://localhost:18086/ when ready.

### Claude Code compatibility

Murmur reads files Claude Code writes and uses its inbox socket, so a new Claude Code can break it.
`make compat` runs the compatibility tests against the `claude` on your `PATH`. Claude Code runs there with a
configuration folder of its own, so it can't use your sign-in: give it an API key, or a token from
`claude setup-token`, which uses your Claude subscription.

```sh
ANTHROPIC_API_KEY=… make compat           # or CLAUDE_CODE_OAUTH_TOKEN=… make compat
```

The `Claude Code compatibility` workflow runs it every night against the newest Claude Code from npm, with the
repository secret `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`; without either, it skips the check with a
warning. When it fails, it opens an issue labelled `compat`, or adds a comment to the one already open.

## How releases work

A release is a `v*` tag. [.github/workflows/release.yml](../.github/workflows/release.yml) runs GoReleaser with
[.goreleaser.yaml](../.goreleaser.yaml), which:

1. builds `murmur` for macOS and Linux on amd64 and arm64, and `murmur-hub` for Linux and macOS, with the
   version and the release key's public half stamped in
   (`-X github.com/EledoneAi/murmur/internal/release.Version=<version>`, and `release.Key`);
2. writes `checksums.txt` with the SHA-256 of every file, and signs it into `checksums.txt.sig` with
   `go run ./deploy/sign`;
3. publishes the GitHub release with the changelog from the commits since the last tag;
4. pushes `ghcr.io/eledoneai/murmur-hub:<version>` and `:latest` for linux/amd64 and linux/arm64, each
   carrying the four laptop binaries, `checksums.txt` and its signature in `/usr/share/murmur/dist`;
5. updates the Homebrew cask in `eledoneai/homebrew-tap`.

A tag with a suffix, such as `v1.2.0-rc.1`, is a prerelease: it doesn't move `latest` and doesn't update the
cask.

### The release key

Release binaries carry the project's public key. Each time a laptop updates, it checks `checksums.txt.sig` against
that key, then checks the download against `checksums.txt`: a hub can't update laptops to a binary the key didn't
sign. The install script only checks the binary against the hub's own `checksums.txt`, so a first install trusts the
hub over HTTPS. A hub built with the key refuses to start when the `checksums.txt.sig` in its `DIST_DIR` doesn't
verify. To make a key:

```sh
go run ./deploy/sign -keygen
```

It prints a key pair. The public key is built into every binary as `internal/release.Key`: GoReleaser and `make`
read it from `MURMUR_RELEASE_PUBKEY`, and the source image from the `RELEASE_PUBKEY` build argument. The private
key goes only into the repository's Actions secrets, as `MURMUR_RELEASE_KEY`. A team that builds its own
releases from source makes its own key pair the same way and builds its public key in
([forking.md](forking.md)). A binary built without a key checks downloads against `checksums.txt` only.

Leave `MURMUR_RELEASE_PUBKEY` unset for `make dev`: it doesn't sign `.dev/dist`, and a hub with the key would
refuse to start.

### One-time setup of the repository

- Actions secret `MURMUR_RELEASE_KEY`: the private key above.
- Actions variable `MURMUR_RELEASE_PUBKEY`: the public key above, built into every binary. The release
  workflow stops at once without either of them.
- Actions secret `HOMEBREW_TAP_TOKEN`: a fine-grained token with **Contents: read and write** on the
  `eledoneai/homebrew-tap` repository. The workflow's own `GITHUB_TOKEN` can't push to another repository.
- The `eledoneai/homebrew-tap` repository, with a `Casks` folder.
- After the first release, make the `murmur-hub` package on ghcr.io public, so anyone can pull the image.

### Making a release

```sh
git switch main && git pull
git tag -a v1.2.0 -m "v1.2.0"
git push origin v1.2.0
```

Then watch the `Release` workflow. To see what it would produce without publishing anything:

```sh
make snapshot
```

That runs GoReleaser into `dist/`, builds the hub image for both architectures locally, and signs with a
throwaway key unless `MURMUR_RELEASE_KEY` is set. It uses `goreleaser` from your `PATH`, or `go run` if there
is none. The cask's install steps need GoReleaser 2.19 or later.

### Images built from source

`make image` builds [deploy/docker/Dockerfile](../deploy/docker/Dockerfile): the hub and the four laptop binaries
with their `checksums.txt`, in one image. To sign them with your key:

```sh
MURMUR_RELEASE_KEY=<private key> docker build --secret id=release_key,env=MURMUR_RELEASE_KEY \
  --build-arg RELEASE_PUBKEY=<public key> -f deploy/docker/Dockerfile -t murmur-hub .
```

Pass both or neither: a hub built with the public key refuses to start with unsigned binaries.

## Recordings and screenshots

The README's recording of the console plays `make demo`. [vhs](https://github.com/charmbracelet/vhs) records it
again from [tapes/console.tape](tapes/console.tape), with the JetBrains Mono font installed:

```sh
make build
vhs docs/tapes/console.tape      # docs/assets/console.gif and console.png
```

The screenshots of the team page and the admin panel come from the pages' demo mode, in both color schemes:

```sh
cd web && pnpm demo              # in another terminal, from the repository root:
uv run --with playwright python3 docs/tapes/pages.py
```
