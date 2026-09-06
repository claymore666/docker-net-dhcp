// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"

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
	// PLUGIN -- there is no lease to be had -- and the distinction in
	// that sentence is the whole of it: the KERNEL may well form one.
	// The RA guard (#875, pkg/dhcp/ra_guard.go) leaves the interface at
	// accept_ra=2 and autoconf=1, so whether an address forms is
	// decided by the A flag on the advertised prefix (RFC 4862 section
	// 5.5.3) and not by this plugin. Any address that does form is the
	// kernel's, is not a lease, and is not reported in docker inspect.
	//
	// (2.0 removed the other half of this note along with dhcpcd. In
	// 1.x the client wrote accept_ra=0 and autoconf=0 on every carrier
	// acquisition and the guard had to shield the sysctls from it; this
	// build execs nothing, so the writes stand on their own -- D30 Q3.)
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
// segment's router advertisements, and what came back on the wire, into
// the verdict for a DHCPv6 acquisition that produced no address.
//
// Pure and total: every (observation, cause) pair maps to exactly one
// verdict.
//
// THE WIRE BEATS THE DIAGNOSTIC, which is why cause is an argument and
// not a thing the caller handles separately. dhcp.ErrNoV6Address means
// the server ANSWERED -- RFC 9915 section 18.2.6's Information-request
// Reply arrived, carrying configuration and no address -- and the
// library only ever sends that exchange on a link whose advertisement
// said M=0 O=1. So it is a stronger statement than any reading of the
// router observation, including a reading taken from an advertisement
// that arrived after the Reply and said something else.
//
// The observation decides the rest:
//
//   - nothing seen: no router answered inside the budget, which no
//     mechanism can work around (RFC 4861 section 6.3.4 has no other
//     source of a default route).
//   - M=1: the segment says addresses are available over DHCPv6 and
//     none arrived. That is the failure this plugin has always
//     reported and it stays fatal.
//   - anything else -- O=1 alone, or neither bit -- is a segment with
//     no DHCPv6 addresses on it, which is a configuration and not a
//     fault.
func classifyV6Absence(ra dhcp.RAObservation, cause error) v6Verdict {
	if errors.Is(cause, dhcp.ErrNoV6Address) {
		return v6NotOffered
	}
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

	switch classifyV6Absence(ra, cause) {
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
