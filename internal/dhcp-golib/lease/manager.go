package lease

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// Config is everything the Manager needs. Every effect is an interface, so the
// whole acquisition path runs with no root, no namespace and no network when
// the fakes are supplied.
type Config struct {
	Params    proto.Params
	Transport Transport
	Clock     Clock
	Timers    Timers
	Entropy   Entropy

	// ARP is the link's ARP traffic, and it is REQUIRED unless
	// Params.Conflict is proto.ConflictOff.
	//
	// Required rather than optional because the alternative is a client that
	// believes it is checking for conflicts and is not: RFC 5227 section 2.1's
	// check is the default (proto.ConflictWait is the zero value), so a
	// caller that simply did not know about this field would otherwise get
	// silence where it had been promised a DHCPDECLINE. NewManager returns
	// ErrNoARP instead.
	ARP ARP

	// Journal and Packets are optional. A nil Journal means transitions are
	// not recorded and Replay has nothing to replay; a nil PacketRing means
	// no packet capture. Both default to a discarding implementation rather
	// than to a nil dereference.
	Journal Journal
	Packets PacketRing

	// EventBuffer is the depth of the outward event channel. Zero means a
	// small default.
	EventBuffer int

	// Resume is a lease this identity held in a PREVIOUS run of this client,
	// and supplying it is what turns the first message on the wire into RFC
	// 2131 section 4.4.2's INIT-REBOOT DHCPREQUEST instead of a
	// DHCPDISCOVER. Record.Resume is where one comes from.
	//
	// Only Addr and Expire are read. Everything else in the Lease — the
	// gateway, the DNS servers, and in particular the ServerID — is
	// deliberately dropped on the way in: section 4.3.2 has a server treat a
	// DHCPREQUEST carrying a server identifier as a SELECTING one and stay
	// SILENT when the identifier is not its own, so a remembered server
	// identifier put on this wire is a hang rather than a wrong answer.
	//
	// A zero Expire is an infinite lease and always qualifies. An expiry
	// already past at NewManager is not an error: the machine journals it and
	// acquires from INIT, because that is what a client with an expired lease
	// should do and refusing to start would be worse.
	//
	// It is CONSUMED BY ONE ATTEMPT. Whatever ends the INIT-REBOOT —
	// a DHCPNAK, an exhausted retransmission budget, a link that dropped —
	// the acquisition that follows is an ordinary DHCPDISCOVER, which is RFC
	// 2131 section 3.2(3)'s "(non-abbreviated) procedure".
	Resume *Lease

	// ------------------------------------------------------------ IPv6 --
	//
	// SETTING Params6 IS WHAT MAKES THIS A v6 MANAGER, and it is the only
	// thing that does. A Config with Params6 set runs proto.Machine6 over
	// TransportV6 and ND; a Config without it runs proto.Machine over
	// Transport and ARP, exactly as before. The two are not mixed in one
	// Manager: one Manager is one lease, and a dual-stack endpoint is two
	// Managers — which is also what makes the section 14.1 bucket's
	// per-interface bound checkable, since the v6 one has a bucket and the v4
	// one has nothing to limit.
	Params6 *proto.Params6

	// TransportV6 and ND are the v6 ports, required when Params6 is set.
	//
	// ND is required rather than optional for ErrNoARP's reason: RFC 4861
	// section 6.3.7's Router Solicitation and RFC 4862 section 5.4's
	// duplicate address detection both need frames on the link, and a client
	// that silently ran neither would acquire, bind and look healthy while
	// skipping the one check RFC 9915 section 18.2.10.1 makes a MUST.
	TransportV6 TransportV6
	ND          ND

	// LinkLocal and LinkHW are this interface's own IPv6 source address and
	// hardware address, used to build RFC 4861 section 4.1's Router
	// Solicitation and nothing else.
	//
	// THEY ARE HERE AND NOT IN Params6 BECAUSE RING 1 NEVER READS THEM. The
	// machine emits proto.ActSendRouterSolicit and this ring renders it;
	// putting the interface's own addresses in the pure machine's parameters
	// would give ring 1 two fields it has no use for and no test could
	// exercise.
	//
	// THE ZERO VALUES ARE CONFORMANT, not a mistake. Section 4.1: the source
	// is "An IP address assigned to the sending interface, or the unspecified
	// address if no address is assigned to the sending interface", and the
	// Source Link-Layer Address option "MUST NOT be included if the Source
	// Address is the unspecified address." A caller that supplies neither
	// sends exactly that message, which is what a host does before SLAAC has
	// given it a link-local address.
	LinkLocal netip.Addr
	LinkHW    []byte

	// DAD is what performs RFC 4862 section 5.4's duplicate address
	// detection when the machine asks for it. It is optional and the nil
	// value is the behaviour that shipped before the port existed; see
	// DADRunner and the ActStartDAD arm below.
	DAD DADRunner

	// Journal6 and PacketsV6 are the v6 halves of Journal and Packets, and
	// they are optional the same way.
	Journal6  Journal6
	PacketsV6 PacketRingV6

	// RateLimit is RFC 9915 section 14.1's token bucket, on the v6 path only.
	// The zero value is section 14.1's own suggested default (20 messages in
	// 20 seconds); a negative Messages turns it off. See RateLimit.
	RateLimit RateLimit

	// Resume6 is a v6 lease this identity held in a PREVIOUS run, and
	// supplying it is what turns the first message on the wire into RFC 9915
	// section 18.2.3's Confirm instead of a Solicit.
	//
	// Addr, Preferred, Valid, Renew, Rebind and ServerDUID are read; the
	// wall-clock deadlines are converted to the REMAINING durations ring 1
	// works in, once, at construction, through the same clock bridge
	// Config.Resume crosses. A deadline already past contributes zero, which
	// makes the resumed lease one the machine will renew immediately rather
	// than one it thinks is live.
	//
	// UNLIKE Config.Resume, THE SERVER IDENTIFIER IS KEPT. RFC 2131 section
	// 4.3.2 makes a remembered server identifier in a v4 DHCPREQUEST a hang;
	// section 18.2.3's Confirm carries no Server Identifier at all, so there
	// is nothing to put on the wire and the DUID is only used afterwards, to
	// address the Renew that follows a successful confirmation.
	Resume6 *Lease
}

