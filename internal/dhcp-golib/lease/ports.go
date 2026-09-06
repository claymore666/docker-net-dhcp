package lease

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// Clock is the two clocks the design document's section 8.2 requires, and they
// are not interchangeable: Mono is the interval clock every ring-1 deadline is
// computed on (RFC 2131 section 3.3), Wall is the absolute clock a lease that
// must survive a restart is persisted on. Which monotonic clock Mono reads
// decides the host-suspend case; see runtime.Clock, where that is chosen.
type Clock interface {
	Mono() proto.Instant
	Wall() time.Time
}

// Entropy is the source of the rnd parameter Step takes. An interface rather
// than an io.Reader so a test supplies a fixed sequence in one line, and so
// that "one value per Step" is a property of this package's loop rather than
// of whatever a Reader returns.
type Entropy interface {
	Uint64() uint64
}

// Inbound is one thing that arrived on the transport.
//
// Err and Payload are mutually exclusive. A transport reporting an error is
// not reporting an empty packet: an error folded into a value has no
// direction, and a zero-length payload that means "the socket died" is exactly
// that defect.
type Inbound struct {
	Payload []byte
	From    netip.Addr
	Err     error
}

// Transport carries DHCP payloads — the UDP payload only. Building the IP and
// UDP headers, and getting them onto a link the kernel has no address on, is
// ring 3's problem.
type Transport interface {
	// Send transmits one payload. A returned error becomes an
	// EvActionFailed for the action that asked for the send, which is R2:
	// the machine never assumes an action succeeded.
	Send(dst proto.Dest, payload []byte) error
	// Received is the stream of inbound payloads. It is closed when the
	// transport is closed.
	Received() <-chan Inbound
	// Close releases the transport. It is safe to call more than once.
	Close() error
}

// ARPInbound is one ARP frame that arrived on the link, or the error that
// ended the stream. Frame and Err are mutually exclusive, for the reason
// Inbound gives.
type ARPInbound struct {
	Frame []byte
	Err   error
}

// ARP is the link's ARP traffic: RFC 5227's Probes and Announcements out, and
// everything the link carries in.
//
// IT IS A SECOND PORT AND NOT A MODE OF Transport. The two sockets carry
// different EtherTypes — ETH_P_IP for the DHCP datagram this library builds
// itself, ETH_P_ARP for these — so a single port would have to inspect what it
// was handed in order to decide where to write it, which is ring 3 parsing
// ring 0's output to route it. Keeping them apart is also what makes
// ConflictOff a shape rather than a flag: that client is built with no ARP
// port at all, so there is nothing open to listen on.
//
// Send takes ENCODED BYTES, like Transport.Send, because encoding is ring 0's
// and the manager does it: an implementation that took a wire.ARPPacket would
// put the codec below the ring that owns it.
//
// Received DELIVERS EVERYTHING ON THE LINK, unfiltered. Which frames matter is
// RFC 5227's question and it is answered in ring 1 (Machine.ARPRelevant), not
// here — a filter in the socket is a protocol rule in a place with no test.
type ARP interface {
	Send(frame []byte) error
	Received() <-chan ARPInbound
	Close() error
}

// Timers turns ring 1's SetTimer and CancelTimer into one fire on Fired.
//
// Set on an already-armed timer REPLACES it: ring 1 re-arms the retransmit
// timer freely and never tracks what is armed, so a Timers that queued a
// second fire would produce a retransmission storm no ring-1 test could see.
type Timers interface {
	Set(id proto.TimerID, after proto.Duration)
	Cancel(id proto.TimerID)
	Fired() <-chan proto.TimerID
	Close() error
}

// Journal records every Step. See proto.JournalEntry.
type Journal interface {
	Append(proto.JournalEntry)
	Entries() []proto.JournalEntry
}

// Direction says which way a captured packet went.
type Direction uint8

// Inbound and outbound, for the packet ring.
const (
	DirIn Direction = iota
	DirOut
)

func (d Direction) String() string {
	if d == DirOut {
		return "out"
	}
	return "in"
}

// CapturedPacket is one message in or out, decoded, with a timestamp (G1).
//
// Raw is kept beside the decoded message because the pcap export (G4) needs
// the bytes, and because a message that FAILED to decode is the one worth
// having: Msg is nil then and DecodeErr says why.
type CapturedPacket struct {
	At        time.Time
	Dir       Direction
	Raw       []byte
	Msg       *wire.Message
	DecodeErr error

	// ARP is set instead of Msg when this capture is an ARP packet: an RFC
	// 5227 Probe or Announcement going out, or a frame coming in that the
	// conflict rules had something to say about.
	//
	// ONLY THE RELEVANT INBOUND FRAMES ARE CAPTURED. A shared link carries
	// ARP continuously and this ring is bounded, so recording every frame
	// would wrap it between one acquisition and the next and take the
	// evidence of the acquisition with it. What is kept is what
	// Machine.ARPRelevant admitted — every frame the conflict rules could
	// have acted on, which is the population a conflict has to be explained
	// from. Stats.ARPIgnored counts the rest, so the gap is a number rather
	// than a silence.
	ARP *wire.ARPPacket
}

