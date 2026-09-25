// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	"github.com/claymore666/dhcp-golib/wire"
	log "github.com/sirupsen/logrus"
)

// maxRtrSolicitationDelay is RFC 4861 section 10's "MAX_RTR_SOLICITATION_DELAY 1 second", which Params6 does not carry.
const maxRtrSolicitationDelay = time.Second

// RFC 4861 section 6.3.7: delay plus MAX_RTR_SOLICITATIONS intervals, 1 + 3*4 = 13 s with the library's defaults
// (#911).

// RouterDiscoveryWindow is the shortest deadline under which "no router advertisement arrived" describes the segment.
func RouterDiscoveryWindow(p proto.Params6) time.Duration {
	n := p.RouterSolicitations
	if n <= 0 {
		n = proto.MaxRtrSolicitations
	}
	interval := time.Duration(p.RouterSolicitInterval)
	if interval <= 0 {
		interval = time.Duration(proto.RtrSolicitationInterval)
	}
	return maxRtrSolicitationDelay + time.Duration(n)*interval
}

// RFC 9915 section 7.6 gives Solicit "MRC 0" and "MRD 0", and the daemon abandons CreateEndpoint after 30 s
// (moby/pkg/plugins); four transmissions fit (#911).

// v6SolicitTransmissions is how many Solicits a one-shot DHCPv6 acquisition is funded for.
const v6SolicitTransmissions = 4

// lease_timeout's 34 s is DHCPv4's; measured on the lane 2026-09-06, a SLAAC segment ran to it and `docker run` failed
// after ~30 s (#868). RFC 4861 section 6.3.7 plus RFC 9915 section 15's Solicit schedule: 13.0 + 8.7 = 21.7 s (#911).

// V6AcquisitionWindow is how long a one-shot DHCPv6 acquisition may run before the chassis draws its verdict.
func V6AcquisitionWindow(p proto.Params6) time.Duration {
	return RouterDiscoveryWindow(p) + v6SolicitWindow(p)
}

// RFC 9915 section 18.2.1 delays the first Solicit up to SOL_MAX_DELAY; section 15's RT = 2*RTprev + RAND*2*RTprev is
// taken at RAND = +0.1 (#911).

// v6SolicitWindow is the Solicit half of V6AcquisitionWindow.
func v6SolicitWindow(p proto.Params6) time.Duration {
	d := proto.DefaultParams6()
	delay := time.Duration(p.SolMaxDelay)
	if delay <= 0 {
		delay = time.Duration(d.SolMaxDelay)
	}
	rt := time.Duration(p.SolTimeout)
	if rt <= 0 {
		rt = time.Duration(d.SolTimeout)
	}
	total := delay
	for i := 1; i < v6SolicitTransmissions; i++ {
		total += rt + rt/10
		rt *= 2
	}
	return total
}

// v6RouterPollInterval is how often getIP6 re-reads the router observation, as advertisements are not lease events
// (#911).
const v6RouterPollInterval = 250 * time.Millisecond

// proto.Machine6 sends no Solicit on an M=0 O=0 link, so the verdict is taken in about two seconds instead of 21.7
// (#911).

// ErrNoDHCPv6OnSegment is a segment whose advertisement carries neither the M nor the O flag (RFC 4861 section 4.2).
var ErrNoDHCPv6OnSegment = errors.New("dhcp: the segment's router advertisement offers no DHCPv6")

// advertisedNoDHCPv6 reports whether the segment has already said DHCPv6 has nothing for this client.
func advertisedNoDHCPv6(r RAObservation) bool {
	return r.Seen && !r.Managed && !r.Other
}

// In slaac and auto the M=0 O=0 advertisement carries the prefix to form from (RFC 4862 section 5.5.3); concluding
// there gave slaac networks no address up to v2.1.x. The dhcp mode keeps #868's two-second conclusion (#818).

// concludesOnAdvertisedAbsence is the early no-DHCPv6 conclusion, taken only in the dhcp ipv6_mode.
func concludesOnAdvertisedAbsence(mode proto.Mode6, r RAObservation) bool {
	return !IPv6ModeFormsAddresses(mode) && advertisedNoDHCPv6(r)
}

// Unguarded, accept_ra=0 on a host-namespace link or a second kernel default route both look healthy (#875, #821).

