// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"net"
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

// buildParams turns one endpoint's options into the protocol parameter
// set for one manager instance.
//
// once distinguishes the CreateEndpoint acquisition manager from the
// persistent Join manager, and the only thing it changes is the desync
// window — see the DesyncMin/DesyncMax assignment below.
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
	if len(opts.ClientID) > 0 {
		p.ClientID = append([]byte{clientIDTypeOpaque}, opts.ClientID...)
	}

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

	p.Broadcast = opts.Broadcast

	if once {
		// D-1. proto.DefaultParams desyncs the first DISCOVER by 1–10
		// seconds (RFC 2131 section 4.4.1's "random delay between one
		// and ten seconds"), which is a rule about a fleet of hosts
		// booting together. The acquisition manager is one container
		// asking for one address inside a `docker run`, against a
		// lease_timeout that defaults to ten seconds — so the desync
		// does not spread load here, it eats the budget, and the
		// failure is intermittent rather than reproducible.
		//
		// Both zero disables the delay; the library documents that as
		// the disabling value rather than as a degenerate range.
		p.DesyncMin, p.DesyncMax = 0, 0
	}

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

// hardwareAddr is net.HardwareAddr's String on a byte slice the library
// handed back, used for logging only.
func hardwareAddr(b []byte) string { return net.HardwareAddr(b).String() }
