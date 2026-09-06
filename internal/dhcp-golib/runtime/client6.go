package runtime

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// Client6 is ring 3's IPv6 implementations wired into ring 2's manager — the
// DHCPv6 counterpart of Client, and the only type a v6 caller needs.
//
// A SECOND TYPE BESIDE Client AND NOT A MODE OF IT, which is the shape
// lease.Config already chose one ring down: "one Manager is one lease, and a
// dual-stack endpoint is two Managers". The ports differ (TransportV6 and ND
// against Transport and ARP), the journal and the packet ring are typed on
// different state enumerations, and there is a duplicate-address-detection
// runner with no v4 counterpart. A single Client holding both families would
// carry one half nil on every construction and every accessor would have to
// say which half it was answering for.
type Client6 struct {
	mgr       *lease.Manager
	transport *PacketTransportV6
	nd        *NDSocket
	dad       *DADProbe
	timers    *Timers
	journal   *Journal6
	packets   *PacketRingV6
}

// ClientConfig6 configures a Client6.
type ClientConfig6 struct {
	// Interface is the link to lease on.
	Interface string

	// Params6 is the protocol parameter set. Its DUID is REQUIRED and is
	// never filled in from the interface: RFC 9915 section 11 says a DUID
	// "SHOULD NOT change over time if at all possible", and an identity this
	// library invented per interface — or worse, per process — is one that
	// changes whenever the caller's plumbing does. Build one with
	// wire.DUIDLL or wire.DUIDUUID and keep it. See NewClient6.
	Params6 proto.Params6

	// JournalSize and PacketRingSize default to DefaultJournalSize and
	// DefaultPacketRingSize, and are bounded for the reason the v4 ones are.
	JournalSize    int
	PacketRingSize int

	// EventBuffer is the depth of the outward event channel.
	EventBuffer int

	// Resume is a binding this identity held in a PREVIOUS run, and supplying
	// it makes the first message on the wire RFC 9915 section 18.2.12's
	// Confirm instead of a Solicit. See lease.Config.Resume6.
	Resume *lease.Lease
}

