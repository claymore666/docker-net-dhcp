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

You need a pre-configured bridge interface on the host. Enslaving the
NIC to a bridge changes how the **host itself** is addressed, so read
the warning below before running anything on a machine you cannot walk
up to.

!!! danger "The host's own address moves to the bridge"
    Once `eth0` is enslaved it must be left **address-less**, and DHCP
    has to run on the bridge instead. Applying this over SSH with the
    NIC still configured — or with a typo in the bridge stanza — drops
    the connection and does not give it back.

    Have console or out-of-band access (IPMI, iDRAC, the hypervisor
    console) before you start. On Ubuntu, `sudo netplan try` gives you
    an automatic revert if you lose the session; the other stacks have
    no equivalent safety net.

    The bridge normally inherits the NIC's MAC, but not on every
    driver. If your DHCP server pins the host's address by MAC
    reservation, the host may come back on a **different** address —
    set the bridge MAC explicitly if that matters.

### Try it now (does not survive a reboot)

These manual steps work on most Linux systems and are the fastest way
to confirm the plugin does what you want. They are lost on the next
reboot — make them permanent with one of the stanzas below.

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

### Make the bridge persistent

Pick the one stanza that matches whatever manages networking on the
host. In every case the pattern is identical: the NIC carries **no**
address and is listed as a bridge port, and the bridge is the thing
that runs DHCP.

