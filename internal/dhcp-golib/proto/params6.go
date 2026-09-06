package proto

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// The client half of Params6: what this node is, what it wants, and the two
// schedules neighbour discovery owns rather than RFC 9915.
//
// The retransmission half lives in backoff6.go, which is where §7.6's table
// is. The split is by AUTHORITY and not by tidiness: a server can rewrite
// SolMaxRT and InfMaxRT on any message (§21.24, §21.25), and nothing on the
// wire can rewrite a DUID.

// The RFC 4861 §10 host constants this client uses, quoted from the "Host
// constants" list: "RTR_SOLICITATION_INTERVAL 4 seconds",
// "MAX_RTR_SOLICITATIONS 3 transmissions".
const (
	RtrSolicitationInterval = 4 * Second
	MaxRtrSolicitations     = 3
)

// The RFC 4861 §10 node constants the DAD deadline is computed from:
// "MAX_MULTICAST_SOLICIT 3 transmissions", "RETRANS_TIMER 1,000
// milliseconds".
const (
	MaxMulticastSolicit = 3
	RetransTimer        = 1000 * Millisecond
)

// DupAddrDetectTransmits is RFC 4862 §5.1's variable of that name, at its
// stated default: "Default: 1, but may be overridden by a link-type specific
// value in the document that covers issues related to the transmission of IP
// over a particular link type".
const DupAddrDetectTransmits = 1

// DefaultDADTimeout is how long Machine6 waits for the EvDADResult that ring 3
// owes it, COMPUTED from the three constants above rather than chosen.
//
// The nominal case is RFC 4862 §5.1: RetransTimer "specifies the delay between
// consecutive Neighbor Solicitation transmissions performed during Duplicate
// Address Detection (if DupAddrDetectTransmits is greater than 1), as well as
// the time a node waits after sending the last Neighbor Solicitation before
// ending the Duplicate Address Detection process." With the default
// DupAddrDetectTransmits that is one solicitation and one RetransTimer wait.
//
// The worst case is RFC 7527 §4.1, which the chassis's DAD may be running
// because RFC 7527 updates RFC 4862: "If any probe is looped back within
// RetransTimer milliseconds after having sent DupAddrDetectTransmits NS(DAD)
// messages, the interface continues with another MAX_MULTICAST_SOLICIT number
// of NS(DAD) messages transmitted RetransTimer milliseconds apart." That adds
// MAX_MULTICAST_SOLICIT further RetransTimer intervals before the address can
// be declared assigned.
//
// So the deadline is (DupAddrDetectTransmits + MAX_MULTICAST_SOLICIT) ×
// RETRANS_TIMER. It is a CEILING ON RING 3's ANSWER and not a schedule this
// ring runs: ring 1 sends no Neighbor Solicitation, and every duration it
// waits is one it must be able to justify without a clock of its own.
const DefaultDADTimeout = (DupAddrDetectTransmits + MaxMulticastSolicit) * RetransTimer

// DefaultMaxSendFailures6 is the consecutive-ActSendV6-failure budget. It
// matches the v4 machine's default for the reason D30 gives: the failure is
// the same failure — the transport cannot put a packet on the link — and a
// different number in the two families would be a difference with no cause.
const DefaultMaxSendFailures6 = 5

// Resume6 is a binding remembered across a restart, offered to Machine6 so it
// can Confirm rather than Solicit.
//
// IT IS THE V6 Resume, AND IT IS A DIFFERENT SHAPE FOR A REASON. v4's Resume
// carries one address and one lease expiry, because a v4 lease is one address.
// An IA_NA is a set (§21.4: "IA_NA-options: Options associated with this
// IA_NA"), each member with its own two lifetimes, and §18.2.3 says the
// Confirm "options include all of the addresses the client currently has
// associated with those IAs" — so a single-address Resume6 would silently drop
// every address after the first from the message that asks whether they are
// still on this link.
type Resume6 struct {
	// Addrs is the addresses to confirm, with the lifetimes last known for
	// them. §18.2.3 says the lifetimes sent in the Confirm SHOULD be zero —
	// "as the server will ignore these fields" — and the machine writes zeros;
	// they are carried here because §18.2.3's silence case needs them: "the
	// client SHOULD continue to use any leases, using the last known lifetimes
	// for those leases".
	Addrs []Addr6

	// ServerDUID is the DUID of the server that granted the binding, as sent.
	// It is what a Renew after a successful Confirm addresses (§18.2.4: "The
	// client MUST include a Server Identifier option ... identifying the
	// server with which the client most recently communicated").
	ServerDUID []byte

	// T1 and T2 are the last known renewal times. Zero means the same thing
	// here as it means on the wire, and Lease6.Deadlines applies §21.4's
	// recommendation at the point of use.
	T1, T2 Duration
}

