# Fly.io

Runs the hub on one Fly Machine, with the sqlite store on a Fly volume. Fly terminates HTTPS and passes
WebSockets through.

## What you need

- A Fly.io account and `flyctl` ([install it](https://fly.io/docs/flyctl/install/)), signed in with
  `fly auth login`.
- An identity provider. [docs/self-hosting.md](../../docs/self-hosting.md) shows how to set up each one.

## Deploy

1. Copy `fly.toml` into an empty folder and edit it:
   - `app`: a name of your own, for example `acme-murmur`. The hub's address becomes
     `https://<app>.fly.dev`.
   - `primary_region`: a region near your team (`fly platform regions` lists them).
   - `PUBLIC_URL`: `https://<app>.fly.dev`, or your own domain once you have added it with `fly certs add`.
   - The sign-in settings, `ALLOWED_DOMAINS` and `ADMINS`.

   Register `PUBLIC_URL` followed by `/auth/callback` as the redirect URL with your identity provider.

2. Create the app and its volume, in the region you chose:

   ```sh
   fly apps create acme-murmur
   fly volumes create murmur_data --region <region> --size 1
   ```

3. Set the secrets. `fly secrets import` reads them from its input as `NAME=VALUE` lines
   ([Fly](https://fly.io/docs/flyctl/secrets-import/)), so they stay off the command line and out of the shell's
   history:

   ```sh
   read -rs OIDC_CLIENT_SECRET    # paste the secret from your identity provider, then press Enter
   printf 'SESSION_SECRET=%s\nOIDC_CLIENT_SECRET=%s\n' "$(openssl rand -base64 32)" "$OIDC_CLIENT_SECRET" |
     fly secrets import
   ```

   For Google or GitHub sign-in, set `GOOGLE_CLIENT_SECRET` or `GITHUB_CLIENT_SECRET` instead, and put the
   matching client ID in `[env]`.

4. Deploy one Machine:

   ```sh
   fly deploy --ha=false
   fly logs
   ```

5. Open `PUBLIC_URL`, sign in, and share the install line it shows.

Keep it at one Machine (`fly scale count 1`): the volume and the sqlite file on it belong to one Machine. For
more than one, use a Postgres store and bus and remove the `[[mounts]]` section; see
[docs/self-hosting.md](../../docs/self-hosting.md).

## Upgrade

Change the image tag in `fly.toml` (or keep `latest`) and run `fly deploy`. Laptops reconnect within seconds
and follow the new version as your team's update policy says
([docs/self-hosting.md](../../docs/self-hosting.md#upgrading)).

## Back up

Fly takes daily snapshots of volumes; `fly volumes snapshots list <volume id>` shows them, and
`fly volumes list` shows the volume id.

A volume lives on one server and isn't replicated, so a daily snapshot can be a day behind. For a copy that keeps
up, use the `-litestream` image, which copies the sqlite file to object storage about once a second: see
[deploy/litestream](../litestream#flyio-compose-and-vms).
