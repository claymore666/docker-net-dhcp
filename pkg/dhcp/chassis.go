// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Package dhcp is the chassis between the plugin and the DHCP library:
// it owns everything the library must not know — Docker identity, the
// sandbox namespace, the option vocabulary operators type at
// `docker network create` — and nothing about the protocol.
//
// The library performs the whole exchange in-process. There is no
// child process, no configuration file, no hook script and no FIFO,
// which is why the mount-namespace prep, the orphan sweep, the event
// builder and the handler binary that used to live here are gone.
package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
)

// ErrIPv6Unsupported is returned by every entry point in this package
// for an IPv6 request.
//
// The 2.0 beta is IPv4-only: the library implements DHCPv4 and DHCPv6
// is M7. CreateNetwork refuses `ipv6=true` outright, so the only way to
// reach this error is a network that was created by an earlier build
// and survived the upgrade. It is a loud failure on purpose — the
// alternative is a container that starts with no IPv6 address and no
// statement anywhere that it was supposed to have one.
var ErrIPv6Unsupported = errors.New("dhcp: DHCPv6 is not implemented in the 2.0 beta (IPv4-only; IPv6 returns at M7)")

// ErrNoLease is returned when an acquisition ended without one.
var ErrNoLease = errors.New("dhcp: no lease was acquired")

// ErrAddressConflict is an acquisition whose last failure was RFC
// 5227's: the address the server offered is already in use on the
// segment.
//
// A separate error because the operator action is different and the
// two are indistinguishable in a timeout log. "No server answered"
// means the network is broken; this means the DHCP server's pool
// overlaps something it cannot see -- a statically configured host
// inside the range -- and it will hand the same address out again.
var ErrAddressConflict = errors.New("dhcp: the offered address is already in use on this segment")