// Clone deep-copies a Resume6, for the reason Resume.Clone exists: it is the
// one pointer in Params6, and a caller holding the value it passed in could
// otherwise move the remembered addresses out from under a machine that has
// already decided to confirm them.
func (r *Resume6) Clone() *Resume6 {
	if r == nil {
		return nil
	}
	out := *r
	out.Addrs = append([]Addr6(nil), r.Addrs...)
	out.ServerDUID = append([]byte(nil), r.ServerDUID...)
	return &out
}

// live reports whether this Resume6 names anything worth confirming.
func (r *Resume6) live() bool {
	if r == nil {
		return false
	}
	for _, a := range r.Addrs {
		if a.Addr.Is6() && !a.Addr.Is4In6() && !a.Addr.IsUnspecified() {
			return true
		}
	}
	return false
}

// The reasons New6 refuses a configuration. Each is its own sentinel because a
// caller's next step differs: a missing DUID is a chassis that has not been
// wired up, a bad hint is a caller passing an IPv4 address to a v6 client.
var (
	// ErrNoDUID is a Params6 with no client DUID.
	ErrNoDUID = errors.New("proto: Params6.DUID is required")
	// ErrBadHint is a Params6.Hint that is not a usable IPv6 address.
	ErrBadHint = errors.New("proto: Params6.Hint is not an IPv6 address")
	// ErrBadResume6 is a Params6.Resume that names no usable IPv6 address.
	ErrBadResume6 = errors.New("proto: Params6.Resume names no usable IPv6 address")
	// ErrBadDADTimeout is a negative Params6.DADTimeout.
	ErrBadDADTimeout = errors.New("proto: Params6.DADTimeout is negative")
	// ErrBadRetransmit6 is a §7.6 parameter that cannot produce a schedule.
	ErrBadRetransmit6 = errors.New("proto: Params6 carries a non-positive retransmission parameter")
)

// validate refuses a configuration the machine cannot run.
//
// It refuses at New6 rather than at the first send, which is the same choice
// Params.validate makes: a machine built from an unusable configuration would
// otherwise emit its first Action — a Solicit with no Client Identifier, which
// §18.2.1 makes a MUST — before anything could tell the caller.
func (p Params6) validate() error {
	if len(p.DUID) == 0 {
		return ErrNoDUID
	}
	if p.Hint.IsValid() && (!p.Hint.Is6() || p.Hint.Is4In6()) {
		return fmt.Errorf("%w: %s", ErrBadHint, p.Hint)
	}
	if p.Resume != nil && !p.Resume.live() {
		return ErrBadResume6
	}
	if p.DADTimeout < 0 {
		return fmt.Errorf("%w: %s", ErrBadDADTimeout, p.DADTimeout)
	}
	for _, c := range []struct {
		name string
		d    Duration
	}{
		{"SolTimeout", p.SolTimeout}, {"SolMaxRT", p.SolMaxRT},
		{"ReqTimeout", p.ReqTimeout}, {"ReqMaxRT", p.ReqMaxRT},
		{"CnfTimeout", p.CnfTimeout}, {"CnfMaxRT", p.CnfMaxRT},
		{"RenTimeout", p.RenTimeout}, {"RenMaxRT", p.RenMaxRT},
		{"RebTimeout", p.RebTimeout}, {"RebMaxRT", p.RebMaxRT},
		{"InfTimeout", p.InfTimeout}, {"InfMaxRT", p.InfMaxRT},
		{"RelTimeout", p.RelTimeout}, {"DecTimeout", p.DecTimeout},
	} {
		if c.d <= 0 {
			return fmt.Errorf("%w: %s is %s", ErrBadRetransmit6, c.name, c.d)
		}
	}
	return nil
}

