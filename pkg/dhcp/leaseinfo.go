// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"strconv"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// infoFromLease renders one library lease, and what the routers on its
// link advertised, as the Info the plugin applies to a container.
//
// Everything the plugin does with a lease — the address, the default
// route, resolv.conf, the MTU, the more-specific routes, the audit line
// — reads this struct, so this function is the whole of the seam's
// data direction. The two rules that are not a field copy are marked.
//
// IT TAKES THE ROUTER OBSERVATION RATHER THAN ONLY THE LEASE, and that
// is not a convenience. The library has already merged the
// advertisement's gateway, MTU, resolvers, search list and
// more-specific routes into the lease it hands over (RFC 4861 section
// 6.3.4's union, RFC 8106 section 5.3.1's DHCP-first precedence), so
// those arrive as ordinary fields. On-link determination does not: it
// is not a property of the lease and has nowhere in it to live. Taking
// it here rather than adding it to the result afterwards is what keeps
// it inside sanitizeInfo's single boundary pass at the bottom.
func infoFromLease(l lease.Lease, r proto.RouterObservation, now time.Time) (Info, int) {
	info := Info{
		MTU:          l.MTU,
		SearchList:   append([]string(nil), l.DomainSearch...),
		LeaseSeconds: leaseSeconds(l, now),
	}
	// RFC 9915 section 7.1's preferred lifetime, on a v6 lease only.
	//
	// THE INFINITE CASE IS NOT ZERO SECONDS, and folding the two would
	// deprecate every address on an infinite lease the moment it was
	// installed. The library spells an infinite lifetime as the zero
	// Time, exactly as it does for Expire, and an infinite preferred
	// lifetime cannot be shorter than the valid one -- so it IS the
	// valid one, which is what the kernel is told. A preferred deadline
	// that is set and already past is a genuinely deprecated address
	// and comes out of secondsUntil as 0, which is what RFC 4862
	// section 5.5.4 asks for and is the case this branch keeps
	// distinguishable.
	if l.Addr.IsValid() && l.Addr.Addr().Is6() {
		if l.Preferred.IsZero() {
			info.PreferredSeconds = info.LeaseSeconds
		} else {
			info.PreferredSeconds = secondsUntil(l.Preferred, now)
		}
	}
	if l.Addr.IsValid() {
		info.IP = l.Addr.String()
	}
	if l.Gateway.IsValid() && !l.Gateway.IsUnspecified() {
		info.Gateway = l.Gateway.String()
	}
	for _, d := range l.DNS {
		info.DNSServers = append(info.DNSServers, d.String())
	}

	// RFC 3442: a 0.0.0.0/0 entry in option 121 supersedes option 3,
	// and the library has already folded it into Lease.Gateway. What is
	// left here is the non-default remainder, which is what the plugin
	// installs as StaticRoutes. Filtering rather than trusting the
	// library to have removed it keeps the two sides independent: if it
	// ever stopped folding, the default route would arrive twice rather
	// than the plugin installing a second one.
	for _, rt := range l.Routes {
		if defaultDestination(rt) {
			continue
		}
		info.Routes = append(info.Routes, Route{
			Destination: rt.Dest.String(),
			Gateway:     routeGateway(rt),
		})
	}
	info.OnLinkPrefixes = onLinkPrefixes(r)

	// THE ADVERTISED MTU IS THE ONLY MTU IPv6 HAS. DHCPv6 carries no
	// MTU option -- option 26 is DHCPv4's (RFC 2132 section 5.1) and
	// the library only ever fills Lease.MTU from it -- so RFC 4861
	// section 4.6.4's MTU option is where a v6 link's MTU comes from,
	// and without this line info.MTU is zero on every DHCPv6 lease and
	// the plugin has nothing to apply. Until #821 that did not show:
	// the container's kernel was at accept_ra=2 and copied the
	// advertised MTU itself.
	//
	// Guarded on Seen so it cannot reach a DHCPv4 lease, whose client
	// never looks at a router advertisement and whose observation is
	// therefore the zero value; and placed after the lease's own value
	// so a server that did send option 26 still wins on its own family.
	info.RouterSeen = r.Seen
	if info.MTU == 0 && r.Seen {
		info.MTU = int(r.MTU)
	}

	info.NTPServers = addrStrings(l.Options, wire.OptNTPServer)
	info.TFTPServer = optText(l.Options, wire.OptTFTPServer)
	info.BootFile = optText(l.Options, wire.OptBootfileName)
	info.WPAD = optText(l.Options, wire.OptWPAD)
	info.PosixTimezone = optText(l.Options, wire.OptPosixTimezone)
	info.TZDBTimezone = optText(l.Options, wire.OptTZDatabase)
	if v, ok := l.Options.Int32(wire.OptTimeOffset); ok {
		info.TimeOffset = strconv.Itoa(int(v))
	}

	// The server chose every string above. sanitizeInfo drops the ones
	// carrying a control character and says how many, which is the
	// count the plugin's unsafe_option_values_dropped counter reads.
	// It runs HERE, at the one point every lease crosses into the
	// plugin, rather than at each consumer: resolv.conf, the link MTU
	// and a log line have no escaping in common, so the only answer
	// that holds for all three is not to carry the value.
	dropped := sanitizeInfo(&info)

	// Option 15 needs a rule sanitizeInfo cannot supply, and 0x20 is
	// why: SafeValue rejects r < 0x20, and the space — precisely the
	// field separator of resolv.conf's `search` line — is 0x20. So a
	// domain of "a.attacker.test corp.example" passes the filter whole
	// and becomes two search entries, with the server's choice first,
	// which decides what a bare name resolves to (#704). Applied AFTER
	// the filter so the two counts add rather than one masking the
	// other, and applied here rather than at the resolv.conf writer so
	// the drop is counted at the boundary every lease crosses.
	if d, cut := FirstSearchDomain(l.Domain); cut {
		info.Domain = d
		dropped++
	} else {
		info.Domain = d
	}

	return info, dropped
}

