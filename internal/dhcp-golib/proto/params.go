package proto

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// Params is everything the machine needs that is not an event.
//
// A value, copied into the Machine at New and never mutated, so that a replay
// constructs the same Machine from the same Params and nothing outside can
// change the configuration between two Steps.
type Params struct {
	// CHAddr is the client hardware address. Required: RFC 2131 section 4.4.1
	// makes it a MUST when it is needed for reply delivery, which it always is
	// on a broadcast segment.
	CHAddr []byte

	// ClientID is option 61. Empty means the option is not sent.
	//
	// M1 sends whatever it is handed. Design decision D10 — whether this
	// becomes an RFC 4361 type-255 IAID+DUID — is the caller's to make and is
	// still open; the machine deliberately does not derive an identifier of
	// its own, so it cannot freeze D10 by accident.
	ClientID []byte

	// Hostname is option 12. Empty means the option is not sent.
	Hostname string

	// VendorClass is option 60. Empty means the option is not sent.
	VendorClass string

	// ParameterList is option 55. Empty means DefaultParameterList is sent.
	//
	// RFC 2131 section 4.4.1: "If the client included a list of requested
	// parameters in a DHCPDISCOVER message, it MUST include that list in all
	// subsequent messages." The machine holds one list and sends it in both,
	// so that MUST cannot be violated by editing one call site.
	ParameterList []wire.OptionCode

	// RequestedIP is a hint placed in option 50 of the DISCOVER. RFC 2131
	// section 4.4.1 makes it a MAY there. It is NOT what drives the REQUEST:
	// that carries the OFFER's yiaddr, which is a MUST.
	//
	// A hint is not a demand: the server may offer something else and this
	// machine takes what it is offered. What it does not do is stay silent
	// about it — the acquisition carries Action.Requested, so the caller can
	// see that the address it asked for is not the address it got and decide.
	// See Machine.requestedAddr.
	RequestedIP netip.Addr

	// Resume is a lease remembered from a previous run of this client, and it
	// is what turns the first DHCPREQUEST into RFC 2131 section 4.4.2's
	// INIT-REBOOT one instead of a DHCPDISCOVER.
	//
	// Nil means a fresh acquisition. See Resume for what it carries and, more
	// to the point, what it does not.
	Resume *Resume

	// RequestedLease is option 51 in the DISCOVER and REQUEST. Zero means the
	// option is not sent and the server chooses.
	RequestedLease Duration

	// Broadcast sets the BROADCAST flag (RFC 2131 section 2), asking the
	// server to broadcast its replies.
	//
	// TRUE in DefaultParams. The flag exists for "a client that cannot receive
	// unicast IP datagrams until its protocol software has been configured
	// with an IP address" (RFC 2131 section 4.1), which is exactly a raw
	// AF_PACKET socket on an unconfigured interface. Clearing it while ring 3
	// is a raw socket produces a client that works against servers ignoring
	// the flag and hangs against those honouring it.
	Broadcast bool

	// Discover and Request are the retransmission schedules for the two
	// transactions. See Backoff.
	Discover Backoff
	Request  Backoff

	// DesyncMin and DesyncMax bound the startup delay of RFC 2131 section
	// 4.4.1: "The client SHOULD wait a random time between one and ten seconds
	// to desynchronize the use of DHCP at startup."
	//
	// Both zero disables it — a configuration, not an opt-out: the delay
	// desynchronises a fleet of hosts booting together, and a single container
	// acquiring one lease has nothing to desynchronise from. The RFC defaults
	// are pinned by TestDesyncWindowIsWithinTheRFC.
	DesyncMin Duration
	DesyncMax Duration

	// RestartDelay is the wait after a DHCPDECLINE before the configuration
	// process restarts (RFC 2131 section 3.1(5)).
	//
	// DECISION 2026-08-30: zero means DefaultRestartDelay, NOT "no wait" —
	// deliberately unlike DesyncMin/DesyncMax, where both zero disables the
	// delay. A desync window has nothing to do when one container acquires one
	// lease. This wait is the opposite case: it exists precisely because a
	// single client whose address is permanently in use is the thing that
	// loops, so the configuration that most wants it is the one a caller is
	// most likely to leave at zero.
	RestartDelay Duration

	// MaxSendFailures is how many consecutive failed sends are tolerated
	// before the machine gives up on the transport and reports
	// ReasonTransport.
	//
	// R2's visible consequence: without it, a machine whose every send fails
	// sits in SELECTING re-arming a timer forever, reporting nothing, looking
	// exactly like one waiting for a slow server.
	//
	// It does NOT apply while a lease is held and being renewed. See
	// Machine.noteActionFailed.
	MaxSendFailures int

	// Servers is the policy on which DHCP servers this client will deal with.
	// Empty means every server.
	Servers ServerPolicy

	// FQDN is option 81 (RFC 4702). A zero value means the option is not
	// sent.
	//
	// SETTING IT SUPPRESSES Hostname. RFC 4702 section 3.1: "clients that
	// send the Client FQDN option in their messages MUST NOT also send the
	// Host Name option". Enforced in Machine.base rather than refused at New,
	// because a caller filling in both is asking for a name to be published
	// and the RFC says which option carries it.
	FQDN FQDN

	// fqdn is FQDN encoded once at New. Unexported because it is derived: a
	// caller that could set it could put bytes on the wire that
	// wire.EncodeFQDN refuses, which is the check New exists to run.
	fqdn []byte
}

