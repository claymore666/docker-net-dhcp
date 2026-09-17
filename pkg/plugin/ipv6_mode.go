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

// Two options say where an endpoint's IPv6 address comes from, and the
// answer is a function of the pair (#817).
//
// ONE DERIVATION, READ ON BOTH PATHS. The pair is resolved here and
// nowhere else, because the two paths that need the answer do not see
// the same inputs: CreateNetwork sees what the operator typed, and
// every endpoint handler sees a stored record, including records served
// by the NetworkInspect fallback for networks CreateNetwork never
// validated. A rule applied at create and a second rule applied at read
// is the shape that produces two answers to one question -- and the
// looser of the two decides, which here would be "this network has no
// IPv6" for a network whose whole configuration is `ipv6_mode=slaac`.

// ipv6Mode is where this network's endpoints get their IPv6 address,
// resolved from the stored option pair.
//
// The rules, and each one's reason:
//
//   - `ipv6_mode` unset: `ipv6=true` is `dhcp` and `ipv6=false` is off.
//     That is the meaning `ipv6` has always had, kept exactly, because
//     changing it would change every existing network's behaviour on
//     upgrade.
//   - `ipv6_mode` set to `dhcp`, `slaac` or `auto`: that mode, and IPv6
//     is on. The option implies it, so a network says one thing rather
//     than two that have to agree.
//   - `ipv6_mode=off` beside `ipv6=true`: REFUSED. The two say opposite
//     things about the same endpoint and there is no reading of the
//     pair that is not a guess about which one the operator meant.
//
// WHAT IS NOT HERE, and why: `ipv6=false` written out beside a non-off
// mode is refused too, and it is refused at CreateNetwork alone. Both
// spellings decode to the same stored record -- `IPv6: false` with a
// mode -- so this function cannot tell the contradiction from the
// documented `ipv6_mode=slaac` on its own. validateIPv6Options is
// handed the set of fields the operator actually wrote and refuses it
// there, once, at the only point where the difference exists.
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

// ipv6Enabled reports whether endpoints on this network get an IPv6
// address from the plugin at all.
//
// A REFUSED PAIR READS AS OFF HERE, and that is safe rather than
// sloppy: the pair is refused at CreateNetwork and again in
// checkStoredOptions, which every endpoint handler goes through before
// anything reads this, so the only caller that can reach a refused pair
// is one that has already failed. Answering "on" for a configuration
// nothing could act on would start a DHCPv6 client for a mode
// buildParams6 refuses.
func (o DHCPNetworkOptions) ipv6Enabled() bool {
	m, err := o.ipv6Mode()
	return err == nil && m != proto.Mode6Off
}