// leaseSeconds is the remaining lifetime the plugin's own bookkeeping
// reads.
//
// A ZERO Expire IS AN INFINITE LEASE (seam D-10). The protocol spells
// that 0xFFFFFFFF and the library represents it as the zero Time so it
// is a value a caller can test rather than a threshold it has to
// guess. Computing a duration from it without this branch yields a
// deadline in year 1, and every consumer downstream then reports an
// outage on a lease that never expires.
func leaseSeconds(l lease.Lease, now time.Time) int {
	return secondsUntil(l.Expire, now)
}

// secondsUntil is the remaining whole seconds to a deadline, with a
// zero Time meaning "no deadline" rather than "the epoch".
//
// ONE FUNCTION FOR BOTH v6 LIFETIMES AND THE v4 EXPIRY, because they
// share the convention and sharing the convention is the whole hazard:
// the protocol spells an infinite lifetime 0xFFFFFFFF, the library
// represents it as the zero Time, and a duration computed from it
// without this branch lands in year 1 -- after which every consumer
// downstream reports an outage on a lease that never expires (seam
// D-10).
func secondsUntil(deadline, now time.Time) int {
	if deadline.IsZero() {
		return 0
	}
	d := deadline.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}

// defaultDestination reports whether a route's destination is the whole
// address space, in EITHER family.
//
// wire.Route.IsDefault answers it for v4 only — it is
// `Dest.Bits() == 0 && Dest.Addr().Is4()`, which is RFC 3442's question
// about option 121 and is false for ::/0. RFC 4191 section 2.3 allows a
// Route Information Option with a prefix length of zero, and the
// library's router table carries it through like any other, so a v6
// endpoint on such a segment would otherwise be handed ::/0 as a static
// route beside the default route Docker installs from the same router.
func defaultDestination(r wire.Route) bool { return r.Dest.Bits() == 0 }

// onLinkPrefixes is the advertisement's on-link determination: RFC 4861
// section 4.6.2's Prefix Information options with the L flag set,
// rendered as CIDR.
//
// STANDARD RFC 4861 section 4.6.2 on the L flag: "When set, indicates
// that this prefix can be used for on-link determination. When not set
// the advertisement makes no statement about on-link or off-link
// properties of the prefix." So an option with L clear is not a prefix
// this is silent about by accident.
//
// STANDARD RFC 4861 section 6.3.4 on the lifetime: a prefix is entered
// in the Prefix List only when "the Prefix Information option's Valid
// Lifetime field is non-zero", and a zero valid lifetime on a prefix
// already there means to "time out the prefix immediately". Zero is
// therefore a withdrawal and not a prefix with no time left.
//
// The link-local prefix is skipped for the reason section 5.5.3 b skips
// it on the other path: the kernel owns fe80::/64 on every interface
// that has an address at all, and installing a second route for it
// would be this plugin claiming a prefix it did not configure.
func onLinkPrefixes(r proto.RouterObservation) []string {
	var out []string
	for _, p := range r.Prefixes {
		if !p.OnLink || p.ValidLifetime == 0 {
			continue
		}
		if !p.Prefix.Is6() || p.Prefix.Is4In6() || p.Prefix.IsLinkLocalUnicast() {
			continue
		}
		pfx := netip.PrefixFrom(p.Prefix, int(p.PrefixLen))
		if !pfx.IsValid() || pfx.Bits() == 0 {
			continue
		}
		s := pfx.Masked().String()
		if !containsString(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func containsString(in []string, s string) bool {
	for _, v := range in {
		if v == s {
			return true
		}
	}
	return false
}

func routeGateway(r wire.Route) string {
	if r.OnLink() {
		return ""
	}
	return r.Router.String()
}

func addrStrings(o wire.Options, c wire.OptionCode) []string {
	addrs, ok := o.Addrs4(c)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

func optText(o wire.Options, c wire.OptionCode) string {
	s, ok := o.Text(c)
	if !ok {
		return ""
	}
	return s
}
