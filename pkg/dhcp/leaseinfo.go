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

// The library has merged the advertisement's gateway, MTU, resolvers, search list and routes into the lease (RFC 4861
// section 6.3.4, RFC 8106 section 5.3.1); on-link prefixes and the network's main prefix are not lease properties, so
// they are passed in and stay inside sanitizeInfo's pass (#821, #818).

// infoFromLease renders one library lease and its link's router observation as the Info the plugin applies.
func infoFromLease(l lease.Lease, r proto.RouterObservation, now time.Time, main netip.Prefix) (Info, int) {
	info := Info{
		MTU:          l.MTU,
		SearchList:   append([]string(nil), l.DomainSearch...),
		LeaseSeconds: leaseSeconds(l, now),
		SLAAC:        l.SLAAC,
	}
	if l.Addr.IsValid() {
		info.IP = l.Addr.String()
	}
	// Per-address values only: proto.Lease6.PreferredUntil refuses a zero preferred lifetime, so a deprecated address
	// leaves Lease.Preferred at the zero Time that also means infinite, and the kernel would never mark it deprecated
	// (RFC 4862 section 5.5.4, #819).
	fillV6Addrs(&info, l, now, main)
	if l.Gateway.IsValid() && !l.Gateway.IsUnspecified() {
		info.Gateway = l.Gateway.String()
	}
	for _, d := range l.DNS {
		info.DNSServers = append(info.DNSServers, d.String())
	}

	// RFC 3442: option 121's 0.0.0.0/0 supersedes option 3 and the library folds it into Gateway; filtered again here
	// so a library change cannot yield a second default route (#899).
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
	info.WithdrawnOnLinkPrefixes = withdrawnOnLinkPrefixes(r)

	// DHCPv6 has no MTU option (option 26 is DHCPv4's, RFC 2132 section 5.1), so RFC 4861 section 4.6.4's is the v6
	// MTU; the kernel stopped copying it at accept_ra=0 (#821). Guarded on Seen, and a server's option 26 still wins.
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

	// sanitizeInfo runs at the one point every lease enters the plugin and feeds unsafe_option_values_dropped (#703).
	dropped := sanitizeInfo(&info)

	// SafeValue passes the space that separates `search` entries (#704); applied after sanitizeInfo so the two counts
	// add.
	if d, cut := FirstSearchDomain(l.Domain); cut {
		info.Domain = d
		dropped++
	} else {
		info.Domain = d
	}

	return info, dropped
}

// A zero Expire is an infinite lease (0xFFFFFFFF on the wire); without this branch the deadline lands in year 1 (#899).

// leaseSeconds is the remaining lifetime the plugin's own bookkeeping reads.
func leaseSeconds(l lease.Lease, now time.Time) int {
	return secondsUntil(l.Expire, now)
}

// secondsUntil is the whole seconds to a deadline, a zero Time meaning no deadline, for v4 expiry and both v6 lifetimes
// (#911).
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

// wire.Route.IsDefault is v4-only, and RFC 4191 section 2.3 allows a ::/0 Route Information Option, which would sit
// beside Docker's default route (#821).

// defaultDestination reports whether a route's destination is the whole address space, in either family.
func defaultDestination(r wire.Route) bool { return r.Dest.Bits() == 0 }

// RFC 4861 section 4.6.2: without L an option says nothing about on-link. Section 6.3.4: a zero valid lifetime times
// the prefix out. fe80::/64 is the kernel's on every interface, so it is skipped (#821).

// onLinkPrefixes renders the advertisement's L-flag Prefix Information options as CIDR.
func onLinkPrefixes(r proto.RouterObservation) []string { return lFlagPrefixes(r, false) }

// withdrawnOnLinkPrefixes renders the L-flag options that carry Valid Lifetime 0 as CIDR.
func withdrawnOnLinkPrefixes(r proto.RouterObservation) []string { return lFlagPrefixes(r, true) }

// lFlagPrefixes renders the L-flag options whose Valid Lifetime is zero exactly when withdrawn is set.
func lFlagPrefixes(r proto.RouterObservation, withdrawn bool) []string {
	var out []string
	for _, p := range r.Prefixes {
		if !p.OnLink || (p.ValidLifetime == 0) != withdrawn {
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

// RFC 4862 section 5.5.3 forms one address per prefix, up to proto.MaxSLAACAddresses. Lifetimes come from each entry,
// since the lease's pair is an aggregate. Docker's endpoint has one AddressIPv6: the first address inside
// `ipv6_main_prefix`, else the lease's first, reported as MainAddrFallback (#818).

// fillV6Addrs renders every address of a v6 lease with its own lifetimes and chooses the one reported to Docker.
func fillV6Addrs(info *Info, l lease.Lease, now time.Time, main netip.Prefix) {
	entries := l.Addrs
	if len(entries) == 0 {
		// Unreachable from the pinned library, which builds Lease.Addr from Lease.Addrs[0]; kept so the function is
		// total (#818).
		if !l.Addr.IsValid() || !l.Addr.Addr().Is6() {
			return
		}
		entries = []lease.Addr6{{Addr: l.Addr, Preferred: l.Preferred, Valid: l.Expire}}
	}
	info.Addrs = make([]V6Addr, 0, len(entries))
	kept := make([]lease.Addr6, 0, len(entries))
	for _, a := range entries {
		// An expired address would render as infinite, since zero is netlink's no-IFA_CACHEINFO; a deadline can pass
		// after the event (#818).
		if !a.Addr.IsValid() || (!a.Valid.IsZero() && !a.Valid.After(now)) {
			continue
		}
		kept = append(kept, a)
		info.Addrs = append(info.Addrs, V6Addr{
			IP:               a.Addr.String(),
			ValidSeconds:     secondsUntil(a.Valid, now),
			PreferredSeconds: v6PreferredSeconds(a, now),
			Deprecated:       v6Deprecated(a, now),
		})
	}
	if len(info.Addrs) == 0 {
		return
	}

	chosen := 0
	if main.IsValid() {
		chosen = -1
		for i, a := range kept {
			if a.Addr.IsValid() && main.Contains(a.Addr.Addr()) {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			chosen, info.MainAddrFallback = 0, true
		}
	}
	info.IP = info.Addrs[chosen].IP
	info.LeaseSeconds = info.Addrs[chosen].ValidSeconds
	info.PreferredSeconds = info.Addrs[chosen].PreferredSeconds
	info.IPDeprecated = info.Addrs[chosen].Deprecated
}

// A zero deadline is an infinite preferred lifetime, rendered as the valid one; 0 would read as deprecated to the
// kernel (#819).

// v6PreferredSeconds is one address's preferred lifetime on Info's convention.
func v6PreferredSeconds(a lease.Addr6, now time.Time) int {
	if a.Preferred.IsZero() {
		return secondsUntil(a.Valid, now)
	}
	return secondsUntil(a.Preferred, now)
}

// The pair (0, 0) is both a permanent address and a deprecated unbounded one, so this reads the deadline instead
// (#819).

// v6Deprecated reports RFC 4862 section 5.5.4's second phase: a preferred lifetime that is set and has run out.
func v6Deprecated(a lease.Addr6, now time.Time) bool {
	return !a.Preferred.IsZero() && !a.Preferred.After(now)
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
