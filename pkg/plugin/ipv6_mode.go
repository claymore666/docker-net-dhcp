// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net/netip"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The option pair is resolved here only, for CreateNetwork and for every stored record alike (#817).

// ipv6Mode resolves the stored pair: unset ipv6_mode keeps ipv6's old meaning, a set mode implies IPv6, and
// ipv6_mode=off beside ipv6=true is refused. An explicit ipv6=false beside a mode is refused at create only,
// since the stored record cannot tell it from an absent key (#817).
func (o DHCPNetworkOptions) ipv6Mode() (proto.Mode6, error) {
	m, set, err := dhcp.ParseIPv6Mode(o.IPv6Mode)
	if err != nil {
		return proto.Mode6Off, fmt.Errorf("%w: %v", util.ErrIPAM, err)
	}
	if !set {
		if o.IPv6 {
			return proto.Mode6DHCP, nil
		}
		return proto.Mode6Off, nil
	}
	if m == proto.Mode6Off {
		if o.IPv6 {
			return proto.Mode6Off, fmt.Errorf("%w: ipv6=true and ipv6_mode=off contradict each other: "+
				"one switches DHCPv6 on for every endpoint on this network and the other says the "+
				"network has no IPv6 at all. Set ipv6_mode to dhcp, slaac or auto, or drop ipv6",
				util.ErrIPAM)
		}
		return proto.Mode6Off, nil
	}
	return m, nil
}

// ipv6Enabled reads a refused pair as off: every endpoint handler has already refused it in checkStoredOptions (#817).
func (o DHCPNetworkOptions) ipv6Enabled() bool {
	m, err := o.ipv6Mode()
	return err == nil && m != proto.Mode6Off
}

func validateIPv6Options(opts DHCPNetworkOptions, set map[string]bool) error {
	mode, err := opts.ipv6Mode()
	if err != nil {
		return err
	}

	if mode != proto.Mode6Off && set["IPv6"] && !opts.IPv6 {
		return fmt.Errorf("%w: ipv6=false and ipv6_mode=%s contradict each other: "+
			"ipv6_mode switches IPv6 on for every endpoint on this network. "+
			"Set ipv6_mode=off, or drop ipv6 and let ipv6_mode=%s speak for itself",
			util.ErrIPAM, mode, mode)
	}

	// slaac and auto are refused on ipvlan: an L2 slave inherits the parent's MAC, RFC 4291 Appendix A forms the
	// interface identifier from it, and RFC 4862 gives no retry after DAD fails. dhcp is allowed because its
	// identity is a per-endpoint DUID-UUID (#895).
	if dhcp.IPv6ModeFormsAddresses(mode) && opts.effectiveMode() == ModeIPvlan {
		return fmt.Errorf("%w: ipv6_mode=%s is not supported in mode=ipvlan: "+
			"ipvlan slaves share the parent link's MAC address, an address formed from a "+
			"router advertisement is derived from that MAC (RFC 4291 appendix A), and every "+
			"container on this network would form the same IPv6 address. "+
			"Use ipv6_mode=dhcp on ipvlan, which gives each endpoint its own DUID. See issue #817",
			util.ErrModeMismatch, mode)
	}

	// ipv6_main_prefix is refused on a mode that forms no addresses, where it could only do nothing (#818).
	if opts.IPv6MainPrefix != "" && !dhcp.IPv6ModeFormsAddresses(mode) {
		return fmt.Errorf("%w: ipv6_main_prefix needs an ipv6_mode that forms addresses from a "+
			"router advertisement, and this network is ipv6_mode=%s: it names which of several "+
			"advertised prefixes Docker reports as the endpoint's address, and a DHCPv6 lease "+
			"carries the one address the server granted. Use ipv6_mode=slaac or ipv6_mode=auto, "+
			"or drop ipv6_main_prefix. See issue #818", util.ErrIPAM, mode)
	}
	if _, err := opts.ipv6MainPrefix(); err != nil {
		return err
	}

	return nil
}

