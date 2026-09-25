// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"
)

// A twin of buildParams, not a mode of it: RFC 9915 section 1.2 keeps the families apart and the two parameter sets
// share no field (#911).

// buildParams6 turns one endpoint's options into the DHCPv6 parameters for one manager instance; once selects nothing.
func buildParams6(opts *DHCPClientOptions, once bool) (proto.Params6, error) {
	if !opts.V6 {
		return proto.Params6{}, fmt.Errorf("dhcp: buildParams6 was asked for a DHCPv4 endpoint")
	}
	if opts.Identity6.IsZero() {
		// Refused here, not left to the library, whose message advises wire.DUIDLL; an empty identity means
		// resolveV6Identity was not consulted (#911).
		return proto.Params6{}, fmt.Errorf("dhcp: no DHCPv6 identity for the endpoint " +
			"(RFC 9915 section 11: a DUID \"SHOULD NOT change over time if at all possible\", " +
			"so the chassis mints it once and stores it)")
	}

	// Mode6Off is refused here: reaching this function with it means the plugin started a v6 client for a network with
	// no IPv6 (#817).
	if opts.Mode6 == proto.Mode6Off {
		return proto.Params6{}, fmt.Errorf("dhcp: ipv6_mode is off for this network, " +
			"so no DHCPv6 client is built for it; a caller that reached here read the " +
			"network's IPv6 decision in two places and they disagreed")
	}

	p := proto.DefaultParams6()
	p.DUID = append([]byte(nil), opts.Identity6.DUID...)
	p.IAID = opts.Identity6.IAID
	p.Mode = opts.Mode6
	// RFC 4291 Appendix A's interface identifier comes from the link's hardware address, which the library never fills
	// in; it is the MAC the chaddr and DUID-LL are pinned to (#152).
	p.LinkAddr = append([]byte(nil), opts.MAC...)
	if opts.StrictAuto6 {
		p.AutoFallback = strictAutoFallback
	}

	// The library adds the mandatory codes itself, so this is the caller's RFC 3646 half. DefaultParams6 leaves ORO
	// nil, and dnsmasq answers an Information-request with only what was requested, so a stateless client would get no
	// DNS (#911).
	p.ORO = proto.DefaultORO()

	// register_dns means RFC 4704 option 39, which the library sends with S=1 on Solicit, Request, Renew and Rebind.
	// Without it no name goes out: v6 has no option 12, and the library cannot send a name without S (#1029 (a)).
	if opts.FQDN != "" {
		p.Hostname = opts.Hostname
	}

	// The #213 hint: RFC 9915 section 18.2.1 lets a Solicit carry addresses "as hints to the server", which a server
	// may ignore.
	if opts.PreferredV6 != "" {
		addr, err := netip.ParseAddr(opts.PreferredV6)
		if err != nil || !addr.Is6() || addr.Is4In6() {
			return proto.Params6{}, fmt.Errorf("dhcp: preferred IPv6 address %q is not an IPv6 address", opts.PreferredV6)
		}
		// Mode6SLAAC sends no Solicit, so the hint cannot be made and is dropped with a log line, not silently (#817,
		// #816). A malformed address is refused in every mode, so a network that changes mode does not find it only
		// then.
		if p.Mode == proto.Mode6SLAAC {
			log.WithField("preferred_ipv6", addr.String()).
				Info("ipv6_mode=slaac forms its address from the router's advertised prefix, " +
					"so the preferred DHCPv6 address for this endpoint is not asked for")
		} else {
			p.Hint = addr
		}
	}

	// RFC 9915's Server Identifier (section 21.3) is an opaque DUID, so the option-54 server lists cannot apply, and
	// proto.Params6 has no Servers field (#911).

	return p, nil
}
