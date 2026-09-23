// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Package dhcp is the chassis between the plugin and the in-process DHCP library, holding the Docker side only (#899).
package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
)

// ErrNoLease is returned when an acquisition ended without one.
var ErrNoLease = errors.New("dhcp: no lease was acquired")

// ErrAddressConflict is an acquisition whose last failure was an RFC 5227 conflict on the offered address.
var ErrAddressConflict = errors.New("dhcp: the offered address is already in use on this segment")

// DHCPClientOptions is one endpoint's DHCP configuration.
type DHCPClientOptions struct {
	// Hostname is DHCPv4 option 12, omitted when empty.
	Hostname string

	// FQDN, when non-empty, asks the server to register Hostname in DNS (RFC 4702 option 81).
	FQDN string

	// V6 selects the DHCPv6 (RFC 9915) library client; a dual-stack endpoint runs one client per family (#911).
	V6 bool

	// RFC 9915 section 11: a DUID "SHOULD NOT change over time if at all possible", so the chassis mints it (D10,
	// #911).

	// Identity6 is the required DHCPv6 DUID and IAID of this endpoint (RFC 9915 sections 11 and 12).
	Identity6 Identity6

	// DHCPv6 carries no router (RFC 9915 section 21), so this client runs RFC 4861 section 6.3.4 discovery and the
	// kernel must not; there is no opt-out (D30 Q3, #821, #875).

	// HonorRouterAdverts asserts the link is under the Router-Advertisement guard, required on a persistent v6 client.
	HonorRouterAdverts bool

	// A path is re-resolved by the callee, and a recycled PID lands the socket in another container (#688).

	// NetNS is the borrowed namespace descriptor to lease in, nil meaning the caller's own.
	NetNS *netns.NsHandle

	// The engine renames the link when it moves it into the sandbox, so the name can go stale (#1050).

	// LinkIndex is the endpoint's link by index, zero meaning the interface name stands.
	LinkIndex int

	// MAC is the endpoint's pinned hardware address, the same for the one-shot and the persistent client (#152).
	MAC net.HardwareAddr

	// RequestedIP, when non-empty, is option 50 in the DISCOVER, a preference the server may ignore (RFC 2131 section
	// 4.4.1).
	RequestedIP string

	// Mode6 is the parsed `ipv6_mode`, set with Identity6 so a forgotten field fails at buildParams6 (#817).
	Mode6 proto.Mode6

	// StrictAuto6 is `ipv6_auto_strict`, mapped by strictAutoFallback to a negative proto.Params6.AutoFallback (#817).
	StrictAuto6 bool

	// MainPrefix6 is `ipv6_main_prefix`, read only in infoFromLease so both sides pick the same address (#818).
	MainPrefix6 netip.Prefix

	// A router readvertises every few seconds (RFC 4861 section 6.2.1), so a dhcp-mode counter would climb forever
	// (#818).

	// OnV6PrefixesIgnored gets the gain in RFC 4862 section 5.5.3 prefixes formed into no address, in forming modes
	// only.
	OnV6PrefixesIgnored func(uint64)

	// OnV6Fallback gets the gain in Mode6Auto fallbacks that formed an address, nil in tests (#817).
	OnV6Fallback func(uint64)

	// PreferredV6 is the RFC 9915 section 21.6 IA Address hint, which section 18.3.2 lets the server overrule, the v6
	// twin of RequestedIP (#213).
	PreferredV6 string

	// AllowServers and DenyServers filter DHCPv4 servers by option 54, where dhcpcd matched the source address (#899).
	AllowServers []string
	DenyServers  []string

	// ClientID is the option-61 payload without its type byte, prepended as type 0 by the chassis (D10).
	ClientID []byte

	// VendorClass overrides option 60, VendorID when empty.
	VendorClass string

	// ConflictMode is the parsed RFC 5227 `conflict_check` mode, zero being proto.ConflictWait (D23, #882).
	ConflictMode proto.ConflictMode

	// OnConflict is the one route both managers report an address conflict on, nil in tests (#882).
	OnConflict func(Conflict)

	// OnACDStats gets the delta in the RFC 5227 counters, keeping the plugin's totals monotonic (#882).
	OnACDStats func(ACDStats)

	// A renewal request produces no lease event: a production host, 24 h lease, sent four across 7h52m with
	// dhcp_timeouts at 0 (#940).

	// OnRenewalStats gets the gain in unanswered renewal requests, fed from a timer too, on the persistent client only.
	OnRenewalStats func(RenewalStats)

	// A quiet router produces no lease event; an event-only fold left a probe run unreported for 19h52m on a production
	// host (#814).

	// OnRouterStats gets the delta in the RFC 4861 router-discovery counters, read on the watch tick too, v6 only.
	OnRouterStats func(RouterStats)

	// Resume is a lease from a previous run, sent as an INIT-REBOOT DHCPREQUEST (RFC 2131 section 4.4.2).
	Resume *lease.Lease

	// Records and RecordID are the durable record this manager writes its own events and counters to, nil in tests.
	Records  *Records
	RecordID string

	// paramsWritten marks the Params snapshot written; a v6 manager has no Params6 slot to write, so it is not
	// replayable (#911).
	paramsWritten bool
	params        proto.Params
	params6       proto.Params6

	// resumedConfigTaken marks carryResumedConfig6's one chance at a resumed lease's RFC 3646 lists (#911).
	resumedConfigTaken bool

	// acdSeen is the last ACD snapshot handed to OnACDStats, which makes it a delta.
	acdSeen ACDStats

	// fallbacksSeen and prefixesIgnoredSeen do the same for OnV6Fallback and OnV6PrefixesIgnored.
	fallbacksSeen       uint64
	prefixesIgnoredSeen uint64

	// routerSeen does the same for OnRouterStats.
	routerSeen RouterStats
}

