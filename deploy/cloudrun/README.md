# Google Cloud Run

Cloud Run runs the hub image without servers to manage. The commands below set up one instance with Firestore as
the store. Replace `PROJECT`, `REGION` and the example values with yours.

## Things to know first

- **Connections last at most an hour.** Cloud Run treats each laptop's WebSocket as one request and ends it at
  the request timeout, 60 minutes at most. Laptops reconnect on their own; `--timeout=3600` makes that happen
  as rarely as Cloud Run allows.
- **Keep an instance running.** With `--min-instances=1` the hub is always there when a laptop connects, and its
  background jobs (expiring messages, the release check, chat pings and the weekly report) keep running when
  nobody is connected.
  `--no-cpu-throttling` gives it CPU between requests for those jobs.
- **Concurrency is connections.** Each connected laptop holds one request open, so `--concurrency` is how many
  laptops one instance takes. Set it to 1000, the most Cloud Run allows.
- **No local disk.** Files in a Cloud Run container vanish when the instance stops, so sqlite is not an option.
  Use Firestore (below) or Cloud SQL for PostgreSQL.
- **One instance needs no bus.** For more than one, see the end of this page.

## 1. Enable the APIs and set variables

```sh
PROJECT=my-project
REGION=europe-west1
VERSION=1.0.0           # a release from https://github.com/EledoneAi/murmur/releases
gcloud config set project "$PROJECT"
gcloud services enable run.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com firestore.googleapis.com
```

## 2. Copy the image into Artifact Registry

Cloud Run pulls images from Artifact Registry and Docker Hub, not from ghcr.io. This copies the release image,
both architectures, without downloading it:

```sh
gcloud artifacts repositories create murmur --repository-format=docker --location="$REGION"
gcloud auth configure-docker "$REGION-docker.pkg.dev"
docker buildx imagetools create \
  --tag "$REGION-docker.pkg.dev/$PROJECT/murmur/murmur-hub:$VERSION" \
  "ghcr.io/eledoneai/murmur-hub:$VERSION"
```

## 3. Create the Firestore database and a service account

```sh
gcloud firestore databases create --database=murmur --location="$REGION"
gcloud iam service-accounts create murmur-hub
SA="murmur-hub@$PROJECT.iam.gserviceaccount.com"
gcloud projects add-iam-policy-binding "$PROJECT" --member="serviceAccount:$SA" --role=roles/datastore.user
```

## 4. Store the secrets

```sh
openssl rand -base64 32 | tr -d '\n' | gcloud secrets create murmur-session-secret --data-file=-
printf '%s' '<client secret from your identity provider>' | gcloud secrets create murmur-oidc-client-secret --data-file=-
for s in murmur-session-secret murmur-oidc-client-secret; do
  gcloud secrets add-iam-policy-binding "$s" --member="serviceAccount:$SA" --role=roles/secretmanager.secretAccessor
done
```

## 5. Deploy

The service's address is known before the first deploy, so `PUBLIC_URL` can be set right away. Register
`PUBLIC_URL` followed by `/auth/callback` as the redirect URL with your identity provider.

```sh
PROJECT_NUMBER="$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')"
PUBLIC_URL="https://murmur-hub-$PROJECT_NUMBER.$REGION.run.app"
```

Write the settings to `env.yaml`. A file keeps values with commas, such as several allowed domains, intact:

```sh
cat > env.yaml <<EOF
PUBLIC_URL: "$PUBLIC_URL"
TRUST_PROXY: "1"
STORE: "firestore://$PROJECT/murmur"
LOG_FORMAT: gcp
OIDC_ISSUER: https://login.example.com
OIDC_CLIENT_ID: murmur
OIDC_NAME: Example SSO
ALLOWED_DOMAINS: example.com
ADMINS: you@example.com
EOF
```

```sh
gcloud run deploy murmur-hub \
  --image="$REGION-docker.pkg.dev/$PROJECT/murmur/murmur-hub:$VERSION" \
  --region="$REGION" \
  --service-account="$SA" \
  --allow-unauthenticated \
  --timeout=3600 \
  --min-instances=1 --max-instances=1 \
  --concurrency=1000 \
  --no-cpu-throttling \
  --cpu=1 --memory=512Mi \
  --env-vars-file=env.yaml \
  --set-secrets="SESSION_SECRET=murmur-session-secret:latest,OIDC_CLIENT_SECRET=murmur-oidc-client-secret:latest"
```

`--allow-unauthenticated` lets laptops and browsers reach the hub, which does its own sign-in. If your
organization's policy forbids public services, put the hub behind your own load balancer instead.

`LOG_FORMAT=gcp` writes logs with the field names Cloud Logging understands, so severities show up as such.

Open `$PUBLIC_URL`, sign in, and share the install line it shows. To use your own domain, map it to the service
and set `PUBLIC_URL` to it.

## Upgrade

Copy the newer image (step 2 with the new `VERSION`) and deploy it:

```sh
gcloud run deploy murmur-hub --region="$REGION" \
  --image="$REGION-docker.pkg.dev/$PROJECT/murmur/murmur-hub:$VERSION"
```

The new revision takes the connections, and laptops follow its version as your team's update policy says
([docs/self-hosting.md](../../docs/self-hosting.md#upgrading)).

## Cloud SQL for PostgreSQL instead of Firestore

The store reaches Cloud SQL through the socket Cloud Run mounts. The URL holds the password, so keep it in Secret
Manager:

```sh
INSTANCE="$PROJECT:$REGION:<instance name>"
printf '%s' "postgres://murmur:<password>@/murmur?host=/cloudsql/$INSTANCE" \
  | gcloud secrets create murmur-store-url --data-file=-
gcloud secrets add-iam-policy-binding murmur-store-url --member="serviceAccount:$SA" \
  --role=roles/secretmanager.secretAccessor
gcloud projects add-iam-policy-binding "$PROJECT" --member="serviceAccount:$SA" --role=roles/cloudsql.client
```

Then remove the `STORE` line from `env.yaml` and deploy as in step 5, with two changes: add
`--add-cloudsql-instances="$INSTANCE"`, and add `STORE=murmur-store-url:latest` to `--set-secrets`.

## More than one instance

Instances must share the store (Firestore or Cloud SQL both work) and pass events over a bus:

- **Postgres bus**: with Cloud SQL, add `BUS=murmur-store-url:latest` to `--set-secrets`: the bus can use the
  same database as the store.
- **Redis bus**: create a Memorystore for Redis instance, give the service a way into its network
  (`--network`, `--subnet` and `--vpc-egress=private-ranges-only`), and add `BUS: redis://<memorystore ip>:6379` to `env.yaml`,
  or `rediss://` when the instance has in-transit encryption.

Then raise `--min-instances` and `--max-instances`. Cloud Run spreads new connections over the instances.
