# How it works

Fundamentally, `net-dhcp` uses the same mechanism as Docker's built-in
`bridge` driver to wire networking to containers: a bridge on the host
acts as a switch, and `veth` pairs connect each container's network
namespace to it. Two things differ:

- **Existing bridge, not a managed one.** Where Docker creates and
  manages its own bridges (and routes/filters traffic), `net-dhcp` uses
  an existing bridge on the host, bridged onto the desired local
  network. (In macvlan/ipvlan mode the parent is a host NIC instead —
  see [parent-attached modes](parent-attached-modes.md).)
- **External addressing.** Instead of allocating addresses from a static
  pool on the Docker host, `net-dhcp` relies on an external DHCP server
  to provide them.

## Flow (bridge mode)

1. A container-creation request is made.
2. A `veth` pair is created and the host end is connected to the bridge
   (both interfaces are still in the host namespace at this point).
3. A DHCP client (`dhcpcd`) is started on the container end (still in
   the host namespace) — the initial IP address is provided to Docker by
   the plugin.
4. Docker moves the container end of the `veth` pair into the
   container's network namespace and sets the IP address — at this point
   that first client is stopped.
5. `net-dhcp` starts a persistent `dhcpcd` on the container end of the
   `veth` pair in the container's **network namespace** (but still in the
   plugin's **PID namespace**, so the container can't see the DHCP
   client). It runs observe-only (`--noconfigure`): the plugin applies
   the lease to the link via netlink rather than letting the client
   reconfigure the interface.
6. `dhcpcd` keeps running, renewing the lease when required, until the
   container shuts down.

## See also

- [Driver reference](reference.md) — options, `/Plugin.Health`, troubleshooting
- [Bridge mode](bridge-mode.md) and [macvlan / ipvlan](parent-attached-modes.md) setup