// record writes one manager event, if this manager has a record.
func (o *DHCPClientOptions) record(ev lease.Event) {
	if o.Records == nil || o.RecordID == "" {
		return
	}
	var params *proto.Params
	if !o.paramsWritten {
		params = &o.params
		o.paramsWritten = true
	}
	if err := o.Records.Observed(o.RecordID, ev, params); err != nil {
		log.WithError(err).WithField("record", o.RecordID).
			Warn("Could not write the lease record; a plugin restart will not resume this lease")
	}
}

// count writes one manager's counter snapshot under its own id.
func (o *DHCPClientOptions) count(manager string, s lease.Stats) {
	if o.Records == nil || o.RecordID == "" || manager == "" {
		return
	}
	if err := o.Records.Counted(o.RecordID, manager, s); err != nil {
		log.WithError(err).WithField("record", o.RecordID).
			Warn("Could not write the manager's counters to the lease record")
	}
}

// The library emits exactly one of Failed or Lost with ReasonConflict per conflict (RFC 5227 sections 2.1 and 2.4,
// #882).

// conflict reports one address conflict to the caller, if this event is one.
func (o *DHCPClientOptions) conflict(ev lease.Event) bool {
	if ev.Reason != proto.ReasonConflict {
		return false
	}
	if ev.Kind != lease.Failed && ev.Kind != lease.Lost {
		return false
	}
	if o.OnConflict != nil {
		o.OnConflict(Conflict{Held: ev.Kind == lease.Lost, Addr: bareAddr(ev.Lease), Note: ev.Note})
	}
	return true
}

// Conflict is one address conflict, as much of it as leaves the library.
type Conflict struct {
	// Held says the address was in use by this endpoint, RFC 5227 section 2.4's ongoing check, not the 2.1 probe
	// (#882).
	Held bool

	// Addr is the address found in use, empty when Held is false because Failed carries no lease (#882).
	Addr string

	// Note is the library's own human-readable line for the event.
	Note string
}

// bareAddr renders a lease's address without its prefix length, or "" for a lease that has none.
func bareAddr(l lease.Lease) string {
	if !l.Addr.IsValid() {
		return ""
	}
	return l.Addr.Addr().String()
}

