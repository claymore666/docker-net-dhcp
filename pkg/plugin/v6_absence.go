// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// v6Verdict splits a failed DHCPv6 acquisition by what the segment advertised, since a timeout alone cannot (#868).
type v6Verdict int

const (
	// v6Fatal: the managed flag was advertised, so silence is a failure.
	v6Fatal v6Verdict = iota
	// v6NotOffered: an advertisement without the managed flag; the endpoint gets no v6 address and no v6 route (#868).
	// The RA guard writes autoconf=0, so the kernel forms no address either (RFC 4862 section 5.5.3, #821). The engine
	// disables IPv6 on a link with no global v6 address and the kernel then refuses every v6 route with EACCES, failing
	// the whole sandbox (#821, measured on the lane 2026-09-16).
	v6NotOffered
	// v6NoRouter: no advertisement arrived within the budget; tolerated, counted and warned about.
	v6NoRouter
	// v6Refused: a server answered with a non-Success Status Code (RFC 9915 section 21.13); fatal, counted apart
	// (#816).
	v6Refused
	// v6SLAACNoPrefix: a router was heard but advertised no prefix to form from (RFC 4862 section 5.5.3, #818).
	v6SLAACNoPrefix
	// v6SLAACNoAddress: an address-forming mode heard a router and formed no address in the budget; DAD found it in use
	// (RFC 4862 section 5.4.5) or the advertisement arrived too late (#818).
	v6SLAACNoAddress
	// v6VerdictCount ends the enumeration so TestAllV6Verdicts_IsEveryDeclaredVerdict catches a short allV6Verdicts.
	v6VerdictCount
)

func allV6Verdicts() []v6Verdict {
	return []v6Verdict{
		v6Fatal, v6NotOffered, v6NoRouter, v6Refused, v6SLAACNoPrefix, v6SLAACNoAddress,
	}
}

// v6AbsenceTolerated says whether an endpoint with no DHCPv6 address may still be created (#868, #818).
// No advertisement fails an address-forming mode: RFC 4862 section 5.5.3 forms from the Prefix Information option.
// v6NotOffered stays tolerated in every mode: it is RFC 9915 section 18.2.6's Information-request answered.
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

// classifyV6Absence maps what the acquisition saw of the router and on the wire to one verdict (#868).
// The wire beats the flags: dhcp.ErrNoV6Address is an Information-request Reply (RFC 9915 section 18.2.6), which
// the library sends only on an M=0 O=1 link. No router seen is v6NoRouter (RFC 4861 section 6.3.4), M=1 is fatal.
func classifyV6Absence(ra dhcp.RAObservation, cause error, mode proto.Mode6) v6Verdict {
	if errors.Is(cause, dhcp.ErrNoV6Address) {
		return v6NotOffered
	}
	if _, refused := dhcp.V6RefusalStatus(cause); refused {
		// On a stateless segment dnsmasq answers the Solicit with NoAddrsAvail while advertising M=0 O=1, so a refusal
		// is a fault only where M=1 promised addresses or no advertisement was seen (#821, measured on the lane).
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
		// Mode6SLAAC never solicits, so a silent DHCPv6 server is not this endpoint's failure (#818).
		return v6SLAACNoAddress
	case ra.Managed:
		return v6Fatal
	case mode == proto.Mode6Auto:
		// Mode6Auto decides once on the first advertisement; without M it formed an address, so this is the slaac row.
		return v6SLAACNoAddress
	default:
		return v6NotOffered
	}
}

func (p *Plugin) noteV6Absence(ra dhcp.RAObservation, iface, endpointID string, cause error, mode proto.Mode6) bool {
	fields := log.Fields{"endpoint": shortID(endpointID), "iface": iface}

	verdict := classifyV6Absence(ra, cause, mode)
	switch verdict {
	case v6Refused:
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
		p.dhcpv6NoRouterAdvert.Add(1)
		entry := log.WithFields(fields).WithField("ipv6_mode", mode.String()).WithError(cause)
		if dhcp.IPv6ModeFormsAddresses(mode) {
			entry.Error("No IPv6 router advertisement on this segment; this network's ipv6_mode " +
				"takes its addresses from the advertisement, so nothing else can provide one")
			break
		}
		entry.Warn("No IPv6 router advertisement on this segment; creating the endpoint without a DHCPv6 address")
	default:
		// The message is the one that shipped before #868, kept word for word.
		p.dhcpv6NoServer.Add(1)
	}
	return v6AbsenceTolerated(verdict, mode)
}
