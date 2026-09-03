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
