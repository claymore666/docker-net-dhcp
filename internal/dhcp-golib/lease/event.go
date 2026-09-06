package lease

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// Lease is what a caller gets. It is about the LEASE, not about the protocol
// (requirement U3): no state names, no message types, and absolute wall-clock
// deadlines rather than the monotonic Instants ring 1 works in.
type Lease struct {
	// Addr is the leased address with its prefix length: the server's option
	// 1 mask for v4, and a /128 for v6.
	//
	// IT IS A /128 FOR v6 AND THAT IS NOT A NARROWING. RFC 9915 §21.6's IA
	// Address option carries an IPv6 address and no prefix length at all, and
	// §18.2.10.1 says of it: "Addresses obtained from an IA Address option
	// MUST NOT be used to form an implicit prefix with a length other than
	// 128." So the on-link prefix comes from a Router Advertisement (RFC 4861
	// §4.6.2) and never from DHCPv6. A caller that needs it reads
	// Event.Router, not this field.
	Addr    netip.Prefix
	Gateway netip.Addr
	DNS     []netip.Addr
	Domain  string
	MTU     int

	// ServerID and ServerDUID both name the server that granted the lease,
	// and EXACTLY ONE OF THEM IS SET, per family: ServerID is RFC 2131
	// option 54's IPv4 address, ServerDUID is RFC 9915 §21.3's opaque octets.
	//
	// They are two fields rather than one because they are not the same kind
	// of thing. §11: "Clients and servers MUST treat DUIDs as opaque values
	// and MUST only compare DUIDs for equality." — no structure a client
	// interprets, and not an address: a v6 server is reached
	// by multicasting to All_DHCP_Relay_Agents_and_Servers, not by sending to
	// its DUID. Folding them into one netip.Addr would require inventing an
	// address for a value that has none.
	ServerID   netip.Addr
	ServerDUID []byte

	// IAID is the identity association this v6 lease belongs to (RFC 9915
	// §12), zero for v4. It is the caller's Params6.IAID echoed back: the
	// binding is keyed on (DUID, IA type, IAID), so a record that lost it
	// could not tell a server which binding it is renewing.
	IAID uint32

	// Preferred and Valid are RFC 9915 §7.1's two v6 lifetimes as wall-clock
	// deadlines, zero for v4 and zero for an infinite lifetime.
	//
	// THEY ARE TWO DEADLINES AND NOT ONE, which is the v6 lease's shape and
	// the reason they are not folded into Expire. §7.1: a preferred lifetime
	// is "the length of time that a valid address is preferred", after which
	// "the address becomes deprecated" — RFC 4862 §5.5.4 says a deprecated
	// address is still usable for an established connection and must not be
	// chosen for a new one. Expire is the valid lifetime, which is when the
	// address goes away; Preferred is when the chassis should stop starting
	// new connections on it. A caller that had only Expire would either drop
	// a usable address early or open new connections on a deprecated one.
	Preferred time.Time
	Valid     time.Time

	// Routes is the classless static routes of option 121, or option 33's
	// when the server sent no 121 (RFC 3442). Gateway is the default route
	// among them, so a caller that only wants a gateway can ignore this.
	Routes []wire.Route

	// DomainSearch is option 119's search list (RFC 3397). Separate from
	// Domain, which is option 15's single name: a resolver configuration
	// needs both and they are not the same field.
	DomainSearch []string

	// Acquired is when the REQUEST that produced this lease was sent, not
	// when its ACK arrived — RFC 2131 section 4.4.5. Renew and Rebind are T1
	// and T2, defaulted to 0.5 and 0.875 of the lease when the server sent
	// neither.
	//
	// A zero Expire means an infinite lease. That is the protocol's
	// 0xFFFFFFFF — RFC 2131 section 3.3's option 51 and RFC 9915 section 7.7's
	// "0xffffffff ... represents infinity" alike — and it is represented as a
	// zero Time rather than as a huge one so that "no expiry" is a value a
	// caller can test rather than a threshold it has to guess.
	//
	// For v6, Acquired is when the message that produced the lease was SENT,
	// which is a DELIBERATE DEVIATION and not the RFC's own origin: RFC 9915
	// section 4.2 defines T1 as "interpreted as a time interval since the
	// message's reception", and section 18.2.10.1 says to "Calculate T1 and
	// T2 times (based on T1 and T2 values sent in the message and the message
	// reception time)". proto.Lease6.Start states why this client measures
	// from the send instead and what the deviation costs. Renew is T1, Rebind
	// is T2, and Expire is the
	// longest valid lifetime in the IA — the same field with the same meaning
	// as Valid, kept so that a caller with no family-specific code has one
	// expiry to read.
	Acquired time.Time
	Renew    time.Time
	Rebind   time.Time
	Expire   time.Time

	// Options is every option from the ACK, unparsed.
	Options wire.Options
}