// checkRouterAdvertGuardShape requires HonorRouterAdverts on the persistent v6 client and refuses it everywhere else.
func checkRouterAdvertGuardShape(opts *DHCPClientOptions, oneShot bool) error {
	if opts.HonorRouterAdverts {
		switch {
		case !opts.V6:
			return errors.New("dhcp: HonorRouterAdverts is IPv6-only")
		case opts.NetNS == nil:
			return errors.New("dhcp: HonorRouterAdverts needs a target network namespace")
		case oneShot:
			return errors.New("dhcp: HonorRouterAdverts is for the persistent client, not the CreateEndpoint acquisition")
		}
		return nil
	}
	if opts.V6 && !oneShot {
		return errors.New("dhcp: a persistent DHCPv6 client needs HonorRouterAdverts: " +
			"DHCPv6 carries no router (RFC 9915 §21 has no next-hop option) and RFC 5942 §4 " +
			"forbids deriving an on-link prefix from the assigned address, so the container's " +
			"kernel must be processing Router Advertisements or the endpoint has an address and no route")
	}
	return nil
}

// The plugin clears disable_ipv6 and writes the RA guard first (pkg/plugin/v6_link.go); NewClient6 then waits for a
// non-tentative link-local itself, and the engine sets disable_ipv6 on a link with no IPv6 address (#911).

// newLibClient6 opens a DHCPv6 library client on iface, inside opts.NetNS when one is given.
func newLibClient6(iface string, params proto.Params6, opts *DHCPClientOptions) (*dhcpruntime.Client6, error) {
	cfg := dhcpruntime.ClientConfig6{
		Interface: iface,
		Params6:   params,
		// A resumed binding makes the first message a Confirm (RFC 9915 section 18.2.12, #820).
		Resume:      opts.Resume,
		EventBuffer: eventBuffer,
	}

	open := func(name string) (*dhcpruntime.Client6, error) {
		cfg.Interface = name
		return dhcpruntime.NewClient6(cfg)
	}
	abandon := func(client *dhcpruntime.Client6) { _ = client.Run(canceledContext()) }

	if opts.NetNS == nil {
		client, _, err := openOnLink(iface, opts.LinkIndex, open, abandon)
		return client, err
	}

	var (
		client *dhcpruntime.Client6
		opened string
		cerr   error
	)
	if err := inNetNS(*opts.NetNS,
		func() { client, opened, cerr = openOnLink(iface, opts.LinkIndex, open, abandon) },
		func() {
			if client != nil {
				_ = client.Run(canceledContext())
			}
		},
	); err != nil {
		return nil, err
	}
	if cerr != nil {
		return nil, fmt.Errorf("dhcp: open a DHCPv6 client on %v: %w", opened, cerr)
	}
	return client, nil
}

// Router() is zero until the first advertisement, and RFC 9915 section 7.6 gives Solicit no MRC or MRD with SOL_MAX_RT
// 3600 s, so ctx ends the attempt, as in 1.9.0 (RFC 4861 section 6.3.7, #911).

// getIP6 is GetIP for DHCPv6, taking the segment's advertisement observation at the deadline.
func getIP6(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
	var ra RAObservation
	if err := checkRouterAdvertGuardShape(opts, true); err != nil {
		return Info{}, ra, err
	}

	params, err := buildParams6(opts, true)
	if err != nil {
		return Info{}, ra, err
	}
	opts.params6 = params
	// No Params snapshot rides a v6 event; see DHCPClientOptions.paramsWritten (#911).
	opts.paramsWritten = true

	if dl, ok := ctx.Deadline(); ok {
		if budget, want := time.Until(dl), RouterDiscoveryWindow(params); budget < want {
			// A deadline shorter than router discovery makes the absence verdict unreliable, so it is logged (#911).
			log.WithField("iface", iface).
				WithField("lease_timeout", budget.Round(time.Second)).
				WithField("router_discovery_window", want).
				Warn("The DHCPv6 acquisition budget is shorter than RFC 4861 router discovery; " +
					"a \"no router advertisement\" verdict on this network describes the deadline, not the segment")
		}
	}

	// At most two passes; retryWithoutHint6 clears the hint before it answers yes (#911).
	for {
		info, ra, err := acquireOnce6(ctx, iface, opts.params6, opts)
		declined, again := opts.retryWithoutHint6(err)
		if !again {
			return info, ra, err
		}
		log.WithField("iface", iface).
			WithField("preferred_ipv6", declined.String()).
			Warn("The preferred DHCPv6 address is in use by another node on the segment; " +
				"it has been declined (RFC 9915 section 18.2.10) and the endpoint is " +
				"asking for a server-chosen address instead")
	}
}

// The resumed binding goes with the hint, as a Confirm would ask about the declined address (RFC 9915 section 18.2.12).

// retryWithoutHint6 reports whether a failed hinted attempt may run once more, clearing the hint and resumed binding.
func (o *DHCPClientOptions) retryWithoutHint6(err error) (netip.Addr, bool) {
	if !errors.Is(err, errV6HintInUse) || !o.params6.Hint.IsValid() {
		return netip.Addr{}, false
	}
	declined := o.params6.Hint
	o.params6.Hint = netip.Addr{}
	o.Resume = nil
	return declined, true
}