// Resume is an address this client held before it was restarted, and the
// moment that holding stops being worth asserting.
//
// It carries the ADDRESS AND THE DEADLINE AND NOTHING ELSE, and the omission is
// the guard rather than a saving. RFC 2131 Table 4 (section 4.3.6) and Table 5
// (section 4.4.1) both make the 'server identifier' option a MUST NOT in the
// message this value produces, and section 4.3.2 says what a server does with a
// DHCPREQUEST that carries one: it reads the message as one "generated during
// SELECTING state", compares the identifier against its own, and stays SILENT
// when it does not match. Silence is also what a message that never left the
// host produces, so the failure is a timeout that looks like a slow server.
//
// A remembered lease HAS a server identifier — lease.Lease carries one, and the
// renewal path needs it. Not carrying it here is what makes the wrong message
// impossible to build rather than merely absent from today's builder: a future
// edit to sendReboot cannot fill in an option whose value is not in reach.
//
// Section 4.4.4's "the client may use that address in the DHCPDISCOVER or
// DHCPREQUEST rather than the IP broadcast address" in REBOOTING is the one
// thing this omission rules out. Table 4's INIT-REBOOT column says broadcast,
// the MAY is a MAY, and a client on a link whose server has moved is better
// served by the broadcast.
type Resume struct {
	// Addr is the remembered address, and it is what option 50 of the
	// INIT-REBOOT DHCPREQUEST carries: section 4.3.2's "'requested IP address'
	// option MUST be filled in with client's notion of its previously assigned
	// address".
	Addr netip.Addr

	// Expire is when the remembered lease runs out, on the SAME monotonic
	// clock Step is fed. HasExpire false means the remembered lease is
	// infinite and never stops being worth asserting.
	//
	// Ring 1 cannot read a clock and cannot hold a wall-clock time (the T1
	// gate refuses "time"), so the conversion from the wall-clock deadline a
	// record stores is ring 2's, done ONCE through the one clock bridge in the
	// library. See lease.Config.Resume.
	//
	// WHY THE DEADLINE TRAVELS AT ALL. Section 4.3.2: a server "MUST remain
	// silent" for a DHCPREQUEST from a client it has no record of. An expired
	// lease is exactly the case where no server has a record, so rebooting one
	// buys a full retransmission budget of silence and then the DISCOVER that
	// should have been sent first. The machine refuses it at EvStart and says
	// so in the journal.
	Expire    Instant
	HasExpire bool
}

// live reports whether this remembered lease is still worth asserting at now.
func (r *Resume) live(now Instant) bool {
	if r == nil || !r.Addr.Is4() || r.Addr.IsUnspecified() {
		return false
	}
	return !r.HasExpire || r.Expire.After(now)
}

