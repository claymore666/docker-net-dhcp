// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// A failed DHCPv6 acquisition has two entirely different meanings and a
// timeout cannot tell them apart. #868 is what happened while it was
// treated as one: on a stateless or SLAAC IPv6 network there is no
// DHCPv6 address BY DEFINITION, the acquisition always timed out, and
// CreateEndpoint treated that as fatal -- so no container started at
// all on the ordinary configuration of a great many home routers.
//
// The discriminator is what the segment ADVERTISED, not how long we
// waited. That distinction is what keeps the guard one-directional:
// tolerating "this segment offers no DHCPv6" must not also tolerate
// "this segment offers DHCPv6 and the server went quiet", and those are
// two different observations rather than two readings of one timeout.
type v6Verdict int

const (
	// v6Fatal: the segment advertised the managed-address flag, so it
	// offers DHCPv6 addresses and the silence is a real failure. This
	// is the behaviour that shipped before #868 and it is preserved
	// exactly for this case.
	v6Fatal v6Verdict = iota
	// v6NotOffered: a router advertisement arrived WITHOUT the managed
	// flag -- stateless (O only) or plain SLAAC. The segment has said
	// there are no DHCPv6 addresses here. This is the NORMAL state, not
	// a degraded one, and the endpoint is created without a v6 address
	// so SLAAC can provide one on the container's own link.
	v6NotOffered
	// v6NoRouter: no router advertisement arrived at all within the
	// acquisition budget.
	//
	// This one is a judgement and it is deliberately not silent. A
	// segment with no router cannot give a container a working IPv6
	// address by ANY mechanism, so refusing to start the container buys
	// nothing -- but it is also indistinguishable from a broken
	// segment, so it gets its own counter and a warning naming the
	// interface rather than passing as an ordinary success.
	v6NoRouter
)

// classifyV6Absence turns what the acquisition observed about the
// segment's router advertisements into the verdict for a DHCPv6
// acquisition that produced no address.
//
// Pure and total: every RAObservation maps to exactly one verdict, and
// the only input is what was advertised.
func classifyV6Absence(ra dhcp.RAObservation) v6Verdict {
	switch {
	case !ra.Seen:
		return v6NoRouter
	case ra.Managed:
		return v6Fatal
	default:
		return v6NotOffered
	}
}

// noteV6Absence records a tolerated DHCPv6 absence and reports whether
// the endpoint may proceed without a v6 address.
//
// Counting is the caller's evidence of intent; it is NOT evidence of
// effect. What proves the fix is a container starting and the address
// it ends up with, which is what the integration cases assert.
func (p *Plugin) noteV6Absence(ra dhcp.RAObservation, iface, endpointID string, cause error) bool {
	fields := log.Fields{"endpoint": shortID(endpointID), "iface": iface}

	switch classifyV6Absence(ra) {
	case v6NotOffered:
		p.dhcpv6NotOffered.Add(1)
		log.WithFields(fields).
			Info("Segment advertises no managed DHCPv6; creating the endpoint without a DHCPv6 address")
		return true
	case v6NoRouter:
		p.dhcpv6NoRouterAdvert.Add(1)
		log.WithFields(fields).WithError(cause).
			Warn("No IPv6 router advertisement on this segment; creating the endpoint without a DHCPv6 address")
		return true
	default:
		return false
	}
}
