// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "github.com/claymore666/dhcp-golib/lease"

// A FORCERENEW or a Reconfigure arrives on a bound lease only, so the fold is on the persistent client and the
// one-shot paths need none (#1119).

// ForcerenewStats is what the library counted of DHCPv4 FORCERENEW (RFC 3203, RFC 6704) and DHCPv6 Reconfigure
// (RFC 8415 section 20.4) on one lease.
type ForcerenewStats struct {
	// Renewed is an authenticated FORCERENEW that started a renewal, AlreadyRenewing one that arrived while a renewal
	// was under way, Refused one the library discarded by any rule. AckRefused is an ACK discarded for lacking the
	// option 90 its OFFER promised and is not part of Refused (#1119).
	Renewed         uint64
	AlreadyRenewing uint64
	AckRefused      uint64
	Refused         uint64

	// ReconfiguresAccepted and ReconfiguresRefused are the DHCPv6 twins. A refusal is not a fault: a resumed lease
	// holds no reconfigure key and a client with AcceptReconfigure off refuses every one while working as asked (#1119).
	ReconfiguresAccepted uint64
	ReconfiguresRefused  uint64
}

func forcerenewStats(s lease.Stats) ForcerenewStats {
	return ForcerenewStats{
		Renewed:              s.ForcerenewsRenewed,
		AlreadyRenewing:      s.ForcerenewsAlreadyRenewing,
		AckRefused:           s.ForcerenewsAckRefused,
		Refused:              s.ForcerenewsRefused,
		ReconfiguresAccepted: s.ReconfiguresAccepted,
		ReconfiguresRefused:  s.ReconfiguresRefused,
	}
}

// Sub returns the counters gained since prev, saturating at zero, since the library's counters only rise (#1119).
func (s ForcerenewStats) Sub(prev ForcerenewStats) ForcerenewStats {
	return ForcerenewStats{
		Renewed:              sub(s.Renewed, prev.Renewed),
		AlreadyRenewing:      sub(s.AlreadyRenewing, prev.AlreadyRenewing),
		AckRefused:           sub(s.AckRefused, prev.AckRefused),
		Refused:              sub(s.Refused, prev.Refused),
		ReconfiguresAccepted: sub(s.ReconfiguresAccepted, prev.ReconfiguresAccepted),
		ReconfiguresRefused:  sub(s.ReconfiguresRefused, prev.ReconfiguresRefused),
	}
}

// IsZero reports whether nothing moved.
func (s ForcerenewStats) IsZero() bool { return s == ForcerenewStats{} }

// AnyRefused reports whether a refusal count moved, which is what separates a warning from an info line.
func (s ForcerenewStats) AnyRefused() bool {
	return s.Refused != 0 || s.AckRefused != 0 || s.ReconfiguresRefused != 0
}