// Manager runs one managed lease.
//
// It is the serialisation point: one event at a time, with the whole action
// list drained before the next Step. Nothing else in the library takes that
// responsibility, and the design document names the alternative as a source
// of heisenbugs (section 2.4 item 2).
type Manager struct {
	cfg     Config
	machine *proto.Machine
	journal Journal
	packets PacketRing

	// machine6 is the v6 machine, and it is non-nil exactly when machine is
	// nil: one Manager runs one lease in one family. Every place that has to
	// know which reads this field, so "which family is this" has one
	// derivation rather than a flag that can disagree with the machine.
	machine6 *proto.Machine6
	journal6 Journal6
	packets6 PacketRingV6

	// limiter is RFC 9915 section 14.1's bucket. Nil on the v4 path, which is
	// what "the v4 path is untouched" means in code.
	limiter *bucket

	events chan Event

	// requests carries the caller's own events — Release and ReportConflict —
	// into Run's loop, because the Machine is owned by that goroutine and a
	// method that touched it directly would race every Step.
	requests chan proto.Event

	// stopping is set on the Run goroutine before the shutdown Stop is
	// dispatched, and is read only there. It switches emit from blocking to
	// best-effort — see emit for why that is required and why it does not
	// lose the final event.
	stopping bool

	mu    sync.Mutex
	lease Lease
	held  bool
	seq   uint64

	// acd is the last ACD phase read off the machine, kept under mu because
	// ACDPhase is called by the caller's goroutine and the machine belongs to
	// Run's. It is written once per Step, from Run's goroutine, which is the
	// only place the machine is touched.
	acd proto.ACDPhase

	// dad, router and config are the v6 counterparts, kept under mu for the
	// same reason and written from the same goroutine.
	dad    proto.DADPhase
	router proto.RouterObservation
	config Configuration

	// stats are counters the plugin's Health RPC will need (constraint C5).
	// They are a fold over what actually happened rather than increments
	// scattered at call sites — the shape the 1.x antipattern list names as
	// having produced counters that could be deleted with the suite still
	// green.
	stats Stats
}

// Stats are the counters this manager produces.
//
// WHEN A COUNTER IS CURRENT. Every counter an event reports is bumped BEFORE
// that event is emitted, so a caller that reads Stats on receiving an event
// sees that event accounted for: LeasesAcquired at Acquired, RenewalsCompleted
// at Renewed, LeasesLost at Lost, AcquireFailures and NaksAccepted at Failed.
// Two tests hold this. One drives every event kind and reads the counters at
// the instant each event arrives; the other reads this file and refuses a
// counter written below an emit, which is where the order is decidable — the
// runtime race is nanoseconds wide, and a test cannot win it reliably.
//
// What the contract does NOT say is that a counter belonging to a LATER event
// is current: one Step can produce several events, and each is emitted as its
// own action is executed. A DHCPNAK that costs a lease produces ActLeaseLost
// and then ActFailed, in that order and for a reason ring 1 is explicit about
// — a caller tears the interface down when it sees the loss — so at the Lost
// event NaksAccepted has not been bumped yet. A caller (or a test) that wants
// that number waits for the Failed event, which is the event that reports it.
//
// The alternative — accounting a whole Step before executing it, so any event
// in it sees the final numbers — was considered and rejected in review round
// 4: Sent and SendFailures are outcomes, not intentions, and a pre-pass would
// have to count sends that then fail.
type Stats struct {
	Steps            uint64
	Sent             uint64
	SendFailures     uint64
	Received         uint64
	DecodeFailures   uint64
	TransportErrors  uint64
	LeasesAcquired   uint64
	LeasesLost       uint64
	AcquireFailures  uint64
	TimerFires       uint64
	EventsDropped    uint64
	ActionsExecuted  uint64
	ActionsFailedFed uint64
	DeclinesSent     uint64
	ReleasesSent     uint64
	RequestsDropped  uint64

	// RenewalsSent counts DHCPREQUESTs sent to extend a held lease, in
	// RENEWING and REBINDING together, retransmissions included.
	// RenewalsCompleted counts the DHCPACKs that ended one. The difference is
	// how hard this client is working to keep its address.
	RenewalsSent      uint64
	RenewalsCompleted uint64

	// NaksSeen counts every DHCPNAK that decoded, NaksAccepted the ones the
	// machine acted on. TWO COUNTERS, because their difference is the
	// diagnostic: a NAK discarded for a stale xid, a foreign chaddr or a
	// server outside Params.Servers is invisible in either number alone, and
	// on a LAN with two DHCP servers it is the number that explains the
	// behaviour.
	NaksSeen     uint64
	NaksAccepted uint64

	// The RFC 5227 counters (P-7).
	//
	// ConflictsDetected counts addresses this client found in use — by the
	// conflict rules or by a caller's ReportConflict — each exactly once,
	// whichever path reported it. It is bumped at the event that reports the
	// conflict: Failed{ReasonConflict} for one found before the address was
	// ever used, Lost{ReasonConflict} for one found afterwards. The two are
	// never both emitted for one conflict, which is what makes "exactly once"
	// true rather than approximately true.
	ConflictsDetected uint64

	// ProbesSent and AnnouncementsSent count the two kinds of ARP packet RFC
	// 5227 defines, read off the packet that actually left the host rather
	// than off the machine's intention — the same rule countSent follows, and
	// the reason a probe that failed to send is in ARPSendFailures and in
	// neither of these.
	ProbesSent        uint64
	AnnouncementsSent uint64
	ARPSendFailures   uint64

	// ARPSeen is every frame the link delivered; ARPIgnored the ones the
	// relevance filter dropped before ring 1 saw them; ARPDecodeFailures the
	// ones that were not an Ethernet/IPv4 ARP packet at all.
	//
	// THREE COUNTERS BECAUSE THEIR DIFFERENCES ARE THE DIAGNOSTIC. Seen minus
	// ignored minus decode failures is what reached the conflict rules, and on
	// a link where a conflict was missed that number is the first thing to
	// look at: a zero says the socket saw nothing worth showing, and a large
	// one with no conflict says the rules looked and disagreed.
	ARPSeen           uint64
	ARPIgnored        uint64
	ARPDecodeFailures uint64
	ARPErrors         uint64

	// The IPv6 counters. They are zero for the whole life of a v4 manager,
	// which is the truth: that client sends no Neighbor Discovery and has no
	// section 14.1 bucket.
	//
	// RateLimited counts messages RFC 9915 section 14.1's bucket refused. It
	// is the number that separates a server that is not answering from a
	// client talking too fast to be allowed to ask: both look like silence
	// on the wire, and only one of them is the network's fault.
	RateLimited uint64

	// NDSeen is every Neighbor Discovery frame the link delivered and
	// NDIgnored the ones this ring dropped — a Neighbor Solicitation or
	// Advertisement, which belong to ring 3's duplicate address detection, or
	// a frame that was not a valid ICMPv6 Neighbor Discovery message at all.
	// Their difference is what reached ring 1, which on a link where a Router
	// Advertisement was missed is the first number to look at: a zero NDSeen
	// says the socket saw nothing, and a large NDIgnored beside it says the
	// frames arrived and none of them was an advertisement.
	//
	// THE TWO CAUSES OF AN IGNORE ARE NOT SEPARATED, deliberately: telling
	// them apart needs a second decode of a frame this ring drops either way,
	// and what distinguishes them is the type octet in the frame the caller
	// still has.
	NDSeen         uint64
	NDIgnored      uint64
	NDErrors       uint64
	NDSendFailures uint64

	// RouterSolicitsSent counts RFC 4861 section 6.3.7's solicitations that
	// left the host; RouterAdvertsSeen the advertisements that reached ring 1.
	RouterSolicitsSent uint64
	RouterAdvertsSeen  uint64

	// DADChecksStarted counts addresses handed to ring 3 for RFC 4862 section
	// 5.4, and DADConflicts the ones that came back in use. Their difference
	// is not "how many passed": a check whose result never arrives is in
	// neither, which is what AcquireFailures with proto.ReasonDADIncomplete
	// reports.
	DADChecksStarted uint64
	DADConflicts     uint64

	// ConfiguredEvents counts RFC 9915 section 18.2.6's stateless answers.
	ConfiguredEvents uint64
}