// DHCPClientOptions is one endpoint's DHCP configuration.
type DHCPClientOptions struct {
	// Hostname is DHCPv4 option 12. Empty omits it.
	Hostname string

	// FQDN, when non-empty, asks the server to register Hostname in DNS
	// (RFC 4702 option 81). The value is the legacy mode string; only
	// its emptiness is read.
	FQDN string

	// V6 selects DHCPv6, which this build does not implement. Every
	// entry point refuses it with ErrIPv6Unsupported.
	V6 bool

	// NetNS is the network namespace to lease in, as an OPEN FILE
	// DESCRIPTOR. nil means the caller's own namespace.
	//
	// It is a descriptor and not a path for the reason it always was:
	// a path is re-resolved by the callee, independently of the
	// caller's own resolution, so a recycled PID between the two lands
	// the socket in a different container (#688). The handle is
	// BORROWED — Start enters it and never closes it.
	NetNS *netns.NsHandle

	// MAC is the endpoint's pinned hardware address. It is the chaddr
	// on the wire and, unless ClientID overrides it, the identity the
	// server files the lease under, so the one-shot acquisition and the
	// persistent client must be given the same one (#152).
	MAC net.HardwareAddr

	// RequestedIP, when non-empty, is option 50 in the DISCOVER: a
	// preference the server MAY ignore (RFC 2131 section 4.4.1). Used
	// for `--ip` and for a tombstone's address.
	RequestedIP string

	// PreferredV6 is the DHCPv6 address hint. Unreachable in this
	// build; kept so the v6 call sites still compile against the
	// refusal rather than being deleted and re-added at M7.
	PreferredV6 string

	// AllowServers and DenyServers restrict which DHCPv4 servers a
	// lease may come from. Evaluated by the library on the server
	// identifier (option 54).
	//
	// THIS IS NOT WHAT dhcpcd DID. dhcpcd's whitelist matched the
	// packet's SOURCE ADDRESS; option 54 is what the server says it
	// is. The two are identical whenever the server answers directly
	// and differ behind a relay, where the source is the relay agent.
	// Option 54 is the correct key — it is what a renewal is unicast
	// to — and the difference is recorded rather than left to be
	// discovered.
	AllowServers []string
	DenyServers  []string

	// ClientID is the option-61 payload WITHOUT its type byte; the
	// chassis prepends type 0 (D10). Empty means no option 61, and the
	// server keys on the chaddr.
	ClientID []byte

	// VendorClass overrides option 60. Empty means VendorID.
	VendorClass string

	// ConflictMode is RFC 5227 conflict detection for this endpoint
	// (D23), from the network's `conflict_check` option. The zero value
	// is proto.ConflictWait, which is both the library's default and
	// the option's.
	//
	// It is the PARSED mode and not the operator's string, so a value
	// that never passed ParseConflictCheck cannot reach the wire: the
	// refusal happens once, at CreateNetwork, and everything after it
	// deals in a type with three inhabitants.
	ConflictMode proto.ConflictMode

	// OnConflict is called once per address conflict this endpoint's
	// client detects, from the manager's own goroutine, with what the
	// event says about it.
	//
	// It exists because the two managers report a conflict on two
	// different paths and the count must not be derived twice. The
	// CreateEndpoint one-shot has no outward event stream at all --
	// GetIP returns a lease or an error -- and the Join manager's
	// stream deliberately drops the conflict (see translateOne), so a
	// counter fed from the plugin's event arm would count half the
	// conflicts and a counter fed from both would count some twice.
	// This is the one route, and both managers take it.
	//
	// nil is the unit-test and probe shape.
	OnConflict func(Conflict)

	// OnACDStats is called with the DELTA in the library's RFC 5227
	// counters since the previous call, so the plugin can hold them
	// process-wide.
	//
	// A DELTA and not a snapshot: the plugin's counters are monotonic
	// across every manager that ever ran, and a manager that exits
	// takes its snapshot with it. Summing live managers instead would
	// make every counter fall when a container stops, which is the one
	// thing a counter may not do.
	//
	// nil is the unit-test and probe shape.
	OnACDStats func(ACDStats)

	// Resume is a lease this identity held in a previous run of the
	// plugin. Supplying it makes the first message on the wire an
	// INIT-REBOOT DHCPREQUEST (RFC 2131 section 4.4.2) instead of a
	// DHCPDISCOVER — the whole of what makes an address survive a
	// plugin restart rather than being re-offered by luck.
	Resume *lease.Lease

	// Records and RecordID are the durable record this manager writes
	// its own events and counters to.
	//
	// THE MANAGER WRITES ITS OWN HALF AND NOTHING ELSE. Which phase the
	// record is in — created, joined, left, retained — is the plugin's
	// decision and is written there; what happened on the wire, and the
	// counters that go with it, are known only here. Splitting it that
	// way is what keeps a manager id unique per MANAGER INSTANCE: the
	// id is minted where the manager is built, so there is no call site
	// that can hand two managers one id.
	//
	// A nil Records writes nothing. That is the unit-test shape, not a
	// production one: an endpoint with no record cannot be resumed
	// after a restart, and the plugin refuses to start without one.
	Records  *Records
	RecordID string

	// paramsWritten is set once the Params snapshot has ridden an
	// event, so the second and later events do not repeat it.
	paramsWritten bool
	params        proto.Params

	// acdSeen is the last ACD counter snapshot handed to OnACDStats,
	// which is what makes that callback a delta rather than a total.
	acdSeen ACDStats
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

// conflict reports one address conflict to the caller, if this event is
// one.
//
// ONE PREDICATE, ONE CALL SITE PER MANAGER. RFC 5227 conflicts leave
// this library as exactly two events -- Failed{ReasonConflict} when
// nothing was held yet (the probe window) and Lost{ReasonConflict} when
// the address was already in use (section 2.4) -- and the library
// guarantees they are exclusive per conflict, so one bump each is one
// bump per conflict. That guarantee is asserted from this side rather
// than assumed: TestConflict_TheLibraryEmitsExactlyOneEventPerConflict
// drives proto.Machine through both cases.
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

// Conflict is one address conflict, as much of it as leaves the
// library.
type Conflict struct {
	// Held says the address was already in use by this endpoint when
	// the conflict was found -- RFC 5227 section 2.4's ongoing check --
	// so the container is about to CHANGE address. False is section
	// 2.1's probe window: nothing was configured, and the container
	// simply gets a different address than it would have.
	//
	// It is the operationally important half of the distinction and it
	// is why the two library events are not folded into one bool here.
	Held bool

	// Addr is the address found in use, and it is EMPTY when Held is
	// false. That is a property of the library rather than an
	// omission: Failed carries no lease, because in the probe window
	// no lease was ever held. The address is in the DHCP server's log
	// as the DHCPDECLINE's, which is the outside evidence anyway.
	Addr string

	// Note is the library's own human-readable line for the event.
	Note string
}

// bareAddr renders a lease's address without its prefix length, or ""
// for a lease that has none.
func bareAddr(l lease.Lease) string {
	if !l.Addr.IsValid() {
		return ""
	}
	return l.Addr.Addr().String()
}

// acdReport hands the caller everything the library's RFC 5227 counters
// have gained since the last call.
//
// Called on every event and once more when the manager ends. The probes
// are sent from a TIMER and not from an event, so between events these
// numbers lag by up to one probe interval; the call after the drain is
// what makes the total exact for a manager that has finished. Stated
// rather than hidden: a live scrape of acd_probes_sent can be one probe
// behind the wire, and no operator decision turns on that.
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

// RAObservation is what a router advertisement told us about a segment.
//
// Nothing in this build observes one: advertisements are IPv6 and the
// beta refuses IPv6. The type survives because the v6 call sites in
// pkg/plugin do, and deleting it would mean deleting and restoring
// those at M7.
type RAObservation struct {
	Seen    bool
	Managed bool
}

// Merge folds another attempt's observation into this one.
func (o RAObservation) Merge(other RAObservation) RAObservation {
	return RAObservation{Seen: o.Seen || other.Seen, Managed: o.Managed || other.Managed}
}

// acquireOutcome is what one lease.Event means to a one-shot
// acquisition: whether the acquisition ENDS here, with what address,
// and what to tell the caller if the deadline ends it instead.
type acquireOutcome struct {
	Info Info
	Done bool
	Err  error
}

// acquireStep decides whether a one-shot acquisition ends on ev.
//
// IT IS A FUNCTION AND NOT THREE LINES INSIDE GetIP's SELECT because
// the rule it carries is the one this milestone turns on and the loop
// around it cannot be driven without a raw socket and a netns: an
// acquisition returns on lease.Acquired and on NOTHING else, in every
// proto.ConflictMode.
//
// A conflict found in RFC 5227 section 2.1's probe window arrives as
// Failed{ReasonConflict}. RFC 2131 section 3.1(5) obliges the
// DHCPDECLINE, and the library sends it, waits section 3.1(5)'s "a
// minimum of ten seconds" and starts again from INIT on its own. The
// only thing returning here would achieve is to fail `docker run` for
// a container the library was about to give a perfectly good second
// address to. GetIP's other select arm -- the deadline -- is what ends
// a hopeless attempt, exactly as it does for a silent server.
//
// Err without Done is deliberate and is the whole shape: it names the
// last real cause so the error the caller finally sees is
// "address conflict" or "acquisition failed: <reason>" rather than
// "context deadline exceeded" alone.
func acquireStep(ev lease.Event, conflicted bool, now time.Time) acquireOutcome {
	switch ev.Kind {
	case lease.Acquired:
		info, _ := infoFromLease(ev.Lease, now)
		return acquireOutcome{Info: info, Done: true}
	case lease.Failed:
		if conflicted {
			return acquireOutcome{Err: fmt.Errorf("%w: %v", ErrAddressConflict, ev.Lease.Addr)}
		}
		return acquireOutcome{Err: fmt.Errorf("dhcp: acquisition failed: %v", ev.Reason)}
	}
	return acquireOutcome{}
}

// GetIP performs one acquisition and returns as soon as a lease exists.
//
// This is the CreateEndpoint path: a link that is still in the host
// namespace, a deadline from lease_timeout, and no interest in what
// happens to the lease afterwards — the record carries it to the Join
// manager, which resumes it as INIT-REBOOT.
//
// The manager is cancelled on the way out, and cancelling makes the
// state machine drop the lease with proto.ReasonStopped. THAT IS NOT A
// LOSS. It is this function's own shutdown reported back to it, and a
// caller that counted it would report a lease loss for every single
// container that started successfully.
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
	var ra RAObservation
	if opts.V6 {
		return Info{}, ra, ErrIPv6Unsupported
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

	// One id for THIS manager instance. The Join manager that follows
	// gets its own from the same mint, which is what stops the record
	// reading the second manager's counters as a continuation of the
	// first's (lease.RecordEvent.Manager).
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
			// A CONFLICT IS NOT THE END OF THIS ACQUISITION, in any
			// mode. RFC 2131 section 3.1(5) obliges the DHCPDECLINE
			// and the library sends it, waits section 3.1(5)'s "a
			// minimum of ten seconds" and starts again from INIT on
			// its own; a chassis that returned here would fail
			// `docker run` for a container the library was about to
			// give a perfectly good second address to. The deadline
			// is what ends the attempt, exactly as it does for a
			// silent server.
			//
			// In proto.ConflictWait that arrives as
			// Failed{ReasonConflict}, because nothing was held yet.
			// In proto.ConflictAsync the address was already handed
			// out, so it arrives as Lost{ReasonConflict} and the
			// caller has by then returned -- this arm is the
			// one-shot's window only.
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
	// Drain IN THE FOREGROUND, and record what is drained.
	//
	// The tail of this manager's life is exactly one event that matters:
	// the Lost{ReasonStopped} the cancel above produces. It is not a
	// lease loss — it is this function's own shutdown reported back —
	// and the fold's OpLost arm is the one place that knows the
	// difference. It has to be on disk BEFORE this function returns,
	// because the Join manager reads the record the moment CreateEndpoint
	// does; a background drain would race it and the resume would
	// sometimes see a lease and sometimes not.
	//
	// Run closes the event channel on its way out, so this terminates.
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

// DHCPClient is the persistent, per-endpoint manager: it runs inside
// the container's namespace for as long as the endpoint is joined, and
// its events drive the address, the routes, resolv.conf, the MTU, the
// audit ledger and the health counters.
type DHCPClient struct {
	iface  string
	opts   DHCPClientOptions
	params proto.Params

	client  *dhcpruntime.Client
	cancel  context.CancelFunc
	done    chan error
	events  chan Event
	manager string

	// src is the library's event stream, taken once in Start. translate
	// ranges over THIS rather than over c.client.Events() so that the
	// goroutine can be driven without a socket: the wedge this field
	// exists for (X-34) is a property of the goroutine and not of
	// translateOne, and a test that cannot start the goroutine cannot
	// see it. c.client stays nil on that path, which Stats() already
	// tolerates.
	src <-chan lease.Event

	// dropped counts emits this client could not hand to the plugin
	// because nothing was reading. See translate.
	dropped atomic.Uint64
}

// eventBuffer is the depth of the channel translate emits on.
//
// DERIVED from the depth the chassis already asked the library for:
// newLibClient sets EventBuffer to the same 16 below, so a burst the
// library was willing to hold is a burst this side can hold too, and a
// smaller number here would start dropping while the library was still
// buffering. (The library's own fallback when nothing is configured is
// 8 — lease/manager.go — so the 16 is this package's choice on both
// sides of the seam, not an inherited default.) The base used 16 here
// for the same reason.
const eventBuffer = 16

// newEventChan builds the channel translate emits on.
//
// A function and not an inline make, because the test that drives
// translate has to obtain its channel from the SAME expression
// production does. MEASURED: while the harness built its own
// `make(chan Event, eventBuffer)`, a mutant that returned Start's
// channel to unbuffered SURVIVED all three tests — they were holding a
// depth they had chosen themselves.
func newEventChan() chan Event { return make(chan Event, eventBuffer) }

// DroppedEvents is how many translated events were discarded because
// the plugin side had stopped reading.
//
// Exported so the drop can be ASSERTED rather than inferred from a log
// line. A silent drop and a wedge look identical from outside the
// package — both produce no event — and the whole of X-34 is that the
// difference matters.
func (c *DHCPClient) DroppedEvents() uint64 { return c.dropped.Load() }

// NewDHCPClient prepares a persistent client. Nothing is opened until
// Start: the socket must be created inside the sandbox namespace, and
// that is a property of the thread Start runs on.
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if opts.V6 {
		return nil, ErrIPv6Unsupported
	}
	params, err := buildParams(opts, false)
	if err != nil {
		return nil, err
	}
	copied := *opts
	copied.params, copied.paramsWritten = params, false
	return &DHCPClient{iface: iface, opts: copied, params: params}, nil
}

// Start opens the client in the endpoint's namespace and begins
// leasing. The returned channel is closed when the client stops.
func (c *DHCPClient) Start() (chan Event, error) {
	client, err := newLibClient(c.iface, c.params, &c.opts)
	if err != nil {
		return nil, err
	}
	c.client = client
	if c.opts.Records != nil {
		c.manager = c.opts.Records.NewManagerID()
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan error, 1)
	c.events = newEventChan()
	c.src = client.Events()

	go func() { c.done <- client.Run(ctx) }()
	go c.translate()

	return c.events, nil
}

// translate turns the library's lease events into the plugin's.
//
// The mapping is one line each except for the two that are not a
// rename:
//
//   - Renewed is the ACK that EXTENDED the lease, and Changed is an
//     ACK whose contents differ. A renewal that also changed something
//     produces BOTH, so emitting "renew" for each would count one
//     renewal twice — in leases_renewed and as two audit rows.
//     "renew" therefore comes from Renewed alone, and Changed emits it
//     only when no renewal accompanied it, which is the re-acquisition
//     case (a NAK, then a different address).
//
//   - Lost carries ReasonStopped when the cause is this process
//     cancelling the manager. That is a shutdown, not a lease loss, and
//     it arrives on every clean Leave.
func (c *DHCPClient) translate() {
	defer close(c.events)
	defer func() {
		final := c.Stats()
		c.opts.acdReport(final)
		c.opts.count(c.manager, final)
	}()

	renewedAt := time.Time{}
	for ev := range c.src {
		now := time.Now()
		// Written before it is translated. The record is the thing a
		// restart reads, and translateOne drops two kinds on the floor
		// deliberately — the coalesced Changed and the stop — neither
		// of which the record may lose.
		c.opts.record(ev)
		c.opts.acdReport(c.Stats())
		c.opts.conflict(ev)

		out, emit, at := translateOne(ev, now, renewedAt)
		renewedAt = at
		if !emit {
			continue
		}

		// THE SEND MUST NOT BLOCK, AND THE LOOP MUST NOT STOP (X-34).
		//
		// The only reader is the per-family goroutine in
		// pkg/plugin/dhcp_manager.go, and its other arm returns on
		// stopChan and never reads this channel again. A bare send here
		// parks this goroutine forever on the first event that arrives
		// in that window — a Leave while a renewal is in flight, a
		// plugin Close over every live endpoint, or the legacy
		// dual-stack path where the v6 client refuses and closes
		// stopChan under a live v4 client.
		//
		// WHICH LOSS THIS CHOOSES, AND WHY. A wedge loses far more than
		// the event that caused it: the range never advances, so every
		// LATER event is lost from the durable record too; deferred
		// close(c.events) never runs, so the reader's own "stream
		// closed" arm never fires; deferred count() never runs, and it
		// is the only writer of this manager's wire counters (P-7's
		// per-endpoint half), so a TICKED parity row silently produces
		// nothing for the endpoint; and the goroutine and its client
		// leak for the life of the daemon. A drop loses exactly one
		// plugin-side event — one ledger row and its counter bumps —
		// and nothing else: c.opts.record(ev) above has ALREADY written
		// this event to the durable record, unconditionally, before the
		// translation, so the record's tail is complete either way. The
		// drop is strictly the smaller loss, and it is the loss the
		// base chose too.
		//
		// WHAT THIS REPLACES. Base pkg/dhcp/client.go:819 made the
		// channel `make(chan Event, 16)` and :839-840 sent through a
		// select/default commented "A full channel drops events rather
		// than blocking the DHCP exchange." The swap deleted both
		// halves and named no replacement. This is that guard,
		// restored, plus the half it never had: the base dropped
		// SILENTLY, so a drop and a wedge were indistinguishable from
		// outside. Every drop is counted on DroppedEvents() and logged
		// at Warn.
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
}

// translateOne is the whole of the event translation, split out from
// the loop above so the two rules that are easiest to break by accident
// can be driven directly.
//
// It returns the event to emit, whether to emit at all, and the updated
// "last renewal" mark. NOTHING here reads a socket or a clock: `now` is
// supplied, which is what lets a test place a Changed inside and
// outside the coalesce window without sleeping.
func translateOne(ev lease.Event, now, renewedAt time.Time) (Event, bool, time.Time) {
	info, dropped := infoFromLease(ev.Lease, now)

	var out Event
	switch ev.Kind {
	case lease.Acquired:
		out = Event{Type: "bound", Data: info}
	case lease.Renewed:
		renewedAt = now
		out = Event{Type: "renew", Data: info}
	case lease.Changed:
		if now.Sub(renewedAt) < coalesceWindow {
			// The Renewed for this same ACK has just been delivered;
			// the plugin re-applies a changed address on "renew"
			// already, so there is nothing left to say.
			return Event{}, false, renewedAt
		}
		out = Event{Type: "renew", Data: info}
	case lease.Lost:
		// THE ONE RULE THAT LOOKS LIKE A MISSING CASE. A Lost carrying
		// ReasonStopped is this process cancelling its own manager --
		// every CreateEndpoint one-shot ends with one, and so does
		// every clean Leave. Emitting it would make a successful
		// container start report a lease loss.
		if ev.Reason == proto.ReasonStopped {
			return Event{}, false, renewedAt
		}
		// A CONFLICT IS NOT A LEASE FAILURE AND MUST NOT BE ONE.
		// "leasefail" is what feeds dhcp_timeouts through
		// countOutageTick, and dhcp_timeouts means the DHCP server
		// went quiet -- which is exactly what has NOT happened here:
		// the server answered, the address it named is occupied, and
		// the library is already declining it and asking for another.
		// Counting it as an outage would make a squatted pool
		// indistinguishable from a dead server in the one counter an
		// operator alerts on.
		//
		// Nothing else is lost by dropping it. The conflict is counted
		// through DHCPClientOptions.OnConflict, which the one-shot
		// takes too; the event is already on the durable record,
		// unconditionally, before this function is called; and the
		// address change the library then wins arrives as the ordinary
		// Acquired -> "bound" that reconfigures the container. The
		// existing Lost -> re-acquire path is the whole handling.
		if ev.Reason == proto.ReasonConflict {
			return Event{}, false, renewedAt
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
	return out, true, renewedAt
}

// coalesceWindow is how close a Changed must follow a Renewed to be
// read as the same DHCPACK.
//
// The library emits both from one action batch, in the same iteration
// of the manager's loop, so the real gap is a channel send. The window
// is generous by three orders of magnitude because being late costs one
// duplicated audit row and being early costs a lost re-acquisition
// event, and only one of those is a lease the container is not using.
const coalesceWindow = 100 * time.Millisecond

// Finish stops the client and waits for it to return.
func (c *DHCPClient) Finish(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	return c.Wait(ctx)
}

// Wait waits for a client that is stopping, or has stopped on its own,
// to return.
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

// Lease is the lease the client currently holds, for the durable
// record.
func (c *DHCPClient) Lease() (lease.Lease, bool) {
	if c.client == nil {
		return lease.Lease{}, false
	}
	return c.client.Lease()
}

// ACDPhase is where RFC 5227 has got to for the address this client
// holds. proto.ACDIdle for a client that is not running, which is also
// the answer in conflict_check=off -- read it beside ConflictMode,
// never alone.
func (c *DHCPClient) ACDPhase() proto.ACDPhase {
	if c.client == nil {
		return proto.ACDIdle
	}
	return c.client.ACDPhase()
}

// ConflictMode is the RFC 5227 mode this client was started in (D23).
// Read from the params the client was built with rather than from the
// network's stored options, so it is the mode in force and not the
// mode the options would resolve to now.
func (c *DHCPClient) ConflictMode() proto.ConflictMode { return c.params.Conflict }

// Stats is the manager's counters, which are the per-endpoint half of
// the health surface (P-7).
func (c *DHCPClient) Stats() lease.Stats {
	if c.client == nil {
		return lease.Stats{}
	}
	return c.client.Stats()
}

// newLibClient opens a library client on iface, inside opts.NetNS when
// one is given.
//
// THE NAMESPACE IS THE THREAD'S, AND THE SOCKET KEEPS IT. The library's
// contract is explicit: NewClient's AF_PACKET socket belongs to the
// network namespace current in the creating thread at the socket(2)
// call, permanently, and the interface name is resolved there too. So
// the goroutine is locked to its thread for the whole of the entry,
// the call and the return — and is NOT unlocked afterwards on the
// failure path back out, because a thread that could not be returned to
// the original namespace must not be handed back to the scheduler.
func newLibClient(iface string, params proto.Params, opts *DHCPClientOptions) (*dhcpruntime.Client, error) {
	cfg := dhcpruntime.ClientConfig{
		Interface: iface,
		Params:    params,
		Resume:    opts.Resume,
		// Deep enough that a plugin busy elsewhere cannot make the
		// manager drop an event on the floor; the manager counts a
		// drop, but a dropped Acquired is an address nobody applies.
		EventBuffer: eventBuffer,
	}

	if opts.NetNS == nil {
		return dhcpruntime.NewClient(cfg)
	}

	runtime.LockOSThread()
	origin, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("dhcp: read the current network namespace: %w", err)
	}
	defer func() { _ = origin.Close() }()

	if err := netns.Set(*opts.NetNS); err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("dhcp: enter the endpoint's network namespace: %w", err)
	}

	client, cerr := dhcpruntime.NewClient(cfg)

	if err := netns.Set(origin); err != nil {
		// The thread is stranded in the container's namespace. Leaving
		// it locked takes it out of the scheduler's rotation for the
		// life of the process, which costs one OS thread; unlocking it
		// would hand a namespace-contaminated thread to unrelated
		// goroutines, which costs correctness everywhere.
		log.WithError(err).Error("Could not return the thread to the plugin's network namespace; it is retired")
		if client != nil {
			_ = client.Run(canceledContext())
		}
		return nil, fmt.Errorf("dhcp: return from the endpoint's network namespace: %w", err)
	}
	runtime.UnlockOSThread()

	if cerr != nil {
		return nil, fmt.Errorf("dhcp: open a DHCP client on %v: %w", iface, cerr)
	}
	return client, nil
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