// dadTimeout is DADTimeout with the zero value meaning the default.
//
// Zero is "the caller did not say" and not "do not wait": a machine that read
// a zero literally would arm a deadline that fires in the same Step that armed
// it, and every acquisition would fail with ReasonDADIncomplete before ring 3
// had been asked anything.
func (p Params6) dadTimeout() Duration {
	if p.DADTimeout <= 0 {
		return DefaultDADTimeout
	}
	return p.DADTimeout
}

// routerSolicitations is RouterSolicitations with the zero value meaning the
// RFC 4861 default. A NEGATIVE value means none: a caller on a link where
// router discovery is somebody else's job can turn the schedule off, and
// there is no other way to say it — zero is already taken by "unset".
func (p Params6) routerSolicitations() int {
	if p.RouterSolicitations == 0 {
		return MaxRtrSolicitations
	}
	if p.RouterSolicitations < 0 {
		return 0
	}
	return p.RouterSolicitations
}

func (p Params6) routerSolicitInterval() Duration {
	if p.RouterSolicitInterval <= 0 {
		return RtrSolicitationInterval
	}
	return p.RouterSolicitInterval
}

func (p Params6) maxSendFailures() int {
	if p.MaxSendFailures <= 0 {
		return DefaultMaxSendFailures6
	}
	return p.MaxSendFailures
}

// oro builds the Option Request option for one message type.
//
// The two mandatory codes are added HERE and not left to the caller, because
// both are MUSTs on this client and a caller cannot be asked to remember them.
// §21.24: "A DHCP client MUST include the SOL_MAX_RT option code in any Option
// Request option (see Section 21.7) it sends in a Solicit message." §21.25 says
// the same of INF_MAX_RT and the Information-request. §21.23 adds the third:
// "A DHCP client MUST request this option in the Option Request option (see
// Section 21.7) when sending Information-request messages. A client MUST NOT
// request this option in the Option Request option in any other messages."
//
// That last MUST NOT is why this takes the message type rather than returning
// one list: a client that requested option 32 everywhere would be as
// non-conformant as one that requested it nowhere, and the two mistakes are
// one line apart.
func (p Params6) oro(t wire.MessageTypeV6) []wire.OptionCodeV6 {
	var out []wire.OptionCodeV6
	switch t {
	case wire.MsgSolicit:
		out = append(out, wire.OptV6SolMaxRTCode)
	case wire.MsgInformationRequest:
		out = append(out, wire.OptV6InfMaxRTCode, wire.OptV6InfoRefresh)
	case wire.MsgRenew, wire.MsgRebind:
		// §18.2.4: "The client includes an Option Request option (see
		// Section 21.7) to request the SOL_MAX_RT option (see Section 21.24)
		// and any other options the client is interested in receiving."
		// §18.2.5 builds the Rebind "as described in Section 18.2.4".
		out = append(out, wire.OptV6SolMaxRTCode)
	}
	for _, c := range p.ORO {
		if !containsCode(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsCode(hay []wire.OptionCodeV6, c wire.OptionCodeV6) bool {
	for _, h := range hay {
		if h == c {
			return true
		}
	}
	return false
}

// DefaultORO is the option codes a caller usually wants beyond the mandatory
// ones: the two RFC 3646 lists. They are a DEFAULT and not built in, because
// unlike 82, 83 and 32 no RFC makes them mandatory and a caller running a
// resolver of its own has a reason not to ask.
func DefaultORO() []wire.OptionCodeV6 {
	return []wire.OptionCodeV6{wire.OptV6DNSServers, wire.OptV6DomainList}
}

// sameDUID compares two DUIDs by bytes, which is what §16.3 asks for: "the
// contents of the Client Identifier option do not match the client's DUID".
// A DUID has no structure this client interprets — §11 calls it "opaque" — so
// bytes is the whole comparison.
func sameDUID(a, b []byte) bool { return bytes.Equal(a, b) }

// hintAddr is the caller's address hint, or the zero Addr.
func (p Params6) hintAddr() netip.Addr {
	if p.Hint.Is6() && !p.Hint.Is4In6() && !p.Hint.IsUnspecified() {
		return p.Hint
	}
	return netip.Addr{}
}
