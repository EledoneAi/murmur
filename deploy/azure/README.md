# Azure

Two ways to run the hub on Azure, both with Azure Database for PostgreSQL flexible server as the store:

- **Azure Container Apps**, the one this page walks through from start to finish.
- **App Service for Containers** on Linux, [further down](#app-service-for-containers).

Replace the example values with yours. Sign-in with Microsoft Entra ID is set up as
[docs/self-hosting.md](../../docs/self-hosting.md#microsoft-entra-id) shows.

## Things to know first

- **No disk for sqlite.** Container Apps storage is either temporary or an Azure Files share, and App Service keeps
  files on a network share; sqlite isn't safe on either
  ([Container Apps](https://learn.microsoft.com/en-us/azure/container-apps/storage-mounts),
  [App Service](https://learn.microsoft.com/en-us/troubleshoot/azure/app-service/faqs-app-service-linux-new)). Use
  Postgres. Nor sqlite copied by the [Litestream image](../litestream): during an update the old revision or
  container keeps running next to the new one, and Litestream allows only one writer.
- **Keep one replica running.** `--min-replicas 1` keeps the hub up when nobody is connected, so its background jobs
  run and laptops find it at once.
- **Give the hub a bus too.** With `BUS` set to the same Postgres URL as the store, the old and the new revision
  both see the whole team while an update rolls out.
- **Connections.** Container Apps documents a request timeout of 240 seconds on its HTTP ingress
  ([Azure](https://learn.microsoft.com/en-us/azure/container-apps/ingress-overview)). Laptops and the hub ping each
  other every 25 seconds, which keeps a WebSocket from sitting idle. If the ingress still ends connections after
  four minutes, laptops reconnect on their own: a reconnection, not lost messages.
- **No platform sign-in.** Leave the app's **Authentication** (Easy Auth) off: laptops can't get through it. The
  hub signs people in itself ([docs/managed-services.md](../../docs/managed-services.md#three-rules)).
- **Cost**, approximately: 0.25 vCPU on Container Apps about $14 a month (an open WebSocket keeps the app active,
  so the idle rate doesn't apply), a Burstable B1ms database about $12 plus storage, and about $26 for the load
  balancer and two public IP addresses that an environment in your own virtual network brings: about $55 a month
  ([billing](https://learn.microsoft.com/en-us/azure/container-apps/billing),
  [virtual network](https://learn.microsoft.com/en-us/azure/container-apps/custom-virtual-networks#managed-resources)).

## 1. Set variables

```sh
RG=murmur
LOCATION=westeurope
VERSION=1.0.0           # a release from https://github.com/EledoneAi/murmur/releases
az extension add --name containerapp --upgrade
az group create --name "$RG" --location "$LOCATION"
```

## 2. Create the network and the database

The database gets no public address. It lives in a virtual network, in a subnet of its own, and the Container Apps
environment joins the same network through another subnet
([Azure](https://learn.microsoft.com/en-us/azure/postgresql/network/concepts-networking-private)). A private DNS
zone, linked to the network, lets the app find the server by its usual name.

```sh
az network vnet create --resource-group "$RG" --name murmur-vnet --location "$LOCATION" \
  --address-prefixes 10.42.0.0/16
az network vnet subnet create --resource-group "$RG" --vnet-name murmur-vnet --name apps \
  --address-prefixes 10.42.0.0/27 --delegations Microsoft.App/environments
az network vnet subnet create --resource-group "$RG" --vnet-name murmur-vnet --name db \
  --address-prefixes 10.42.1.0/28 --delegations Microsoft.DBforPostgreSQL/flexibleServers
az network private-dns zone create --resource-group "$RG" --name murmur.private.postgres.database.azure.com
az network private-dns link vnet create --resource-group "$RG" --zone-name murmur.private.postgres.database.azure.com \
  --name murmur-vnet --virtual-network murmur-vnet --registration-enabled false

az postgres flexible-server create --resource-group "$RG" --name <server name> --location "$LOCATION" \
  --tier Burstable --sku-name Standard_B1ms --storage-size 32 \
  --admin-user murmur --admin-password '<a long password>' \
  --vnet murmur-vnet --subnet db --private-dns-zone murmur.private.postgres.database.azure.com
az postgres flexible-server db create --resource-group "$RG" --server-name <server name> --name murmur
```

Don't use `--public-access 0.0.0.0` instead: it lets in connections from any resource in Azure, including other
customers' ([Azure](https://learn.microsoft.com/en-us/azure/postgresql/network/concepts-networking-public#allow-all-azure-ip-addresses)).
Nor a firewall rule for the app's outbound addresses: on Container Apps they can change over time
([Azure](https://learn.microsoft.com/en-us/azure/container-apps/networking#ports-and-ip-addresses)).

The store's URL is then:

```
postgres://murmur:<password>@<server name>.postgres.database.azure.com:5432/murmur?sslmode=require
```

URL-encode characters such as `@`, `:` or `/` in the password. Use port 5432, not the built-in PgBouncer on 6432,
which runs in transaction mode ([docs/managed-services.md](../../docs/managed-services.md#postgres)).

## 3. Create the environment

The environment goes in the `apps` subnet. It can't move to another network later.

```sh
az containerapp env create --name murmur-env --resource-group "$RG" --location "$LOCATION" \
  --infrastructure-subnet-resource-id "$(az network vnet subnet show --resource-group "$RG" \
    --vnet-name murmur-vnet --name apps --query id --output tsv)"
```

The app's address will be `https://murmur-hub.<the environment's default domain>`:

```sh
PUBLIC_URL="https://murmur-hub.$(az containerapp env show --name murmur-env --resource-group "$RG" \
  --query properties.defaultDomain --output tsv)"
echo "$PUBLIC_URL"
```

Register `PUBLIC_URL` followed by `/auth/callback` as the redirect URL with your identity provider.

## 4. Create the app

The secrets live in the app, and the environment variables refer to them with `secretref:`. To keep them in Key
Vault instead, see [below](#secrets-in-key-vault).

```sh
az containerapp create --name murmur-hub --resource-group "$RG" --environment murmur-env \
  --image "ghcr.io/eledoneai/murmur-hub:$VERSION" \
  --ingress external --target-port 8080 \
  --min-replicas 1 --max-replicas 1 \
  --cpu 0.25 --memory 0.5Gi \
  --secrets session-secret="$(openssl rand -base64 32)" \
    oidc-client-secret='<the secret value from Entra>' \
    database-url='postgres://murmur:<password>@<server name>.postgres.database.azure.com:5432/murmur?sslmode=require' \
  --env-vars PUBLIC_URL="$PUBLIC_URL" TRUST_PROXY=1 \
    OIDC_ISSUER='https://login.microsoftonline.com/<directory (tenant) ID>/v2.0' \
    OIDC_CLIENT_ID='<application (client) ID>' OIDC_NAME=Microsoft OIDC_TRUST_EMAIL=1 \
    ALLOWED_DOMAINS=example.com ADMINS=you@example.com \
    SESSION_SECRET=secretref:session-secret OIDC_CLIENT_SECRET=secretref:oidc-client-secret \
    STORE=secretref:database-url BUS=secretref:database-url \
  --query properties.configuration.ingress.fqdn
```

The command prints the app's address; it should match `PUBLIC_URL`. Open `PUBLIC_URL`, sign in, and share the
install line it shows. `az containerapp logs show --name murmur-hub --resource-group "$RG" --follow` shows the
hub's log.

## 5. Your own domain

Container Apps gives a free managed certificate for a custom domain
([Azure](https://learn.microsoft.com/en-us/azure/container-apps/custom-domains-managed-certificates)). For a
subdomain such as `murmur.example.com`:

1. Create two DNS records: a CNAME from `murmur` to the app's address (directly, not through another CNAME or a
   proxy such as Cloudflare's), and a TXT record `asuid.murmur` holding the app's verification ID:

   ```sh
   az containerapp show --name murmur-hub --resource-group "$RG" \
     --query properties.customDomainVerificationId --output tsv
   ```

2. If your domain has a CAA record, allow DigiCert, Azure's certificate authority for these certificates:
   `0 issue digicert.com`. Without it, issuing and renewing fail.

3. Add the domain and its certificate. The app must be running while the certificate is issued and renewed.

   ```sh
   az containerapp hostname add --hostname murmur.example.com --resource-group "$RG" --name murmur-hub
   az containerapp hostname bind --hostname murmur.example.com --resource-group "$RG" --name murmur-hub \
     --environment murmur-env --validation-method CNAME
   ```

4. Move the hub to it, and register the new redirect URL with Entra:

   ```sh
   az containerapp update --name murmur-hub --resource-group "$RG" \
     --set-env-vars PUBLIC_URL=https://murmur.example.com
   ```

## Secrets in Key Vault

Container Apps can read each secret from Key Vault through a managed identity
([Azure](https://learn.microsoft.com/en-us/azure/container-apps/manage-secrets)). A user-assigned identity works
from the first `az containerapp create`:

```sh
az identity create --name murmur-hub --resource-group "$RG"
ID="$(az identity show --name murmur-hub --resource-group "$RG" --query id --output tsv)"
PRINCIPAL="$(az identity show --name murmur-hub --resource-group "$RG" --query principalId --output tsv)"
az role assignment create --assignee "$PRINCIPAL" --role "Key Vault Secrets User" --scope <your key vault's ID>
```

Then, in step 4, add `--user-assigned "$ID"`, and give each secret as a reference to its Key Vault secret:

```
--secrets session-secret="keyvaultref:https://<vault>.vault.azure.net/secrets/murmur-session-secret,identityref:$ID" ...
```

A reference without a version follows the secret's latest version within 30 minutes, and restarts the app when it
changes.

## Upgrade

```sh
az containerapp update --name murmur-hub --resource-group "$RG" --image "ghcr.io/eledoneai/murmur-hub:$VERSION"
```

A new revision starts, takes the new connections, and the old one stops; its laptops reconnect. Laptops follow the
new version as your team's update policy says ([docs/self-hosting.md](../../docs/self-hosting.md#upgrading)).

## More than one replica

`BUS` already points at the database, so raise `--max-replicas` with `az containerapp update`. To carry events over
Redis instead, use Azure Managed Redis with the Enterprise or Non-clustered clustering policy, and
`BUS=rediss://:<access key>@<host>:10000` ([docs/managed-services.md](../../docs/managed-services.md#three-rules)).
Azure Cache for Redis is closed to new customers and retiring.

## App Service for Containers

App Service runs the same image on a Linux plan. Microsoft documents a request timeout of 240 seconds on Linux,
which it puts down to the Azure Load Balancer's default idle timeout of four minutes
([Azure](https://learn.microsoft.com/en-us/troubleshoot/azure/app-service/web-request-times-out-app-service)).
The 25-second pings keep laptops' connections from sitting idle; if one is still cut, the laptop reconnects on its
own. WebSockets are always on for Linux apps.

1. Create the network and the database as in step 2. The `apps` subnet is for Container Apps only; App Service needs
   a subnet of its own:

   ```sh
   az network vnet subnet create --resource-group "$RG" --vnet-name murmur-vnet --name web \
     --address-prefixes 10.42.2.0/27 --delegations Microsoft.Web/serverFarms
   ```

2. Create a Linux plan, Basic (B1, about $12 a month) or above: only paid plans keep the app always on and join a
   virtual network. Then create the app and join it to the `web` subnet:

   ```sh
   az appservice plan create --name murmur-plan --resource-group "$RG" --is-linux --sku B1
   az webapp create --name <app name> --resource-group "$RG" --plan murmur-plan \
     --container-image-name "ghcr.io/eledoneai/murmur-hub:$VERSION"
   az webapp vnet-integration add --name <app name> --resource-group "$RG" --vnet murmur-vnet --subnet web
   az webapp show --name <app name> --resource-group "$RG" --query defaultHostName --output tsv
   ```

   The app then reaches the database through the network, and finds its name through the private DNS zone linked to
   it ([Azure](https://learn.microsoft.com/en-us/azure/app-service/overview-vnet-integration)). The last command
   prints the app's address. `PUBLIC_URL` is `https://` followed by it, or your own domain once you have added one.

3. Turn on Always On, and set the settings. `WEBSITES_PORT=8080` tells App Service which port the hub listens on.

   ```sh
   az webapp config set --name <app name> --resource-group "$RG" --always-on true
   az webapp config appsettings set --name <app name> --resource-group "$RG" --settings \
     WEBSITES_PORT=8080 PUBLIC_URL=https://<app address> TRUST_PROXY=1 \
     STORE='postgres://...?sslmode=require' BUS='postgres://...?sslmode=require' \
     SESSION_SECRET="$(openssl rand -base64 32)" \
     OIDC_ISSUER='https://login.microsoftonline.com/<directory (tenant) ID>/v2.0' OIDC_CLIENT_ID='<client ID>' \
     OIDC_CLIENT_SECRET='<secret>' OIDC_NAME=Microsoft OIDC_TRUST_EMAIL=1 \
     ALLOWED_DOMAINS=example.com ADMINS=you@example.com
   ```

   App settings can also refer to Key Vault secrets, as `@Microsoft.KeyVault(SecretUri=...)`, once the app has a
   managed identity that may read them.

4. Leave `WEBSITES_ENABLE_APP_SERVICE_STORAGE` unset or `false`: the hub keeps nothing on disk here, and sqlite
   can't lock files on the `/home` share.

To upgrade, `az webapp config container set --name <app name> --resource-group "$RG" --container-image-name
"ghcr.io/eledoneai/murmur-hub:$VERSION"`. App Service starts the new container before it stops the old one, so keep
`BUS` set.