// ipv6MainPrefix: unset is the zero prefix. A value with host bits is refused, since ParsePrefix accepts it and
// Prefix.Contains masks them (#818).
func (o DHCPNetworkOptions) ipv6MainPrefix() (netip.Prefix, error) {
	if o.IPv6MainPrefix == "" {
		return netip.Prefix{}, nil
	}
	p, err := netip.ParsePrefix(o.IPv6MainPrefix)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: ipv6_main_prefix %q is not a prefix in CIDR form "+
			"(want something like 2001:db8:1::/64): %v", util.ErrIPAM, o.IPv6MainPrefix, err)
	}
	if !p.Addr().Is6() || p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%w: ipv6_main_prefix %q is not an IPv6 prefix",
			util.ErrIPAM, o.IPv6MainPrefix)
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("%w: ipv6_main_prefix %q has bits set below its prefix "+
			"length: it names a prefix and not an address, so write %s",
			util.ErrIPAM, o.IPv6MainPrefix, p.Masked())
	}
	return p, nil
}

// v6Wiring sets identity, record and mode together: proto.Mode6's zero is Mode6DHCP, and buildParams6 refuses
// an empty Identity6, so a site that skips it fails to start instead of running the wrong mode (#817).
func (p *Plugin) v6Wiring(base *dhcp.DHCPClientOptions, opts DHCPNetworkOptions, id6 dhcp.Identity6, recordID6, preferredV6, endpointID string) error {
	mode, err := opts.ipv6Mode()
	if err != nil {
		return err
	}
	if mode == proto.Mode6Off {
		return fmt.Errorf("%w: a DHCPv6 client was requested for a network whose ipv6_mode is off",
			util.ErrIPAM)
	}
	base.Identity6 = id6
	base.RecordID = recordID6
	base.PreferredV6 = preferredV6
	base.Mode6 = mode
	base.StrictAuto6 = opts.IPv6AutoStrict
	main, err := opts.ipv6MainPrefix()
	if err != nil {
		return err
	}
	base.MainPrefix6 = main
	if p == nil {
		return nil
	}
	if dhcp.IPv6ModeFormsAddresses(mode) {
		base.OnV6PrefixesIgnored = p.v6PrefixesIgnoredReporter(endpointID)
	}
	// Router-discovery counters are set in every mode: they describe the segment, not the mode (#814).
	base.OnRouterStats = p.addRouterStats
	if mode == proto.Mode6Auto {
		// Only auto: the library raises SLAACFallbacks from the timer Mode6Auto arms on M=1 (proto/machine6_slaac.go)
		// (#817).
		base.OnV6Fallback = p.v6FallbackReporter(endpointID)
	}
	return nil
}

// v6FallbackReporter counts fallbacks that formed an address and warns, since the address source differs from the
// configured one (#817).
func (p *Plugin) v6FallbackReporter(endpointID string) func(uint64) {
	return func(n uint64) {
		if n == 0 {
			return
		}
		p.dhcpv6AutoFallbacks.Add(int32(n))
		log.WithFields(log.Fields{
			"endpoint":  shortID(endpointID),
			"fallbacks": n,
		}).Warn("ipv6_mode=auto fell back to the router's advertised prefix: the segment advertised DHCPv6 and no server answered inside the fallback window. " +
			"Set ipv6_auto_strict=true to fail the endpoint instead")
	}
}

// v6PrefixesIgnoredReporter logs at info: a refused prefix is the ordinary state of a mixed link (#818).
func (p *Plugin) v6PrefixesIgnoredReporter(endpointID string) func(uint64) {
	return func(n uint64) {
		if n == 0 {
			return
		}
		p.ipv6SLAACPrefixesIgnored.Add(int32(n))
		log.WithFields(log.Fields{
			"endpoint": shortID(endpointID),
			"prefixes": n,
		}).Info("Advertised prefixes this endpoint formed no address from (RFC 4862 section 5.5.3, " +
			"or this client's cap of eight addresses per endpoint)")
	}
}
