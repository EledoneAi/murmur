# A virtual machine

The cheapest real setup is one small Linux VM running the [Docker Compose recipe](../compose): the hub keeps its
sqlite file on the VM's own disk, and Caddy gets the HTTPS certificate. This page lists what to set up at each
provider before you follow that recipe. To run the binary without Docker, follow [deploy/systemd](../systemd)
instead.

## What every VM needs

- A Linux VM, Ubuntu or Debian for example. The smallest size a provider offers is enough for a team.
- [Docker Engine with the Compose plugin](https://docs.docker.com/engine/install/).
- A public address that doesn't change, and a DNS name for the hub pointing at it.
- Ports 80 and 443 open to the internet, or at least to your team and to Let's Encrypt, and port 22 for you.

Then follow [One server](../compose#one-server) in the Compose recipe.

The VM's disk holds everything that matters, so back it up: the Compose recipe shows how to copy the sqlite file,
most providers also offer snapshots of the whole disk, and the `-litestream` image copies the file to object
storage as it changes ([deploy/litestream](../litestream)).

## Hetzner Cloud

- The smallest shared-vCPU server, CX23, costs about €5.49 a month
  ([Hetzner](https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/)).
- Pick an Ubuntu image and add your SSH key when you create it.
- Add a firewall that lets in ports 22, 80 and 443, and attach it to the server.
- Point an A record (and an AAAA record for IPv6) at the server's addresses. They stay the same as long as the
  server exists.

## Amazon Lightsail

- Use a Lightsail **instance**, not Lightsail containers: containers have no lasting storage
  ([AWS](https://docs.aws.amazon.com/lightsail/latest/userguide/amazon-lightsail-faq-containers.html)), and their
  support for long-lived WebSockets isn't documented. The smallest instances cost about $5–7 a month.
- Pick the **OS Only** Ubuntu blueprint.
- On the instance's **Networking** tab, make sure the IPv4 firewall allows HTTP (80) and HTTPS (443), and attach a
  static IP: without one, the address changes when the instance stops and starts.

## DigitalOcean Droplets

- A Basic Droplet with Ubuntu, and your SSH key.
- Add a Cloud Firewall that lets in ports 22, 80 and 443, and apply it to the Droplet.
- A Droplet keeps its address while it exists; a reserved IP lets you move the address to another Droplet later.
- Not App Platform: it has no volumes, so the sqlite file would vanish at each deploy
  ([DigitalOcean](https://docs.digitalocean.com/products/app-platform/details/limits/)).

## Amazon EC2 and Azure virtual machines

The same steps: an Ubuntu VM, a security group (EC2) or network security group (Azure) that lets in ports 22, 80
and 443, and a fixed public address (an Elastic IP on EC2, a static public IP on Azure). Keep the sqlite file on the
VM's own disk, not on EFS or Azure Files.