// clone deep-copies a Resume. netip.Addr is a value, so this is the pointer and
// nothing else — but a shallow copy of the POINTER is what makes a snapshot
// alias the caller's memory, which is the defect SnapshotParams exists to
// prevent.
// Clone deep-copies a Resume. It is exported because lease.SnapshotParams has
// to reach it: Resume is the only pointer in Params, and a record that shared
// one with its caller would say what the caller's memory holds now rather than
// what the manager ran with.
func (r *Resume) Clone() *Resume {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// ServerPolicy decides which servers this client accepts, by the 'server
// identifier' option (54) their messages carry.
//
// It is a protocol predicate and lives in the pure ring so it can be tabled,
// not in whatever assembles the client. The plugin's dhcp_servers and
// dhcp_deny_servers map onto Allow and Deny with no translation.
//
// It is NOT authentication. A server identifier is a value in a datagram
// anyone on the link can send; an allow list narrows which claimed identities
// this client will act on and proves nothing about who sent them.
type ServerPolicy struct {
	// Allow, when non-empty, is the only set of server identifiers this
	// client will take a lease from or accept a refusal from.
	Allow []netip.Addr
	// Deny is the set it will not, whatever Allow says.
	Deny []netip.Addr
}

// permits reports whether a message carrying this server identifier may be
// acted on, and returns the reason when it may not.
//
// present says whether the message carried the option at all, which is a
// third case and not the same as a zero address:
//
//   - DENY WINS. A server named in both lists is denied. The alternative
//     ordering makes a deny list something an allow list can silently cancel,
//     and an operator writing both means the second one to exclude.
//   - AN ALLOW LIST FAILS CLOSED on an absent identifier. "Only these servers"
//     that a message can satisfy by omitting the field is not a restriction.
//   - A DENY LIST ALONE FAILS OPEN on an absent identifier, because nothing
//     can show the message came from a denied server. That direction is
//     stated rather than hidden: a client with only a deny list configured
//     still acts on a message that names no server.
func (s ServerPolicy) permits(sid netip.Addr, present bool) (bool, string) {
	if present {
		for _, d := range s.Deny {
			if d == sid {
				return false, "server " + sid.String() + " is on the deny list"
			}
		}
	}
	if len(s.Allow) == 0 {
		return true, ""
	}
	if !present {
		return false, "an allow list is configured and this message carries no server identifier"
	}
	for _, a := range s.Allow {
		if a == sid {
			return true, ""
		}
	}
	return false, "server " + sid.String() + " is not on the allow list"
}

// FQDN is option 81's contents (RFC 4702).
//
// Flags are the caller's: the library refuses at New the three combinations
// section 2.1 forbids a CLIENT to send, and otherwise sends what it is given,
// because which DNS updates to ask for is policy and not protocol.
type FQDN struct {
	// Name is the client's fully or partially qualified domain name. A
	// trailing dot makes it fully qualified. Empty means option 81 is not
	// sent at all.
	Name string
	// Flags is the RFC 4702 section 2.1 octet. Zero with a non-empty Name
	// means DefaultFQDNFlags.
	Flags uint8
}

// DefaultFQDNFlags is S|E: ask the server to update the A RR as well as the
// PTR RR it always owns, and encode the name in the canonical wire format RFC
// 4702 section 2.1 says clients SHOULD use.
const DefaultFQDNFlags = wire.FQDNFlagS | wire.FQDNFlagE

// flags resolves the zero value.
func (f FQDN) flags() uint8 {
	if f.Flags == 0 {
		return DefaultFQDNFlags
	}
	return f.Flags
}

// DefaultRestartDelay is RFC 2131 section 3.1(5)'s "minimum of ten seconds"
// before restarting the configuration process after a DHCPDECLINE. Pinned by
// TestRestartDelayMeetsTheRFCMinimum.
const DefaultRestartDelay = 10 * Second

// DefaultParameterList is option 55's default contents.
//
// THE ORDER IS NORMATIVE AND IS NOT ASCENDING. RFC 3442's client behaviour:
// "The Classless Static Routes option code MUST appear in the parameter
// request list prior to both the Router option code and the Static Routes
// option code, if present", and the same paragraph makes requesting BOTH 121
// and 3 a MUST for a client that supports 121. Sorting this list breaks a MUST
// silently, which is why 121 leads it and why the order is pinned by a test on
// the ENCODED option rather than by this comment.
//
// It asks for more than this library turns into a Lease, and that is
// deliberate. Every option a server sends is kept verbatim in Lease.Options
// (requirements section 9, choice 1: a forgotten option is recoverable rather
// than gone), and a server generally sends only what was asked for — so an
// option left out of this list is not merely unparsed, it never arrives, and
// the pass-through bag cannot recover it.
func DefaultParameterList() []wire.OptionCode {
	return []wire.OptionCode{
		wire.OptClasslessStaticRte,
		wire.OptRouter,
		wire.OptStaticRoute,
		wire.OptSubnetMask,
		wire.OptDNSServer,
		wire.OptDomainName,
		wire.OptDomainSearch,
		wire.OptInterfaceMTU,
		wire.OptBroadcastAddress,
		wire.OptNTPServer,
		wire.OptTimeOffset,
		wire.OptPosixTimezone,
		wire.OptTZDatabase,
		wire.OptTFTPServer,
		wire.OptBootfileName,
		wire.OptWPAD,
	}
}

// DefaultParams returns the RFC's schedules and delays for a client with the
// given hardware address. Everything a caller must decide is left zero.
func DefaultParams(chaddr []byte) Params {
	return Params{
		CHAddr:          append([]byte(nil), chaddr...),
		ParameterList:   DefaultParameterList(),
		Broadcast:       true,
		Discover:        DefaultBackoff(),
		Request:         DefaultBackoff(),
		DesyncMin:       1 * Second,
		DesyncMax:       10 * Second,
		RestartDelay:    DefaultRestartDelay,
		MaxSendFailures: 5,
	}
}

// ErrNoCHAddr is returned by New when the hardware address is missing.
var ErrNoCHAddr = errors.New("proto: Params.CHAddr is required")

// ErrCHAddrTooLong is returned by New when the hardware address cannot fit the
// BOOTP field.
var ErrCHAddrTooLong = errors.New("proto: Params.CHAddr is longer than 16 octets")

// ErrBadDesync is returned by New when the desync window is inverted.
var ErrBadDesync = errors.New("proto: Params.DesyncMin is greater than Params.DesyncMax")

// ErrBadRestartDelay is returned by New for a negative restart delay. A
// negative delay is refused rather than clamped: it is a caller error, and
// clamping it to the default would hide the mistake behind correct behaviour.
var ErrBadRestartDelay = errors.New("proto: Params.RestartDelay is negative")

// ErrBadFQDN is returned by New for an option 81 configuration that cannot be
// sent. Refused at construction rather than at the first DHCPDISCOVER: a
// client that only discovers its own misconfiguration when it tries to lease
// has already been deployed.
var ErrBadFQDN = errors.New("proto: Params.FQDN cannot be encoded")

// ErrBadResume is returned by New for a Resume that names no usable IPv4
// address. See Params.validate.
var ErrBadResume = errors.New("proto: Params.Resume names no usable IPv4 address")

func (p Params) validate() error {
	if len(p.CHAddr) == 0 {
		return ErrNoCHAddr
	}
	if len(p.CHAddr) > 16 {
		return fmt.Errorf("%w: %d", ErrCHAddrTooLong, len(p.CHAddr))
	}
	if p.DesyncMin < 0 || p.DesyncMax < 0 || p.DesyncMin > p.DesyncMax {
		return fmt.Errorf("%w: [%s, %s]", ErrBadDesync, p.DesyncMin, p.DesyncMax)
	}
	if p.RestartDelay < 0 {
		return fmt.Errorf("%w: %s", ErrBadRestartDelay, p.RestartDelay)
	}
	if p.Resume != nil && (!p.Resume.Addr.Is4() || p.Resume.Addr.IsUnspecified()) {
		// Refused at construction rather than ignored at EvStart. A Resume
		// carrying no usable address is a caller that meant to remember one;
		// silently falling back to a DISCOVER gives it the behaviour it was
		// trying to replace and no way to find out.
		return fmt.Errorf("%w: %s", ErrBadResume, p.Resume.Addr)
	}
	if p.FQDN.Name != "" {
		if _, err := wire.EncodeFQDN(p.FQDN.flags(), p.FQDN.Name); err != nil {
			return fmt.Errorf("%w: %w", ErrBadFQDN, err)
		}
	}
	return nil
}

func (p Params) restartDelay() Duration {
	if p.RestartDelay <= 0 {
		return DefaultRestartDelay
	}
	return p.RestartDelay
}

// desync returns the startup delay for this entropy value.
func (p Params) desync(rnd uint64) Duration {
	span := p.DesyncMax - p.DesyncMin
	if span <= 0 {
		return p.DesyncMin
	}
	return p.DesyncMin + Duration(rnd%uint64(span+1))
}

func (p Params) parameterList() []wire.OptionCode {
	if len(p.ParameterList) == 0 {
		return DefaultParameterList()
	}
	return p.ParameterList
}
