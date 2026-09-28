# Architecture

The plugin sits between the Docker Engine and the DHCP server you already
run. The diagram names the parts and what passes between them.

```mermaid
flowchart TB
  E["Docker Engine (libnetwork)<br/>network driver calls: CreateNetwork, CreateEndpoint, Join, Leave, DeleteEndpoint<br/>IPAM calls when the network also names the plugin as its IPAM driver: RequestPool, RequestAddress"]
  subgraph plugin["net-dhcp plugin, privileged, host network namespace"]
    H["pkg/plugin<br/>Docker handlers, lease records, counters and health; puts the leased address on the container's link"]
    C["pkg/dhcp<br/>protocol parameters; enters the container's network namespace and opens the client's link"]
    L["dhcp-golib, the project's own library<br/>DHCPv4, DHCPv6 and router advertisement client, one per endpoint and address family: renew, rebind, NAK, expiry"]
    S[("/var/lib/net-dhcp<br/>per-network options, lease-records.jsonl")]
    TS[("tombstones.json, same directory<br/>written at DeleteEndpoint, read at the next CreateEndpoint, kept 60 s: MAC, hostname, last IPv4 and IPv6 address")]
  end
  subgraph links["Links on the Docker host"]
    direction LR
    B["bridge mode<br/>an existing bridge, or one the plugin makes from a spare NIC, plus a veth pair per container"]
    M["macvlan / ipvlan mode<br/>a child link on a parent NIC, optionally on a VLAN of it"]
  end
  subgraph lan["Your LAN"]
    D["The DHCP server you already run<br/>router, Kea, ISC dhcpd, dnsmasq"]
    T["Its lease table, reservations and DNS see the container as one more host"]
  end
  E -- "plugin socket" --> H
  H --> C --> L
  H <--> S
  H <--> TS
  L -- "client socket in the container's network namespace" --> B
  L --> M
  B <-- "Discover, Offer, Request, ACK" --> D
  M <--> D
  D --> T
  style L fill:#0f6e63,stroke:#0f6e63,color:#ffffff
```

Docker calls the plugin over its socket; `pkg/plugin` answers Docker and
keeps the records and tombstones, `pkg/dhcp` enters the container's
network namespace, and the library speaks DHCP over the container's own
link to the server on your LAN. The DHCP client is
[claymore666/dhcp-golib](https://github.com/claymore666/dhcp-golib), this
project's own library, pinned in `go.mod`.

It is a privileged plugin: it runs with host networking, the Docker socket
and `CAP_NET_ADMIN`. The full list is under [Requirements](index.md#requirements),
and what each item is for is in [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md#scope--what-this-plugin-is).
The call sequence behind the arrows is in [How it works](internals.md).