// ErrNoTransport and friends are returned by NewManager for a Config that
// cannot work. They are distinct values so a caller can tell a programming
// error from a runtime one.
var (
	ErrNoTransport = errors.New("lease: Config.Transport is required")
	ErrNoClock     = errors.New("lease: Config.Clock is required")
	ErrNoTimers    = errors.New("lease: Config.Timers is required")
	ErrNoEntropy   = errors.New("lease: Config.Entropy is required")

	// ErrNoARP is a Config with conflict detection on and no ARP port.
	//
	// It is an error and not a silent downgrade to proto.ConflictOff, because
	// the downgrade is invisible: the client acquires, binds and looks
	// healthy, and the one thing it does not do is the thing it was
	// configured to do. See Config.ARP.
	ErrNoARP = errors.New("lease: Config.ARP is required unless Params.Conflict is proto.ConflictOff")

	// ErrResumeTwice is Config.Resume and Config.Params.Resume both set.
	//
	// ONE FACT, ONE DERIVATION. They are two spellings of the same thing on
	// two clocks, and the pair cannot be checked for agreement here: the
	// monotonic one names an epoch this process cannot compare against a wall
	// clock without assuming the very conversion that is in question. Silently
	// preferring one would make the answer depend on which field a caller
	// happened to fill.
	ErrResumeTwice = errors.New("lease: set Config.Resume or Config.Params.Resume, not both")

	// ErrResumeNoAddr is a Config.Resume whose Addr is not a usable IPv4
	// address. Refused rather than ignored: a caller that meant to keep an
	// address and passed an empty Lease would otherwise get the plain
	// DHCPDISCOVER it was trying to replace, with nothing to read that says
	// so.
	ErrResumeNoAddr = errors.New("lease: Config.Resume.Addr is not a usable IPv4 address")

	// ErrBothFamilies is Config.Params6 set beside a v4 field that only the
	// v4 path reads. One Manager is one lease in one family; a Config that
	// filled both halves would leave the unused half silently dead, which is
	// the shape a caller cannot see from the outside.
	ErrBothFamilies = errors.New("lease: Config.Params6 is set beside the v4 Transport, ARP or Resume: one manager runs one family")

	// ErrNoTransportV6 and ErrNoND are the v6 ports, required when
	// Config.Params6 is set. ErrNoND is an error and not a silent downgrade
	// for ErrNoARP's reason: RFC 9915 section 18.2.10.1 makes duplicate
	// address detection a MUST, and a client with nowhere to send a Neighbor
	// Solicitation cannot perform it.
	ErrNoTransportV6 = errors.New("lease: Config.TransportV6 is required when Config.Params6 is set")
	ErrNoND          = errors.New("lease: Config.ND is required when Config.Params6 is set")

	// ErrResume6NoAddr is a Config.Resume6 whose Addr is not a usable IPv6
	// address, refused for ErrResumeNoAddr's reason.
	ErrResume6NoAddr = errors.New("lease: Config.Resume6.Addr is not a usable IPv6 address")

	// ErrResume6Twice is Config.Resume6 and Config.Params6.Resume both set:
	// ErrResumeTwice's counterpart, one fact with one derivation.
	ErrResume6Twice = errors.New("lease: set Config.Resume6 or Config.Params6.Resume, not both")
)

// NewManager builds a Manager.
//
// Config.Params6 selects the family: with it, a v6 Manager over TransportV6
// and ND; without it, the v4 Manager this package has always built.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Params6 != nil {
		return newManager6(cfg)
	}
	switch {
	case cfg.Transport == nil:
		return nil, ErrNoTransport
	case cfg.Clock == nil:
		return nil, ErrNoClock
	case cfg.Timers == nil:
		return nil, ErrNoTimers
	case cfg.Entropy == nil:
		return nil, ErrNoEntropy
	case cfg.Params.Conflict != proto.ConflictOff && cfg.ARP == nil:
		return nil, ErrNoARP
	}
	params := cfg.Params
	if cfg.Resume != nil {
		if params.Resume != nil {
			return nil, ErrResumeTwice
		}
		addr := cfg.Resume.Addr.Addr()
		if !addr.Is4() || addr.IsUnspecified() {
			return nil, ErrResumeNoAddr
		}
		// The one crossing, taken ONCE at construction from a single paired
		// reading, for the reason clockBridge exists: two readings taken
		// separately let a wall-clock step land between them.
		b := bridge(cfg.Clock)
		r := &proto.Resume{Addr: addr}
		if !cfg.Resume.Expire.IsZero() {
			r.Expire, r.HasExpire = b.instant(cfg.Resume.Expire), true
		}
		params.Resume = r
	}
	m, err := proto.New(params)
	if err != nil {
		return nil, err
	}
	buf := cfg.EventBuffer
	if buf <= 0 {
		buf = 8
	}
	mg := &Manager{
		cfg:     cfg,
		machine: m,
		journal: cfg.Journal,
		packets: cfg.Packets,
		events:  make(chan Event, buf),
		// Four is enough for every distinct request that can be outstanding
		// at once and then some: the two kinds are idempotent, so a second
		// copy of one already queued would change nothing.
		requests: make(chan proto.Event, 4),
	}
	if mg.journal == nil {
		mg.journal = discardJournal{}
	}
	if mg.packets == nil {
		mg.packets = discardPackets{}
	}
	return mg, nil
}

