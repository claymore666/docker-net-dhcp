// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/proto"
)

// buildParams6 turns one endpoint's options into the DHCPv6 parameter
// set for one manager instance.
//
// It is buildParams's twin and NOT a mode of it. RFC 9915 section 1.2
// declines to merge the two families and the parameter sets have no
// field in common: proto.Params carries option 60, option 61 and RFC
// 2131 section 4.4.1's desync, proto.Params6 carries a DUID, an IAID
// and RFC 9915 section 7.6's twenty-three retransmission parameters.
// A single builder would be a switch over two disjoint bodies.
//
// once is accepted for the same reason buildParams accepts it — it is
// how the seam names which manager a call site is building — and, as
// there, it selects nothing. The equality is the rule; a future arm has
// to be argued at the assignment it would sit beside.
func buildParams6(opts *DHCPClientOptions, once bool) (proto.Params6, error) {
	if !opts.V6 {
		return proto.Params6{}, fmt.Errorf("dhcp: buildParams6 was asked for a DHCPv4 endpoint")
	}
	if opts.Identity6.IsZero() {
		// REFUSED HERE RATHER THAN AT THE LIBRARY, which would refuse
		// it too. The library's message names wire.DUIDLL and
		// wire.DUIDUUID, which are the right advice for a caller
		// writing against the library and the wrong advice here: the
		// chassis mints identities in one place (resolveV6Identity)
		// and an empty one means that place was not consulted.
		return proto.Params6{}, fmt.Errorf("dhcp: no DHCPv6 identity for the endpoint " +
			"(RFC 9915 section 11: a DUID \"SHOULD NOT change over time if at all possible\", " +
			"so the chassis mints it once and stores it)")
	}

	p := proto.DefaultParams6()
	p.DUID = append([]byte(nil), opts.Identity6.DUID...)
	p.IAID = opts.Identity6.IAID

	// The two RFC 3646 lists. The library adds section 21.24's, 21.25's
	// and 21.23's mandatory codes itself, per message type, so this is
	// exactly the caller's half and nothing is spelled twice.
	//
	// DefaultParams6 leaves ORO nil (MEASURED at the library's M7c
	// round: a stateless client with a nil ORO gets a Reply with no
	// DNS in it at all, because dnsmasq answers an Information-request
	// with exactly what was requested). P-8.5 is the whole reason the
	// v6 path exists on a stateless segment, so the default has to be
	// asked for here.
	p.ORO = proto.DefaultORO()

	// #213: `docker run --ip6` and a tombstone's address arrive as the
	// IA Address hint inside the Solicit's IA_NA (section 18.2.1: the
	// client MAY include addresses "as hints to the server"). Empty
	// sends no hint, and a server is free to ignore one that is sent.
	if opts.PreferredV6 != "" {
		addr, err := netip.ParseAddr(opts.PreferredV6)
		if err != nil || !addr.Is6() || addr.Is4In6() {
			return proto.Params6{}, fmt.Errorf("dhcp: preferred IPv6 address %q is not an IPv6 address", opts.PreferredV6)
		}
		p.Hint = addr
	}

	// NO SERVER POLICY, AND IT CANNOT BE ADDED HERE (P-8.20). The
	// allow- and deny-lists filter on DHCPv4's option 54, which is a
	// server's IPv4 address; RFC 9915's Server Identifier (section
	// 21.3) is an opaque DUID and names no address at all. proto.Params6
	// has no Servers field, so a v6 client that tried to carry one
	// would not compile — which is the guard this row wants, rather
	// than a comment saying the lists are ignored.

	return p, nil
}
