# Render

Runs the hub image as one Render web service, with the sqlite store on a Render persistent disk. Render
terminates HTTPS and sets no maximum on how long a WebSocket stays open
([Render](https://render.com/docs/websocket)).

## Things to know first

- **A paid instance type.** Free web services sleep when idle, which drops every laptop, and can't have a disk.
- **One instance.** A disk belongs to one instance and turns off zero-downtime deploys: Render stops the old
  instance before it starts the new one ([Render](https://render.com/docs/disks)). Laptops disconnect for a
  moment at each deploy and reconnect on their own.
- **Backups.** Render snapshots each disk every 24 hours and keeps the snapshots for at least seven days.
- **Check the first write.** The image runs as user 65532, and Render doesn't say which users may write to a disk.
  The hub creates `/data/murmur.db` as it starts: if the first deploy fails and the log shows a `STORE:` error
  about that file, the disk isn't writable by that user. Then use a Postgres store, such as Render Postgres, and no
  disk.

## Deploy

1. Create a web service from an existing image: `ghcr.io/eledoneai/murmur-hub:<version>`.
2. Pick a paid instance type.
3. Under **Advanced**, add a disk mounted at `/data` (1 GB is plenty; the image's default store is
   `sqlite:///data/murmur.db`) and set the health check path to `/healthz`.
4. Set the environment variables. `PORT=8080` makes Render send traffic to the port the hub listens on.
   `PUBLIC_URL` is the service's `onrender.com` address, or your own domain once you have added it. Register
   `PUBLIC_URL` followed by `/auth/callback` as the redirect URL with your identity provider.

   ```sh
   PORT=8080
   PUBLIC_URL=https://<service>.onrender.com
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

5. Deploy, and check in the log that the hub started ([Things to know first](#things-to-know-first)). Open
   `PUBLIC_URL`, sign in, and share the install line it shows.

A service that runs an image doesn't deploy on its own when a new release comes out: change the tag and deploy it
by hand ([Render](https://render.com/docs/web-services)).