// acquireOnce6 is one DHCPv6 acquisition on iface under params, with the observation taken at the end.
func acquireOnce6(ctx context.Context, iface string, params proto.Params6, opts *DHCPClientOptions) (Info, RAObservation, error) {
	var ra RAObservation

	client, err := newLibClient6(iface, params, opts)
	if err != nil {
		return Info{}, ra, err
	}

	manager := ""
	if opts.Records != nil {
		manager = opts.Records.NewManagerID()
	}
	// A new manager's counters start at zero, so the delta snapshots are reset on the retry pass (#814).
	opts.managerStarted()

	info, lastE := runAcquisition6(ctx, iface, client, opts, params.Hint, V6AcquisitionWindow(params))

	// After the drain, since the last advertisement can arrive with the final event (#911).
	ra = raObservation(client.Router())
	stats := client.Stats()
	opts.count(manager, stats)
	opts.v6ModeReport(stats)
	opts.v6PrefixReport(stats)
	// The one-shot's RFC 4861 solicitations and advertisements are counted here or nowhere (#814).
	opts.routerReport(stats)

	out, err := acquisitionResult6(info, lastE)
	return out, ra, err
}

// Measured on the lane 2026-09-16, run 35131643324: advertised routes on a link with no global IPv6 made the daemon
// fail the whole sandbox with "routes ... permission denied", as the engine disables IPv6 there (#821, #818).

// acquisitionResult6 is the DHCPv6 acquisition's verdict: the lease, or the zero Info beside the reason there is none.
func acquisitionResult6(info Info, lastE error) (Info, error) {
	if info.IP != "" {
		return info, nil
	}
	if lastE == nil {
		lastE = ErrNoLease
	}
	return Info{}, lastE
}

// Measured against dnsmasq 2.91 on the lane 2026-09-06, run 34058213252: a server that honours hints re-offered the
// declined address about once a second until the deadline (RFC 9915 sections 18.2.1 and 18.2.10.1, #911).

// errV6HintInUse is a DAD conflict on an attempt that asked the server for a particular address.
var errV6HintInUse = errors.New("the preferred DHCPv6 address is in use by another node on the segment")

// v6AcquisitionClient is the part of *dhcpruntime.Client6 the acquisition loop reads, including Router() (#911).
type v6AcquisitionClient interface {
	Run(ctx context.Context) error
	Events() <-chan lease.Event
	Router() proto.RouterObservation
}

// Measured 2026-09-06: three mutants of the loop survived while it lived inside getIP6 (#911).

// runAcquisition6 runs one DHCPv6 acquisition to a verdict under the smaller of ctx and window, then drains the client.
func runAcquisition6(ctx context.Context, iface string, client v6AcquisitionClient, opts *DHCPClientOptions, hint netip.Addr, window time.Duration) (Info, error) {
	acqCtx, endAcq := context.WithTimeout(ctx, window)
	defer endAcq()

	runCtx, cancel := context.WithCancel(acqCtx)
	done := make(chan error, 1)
	go func() { done <- client.Run(runCtx) }()

	poll := time.NewTicker(v6RouterPollInterval)
	defer poll.Stop()

	var (
		info  Info
		got   bool
		lastE error
	)
	for !got {
		select {
		case <-acqCtx.Done():
			// The refusal already in hand is kept beside the deadline (RFC 9915 section 18.2.10.1, #816).
			if lastE == nil {
				lastE = acqCtx.Err()
			} else {
				lastE = fmt.Errorf("%w; the DHCPv6 acquisition budget then ran out: %w", lastE, acqCtx.Err())
			}
			got = true

		case <-poll.C:
			// The segment answered with an advertisement, not a lease event (#818).
			if concludesOnAdvertisedAbsence(opts.Mode6, raObservation(client.Router())) {
				lastE = ErrNoDHCPv6OnSegment
				got = true
			}

		case ev, ok := <-client.Events():
			if !ok {
				lastE = ErrNoLease
				got = true
				break
			}
			// Before the record and the step, as in the persistent client's loop (#911).
			opts.carryResumedConfig6(&ev)
			opts.record(ev)
			opts.reportFQDN6(ev)
			out := acquireStep6(ev, hint.IsValid(), opts.MainPrefix6)
			if out.Err != nil {
				lastE = out.Err
			}
			if out.Done {
				info = out.Info
				got = true
			}
		}
	}

	cancel()
	// Drained in the foreground for the reason GetIP's drain gives (#911).
	for ev := range client.Events() {
		opts.carryResumedConfig6(&ev)
		opts.record(ev)
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		log.WithError(err).WithField("iface", iface).Debug("DHCPv6 acquisition manager returned an error")
	}
	return info, lastE
}