// Probes are sent from a timer: a ConflictAsync endpoint probed within 9 s of the bind was folded 19h52m later,
// measured on the production host 2026-09-10 (#882).

// acdReport hands the caller the RFC 5227 counter gains since the last call, on every event and at manager end.
func (o *DHCPClientOptions) acdReport(s lease.Stats) {
	if o.OnACDStats == nil {
		return
	}
	cur := acdStats(s)
	delta := cur.Sub(o.acdSeen)
	o.acdSeen = cur
	if delta.IsZero() {
		return
	}
	o.OnACDStats(delta)
}

// routerReport hands the caller the RFC 4861 counter gains since the last call, on the v6 fold and the watch tick
// (#814).
func (o *DHCPClientOptions) routerReport(s lease.Stats) {
	if o.OnRouterStats == nil {
		return
	}
	cur := routerStats(s)
	delta := cur.Sub(o.routerSeen)
	o.routerSeen = cur
	if delta.IsZero() {
		return
	}
	o.OnRouterStats(delta)
}

// getIP6 retries through one options value with a fresh manager; stale snapshots halved the reported counts (#814).

// managerStarted forgets every delta snapshot on this options value.
func (o *DHCPClientOptions) managerStarted() {
	o.acdSeen = ACDStats{}
	o.fallbacksSeen = 0
	o.prefixesIgnoredSeen = 0
	o.routerSeen = RouterStats{}
}

// v6ModeReport hands the caller the Mode6Auto fallbacks counted since the last call, on the v6 paths only (#817).
func (o *DHCPClientOptions) v6ModeReport(s lease.Stats) {
	if o.OnV6Fallback == nil {
		return
	}
	if s.SLAACFallbacks <= o.fallbacksSeen {
		return
	}
	delta := s.SLAACFallbacks - o.fallbacksSeen
	o.fallbacksSeen = s.SLAACFallbacks
	o.OnV6Fallback(delta)
}

// v6PrefixReport hands the caller the advertised prefixes formed into no address since the last call (#818).
func (o *DHCPClientOptions) v6PrefixReport(s lease.Stats) {
	if o.OnV6PrefixesIgnored == nil {
		return
	}
	if s.SLAACPrefixesIgnored <= o.prefixesIgnoredSeen {
		return
	}
	delta := s.SLAACPrefixesIgnored - o.prefixesIgnoredSeen
	o.prefixesIgnoredSeen = s.SLAACPrefixesIgnored
	o.OnV6PrefixesIgnored(delta)
}

// RAObservation is what this segment's advertisements said (RFC 4861 section 4.2), a diagnostic only (D30 Q2, #868).
type RAObservation struct {
	// Seen says at least one advertisement arrived on this link (RFC 4861 section 4.2).
	Seen bool
	// Managed is the M bit, "addresses are available via DHCPv6".
	Managed bool
	// Other is the O bit, the stateless segment of RFC 9915 section 18.2.6.
	Other bool
}

// Merge ORs another attempt's observation into this one, since an early advertisement stays evidence (#911).
func (o RAObservation) Merge(other RAObservation) RAObservation {
	return RAObservation{
		Seen:    o.Seen || other.Seen,
		Managed: o.Managed || other.Managed,
		Other:   o.Other || other.Other,
	}
}

// raObservation converts the library's router observation, as pkg/plugin must not name a library type (M6b, #911).
func raObservation(r proto.RouterObservation) RAObservation {
	return RAObservation{Seen: r.Seen, Managed: r.Managed, Other: r.Other}
}

// acquireOutcome is whether a one-shot acquisition ends on an event, with what address, and the cause if it times out.
type acquireOutcome struct {
	Info Info
	Done bool
	Err  error
}

// RFC 2131 section 3.1(5): the library sends the DHCPDECLINE, waits "a minimum of ten seconds" and restarts (#882).

