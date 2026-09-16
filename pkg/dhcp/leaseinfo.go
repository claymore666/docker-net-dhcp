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

// infoFromRouter builds an Info from the Router Advertisement ALONE,
// with no DHCPv6 lease behind it.
//
// WHY IT EXISTS. infoFromLease reads Gateway, MTU, DNS, DomainSearch
// and Routes off the lease, because the library folds the router table
// into the lease it hands back. On a segment that advertises M=0 there
// is no lease to fold anything into, and that segment is precisely the
// one where the advertisement is the ONLY source of configuration: no
// DHCPv6 address, no DHCPv6 options, one router saying what the link
// is. Before #821 the container's kernel read it. #821 turns the kernel
// off, so something has to read it here or the container gets nothing.
//
// NO ADDRESS AND NO LIFETIMES, deliberately. Info.IP stays empty and
// the caller can still ask "did this produce an address" the way it
// always has. Forming an address from the advertised prefix is SLAAC
// and belongs to #818; this function is about the other four things an
// advertisement carries.
//
// THE GATEWAY COMES FROM Routers AND NOT FROM Router. Router is "who
// last spoke" and never expires; Routers is RFC 4861 section 6.3.4's
// Default Router List, which a Router Lifetime of 0 empties. Reading
// Router here would give a withdrawn router back as a gateway forever.
func infoFromRouter(r proto.RouterObservation) (Info, int) {
	info := Info{
		MTU:            int(r.MTU),
		SearchList:     append([]string(nil), r.Search...),
		OnLinkPrefixes: onLinkPrefixes(r),
	}
	if len(r.Routers) > 0 {
		info.Gateway = r.Routers[0].String()
	}
	for _, d := range r.DNS {
		info.DNSServers = append(info.DNSServers, d.String())
	}
	// Same filter as the lease path and for the same reason: ::/0 in a
	// Route Information option IS the default route (RFC 4191 allows
	// it), and exporting it as a static route as well would install the
	// default twice.
	for _, rt := range r.Routes {
		if defaultDestination(rt) {
			continue
		}
		info.Routes = append(info.Routes, Route{
			Destination: rt.Dest.String(),
			Gateway:     routeGateway(rt),
		})
	}
	// The router chose every string above, so it gets the same
	// treatment a server's do. The count is returned rather than
	// counted here; the caller decides whether it has an event to carry
	// it on.
	return info, sanitizeInfo(&info)
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
