# Bridge mode

Bridge mode is `docker-net-dhcp`'s default. Unlike the parent-attached
modes ([macvlan / ipvlan](parent-attached-modes.md)), bridge mode plugs
container `veth`s into **a Linux bridge you maintain** — so it needs a
small amount of one-time host setup, but works anywhere a bridge can be
bridged onto the LAN where the DHCP server lives.

For the full option/observability/troubleshooting matrix see the
[driver reference](reference.md). This page is the end-to-end
walkthrough.

## 1. Prepare a host bridge

You need a pre-configured bridge interface on the host. How you set this
up depends on your distro; the following manual steps work on most Linux
systems:

```bash
# Create the bridge
sudo ip link add my-bridge type bridge
sudo ip link set my-bridge up

# Assuming 'eth0' is connected to your LAN (where the DHCP server is)
sudo ip link set eth0 up
# Attach your network card to the bridge
sudo ip link set eth0 master my-bridge

# If your firewall's forwarding policy is DROP, add an ACCEPT rule
sudo iptables -A FORWARD -i my-bridge -j ACCEPT

# Get an IP for the host (goes out to the DHCP server, since eth0 is
# attached to the bridge). Replace with whatever config you used for eth0.
sudo dhcpcd my-bridge
```

## 2. Create the network

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.3.0 \
  --ipam-driver null -o bridge=my-bridge my-dhcp-net
```

With IPv6 as well (the `docker network create --ipv6` flag does **not**
work with the null IPAM driver; use the `ipv6` driver option instead):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.3.0 \
  --ipam-driver null -o bridge=my-bridge -o ipv6=true my-dhcp-net
```

> **The `null` IPAM driver is mandatory.** Without it Docker allocates
> addresses from its own pool, which collides with the real LAN the
> bridge is attached to.

See the [driver reference](reference.md#driver-options-network-level)
for every network-level option (`lease_timeout`, `ignore_conflicts`,
`skip_routes`, `gateway`, `propagate_dns`, …).

## 3. Run containers

```console
$ docker run --rm -ti --network my-dhcp-net alpine
/ # ip address show
159: my-bridge0@if160: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1500 ...
    link/ether 86:41:68:f8:85:b9 brd ff:ff:ff:ff:ff:ff
    inet 10.255.0.246/24 brd 10.255.0.255 scope global my-bridge0
/ # ip route show
default via 10.255.0.123 dev my-bridge0
10.255.0.0/24 dev my-bridge0 scope link src 10.255.0.246
```

Or in Docker Compose, against a network created out-of-band (the
**recommended** approach — the network is shared across compose projects
and survives `compose down`):

```yaml
services:
  app:
    hostname: my-http
    image: nginx
    mac_address: 86:41:68:f8:85:b9
    networks:
      - dhcp
networks:
  dhcp:
    external:
      name: my-dhcp-net
```

You can also have Compose manage the network itself (it is then deleted
on `compose down`):

```yaml
services:
  app:
    image: nginx
    hostname: my-server
    networks:
      - dhcp
networks:
  dhcp:
    driver: ghcr.io/claymore666/docker-net-dhcp:v1.3.0
    driver_opts:
      bridge: my-bridge
      ipv6: 'true'
    ipam:
      driver: 'null'
```

Notes:

- The container takes a little longer than usual to start — a DHCP lease
  is obtained before it is created.
- A persistent DHCP client renews the lease (and updates the container's
  default gateway) for the life of the endpoint. **It runs separately
  from the container.**
- Use `--mac-address` / `mac_address` for MAC-keyed reservations or to
  reuse an old lease; `--hostname` / `hostname` is sent as DHCP option 12
  for DHCP-DNS integration. Per-endpoint and per-container knobs are
  documented in the [driver reference](reference.md#driver-options-per-endpoint).

## See also

- [Driver reference](reference.md) — all options, observability, troubleshooting
- [macvlan / ipvlan modes](parent-attached-modes.md) — attach to a host NIC, no bridge
- [How it works](internals.md) — the veth + DHCP-client mechanism