// acquireStep ends a one-shot acquisition on lease.Acquired only, keeping the last cause in Err for the deadline.
func acquireStep(ev lease.Event, conflicted bool, now time.Time) acquireOutcome {
	switch ev.Kind {
	case lease.Acquired:
		// A v4 lease has no Addrs list, so there is no main prefix to pass (#818).
		info, _ := infoFromLease(ev.Lease, ev.Router, now, netip.Prefix{})
		return acquireOutcome{Info: info, Done: true}
	case lease.Failed:
		if conflicted {
			return acquireOutcome{Err: fmt.Errorf("%w: %v", ErrAddressConflict, ev.Lease.Addr)}
		}
		return acquireOutcome{Err: fmt.Errorf("dhcp: acquisition failed: %v", ev.Reason)}
	}
	return acquireOutcome{}
}

// GetIP performs the CreateEndpoint acquisition and returns as soon as a lease exists.
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
	var ra RAObservation
	if opts.V6 {
		return getIP6(ctx, iface, opts)
	}

	params, err := buildParams(opts, true)
	if err != nil {
		return Info{}, ra, err
	}
	opts.params = params

	client, err := newLibClient(iface, params, opts)
	if err != nil {
		return Info{}, ra, err
	}

	// One manager id per instance; the Join manager mints its own (lease.RecordEvent.Manager, #899).
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
			opts.acdReport(client.Stats())
			// A conflict does not end the acquisition: the library declines and restarts, and the deadline ends it (RFC
			// 2131 section 3.1(5), #882).
			out := acquireStep(ev, opts.conflict(ev), time.Now())
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
	// Drained in the foreground: the Lost{ReasonStopped} must be on disk before the Join manager reads the record
	// (#899).
	for ev := range client.Events() {
		opts.record(ev)
		opts.conflict(ev)
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		log.WithError(err).WithField("iface", iface).Debug("Acquisition manager returned an error")
	}
	final := client.Stats()
	opts.acdReport(final)
	opts.count(manager, final)

	if info.IP == "" {
		if lastE == nil {
			lastE = ErrNoLease
		}
		return Info{}, ra, lastE
	}
	return info, ra, nil
}

// DHCPClient is the persistent per-endpoint manager inside the container's namespace while the endpoint is joined.
type DHCPClient struct {
	iface   string
	opts    DHCPClientOptions
	params  proto.Params
	params6 proto.Params6

	// Exactly one of client and client6 is non-nil; ACD is RFC 5227 and v4 only, DAD RFC 4862 and v6 only (#911).
	client  *dhcpruntime.Client
	client6 *dhcpruntime.Client6
	cancel  context.CancelFunc
	done    chan error
	events  chan Event
	manager string

	// runner is the family-independent half of the built client, which a renewal test can replace (#940).
	runner libClient

	// renewals turns the library's renewal counters into the unanswered count, touched only by translate (#940).
	renewals renewalWatch

	// pollEvery is how often translate folds the counters with no event, zero meaning renewalPollInterval (#940).
	pollEvery time.Duration

	// src is the library's event stream, taken once in Start, so a test can drive translate with no socket (#899).
	src <-chan lease.Event

	// dropped counts emits discarded because nothing was reading (#899).
	dropped atomic.Uint64

	// view is what the advertisement watch reads, defaulting to c.Lease, a seam for tests (#821).
	view func() (lease.Lease, bool)

	// routerView is the watch's router observation, defaulting to the library client's own (#821).
	routerView func() proto.RouterObservation

	// advert is the last reported advertised configuration, touched only by translate (#821).
	advert      Info
	advertKnown bool
}

// eventBuffer matches the EventBuffer newLibClient asks the library for (#899).
const eventBuffer = 16

// newEventChan builds the channel translate emits on, shared with the test that drives translate (#899).
func newEventChan() chan Event { return make(chan Event, eventBuffer) }

// DroppedEvents is how many translated events were discarded because the plugin side had stopped reading.
func (c *DHCPClient) DroppedEvents() uint64 { return c.dropped.Load() }

