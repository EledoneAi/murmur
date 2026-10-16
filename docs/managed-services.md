# Managed Postgres and Redis

The hub's Postgres store and bus, and its Redis bus, work with the clouds' managed databases as they are. What
matters is which endpoint you give them. Three rules cover most of it; the tables below go service by service.
[self-hosting.md](self-hosting.md#choose-a-store-and-a-bus) says when you need a store and a bus at all.

## Three rules

1. **The Postgres bus needs a session of its own.** Point `BUS` at the database server, or at a pooler in session
   mode. Not at a pooler in transaction mode, and not at RDS Proxy: the bus holds a `LISTEN` open, and a
   transaction pooler hands the connection to someone else between transactions.

   `STORE` may go through a transaction pooler in two cases:
   - its URL adds `default_query_exec_mode=exec`, for example
     `postgres://murmur:<password>@pooler.example.com:6432/murmur?sslmode=require&default_query_exec_mode=exec`;
   - or the pooler is PgBouncer 1.21 or later with `max_prepared_statements` above 0.

   Otherwise the store fails with errors such as `prepared statement "stmtcache_..." does not exist`: the Postgres
   driver, pgx, prepares statements on one connection and the pooler runs them on another
   ([pgx](https://pkg.go.dev/github.com/jackc/pgx/v5#hdr-PgBouncer)).

2. **Azure Managed Redis needs the Enterprise or the Non-clustered clustering policy.** The hub's Redis client
   talks to one server, not to a cluster, and the OSS policy expects a cluster client. Choose when you create the
   cache: the OSS and Enterprise policies can't be changed afterwards
   ([Azure](https://learn.microsoft.com/en-us/azure/redis/choose-clustering-policy)). The cache listens on port
   10000 with TLS, and signs clients in with Microsoft Entra by default. Turn on **Access Keys Authentication**
   under **Authentication**, **Access keys**, then use `rediss://:<access key>@<host>:10000`
   ([Azure](https://learn.microsoft.com/en-us/azure/redis/entra-for-authentication)).

3. **Nothing signs people in before the hub.** Laptops reach the hub with their own tokens, and scripts and GitHub
   call `/install.sh`, `/dl/`, `/cli/token` and `/github/webhook` without a browser. None of them can get past a
   sign-in page the platform puts in front. So don't turn on an Application Load Balancer's `authenticate-oidc`
   action, Easy Auth on Azure Container Apps or App Service, Cloudflare Access, or Google Cloud's Identity-Aware
   Proxy for the hub. The hub signs people in itself, with your identity provider.

## Postgres

| Service | Works? | What to know |
|---|---|---|
| Amazon RDS for PostgreSQL | Yes | Use the instance's endpoint. |
| Amazon Aurora PostgreSQL | Yes | Use the cluster's writer endpoint for both: readers take no writes and no `LISTEN`. The bus's open connection keeps Aurora Serverless v2 from pausing at 0 ACUs ([AWS](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-serverless-v2-auto-pause.html)). |
| Through RDS Proxy | The store, yes | Point `BUS` at the database's own endpoint. Through the proxy, `LISTEN` pins a connection to the bus for good ([AWS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-proxy-pinning.html)). |
| Amazon Aurora DSQL | No | A transaction holds one schema change at most, never with other writes, and changes at most 3,000 rows ([AWS](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/working-with-postgresql-compatibility-migration-guide.html#working-with-postgresql-compatibility-unsupported-limitations)). Clients sign in with IAM authentication tokens instead of passwords ([AWS](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/SECTION_authentication-token.html)). AWS's guide to it never mentions `LISTEN`, which the bus needs. |
| Azure Database for PostgreSQL flexible server | Yes, on port 5432 | Add `sslmode=require`. Its built-in PgBouncer, on port 6432, runs in transaction mode and isn't offered on Burstable servers: the store may use it as rule 1 says, the bus never ([Azure](https://learn.microsoft.com/en-us/azure/postgresql/connectivity/concepts-pgbouncer)). |
| Google Cloud SQL for PostgreSQL | Yes, direct | Managed Connection Pooling in transaction mode breaks `LISTEN`: keep the bus on a direct connection ([Google](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling)). |
| Neon | Yes; the bus on the direct endpoint | Endpoints with `-pooler` in their name go through PgBouncer in transaction mode, which supports prepared statements (`max_prepared_statements=1000`) but not `LISTEN` ([Neon](https://neon.com/docs/connect/connection-pooling)): the store may use one, as rule 1 says, the bus never. Scale to zero suspends the compute after 5 minutes without queries and closes its connections ([Neon](https://neon.com/docs/introduction/compute-lifecycle)); turn it off for the hub, which takes a paid plan ([Neon](https://neon.com/docs/guides/scale-to-zero-guide)). |
| Supabase | Direct, or the session pooler on port 5432 | The transaction pooler on port 6543 breaks `LISTEN` ([Supabase](https://supabase.com/docs/guides/database/connecting-to-postgres)). |

## Redis

Redis is only ever the bus. It keeps nothing, so it needs no persistence or backups.

| Service | Works? | What to know |
|---|---|---|
| Amazon ElastiCache Serverless, Valkey or Redis OSS | Yes | TLS is always on: use `rediss://`. Without a public endpoint, which takes Valkey 9.0 or later, the cache is reachable only through its VPC ([AWS](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/WhatIs.deployment.html#WhatIs.deployment.comparing)). |
| Amazon ElastiCache, node-based | Yes | Cluster mode off is the simple choice. With cluster mode on, classic pub/sub reaches every node ([Valkey](https://valkey.io/topics/pubsub/)). Use `rediss://` when in-transit encryption is on. |
| Azure Managed Redis | Yes, with the Enterprise or Non-clustered policy | See rule 2. |
| Azure Cache for Redis | Yes, but retiring | Closed to new customers since 1 April 2026. Basic, Standard and Premium retire on 30 September 2028, Enterprise on 31 March 2027 ([Microsoft](https://techcommunity.microsoft.com/blog/azure-managed-redis/azure-cache-for-redis-retirement-what-to-know-and-how-to-prepare/4458721)). Use Azure Managed Redis for anything new. |
| Google Memorystore for Valkey | Yes | In-transit encryption signs with a certificate authority of the instance's own: see below ([Google](https://docs.cloud.google.com/memorystore/docs/valkey/about-in-transit-encryption)). |
| Upstash | Yes, over `rediss://` | Billed per command ([Upstash](https://upstash.com/pricing/redis)). |

## A certificate authority of the service's own

`rediss://` URLs, and Postgres URLs with `sslmode=verify-ca` or `verify-full`, check the server's certificate
against the certificate authorities in the image. Some services sign with one of their own, such as Memorystore's
in-transit encryption or your company's private authority. Give the hub that authority's certificate in a file:

- **Postgres**: add `sslrootcert=<path to the file>` to the URL.
- **Redis**: the hub, a Go program on Linux, reads two environment variables. `SSL_CERT_DIR` names a folder of extra
  certificates and keeps the image's own: mount the file at `/etc/murmur/ca/server-ca.pem`, for example, and set
  `SSL_CERT_DIR=/etc/murmur/ca`. `SSL_CERT_FILE` instead replaces the image's certificates, so that file must also
  hold the public ones, or sign-in with your identity provider fails.

`sslmode=require` encrypts without checking the server's certificate, so it needs neither.
