# Deploying a hub

[docs/self-hosting.md](../docs/self-hosting.md) helps you choose a platform, a store and a bus. Each folder here
is one way to run the hub, with a README you can follow from start to finish:

| Folder | What it runs |
|---|---|
| [compose](compose) | Docker Compose on one server, with Caddy for HTTPS; also two instances with Postgres |
| [vm](vm) | What to set up at Hetzner, Lightsail, DigitalOcean, EC2 or Azure before the Compose recipe |
| [helm/murmur-hub](helm/murmur-hub) | A Helm chart for Kubernetes |
| [cloudrun](cloudrun) | Google Cloud Run, with Firestore or Cloud SQL |
| [fly](fly) | Fly.io, one Machine with a volume |
| [railway](railway) | Railway, one service with a volume |
| [render](render) | Render, one web service with a disk |
| [aws](aws) | AWS: ECS Express Mode, or ECS on Fargate behind your own load balancer, with RDS for PostgreSQL |
| [azure](azure) | Azure Container Apps or App Service, with Azure Database for PostgreSQL |
| [systemd](systemd) | The release binary as a hardened systemd service |
| [litestream](litestream) | The hub image with Litestream: one instance keeps sqlite, copied to object storage as it changes |

The other folders are for building and releasing murmur itself: [docker](docker) holds the hub's Dockerfiles,
and [sign](sign) signs a release's checksums (see [docs/development.md](../docs/development.md)).
