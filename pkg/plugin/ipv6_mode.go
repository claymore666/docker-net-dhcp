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
		stable := ""
		if iid, _ := opts.ipv6IID(); iid == proto.IIDModeStablePrivacy {
			stable = " ipv6_iid=stable-privacy does not lift this: its inputs are the same on every slave, so the slaves " +
				"separate only through the duplicate-address counter, and which one gets which address depends on the " +
				"order they start in (RFC 7217 section 6)."
		}
		return fmt.Errorf("%w: ipv6_mode=%s is not supported in mode=ipvlan: "+
			"ipvlan slaves share the parent link's MAC address, an address formed from a "+
			"router advertisement is derived from that MAC (RFC 4291 appendix A), and every "+
			"container on this network would form the same IPv6 address.%s "+
			"Use ipv6_mode=dhcp on ipvlan, which gives each endpoint its own DUID. See issue #817",
			util.ErrModeMismatch, mode, stable)
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

	// ipv6_temporary rides in a Solicit, which off and slaac never send, so there it could only do nothing (#927).
	if opts.IPv6Temporary && mode != proto.Mode6DHCP && mode != proto.Mode6Auto {
		return fmt.Errorf("%w: ipv6_temporary needs an ipv6_mode that sends a DHCPv6 Solicit, and this "+
			"network is ipv6_mode=%s: the temporary address is requested with an IA_TA in the Solicit "+
			"and the Request (RFC 8415 section 21.5), and this mode sends neither. Use ipv6_mode=dhcp "+
			"or ipv6_mode=auto, or drop ipv6_temporary. See issue #927", util.ErrIPAM, mode)
	}

	if err := validateIPv6PD(opts, set, mode); err != nil {
		return err
	}

	// ipv6_iid=stable-privacy shapes the identifier of an address formed from a router advertisement, which off and
	// dhcp never form, so there it could only do nothing (#1032).
	iid, err := opts.ipv6IID()
	if err != nil {
		return err
	}
	if iid == proto.IIDModeStablePrivacy && !dhcp.IPv6ModeFormsAddresses(mode) {
		return fmt.Errorf("%w: ipv6_iid=stable-privacy needs an ipv6_mode that forms addresses from a "+
			"router advertisement, and this network is ipv6_mode=%s: it sets the interface identifier "+
			"of that address (RFC 7217), and a DHCPv6 lease carries the address the server granted. "+
			"Use ipv6_mode=slaac or ipv6_mode=auto, or drop ipv6_iid. See issue #1032", util.ErrIPAM, mode)
	}

	return nil
}

// validateIPv6PD refuses ipv6_pd where no Solicit carries it, and on ipvlan, which shares the parent's link (#214).
// An empty value never reaches set, so ipv6_pd=0 is the only way to be in set at zero.
func validateIPv6PD(opts DHCPNetworkOptions, set map[string]bool, mode proto.Mode6) error {
	if opts.IPv6PD == 0 && !set["IPv6PD"] {
		return nil
	}
	if opts.IPv6PD < 1 || opts.IPv6PD > 128 {
		return fmt.Errorf("%w: ipv6_pd=%d is not a prefix length; want 1 to 128, such as 64. See issue #214",
			util.ErrIPAM, opts.IPv6PD)
	}
	if mode != proto.Mode6DHCP && mode != proto.Mode6Auto {
		return fmt.Errorf("%w: ipv6_pd needs an ipv6_mode that sends a DHCPv6 Solicit, and this network is "+
			"ipv6_mode=%s: the prefix is requested with an IA_PD in the Solicit and the Request (RFC 8415 "+
			"section 21.21), and this mode sends neither. Use ipv6_mode=dhcp or ipv6_mode=auto, or drop "+
			"ipv6_pd. See issue #214", util.ErrIPAM, mode)
	}
	if opts.effectiveMode() == ModeIPvlan {
		return fmt.Errorf("%w: ipv6_pd is not supported in mode=ipvlan: the router that delegates the "+
			"prefix routes it to the endpoint's link-local address, and an ipvlan slave shares the "+
			"parent's MAC and link. Use mode=bridge or mode=macvlan. See issue #214", util.ErrModeMismatch)
	}
	return nil
}

