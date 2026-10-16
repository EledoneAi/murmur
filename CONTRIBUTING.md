# Contributing to murmur

Thank you for helping. Bug reports, fixes, docs and deploy recipes are all welcome. For anything bigger than a
fix, open an issue first and say what you want to change and why, so nobody spends a week on something that
can't be merged.

Please follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report security problems privately, as
[SECURITY.md](SECURITY.md) explains, not in an issue.

## Set up

You need:

- macOS or Linux (on Windows, work inside WSL)
- Go, at the version `go.mod` names or later
- `make` and `git`
- Docker, for the store and bus tests and for building images
- Node and pnpm, only to change the hub's web pages in `web/`
- Claude Code 2.1.224 or later, to try a laptop end to end (`npm install -g @anthropic-ai/claude-code`)

Then:

```sh
git clone https://github.com/EledoneAi/murmur
cd murmur
make            # lists every target
make test
```

To run murmur while you work on it, open two terminals:

```sh
make dev        # terminal 1: a hub on http://localhost:8080 that lets you sign in with any email
make laptop     # terminal 2: a shell for a test laptop, then: murmur join, claude, murmur
```

`make laptop` points murmur and Claude Code at `.dev/` inside the repo, so your own `~/.claude` and
`~/.murmur` are never touched. `make demo` opens the console with made-up data and needs no hub.
[docs/development.md](docs/development.md) explains all of this in more detail, and
[docs/architecture.md](docs/architecture.md) explains how the code is laid out.

## Tests

```sh
make test       # every test that needs nothing but Go
make race       # the same with the race detector, as CI runs them
make lint       # golangci-lint, with the settings in .golangci.yml
make vuln       # govulncheck
```

Some store and bus tests need a real Postgres, Redis or Firestore emulator. They are skipped unless their
environment variable is set:

| Variable | What it points to |
|---|---|
| `STORE_TEST_POSTGRES` | a Postgres URL for the `postgres` store tests |
| `BUS_TEST_POSTGRES` | a Postgres URL for the `postgres` bus tests |
| `BUS_TEST_REDIS` | a Redis URL for the `redis` bus tests |
| `FIRESTORE_EMULATOR_HOST` | `host:port` of a Firestore emulator, for the `firestore` store tests |

`make services` starts all three in Docker on unusual ports, `make integration` runs the tests against them,
and `make services-down` removes them. CI runs the same tests on every pull request.

## Style

- Run `make fmt` and `make lint` before you push.
- Comments explain why, in plain sentences. The code already says what it does; a comment says why it does it
  that way, or what would go wrong otherwise.
- Write for people who read English as a second language: short sentences, common words, no jargon you don't
  need.
- Keep dependencies few. Prefer the standard library. Say in the pull request why a new dependency is worth it.
- Messages and errors people see say what happened and what to do next.
- Test what you change. A bug fix comes with a test that fails without it.

## Pull requests

- Keep them small and about one thing. Two small pull requests are reviewed faster than one big one.
- Write commit messages like the ones in `git log`: a short subject line that says what changes, then a few
  lines saying what and why.
- Update the docs in the same pull request when you change behavior, a command or a setting.
- CI must pass: formatting, `go vet`, golangci-lint, the tests on Linux and macOS, the web pages' build and
  check, govulncheck, the store and bus tests, and the image build. After a change in `web/`, run `make web` and
  commit `internal/hub/webdist` with it.

## License

Murmur is under the [Apache License 2.0](LICENSE). By contributing, you agree that your contribution is
licensed under it too.