// newManager6 builds the v6 half of NewManager.
//
// It is a SEPARATE FUNCTION rather than branches inside NewManager because
// every check differs: different ports, different resume shape, different
// machine constructor, and a bucket the v4 path must not get. Interleaving
// them would produce a function in which half the lines are dead for any given
// caller and no reader could tell which half.
func newManager6(cfg Config) (*Manager, error) {
	switch {
	case cfg.Transport != nil || cfg.ARP != nil || cfg.Resume != nil:
		return nil, ErrBothFamilies
	case cfg.TransportV6 == nil:
		return nil, ErrNoTransportV6
	case cfg.Clock == nil:
		return nil, ErrNoClock
	case cfg.Timers == nil:
		return nil, ErrNoTimers
	case cfg.Entropy == nil:
		return nil, ErrNoEntropy
	case cfg.ND == nil:
		return nil, ErrNoND
	}
	params := *cfg.Params6
	if cfg.Resume6 != nil {
		if params.Resume != nil {
			return nil, ErrResume6Twice
		}
		addr := cfg.Resume6.Addr.Addr()
		if !addr.Is6() || addr.Is4In6() || addr.IsUnspecified() {
			return nil, ErrResume6NoAddr
		}
		// The one crossing, taken ONCE from a single paired reading, for
		// clockBridge's reason.
		b := bridge(cfg.Clock)
		params.Resume = &proto.Resume6{
			Addrs: []proto.Addr6{{
				Addr:      addr,
				Preferred: remaining(b, cfg.Resume6.Preferred),
				Valid:     remaining(b, cfg.Resume6.Valid, cfg.Resume6.Expire),
			}},
			ServerDUID: append([]byte(nil), cfg.Resume6.ServerDUID...),
			T1:         remaining(b, cfg.Resume6.Renew),
			T2:         remaining(b, cfg.Resume6.Rebind),
		}
	}
	m, err := proto.New6(params)
	if err != nil {
		return nil, err
	}
	buf := cfg.EventBuffer
	if buf <= 0 {
		buf = 8
	}
	mg := &Manager{
		cfg:      cfg,
		machine6: m,
		journal:  cfg.Journal,
		journal6: cfg.Journal6,
		packets:  cfg.Packets,
		packets6: cfg.PacketsV6,
		limiter:  newBucket(cfg.RateLimit),
		events:   make(chan Event, buf),
		requests: make(chan proto.Event, 4),
	}
	if mg.journal == nil {
		mg.journal = discardJournal{}
	}
	if mg.journal6 == nil {
		mg.journal6 = discardJournal6{}
	}
	if mg.packets == nil {
		mg.packets = discardPackets{}
	}
	if mg.packets6 == nil {
		mg.packets6 = discardPackets6{}
	}
	if !mg.limiter.on {
		// The escape is deliberate and it is recorded where the exchange is
		// recorded: a journal that shows an unbounded burst and does not say
		// the bound was removed reads as a defect in this library.
		mg.journalNote("RFC 9915 §14.1's rate limit is disabled by Config.RateLimit.Messages < 0: this client will not bound the messages it sends")
	}
	return mg, nil
}

// remaining is a wall-clock deadline expressed as the duration still to run,
// on ring 1's clock, taking the FIRST non-zero of the deadlines offered.
//
// A deadline already past yields zero rather than a negative duration: RFC
// 9915 section 7.1's lifetimes are non-negative, a negative one would encode
// as a huge unsigned number on the wire, and "this expired while we were not
// running" is exactly the input a resumed lease is expected to carry.
func remaining(b clockBridge, at ...time.Time) proto.Duration {
	for _, t := range at {
		if t.IsZero() {
			continue
		}
		d := proto.Duration(t.Sub(b.wall))
		if d < 0 {
			return 0
		}
		return d
	}
	// Every deadline offered was zero, which on Lease's convention is an
	// infinite lifetime.
	return proto.Infinite
}

// v6 reports whether this manager runs the v6 machine.
func (mg *Manager) v6() bool { return mg.machine6 != nil }

// Events is the outward stream. It is closed when Run returns.
func (mg *Manager) Events() <-chan Event { return mg.events }

// Lease returns a snapshot of the held lease.
func (mg *Manager) Lease() (Lease, bool) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.lease, mg.held
}

// Stats returns a snapshot of the counters.
func (mg *Manager) Stats() Stats {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.stats
}

// Journal returns the recorded transitions. It is public on purpose: replay
// (T4, G3) is a supported entry point, not a test hook.
func (mg *Manager) Journal() []proto.JournalEntry { return mg.journal.Entries() }

// Packets returns the packet ring (G1).
func (mg *Manager) Packets() []CapturedPacket { return mg.packets.Packets() }

// Run drives the lease until ctx is cancelled.
//
// It returns ctx.Err() on cancellation, which is the ordinary exit. On the way
// out it feeds Stop into the machine so the lease is reported lost and every
// timer is cancelled, then closes the event channel — so a caller ranging over
// Events sees the final Lost and then a clean close, rather than a channel
// that simply stops.
func (mg *Manager) Run(ctx context.Context) error {
	defer close(mg.events)

	mg.dispatch(ctx, proto.Simple(proto.EvStart))

	fired := mg.cfg.Timers.Fired()

	// A nil channel blocks forever, so a client with conflict detection off
	// has no ARP arm in the select rather than an arm guarded by a flag. That
	// is the whole implementation of "no listener" — and the same mechanism
	// is what gives a v6 manager no ARP arm and a v4 manager no ND arm,
	// rather than a family flag read inside the loop.
	var inbound <-chan Inbound
	var arpIn <-chan ARPInbound
	var ndIn <-chan NDInbound
	if mg.v6() {
		inbound = mg.cfg.TransportV6.Received()
		ndIn = mg.cfg.ND.Received()
	} else {
		inbound = mg.cfg.Transport.Received()
		if mg.cfg.ARP != nil {
			arpIn = mg.cfg.ARP.Received()
		}
	}

	for {
		select {
		case req := <-mg.requests:
			mg.dispatch(ctx, req)

		case <-ctx.Done():
			mg.shutdown()
			return ctx.Err()

		case in, ok := <-inbound:
			if !ok {
				// The transport closed under us. That is not a clean stop:
				// something took the socket away.
				mg.shutdown()
				return errors.New("lease: transport closed")
			}
			mg.onInbound(ctx, in)

		case in, ok := <-arpIn:
			if !ok {
				// The ARP socket closing is NOT fatal to the lease. The DHCP
				// transport still works, the lease is still held and its
				// timers are still armed; what has stopped is RFC 5227
				// section 2.4's ongoing detection. Tearing the client down
				// here would turn a lost listener into a lost address, which
				// is strictly worse than the conflict it was watching for.
				//
				// The arm is nilled so the closed channel does not spin the
				// loop, and the loss is journalled: a client that has stopped
				// watching must not look like one that is watching and seeing
				// nothing.
				arpIn = nil
				mg.journal.Append(proto.JournalEntry{
					Kind:   proto.EvARPReceived,
					Reason: "the ARP socket closed: RFC 5227 2.4 ongoing conflict detection has stopped for this lease",
				})
				continue
			}
			mg.onARP(ctx, in)

		case in, ok := <-ndIn:
			if !ok {
				// The Neighbor Discovery socket closing is NOT fatal to the
				// lease, for the reason the ARP arm gives: the DHCPv6
				// transport still works and the lease is still held. What has
				// stopped is router discovery and any duplicate address
				// detection ring 3 would have run through this port, and the
				// journal says so — a client that has stopped watching must
				// not look like one that is watching and seeing nothing.
				ndIn = nil
				mg.journalNote("the Neighbor Discovery socket closed: router discovery and duplicate address detection have stopped for this lease")
				continue
			}
			mg.onND(ctx, in)

		case id, ok := <-fired:
			if !ok {
				mg.shutdown()
				return errors.New("lease: timers closed")
			}
			mg.bump(func(s *Stats) { s.TimerFires++ })
			mg.dispatch(ctx, proto.TimerFired(id))
		}
	}
}

