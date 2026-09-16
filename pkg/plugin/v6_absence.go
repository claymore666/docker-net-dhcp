// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
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
	// PLUGIN -- there is no lease to be had -- and none from the kernel
	// either. Since #821 the RA guard (pkg/dhcp/ra_guard.go) writes
	// autoconf=0 on the interface, so the kernel forms no address from
	// the advertised prefix whatever its A flag says (RFC 4862 section
	// 5.5.3): the plugin holds the lease for the address a container
	// uses, and two sources of global address on one link is not a
	// state anything downstream is written for.
	//
	// A NETWORK WHOSE ipv6_mode FORMS ADDRESSES DOES NOT REACH THIS
	// VERDICT on a seen advertisement. There the plugin forms the
	// address itself and installs it (#818), and the two endings for
	// that mode are v6SLAACNoPrefix and v6SLAACNoAddress below.
	//
	// THE ENDPOINT THEREFORE GETS NO IPv6 ROUTE either, and that is a
	// fact about the engine rather than a choice (#821, MEASURED on the
	// lane 2026-09-16, run 35131643324). The engine disables IPv6 on a
	// container link that carries no global IPv6 address, and the
	// kernel then refuses every IPv6 route on such a link: a Join
	// answer carrying the advertisement's gateway or routes fails the
	// whole sandbox with `error setting interface routes to
	// ["fd00:...::/64"]: permission denied`, and no container starts on
	// the segment at all -- losing its IPv4 with it. The plugin cannot
	// clear disable_ipv6 ahead of the engine either: that clear runs in
	// the manager goroutine Join spawns, after the engine has moved the
	// link and applied the answer.
	//
	// So before #821 the container's own kernel gave it a default route
	// here and now nothing does. #818 gives the container a global IPv6
	// address, and the route becomes both installable and useful in the
	// same change; #821 and #818 merge together for that reason.
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
	// v6Refused: a DHCPv6 server ANSWERED this client and turned it
	// down -- RFC 9915 section 21.13's Status Code option carrying
	// something other than Success, "NoAddrsAvail" being the one an
	// exhausted pool produces (#816).
	//
	// FATAL, like v6Fatal, AND COUNTED APART FROM IT. That is the whole
	// of #816: both endings look like "no DHCPv6 address arrived", and
	// what an operator has to do about them has nothing in common. A
	// server that refuses is reachable, configured and out of addresses
	// for this client; a server that says nothing is unreachable or
	// gone. One counter for both meant the difference could not be seen
	// on /metrics at all.
	v6Refused
	// v6SLAACNoPrefix: a router WAS heard, and in a mode that forms its
	// own address it advertised no prefix this client could form one
	// from -- no Prefix Information option with the Autonomous flag, or
	// only ones RFC 4862 section 5.5.3 refuses.
	//
	// FATAL. `ipv6_mode=slaac` says the advertisement is where this
	// network's addresses come from, so an advertisement that carries
	// none is the failure of the only mechanism configured. It is its
	// own verdict rather than v6Fatal's because the thing to go and fix
	// is the router's prefix configuration and not a DHCPv6 server.
	//
	// It cannot arise on an `ipv6_mode=dhcp` network: the library
	// reports this reason only while awaitingAddress, which is false in
	// every mode that does not form addresses
	// (proto/machine6_slaac.go:28).
	v6SLAACNoPrefix
	// v6SLAACNoAddress: a router WAS heard, this network's ipv6_mode
	// forms the address from the advertisement, and no address was
	// formed inside the acquisition budget -- without the library
	// naming a reason, which is what separates this from
	// v6SLAACNoPrefix.
	//
	// FATAL, and it is the row that did not exist while `slaac` could
	// not give a container an address at all. Two things produce it and
	// both are worth being able to see: duplicate address detection
	// found the formed address in use (RFC 4862 section 5.4.5, and a
	// modified EUI-64 identifier gets no retry), or the advertisement
	// arrived too late in the budget for detection to finish. The
	// router's prefix configuration is not what to look at; the other
	// node holding that address is.
	//
	// IT IS ALSO THE VERDICT THAT KEEPS `slaac` OFF v6Fatal's MESSAGE.
	// proto.Mode6SLAAC "sends no Solicit ever, whatever the M flag
	// says", so on a managed segment the pre-#818 answer for a
	// `slaac` endpoint with no address was "no DHCPv6 server answered
	// within N s" -- about an exchange that never happened.
	v6SLAACNoAddress
	// v6VerdictCount is not a verdict. It is the end of the
	// enumeration, so allV6Verdicts cannot silently go short: a verdict
	// added above this line and not added to that slice fails
	// TestAllV6Verdicts_IsEveryDeclaredVerdict, and the tolerance table
	// that iterates the slice is a table over the population rather
	// than over whichever verdicts somebody remembered.
	v6VerdictCount
)