func (l Lease) String() string {
	if len(l.ServerDUID) > 0 {
		return fmt.Sprintf("%s from %x until %s", l.Addr, l.ServerDUID, l.Expire.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s via %s until %s", l.Addr, l.Gateway, l.Expire.Format(time.RFC3339))
}

// EventKind is what happened to the lease.
type EventKind uint8

// The outward events.
const (
	// Acquired: a lease the caller did not have.
	Acquired EventKind = iota
	// Changed: a lease whose contents differ from the one the caller had.
	Changed
	// Renewed: the lease was EXTENDED. Emitted on every DHCPACK that answers
	// a renewal, whether or not anything in the lease changed — a Changed
	// arrives beside it when something did.
	//
	// It is what makes a client that is renewing distinguishable from one
	// that is stuck: a caller watching only Changed sees nothing at all
	// through a year of successful renewals.
	Renewed
	// Lost: the lease is gone. Reason says why.
	Lost
	// Failed: acquisition failed. Reason says why; the client keeps trying
	// unless the reason is terminal. This is the "notify the user that the
	// initialization process has failed and is restarting" of RFC 2131
	// section 3.1(5).
	Failed
	// Configured: RFC 9915 section 18.2.6's stateless answer arrived. It
	// carries Config and NO LEASE, which is the whole reason it is its own
	// kind rather than a Changed with an empty address (design Q7).
	//
	// Section 18.2.6: "The client uses an Information-request message to
	// obtain configuration information without requesting addresses and/or
	// delegated prefixes to be assigned." A caller told Changed{Lease{}}
	// would have to distinguish "the
	// lease's contents changed to nothing" from "there was never a lease
	// here"; the two mean opposite things to a chassis that installs
	// addresses, so they are two kinds.
	//
	// It arrives on the v6 path only. RFC 2131 has DHCPINFORM, which this
	// library does not send.
	Configured
)

// AllEventKinds is every EventKind, so a test that has to enumerate them
// takes the enumeration from one place.
//
// It exists for AllStates' reason (proto's defeat row D-1): a kind added to
// the constant block and not to this slice SHRINKS the domain of every test
// that walks it, which reports a smaller domain as fully covered.
// TestAllEventKindsIsEveryDeclaredKind is the test whose own domain is the
// EventKind space rather than this slice, which is why it is the one that can
// see a member go missing.
func AllEventKinds() []EventKind {
	return []EventKind{Acquired, Changed, Renewed, Lost, Failed, Configured}
}

func (k EventKind) String() string {
	switch k {
	case Acquired:
		return "acquired"
	case Changed:
		return "changed"
	case Renewed:
		return "renewed"
	case Lost:
		return "lost"
	case Failed:
		return "failed"
	case Configured:
		return "configured"
	default:
		return fmt.Sprintf("eventkind(%d)", uint8(k))
	}
}

// Event is one lease event.
//
// Reason is proto.Reason, a typed value — requirement U5 is that a caller can
// branch on the cause without string matching. Note is for humans and for the
// journal, and is never the thing to switch on.
type Event struct {
	Kind   EventKind
	Lease  Lease
	Reason proto.Reason
	Note   string

	// Requested is the address the client ASKED FOR, on Acquired only, and
	// the zero value means it asked for none. It is set when the caller
	// supplied Config.Resume (the INIT-REBOOT address) or
	// Params.RequestedIP (option 50 in the DHCPDISCOVER).
	//
	// It is here because RFC 2131 lets a server answer either with something
	// else: section 4.4.1 makes option 50 in a DHCPDISCOVER a MAY, and
	// section 4.4.2 accepts a DHCPACK for an INIT-REBOOT request "from any
	// server" on the xid alone. A caller that cannot see the difference
	// applies an address it did not ask for, and the plugin's `ip` option
	// then silently means nothing.
	//
	// IT IS A REPORT, NOT A VERDICT. This library binds the address the
	// server gave. Refusing it is the chassis's decision, because only the
	// chassis knows whether the container can be started with a different
	// address; the check is Requested.IsValid() && Requested != the lease's
	// address.
	Requested netip.Addr

	// ACD is where RFC 5227's conflict check stood when this event was
	// emitted. It is proto.ACDIdle for a client running with
	// proto.ConflictOff, which is the truth: that client runs no check.
	//
	// IT IS ON EVERY EVENT BECAUSE OF proto.ConflictAsync (D23). That client
	// is told Acquired while the probing is still running, so "is this address
	// checked yet" is a real question with a real answer, and the answer is
	// only here. A caller that persists the record and restarts inside the
	// window needs it to resume the probe rather than skip it — a lease
	// recorded as ACDProbing has not been cleared, and one recorded as
	// ACDDefending has.
	//
	// For proto.ConflictWait it is ACDAnnouncing on Acquired and never
	// earlier, because that mode does not announce the lease until the check
	// has passed. That difference between the two modes on the SAME field is
	// what TestTheModesDifferInWhenAcquiredIsEmitted reads.
	ACD proto.ACDPhase

	// DAD is where RFC 4862 section 5.4's check stood when this event was
	// emitted, and it is ACD's counterpart for the v6 path: proto.DADIdle on
	// every v4 event, because a v4 client runs RFC 5227 instead and reports
	// it in ACD.
	//
	// IT IS ALWAYS proto.DADPassed ON A v6 Acquired, and that is not a
	// tautology worth deleting: it is the assertion that this client does not
	// have proto.ConflictAsync's shape in v6. RFC 9915 section 18.2.10.1
	// ("The client performs the duplicate address detection before using the
	// received addresses for any traffic") leaves no room for the async mode
	// D23 gave v4, so there is no window in which a v6 caller holds an
	// unchecked address — and this field is where a caller can see that
	// rather than take it on trust.
	DAD proto.DADPhase

	// Config is the stateless configuration, on Configured only.
	Config Configuration

	// Family is the address family this event's manager runs, stamped by the
	// manager rather than supplied by a caller.
	//
	// It is here so that Record.Family is a DATA DEPENDENCY and not a
	// convention every call site has to remember: EventRecord copies it, and
	// a caller that recorded a v6 event under FamilyV4 would build a record
	// whose address family disagreed with its address.
	Family Family

	// Router is the last Router Advertisement this client saw, on every v6
	// event, and the zero value means it has seen none.
	//
	// It is a DIAGNOSTIC and never an instruction (design Q2). A caller whose
	// own deadline ran out on a link whose router says M=0 and O=0 — RFC 4861
	// section 4.2: "no information is available via DHCPv6" — has not hit a
	// bug, and this is the only thing that tells it which of the two
	// happened.
	Router proto.RouterObservation
}

// Configuration is RFC 9915 section 18.2.6's answer: what a server sends a client
// that has no addresses from it.
//
// It is also filled in on a v6 Acquired, because a Reply to a Request carries
// the same options beside the IA_NA and a caller should not have to run an
// Information-request to read the DNS servers it was already sent.
type Configuration struct {
	DNS    []netip.Addr
	Search []string

	// Refresh is when the client will ask again, on the WALL CLOCK, and a
	// zero value means never.
	//
	// RFC 9915 section 21.23 gives the two ends of that: a value below
	// IRT_MINIMUM is raised to it ("A client MUST use the refresh time
	// IRT_MINIMUM if it receives the option with a value less than
	// IRT_MINIMUM"), an absent option means IRT_DEFAULT, and 0xffffffff
	// "implies that the client should not refresh its configuration data
	// without some other trigger (such as detecting movement to a new link)"
	// — which is the zero Time here, on the same convention Lease.Expire
	// uses for an infinite lease.
	Refresh time.Time
}

func (e Event) String() string {
	switch e.Kind {
	case Configured:
		return fmt.Sprintf("configured: %d DNS server(s), %d search domain(s)", len(e.Config.DNS), len(e.Config.Search))
	case Acquired, Changed, Renewed:
		if e.Requested.IsValid() && e.Lease.Addr.IsValid() && e.Requested != e.Lease.Addr.Addr() {
			return fmt.Sprintf("%s %s (asked for %s)", e.Kind, e.Lease, e.Requested)
		}
		return fmt.Sprintf("%s %s", e.Kind, e.Lease)
	default:
		return fmt.Sprintf("%s %s: %s", e.Kind, e.Reason, e.Note)
	}
}

// clockBridge converts a monotonic Instant to wall-clock time.
//
// It is built from one PAIR of readings taken at the same moment, and it is
// the only place in the library where the two clocks meet. Taking the two
// readings separately at each conversion would let a wall-clock step land
// between them, which is precisely the error the monotonic clock exists to
// avoid.
type clockBridge struct {
	mono proto.Instant
	wall time.Time
}

func bridge(c Clock) clockBridge {
	// Order matters only in that the gap between the two calls is the error
	// bound. Both are cheap vDSO reads.
	return clockBridge{mono: c.Mono(), wall: c.Wall()}
}

func (b clockBridge) at(i proto.Instant) time.Time {
	return b.wall.Add(time.Duration(i.Sub(b.mono)))
}

// instant is at's inverse: a wall-clock deadline that outlived the process
// that computed it, expressed in the monotonic clock this process is running
// on.
//
// It exists for exactly one input — the remembered lease's expiry, read back
// from a record written by a PREVIOUS run — and it is the only direction that
// crosses that way. A monotonic epoch means nothing to the next process, which
// is why the record stores wall-clock deadlines; ring 1 cannot import time,
// which is why they have to come back across here.
//
// THE STEP RISK IS REAL AND IS ACCEPTED. A wall clock that jumped while this
// client was not running moves the converted deadline by the size of the jump,
// so an NTP correction can make a live remembered lease look expired or the
// reverse. The alternative is to keep no deadline at all and INIT-REBOOT
// unconditionally, which RFC 2131 section 4.3.2 says buys a retransmission
// budget of silence from any server with no record of the client. One
// conversion, at construction, is the smaller error.
func (b clockBridge) instant(t time.Time) proto.Instant {
	return b.mono.Add(proto.Duration(t.Sub(b.wall)))
}

// toLease converts ring 1's Lease into the outward one.
func toLease(l proto.Lease, b clockBridge) Lease {
	out := Lease{
		Addr:         l.Addr,
		DNS:          append([]netip.Addr(nil), l.DNS...),
		Domain:       l.Domain,
		MTU:          l.MTU,
		ServerID:     l.ServerID,
		Routes:       append([]wire.Route(nil), l.Routes...),
		DomainSearch: append([]string(nil), l.DomainSearch...),
		Acquired:     b.at(l.Start),
		Options:      l.Options.Clone(),
	}
	// proto.Lease.Gateway, not Router[0]: after RFC 3442 the default route can
	// come from option 121, and a server sending 121 is required to have its
	// router option ignored. Reading Router here would give the gateway the
	// RFC says to discard.
	if g, ok := l.Gateway(); ok {
		out.Gateway = g
	}
	if t, ok := l.Expire(); ok {
		out.Expire = b.at(t)
	}
	if t, ok := l.RenewAt(); ok {
		out.Renew = b.at(t)
	}
	if t, ok := l.RebindAt(); ok {
		out.Rebind = b.at(t)
	}
	return out
}

// toLease6 converts ring 1's Lease6 into the outward one.
//
// IT PRODUCES THE SAME lease.Lease THE v4 PATH DOES, which is D30 at the
// boundary a caller actually touches: a chassis that installs an address, sets
// a route and writes a resolver file has one type to read whichever family it
// asked for, and the fields that do not apply are zero. The alternative — a
// second outward lease type — would double every consumer of this package for
// a difference of four fields.
func toLease6(l proto.Lease6, b clockBridge) Lease {
	out := Lease{
		DNS:          append([]netip.Addr(nil), l.DNS...),
		DomainSearch: append([]string(nil), l.Search...),
		ServerDUID:   append([]byte(nil), l.ServerDUID...),
		IAID:         l.IAID,
		Acquired:     b.at(l.Start),
	}
	if pfx, ok := l.Prefix(); ok {
		out.Addr = pfx
	}
	// Domain is option 15's single name and has no DHCPv6 counterpart: RFC
	// 3646 defines a search LIST (option 24) and no single-name option, so
	// filling Domain from Search[0] would invent a fact the server did not
	// send.
	d := l.Deadlines()
	if d.HasExpire {
		out.Expire = b.at(d.Expire)
		out.Valid = out.Expire
	}
	if d.HasRenew {
		out.Renew = b.at(d.Renew)
	}
	if d.HasRebind {
		out.Rebind = b.at(d.Rebind)
	}
	if t, ok := l.PreferredUntil(); ok {
		out.Preferred = b.at(t)
	}
	return out
}

// toConfig converts ring 1's stateless configuration into the outward one.
//
// THE REFRESH TIME COMES FROM c AND FROM NOWHERE ELSE. It used to arrive as a
// second parameter beside c, which is how one fact came to be derived twice:
// ring 1 stamped the raw option value into c.RefreshTime and armed its own
// timer from §21.23's bounded one, and this function copied whichever it was
// handed. Config6.RefreshTime is now the bounded value, and the parameter that
// let the two disagree is gone.
func toConfig(c proto.Config6, b clockBridge) Configuration {
	out := Configuration{
		DNS:    append([]netip.Addr(nil), c.DNS...),
		Search: append([]string(nil), c.Search...),
	}
	if c.RefreshTime > 0 && !c.RefreshTime.IsInfinite() {
		out.Refresh = b.at(b.mono.Add(c.RefreshTime))
	}
	return out
}
