package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
)

// NewManager builds a Manager.
func NewManager(cfg Config) (*Manager, error) {
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

	inbound := mg.cfg.Transport.Received()
	fired := mg.cfg.Timers.Fired()

	// A nil channel blocks forever, so a client with conflict detection off
	// has no ARP arm in the select rather than an arm guarded by a flag. That
	// is the whole implementation of "no listener".
	var arpIn <-chan ARPInbound
	if mg.cfg.ARP != nil {
		arpIn = mg.cfg.ARP.Received()
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
		from := mg.machine.State()
		to, acts := mg.machine.Step(now, rnd, e)

		mg.mu.Lock()
		seq := mg.seq
		mg.seq++
		mg.stats.Steps++
		mg.acd = mg.machine.ACDPhase()
		mg.mu.Unlock()

		mg.journal.Append(proto.NewJournalEntry(seq, now, rnd, e, from, to, acts))

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
			l := toLease(a.Lease, bridgeAt)
			mg.mu.Lock()
			mg.lease, mg.held = l, true
			mg.stats.LeasesAcquired++
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Acquired, Lease: l, Requested: a.Requested})

		case proto.ActLeaseRenewed:
			l := toLease(a.Lease, bridgeAt)
			mg.mu.Lock()
			mg.lease, mg.held = l, true
			mg.stats.RenewalsCompleted++
			mg.mu.Unlock()
			mg.emit(ctx, Event{Kind: Renewed, Lease: l})

		case proto.ActLeaseChanged:
			l := toLease(a.Lease, bridgeAt)
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
				// RFC 5227 section 2.4's path: the address was in use and had
				// already been announced to the caller. See the Failed arm for
				// the other half.
				mg.stats.ConflictsDetected++
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
					// 5227 section 2.1's check failing, so nothing was
					// acquired and no ActLeaseLost follows. This is the
					// counterpart bump to the one in the Lost arm, and the two
					// are mutually exclusive by construction — ring 1 emits
					// ActFailed with this reason only when no lease is held.
					s.ConflictsDetected++
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
	e.ACD = mg.machine.ACDPhase()
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
