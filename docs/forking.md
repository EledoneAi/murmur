# Publishing your own builds

A team that builds and publishes murmur under its own GitHub organization changes the places below. They all
name `EledoneAi`, `eledoneai` or the project's release key; `grep -rni eledoneai .` finds them.

## The release key

Laptops update only to binaries signed with the key built into them, so your builds need a key pair of your own:

```sh
go run ./deploy/sign -keygen
```

- Put the public key in the repository's Actions variable `MURMUR_RELEASE_PUBKEY`, and the private key in the
  Actions secret `MURMUR_RELEASE_KEY`. `.github/workflows/release.yml` stops without either.
- `Makefile` and `.goreleaser.yaml` build the public key in from `MURMUR_RELEASE_PUBKEY`
  (`-X …/internal/release.Key=`), and `deploy/docker/Dockerfile` from its `RELEASE_PUBKEY` build argument.

Laptops installed from the project's releases carry the project's key: they can't update to your builds. Install
them again from a hub that serves yours.

## The code

- `internal/release/release.go`: `Repo` is where hubs look for new releases once a day
  (`https://api.github.com/repos/<Repo>/releases/latest`). Set it to your repository, or build it in with
  `-X github.com/EledoneAi/murmur/internal/release.Repo=<owner>/<name>`. `docs/configuration.md` names it
  under Admin settings. `Tap` is the Homebrew cask the hub's front page tells people to install; build it in
  the same way, with `-X github.com/EledoneAi/murmur/internal/release.Tap=<your org>/tap/murmur`.
- `go.mod`: the module path, `github.com/EledoneAi/murmur`. Builds from a clone work without changing it; it
  must match your repository only for `go install`. If you change it, change it everywhere at once: the imports in
  every `.go` file, `MODULE` in `Makefile`, the `ldflags` in `.goreleaser.yaml` and `deploy/docker/Dockerfile`,
  and the examples in `docs/configuration.md`, `docs/development.md` and `deploy/sign/main.go`.

## Releases and images

- `.goreleaser.yaml`:
  - `dockers_v2`: the image name `ghcr.io/eledoneai/murmur-hub`, and the `org.opencontainers.image.source` label;
  - `homebrew_casks`: the tap `repository` (`owner: eledoneai`, `name: homebrew-tap`), the `url` template and
    `homepage`;
  - `release.github`: `owner` and `name`;
  - the release `footer` and the comments, which name the image and the tap.
- `deploy/docker/Dockerfile`: the `org.opencontainers.image.source` label.
- The tap: a repository named `homebrew-tap` in your organization, with a `Casks` folder, and the Actions secret
  `HOMEBREW_TAP_TOKEN`: a fine-grained token with **Contents: read and write** on it. People then install with
  `brew install <your org>/tap/murmur`.
- After the first release, make the `murmur-hub` package on ghcr.io public.

`GITHUB_TOKEN` is the workflow's own: it needs nothing from you.

## Deploy recipes

The image name `ghcr.io/eledoneai/murmur-hub`:

- `deploy/compose/docker-compose.yml` and `deploy/compose/compose.ha.yml` (`MURMUR_IMAGE` overrides it)
- `deploy/helm/murmur-hub/values.yaml` (`image.repository`)
- `deploy/fly/fly.toml`
- `deploy/cloudrun/README.md`, `deploy/compose/README.md`, `deploy/helm/murmur-hub/README.md`
- `README.md` and `docs/self-hosting.md` (the `docker run` line, Azure and ECS)
- `docs/development.md` (what a release pushes)

The repository's address, `https://github.com/EledoneAi/murmur`:

- `deploy/helm/murmur-hub/Chart.yaml`: `home` and `sources`
- the `git clone` lines in `CONTRIBUTING.md`, `deploy/compose/README.md` and `deploy/helm/murmur-hub/README.md`
- the release downloads and raw file links in `deploy/systemd/README.md`, and the releases link in
  `deploy/cloudrun/README.md`
- `Documentation=` in `deploy/systemd/murmur-hub.service`
- the security advisory link in `SECURITY.md`, and the links in `.github/ISSUE_TEMPLATE/bug_report.yml` and
  `.github/ISSUE_TEMPLATE/config.yml`
- the tap in `README.md`, `docs/using.md`, `docs/configuration.md` and `docs/development.md`

## The name

Keep the binaries' name, `murmur`, unless you are ready to change Go code: the hub serves
`murmur-<os>-<arch>` (`internal/hub/dist.go`, `internal/update/update.go`), Claude Code's settings get the
`murmur` MCP server and its tool permissions (`internal/setup/setup.go`, `internal/harness/claude/settings.go`),
uninstall finds murmur's hooks by `murmur` in their command (`internal/harness/harness.go`), and the
laptop keeps its files in `~/.murmur` (`internal/config/config.go`).
