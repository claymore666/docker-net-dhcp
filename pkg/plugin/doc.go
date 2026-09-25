// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Package plugin is the Docker side of docker-net-dhcp: it serves the
// two libnetwork contracts that make this a network plugin,
// /NetworkDriver.* and /IpamDriver.*, on one Unix socket in one
// process, beside two paths that belong to neither contract and are
// served on the same socket anyway: /Plugin.Health and /metrics.
//
// Every container gets its address from the DHCP server the LAN already
// runs (a router, a Fritz!Box, dnsmasq), over `bridge`, `macvlan` or
// `ipvlan`, for IPv4 and IPv6. The protocol exchange is not here: it
// runs in-process on the dhcp-golib engine behind the pkg/dhcp chassis,
// so there is no external DHCP client to install and no client process
// per container. What is here is everything Docker-shaped around it:
// the network and endpoint lifecycle, the link and namespace work each
// mode needs, the renewal manager per endpoint, the state directory
// that lets an enabled plugin be restarted without losing what it was
// serving, and the counters and health document an operator reads back.
//
// # The two IPAM shapes
//
// Docker's built-in IPAM is never the allocator: it would allocate from
// a subnet of its own choosing and collide with the real LAN. There are
// two supported ways to say so, and which one a network uses is fixed
// at `docker network create`:
//
//   - `--ipam-driver null`, what 1.x and 2.0 shipped, where Docker
//     allocates nothing and the /NetworkDriver.CreateEndpoint handler
//     answers libnetwork with the leased address. All three modes,
//     IPv4 and IPv6.
//   - `--ipam-driver <this plugin>` (v2.1.0+, #110), where the
//     /IpamDriver.* handlers put the lease into Docker's own address
//     management, which is what makes `docker run --ip` and Compose
//     `ipv4_address` work. `bridge` and `macvlan`: ipvlan (#949) is
//     refused at network creation, because the alternative is an
//     endpoint that fails at container start with an error naming
//     neither. IPv6 is switched on with `-o ipv6=true` or
//     `-o ipv6_mode=`, and CreateEndpoint reports the address (#960).
//
// # The exported surface is a wire contract, not a library API
//
// Most of the types below are the JSON payloads libnetwork sends and
// expects, from moby's libnetwork/drivers/remote/api and
// libnetwork/ipams/remote/api. They are declared here because this
// plugin talks to the daemon over HTTP and JSON: the wire shape is the
// contract, and importing moby's structs would hide a rename behind a
// compile that still succeeded.
//
// The one entry point meant for callers is NewPlugin, which cmd/net-dhcp
// calls with Options built from the plugin's environment;
// NewPlugin(Options{}) is a valid production configuration. This
// package is not built to be embedded in another program, and nothing
// outside this module is expected to import it: the supported interface
// to a running plugin is the socket.
//
// Operator documentation lives at
// https://claymore666.github.io/docker-net-dhcp/.
package plugin
