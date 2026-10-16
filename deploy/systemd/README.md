# One server with systemd

Runs the hub binary from a release as a hardened systemd service, with the sqlite store in
`/var/lib/murmur`. A reverse proxy on the same server, Caddy here, serves HTTPS.

## What you need

- A Linux server (amd64 or arm64) with systemd, and `curl`.
- A DNS name for the hub, for example `murmur.example.com`, pointing at the server, with ports 80 and 443
  open.
- An identity provider. [docs/self-hosting.md](../../docs/self-hosting.md) shows how to set up each one.

## Install

1. Download a release: the hub, the four laptop binaries it serves, and the signed checksums. Check them:

   ```sh
   VERSION=2.0.1     # from https://github.com/EledoneAi/murmur/releases
   ARCH=amd64        # or arm64
   BASE="https://github.com/EledoneAi/murmur/releases/download/v$VERSION"
   cd "$(mktemp -d)"
   curl -fsSLO "$BASE/murmur-hub_${VERSION}_linux_$ARCH.tar.gz"
   curl -fsSLO "$BASE/checksums.txt"
   curl -fsSLO "$BASE/checksums.txt.sig"
   for t in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64; do curl -fsSLO "$BASE/murmur-$t"; done
   sha256sum --check --ignore-missing checksums.txt
   ```

   `sha256sum` checks the downloads against `checksums.txt`. The hub checks `checksums.txt.sig` against the release
   key built into it when it starts, and refuses to start if it doesn't verify.

2. Put them in place. The laptop binaries and the checksums go in the folder `DIST_DIR` names:

   ```sh
   tar -xzf "murmur-hub_${VERSION}_linux_$ARCH.tar.gz" murmur-hub
   sudo install -m 0755 murmur-hub /usr/local/bin/murmur-hub
   sudo install -d /usr/share/murmur/dist
   sudo install -m 0644 murmur-darwin-* murmur-linux-* checksums.txt checksums.txt.sig /usr/share/murmur/dist/
   ```

3. Download the settings file and the service from the same release's sources (the release files don't carry
   them), then write the settings:

   ```sh
   RAW="https://raw.githubusercontent.com/EledoneAi/murmur/v$VERSION/deploy/systemd"
   curl -fsSLO "$RAW/hub.env.example"
   curl -fsSLO "$RAW/murmur-hub.service"
   sudo install -d -m 0700 /etc/murmur
   sudo install -m 0600 hub.env.example /etc/murmur/hub.env
   sudo ${EDITOR:-nano} /etc/murmur/hub.env
   ```

   Set `PUBLIC_URL`, `SESSION_SECRET` (`openssl rand -base64 32`), one sign-in provider, `ALLOWED_DOMAINS` (or
   `ALLOWED_EMAILS`) and `ADMINS`. Register `PUBLIC_URL` followed by `/auth/callback` as the redirect URL with
   your identity provider.

4. Install and start the service you downloaded in step 3:

   ```sh
   sudo install -m 0644 murmur-hub.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now murmur-hub
   journalctl -u murmur-hub -f
   ```

   The service runs as a user systemd makes up for it, can write only to `/var/lib/murmur`, and can't gain
   privileges. `systemd-analyze security murmur-hub` rates it.

5. Put HTTPS in front. With [Caddy](https://caddyserver.com/docs/install), `/etc/caddy/Caddyfile` needs only:

   ```
   murmur.example.com {
   	reverse_proxy localhost:8080
   }
   ```

   Then `sudo systemctl reload caddy`. Caddy gets the certificate and passes WebSockets through without a time
   limit. With nginx, see [docs/self-hosting.md](../../docs/self-hosting.md#reverse-proxies-and-websockets).

   The hub listens on port 8080 on every interface: let your firewall allow that port only from the proxy.

6. Open `PUBLIC_URL`, sign in, and share the install line it shows.

## Upgrade

Repeat steps 1 and 2 with the new version, then `sudo systemctl restart murmur-hub`. Laptops reconnect within
seconds and follow the new version as your team's update policy says
([docs/self-hosting.md](../../docs/self-hosting.md#upgrading)).

## Back up

The sqlite file is everything that matters. `sqlite3` copies it safely while the hub runs:

```sh
sudo sqlite3 /var/lib/murmur/murmur.db ".backup /root/murmur-$(date +%F).db"
```
