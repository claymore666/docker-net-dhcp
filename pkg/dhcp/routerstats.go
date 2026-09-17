// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "github.com/claymore666/dhcp-golib/lease"

// RouterStats is what the library counted of RFC 4861 router discovery
// on one link: the solicitations it sent, the advertisements that
// reached it, and the ones it or its router table would not take.
//
// SIX FIELDS AND NOT lease.Stats ENTIRE, on ACDStats' rule: these are
// the ones that answer "is this link's router discovery working", which
// is the question an operator has to be able to answer before reading
// an IPv6 endpoint with no gateway, no MTU and no resolver as a
// misconfigured segment. The rest of lease.Stats is already
// per-endpoint on the durable record.
//
// THE SOLICITATION COUNT IS IN HERE BECAUSE THE SIGHTING COUNT IS
// UNREADABLE WITHOUT IT, which is acd_probes_sent's reason one family
// over. AdvertsSeen of zero is either a link whose routers are silent
// or a client that never asked, and only one of those is a segment to
// go and fix.
type RouterStats struct {
	// SolicitsSent is RFC 4861 section 6.3.7's Router Solicitations
	// that left the host.
	SolicitsSent uint64

	// AdvertsSeen is advertisements that decoded, AdvertsRefused
	// frames whose ICMPv6 type said Router Advertisement and which
	// would not decode.
	//
	// TWO COUNTERS BECAUSE THEIR DIFFERENCE IS THE DIAGNOSTIC. A link
	// with no router and a link whose router is advertising something
	// this client refuses are the same number in a total that holds
	// both, and the operator's next step differs: one is a router to
	// find, the other is a router to fix.
	AdvertsSeen    uint64
	AdvertsRefused uint64

	// OptionsIgnored counts OPTIONS and not frames: one option refused
	// by its own standard's validity rule out of an advertisement the
	// rest of which was read. It rises on advertisements that are
	// otherwise fine, which is why it is not folded into
	// AdvertsRefused.
	OptionsIgnored uint64

	// EntriesDropped is an arrival a full list in the library's router
	// table would not take, EntriesEvicted an entry a full list threw
	// out to take an arrival (RFC 8106 section 6.2 (d)).
	//
	// EITHER ABOVE ZERO MEANS THE TABLE'S CAPS ARE IN FORCE, which on
	// a link carrying one segment's worth of routers means something
	// is advertising more than a link has. They are two counters
	// rather than one because they lose differently: a refusal holds
	// what was heard FIRST and keeps a newer router out, an eviction
	// holds what expires LAST and throws a held entry away.
	EntriesDropped uint64
	EntriesEvicted uint64
}

// routerStats projects the library's counters onto the six above.
func routerStats(s lease.Stats) RouterStats {
	return RouterStats{
		SolicitsSent:   s.RouterSolicitsSent,
		AdvertsSeen:    s.RouterAdvertsSeen,
		AdvertsRefused: s.RouterAdvertsRefused,
		OptionsIgnored: s.RouterAdvertOptionsIgnored,
		EntriesDropped: s.RouterTableEntriesDropped,
		EntriesEvicted: s.RouterTableEntriesEvicted,
	}
}

// Sub returns the counters gained since prev, saturating rather than
// wrapping for ACDStats.Sub's reason: the library's counters only rise
// within one manager, so a negative difference is impossible by
// construction and a nonsense value here would become a huge positive
// delta on an unsigned subtraction.
func (s RouterStats) Sub(prev RouterStats) RouterStats {
	return RouterStats{
		SolicitsSent:   sub(s.SolicitsSent, prev.SolicitsSent),
		AdvertsSeen:    sub(s.AdvertsSeen, prev.AdvertsSeen),
		AdvertsRefused: sub(s.AdvertsRefused, prev.AdvertsRefused),
		OptionsIgnored: sub(s.OptionsIgnored, prev.OptionsIgnored),
		EntriesDropped: sub(s.EntriesDropped, prev.EntriesDropped),
		EntriesEvicted: sub(s.EntriesEvicted, prev.EntriesEvicted),
	}
}

// IsZero reports whether nothing moved.
func (s RouterStats) IsZero() bool { return s == RouterStats{} }
