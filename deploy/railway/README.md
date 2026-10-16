# Railway

Runs the hub image as one Railway service, with the sqlite store on a Railway volume. Railway terminates HTTPS
and keeps WebSockets open with no time limit
([Railway](https://docs.railway.com/networking/public-networking/specs-and-limits)).

## Things to know first

- **One instance.** A service with a volume can't have replicas, and a redeploy has a short downtime
  ([Railway](https://docs.railway.com/reference/volumes)): laptops disconnect for a moment and reconnect on their
  own.
- **Run it as root.** The image runs as a non-root user, and Railway volumes aren't writable by one until the
  service sets `RAILWAY_RUN_UID=0`.
- **The volume's storage.** Railway doesn't say what kind of storage backs a volume. sqlite needs a local disk,
  not a network file system. If you'd rather not depend on it, use a Postgres store, such as Railway's own
  Postgres, and no volume.

## Deploy

1. Create a project and a service from the Docker image `ghcr.io/eledoneai/murmur-hub:<version>`.
2. Attach a volume to the service, mounted at `/data`. The image's default store is `sqlite:///data/murmur.db`.
3. Give the service a public domain, with target port 8080, or your own domain. `PUBLIC_URL` is `https://`
   followed by it. Register `PUBLIC_URL` followed by `/auth/callback` as the redirect URL with your identity
   provider.
4. Set the variables. Railway injects a `PORT` variable and checks the service's health on that port
   ([Railway](https://docs.railway.com/deployments/healthchecks)), and the hub listens on the port `PORT` names:
   `PORT=8080` keeps both on the target port of step 3.

   ```sh
   RAILWAY_RUN_UID=0
   PORT=8080
   PUBLIC_URL=https://<the service's domain>
   TRUST_PROXY=1
   SESSION_SECRET=<openssl rand -base64 32>
   OIDC_ISSUER=https://login.example.com
   OIDC_CLIENT_ID=murmur
   OIDC_CLIENT_SECRET=<from your identity provider>
   OIDC_NAME=Example SSO
   ALLOWED_DOMAINS=example.com
   ADMINS=you@example.com
   ```

   [docs/configuration.md](../../docs/configuration.md) lists every setting, and
   [docs/self-hosting.md](../../docs/self-hosting.md) shows how to set up each identity provider.

5. Set the health check path to `/healthz`, deploy, open `PUBLIC_URL`, sign in, and share the install line it
   shows.

To upgrade, change the image tag and redeploy. Back up the volume with Railway's volume backups, or copy the sqlite
file as [deploy/compose](../compose#back-up) shows.