// ipv6IID: unset is eui64; a value outside the library's set is refused, never run as eui64 (#1032).
func (o DHCPNetworkOptions) ipv6IID() (proto.IIDMode, error) {
	m, err := dhcp.ParseIPv6IID(o.IPv6IID)
	if err != nil {
		return proto.IIDModeEUI64, fmt.Errorf("%w: %v", util.ErrIPAM, err)
	}
	return m, nil
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
func (p *Plugin) v6Wiring(base *dhcp.DHCPClientOptions, opts DHCPNetworkOptions, id6 dhcp.Identity6, recordID6, preferredV6, endpointID, networkID string) error {
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
	if mode == proto.Mode6Auto && p != nil && p.v6AbsenceServedEndpoint(endpointID) {
		// A served endpoint runs slaac at both clients: soliciting in auto could bind a lease and move the address (#1038).
		base.Mode6 = proto.Mode6SLAAC
	}
	main, err := opts.ipv6MainPrefix()
	if err != nil {
		return err
	}
	base.MainPrefix6 = main
	// Every v6 client, the one-shot and the persistent one, passes here, so both ask for the same prefix (#214).
	base.IPv6PD = opts.IPv6PD
	// Every DHCPv6 client starts here, and only stable-privacy reads the secret, so an eui64 network never touches the
	// file (#1032).
	iid, err := opts.ipv6IID()
	if err != nil {
		return err
	}
	base.IPv6IID = iid
	if iid == proto.IIDModeStablePrivacy {
		secret, err := loadIIDSecret()
		if err != nil {
			return err
		}
		base.IPv6IIDSecret = secret
		base.IPv6IIDNetworkID = []byte(networkID)
	}
	if p == nil {
		return nil
	}
	if dhcp.IPv6ModeFormsAddresses(mode) {
		base.OnV6PrefixesIgnored = p.v6PrefixesIgnoredReporter(endpointID)
	}
	// Router-discovery counters are set in every mode: they describe the segment, not the mode (#814).
	base.OnRouterStats = p.addRouterStats
	// RFC 4704 section 5: option 39 rides stateful messages only, so a SLAAC address gets no AAAA (#1029).
	if opts.RegisterDNS && mode == proto.Mode6SLAAC {
		log.WithField("endpoint", shortID(endpointID)).
			Info("ipv6_mode=slaac sends no DHCPv6 Solicit, so register_dns registers this container's A record and no AAAA")
	}
	if base.Mode6 == proto.Mode6Auto {
		// Only auto: the library raises SLAACFallbacks from the timer Mode6Auto arms on M=1 (proto/machine6_slaac.go)
		// (#817).
		report := p.v6FallbackReporter(endpointID)
		if opts.RegisterDNS {
			report = noAAAAAfterFallback(endpointID, report)
		}
		base.OnV6Fallback = report
	}
	return nil
}

// noAAAAAfterFallback warns that a fallback, which precedes any DHCPv6 lease, leaves the name with no AAAA (#1029).
func noAAAAAfterFallback(endpointID string, report func(uint64)) func(uint64) {
	return func(n uint64) {
		report(n)
		if n != 0 {
			log.WithField("endpoint", shortID(endpointID)).
				Warn("register_dns: no AAAA record is registered for this container's name, since the address came " +
					"from the router's advertised prefix and not from a DHCPv6 lease")
		}
	}
}

// v6FallbackReporter counts an endpoint's first fallback and warns on every one, since the address source differs
// from the configured one (#817, #1016).
func (p *Plugin) v6FallbackReporter(endpointID string) func(uint64) {
	return func(n uint64) {
		if n == 0 {
			return
		}
		if _, seen := p.autoFallbackCounted.LoadOrStore(endpointID, struct{}{}); !seen {
			p.dhcpv6AutoFallbacks.Add(1)
		}
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
