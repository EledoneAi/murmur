# Helm chart for the murmur hub

Runs the hub on Kubernetes: a Deployment, a Service, an optional Ingress or Gateway API HTTPRoute, and a volume for
the sqlite store when there is one replica.

## What you need

- A Kubernetes cluster and Helm 3 or later.
- A way to route `https://` traffic to a Service: an ingress controller, or a Gateway API implementation with a
  Gateway, and a way to get TLS certificates (for example cert-manager).
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

2. Write your settings to `values.yaml`. With an Ingress:

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
     className: traefik # your controller's IngressClass
     annotations:
       cert-manager.io/cluster-issuer: letsencrypt
     tls:
       - secretName: murmur-tls
         hosts: [murmur.example.com]
   ```

   Or, with a Gateway you already run, replace the `ingress` section with an HTTPRoute. The Gateway's HTTPS
   listener holds the certificate.

   ```yaml
   httpRoute:
     enabled: true
     parentRefs:
       - name: public
         namespace: gateway-system
   ```

   Check [WebSockets and timeouts](#websockets-and-timeouts) for your controller or gateway.

3. Install from a checkout of this repository:

   ```sh
   git clone --depth 1 https://github.com/EledoneAi/murmur
   helm install murmur-hub murmur/deploy/helm/murmur-hub -n murmur -f values.yaml
   kubectl -n murmur rollout status deploy/murmur-hub
   ```

4. Open `https://murmur.example.com`, sign in, and share the install line it shows.

The data lives in the PersistentVolumeClaim the chart creates. `helm uninstall` keeps that claim, so the team's
data survives a reinstall; delete it yourself when you mean to. Give it a block storage class (the default on most
clusters), not NFS or another network file system: sqlite isn't safe there.

With sqlite, the chart uses the `Recreate` strategy, whatever `replicaCount` says: during an upgrade the old pod
stops, letting go of the sqlite file and its ReadWriteOnce volume, before the new one starts. Laptops reconnect a few
seconds later. It does the same with the local bus, where two pods would each hold part of the team. Rolling
updates need a `store` that is a Postgres or Firestore URL and a `bus` that is a Postgres or Redis URL.

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

A password with characters such as `@`, `:` or `/` must be URL-encoded in the Secret. With a managed database, see
[docs/managed-services.md](../../../docs/managed-services.md) for which endpoint the bus may use.

## WebSockets and timeouts

Every laptop keeps one WebSocket open to the hub, and laptops and the hub ping each other every 25 seconds, so any
idle timeout of a minute or more works. What cuts laptops off is a limit on how long a whole request may last: to
a proxy, a WebSocket is one request that lasts as long as the laptop stays connected. When a proxy does cut one, the
laptop reconnects on its own and resends what wasn't confirmed: a reconnection, not lost messages.

- **Gateway API HTTPRoute**: never set `timeouts.request` or `timeouts.backendRequest` on the route; some
  implementations end a WebSocket when it reaches that age. Leave `httpRoute.timeouts` empty. If your
  implementation applies a request timeout by default, set `httpRoute.timeouts.request: 0s`, which the Gateway API
  defines as no timeout ([Gateway API](https://gateway-api.sigs.k8s.io/guides/http-timeouts/)).
- **Envoy Gateway, and other gateways built on Envoy**, close a stream after 5 minutes without data by default
  ([kgateway](https://kgateway.dev/docs/envoy/main/resiliency/timeouts/idle-stream/)). The pings keep it busy.
- **GKE Gateway**: the load balancer's backend timeout is 30 seconds by default, and for a WebSocket it caps the
  connection's whole life
  ([Google](https://docs.cloud.google.com/load-balancing/docs/https/request-distribution)). Attach a
  GCPBackendPolicy to the hub's Service with `timeoutSec` of 3600 or more
  ([GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-gateway-resources)); laptops then
  reconnect once an hour:

  ```yaml
  apiVersion: networking.gke.io/v1
  kind: GCPBackendPolicy
  metadata:
    name: murmur-hub
    namespace: murmur
  spec:
    default:
      timeoutSec: 3600
    targetRef:
      group: ""
      kind: Service
      name: murmur-hub # the Service the chart created
  ```

- **Traefik** passes WebSockets through with its defaults. It also understands many ingress-nginx annotations,
  which helps when you move an Ingress over from it
  ([SUSE](https://documentation.suse.com/cloudnative/rke2/latest/en/reference/ingress_migration.html)).
- **AKS**: the managed NGINX of the application routing add-on gets only critical fixes until November 2026
  ([AKS](https://blog.aks.azure.com/2026/03/18/app-routing-gateway-api)). It works with its defaults, like
  ingress-nginx below; for the longer run, use a Gateway API implementation and the HTTPRoute.
- **ingress-nginx** was archived on 24 March 2026
  ([Kubernetes](https://kubernetes.io/blog/2025/11/11/ingress-Nginx-retirement/)). Its 60-second read and send
  timeouts work with the pings. The chart no longer sets its annotations; to leave a margin, add them yourself:

  ```yaml
  ingress:
    className: nginx
    annotations:
      nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
      nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
  ```

- **Other controllers**: allow WebSocket upgrades, and make sure nothing limits how long a request may last, or set
  that limit to an hour or more.

## Values

| Value | Default | What it does |
|---|---|---|
| `publicURL` | (required) | The address people and laptops use |
| `existingSecret` | `""` | A Secret whose keys become environment variables (`SESSION_SECRET`, client secrets, passwords) |
| `env` | `{}` | Every other setting as a plain environment variable; see [docs/configuration.md](../../../docs/configuration.md). Every replica gets the same values, so leave `INSTANCE_ID` out |
| `store` | `""` (sqlite in `/data`) | `STORE` for the hub |
| `bus` | `""` (`local://`) | `BUS` for the hub; rolling updates need a Postgres or Redis URL here and a networked `store` |
| `replicaCount` | `1` | Above 1 needs a networked store and bus |
| `image.repository`, `image.tag` | `ghcr.io/eledoneai/murmur-hub`, appVersion | The image; pin `image.tag` |
| `persistence.*` | enabled, 1Gi | The sqlite volume (one replica only) |
| `ingress.*` | disabled, no annotations | `className` and `annotations` are your controller's; the host defaults to the host of `publicURL` |
| `httpRoute.*` | disabled | A Gateway API HTTPRoute; `parentRefs` is required, `hostnames` defaults to the host of `publicURL`, `timeouts` stays unset |
| `serviceAccount.annotations` | `{}` | For example Workload Identity, to reach Firestore on GKE |
| `resources`, `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity` | see `values.yaml` | The usual pod settings |