// PacketRing is the bounded ring of every message in and out (G1, R3).
type PacketRing interface {
	Record(CapturedPacket)
	Packets() []CapturedPacket
}

// Store is the durable lease-record log: the port ring 3 implements and the
// only thing that survives the process.
//
// It is deliberately NOT a record store. What is durable is the event stream;
// the record is the fold of it (Rebuild). A port with Get and Put for records
// would make the last writer the truth and would lose the one property this
// design is built on — that a restart replays what happened rather than
// trusting a summary somebody wrote down.
//
// APPEND-ONLY AND IN ORDER. Load returns every event this store holds, in the
// order it was appended, which is what makes the fold's answer a function of
// the file. A Load that sorted, de-duplicated or reversed would satisfy any
// test that counted lines; the ordering is asserted directly, and the fold's
// own per-record sequence check refuses a reordering independently.
//
// AN IMPLEMENTATION MAY SKIP WHAT IT CANNOT READ. A process killed inside an
// Append leaves a fragment, and refusing the whole file for it would lose every
// record written before the crash. Skipping is therefore allowed and COUNTING
// what was skipped is not optional — which is why Damage is on the port and
// not on one implementation: a caller holding a Store could otherwise be told
// "here is every event" by a store that had just dropped one, and would have
// no way to ask.
//
// AN IMPLEMENTATION MAY NOT CREATE DAMAGE. Skipping a fragment somebody else
// left is allowed; appending onto one is not. A store that writes an event
// after a half-written line destroys BOTH — the fragment and the event it was
// just handed — and the count then names one line for two losses, which is the
// count lying rather than reporting. The fragment can arrive at any time: from
// this store's own short write, or from another process on the same file that
// died inside its Append. An implementation that appends to a shared file
// therefore has to check the file, not its own memory of what it wrote.
type Store interface {
	// Append writes one event. It must be atomic against a concurrent Append
	// from another process on the same file: one line, one write. An
	// implementation repairing a fragment ahead of the event puts the repair
	// in that same write, so the guarantee holds for a repaired append too.
	Append(RecordEvent) error
	// Load returns every event, in append order.
	Load() ([]RecordEvent, error)
	// Damage reports the lines this store could not read, whether a Load
	// found them or the store had to repair them to open at all. A store that
	// read everything reports a zero value.
	Damage() StoreDamage
}

// StoreDamage is what a Load could not read.
//
// The two numbers are reported apart rather than folded into one, because they
// mean different things: a torn tail is a crash, and an unreadable line
// anywhere else is two writers or a damaged file.
type StoreDamage struct {
	// TornTail counts the file's LAST line when it has no terminating newline
	// and does not parse — the shape a process killed inside Append leaves.
	//
	// Only the missing newline makes a fragment. A last line that HAS its
	// newline and still does not parse is counted in Skipped: the writer got
	// the newline out after it, so whatever damaged the line was not a crash
	// in the middle of writing it.
	TornTail int
	// Skipped counts every other unreadable line: the interior ones, and the
	// last one when its newline is present.
	Skipped int
}

// Any reports whether anything was unreadable.
func (d StoreDamage) Any() bool { return d.TornTail > 0 || d.Skipped > 0 }

func (d StoreDamage) String() string {
	return fmt.Sprintf("%d torn tail, %d skipped", d.TornTail, d.Skipped)
}

// TransportV6 carries DHCPv6 payloads — the UDP payload only, like Transport.
//
// IT IS A SECOND PORT AND NOT A MODE OF Transport, for ARP's reason applied to
// a different pair: the two carry different address families on different
// sockets (UDP/IPv4 port 68 against UDP/IPv6 port 546), and a single port
// would have to inspect proto.Dest to decide which socket to write to — ring 3
// reading ring 1's output to route it.
//
// Send TAKES A proto.Dest WHOSE Addr IS AN IPv6 ADDRESS, and for every message
// this client sends that address is RFC 9915 section 7.1's
// All_DHCP_Relay_Agents_and_Servers (ff02::1:2). There is no unicast
// destination: RFC 9915 removed the Server Unicast option that RFC 3315 had —
// see section 6.4 of the sequencing note — so a v6 client multicasts every
// message of every exchange, including a Renew to the server that granted the
// lease.
type TransportV6 interface {
	Send(dst proto.Dest, payload []byte) error
	Received() <-chan Inbound
	Close() error
}

// NDInbound is one ICMPv6 Neighbor Discovery frame that arrived on the link,
// or the error that ended the stream. Frame and Err are mutually exclusive,
// for the reason Inbound gives.
type NDInbound struct {
	Frame []byte
	Err   error
}

