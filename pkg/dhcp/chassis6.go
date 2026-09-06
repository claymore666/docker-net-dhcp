// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
)

// maxRtrSolicitationDelay is RFC 4861 section 10's host constant of that
// name: "MAX_RTR_SOLICITATION_DELAY 1 second".
//
// Spelled here rather than read from proto.Params6 because the library
// does not carry it: the delay is applied by the sender of the first
// solicitation and the library's schedule starts after it. It is in
// RouterDiscoveryWindow because the CALLER's deadline has to cover the
// whole of RFC 4861 section 6.3.7 or its verdict is about the deadline.
const maxRtrSolicitationDelay = time.Second

// RouterDiscoveryWindow is the longest RFC 4861 section 6.3.7's
// solicitation schedule can take, and therefore the shortest deadline
// under which "no router advertisement arrived" is a statement about
// the SEGMENT rather than about the deadline.
//
// Section 6.3.7: a host waits "a random amount of time between 0 and
// MAX_RTR_SOLICITATION_DELAY" and then transmits "up to
// MAX_RTR_SOLICITATIONS Router Solicitation messages", each "separated
// by at least RTR_SOLICITATION_INTERVAL seconds". So the last
// solicitation leaves at delay + (n-1) intervals, and one further
// interval is the wait for its answer: delay + n*interval, which is
// 1 + 3*4 = 13s with the library's defaults.
//
// WHY IT IS DERIVED FROM Params6 AND NOT WRITTEN DOWN. The two numbers
// are the library's, a caller can change them, and a constant here
// would be a second derivation of one fact -- the shape that produces
// two answers to "how long does discovery take". The one number this
// function does own is the initial delay, which the library has no
// field for; see maxRtrSolicitationDelay.
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

// checkRouterAdvertGuardShape refuses HonorRouterAdverts on every shape
// it does not belong on, and refuses a persistent v6 client that does
// not carry it.
//
// TWO REFUSALS IN ONE FUNCTION BECAUSE THEY ARE ONE RULE: the guard
// belongs to the persistent DHCPv6 client and to nothing else, which
// makes both "set on the wrong client" and "missing on the right one"
// wiring mistakes of the same kind. A dropped flag is a wiring mistake
// that looks like a working plugin, and neither of its failures is one
// anything downstream would report -- accept_ra=2 on a link still in
// the HOST namespace changes the host's router discovery, and a v6
// endpoint whose kernel ignores advertisements has an address, no
// route, and a completely healthy look for the length of one router
// lifetime (#875).
//
// oneShot is the CreateEndpoint acquisition. Its link is still in the
// host's network namespace when it runs, which is why the guard is
// refused there and not merely skipped.
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

// newLibClient6 opens a DHCPv6 library client on iface, inside
// opts.NetNS when one is given.
//
// THE ORDER ON THIS PATH IS FIXED AND EACH STEP IS A PRECONDITION OF
// THE NEXT:
//
//  1. disable_ipv6 cleared on the link. NOT here -- the plugin does it
//     (pkg/plugin/v6_link.go) before this is called, in the same
//     namespace entry as step 2, because it needs a writable /proc/sys
//     and the plugin is where the mount-namespace machinery lives. On a
//     link with disable_ipv6=1 no link-local ever appears, so step 3
//     fails inside its own bound and the failure reads as a quiet
//     segment. The engine sets that flag on a sandbox interface whose
//     endpoint carries no IPv6 address, which is a reachable state.
//  2. the Router-Advertisement guard written and read back, same place,
//     same reason. HonorRouterAdverts is this function's assertion that
//     it happened.
//  3. NewClient6, which is where the sockets are made and therefore
//     where the namespace is decided. IT WAITS FOR A NON-TENTATIVE
//     LINK-LOCAL ITSELF -- the library's InterfaceLinkLocal states the
//     bound -- and the chassis does NOT wait a second time: one fact,
//     one derivation.
//  4. Run, from Start, on any thread.
func newLibClient6(iface string, params proto.Params6, opts *DHCPClientOptions) (*dhcpruntime.Client6, error) {
	cfg := dhcpruntime.ClientConfig6{
		Interface: iface,
		Params6:   params,
		// The binding this identity held in a previous run of the
		// plugin, which makes the first message on the wire RFC 9915
		// section 18.2.12's Confirm instead of a Solicit -- the whole
		// of what makes an address survive a plugin restart (#820).
		Resume:      opts.Resume,
		EventBuffer: eventBuffer,
	}

	if opts.NetNS == nil {
		return dhcpruntime.NewClient6(cfg)
	}

	var (
		client *dhcpruntime.Client6
		cerr   error
	)
	if err := inNetNS(*opts.NetNS,
		func() { client, cerr = dhcpruntime.NewClient6(cfg) },
		func() {
			if client != nil {
				_ = client.Run(canceledContext())
			}
		},
	); err != nil {
		return nil, err
	}
	if cerr != nil {
		return nil, fmt.Errorf("dhcp: open a DHCPv6 client on %v: %w", iface, cerr)
	}
	return client, nil
}