// Release asks the client to give up the address and stop: a DHCPRELEASE to
// the server if a lease is held, and STOPPED either way (RFC 2131 section
// 4.4.6).
//
// It does not block and it does not report success. Neither message in that
// section is answered by the server, so there is nothing to wait for.
// Calling it with no Run in flight, or twice, does nothing the second time.
//
// LOST ARRIVES ONLY WHEN A LEASE WAS HELD. It is the confirmation that one was
// given back, so releasing during acquisition — INIT, SELECTING or REQUESTING
// — correctly produces no Lost and no DHCPRELEASE: there was no binding to
// relinquish. The client still stops, which is the part that is not optional
// and the part that was missing until round 4.
// TestReleaseDuringAcquisitionStopsTheClient holds it.
//
// IT CAN BE DROPPED. The request queue is bounded, and a call made while it is
// full is counted in Stats.RequestsDropped and does nothing else — no error,
// no panic, no retry. A caller that needs to know whether the call landed
// reads Stats.RequestsDropped, NOT the absence of a Lost event: absence is the
// ordinary outcome whenever no lease was held, so it cannot distinguish a
// dropped Release from a delivered one. That distinction is why the sentence
// here used to be wrong — it named calling again as the remedy for a missing
// Lost, which for a client still acquiring was an unbounded loop.
//
// THAT READING IS SOUND FOR A SINGLE CALLER ONLY. Stats.RequestsDropped is one
// counter over both request kinds and over every caller, so "it went up" means
// SOME request was dropped, not that THIS one was: a second goroutine calling
// Release or ReportConflict raises it too, and two callers racing can each see
// a rise the other caused. There is no per-call receipt, deliberately —
// returning one would make a fire-and-forget call something every caller has
// to check — so a program with more than one caller has to serialise them if
// it wants to read the counter this way.
func (mg *Manager) Release() { mg.request(proto.Simple(proto.EvRelease)) }

// ReportConflict tells the client that something else is using the address it
// holds, which obliges a DHCPRELEASE's counterpart — a DHCPDECLINE (RFC 2131
// section 3.1(5), a MUST).
//
// IT IS NOT THE ONLY DETECTOR ANY MORE, and it is still here for the callers
// that have evidence this library cannot see: a kernel ARP cache entry, a
// switch complaint, a second interface answering. Since M6 the library runs RFC
// 5227 itself unless Params.Conflict is proto.ConflictOff — and it stays
// available in that mode too, which is what makes ConflictOff "no probing"
// rather than "no DHCPDECLINE".
//
// It can be dropped for the same reason Release can, and is counted the same
// way; the event to wait for is Lost carrying proto.ReasonConflict.
func (mg *Manager) ReportConflict() { mg.request(proto.Simple(proto.EvConflictDetected)) }

// ReportDADResult tells the client what RFC 4862 section 5.4's duplicate
// address detection found for one address.
//
// IT IS THE OTHER HALF OF proto.ActStartDAD AND IT IS THE CALLER'S TO CALL.
// The machine asks (the action), ring 3 performs the exchange, and this is how
// the answer gets back: joining the solicited-node multicast group, sending
// the Neighbor Solicitations and reading RFC 7527 section 4.1's looped-back
// frames all need a socket, which is ring 3's and not this ring's. Until M7c
// builds it, a caller — or a test — supplies the answer directly.
//
// A RESULT FOR AN ADDRESS THE MACHINE NEVER ASKED ABOUT IS IGNORED AND
// JOURNALLED, not an error: ring 3 runs the same check for the chassis, and a
// client that acted on somebody else's answer would decline an address it does
// not hold. See proto.Machine6's takeDADResult.
//
// It can be dropped for the reason Release can be, and is counted the same
// way; the event to wait for is Acquired, or Failed carrying
// proto.ReasonDADIncomplete.
func (mg *Manager) ReportDADResult(addr netip.Addr, duplicate bool) {
	mg.request(proto.DADResult(addr, duplicate))
}

// ReportAddressLost tells the client that an address it holds has been
// withdrawn from the interface.
//
// It is sequencing section 8.2's input and RFC 4429 section 3.3's: an
// optimistic address can be removed under a bound lease by the kernel's own
// duplicate address detection, and the withdrawal is evidence somebody else
// has it. The v6 machine answers with a Decline and a restart; the v4 machine
// treats it as a conflict.
func (mg *Manager) ReportAddressLost() { mg.request(proto.Simple(proto.EvAddressLost)) }

func (mg *Manager) request(ev proto.Event) {
	select {
	case mg.requests <- ev:
	default:
		mg.bump(func(s *Stats) { s.RequestsDropped++ })
	}
}

// shutdown feeds Stop so the lease is reported lost and every timer is
// cancelled, on the way out of Run.
//
// It uses a background context because the caller's is already cancelled and
// dispatching with a dead context would cancel the very actions — timer
// cancellation, the final Lost — that shutdown exists to perform.
func (mg *Manager) shutdown() {
	mg.stopping = true
	mg.dispatch(context.Background(), proto.Simple(proto.EvStop))
}

