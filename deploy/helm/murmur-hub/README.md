# Helm chart for the murmur hub

Runs the hub on Kubernetes: a Deployment, a Service, an optional Ingress, and a volume for the sqlite store when
there is one replica.

## What you need

- A Kubernetes cluster and Helm 3.
- An ingress controller and a way to get TLS certificates (for example ingress-nginx and cert-manager), or another
  way to route `https://` traffic to a Service.
- A DNS name for the hub, for example `murmur.example.com`.
- An identity provider. [docs/self-hosting.md](../../../docs/self-hosting.md) shows how to set up Google,
  Microsoft Entra, Okta, Keycloak, Authentik, GitLab or GitHub. Register
  `https://murmur.example.com/auth/callback` as the redirect URL.

## Install with one replica (sqlite)

1. Put the secrets in a Secret. Each key becomes an environment variable of the hub:

   ```sh
   kubectl create namespace murmur
   kubectl -n murmur create secret generic murmur-hub \
     --from-literal=SESSION_SECRET="$(openssl rand -base64 32)" \
     --from-literal=OIDC_CLIENT_SECRET='<from your identity provider>'
   ```

2. Write your settings to `values.yaml`:

   ```yaml
   publicURL: https://murmur.example.com
   existingSecret: murmur-hub
   image:
     tag: "<a release, for example 1.0.0>"
   env:
     OIDC_ISSUER: https://login.example.com
     OIDC_CLIENT_ID: murmur
     OIDC_NAME: Example SSO
     ALLOWED_DOMAINS: example.com
     ADMINS: you@example.com
   ingress:
     enabled: true
     className: nginx
     annotations:
       cert-manager.io/cluster-issuer: letsencrypt
     tls:
       - secretName: murmur-tls
         hosts: [murmur.example.com]
   ```

   Helm merges your `ingress.annotations` with the chart's WebSocket timeouts, so you keep both.

3. Install from a checkout of this repository:

   ```sh
   git clone --depth 1 https://github.com/EledoneAi/murmur
   helm install murmur-hub murmur/deploy/helm/murmur-hub -n murmur -f values.yaml
   kubectl -n murmur rollout status deploy/murmur-hub
   ```

4. Open `https://murmur.example.com`, sign in, and share the install line it shows.

The data lives in the PersistentVolumeClaim the chart creates. `helm uninstall` keeps that claim, so the team's
data survives a reinstall; delete it yourself when you mean to.

Unless `bus` is a `postgres://`, `postgresql://`, `redis://` or `rediss://` URL, the chart uses the `Recreate`
strategy, whatever `replicaCount` says: during an upgrade the old pod stops before the new one starts, and laptops
reconnect a few seconds later. Two pods on the local bus would each hold part of the team, and two pods can't share
a sqlite volume.

## More than one replica

Replicas must share the store and the bus, so the chart refuses `replicaCount` above 1 unless `store` is a
`postgres://` or `firestore://` URL and `bus` is a `postgres://`, `redis://` or `rediss://` URL. Keep passwords
in the Secret and refer to them as `$(KEY)`; Kubernetes fills them in:

```sh
kubectl -n murmur create secret generic murmur-hub \
  --from-literal=SESSION_SECRET="$(openssl rand -base64 32)" \
  --from-literal=OIDC_CLIENT_SECRET='<from your identity provider>' \
  --from-literal=POSTGRES_PASSWORD='<the database password>'
```

```yaml
replicaCount: 2
store: postgres://murmur:$(POSTGRES_PASSWORD)@postgres.example.internal:5432/murmur
bus: postgres://murmur:$(POSTGRES_PASSWORD)@postgres.example.internal:5432/murmur
```

A password with characters such as `@`, `:` or `/` must be URL-encoded in the Secret.

## WebSockets and timeouts

Every laptop keeps one WebSocket open to the hub. The chart sets ingress-nginx's read and send timeouts to an hour.
With another controller, raise its idle and stream timeouts to an hour or more, or laptops will reconnect each
time the controller cuts them off. They do reconnect on their own, so a lower limit costs reconnections, not
messages.

## Values

| Value | Default | What it does |
|---|---|---|
| `publicURL` | (required) | The address people and laptops use |
| `existingSecret` | `""` | A Secret whose keys become environment variables (`SESSION_SECRET`, client secrets, passwords) |
| `env` | `{}` | Every other setting as a plain environment variable; see [docs/configuration.md](../../../docs/configuration.md). Every replica gets the same values, so leave `INSTANCE_ID` out |
| `store` | `""` (sqlite in `/data`) | `STORE` for the hub |
| `bus` | `""` (`local://`) | `BUS` for the hub; anything but a Postgres or Redis URL makes upgrades `Recreate` |
| `replicaCount` | `1` | Above 1 needs a networked store and bus |
| `image.repository`, `image.tag` | `ghcr.io/eledoneai/murmur-hub`, appVersion | The image; pin `image.tag` |
| `persistence.*` | enabled, 1Gi | The sqlite volume (one replica only) |
| `ingress.*` | disabled | Host defaults to the host of `publicURL` |
| `serviceAccount.annotations` | `{}` | For example Workload Identity, to reach Firestore on GKE |
| `resources`, `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity` | see `values.yaml` | The usual pod settings |