// allV6Verdicts is every verdict, in declaration order.
func allV6Verdicts() []v6Verdict {
	return []v6Verdict{
		v6Fatal, v6NotOffered, v6NoRouter, v6Refused, v6SLAACNoPrefix, v6SLAACNoAddress,
	}
}

// v6AbsenceTolerated says whether an endpoint with no DHCPv6 address
// may still be created, given the verdict and the network's ipv6_mode.
//
// IT IS A SECOND FUNCTION AND NOT A `return` INSIDE noteV6Absence's
// SWITCH, because the endpoint outcome and the counter are two claims
// and only one of them used to be testable without a Plugin. The
// switch below still owns the counter and the log line; this owns what
// Docker is told, and the table test drives it over every verdict and
// every mode.
//
// THE MODE TERM IS ON ONE ROW. A segment with no router advertisement
// at all gives a mode that forms its own address no source of one: RFC
// 4862 section 5.5.3 forms addresses from the Prefix Information
// option, and there is no advertisement to carry it. Up to v2.1.x that
// row was tolerated in every mode, and docs/reference.md said why --
// the address those modes would form was not installed yet, so failing
// the endpoint would have refused a container for the absence of
// something the release did not deliver. It is delivered here (#818),
// so the reason is gone and the row goes with it.
//
// v6NotOffered stays tolerated in EVERY mode, including the two that
// form addresses, and that is deliberate rather than an oversight. It
// is reached only through dhcp.ErrNoV6Address, which is RFC 9915
// section 18.2.6's Information-request answered with configuration and
// no address -- an exchange the library performs on an O=1 M=0 link,
// having already decided what to do about addresses. The endpoint has
// the configuration it asked for.
func v6AbsenceTolerated(v v6Verdict, mode proto.Mode6) bool {
	switch v {
	case v6NotOffered:
		return true
	case v6NoRouter:
		return !dhcp.IPv6ModeFormsAddresses(mode)
	default:
		return false
	}
}

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
func classifyV6Absence(ra dhcp.RAObservation, cause error, mode proto.Mode6) v6Verdict {
	if errors.Is(cause, dhcp.ErrNoV6Address) {
		return v6NotOffered
	}
	// The two verdicts the WIRE settles, on the same rule and before
	// the observation is read at all. A Status Code came from a server
	// that answered and a refused prefix came from a router that
	// advertised, so each is a stronger statement than any reading of
	// the flags -- and each names a different thing to go and fix.
	if _, refused := dhcp.V6RefusalStatus(cause); refused {
		// A REFUSAL IS A FAULT ONLY WHERE AN ADDRESS WAS PROMISED.
		//
		// MEASURED (#821, run 35141032546, shard main-4): on a
		// stateless segment dnsmasq answers the Solicit with Status
		// Code 2, NoAddrsAvail, four times inside the budget, and the
		// advertisement on that same segment carries O=1 and M=0. The
		// server is agreeing with its own advertisement -- "this
		// segment hands out no DHCPv6 addresses" -- said twice, once
		// in the flags and once on the wire. Reading the second
		// statement as a refusal made CreateEndpoint fail with
		// "failed to get initial IPv6 address via DHCPv6", so no
		// container started on a correctly configured stateless
		// network and it lost its IPv4 with it.
		//
		// That is the rule below this switch, applied one branch too
		// late: "anything else -- O=1 alone, or neither bit -- is a
		// segment with no DHCPv6 addresses on it, which is a
		// configuration and not a fault". The wire IS the stronger
		// statement, and where it AGREES with an M=0 advertisement
		// what the two agree on is that no address is coming.
		//
		// M=1 keeps the old verdict, which is the case the refusal
		// was written for: the segment promised addresses over
		// DHCPv6, a server answered, and it had none. An unseen
		// advertisement keeps it too -- with nothing on the wire
		// saying otherwise, a server that answers and refuses is the
		// only evidence there is, and it is evidence of a fault.
		if ra.Seen && !ra.Managed {
			return v6NotOffered
		}
		return v6Refused
	}
	if errors.Is(cause, dhcp.ErrNoSLAACPrefix) {
		return v6SLAACNoPrefix
	}
	switch {
	case !ra.Seen:
		return v6NoRouter
	case mode == proto.Mode6SLAAC:
		// `slaac` NEVER SOLICITS, so no reading of the M flag can make
		// a silent DHCPv6 server this endpoint's problem: there was no
		// exchange. A router was heard and the one mechanism this
		// network is configured for produced nothing.
		return v6SLAACNoAddress
	case ra.Managed:
		return v6Fatal
	case mode == proto.Mode6Auto:
		// `auto` on an advertisement without M resolved to forming an
		// address (proto.Mode6Auto decides once, on the first
		// advertisement), so this is the slaac row with the mode
		// spelled differently. It is NOT v6NotOffered: that verdict
		// tolerates the endpoint, and an `auto` network that was told
		// to form an address and did not has lost its only mechanism.
		return v6SLAACNoAddress
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
func (p *Plugin) noteV6Absence(ra dhcp.RAObservation, iface, endpointID string, cause error, mode proto.Mode6) bool {
	fields := log.Fields{"endpoint": shortID(endpointID), "iface": iface}

	verdict := classifyV6Absence(ra, cause, mode)
	switch verdict {
	case v6Refused:
		// COUNTED AND LOGGED ON THE WAY TO FAILING. The endpoint still
		// fails -- the caller turns this false into the error Docker
		// shows -- and the counter and the code are what separate this
		// ending from a silent one afterwards, on /metrics and in the
		// log. The code's name is printed and nothing branches on it:
		// what a status code means is the library's to decide.
		p.dhcpv6Refused.Add(1)
		status, _ := dhcp.V6RefusalStatus(cause)
		log.WithFields(fields).WithField("status_code", status).WithError(cause).
			Error("The DHCPv6 server refused this client; it answered and has no address for it")
	case v6SLAACNoPrefix:
		p.dhcpv6SLAACNoPrefix.Add(1)
		log.WithFields(fields).WithError(cause).
			Error("A router advertises on this segment and none of its prefixes formed an address; " +
				"this network's ipv6_mode takes its addresses from the advertisement")
	case v6SLAACNoAddress:
		p.dhcpv6SLAACNoAddress.Add(1)
		log.WithFields(fields).WithField("ipv6_mode", mode.String()).WithError(cause).
			Error("A router advertises on this segment and no address formed from it inside the " +
				"acquisition budget; another node may hold the address this endpoint's prefix and " +
				"MAC address form, and no DHCPv6 exchange took place")
	case v6NotOffered:
		p.dhcpv6NotOffered.Add(1)
		log.WithFields(fields).
			Info("Segment advertises no managed DHCPv6; creating the endpoint without a DHCPv6 address")
	case v6NoRouter:
		// ONE COUNTER FOR BOTH ENDINGS, because the population it
		// counts is "endpoints that saw no router advertisement" and
		// that is the same population whichever mode the network is
		// in. What the mode changes is the endpoint's outcome and the
		// level the line is written at, and an operator reading a
		// WARN beside a started container and an ERROR beside a
		// refused one is reading the difference.
		p.dhcpv6NoRouterAdvert.Add(1)
		entry := log.WithFields(fields).WithField("ipv6_mode", mode.String()).WithError(cause)
		if dhcp.IPv6ModeFormsAddresses(mode) {
			entry.Error("No IPv6 router advertisement on this segment; this network's ipv6_mode " +
				"takes its addresses from the advertisement, so nothing else can provide one")
			break
		}
		entry.Warn("No IPv6 router advertisement on this segment; creating the endpoint without a DHCPv6 address")
	default:
		// v6Fatal: the segment advertised M=1 and nothing usable came
		// back inside the budget. The message stays the caller's, word
		// for word, because it is the one that shipped and the one
		// #868's tolerance is measured against; what is added here is
		// the counter that makes it a different row from a refusal.
		p.dhcpv6NoServer.Add(1)
	}
	return v6AbsenceTolerated(verdict, mode)
}