// proto.Machine6 sends an Information-request only on M=0 O=1, so Configured is the stateless verdict (RFC 9915 section
// 18.2.6, #868).

// acquireStep6 ends a one-shot DHCPv6 acquisition on Acquired, Configured, or a hinted conflict.
func acquireStep6(ev lease.Event, hinted bool, main netip.Prefix) acquireOutcome {
	switch ev.Kind {
	case lease.Acquired:
		info, _ := infoFromLease(ev.Lease, ev.Router, time.Now(), main)
		return acquireOutcome{Info: info, Done: true}
	case lease.Configured:
		return acquireOutcome{Done: true, Err: ErrNoV6Address}
	case lease.Failed:
		if hinted && ev.Reason == proto.ReasonConflict {
			return acquireOutcome{Done: true, Err: fmt.Errorf("dhcp: %w: %v", errV6HintInUse, ev.Note)}
		}
		if err := v6FailureCause(ev); err != nil {
			return acquireOutcome{Err: err}
		}
		return acquireOutcome{Err: fmt.Errorf("dhcp: DHCPv6 acquisition failed: %v", ev.Reason)}
	}
	return acquireOutcome{}
}

// ErrNoV6Address is a stateless segment's configuration without an address (RFC 9915 section 18.2.6), not a fault
// (#868).
var ErrNoV6Address = errors.New("dhcp: the segment offers DHCPv6 configuration but no address")

// infoFromConfig renders the stateless answer's options 23 and 24 (RFC 3646) as an Info, leaving every other field
// empty.
func infoFromConfig(c lease.Configuration) (Info, int) {
	info := Info{SearchList: append([]string(nil), c.Search...)}
	for _, d := range c.DNS {
		info.DNSServers = append(info.DNSServers, d.String())
	}
	// The same sanitising filter every lease crosses (#911).
	return info, sanitizeInfo(&info)
}

// A Confirm's Reply carries no options 23 or 24 (RFC 9915 section 18.2.13); measured on the lane 2026-09-06, a restart
// lost the DHCPv6 resolver until T1, sixty seconds later (#911).

// carryResumedConfig6 fills a resumed binding's RFC 3646 lists from the remembered lease, once, when both are absent.
func (o *DHCPClientOptions) carryResumedConfig6(ev *lease.Event) {
	if o.resumedConfigTaken || !o.V6 || o.Resume == nil {
		return
	}
	switch ev.Kind {
	case lease.Acquired, lease.Renewed, lease.Changed:
	default:
		return
	}
	o.resumedConfigTaken = true
	if len(ev.Lease.DNS) > 0 || len(ev.Lease.DomainSearch) > 0 {
		return
	}
	ev.Lease.DNS = append([]netip.Addr(nil), o.Resume.DNS...)
	ev.Lease.DomainSearch = append([]string(nil), o.Resume.DomainSearch...)
}

// reportFQDN6 logs the server's option 39 at bind. S set in the Reply is the server taking the AAAA (RFC 4704 section
// 4.1); a Reply without S, or without the option, leaves the name unregistered by anyone, since the plugin does no DNS
// update of its own (#1029).
func (o *DHCPClientOptions) reportFQDN6(ev lease.Event) {
	if !o.V6 || o.FQDN == "" || o.Hostname == "" || ev.Kind != lease.Acquired {
		return
	}
	entry := log.WithField("hostname", o.Hostname)
	if !ev.Lease.HasFQDN {
		entry.Info("The DHCPv6 server's Reply carried no Client FQDN option, so it did not say whether it registers " +
			"an AAAA record for this name")
		return
	}
	flags := ev.Lease.FQDN.Flags
	entry = entry.WithFields(log.Fields{
		"fqdn_name":  ev.Lease.FQDN.Name,
		"fqdn_flags": fmt.Sprintf("0x%02x", flags),
		"fqdn_s":     flags&wire.ClientFQDNFlagS != 0,
		"fqdn_o":     flags&wire.ClientFQDNFlagO != 0,
		"fqdn_n":     flags&wire.ClientFQDNFlagN != 0,
	})
	if flags&wire.ClientFQDNFlagS == 0 {
		entry.Warn("The DHCPv6 server answered the Client FQDN option without the S flag, so it does not register " +
			"the AAAA record for this name and the plugin does not either")
		return
	}
	entry.Info("The DHCPv6 server registers the AAAA record for this name")
}
