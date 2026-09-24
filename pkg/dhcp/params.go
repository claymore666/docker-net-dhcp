// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/proto"
)

// Supplied here: an empty proto.Params.VendorClass sends no option 60, while an empty vendor_class has always sent this
// string, and a server keyed on it would fall through to the untagged pool (#899).

// VendorID is DHCPv4 option 60 when the network sets no vendor_class.
const VendorID = "docker-net-dhcp"

// The library sends Params.ClientID verbatim, so a missing type byte files the lease under another key and a second
// address (#899).

// clientIDTypeOpaque is RFC 2132 section 9.14's type byte for a client-identifier that is not a DUID.
const clientIDTypeOpaque = 0x00

// Exported because the durable record stores the identity as sent, not re-derived; a payload-only record would name
// another client (#899).

// ClientIdentity is the option-61 value as sent: the chassis's type byte followed by the caller's payload.
func ClientIdentity(clientID []byte) []byte {
	if len(clientID) == 0 {
		return nil
	}
	return append([]byte{clientIDTypeOpaque}, clientID...)
}

// A re-bind must go out under the value the server filed the lease under; a DUID or other shape is refused, not
// truncated, so no value goes on the wire that no record holds (#110).

// ClientIDPayload is ClientIdentity's inverse, and ok is false for an identity this chassis did not write.
func ClientIDPayload(identity []byte) ([]byte, bool) {
	if len(identity) < 2 || identity[0] != clientIDTypeOpaque {
		return nil, false
	}
	return append([]byte(nil), identity[1:]...), true
}

// buildParams turns one endpoint's options into the DHCPv4 parameters for one manager instance; once selects nothing
// (#899).
func buildParams(opts *DHCPClientOptions, once bool) (proto.Params, error) {
	if opts.V6 {
		// proto.Params is RFC 2131's; a v6 endpoint takes buildParams6, and a v6 client handed this would send a
		// DHCPDISCOVER (#911).
		return proto.Params{}, fmt.Errorf("dhcp: buildParams was asked for a DHCPv6 endpoint")
	}
	if len(opts.MAC) == 0 {
		return proto.Params{}, fmt.Errorf("dhcp: no MAC address for the endpoint")
	}

	// The library's probe-window own-traffic exemption is keyed on CHAddr, so a CHAddr that is not the link's address
	// DECLINEs its own address (#882). Callers pass link.Attrs().HardwareAddr;
	// TestConflictCheck_BridgeModeDoesNotSelfReport is the end-to-end proof.
	p := proto.DefaultParams(opts.MAC)

	p.Hostname = opts.Hostname
	p.VendorClass = opts.VendorClass
	if p.VendorClass == "" {
		p.VendorClass = VendorID
	}
	p.ClientID = ClientIdentity(opts.ClientID)

	// RFC 5227 per network; the zero value is proto.ConflictWait. Both managers get the same mode: the one-shot wins
	// the address and the Join manager must keep listening for section 2.4's conflicts (#882).
	p.Conflict = opts.ConflictMode

	// register_dns means RFC 4702 option 81; zero flags resolve in the library to S|E, the A and PTR RRs in canonical
	// wire format (#899).
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

	// Broadcast stays at the library's true: a raw AF_PACKET socket on an unconfigured interface cannot receive unicast
	// (RFC 2131 section 4.1). The 1.x `mode == ModeIPvlan` expression here cleared it for bridge and macvlan (#243,
	// #899). dnsmasq and Kea answer either way, so no fixture can see it.

	// RFC 2131 section 4.4.1's 1-10 s desync is for a fleet booting together; each manager here is one container. Run
	// 33785125087 showed the Join manager spending a container's life inside the draw: a JOINED record whose lease
	// expired while the container was down starts from INIT, and dhcpManager.renew applies options 6 and 26 only after
	// a bind (#899). The library draws only for EvStart and EvLinkUp, so this moves one packet. Zero for both disables
	// the delay.
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