Each recipe disables STP explicitly. That is not cosmetic here — see
[Leave STP off](#leave-stp-off-unless-you-need-it) below.

#### Debian / ifupdown

Needs `bridge-utils` (`sudo apt install bridge-utils`).

Note that `networking.service` runs `ifup -a` **synchronously**, so the
boot blocks until the bridge's DHCP request completes or `dhclient`
gives up — around a minute if the DHCP server is slow or absent.
Measured at ~2 minutes to reach a settled state on a bridge whose
lease was delayed. That is normal for this stack, not a fault; if a
fast boot matters more than having the address ready at boot, the
networkd or netplan recipes do not have this property.

```ini
# /etc/network/interfaces
auto lo
iface lo inet loopback

# Enslaved: no address of its own.
iface eth0 inet manual

auto my-bridge
iface my-bridge inet dhcp
    bridge_ports eth0
    bridge_stp off
    bridge_fd 0
```

#### Ubuntu / netplan

```yaml
# /etc/netplan/60-dhcp-bridge.yaml   (chmod 600)
network:
  version: 2
  renderer: networkd
  ethernets:
    eth0:
      dhcp4: false
      dhcp6: false
  bridges:
    my-bridge:
      interfaces: [eth0]
      dhcp4: true
      parameters:
        stp: false
        forward-delay: 0
```

```bash
sudo chmod 600 /etc/netplan/60-dhcp-bridge.yaml
sudo netplan try      # auto-reverts in 120s if you lose the session
sudo netplan apply
```

#### systemd-networkd

Three files — the bridge device, the port, and the bridge's own
addressing:

```ini
# /etc/systemd/network/10-my-bridge.netdev
[NetDev]
Name=my-bridge
Kind=bridge

[Bridge]
STP=false
ForwardDelaySec=0
```

```ini
# /etc/systemd/network/20-eth0.network
[Match]
Name=eth0

[Network]
Bridge=my-bridge
```

```ini
# /etc/systemd/network/30-my-bridge.network
[Match]
Name=my-bridge

[Network]
DHCP=ipv4
ConfigureWithoutCarrier=yes
```

`ConfigureWithoutCarrier=yes` matters at boot: a bridge with no port up
yet has no carrier, and without it `networkd` can decline to start DHCP
on the bridge at all. This is what `netplan` emits for the equivalent
bridge, so it is the reference implementation's own answer rather than
a workaround.

```bash
sudo systemctl enable --now systemd-networkd
```

#### NetworkManager (nmcli)

!!! warning "Not on Ubuntu — netplan owns ethernet there"
    Ubuntu ships NetworkManager restricted to wireless devices:

    ```ini
    # /usr/lib/NetworkManager/conf.d/10-globally-managed-devices.conf
    [keyfile]
    unmanaged-devices=*,except:type:wifi,except:type:gsm,except:type:cdma
    ```

    Every ethernet device is therefore **unmanaged**, and `nmcli con up`
    fails with *"No suitable device found for this connection"* no
    matter how correct the profile is. Use the netplan recipe above on
    Ubuntu. This recipe is for distributions where NetworkManager
    manages ethernet — Fedora, RHEL and derivatives, and Debian
    installs that chose it.

The existing standalone profile for the NIC has to stop autoconnecting,
or it will race the bridge for `eth0`. Find its name with
`nmcli con show`.

```bash
sudo nmcli con add type bridge ifname my-bridge con-name my-bridge \
    ipv4.method auto bridge.stp no bridge.forward-delay 0
sudo nmcli con add type ethernet ifname eth0 con-name my-bridge-port \
    master my-bridge slave-type bridge

# Stop the old profile from grabbing the NIC on boot
sudo nmcli con mod "Wired connection 1" connection.autoconnect no
sudo nmcli con down "Wired connection 1"

sudo nmcli con up my-bridge
```

### Leave STP off unless you need it

With STP **enabled**, a bridge puts every newly added port through
listening and learning states before it forwards — two forwarding
delays, 30 seconds at the stock 15s setting. Container `veth`s are
added at attach time, so a bridge with STP on **breaks DHCP for every
container**: the client broadcasts into a port that is not forwarding
yet, gets no answer, and the attach fails or falls back.

A bridge created with plain `ip link add ... type bridge` has STP off
already, which is why the imperative recipe above works as written.
Managed stacks are the risk — they apply their own default, and it is
not always off. That is why every stanza above sets it explicitly
rather than relying on one.

If containers only get addresses when you attach them slowly, or
`leases_obtained` sits flat while `dhcp_timeouts` climbs, check STP
first:

```console
$ ip -d link show my-bridge | grep -o 'stp_state [0-9]*'
stp_state 0
```

`stp_state 0` is what you want. Note that a non-zero `forward_delay` in
the same output is harmless while STP is off — and that it is reported
in **centiseconds**, so the stock 15 seconds reads as `1500`.

### Persist the firewall rule too

The `FORWARD -i my-bridge -j ACCEPT` rule from the imperative recipe is
as temporary as the bridge was. Whichever stanza you used above, the
rule needs the distro's own persistence mechanism:

| Stack | How |
|---|---|
| iptables | `sudo apt install iptables-persistent`, then `sudo netfilter-persistent save` |
| nftables | add the rule to `/etc/nftables.conf`, `sudo systemctl enable nftables` |
| firewalld | `sudo firewall-cmd --permanent --direct --add-rule ipv4 filter FORWARD 0 -i my-bridge -j ACCEPT && sudo firewall-cmd --reload` |
| ufw | set `DEFAULT_FORWARD_POLICY="ACCEPT"` in `/etc/default/ufw`, then `sudo ufw reload` |

You only need this if the forwarding policy is `DROP` — check with
`sudo iptables -S FORWARD | head -1`.

## 2. Create the network

```bash
# On arm64 use the -arm64 tag — a network stores this exact reference
# as its driver, so it must name the plugin you installed.
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.8.0 \
  --ipam-driver null -o bridge=my-bridge my-dhcp-net
```

With IPv6 as well (the `docker network create --ipv6` flag does **not**
work with the null IPAM driver; use the `ipv6` driver option instead):

```bash
# arm64: the -arm64 tag here too.
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.8.0 \
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
    external: true
    name: my-dhcp-net
```

(The older `external:` block with a nested `name:` is deprecated and
warns on current Compose — the two-key form above is the supported one.)

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
    # arm64: the -arm64 tag, matching the plugin you installed.
    driver: ghcr.io/claymore666/docker-net-dhcp:v1.8.0
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