func (mg *Manager) onInbound(ctx context.Context, in Inbound) {
	if mg.v6() {
		mg.onInbound6(ctx, in)
		return
	}
	if in.Err != nil {
		mg.bump(func(s *Stats) { s.TransportErrors++ })
		mg.packets.Record(CapturedPacket{
			At: mg.cfg.Clock.Wall(), Dir: DirIn, DecodeErr: in.Err,
		})
		return
	}
	mg.bump(func(s *Stats) { s.Received++ })
	msg, err := wire.Decode(in.Payload)
	if err == nil {
		if t, ok := msg.Type(); ok && t == wire.MsgNak {
			// Counted here, before ring 1 sees it, so NaksSeen measures the
			// wire and not the machine's opinion of the wire.
			mg.bump(func(s *Stats) { s.NaksSeen++ })
		}
	}
	mg.packets.Record(CapturedPacket{
		At:        mg.cfg.Clock.Wall(),
		Dir:       DirIn,
		Raw:       append([]byte(nil), in.Payload...),
		Msg:       msg,
		DecodeErr: err,
	})
	if err != nil {
		// A packet that will not decode never reaches ring 1. It is counted
		// and captured, because "we dropped something" with no evidence is
		// the failure mode this library's debug requirements exist for.
		mg.bump(func(s *Stats) { s.DecodeFailures++ })
		return
	}
	mg.dispatch(ctx, proto.Received(msg, append([]byte(nil), in.Payload...)))
}

// dispatch feeds one event and everything that event produces.
//
// The queue is what enforces the ordering rule: an action that fails becomes
// an EvActionFailed appended to the queue, and it is fed only after the
// CURRENT action list has been drained in full. Feeding it immediately would
// re-enter Step in the middle of executing the previous Step's actions, which
// is the reentrancy the design document forbids.
func (mg *Manager) dispatch(ctx context.Context, ev proto.Event) {
	queue := []proto.Event{ev}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]

		now := mg.cfg.Clock.Mono()
		rnd := mg.cfg.Entropy.Uint64()

		var acts []proto.Action
		var seq uint64
		if mg.v6() {
			from := mg.machine6.State()
			var to proto.State6
			to, acts = mg.machine6.Step(now, rnd, e)

			mg.mu.Lock()
			seq = mg.seq
			mg.seq++
			mg.stats.Steps++
			mg.dad = mg.machine6.DADPhase()
			// The router observation is taken HERE and not in the
			// ActRouterObserved arm, so it has ONE derivation: the machine's
			// own view after the Step. The action's copy is the same value —
			// it is stamped from the same field — and reading it there too
			// would be one fact derived twice, in two places that can be
			// edited apart.
			mg.router = mg.machine6.Router()
			mg.mu.Unlock()

			mg.journal6.Append(proto.NewJournalEntry6(seq, now, rnd, e, from, to, acts))
		} else {
			from := mg.machine.State()
			var to proto.State
			to, acts = mg.machine.Step(now, rnd, e)

			mg.mu.Lock()
			seq = mg.seq
			mg.seq++
			mg.stats.Steps++
			mg.acd = mg.machine.ACDPhase()
			mg.mu.Unlock()

			mg.journal.Append(proto.NewJournalEntry(seq, now, rnd, e, from, to, acts))
		}

		queue = append(queue, mg.drain(ctx, acts)...)
	}
}

