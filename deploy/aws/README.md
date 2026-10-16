# AWS

Two ways to run the hub on Amazon ECS with Fargate, both with RDS for PostgreSQL as the store:

- **ECS Express Mode**, the simple one. One command creates the service, a load balancer with a certificate, and
  an address. It is AWS's replacement for App Runner, which is closed to new customers.
- **An ECS service behind your own Application Load Balancer**, for full control over the load balancer and
  deployments, and for sqlite copied to S3 instead of a database.

Replace `REGION` and the example values with yours. [docs/self-hosting.md](../../docs/self-hosting.md) shows how to
set up your identity provider.

## Things to know first

- **No lasting disk.** A Fargate task's storage goes away with the task, and EFS is a network file system, where
  sqlite isn't safe. Use RDS for PostgreSQL as the store, or, on the manual path only, sqlite copied to S3 by the
  Litestream image ([below](#sqlite-and-s3-instead-of-rds)).
- **Give the hub a bus too.** Express Mode deploys with canaries: for a few minutes, old and new tasks run side by
  side. With `BUS` set to the same Postgres URL as the store, both see the whole team.
- **Connections.** The load balancer closes a connection when no data has been sent or received for its idle
  timeout, 60 seconds by default, and AWS advises sending at least a byte before each timeout ends
  ([AWS](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/edit-load-balancer-attributes.html#connection-idle-timeout)).
  Laptops and the hub ping each other every 25 seconds.
- **No platform sign-in.** Don't add the load balancer's `authenticate-oidc` action in front of the hub: laptops
  can't get through it. The hub signs people in itself
  ([docs/managed-services.md](../../docs/managed-services.md#three-rules)).
- **Cost**, approximately: Fargate with 0.25 vCPU and 0.5 GB about $7–9 a month, the load balancer about $16 plus
  usage (Express Mode shares one among up to 25 services), RDS on `db.t4g.micro` about $12, plus public IPv4
  addresses: about $45–55 a month in all ([Fargate](https://aws.amazon.com/fargate/pricing/),
  [ELB](https://aws.amazon.com/elasticloadbalancing/pricing/), [VPC](https://aws.amazon.com/vpc/pricing/)).

## 1. Set variables

```sh
REGION=eu-west-1
export AWS_DEFAULT_REGION="$REGION"
ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
VERSION=1.0.0           # a release from https://github.com/EledoneAi/murmur/releases
```

## 2. Create the database

First, two security groups in your default VPC: `murmur-hub` for the hub's tasks, and `murmur-db` for the
database, which lets PostgreSQL (port 5432) in from `murmur-hub` only.

```sh
VPC="$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)"
HUB_SG="$(aws ec2 create-security-group --group-name murmur-hub --description 'murmur hub tasks' \
  --vpc-id "$VPC" --query GroupId --output text)"
DB_SG="$(aws ec2 create-security-group --group-name murmur-db --description 'murmur database' \
  --vpc-id "$VPC" --query GroupId --output text)"
aws ec2 authorize-security-group-ingress --group-id "$DB_SG" --protocol tcp --port 5432 --source-group "$HUB_SG"
```

Then, in the RDS console, create a PostgreSQL database:

- the smallest instance class is enough for a team, for example `db.t4g.micro`;
- in your default VPC, **not** publicly accessible;
- an initial database named `murmur`, and a master user and password you choose;
- the `murmur-db` security group, in place of the VPC's default one.

Note the database's endpoint. If you use Aurora PostgreSQL instead, use the cluster's writer endpoint.
[docs/managed-services.md](../../docs/managed-services.md#postgres) says what to avoid, such as RDS Proxy for the
bus.

## 3. Store the secrets

The database URL holds the password, so it goes in Secrets Manager with the others. URL-encode characters such as
`@`, `:` or `/` in the password.

```sh
SESSION_ARN="$(aws secretsmanager create-secret --name murmur/session-secret \
  --secret-string "$(openssl rand -base64 32)" --query ARN --output text)"
OIDC_ARN="$(aws secretsmanager create-secret --name murmur/oidc-client-secret \
  --secret-string '<client secret from your identity provider>' --query ARN --output text)"
DB_ARN="$(aws secretsmanager create-secret --name murmur/database-url \
  --secret-string 'postgres://<user>:<password>@<endpoint>:5432/murmur?sslmode=require' --query ARN --output text)"
```

## 4. Create the roles

Express Mode needs a task execution role, which pulls the image, writes logs and reads the secrets, and an
infrastructure role, which manages the load balancer. These are the commands from
[AWS's guide](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-getting-started.html);
skip the ones for roles you already have.

```sh
aws iam create-role --role-name ecsTaskExecutionRole --assume-role-policy-document '{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Principal": {"Service": "ecs-tasks.amazonaws.com"}, "Action": "sts:AssumeRole"}]
}'
aws iam attach-role-policy --role-name ecsTaskExecutionRole \
  --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy

aws iam create-role --role-name ecsInfrastructureRoleForExpressServices --assume-role-policy-document '{
  "Version": "2012-10-17",
  "Statement": [{"Effect": "Allow", "Principal": {"Service": "ecs.amazonaws.com"}, "Action": "sts:AssumeRole"}]
}'
aws iam attach-role-policy --role-name ecsInfrastructureRoleForExpressServices \
  --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRoleforExpressGatewayServices
```

The execution role's managed policy doesn't cover Secrets Manager. Let it read the hub's secrets:

```sh
aws iam put-role-policy --role-name ecsTaskExecutionRole --policy-name murmur-secrets --policy-document "{
  \"Version\": \"2012-10-17\",
  \"Statement\": [{\"Effect\": \"Allow\", \"Action\": \"secretsmanager:GetSecretValue\",
    \"Resource\": \"arn:aws:secretsmanager:$REGION:$ACCOUNT:secret:murmur/*\"}]
}"
```

## 5. Deploy with Express Mode

An Express Mode service's address is `https://<service name>.ecs.<region>.on.aws`. Register `PUBLIC_URL` followed
by `/auth/callback` as the redirect URL with your identity provider.

```sh
PUBLIC_URL="https://murmur-hub.ecs.$REGION.on.aws"
```

Write the container's settings to `container.json`. Fargate pulls the public image from ghcr.io; to keep a copy in
your account, copy it into ECR with `docker buildx imagetools create` and use that address instead.

```sh
cat > container.json <<EOF
{
  "image": "ghcr.io/eledoneai/murmur-hub:$VERSION",
  "containerPort": 8080,
  "environment": [
    {"name": "PUBLIC_URL", "value": "$PUBLIC_URL"},
    {"name": "TRUST_PROXY", "value": "1"},
    {"name": "OIDC_ISSUER", "value": "https://login.example.com"},
    {"name": "OIDC_CLIENT_ID", "value": "murmur"},
    {"name": "OIDC_NAME", "value": "Example SSO"},
    {"name": "ALLOWED_DOMAINS", "value": "example.com"},
    {"name": "ADMINS", "value": "you@example.com"}
  ],
  "secrets": [
    {"name": "SESSION_SECRET", "valueFrom": "$SESSION_ARN"},
    {"name": "OIDC_CLIENT_SECRET", "valueFrom": "$OIDC_ARN"},
    {"name": "STORE", "valueFrom": "$DB_ARN"},
    {"name": "BUS", "valueFrom": "$DB_ARN"}
  ]
}
EOF
```

Express Mode's defaults suit a web app, not the hub: it checks `/ping` on port 80. Set the health check path and
the port, and keep at least one task. `securityGroups` adds `murmur-hub` to the tasks, next to the group Express Mode
creates for them, so the database lets them in:

```sh
aws ecs create-express-gateway-service \
  --service-name murmur-hub \
  --execution-role-arn "arn:aws:iam::$ACCOUNT:role/ecsTaskExecutionRole" \
  --infrastructure-role-arn "arn:aws:iam::$ACCOUNT:role/ecsInfrastructureRoleForExpressServices" \
  --primary-container file://container.json \
  --network-configuration "securityGroups=$HUB_SG" \
  --health-check-path /healthz \
  --cpu 256 --memory 512 \
  --scaling-target minTaskCount=1,maxTaskCount=2 \
  --monitor-resources
```

When the service is `ACTIVE`, check its address:

```sh
SERVICE_ARN="arn:aws:ecs:$REGION:$ACCOUNT:service/default/murmur-hub"
aws ecs describe-express-gateway-service --service-arn "$SERVICE_ARN" \
  --query 'service.activeConfigurations[0].ingressPaths'
```

If it differs from `PUBLIC_URL`, put the right one in `container.json` and in your identity provider, and run the
update below. Then open `PUBLIC_URL`, sign in, and share the install line it shows.

### Your own domain

Express Mode has no setting for a domain of your own, but the load balancer it creates takes one
([AWS](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-advanced-customization.html#express-service-add-custom-domain)).
With a certificate for the domain in ACM, in the same region:

1. In the ECS console, open the service, then the listener rule on its **Resources** tab. Edit the rule's **Host
   header** condition: keep the service's address, and add your domain as a second value.
2. On the load balancer's HTTPS listener, under **Certificates**, add the domain's certificate.
3. Point the domain at the load balancer: an alias record in Route 53, or a CNAME to the load balancer's DNS name
   elsewhere.
4. Set `PUBLIC_URL` to the new address in `container.json` and in your identity provider, and run the update below.

Express Mode updates keep these changes, unless an update sets the same thing again.

### Upgrade

Change the image tag in `container.json`, then:

```sh
aws ecs update-express-gateway-service --service-arn "$SERVICE_ARN" \
  --primary-container file://container.json --monitor-resources
```

The new task takes connections next to the old one, then the old one stops, and its laptops reconnect. Laptops
follow the new version as your team's update policy says
([docs/self-hosting.md](../../docs/self-hosting.md#upgrading)).

## The manual path: your own load balancer

Steps 1 to 4 are the same, without the infrastructure role.

1. Register a task definition. Write `task.json` with the same settings as `container.json` above:

   ```sh
   aws logs create-log-group --log-group-name /ecs/murmur-hub
   cat > task.json <<EOF
   {
     "family": "murmur-hub",
     "requiresCompatibilities": ["FARGATE"],
     "networkMode": "awsvpc",
     "cpu": "256",
     "memory": "512",
     "executionRoleArn": "arn:aws:iam::$ACCOUNT:role/ecsTaskExecutionRole",
     "containerDefinitions": [{
       "name": "hub",
       "image": "ghcr.io/eledoneai/murmur-hub:$VERSION",
       "essential": true,
       "portMappings": [{"containerPort": 8080}],
       "environment": [
         {"name": "PUBLIC_URL", "value": "https://murmur.example.com"},
         {"name": "TRUST_PROXY", "value": "1"},
         {"name": "OIDC_ISSUER", "value": "https://login.example.com"},
         {"name": "OIDC_CLIENT_ID", "value": "murmur"},
         {"name": "OIDC_NAME", "value": "Example SSO"},
         {"name": "ALLOWED_DOMAINS", "value": "example.com"},
         {"name": "ADMINS", "value": "you@example.com"}
       ],
       "secrets": [
         {"name": "SESSION_SECRET", "valueFrom": "$SESSION_ARN"},
         {"name": "OIDC_CLIENT_SECRET", "valueFrom": "$OIDC_ARN"},
         {"name": "STORE", "valueFrom": "$DB_ARN"},
         {"name": "BUS", "valueFrom": "$DB_ARN"}
       ],
       "logConfiguration": {"logDriver": "awslogs", "options": {
         "awslogs-group": "/ecs/murmur-hub", "awslogs-region": "$REGION", "awslogs-stream-prefix": "hub"}}
     }]
   }
   EOF
   aws ecs register-task-definition --cli-input-json file://task.json
   ```

2. In the EC2 console, create the load balancer:
   - a target group of type **IP addresses**, protocol HTTP, port 8080, health check path `/healthz`;
   - an internet-facing Application Load Balancer with an HTTPS listener on 443, a certificate for your domain
     from ACM, and the target group as its default action. Keep its idle timeout at 60 seconds or more
     ([Things to know first](#things-to-know-first)).

   Then let the load balancer reach the tasks on port 8080:

   ```sh
   aws ec2 authorize-security-group-ingress --group-id "$HUB_SG" --protocol tcp --port 8080 \
     --source-group <the load balancer's security group>
   ```

3. Create the `default` cluster, unless the account already has one in this region (Express Mode creates it), and
   the service, in the same subnets as the load balancer:

   ```sh
   aws ecs create-cluster --cluster-name default
   aws ecs create-service --cluster default --service-name murmur-hub \
     --task-definition murmur-hub --desired-count 1 --launch-type FARGATE \
     --network-configuration "awsvpcConfiguration={subnets=[<subnet>,<subnet>],securityGroups=[$HUB_SG],assignPublicIp=ENABLED}" \
     --load-balancers "targetGroupArn=<target group ARN>,containerName=hub,containerPort=8080"
   ```

   `assignPublicIp=ENABLED` lets tasks in public subnets pull the image; in private subnets, use a NAT gateway and
   `DISABLED`.

4. Point your domain at the load balancer (an alias record in Route 53, or a CNAME elsewhere). Open `PUBLIC_URL`,
   sign in, and share the install line it shows.

To upgrade, register the task definition again with the new image and run
`aws ecs update-service --cluster default --service murmur-hub --task-definition murmur-hub`.

## sqlite and S3 instead of RDS

One task can keep sqlite with the `-litestream` image, which copies the file to S3 as it changes and restores it
when a new task starts. There is then no database to pay for. It allows one writer only, so never use it with
Express Mode, whose canary deployments run old and new tasks side by side. On the manual path, deploy one task with
`minimumHealthyPercent=0` and `maximumPercent=100`: [deploy/litestream](../litestream#ecs-on-fargate) gives the
details.

## More than one task

Both paths above already set `BUS` to the database, so more tasks see the whole team: raise `maxTaskCount` on
Express Mode, or `--desired-count` on the manual path. To carry events over Redis instead, use ElastiCache with
`BUS=rediss://...`; [docs/managed-services.md](../../docs/managed-services.md#redis) covers its flavours.

## Back up

Turn on RDS automated backups (the default for new databases) and set how many days they keep. Without the store,
everyone signs in again and laptops are connected again.
