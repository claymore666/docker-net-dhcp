// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/proto"
)

// VendorID is DHCPv4 option 60 when the network sets no vendor_class.
//
// The default is supplied HERE and not left to the library, and the two
// are not the same thing: an empty proto.Params.VendorClass sends no
// option 60 at all, while an empty vendor_class has always meant this
// string on the wire. A server keyed on it — the tagged-gateway shape
// the vendor-class integration tests build — would silently fall
// through to the untagged pool.
const VendorID = "docker-net-dhcp"

// clientIDTypeOpaque is RFC 2132 section 9.14's type byte for a
// client-identifier that is not a DUID.
//
// The chassis owns the WHOLE option-61 value including this byte
// (D10). The library sends Params.ClientID verbatim, so a caller that
// forgets the prefix changes the identity without changing anything
// visible from the container: the server files the lease under a
// different key and hands out a second address from the pool.
const clientIDTypeOpaque = 0x00

// ClientIdentity is the option-61 value AS SENT: the chassis's type
// byte followed by the caller's payload.
//
// Exported because the durable record stores the identity rather than
// re-deriving it (D10), and it must store what went on the wire. A
// record that kept the payload alone would be a record of a different
// client than the one the server filed the lease under, and the two
// would only be seen to differ when a restart got a second address.
func ClientIdentity(clientID []byte) []byte {
	if len(clientID) == 0 {
		return nil
	}
	return append([]byte{clientIDTypeOpaque}, clientID...)
}