// drain executes an action list in order and returns the failure events it
// produced.
func (mg *Manager) drain(ctx context.Context, acts []proto.Action) []proto.Event {
	var failures []proto.Event
	bridgeAt := bridge(mg.cfg.Clock)

	for _, a := range acts {
		mg.bump(func(s *Stats) { s.ActionsExecuted++ })
		switch a.Kind {
		case proto.ActSend:
			raw, err := wire.Encode(a.Msg)
			if err != nil {
				mg.bump(func(s *Stats) { s.SendFailures++ })
				failures = append(failures, proto.ActionFailed(a.ID, "encode: "+err.Error()))
				continue
			}
			if err := mg.cfg.Transport.Send(a.Dest, raw); err != nil {
				mg.bump(func(s *Stats) { s.SendFailures++ })
				failures = append(failures, proto.ActionFailed(a.ID, err.Error()))
				continue
			}
			mg.bump(func(s *Stats) { s.Sent++ })
			mg.countSent(a.Msg)
			mg.packets.Record(CapturedPacket{
				At: bridgeAt.wall, Dir: DirOut, Raw: raw, Msg: a.Msg,
			})

		case proto.ActSendV6:
			if ev := mg.sendV6(a, bridgeAt); ev != nil {
				failures = append(failures, *ev)
			}

		case proto.ActSendRouterSolicit:
			if ev := mg.sendRouterSolicit(a, bridgeAt); ev != nil {
				failures = append(failures, *ev)
			}

		case proto.ActStartDAD:
			// RING 3 OWNS THE EXCHANGE AND THIS RING OWNS NOTHING BUT THE
			// HANDOVER. RFC 4862 section 5.4's Neighbor Solicitations, the
			// join of the solicited-node multicast group, and RFC 7527
			// section 4.1's looped-back frames all need a socket, which is
			// runtime.DADProbe's; what comes back is one proto.EvDADResult
			// per address, through the callback below.
			//
			// THE CALLBACK GOES THROUGH ReportDADResult AND NOT STRAIGHT INTO
			// THE MACHINE. It arrives on another goroutine — the runner's —
			// and every other outside input to this manager takes the same
			// request channel for that reason; a direct Step from a runner's
			// goroutine would race every Step this loop makes. It also means
			// a result the machine no longer wants is dropped and journalled
			// there rather than here, in the one place that rule is written.
			//
			// A Config WITHOUT A RUNNER counts and journals and nothing else,
			// which is what this arm did before the port existed. The machine
			// has already armed its own deadline, so such a client fails the
			// acquisition with proto.ReasonDADIncomplete rather than waiting
			// — a fault, not a hang, and that is the point.
			mg.bump(func(s *Stats) { s.DADChecksStarted++ })
			if mg.cfg.DAD != nil {
				mg.journalNote("duplicate address detection started for " + a.Target.String() + " (RFC 4862 section 5.4)")
				mg.cfg.DAD.Start(a.Target, mg.ReportDADResult)
			} else {
				mg.journalNote("duplicate address detection requested for " + a.Target.String() + " (RFC 4862 section 5.4; no runner configured, the caller owes the result)")
			}

		case proto.ActRouterObserved:
			// A diagnostic and NOTHING ELSE (design Q2): nothing in this
			// library waits for a Router Advertisement or acts on one beyond
			// design section A.3.3's interlock, which ring 1 has already
			// applied by the time this action exists. The manager's copy is
			// taken from the machine at the Step above rather than from
			// a.Router here — one fact, one derivation — so this arm exists
			// to name the action and to keep it out of the default one.

		case proto.ActConfigured:
			cfg := toConfig(a.Config, bridgeAt)
			mg.mu.Lock()
			mg.config = cfg
			mg.stats.ConfiguredEvents++
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Configured, Config: cfg})

		case proto.ActSendARP:
			raw, err := wire.EncodeARP(a.ARP)
			if err != nil {
				mg.bump(func(s *Stats) { s.ARPSendFailures++ })
				failures = append(failures, proto.ActionFailed(a.ID, "encode ARP: "+err.Error()))
				continue
			}
			if mg.cfg.ARP == nil {
				// Unreachable through NewManager, which refuses this Config
				// with ErrNoARP. Handled anyway, and as a FAILURE rather than
				// a panic: R2 says the machine is told when an action did not
				// happen, and a nil dereference in the loop that owns the
				// lease takes the lease with it.
				mg.bump(func(s *Stats) { s.ARPSendFailures++ })
				failures = append(failures, proto.ActionFailed(a.ID, "no ARP port"))
				continue
			}
			if err := mg.cfg.ARP.Send(raw); err != nil {
				mg.bump(func(s *Stats) { s.ARPSendFailures++ })
				failures = append(failures, proto.ActionFailed(a.ID, err.Error()))
				continue
			}
			// Counted off the packet that left, not off the intention. An ARP
			// Probe is defined by its all-zero sender IP (RFC 5227 section
			// 1.1), so this reads the same field a receiver would.
			if a.ARP.IsProbe() {
				mg.bump(func(s *Stats) { s.ProbesSent++ })
			} else {
				mg.bump(func(s *Stats) { s.AnnouncementsSent++ })
			}
			mg.packets.Record(CapturedPacket{
				At: bridgeAt.wall, Dir: DirOut, Raw: raw, ARP: a.ARP,
			})

		case proto.ActSetTimer:
			mg.cfg.Timers.Set(a.Timer, a.After)

		case proto.ActCancelTimer:
			mg.cfg.Timers.Cancel(a.Timer)

		case proto.ActLeaseAcquired:
			l := mg.outward(a, bridgeAt)
			mg.mu.Lock()
			mg.lease, mg.held = l, true
			mg.stats.LeasesAcquired++
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Acquired, Lease: l, Requested: a.Requested})

		case proto.ActLeaseRenewed:
			l := mg.outward(a, bridgeAt)
			mg.mu.Lock()
			mg.lease, mg.held = l, true
			mg.stats.RenewalsCompleted++
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Renewed, Lease: l})

		case proto.ActLeaseChanged:
			l := mg.outward(a, bridgeAt)
			mg.mu.Lock()
			mg.lease, mg.held = l, true
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Changed, Lease: l})

		case proto.ActLeaseLost:
			mg.mu.Lock()
			lost := mg.lease
			mg.lease, mg.held = Lease{}, false
			mg.stats.LeasesLost++
			if a.Reason == proto.ReasonConflict {
				// RFC 5227 section 2.4's path, or RFC 9915 section 18.2.10.1's
				// after a bound address was withdrawn: the address was in use
				// and had already been announced to the caller. See the Failed
				// arm for the other half.
				mg.stats.ConflictsDetected++
				if mg.machine6 != nil {
					mg.stats.DADConflicts++
				}
			}
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Lost, Lease: lost, Reason: a.Reason})

		case proto.ActFailed:
			// Bumped before the emit, like every other counter here: the
			// Failed event is the one that reports these two, and a caller
			// reading Stats when it arrives must see them. See Stats.
			mg.bump(func(s *Stats) {
				s.AcquireFailures++
				switch a.Reason {
				case proto.ReasonNak:
					s.NaksAccepted++
				case proto.ReasonConflict:
					// A conflict found before the address was ever used: RFC
					// 5227 section 2.1's check failing, or RFC 4862 section
					// 5.4's, so nothing was acquired and no ActLeaseLost
					// follows. This is the counterpart bump to the one in the
					// Lost arm, and the two are mutually exclusive by
					// construction — ring 1 emits ActFailed with this reason
					// only when no lease is held.
					s.ConflictsDetected++
					if mg.machine6 != nil {
						// The same split the Lost arm makes, and the v6 path
						// arrives here rather than there in the case that
						// matters: RFC 4862 section 5.4 runs BEFORE the lease
						// is announced, so a duplicate on a fresh acquisition
						// never held one. Counting it only in the Lost arm
						// left DADConflicts at zero for the ordinary
						// duplicate and told WireCounters' reader that a v6
						// conflict had come from RFC 5227's check.
						s.DADConflicts++
					}
				}
			})
			mg.emit(ctx, Event{Kind: Failed, Reason: a.Reason, Note: a.Note})

		case proto.ActJournal:
			// Already in the journal entry's Actions. Nothing else to do —
			// and the case is written out rather than falling into a default,
			// so that adding an action kind makes this switch incomplete
			// under a linter rather than silently doing nothing.

		default:
			mg.journal.Append(proto.JournalEntry{
				Kind: proto.EvActionFailed,
				Reason: fmt.Sprintf("manager does not implement action kind %s",
					a.Kind),
			})
		}
	}
	if len(failures) > 0 {
		mg.bump(func(s *Stats) { s.ActionsFailedFed += uint64(len(failures)) })
	}
	return failures
}

// outward is the lease an action carries, in whichever family this manager
// runs.
//
// ONE DERIVATION, READ OFF THE MANAGER AND NOT OFF THE ACTION. proto.Action
// carries both a Lease and a Lease6 and only one of them is set, so a switch
// on "which field looks filled" would answer differently for an empty lease
// than for a populated one — and an empty lease is exactly what a Lost event
// on a machine that never bound would carry.
func (mg *Manager) outward(a proto.Action, b clockBridge) Lease {
	if mg.v6() {
		return toLease6(a.Lease6, b)
	}
	return toLease(a.Lease, b)
}

