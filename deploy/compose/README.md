# Docker Compose

Two recipes:

- `docker-compose.yml`: one server. The hub keeps its data in a sqlite file on a Docker volume, and Caddy gets
  the HTTPS certificate. Right for most teams.
- `compose.ha.yml`: two hub instances sharing Postgres for the store and the bus, behind the same Caddy. Use it
  when one instance going down for a minute is not acceptable.

## What you need

- A Linux server with Docker and the Compose plugin (`docker compose version` works).
- A DNS name for the hub, for example `murmur.example.com`, pointing at the server.
- Ports 80 and 443 open to the internet, or at least to your team and to Let's Encrypt. Caddy uses port 80 to
  prove it owns the name.
- An identity provider your team signs in with. [docs/self-hosting.md](../../docs/self-hosting.md) shows how to
  set up Google, Microsoft Entra, Okta, Keycloak, Authentik, GitLab or GitHub.

## One server

1. Copy this folder to the server:

   ```sh
   git clone --depth 1 https://github.com/EledoneAi/murmur
   cd murmur/deploy/compose
   ```

2. Create the settings file and fill it in:

   ```sh
   cp env.example .env
   openssl rand -base64 32   # paste the result as SESSION_SECRET
   ${EDITOR:-nano} .env
   ```

   Set `PUBLIC_URL`, `SESSION_SECRET`, one sign-in provider, `ALLOWED_DOMAINS` (or `ALLOWED_EMAILS`) and
   `ADMINS`. The lists and `TOKEN_DAYS` are read only when the hub first starts; after that, admins change them
   in the hub's admin panel. In your identity provider, register `PUBLIC_URL` followed by `/auth/callback` as the redirect URL,
   for example `https://murmur.example.com/auth/callback`.

3. Start it:

   ```sh
   docker compose up -d
   docker compose logs -f hub    # Ctrl-C stops following the log, not the hub
   ```

4. Open `PUBLIC_URL` in a browser and sign in. The page shows the install line for laptops:

   ```sh
   curl -fsSL https://murmur.example.com/install.sh | sh
   ```

To pin a version instead of following `latest`, set `MURMUR_IMAGE=ghcr.io/eledoneai/murmur-hub:<version>`
in `.env`.

## Two instances

Use `compose.ha.yml` the same way, with one more line in `.env`:

```sh
POSTGRES_PASSWORD=<a long random password>
```

```sh
docker compose -f compose.ha.yml up -d
```

The store and the bus both use the Postgres this file starts. To carry events between the instances over Redis
instead, add `BUS=redis://redis:6379` to `.env` and start with `docker compose -f compose.ha.yml --profile redis up -d`.

Both instances run on the same server here, so this protects against a hub crashing or being upgraded, not
against losing the server. To spread instances over several servers, run Postgres (or Redis) where all of them
can reach it and use the [Helm chart](../helm/murmur-hub) or your platform's own scaling.

## Upgrade

A newer image carries the matching laptop binaries. Once the hub runs it, laptops follow it as your team's update
policy says ([docs/self-hosting.md](../../docs/self-hosting.md#upgrading)).

```sh
docker compose pull
docker compose up -d
```

## Back up

With one server, everything that matters is the sqlite file in the `murmur_hub-data` volume. This copies it
while the hub keeps running (the image has no shell, so a throwaway container reads the volume):

```sh
docker run --rm -v murmur_hub-data:/data -v "$PWD":/backup alpine \
  sh -c 'apk add -q sqlite && sqlite3 /data/murmur.db ".backup /backup/murmur-$(date +%F).db"'
```

For a copy that keeps up as the file changes, run the `-litestream` image, which copies it to object storage about
once a second: see [deploy/litestream](../litestream).

With `compose.ha.yml`, back up Postgres:

```sh
docker compose -f compose.ha.yml exec postgres pg_dump -U murmur murmur > murmur-$(date +%F).sql
```

## Try it on your own machine

From the repository root, `make compose-up` builds the image from source and starts `docker-compose.yml` on
`http://localhost` with dev sign-in, where you type any email. It needs port 80 free. `make compose-down` stops
it.