// NewDHCPClient prepares a persistent client that opens nothing until Start runs in the sandbox namespace.
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if err := checkRouterAdvertGuardShape(opts, false); err != nil {
		return nil, err
	}
	if opts.V6 {
		params6, err := buildParams6(opts, false)
		if err != nil {
			return nil, err
		}
		copied := *opts
		// No Params snapshot rides a v6 event; see DHCPClientOptions.paramsWritten (#911).
		copied.params6, copied.paramsWritten = params6, true
		return &DHCPClient{iface: iface, opts: copied, params6: params6}, nil
	}
	params, err := buildParams(opts, false)
	if err != nil {
		return nil, err
	}
	copied := *opts
	copied.params, copied.paramsWritten = params, false
	return &DHCPClient{iface: iface, opts: copied, params: params}, nil
}

// Start opens the client in the endpoint's namespace and returns the event channel, closed when the client stops.
func (c *DHCPClient) Start() (chan Event, error) {
	var runner libClient
	if c.opts.V6 {
		client6, err := newLibClient6(c.iface, c.params6, &c.opts)
		if err != nil {
			return nil, err
		}
		c.client6, runner = client6, client6
	} else {
		client, err := newLibClient(c.iface, c.params, &c.opts)
		if err != nil {
			return nil, err
		}
		c.client, runner = client, client
	}
	if c.opts.Records != nil {
		c.manager = c.opts.Records.NewManagerID()
	}

	c.runner = runner

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan error, 1)
	c.events = newEventChan()
	c.src = runner.Events()

	go func() { c.done <- runner.Run(ctx) }()
	go c.translate()

	return c.events, nil
}

// libClient is the family-independent half of the library client, declared by the chassis (#911).
type libClient interface {
	Run(ctx context.Context) error
	Events() <-chan lease.Event
	Lease() (lease.Lease, bool)
	Stats() lease.Stats
}

// translate turns library events into the plugin's, emitting "renew" from Renewed alone and treating ReasonStopped as
// shutdown (#899).
func (c *DHCPClient) translate() {
	defer close(c.events)
	defer func() {
		final := c.Stats()
		c.opts.acdReport(final)
		c.opts.v6ModeReport(final)
		c.opts.v6PrefixReport(final)
		c.opts.routerReport(final)
		c.renewals.report(final, c.opts.OnRenewalStats)
		c.opts.count(c.manager, final)
	}()

	// A renewal request is sent from a retransmission timer and produces no event, so an event-only fold freezes for
	// the whole outage (#940).
	poll := time.NewTicker(c.renewalPoll())
	defer poll.Stop()

	// The advertisement watch runs on v6 only; a v4 lease changes only with a DHCPACK event (RFC 4861, #821).
	raWatch := newStoppedTicker()
	if c.opts.V6 {
		raWatch = time.NewTicker(raWatchInterval)
	}
	defer raWatch.Stop()

	renewedAt := time.Time{}
	for {
		var ev lease.Event
		select {
		case <-poll.C:
			c.renewals.report(c.Stats(), c.opts.OnRenewalStats)
			continue
		case <-raWatch.C:
			// Counted on every advertisement, not only on those that change something (#814).
			c.opts.routerReport(c.Stats())
			if out, ok := c.takeAdvertChange(time.Now()); ok {
				c.deliver(out)
			}
			continue
		case e, ok := <-c.src:
			if !ok {
				return
			}
			ev = e
		}
		now := time.Now()
		// Before the record is written, so a second restart still finds the resolver (#911).
		c.opts.carryResumedConfig6(&ev)
		// Recorded before translation: translateOne drops the coalesced Changed and the stop, and the record must not
		// (#899).
		c.opts.record(ev)
		// One snapshot folded before the cycle reset, so no request in flight is lost (#940).
		stats := c.Stats()
		c.opts.acdReport(stats)
		c.opts.v6ModeReport(stats)
		c.opts.v6PrefixReport(stats)
		c.opts.routerReport(stats)
		c.renewals.report(stats, c.opts.OnRenewalStats)
		c.renewals.cycleEnded(stats)
		c.opts.conflict(ev)

		out, emit, at := translateOne(ev, now, renewedAt, c.opts.MainPrefix6)
		renewedAt = at
		if !emit {
			continue
		}

		// The advertisement baseline is taken only on events that apply configuration (#821).
		if out.Type == "bound" || out.Type == "renew" {
			c.baselineAdvert(now)
		}
		c.deliver(out)
	}
}