// validateIPv6Options refuses an IPv6 configuration this plugin will
// not act on as written, at `docker network create`.
//
// set is the field names the operator's options actually carried, from
// decodeOptsSet. It is needed for exactly one refusal and the comment
// on ipv6Mode says why.
func validateIPv6Options(opts DHCPNetworkOptions, set map[string]bool) error {
	mode, err := opts.ipv6Mode()
	if err != nil {
		return err
	}

	// The written-out contradiction. `ipv6=false` is a value an
	// operator types, not just a zero the decoder left behind, and
	// beside a mode that switches IPv6 on it is the same kind of
	// mistake as the pair ipv6Mode refuses -- caught here because this
	// is the only place the two spellings are still distinguishable.
	if mode != proto.Mode6Off && set["IPv6"] && !opts.IPv6 {
		return fmt.Errorf("%w: ipv6=false and ipv6_mode=%s contradict each other: "+
			"ipv6_mode switches IPv6 on for every endpoint on this network. "+
			"Set ipv6_mode=off, or drop ipv6 and let ipv6_mode=%s speak for itself",
			util.ErrIPAM, mode, mode)
	}

	// SLAAC ON ipvlan IS REFUSED, and it is the same fact #895 refused
	// the MAC-derived DUID for. An ipvlan L2 slave inherits the parent
	// link's hardware address, RFC 4291 Appendix A forms the interface
	// identifier from that address, and RFC 4862 gives a node with a
	// fixed identifier no retry after duplicate address detection
	// fails -- so every container on such a network would form ONE
	// address, and the second one onwards would sit in a conflict it
	// cannot recover from. DHCPv6 has no such problem here because the
	// identity is a per-endpoint DUID-UUID, which is why `dhcp` is
	// allowed and these two are not.
	if dhcp.IPv6ModeFormsAddresses(mode) && opts.effectiveMode() == ModeIPvlan {
		return fmt.Errorf("%w: ipv6_mode=%s is not supported in mode=ipvlan: "+
			"ipvlan slaves share the parent link's MAC address, an address formed from a "+
			"router advertisement is derived from that MAC (RFC 4291 appendix A), and every "+
			"container on this network would form the same IPv6 address. "+
			"Use ipv6_mode=dhcp on ipvlan, which gives each endpoint its own DUID. See issue #817",
			util.ErrModeMismatch, mode)
	}

	// `ipv6_main_prefix` ON A MODE THAT DOES NOT FORM ADDRESSES IS
	// REFUSED. The option picks which of several formed addresses
	// Docker is told about; a DHCPv6 lease holds the one address the
	// server granted, so on `dhcp` the option can only ever do nothing
	// and an operator who set it is asking for something this network
	// cannot give. Refused rather than ignored on the rule the mode
	// contradictions are refused by: a setting that reads as honoured
	// at every layer and changes nothing is the silent half.
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

// ipv6MainPrefix parses the `ipv6_main_prefix` option.
//
// AN UNSET OPTION IS THE ZERO PREFIX AND NOT AN ERROR, which is what
// every network that never set it gets and is what infoFromLease reads
// as "the first prefix the router advertised".
//
// A VALUE THAT IS NOT THE NETWORK ADDRESS OF ITS OWN PREFIX IS REFUSED.
// netip.ParsePrefix accepts `2001:db8::5/64`, and netip.Prefix.Contains
// masks the prefix before comparing, so such a value would work
// perfectly and read to anyone looking at `docker network inspect` as
// naming one address rather than a prefix. Refusing it is a guard in
// one direction and the other direction is named: it refuses nothing an
// operator can mean, because the only reading of the host bits is the
// one Contains discards.
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

// v6Wiring fills in everything a DHCPv6 client needs and a DHCPv4
// client has no counterpart for: the identity it speaks as, the record
// it writes to, the address it would like, and the mode it runs in.
//
// IT IS ONE HELPER BECAUSE THE MODE HAS NO USABLE ZERO AND THE IDENTITY
// DOES NOT EITHER. proto.Mode6's zero is Mode6DHCP, so a call site that
// set Identity6 and forgot the mode would get a perfectly healthy
// client running the behaviour that shipped before `ipv6_mode` existed
// -- on every network, silently, with the option stored and the
// reference documenting something else. dhcp.buildParams6 refuses an
// empty Identity6 loudly, so travelling together is what makes the
// silent half impossible: a site that skips this helper does not reach
// the wrong mode, it fails to start a client at all.
//
// The attach path exists twice (network.go, parent_attached.go) and the
// persistent client is a third site; ipv6_mode_sites_test.go is the
// population, on v6_absence_sites_test.go's rule.
func (p *Plugin) v6Wiring(base *dhcp.DHCPClientOptions, opts DHCPNetworkOptions, id6 dhcp.Identity6, recordID6, preferredV6, endpointID string) error {
	mode, err := opts.ipv6Mode()
	if err != nil {
		return err
	}
	if mode == proto.Mode6Off {
		// Reachable only from a caller that decided to start a DHCPv6
		// client for a network that has none, which is the two-answers
		// shape ipv6Mode exists to prevent. Refused rather than
		// defaulted, because defaulting here is how the two answers
		// would stop disagreeing without either becoming right.
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
	// The fields above reach the wire even with no plugin behind them,
	// and the callback does not, on conflictWiring's rule: a manager
	// built for a unit test carries a nil plugin, and a counter has
	// nowhere to go, but a client that ran in the wrong mode would be
	// testing a configuration no call site can produce.
	if p == nil {
		return nil
	}
	if dhcp.IPv6ModeFormsAddresses(mode) {
		// IN BOTH FORMING MODES, and not only in auto: `slaac` sees
		// the same refused prefixes and has the same question about
		// them. See OnV6PrefixesIgnored for why it is NOT set in
		// `dhcp`, where the same library counter counts every
		// autonomous prefix on every advertisement.
		base.OnV6PrefixesIgnored = p.v6PrefixesIgnoredReporter(endpointID)
	}
	// IN EVERY MODE, which is what makes it unlike the two reporters
	// beside it. Router discovery is not a mode's business: a `dhcp`
	// segment's client solicits and listens for the M and O flags
	// exactly as a `slaac` one does, and what these counters describe is
	// the segment and not what the plugin decided to do with it (#814).
	//
	// HERE AND NOT IN conflictWiring, because this is where the three
	// v6 client sites meet -- the two attach paths and the persistent
	// client -- and ipv6_mode_sites_test.go is already the population
	// that says so. conflictWiring runs on the v4 paths as well, and a
	// v4 client has no Neighbor Discovery socket and nothing to count.
	base.OnRouterStats = p.addRouterStats
	if mode == proto.Mode6Auto {
		// ONLY IN auto, because only auto can fall back. The library
		// raises lease.Stats.SLAACFallbacks from the timer
		// Mode6Auto arms when a router says M=1 (proto/machine6_slaac.go,
		// autoFallbackFired), and no other mode arms it -- so a
		// callback set in `dhcp` or `slaac` could not fire, and the
		// sentence it would log names a mode the network is not in.
		base.OnV6Fallback = p.v6FallbackReporter(endpointID)
	}
	return nil
}

// v6FallbackReporter builds the callback that turns the library's count
// of SLAAC fallbacks into this plugin's counter and the operator's log
// line.
//
// IT COUNTS EFFECT AND NOT INTENT, because the library's counter does:
// its own contract is a fallback that FORMED an address, and a fallback
// deadline that passed with no usable prefix ends the acquisition
// instead of raising it. So a non-zero counter here means containers
// really are running on advertised prefixes, which is the fact an
// operator who set `ipv6_mode=auto` on a segment they believed was
// managed needs to see.
//
// LOUD, at warning, on "loud has two settings": nothing is degraded and
// the endpoint has an address, but the address came from a different
// place than the network's configuration nominally asked for, and a
// silent switch between two address sources is the thing #817's option
// exists to make visible. `-o ipv6_auto_strict=true` is the setting
// that fails the endpoint instead.
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

// v6PrefixesIgnoredReporter turns the library's count of advertised
// prefixes that formed no address into this plugin's counter.
//
// AT INFO AND NOT AT WARNING, which is the opposite choice from the
// fallback reporter beside it, and the difference is what an operator
// has to do. A refused prefix is the ordinary state of a link that
// advertises one autonomous prefix and one that is not: nothing is
// degraded and no configuration is being second-guessed. What makes
// the number worth having is the two readings it separates -- an
// endpoint holding this client's eight addresses on a link advertising
// nine, and an endpoint that formed nothing at all -- and both of those
// are read off the counter, not off a log line.
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