// onARP is one frame off the link.
//
// THE THREE REFUSALS ARE COUNTED SEPARATELY and none of them reaches ring 1.
// A shared link carries ARP continuously, every event that reaches Step costs a
// journal entry, and the journal is bounded — so an unfiltered feed would wrap
// it between one acquisition and the next and destroy the replay (R3). The
// filter itself is ring 1's, for the reason Machine.ARPRelevant gives.
func (mg *Manager) onARP(ctx context.Context, in ARPInbound) {
	if in.Err != nil {
		mg.bump(func(s *Stats) { s.ARPErrors++ })
		return
	}
	// ONE bump per frame, carrying both the sighting and its verdict.
	//
	// Two bumps would leave a window in which a reader sees ARPSeen raised and
	// the frame not yet classified, so the invariant a caller reads these
	// counters for — every frame seen is exactly one of decode-failed, ignored
	// and delivered — would be false at arbitrary moments rather than only
	// while a frame is genuinely in flight.
	p, err := wire.DecodeARP(in.Frame)
	if err != nil {
		// Not an Ethernet/IPv4 ARP packet, or shorter than one. Counted and
		// dropped: RFC 5227's rules are all predicates over the sender and
		// target addresses of such a packet, and a frame that has none has
		// nothing for them to read.
		mg.bump(func(s *Stats) { s.ARPSeen++; s.ARPDecodeFailures++ })
		return
	}
	if !mg.machine.ARPRelevant(p) {
		mg.bump(func(s *Stats) { s.ARPSeen++; s.ARPIgnored++ })
		return
	}
	mg.bump(func(s *Stats) { s.ARPSeen++ })
	mg.packets.Record(CapturedPacket{
		At:  mg.cfg.Clock.Wall(),
		Dir: DirIn,
		Raw: append([]byte(nil), in.Frame...),
		ARP: p,
	})
	mg.dispatch(ctx, proto.ARPReceived(p))
}

// ACDPhase reports where RFC 5227's conflict check stands.
//
// It is proto.ACDIdle before Run starts, after it returns, and for the whole
// life of a client running with proto.ConflictOff.
func (mg *Manager) ACDPhase() proto.ACDPhase {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.acd
}

// DADPhase reports where RFC 4862 section 5.4's duplicate address detection
// stands. It is proto.DADIdle for the whole life of a v4 manager.
func (mg *Manager) DADPhase() proto.DADPhase {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.dad
}

// Router is the last Router Advertisement this manager saw, and the zero value
// means it has seen none. It is a diagnostic (design Q2), never an
// instruction: nothing in this library waits for it.
func (mg *Manager) Router() proto.RouterObservation {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.router
}

// Config is the stateless configuration this manager last received, on the v6
// path. The zero value means none has arrived.
func (mg *Manager) Config() Configuration {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.config
}

// Journal6 returns the recorded v6 transitions, for proto.Replay6.
func (mg *Manager) Journal6() []proto.JournalEntry6 { return mg.journal6.Entries() }

// PacketsV6 returns the v6 packet ring.
func (mg *Manager) PacketsV6() []CapturedPacketV6 { return mg.packets6.Packets() }

// journalNote records one line in whichever journal this manager writes.
func (mg *Manager) journalNote(note string) {
	if mg.v6() {
		mg.journal6.Append(proto.JournalEntry6{Kind: proto.EvActionFailed, Reason: note, Note: true})
		return
	}
	mg.journal.Append(proto.JournalEntry{Kind: proto.EvActionFailed, Reason: note})
}

// emit delivers an outward event.
//
// While running it BLOCKS until the caller takes it or the context ends.
// Dropping a lease event to keep the loop moving would mean the caller's view
// of the lease silently diverging from the machine's, which is the class of
// bug this library exists to remove.
//
// While shutting down it is best-effort, and that is not a weakening of the
// rule above. The caller has already cancelled; a blocking send would deadlock
// Run against a caller that has stopped reading, which is the ordinary way to
// stop a client. The final event is not lost by it either: the event channel
// is buffered and Go delivers buffered values after a close, so a caller that
// drains Events after Run returns still sees the closing Lost. EventsDropped
// counts the case where even the buffer was full, so "we dropped one" is a
// measurement rather than an assumption.
func (mg *Manager) emit(ctx context.Context, e Event) {
	// Stamped here rather than at each of the five call sites, so that a new
	// event kind cannot be added without it. emit runs on Run's goroutine,
	// which is the machine's owner, so this reads the live phase and not the
	// snapshot ACDPhase serves to other goroutines.
	if mg.v6() {
		e.Family = FamilyV6
		e.DAD = mg.machine6.DADPhase()
		e.Router = mg.machine6.Router()
		if e.Kind != Configured {
			e.Config = mg.config
		}
	} else {
		e.Family = FamilyV4
		e.ACD = mg.machine.ACDPhase()
	}
	if mg.stopping {
		select {
		case mg.events <- e:
		default:
			mg.bump(func(s *Stats) { s.EventsDropped++ })
		}
		return
	}
	select {
	case mg.events <- e:
	case <-ctx.Done():
		mg.bump(func(s *Stats) { s.EventsDropped++ })
	}
}

// countSent counts the two messages that are never answered.
//
// Counted where the send SUCCEEDED rather than where the machine decided to
// send, because these two are the library's only outputs with no reply and no
// retransmission: "we decided to decline" and "a DHCPDECLINE left the host"
// are different facts, and only the second one is worth a counter.
func (mg *Manager) countSent(msg *wire.Message) {
	t, ok := msg.Type()
	if !ok {
		return
	}
	switch t {
	case wire.MsgDecline:
		mg.bump(func(s *Stats) { s.DeclinesSent++ })
	case wire.MsgRelease:
		mg.bump(func(s *Stats) { s.ReleasesSent++ })
	case wire.MsgRequest:
		// A DHCPREQUEST with 'ciaddr' filled in is a renewal, and only a
		// renewal. RFC 2131 Table 5 gives ciaddr as zero in the SELECTING and
		// INIT-REBOOT columns and as the client's address in the RENEWING and
		// REBINDING ones, so this reads the message rather than asking the
		// machine what state it was in — which is what keeps the counter
		// right when INIT-REBOOT arrives in M5.
		if msg.CIAddr.Is4() && !msg.CIAddr.IsUnspecified() {
			mg.bump(func(s *Stats) { s.RenewalsSent++ })
		}
	}
}

func (mg *Manager) bump(f func(*Stats)) {
	mg.mu.Lock()
	f(&mg.stats)
	mg.mu.Unlock()
}

type discardJournal struct{}

func (discardJournal) Append(proto.JournalEntry)     {}
func (discardJournal) Entries() []proto.JournalEntry { return nil }

type discardPackets struct{}

func (discardPackets) Record(CapturedPacket)     {}
func (discardPackets) Packets() []CapturedPacket { return nil }

type discardJournal6 struct{}

func (discardJournal6) Append(proto.JournalEntry6)     {}
func (discardJournal6) Entries() []proto.JournalEntry6 { return nil }

type discardPackets6 struct{}

func (discardPackets6) Record(CapturedPacketV6)     {}
func (discardPackets6) Packets() []CapturedPacketV6 { return nil }
