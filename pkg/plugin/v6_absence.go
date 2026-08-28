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
	// a degraded one, and the endpoint is created without a v6 address.
	//
	// The endpoint then starts with no global IPv6 address FROM THIS
	// PLUGIN -- there is no lease to be had -- and this comment must be
	// precise about which of the two statements it is making, because
	// the stronger one used to be true here and is not any more.
	//
	// It used to read: SLAAC does NOT step in, because dhcpcd writes
	// net.ipv6.conf.<if>.autoconf=0 and accept_ra=0 on the interface it
	// manages (if-linux.c, if_setup_inet6, dhcpcd 10.3.2) and
	// --noconfigure does not gate that write. Both halves of that are
	// still true OF DHCPCD, and they are no longer true of the
	// interface: the RA guard (#875, pkg/dhcp/ra_guard.go) sets
	// accept_ra=2 and autoconf=1 before dhcpcd starts and then makes
	// /proc/sys read-only in the client's mount namespace so dhcpcd's
	// write is refused. The kernel is therefore free to autoconfigure,
	// and whether it does is decided by the A flag on the advertised
	// prefix (RFC 4862 section 5.5.3), not by this plugin.
	//
	// What is unchanged: under --noconfigure -- which this plugin
	// always passes -- DHCPCD does not apply the advertisement itself.
	// The kernel doing it and dhcpcd doing it are different actors, and
	// only the second is still suppressed. Any address that forms is
	// the kernel's, is not a lease, and is not reported in
	// docker inspect.
	//
	// What the container gets regardless is IPv4 from DHCP, an IPv6
	// link-local, and the stateless DHCPv6 configuration (#815) where
	// the segment offers it. docs/reference.md's DHCPv6 section is the
	// reference and says the same thing.
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