// NewClient6 assembles a Client6 on the named interface.
//
// THE CALLING GOROUTINE'S NETWORK NAMESPACE IS THE CLIENT'S, PERMANENTLY, and
// it is the SAME contract NewClient carries, in the same words, because it is
// the same mechanism: a socket in Linux belongs to the network namespace that
// was current in the creating thread at the moment of the socket(2) call. It
// does not follow the thread afterwards and it does not follow the process. A
// caller that locks its goroutine to a thread, enters a namespace with
// setns(2), calls NewClient6 and then leaves has a client whose sockets are
// still where they were made, and Run may then be called from any goroutine on
// any thread.
//
// BOTH SOCKETS AND THE ADDRESS ARE TAKEN HERE, IN THIS CALL, and that is the
// point rather than a convenience — seam row G-8, restated for a family with
// one more socket in it. The DHCPv6 transport, the Neighbor Discovery socket
// and the link-local address that every one of them puts on the wire are read
// in one namespace, so a caller cannot assemble a client whose transport is on
// the container's link and whose Neighbor Discovery is on the host's. The
// symptom of that mistake would be duplicate address detection that never sees
// anything — solicitations on one link and answers on another — which is
// precisely the failure that looks like success. Opening them together makes
// it unconstructible rather than documented.
//
// TestTheV6ClientKeepsTheNamespaceItWasBuiltIn measures it, including the
// control that the interface is invisible from the parent namespace.
//
// THE INTERFACE MUST ALREADY HAVE A SETTLED LINK-LOCAL ADDRESS. See
// InterfaceLinkLocal for why the address is read rather than derived and what
// a tentative one means; the error names which of the two it was.
//
// The order below matters for cleanup: each resource opened is released if a
// later one fails, because a half-constructed Client6 has no Close to call.
func NewClient6(cfg ClientConfig6) (*Client6, error) {
	if cfg.Interface == "" {
		return nil, fmt.Errorf("runtime: no interface named")
	}
	// REFUSED BEFORE A SINGLE SOCKET IS OPENED. proto.New6 would refuse it
	// too, three constructions later, and the client would then have opened
	// and closed two raw sockets to report a fact known from the argument.
	// The message names the two builders, because "required" without them
	// sends the caller looking for a default that must not exist.
	if len(cfg.Params6.DUID) == 0 {
		return nil, fmt.Errorf("runtime: ClientConfig6.Params6.DUID is required and this library will not invent one (RFC 9915 section 11: a DUID \"SHOULD NOT change over time if at all possible\"); build one with wire.DUIDLL or wire.DUIDUUID and keep it: %w", proto.ErrNoDUID)
	}

	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return nil, fmt.Errorf("runtime: interface %q: %w", cfg.Interface, err)
	}

	ent, err := NewEntropy()
	if err != nil {
		return nil, fmt.Errorf("runtime: entropy: %w", err)
	}

	tr, err := NewPacketTransportV6(cfg.Interface)
	if err != nil {
		return nil, err
	}

	// The ND socket is opened HERE, in the same call and therefore in the
	// same namespace as the DHCPv6 one. It is not optional the way the v4 ARP
	// socket is: lease.Config has no ConflictOff for this family, because RFC
	// 4862 section 5.4's check is a MUST rather than a policy, and a client
	// with no ND socket could neither solicit a router nor check an address.
	nd, err := NewNDSocket(cfg.Interface)
	if err != nil {
		_ = tr.Close()
		return nil, err
	}

	// And the runner, which is the ONLY consumer of nd.Frames(). NDSocket
	// drops what nobody drains, so a client that opened the socket and left
	// that channel unread would lose the advertisements the check depends on
	// and report every address free.
	dad, err := NewDADProbe(nd)
	if err != nil {
		_ = nd.Close()
		_ = tr.Close()
		return nil, err
	}

	jsize := cfg.JournalSize
	if jsize == 0 {
		jsize = DefaultJournalSize
	}
	psize := cfg.PacketRingSize
	if psize == 0 {
		psize = DefaultPacketRingSize
	}

	timers := NewTimers()
	c := &Client6{
		transport: tr,
		nd:        nd,
		dad:       dad,
		timers:    timers,
		journal:   NewJournal6(jsize),
		packets:   NewPacketRingV6(psize),
	}

	params := cfg.Params6
	mgr, err := lease.NewManager(lease.Config{
		Params6:     &params,
		Resume6:     cfg.Resume,
		TransportV6: tr,
		ND:          nd,
		DAD:         dad,
		// ONE READING OF THE ADDRESS, handed to both the transport that sends
		// from it and the Router Solicitation that carries it. The transport
		// read it; nothing here reads it again. One fact derived twice gets
		// two answers, and here the two would be the source address of the
		// Solicit and the source address of the Router Solicitation, which a
		// reader of a capture would have no way to reconcile.
		LinkLocal:   tr.Source(),
		LinkHW:      append([]byte(nil), iface.HardwareAddr...),
		Clock:       Clock{},
		Timers:      timers,
		Entropy:     ent,
		Journal6:    c.journal,
		PacketsV6:   c.packets,
		EventBuffer: cfg.EventBuffer,
	})
	if err != nil {
		_ = dad.Close()
		_ = nd.Close()
		_ = tr.Close()
		_ = timers.Close()
		return nil, err
	}
	c.mgr = mgr
	return c, nil
}

