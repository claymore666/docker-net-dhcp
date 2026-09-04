package runtime

import (
	"context"
	"fmt"
	"net"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// Client is ring 3's implementations wired into ring 2's manager — the only
// type a caller needs in the ordinary case.
//
// A thin assembly on purpose: all it does is choose which implementation goes
// into which port, and every choice is replaceable. The same manager runs
// against fakes with no root and no network, which is what makes the
// acquisition path table-testable.
type Client struct {
	mgr       *lease.Manager
	transport *PacketTransport
	arp       *ARPSocket
	timers    *Timers
	journal   *Journal
	packets   *PacketRing
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// Interface is the link to lease on.
	Interface string

	// Params is the protocol parameter set. If CHAddr is empty it is filled
	// from the interface's hardware address.
	Params proto.Params

	// JournalSize and PacketRingSize default to DefaultJournalSize and
	// DefaultPacketRingSize. Both are bounded (R3); see those types for what a
	// wrap costs.
	JournalSize    int
	PacketRingSize int

	// EventBuffer is the depth of the outward event channel.
	EventBuffer int

	// Resume is a lease this identity held in a PREVIOUS run of this client,
	// and supplying it makes the first message on the wire RFC 2131 section
	// 4.4.2's INIT-REBOOT DHCPREQUEST instead of a DHCPDISCOVER. See
	// lease.Config.Resume for the rules; lease.Record.Resume is where one
	// comes from, and lease.Record.Prefer is what to do when it says no.
	Resume *lease.Lease
}

// NewClient assembles a Client on the named interface.
//
// THE CALLING GOROUTINE'S NETWORK NAMESPACE IS THE CLIENT'S, PERMANENTLY.
// This call opens an AF_PACKET socket, and a socket in Linux belongs to the
// network namespace that was current in the CREATING THREAD at the moment of
// the socket(2) call. It does not follow the thread afterwards and it does not
// follow the process: a caller that locks its goroutine to a thread, enters a
// namespace with setns(2), calls NewClient and then leaves that namespace has
// a Client whose socket is still in the namespace it was created in, and Run
// may be called from any goroutine on any thread thereafter.
//
// That is the whole of the contract, and it is what makes one process able to
// lease on many containers' interfaces at once: the client, not the caller,
// carries the namespace. It also means the interface name is resolved THERE —
// cfg.Interface and the hardware address it fills in are looked up in the
// calling goroutine's namespace, so a name that exists in both namespaces
// resolves to the one the caller was standing in.
//
// TestTheClientKeepsTheNamespaceItWasBuiltIn measures it, including the
// control that the interface is invisible from the parent namespace.
//
// The order below matters for cleanup: each resource opened is released if a
// later one fails, because a half-constructed Client has no Close to call.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Interface == "" {
		return nil, fmt.Errorf("runtime: no interface named")
	}
	if len(cfg.Params.CHAddr) == 0 {
		iface, err := net.InterfaceByName(cfg.Interface)
		if err != nil {
			return nil, fmt.Errorf("runtime: interface %q: %w", cfg.Interface, err)
		}
		cfg.Params.CHAddr = append([]byte(nil), iface.HardwareAddr...)
	}

	ent, err := NewEntropy()
	if err != nil {
		return nil, fmt.Errorf("runtime: entropy: %w", err)
	}

	tr, err := NewPacketTransport(cfg.Interface)
	if err != nil {
		return nil, err
	}

	// The ARP socket is opened HERE, in the same call and therefore in the
	// same network namespace as the DHCP one — seam row G-8. A caller that
	// opened it separately, after leaving the container's namespace, would get
	// a client whose two sockets were on two different links, and the symptom
	// would be conflict detection that never sees anything: probes on one link
	// and answers on another. Opening both together is what makes that
	// unconstructible rather than documented.
	//
	// Not opened at all when the caller asked for no conflict detection. There
	// is nothing to listen on then, and a socket held open for a check nobody
	// runs is a capability this process did not need.
	var arp *ARPSocket
	if cfg.Params.Conflict != proto.ConflictOff {
		arp, err = NewARPSocket(cfg.Interface)
		if err != nil {
			_ = tr.Close()
			return nil, err
		}
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
	c := &Client{
		transport: tr,
		arp:       arp,
		timers:    timers,
		journal:   NewJournal(jsize),
		packets:   NewPacketRing(psize),
	}

	mgr, err := lease.NewManager(lease.Config{
		Params:      cfg.Params,
		Resume:      cfg.Resume,
		Transport:   tr,
		Clock:       Clock{},
		ARP:         arpPort(arp),
		Timers:      timers,
		Entropy:     ent,
		Journal:     c.journal,
		Packets:     c.packets,
		EventBuffer: cfg.EventBuffer,
	})
	if err != nil {
		_ = tr.Close()
		if arp != nil {
			_ = arp.Close()
		}
		_ = timers.Close()
		return nil, err
	}
	c.mgr = mgr
	return c, nil
}