// A blocking send wedges translate once the dhcp_manager.go reader has returned on stopChan; a drop loses one event,
// already on the record, and is counted and logged (#899).

// deliver hands one translated event to the plugin, or drops it.
func (c *DHCPClient) deliver(out Event) {
	select {
	case c.events <- out:
	default:
		c.dropped.Add(1)
		log.
			WithField("record", c.opts.RecordID).
			WithField("event", out.Type).
			WithField("dropped_total", c.dropped.Load()).
			Warn("The plugin stopped reading this endpoint's DHCP events; the event was " +
				"dropped. The durable record still has it; the ledger row and counters for " +
				"it are lost.")
	}
}

// newStoppedTicker is a ticker that never fires, since a nil *time.Ticker cannot be selected on or stopped (#821).
func newStoppedTicker() *time.Ticker {
	t := time.NewTicker(time.Hour)
	t.Stop()
	return t
}

// leaseView is what the advertisement watch reads.
func (c *DHCPClient) leaseView() (lease.Lease, bool) {
	if c.view != nil {
		return c.view()
	}
	return c.Lease()
}

// DHCPv6 has no MTU option (RFC 2132 section 5.1 is DHCPv4's), so the RFC 4861 section 4.6.4 MTU arrives only by the
// router observation; on-link prefixes are left out and update only at Join (#821).

// advertRouterView is the router observation the watch reads, carrying only the advertised MTU.
func (c *DHCPClient) advertRouterView() proto.RouterObservation {
	var r proto.RouterObservation
	if c.routerView != nil {
		r = c.routerView()
	} else if c.client6 != nil {
		r = c.client6.Router()
	}
	return proto.RouterObservation{Seen: r.Seen, MTU: r.MTU}
}

// baselineAdvert records the advertised configuration without reporting it, for a caller that has just applied it.
func (c *DHCPClient) baselineAdvert(now time.Time) {
	c.takeAdvertChange(now)
}

// The library reports the latest frame's prefixes, not a union, so on-link determination stays at Join (#821).

// takeAdvertChange reports the advertised configuration when it differs from the last view, never a first sight.
func (c *DHCPClient) takeAdvertChange(now time.Time) (Event, bool) {
	l, ok := c.leaseView()
	if !ok {
		return Event{}, false
	}
	// Rendered with the network's main prefix, as bound and renew are, so the choice is not itself a change (#818).
	info, dropped := infoFromLease(l, c.advertRouterView(), now, c.opts.MainPrefix6)
	first := !c.advertKnown
	same := c.advertKnown && !advertisedDiffers(c.advert, info)
	c.advert, c.advertKnown = info, true
	if first || same {
		return Event{}, false
	}
	return Event{
		Type:                "routeradvert",
		Data:                info,
		UnsafeValuesDropped: dropped,
		RouterFlags:         raFlags(c.RA()),
	}, true
}

