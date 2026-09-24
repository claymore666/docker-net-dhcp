// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "github.com/claymore666/dhcp-golib/lease"

// The solicitation count is here because zero AdvertsSeen means either silent routers or a client that never asked
// (#814).

// RouterStats is what the library counted of RFC 4861 router discovery on one link.
type RouterStats struct {
	// SolicitsSent is the Router Solicitations sent (RFC 4861 section 6.3.7).
	SolicitsSent uint64

	// AdvertsSeen is advertisements that decoded, AdvertsRefused Router Advertisements that did not (#814).
	AdvertsSeen    uint64
	AdvertsRefused uint64

	// OptionsIgnored counts options refused by their own validity rule out of advertisements otherwise read (#814).
	OptionsIgnored uint64

	// EntriesDropped is an arrival a full router-table list refused, EntriesEvicted an entry it threw out (RFC 8106
	// section 6.2 (d)).
	EntriesDropped uint64
	EntriesEvicted uint64
}

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

// Sub returns the counters gained since prev, saturating at zero, since the library's counters only rise (#814).
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
