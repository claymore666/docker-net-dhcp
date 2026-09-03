// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"strconv"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

// infoFromLease renders one library lease as the Info the plugin
// applies to a container.
//
// Everything the plugin does with a lease — the address, the default
// route, resolv.conf, the MTU, the classless routes, the audit line —
// reads this struct, so this function is the whole of the seam's
// data direction. The two rules that are not a field copy are marked.
func infoFromLease(l lease.Lease, now time.Time) (Info, int) {
	info := Info{
		MTU:          l.MTU,
		SearchList:   append([]string(nil), l.DomainSearch...),
		LeaseSeconds: leaseSeconds(l, now),
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
	// installs as StaticRoutes. Filtering on IsDefault rather than
	// trusting the library to have removed it keeps the two sides
	// independent: if it ever stopped folding, the default route would
	// arrive twice rather than the plugin installing a second one.
	for _, r := range l.Routes {
		if r.IsDefault() {
			continue
		}
		info.Routes = append(info.Routes, Route{
			Destination: r.Dest.String(),
			Gateway:     routeGateway(r),
		})
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
	if l.Expire.IsZero() {
		return 0
	}
	d := l.Expire.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
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