// ND is the link's IPv6 Neighbor Discovery traffic: Router Solicitations and
// the duplicate-address-detection Neighbor Solicitations out, Router
// Advertisements and Neighbor Advertisements in.
//
// IT IS ARP's COUNTERPART AND HAS ARP's SHAPE (D30): encoding is ring 0's and
// the manager does it, so an implementation that built its own Router
// Solicitation would put the codec below the ring that owns it.
//
// THE ONE DIFFERENCE FROM ARP IS THAT Send TAKES A wire.ICMPv6Packet AND NOT A
// []byte, and it is forced by the protocol rather than chosen. An ARP frame is
// self-contained; an ICMPv6 message is not — RFC 4443 section 2.3 computes its
// checksum over a pseudo-header made of the SOURCE and DESTINATION addresses,
// so those two addresses are part of the encoded message whether or not they
// are part of its bytes. wire.ICMPv6Packet is those three fields, and it is
// what ring 0 produces. A []byte here would leave ring 3 to choose a source
// address, and any choice but the one the checksum was computed over produces
// a frame every receiver drops.
//
// Received DELIVERS EVERYTHING THE SOCKET GAVE IT, unfiltered, for ARP's
// reason: which frames matter is a protocol question and it is answered above
// this port, not in the socket where no test can see it. The manager decodes
// each frame, routes a Router Advertisement to EvRouterAdvert and drops the
// rest with a counter.
//
// WHAT IT DOES NOT DO IS RUN DUPLICATE ADDRESS DETECTION. The machine emits
// proto.ActStartDAD and waits for a proto.EvDADResult; performing RFC 4862
// section 5.4's exchange — joining the solicited-node multicast group, sending
// the Neighbor Solicitations, timing the wait, and reading RFC 7527 section
// 4.1's looped-back frames back out — is ring 3's, and it is M7c's to build.
// This port is how those frames get on and off the link; DAD is the caller of
// it.
type ND interface {
	Send(pkt wire.ICMPv6Packet) error
	Received() <-chan NDInbound
	Close() error
}

// Journal6 records every Machine6 Step. See proto.JournalEntry6.
//
// It is a second port beside Journal for JournalEntry6's reason: the recorded
// from- and to-states are the whole point of a journal entry, and proto.State
// and proto.State6 are two enumerations. One port taking both would record two
// states per Step of which two are always zero.
type Journal6 interface {
	Append(proto.JournalEntry6)
	Entries() []proto.JournalEntry6
}

// CapturedPacketV6 is one DHCPv6 or Neighbor Discovery frame in or out,
// decoded, with a timestamp.
//
// It is a second capture type beside CapturedPacket because the decoded
// message types differ; the ring is the same shape and the same rules apply,
// including "a message that FAILED to decode is the one worth having".
type CapturedPacketV6 struct {
	At        time.Time
	Dir       Direction
	Raw       []byte
	Msg       *wire.MessageV6
	DecodeErr error

	// RA is set instead of Msg when this capture is a Router Advertisement
	// the manager admitted. Only the admitted ones are captured, for
	// CapturedPacket.ARP's reason: a shared link carries Neighbor Discovery
	// continuously and this ring is bounded.
	RA *wire.RouterAdvert

	// RS is true when this capture is an outgoing Router Solicitation, which
	// carries no decoded form worth keeping: RFC 4861 section 4.1's message
	// has no fields this client varies.
	RS bool
}

// PacketRingV6 is the bounded ring of every v6 message in and out.
type PacketRingV6 interface {
	Record(CapturedPacketV6)
	Packets() []CapturedPacketV6
}

// DADRunner performs RFC 4862 section 5.4's duplicate address detection for
// one address and reports the verdict.
//
// IT IS THE OTHER HALF OF proto.ActStartDAD, and it exists because that action
// otherwise reached nobody. The machine emits it, arms proto.DADTimeout, and
// waits for exactly one proto.EvDADResult per address; before this port the
// only thing that could supply that result was a caller calling
// Manager.ReportDADResult by hand, so a client wired to real sockets and left
// alone would fail every acquisition on the deadline. See Manager's
// ActStartDAD arm.
//
// IT IS OPTIONAL, AND THE NIL VALUE IS THE BEHAVIOUR THAT SHIPPED BEFORE IT.
// A Config without one counts the request and journals it and nothing else,
// exactly as before, so a caller supplying its own answer through
// ReportDADResult is unaffected and no test that did so has to change.
// runtime.NewClient6 always supplies one, because a client that owns the
// sockets has no excuse not to.
//
// Start MUST NOT BLOCK. It is called from the manager's own goroutine in the
// middle of a Step, and RFC 4862 section 5.4.2's schedule is at least
// RetransTimer long; a Start that waited for the verdict would stop the
// manager answering anything for the duration, including the very exchange the
// address came from.
//
// report IS CALLED EXACTLY ONCE PER Start, from another goroutine, and it is a
// callback rather than a reference back to the Manager for a construction
// reason: a runner holding the manager and a manager holding the runner is a
// cycle, and the place that has to break it is the place a test cannot reach.
// Calling it twice for one address answers a question ring 1 asked once —
// proto.Machine6's takeDADResult ignores and journals the second, so the
// damage is bounded, but the count is then wrong and the count is the evidence.
// Not calling it at all is the case proto.DADTimeout exists for.
type DADRunner interface {
	Start(addr netip.Addr, report func(addr netip.Addr, duplicate bool))
}