// arpPort is the nil dance a typed-nil interface would otherwise get wrong.
//
// A (*ARPSocket)(nil) stored in a lease.ARP is NOT nil, so assigning the
// pointer straight into the Config would defeat NewManager's ErrNoARP check
// and produce a client that dereferenced nil on its first probe. This is the
// one place the conversion happens, so there is one place to get it right.
func arpPort(s *ARPSocket) lease.ARP {
	if s == nil {
		return nil
	}
	return s
}

// Run drives the client until ctx is cancelled. It closes the transport and
// the timers on the way out.
func (c *Client) Run(ctx context.Context) error {
	defer func() {
		_ = c.transport.Close()
		if c.arp != nil {
			_ = c.arp.Close()
		}
		_ = c.timers.Close()
	}()
	return c.mgr.Run(ctx)
}

// Events is the outward lease event stream.
func (c *Client) Events() <-chan lease.Event { return c.mgr.Events() }

// Lease returns a snapshot of the held lease.
func (c *Client) Lease() (lease.Lease, bool) { return c.mgr.Lease() }

// Release gives the lease back (RFC 2131 section 4.4.6). See Manager.Release
// for what it does not promise.
func (c *Client) Release() { c.mgr.Release() }

// ReportConflict says the held address is in use, which obliges a DHCPDECLINE
// (RFC 2131 section 3.1(5)).
//
// It is for a caller with evidence this client cannot see. Since M6 the client
// runs RFC 5227 itself unless Params.Conflict is proto.ConflictOff, and this
// stays available in that mode too. See Manager.ReportConflict.
func (c *Client) ReportConflict() { c.mgr.ReportConflict() }

// ACDPhase reports where RFC 5227's conflict check stands, and is
// proto.ACDIdle for a client running with proto.ConflictOff.
func (c *Client) ACDPhase() proto.ACDPhase { return c.mgr.ACDPhase() }

// ARPStats returns the ARP socket's counters, and the zero value for a client
// running with proto.ConflictOff — which has no such socket. Present is false
// in exactly that case, and it is the only way to tell "there is no ARP port"
// from "the port has read nothing".
//
// Separate from Stats for the reason TransportStats is: the socket counts what
// arrived, the manager counts what it acted on, and a client that saw no
// conflict is diagnosed by the difference.
func (c *Client) ARPStats() ARPStats {
	if c.arp == nil {
		return ARPStats{}
	}
	return c.arp.Stats()
}

// Stats returns the manager's counters.
func (c *Client) Stats() lease.Stats { return c.mgr.Stats() }

// TransportStats returns the socket's counters. Separate from Stats because
// they answer different questions: the manager counts what it processed, the
// transport counts what arrived and was rejected before it. A client that is
// seeing nothing is diagnosed by the difference between them.
func (c *Client) TransportStats() TransportStats { return c.transport.Stats() }

// Journal returns the recorded steps (G2, G6).
func (c *Client) Journal() []proto.JournalEntry { return c.journal.Entries() }

// JournalDropped reports whether the journal ring has wrapped. Non-zero means
// the journal is no longer replayable — see Journal.
func (c *Client) JournalDropped() int { return c.journal.Dropped() }

// Packets returns the captured packets (G1).
func (c *Client) Packets() []lease.CapturedPacket { return c.packets.Packets() }