// Run drives the client until ctx is cancelled. It closes the probe, both
// sockets and the timers on the way out.
//
// THE PROBE IS CLOSED FIRST, before the socket it reads from: a probe still
// draining nd.Frames() after the socket closed that channel would exit on its
// own, but a probe still IN a Send would be writing to a closed descriptor.
// Closing it first ends every schedule in flight, and a probe cut short
// reports nothing — which is correct, because a cancelled client has no
// acquisition left to answer.
func (c *Client6) Run(ctx context.Context) error {
	defer func() {
		_ = c.dad.Close()
		_ = c.nd.Close()
		_ = c.transport.Close()
		_ = c.timers.Close()
	}()
	return c.mgr.Run(ctx)
}

// Events is the outward lease event stream.
func (c *Client6) Events() <-chan lease.Event { return c.mgr.Events() }

// Lease returns a snapshot of the held lease.
func (c *Client6) Lease() (lease.Lease, bool) { return c.mgr.Lease() }

// Release gives the binding back (RFC 9915 section 18.2.7).
func (c *Client6) Release() { c.mgr.Release() }

// ReportAddressLost tells the client that an address it holds has been
// withdrawn from the interface — RFC 4429 section 3.3's case, where the
// kernel's own duplicate address detection removed an optimistic address under
// a bound lease.
//
// IT IS FOR A CALLER WITH EVIDENCE THIS CLIENT CANNOT SEE, and it stays the
// caller's to produce. The probe here answers one question per
// proto.ActStartDAD and stops; watching an installed address for a later
// conflict would mean watching an address this library did not install, which
// it does not do. See DADProbe's first bound.
func (c *Client6) ReportAddressLost() { c.mgr.ReportAddressLost() }

// DADPhase reports where RFC 4862 section 5.4's duplicate address detection
// stands.
func (c *Client6) DADPhase() proto.DADPhase { return c.mgr.DADPhase() }

// Source is the link-local address this client sends from, and therefore the
// address a server's reply comes back to. See InterfaceLinkLocal.
func (c *Client6) Source() netip.Addr { return c.transport.Source() }

// Stats returns the manager's counters.
func (c *Client6) Stats() lease.Stats { return c.mgr.Stats() }

// Router is the last Router Advertisement ring 1 saw, as RFC 4861 section
// 4.2's two flags.
//
// IT IS A DIAGNOSTIC AND NOT A GATE (design Q2). Nothing in this library waits
// for an advertisement or refuses to run without one; what the observation
// buys the caller is the distinction section 4.2 draws — "If neither M nor O
// flags are set, this indicates that no information is available via DHCPv6"
// — which is what separates a link with no DHCPv6 server from a server that is
// present and not answering. The 1.9.0 plugin could not tell those apart.
func (c *Client6) Router() proto.RouterObservation { return c.mgr.Router() }

// TransportStats returns the DHCPv6 socket's counters. Separate from Stats for
// the reason the v4 pair is separate: the manager counts what it processed and
// the socket counts what arrived and was rejected before it, and a client that
// is seeing nothing is diagnosed by the difference.
func (c *Client6) TransportStats() TransportStatsV6 { return c.transport.Stats() }

// NDStats returns the Neighbor Discovery socket's counters — including the
// frames it refused for RFC 4861 section 6.1.2, each on its own number.
func (c *Client6) NDStats() NDStats { return c.nd.Stats() }

// DADStats returns the duplicate-address-detection runner's counters.
//
// The one to read is OwnIgnored: a run that acquired an address with it at
// zero is a run in which the exclusion of this host's own looped-back
// solicitation never had to work, and the whole check would pass the same way
// if it were removed.
func (c *Client6) DADStats() DADStats { return c.dad.Stats() }

// Journal returns the recorded v6 steps, for proto.Replay6.
func (c *Client6) Journal() []proto.JournalEntry6 { return c.journal.Entries() }

// JournalDropped reports whether the journal ring has wrapped. Non-zero means
// the journal is no longer replayable — see Journal.
func (c *Client6) JournalDropped() int { return c.journal.Dropped() }

// Packets returns the captured v6 packets.
func (c *Client6) Packets() []lease.CapturedPacketV6 { return c.packets.Packets() }
