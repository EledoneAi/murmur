# sqlite with Litestream

The hub image tagged `<version>-litestream`, such as `ghcr.io/eledoneai/murmur-hub:1.0.0-litestream`, is the hub
plus [Litestream](https://litestream.io). Litestream copies the sqlite store to object storage (Amazon S3, Google
Cloud Storage, Azure Blob Storage and others) about once a second. When a container starts without the sqlite
file, Litestream restores it from that copy before the hub starts. `ghcr.io/eledoneai/murmur-hub:litestream`
follows the latest release.

So one instance can keep sqlite where there is no lasting disk, such as ECS on Fargate or Kubernetes without block
volumes, and a hub that has a disk gets a continuous backup.

## One writer, never two

Litestream allows one writer per copy: two instances writing to the same place can leave a copy you can't restore
([Litestream](https://litestream.io/tips/)). Use the image only where the platform stops the old instance before it
starts the new one.

| Where | Use it? |
|---|---|
| A VM with Docker Compose or systemd | Yes, as a backup of the disk. |
| Fly.io, one Machine | Yes, as a backup: a Fly volume isn't replicated. |
| Railway or Render, one instance | Yes, as a backup of the volume or disk. |
| Kubernetes, one replica | Yes. The Helm chart uses the `Recreate` strategy with sqlite, so the old pod stops first. |
| ECS on Fargate, one task | Yes, on a service you deploy with `minimumHealthyPercent=0` and `maximumPercent=100` ([below](#ecs-on-fargate)). |
| ECS Express Mode | No: it always deploys with canaries, old and new tasks side by side. |
| Google Cloud Run | No: an old revision keeps serving WebSockets for up to an hour after a new one starts. |
| Azure Container Apps, App Service | No: old and new revisions or containers overlap during an update. |

Replication is asynchronous: if the container is killed, the last second or so of writes can be lost. A normal
stop sends SIGTERM, and Litestream copies everything before it exits. For the hub, a lost second means at most a
sign-in or a waiting message.

## Settings

The image reads Litestream's settings from `/etc/litestream.yml`, which is [litestream.yml](litestream.yml) in this
folder. It copies `/data/murmur.db`, the image's default `STORE`, to the address in `LITESTREAM_REPLICA_URL`:

| Storage | `LITESTREAM_REPLICA_URL` | Credentials |
|---|---|---|
| Amazon S3 | `s3://<bucket>/murmur` | The task's or pod's IAM role, or `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` |
| Google Cloud Storage | `gs://<bucket>/murmur` | The service account on Google Cloud, or `GOOGLE_APPLICATION_CREDENTIALS` naming a key file |
| Azure Blob Storage | `abs://<account>@<account>.blob.core.windows.net/<container>/murmur` | `LITESTREAM_AZURE_ACCOUNT_KEY` |
| A folder, for trying it | `file:///<path>` | None |

Keep the bucket private, and let the hub's credentials reach only it; Litestream's guides list the permissions each
storage needs ([S3](https://litestream.io/guides/s3/)). Without `LITESTREAM_REPLICA_URL`, the container stops at
once with `file replica path required`.

For other storage (SFTP, S3-compatible services) or other settings, such as how often Litestream takes a full
snapshot, mount your own file at `/etc/litestream.yml`
([reference](https://litestream.io/reference/config/)). Keep the database path in step with `STORE`.

## Try it on your own machine

A Docker volume stands in for the bucket. The image runs as user 65532, so that user must own it:

```sh
docker volume create murmur-replica
docker run --rm -v murmur-replica:/replica alpine chown 65532:65532 /replica
docker run --rm -p 127.0.0.1:8080:8080 -e DEV_LOGIN=1 -e PUBLIC_URL=http://localhost:8080 \
  -e LITESTREAM_REPLICA_URL=file:///replica/murmur -v murmur-replica:/replica \
  ghcr.io/eledoneai/murmur-hub:litestream
```

Sign in at http://localhost:8080, stop the container with Ctrl-C, and start it again: `/data` starts empty each
time, and the log says `restore completed` before the hub starts, with your account still there.

## Kubernetes

With the [Helm chart](../helm/murmur-hub), one replica:

```yaml
image:
  tag: "<version>-litestream"
env:
  LITESTREAM_REPLICA_URL: s3://my-bucket/murmur
# On a cluster without block volumes, /data can be temporary: the copy in the bucket is what lasts.
persistence:
  enabled: false
```

Give the pod its credentials through `serviceAccount.annotations` (IAM roles for service accounts on EKS, Workload
Identity on GKE), or put the keys in the Secret that `existingSecret` names. The chart keeps one replica with sqlite
and uses the `Recreate` strategy.

## ECS on Fargate

Follow the manual path in [deploy/aws](../aws#the-manual-path-your-own-load-balancer), with these changes:

- the image `ghcr.io/eledoneai/murmur-hub:<version>-litestream`;
- no `STORE` and no `BUS` in the task definition, and `LITESTREAM_REPLICA_URL=s3://<bucket>/murmur` in its
  environment;
- a task role (`taskRoleArn`) that may read and write the bucket;
- one task, deployed so the old task stops before the new one starts:

  ```sh
  aws ecs create-service ... --desired-count 1 \
    --deployment-configuration "minimumHealthyPercent=0,maximumPercent=100"
  ```

Each deploy then has a short gap, and laptops reconnect on their own.

## Fly.io, Compose and VMs

Keep the disk or volume, and switch to the `-litestream` image:

- **Fly.io**: set `image` in `fly.toml` to the `-litestream` tag, and the address and credentials with
  `fly secrets set LITESTREAM_REPLICA_URL=s3://<bucket>/murmur AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...`.
  If the volume is ever lost, a new, empty volume is restored from the copy.
- **Docker Compose**: set `MURMUR_IMAGE` to the `-litestream` tag in `.env`, with `LITESTREAM_REPLICA_URL` and the
  credentials. `docker-compose.yml` passes them through.

## Go back in time

Litestream keeps past states for a while and can restore one of them. Stop the hub, then restore into an empty
volume and start the hub on it:

```sh
docker run --rm -v murmur-restored:/data -e LITESTREAM_REPLICA_URL=s3://<bucket>/murmur \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  --entrypoint /usr/local/bin/litestream ghcr.io/eledoneai/murmur-hub:<version>-litestream \
  restore -timestamp 2026-10-01T12:00:00Z /data/murmur.db
```
