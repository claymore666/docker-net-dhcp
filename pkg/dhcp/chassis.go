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

	// Broadcast asks for an L2-broadcast reply (the BROADCAST flag,
	// RFC 2131 section 2).
	Broadcast bool

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
			switch ev.Kind {
			case lease.Acquired:
				info, _ = infoFromLease(ev.Lease, time.Now())
				got = true
			case lease.Failed:
				// Not terminal by itself: the machine keeps trying
				// and the deadline above is what ends the attempt.
				// Recorded so the error the caller sees names the
				// last real cause rather than "context deadline
				// exceeded" alone.
				lastE = fmt.Errorf("dhcp: acquisition failed: %v", ev.Reason)
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
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		log.WithError(err).WithField("iface", iface).Debug("Acquisition manager returned an error")
	}
	opts.count(manager, client.Stats())

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
}

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
	c.events = make(chan Event)

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
	defer func() { c.opts.count(c.manager, c.Stats()) }()

	renewedAt := time.Time{}
	for ev := range c.client.Events() {
		now := time.Now()
		// Written before it is translated. The record is the thing a
		// restart reads, and translate() drops two kinds on the floor
		// deliberately — the coalesced Changed and the stop — neither
		// of which the record may lose.
		c.opts.record(ev)
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
				// The Renewed for this same ACK has just been
				// delivered; the plugin re-applies a changed address
				// on "renew" already, so there is nothing left to say.
				continue
			}
			out = Event{Type: "renew", Data: info}
		case lease.Lost:
			if ev.Reason == proto.ReasonStopped {
				continue
			}
			if ev.Reason == proto.ReasonNak {
				out = Event{Type: "nak"}
			} else {
				out = Event{Type: "leasefail"}
			}
		case lease.Failed:
			if ev.Reason == proto.ReasonNak {
				out = Event{Type: "nak"}
			} else {
				out = Event{Type: "leasefail"}
			}
		default:
			continue
		}
		out.UnsafeValuesDropped = dropped
		c.events <- out
	}
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
		EventBuffer: 16,
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