// buildParams turns one endpoint's options into the protocol parameter
// set for one manager instance.
//
// once distinguishes the CreateEndpoint acquisition manager from the
// persistent Join manager. Since the desync fix below it selects
// NOTHING: both managers get the same parameters. It stays in the
// signature because it is how the seam names which manager a call site
// is building, and because the equality is the rule — a future arm here
// has to be argued at the assignment it would sit beside, not slipped
// in. TestBuildParams_NeitherManagerDesyncs asserts the equality.
func buildParams(opts *DHCPClientOptions, once bool) (proto.Params, error) {
	if opts.V6 {
		return proto.Params{}, ErrIPv6Unsupported
	}
	if len(opts.MAC) == 0 {
		return proto.Params{}, fmt.Errorf("dhcp: no MAC address for the endpoint")
	}

	p := proto.DefaultParams(opts.MAC)

	p.Hostname = opts.Hostname
	p.VendorClass = opts.VendorClass
	if p.VendorClass == "" {
		p.VendorClass = VendorID
	}
	p.ClientID = ClientIdentity(opts.ClientID)

	// RFC 5227 conflict detection, per network (D23). The zero value is
	// proto.ConflictWait and DefaultConflictCheck is that value's own
	// name, so an endpoint whose network predates the option gets the
	// mode the option's default names — one fact, read from the
	// library, never spelled here.
	//
	// BOTH MANAGERS GET THE SAME MODE, which is not a detail. The
	// one-shot wins the address and the Join manager holds it; a mode
	// that applied to one of them would probe the address before use
	// and then stop listening for section 2.4's conflicts for the whole
	// of the container's life, or the reverse. `once` selects nothing
	// here for the same reason it selects nothing below.
	p.Conflict = opts.ConflictMode

	// Params.CHAddr is left to runtime.NewClient, which fills it from
	// the link, and that is load-bearing rather than lazy (M6 review r2,
	// finding 1). The library's own-traffic exemption in the probe
	// window is keyed on CHAddr: a client whose CHAddr is a stable
	// identity rather than the sending interface's hardware address
	// reads its own kernel's ARP replies as conflicts and DECLINEs its
	// own address on every acquisition. proto.DefaultParams(opts.MAC)
	// above sets it to the endpoint's pinned MAC, which IS the link's
	// address -- the same MAC Docker put on the interface -- so the two
	// agree; TestBuildParams_TheCHAddrIsTheLinkSHardwareAddress is the
	// assertion that they still do.

	// register_dns arrives as a mode string because dhcpcd spelled it
	// that way ("both"); what it means is "ask the server to register
	// the name in DNS", which RFC 4702 encodes as option 81. Flags are
	// left zero, which the library resolves to S|E — ask for the A RR
	// as well as the PTR RR, in canonical wire format.
	if opts.FQDN != "" && opts.Hostname != "" {
		p.FQDN = proto.FQDN{Name: opts.Hostname}
	}

	if opts.RequestedIP != "" {
		addr, err := netip.ParseAddr(opts.RequestedIP)
		if err != nil || !addr.Is4() {
			return proto.Params{}, fmt.Errorf("dhcp: requested IP %q is not an IPv4 address", opts.RequestedIP)
		}
		p.RequestedIP = addr
	}

	allow, err := parseAddrs(opts.AllowServers)
	if err != nil {
		return proto.Params{}, fmt.Errorf("dhcp: dhcp_servers: %w", err)
	}
	deny, err := parseAddrs(opts.DenyServers)
	if err != nil {
		return proto.Params{}, fmt.Errorf("dhcp: dhcp_deny_servers: %w", err)
	}
	p.Servers = proto.ServerPolicy{Allow: allow, Deny: deny}

	// Broadcast is NOT set from an option, and that is the fix for a
	// regression this seam introduced rather than a simplification.
	//
	// proto.DefaultParams sets it TRUE and the library documents why:
	// the flag exists for "a client that cannot receive unicast IP
	// datagrams until its protocol software has been configured with an
	// IP address", which is exactly ring 3's raw AF_PACKET socket on an
	// unconfigured interface, and clearing it "produces a client that
	// works against servers ignoring the flag and hangs against those
	// honouring it".
	//
	// The plugin used to pass `mode == ModeIPvlan` here, carried across
	// unchanged from 1.x. Under dhcpcd that expression ADDED the flag
	// for ipvlan on top of whatever dhcpcd did by default (#243). Here
	// it OVERWROTE a default of true, so bridge and macvlan endpoints
	// cleared a flag the transport underneath them requires. The
	// expression survived the swap; its meaning inverted.
	//
	// The fixture cannot see this: dnsmasq and Kea both answer an
	// unconfigured client whether or not the flag is set, which is why
	// every integration suite is green. The server that decides it is
	// the one in production.

	// D-1, and it applies to BOTH managers.
	//
	// proto.DefaultParams desyncs the first DISCOVER by 1–10 seconds
	// (RFC 2131 section 4.4.1's "random delay between one and ten
	// seconds"), which is a rule about a fleet of hosts booting
	// together. Neither manager here is a fleet: each one is a single
	// container asking for a single address, started by a single
	// `docker run` or `docker start`.
	//
	// The acquisition manager was exempted first, because the desync
	// ate a lease_timeout that defaults to ten seconds. The Join
	// manager was left with the draw on the argument that a plugin
	// restart starts many of them at once — and that argument was
	// FALSIFIED by measurement rather than re-reasoned. Run
	// 33785125087 scored the `Resume`-dropped mutant and took two
	// extra kills with it, TestDNSPropagate_OptInWritesResolvConf and
	// TestMTUPropagate_OptInSetsLinkMTU, on two different shards. What
	// the run's own dumps showed was not a slow exchange but SILENCE:
	// the fixture logged exactly one DHCP transaction for the
	// container's MAC — CreateEndpoint's one-shot — and the plugin
	// logged, at teardown, "Persistent client stopped before it ever
	// held the lease; the one-shot's lease is left to expire on the
	// server". The Join manager spent the container's whole life
	// inside the draw and sent nothing.
	//
	// That path is reachable UNMUTATED: proto.Machine.takeResume
	// returns false for a remembered lease that is no longer live and
	// falls through to this same draw, so a JOINED record whose lease
	// expired while the container was down (a weekend) starts from
	// INIT with a 1–10 s wait in front of it. Options 6 and 26 are
	// applied from the bind event (pkg/plugin/dhcp_manager.go:508-509,
	// reached from the "bound" and "renew" arms), so for the length of
	// the draw plus a DORA the container runs with Docker's own
	// resolv.conf and the link-default MTU on a network that opted
	// into propagate_dns / propagate_mtu.
	//
	// The blast radius is exactly one packet, and that is checkable
	// rather than asserted: the library requests the desync only for
	// EvStart and EvLinkUp (proto/machine.go, stepStopped and
	// stepInit; every other re-acquisition passes withDesync=false),
	// nothing above ring 1 in this tree ever emits EvLinkUp, and
	// lease.Manager.Run dispatches EvStart exactly once. So this
	// assignment moves the FIRST packet of a cold-lease start and
	// nothing else.
	//
	// Both zero disables the delay; the library documents that as the
	// disabling value rather than as a degenerate range.
	p.DesyncMin, p.DesyncMax = 0, 0

	return p, nil
}

func parseAddrs(in []string) ([]netip.Addr, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]netip.Addr, 0, len(in))
	for _, s := range in {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address: %w", s, err)
		}
		if !a.Is4() {
			return nil, fmt.Errorf("%q is not an IPv4 address", s)
		}
		out = append(out, a)
	}
	return out, nil
}