// advertisedDiffers compares the five advertisable fields only, not the address that moves on every renewal (#821).
func advertisedDiffers(a, b Info) bool {
	return a.Gateway != b.Gateway ||
		a.MTU != b.MTU ||
		!sameStrings(a.DNSServers, b.DNSServers) ||
		!sameStrings(a.SearchList, b.SearchList) ||
		!sameRoutes(a.Routes, b.Routes)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameRoutes(a, b []Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// translateOne translates one event with a supplied clock, returning the event, whether to emit, and the renewal mark.
func translateOne(ev lease.Event, now, renewedAt time.Time, main netip.Prefix) (Event, bool, time.Time) {
	info, dropped := infoFromLease(ev.Lease, ev.Router, now, main)

	var out Event
	switch ev.Kind {
	case lease.Configured:
		// RFC 9915 section 18.2.6's configuration without an address; it does not clear the outage deadline (D30 Q7,
		// #911).
		cfg, cfgDropped := infoFromConfig(ev.Config)
		dropped = cfgDropped
		out = Event{Type: "config", Data: cfg}
	case lease.Acquired:
		out = Event{Type: "bound", Data: info}
	case lease.Renewed:
		renewedAt = now
		out = Event{Type: "renew", Data: info}
	case lease.Changed:
		if now.Sub(renewedAt) < coalesceWindow {
			// The Renewed for this ACK was already delivered, and "renew" re-applies a changed address (#899).
			return Event{}, false, renewedAt
		}
		out = Event{Type: "renew", Data: info}
	case lease.Lost:
		// ReasonStopped is this process cancelling its own manager, not a lease loss (#899).
		if ev.Reason == proto.ReasonStopped {
			return Event{}, false, renewedAt
		}
		// A conflict is not an outage: "leasefail" feeds dhcp_timeouts, and OnConflict already counted it (RFC 5227,
		// #882).
		if ev.Reason == proto.ReasonConflict {
			return Event{}, false, renewedAt
		}
		// A SLAAC address ending (RFC 4862 section 5.5.3) is not a DHCP outage, so it has its own event type (#818).
		if ev.Lease.SLAAC {
			out = Event{Type: "slaac_lost", Data: info}
			break
		}
		if ev.Reason == proto.ReasonNak {
			out = Event{Type: "nak"}
		} else {
			out = Event{Type: "leasefail"}
		}
	case lease.Failed:
		if ev.Reason == proto.ReasonConflict {
			return Event{}, false, renewedAt
		}
		if ev.Reason == proto.ReasonNak {
			out = Event{Type: "nak"}
		} else {
			out = Event{Type: "leasefail"}
		}
	default:
		return Event{}, false, renewedAt
	}
	out.UnsafeValuesDropped = dropped
	out.RouterFlags = routerFlags(ev.Router)
	return out, true, renewedAt
}

// routerFlags renders the RFC 4861 section 4.2 M and O bits for a log line, "" for neither or for v4.
func routerFlags(r proto.RouterObservation) string { return raFlags(raObservation(r)) }

// raFlags is routerFlags over the chassis's RAObservation.
func raFlags(r RAObservation) string {
	if !r.Seen {
		return ""
	}
	out := ""
	if r.Managed {
		out += "M"
	}
	if r.Other {
		out += "O"
	}
	return out
}

// coalesceWindow is how close a Changed must follow a Renewed to be the same DHCPACK, far above the real gap (#899).
const coalesceWindow = 100 * time.Millisecond

// Measured against dhcp-golib v1.0.0: an advertisement that changes lifetime, MTU, routes or resolvers emits no lease
// event (#821).

// minDelayBetweenRAs is RFC 4861 section 10's "MIN_DELAY_BETWEEN_RAS 3 seconds".
const minDelayBetweenRAs = 3 * time.Second

const raWatchInterval = minDelayBetweenRAs / 4

// Finish stops the client and waits for it to return.
func (c *DHCPClient) Finish(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	return c.Wait(ctx)
}

// Wait waits for a client that is stopping, or has stopped on its own, to return.
func (c *DHCPClient) Wait(ctx context.Context) error {
	if c.done == nil {
		return nil
	}
	select {
	case err := <-c.done:
		c.done = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dhcp: client did not stop: %w", ctx.Err())
	}
}

// Lease is the lease the client currently holds, for the durable record.
func (c *DHCPClient) Lease() (lease.Lease, bool) {
	switch {
	case c.client6 != nil:
		return c.client6.Lease()
	case c.client != nil:
		return c.client.Lease()
	}
	return lease.Lease{}, false
}

// ACDPhase is RFC 5227's phase for the held address, proto.ACDIdle also under conflict_check=off (#882).
func (c *DHCPClient) ACDPhase() proto.ACDPhase {
	if c.client == nil {
		return proto.ACDIdle
	}
	return c.client.ACDPhase()
}

// ErrNoRunningClient is returned by SetHostname when there is no started client to hand the name to.
var ErrNoRunningClient = errors.New("dhcp: no running client to give a hostname to")

// The library renews early to carry the name (RFC 2131 section 4.4.5); option 12 is refused beside option 81 (RFC 4702
// section 3.1); proto.Params6 has no hostname, so v6 is refused (#961).

// SetHostname hands the running client the option-12 name and makes it tell the server at once (#961).
func (c *DHCPClient) SetHostname(name string) error {
	if c.opts.V6 {
		return fmt.Errorf("dhcp: %w", lease.ErrHostnameV6)
	}
	if c.client == nil {
		return ErrNoRunningClient
	}
	return c.client.SetHostname(name)
}

// A v6 client answers the zero mode but runs RFC 4862 DAD, per RFC 9915 section 18.2.10.1, reported on DADPhase (#911).

// ConflictMode is the RFC 5227 mode this client was started in (D23).
func (c *DHCPClient) ConflictMode() proto.ConflictMode { return c.params.Conflict }

// Stats is the manager's counters, the per-endpoint half of the health surface.
func (c *DHCPClient) Stats() lease.Stats {
	if c.runner == nil {
		return lease.Stats{}
	}
	return c.runner.Stats()
}

// DADPhase is RFC 4862 section 5.4's phase for the held address, proto.DADIdle for a v4 client.
func (c *DHCPClient) DADPhase() proto.DADPhase {
	if c.client6 == nil {
		return proto.DADIdle
	}
	return c.client6.DADPhase()
}

// RA is the last router advertisement this client saw, the zero value for a v4 client.
func (c *DHCPClient) RA() RAObservation {
	if c.client6 == nil {
		return RAObservation{}
	}
	return raObservation(c.client6.Router())
}

// NewClient's AF_PACKET socket keeps the namespace of the creating thread, so the thread stays locked (#899).

// newLibClient opens a library client on iface, inside opts.NetNS when one is given.
func newLibClient(iface string, params proto.Params, opts *DHCPClientOptions) (*dhcpruntime.Client, error) {
	cfg := dhcpruntime.ClientConfig{
		Interface: iface,
		Params:    params,
		Resume:    opts.Resume,
		// A dropped Acquired is an address nobody applies (#899).
		EventBuffer: eventBuffer,
	}

	open := func(name string) (*dhcpruntime.Client, error) {
		cfg.Interface = name
		return dhcpruntime.NewClient(cfg)
	}
	abandon := func(client *dhcpruntime.Client) { _ = client.Run(canceledContext()) }

	if opts.NetNS == nil {
		client, _, err := openOnLink(iface, opts.LinkIndex, open, abandon)
		return client, err
	}

	var (
		client *dhcpruntime.Client
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
		return nil, fmt.Errorf("dhcp: open a DHCP client on %v: %w", opened, cerr)
	}
	return client, nil
}

// A client built and then dropped unrun leaks its sockets, so abandon runs in the container's namespace (#911).

// inNetNS runs open with the calling thread inside ns and returns it, calling abandon only if it could not.
func inNetNS(ns netns.NsHandle, open, abandon func()) error {
	runtime.LockOSThread()
	origin, err := netns.Get()
	if err != nil {
		// Nothing was entered, so the thread goes back to the scheduler (#911).
		runtime.UnlockOSThread()
		return fmt.Errorf("dhcp: read the current network namespace: %w", err)
	}
	defer func() { _ = origin.Close() }()

	if err := netns.Set(ns); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("dhcp: enter the endpoint's network namespace: %w", err)
	}

	open()

	if err := netns.Set(origin); err != nil {
		// A thread stranded in the container's namespace stays locked and is retired, costing one OS thread (#899).
		log.WithError(err).Error("Could not return the thread to the plugin's network namespace; it is retired")
		abandon()
		return fmt.Errorf("dhcp: return from the endpoint's network namespace: %w", err)
	}
	runtime.UnlockOSThread()
	return nil
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