// getIP6 is GetIP for a DHCPv6 endpoint: one acquisition, bounded by
// the caller's deadline, and an observation of what the segment
// advertised whether or not an address came out of it.
//
// THE OBSERVATION IS TAKEN AT THE END AND NOWHERE ELSE. Router() is a
// running answer -- it is the zero value until the first advertisement
// arrives, and RFC 4861 section 6.3.7 allows that to be seconds --
// so a read taken when the loop starts, or on the first event, would
// say "no router" for a segment that answers in a second. The verdict
// pkg/plugin/v6_absence.go draws is only as good as the moment this is
// read, and the moment is the deadline. See RouterDiscoveryWindow for
// what the deadline has to cover for the reading to be about the
// segment at all.
//
// THE v6 MACHINE NEVER GIVES UP ON ITS OWN. RFC 9915 section 7.6 puts
// SOL_MAX_RT at 3600 s and gives the Solicit exchange no MRC and no
// MRD, so ctx is the only thing that ends a hopeless attempt. That is
// the same shape the v4 one-shot has and the same shape 1.9.0 had; it
// is stated because it is the reason lease_timeout is not advisory
// here.
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
	// No Params snapshot rides a v6 event; see
	// DHCPClientOptions.paramsWritten.
	opts.paramsWritten = true

	if dl, ok := ctx.Deadline(); ok {
		if budget, want := time.Until(dl), RouterDiscoveryWindow(params); budget < want {
			// AUDIBLE, NOT FATAL. The endpoint can still get an
			// address -- a segment that answers immediately answers
			// inside any budget -- but the ABSENCE verdict this
			// function's observation feeds cannot be trusted below
			// this line, and an operator reading
			// dhcpv6_no_router_advert deserves to know that the
			// number was produced by a deadline shorter than router
			// discovery.
			log.WithField("iface", iface).
				WithField("lease_timeout", budget.Round(time.Second)).
				WithField("router_discovery_window", want).
				Warn("The DHCPv6 acquisition budget is shorter than RFC 4861 router discovery; " +
					"a \"no router advertisement\" verdict on this network describes the deadline, not the segment")
		}
	}

	client, err := newLibClient6(iface, params, opts)
	if err != nil {
		return Info{}, ra, err
	}

	manager := ""
	if opts.Records != nil {
		manager = opts.Records.NewManagerID()
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- client.Run(runCtx) }()

	var (
		info  Info
		got   bool
		lastE error
	)
	for !got {
		select {
		case <-ctx.Done():
			lastE = ctx.Err()
			got = true

		case ev, ok := <-client.Events():
			if !ok {
				lastE = ErrNoLease
				got = true
				break
			}
			opts.record(ev)
			out := acquireStep6(ev)
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
	// Drained in the foreground and recorded, for the reason GetIP's
	// drain gives: the Join manager reads this record the moment
	// CreateEndpoint returns, and a background drain would race it.
	for ev := range client.Events() {
		opts.record(ev)
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		log.WithError(err).WithField("iface", iface).Debug("DHCPv6 acquisition manager returned an error")
	}
	// AFTER the drain: the last advertisement can arrive on the same
	// pass as the event that ended the loop.
	ra = raObservation(client.Router())
	opts.count(manager, client.Stats())

	if info.IP == "" {
		if lastE == nil {
			lastE = ErrNoLease
		}
		return Info{}, ra, lastE
	}
	return info, ra, nil
}

// acquireStep6 decides whether a one-shot DHCPv6 acquisition ends on ev.
//
// Three arms, and the third is the one that is not obvious:
//
//   - Acquired ends it with an address, as v4's does.
//   - Failed names the cause so the caller's error is the reason rather
//     than "context deadline exceeded".
//   - Configured ENDS IT WITHOUT AN ADDRESS, and that is a deliberate
//     early return rather than a wait for the deadline. RFC 9915
//     section 18.2.6's Reply only ever arrives on the stateless path:
//     proto.Machine6 switches to the Information-request exactly when a
//     Router Advertisement says M=0 O=1, so a Configured in this window
//     IS the observation the deadline would have been waited out to
//     take -- the segment has said, on the wire, that it has no
//     addresses. Waiting the remaining lease_timeout would produce the
//     same verdict (dhcpv6_not_offered) and charge every container
//     start on every stateless network for it.
//
// A Lost is impossible before an Acquired and needs no arm: the library
// emits it only for a lease it had.
func acquireStep6(ev lease.Event) acquireOutcome {
	switch ev.Kind {
	case lease.Acquired:
		info, _ := infoFromLease(ev.Lease, time.Now())
		return acquireOutcome{Info: info, Done: true}
	case lease.Configured:
		return acquireOutcome{Done: true, Err: ErrNoV6Address}
	case lease.Failed:
		return acquireOutcome{Err: fmt.Errorf("dhcp: DHCPv6 acquisition failed: %v", ev.Reason)}
	}
	return acquireOutcome{}
}

// ErrNoV6Address is a DHCPv6 exchange that produced configuration and
// no address: the segment is stateless (RFC 9915 section 18.2.6).
//
// It is an ERROR because the caller asked for an address and did not
// get one, and it is NOT A FAULT: the endpoint is created without a
// DHCPv6 address and the container starts. The two statements live
// together here because separating them is what #868 was -- a timeout
// that meant either "this segment has no DHCPv6 addresses" or "the
// DHCPv6 server went quiet", read as the second every time.
var ErrNoV6Address = errors.New("dhcp: the segment offers DHCPv6 configuration but no address")

// infoFromConfig renders RFC 9915 section 18.2.6's stateless answer as
// the Info the plugin applies.
//
// EVERYTHING AN ADDRESS WOULD HAVE CARRIED IS ABSENT AND STAYS ABSENT.
// The DNS servers are option 23 and the search list option 24 (RFC
// 3646); there is no address, no lifetime, no MTU and no route, because
// none of those is in an Information-request Reply. The plugin's
// contract for an empty field is "do not change what the container
// has", so an Info built here changes exactly the two things the server
// actually sent.
func infoFromConfig(c lease.Configuration) (Info, int) {
	info := Info{SearchList: append([]string(nil), c.Search...)}
	for _, d := range c.DNS {
		info.DNSServers = append(info.DNSServers, d.String())
	}
	// The same filter every lease crosses, at the same boundary and for
	// the same reason: these strings are the server's choice.
	return info, sanitizeInfo(&info)
}
