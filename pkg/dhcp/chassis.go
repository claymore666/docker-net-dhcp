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
	"net/netip"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
)

// ErrNoLease is returned when an acquisition ended without one.
var ErrNoLease = errors.New("dhcp: no lease was acquired")

// ErrAddressConflict is an acquisition whose last failure was RFC
// 5227's: the address the server offered is already in use on the
// segment.
//
// A separate error because the operator action is different and the
// two are indistinguishable in a timeout log. "No server answered"
// means the network is broken; this means the DHCP server's pool
// overlaps something it cannot see -- a statically configured host
// inside the range -- and it will hand the same address out again.
var ErrAddressConflict = errors.New("dhcp: the offered address is already in use on this segment")

// DHCPClientOptions is one endpoint's DHCP configuration.
type DHCPClientOptions struct {
	// Hostname is DHCPv4 option 12. Empty omits it.
	Hostname string

	// FQDN, when non-empty, asks the server to register Hostname in DNS
	// (RFC 4702 option 81). The value is the legacy mode string; only
	// its emptiness is read.
	FQDN string

	// V6 selects DHCPv6 (RFC 9915) for this endpoint.
	//
	// IT SELECTS A DIFFERENT LIBRARY CLIENT, not a mode of one: the two
	// families are two state machines over two transports with two
	// identity schemes, and every entry point here routes on this bool
	// to the family's own constructor. An endpoint that wants both
	// families runs TWO of these, one per family, which is what the
	// plugin's Join manager does.
	V6 bool

	// Identity6 is the DHCPv6 client identity: the DUID this endpoint
	// is known by and the IAID of the identity association it asks for
	// (RFC 9915 sections 11 and 12). REQUIRED when V6 is set, and the
	// library refuses an empty DUID rather than inventing one.
	//
	// It is BYTES FROM THE CHASSIS, on the same rule as ClientID (D10):
	// the library never derives an identity, because an identity a
	// library invents per interface -- or worse, per process -- is one
	// that changes whenever the caller's plumbing does, and RFC 9915
	// section 11 says a DUID "SHOULD NOT change over time if at all
	// possible". The chassis mints it once, writes it into the durable
	// record, and reads it back on every restart. See identity6.go.
	Identity6 Identity6

	// HonorRouterAdverts asserts that this endpoint's link is under the
	// Router-Advertisement guard: accept_ra=0, autoconf=0 and
	// keep_addr_on_down=1 written and read back inside the container's
	// network namespace, and the routes the kernel had already installed
	// from an advertisement taken off it (#875, #821, ra_guard.go).
	//
	// THE FIRST TWO VALUES ARE THE OPPOSITE OF WHAT 2.0 SHIPPED, and the
	// reason is that the answer moved rather than that the argument
	// changed. DHCPv6 carries no router -- RFC 9915 section 21's option
	// catalogue has no next hop -- and RFC 5942 section 4 rule 1 forbids
	// deriving an on-link prefix from an assigned address, so router
	// discovery is RFC 4861 section 6.3.4 and somebody has to do it.
	// Until #821 that somebody was the container's kernel; it is now
	// THIS client, which reads advertisements off its own socket and
	// hands the gateway, MTU, routes and DNS to the plugin. A kernel
	// still acting on the same frames would install a second default
	// route beside the plugin's.
	//
	// IT IS NOT AN OPERATOR OPTION AND THERE IS NO WAY TO TURN IT OFF
	// (D30 Q3). A persistent v6 client built without it is REFUSED
	// rather than started, because a v6 endpoint with two default
	// routes, or with none, looks completely healthy from the outside.
	//
	// It is refused on every other shape -- v4, no namespace, the
	// CreateEndpoint one-shot -- because the values are host
	// configuration for a container's link, and the one-shot's link is
	// still in the HOST namespace when it runs.
	HonorRouterAdverts bool

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

	// Mode6 is where this endpoint's IPv6 address comes from: DHCPv6,
	// an advertised prefix, or whichever of the two the router names
	// (#817). It is the `ipv6_mode` option's value, parsed once at
	// CreateNetwork by ParseIPv6Mode, and it is read only when V6 is
	// set.
	//
	// IT IS SET ON THE SAME ASSIGNMENT AS Identity6 AND THAT IS THE
	// GUARD. proto.Mode6's zero value is Mode6DHCP, so a v6 call site
	// that forgot this field would get a working client in the mode the
	// plugin had before the option existed -- the silent half of defeat
	// row 1. Identity6 has no usable zero (buildParams6 refuses an
	// empty DUID), so the two travel through one helper and a site that
	// skips it fails at the refusal rather than running in the wrong
	// mode. pkg/plugin's v6 wiring helper is that one helper.
	Mode6 proto.Mode6

	// StrictAuto6 is the `ipv6_auto_strict` option: in Mode6Auto, a
	// router that said M=1 and a DHCPv6 server that then answers
	// nothing FAILS the endpoint instead of forming an address from an
	// advertised prefix.
	//
	// A BOOLEAN HERE AND A DURATION ON THE WIRE. The library's switch
	// is proto.Params6.AutoFallback, whose zero means "the caller did
	// not say" and whose NEGATIVE means strict; the operator's question
	// is "does a silent server fail my container", which has two
	// answers. buildParams6 is where the two meet, through
	// strictAutoFallback, so no call site can set a delay and switch it
	// off in the same value.
	StrictAuto6 bool

	// MainPrefix6 is the network's `ipv6_main_prefix`: which of the
	// addresses a forming mode ends up with is the one Docker is told
	// about and `docker inspect` shows. The zero value means "the first
	// prefix the router advertised", which is what a network that never
	// set the option gets.
	//
	// IT IS READ AT THE LEASE SEAM AND NOWHERE ELSE. infoFromLease is
	// the one point every lease crosses into the plugin, and the
	// selection has to be the same one on both sides of the endpoint's
	// life: CreateEndpoint answers Docker with an address, and the
	// persistent client re-applies the same lease minutes later. Two
	// derivations of "which address is the main one" would disagree the
	// first time a router reordered its prefixes, and Docker's view and
	// the link's would then name different addresses with nothing
	// failing.
	//
	// It is refused at CreateNetwork on a mode that does not form
	// addresses: a DHCPv6 lease holds the address the server granted,
	// and a prefix filter over one address can only ever do nothing.
	MainPrefix6 netip.Prefix

	// OnV6PrefixesIgnored is called with the GAIN in the library's
	// count of advertised Prefix Information options this client formed
	// no address from, for any of RFC 4862 section 5.5.3's rules and
	// including the library's own cap of proto.MaxSLAACAddresses.
	//
	// IT IS SET ONLY IN A MODE THAT FORMS ADDRESSES, and that is not
	// tidiness: on an `ipv6_mode=dhcp` network the library counts every
	// autonomous prefix it sees under SLAACIgnoreModeDHCP -- correctly,
	// since the address comes from the server -- and a router
	// readvertises every few seconds (RFC 4861 section 6.2.1). A
	// counter wired up there would climb forever on every healthy
	// dual-stack network and mean nothing. In `slaac` and `auto` the
	// same number answers a question an operator has: a prefix was
	// advertised and this endpoint has no address from it.
	OnV6PrefixesIgnored func(uint64)

	// OnV6Fallback is called with the GAIN in the library's count of
	// Mode6Auto fallbacks -- an `auto` endpoint that gave up on a
	// silent DHCPv6 server and formed an address from an advertised
	// prefix instead.
	//
	// A GAIN and not a total, for the reason OnACDStats gives. It
	// counts EFFECT and not intent, because the library's counter does:
	// lease.Stats.SLAACFallbacks "counts the fallback that FORMED
	// something: a deadline that passed with no usable prefix ends the
	// acquisition and leaves this where it was".
	//
	// nil is the unit-test and probe shape.
	OnV6Fallback func(uint64)

	// PreferredV6, when non-empty, is the address this endpoint would
	// like: RFC 9915 section 21.6's IA Address option inside the
	// Solicit's IA_NA. A preference and not a claim -- section 18.3.2
	// leaves the server free to assign something else -- so it is the
	// v6 twin of RequestedIP and is used for the same two things, an
	// operator's `--ip6` and a tombstone's address (#213).
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

	// ConflictMode is RFC 5227 conflict detection for this endpoint
	// (D23), from the network's `conflict_check` option. The zero value
	// is proto.ConflictWait, which is both the library's default and
	// the option's.
	//
	// It is the PARSED mode and not the operator's string, so a value
	// that never passed ParseConflictCheck cannot reach the wire: the
	// refusal happens once, at CreateNetwork, and everything after it
	// deals in a type with three inhabitants.
	ConflictMode proto.ConflictMode

	// OnConflict is called once per address conflict this endpoint's
	// client detects, from the manager's own goroutine, with what the
	// event says about it.
	//
	// It exists because the two managers report a conflict on two
	// different paths and the count must not be derived twice. The
	// CreateEndpoint one-shot has no outward event stream at all --
	// GetIP returns a lease or an error -- and the Join manager's
	// stream deliberately drops the conflict (see translateOne), so a
	// counter fed from the plugin's event arm would count half the
	// conflicts and a counter fed from both would count some twice.
	// This is the one route, and both managers take it.
	//
	// nil is the unit-test and probe shape.
	OnConflict func(Conflict)

	// OnACDStats is called with the DELTA in the library's RFC 5227
	// counters since the previous call, so the plugin can hold them
	// process-wide.
	//
	// A DELTA and not a snapshot: the plugin's counters are monotonic
	// across every manager that ever ran, and a manager that exits
	// takes its snapshot with it. Summing live managers instead would
	// make every counter fall when a container stops, which is the one
	// thing a counter may not do.
	//
	// nil is the unit-test and probe shape.
	OnACDStats func(ACDStats)

	// OnRenewalStats is called with the GAIN in this manager's count of
	// renewal requests that went unanswered, from the manager's own
	// goroutine.
	//
	// A GAIN and not a total, for the reason OnACDStats gives: the
	// plugin's counters are monotonic across every manager that ever
	// ran, and a manager that exits takes its snapshot with it.
	//
	// IT IS FED FROM A TIMER AS WELL AS FROM THE EVENTS, and that is
	// the whole of #940. A renewal request is sent from the library's
	// retransmission timer and produces no lease event, so a counter
	// folded on events alone reads zero for the entire outage and
	// first moves when the lease expires -- MEASURED on a production
	// host over a 24 hour lease, four renewal requests across 7h52m
	// with dhcp_timeouts at 0 and nothing in the log. See translate.
	//
	// WIRED ON THE PERSISTENT CLIENT ONLY. GetIP's one-shot acquires
	// and returns; it holds no lease to renew, so a fold there could
	// only add a second writer to a counter about renewals for a path
	// that has none.
	//
	// nil is the unit-test and probe shape.
	OnRenewalStats func(RenewalStats)

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
	//
	// A v6 manager sets it before its first event and never writes a
	// snapshot at all: lease.RecordEvent carries a *proto.Params and
	// has no Params6 slot, so the only thing a v6 manager could attach
	// is a ZERO v4 parameter set -- a record saying this endpoint sent
	// a DHCPDISCOVER with no client id, which is worse than a record
	// that says nothing. The consequence is stated rather than worked
	// around: a v6 record is not replayable through proto.Replay, and
	// the gap is the library's to close.
	paramsWritten bool
	params        proto.Params
	params6       proto.Params6

	// resumedConfigTaken is set once carryResumedConfig6 has had its one
	// chance to fill a resumed v6 lease's RFC 3646 lists. See that
	// method: the memory is worth at most one event and must never
	// outlive the first thing the server says.
	resumedConfigTaken bool

	// acdSeen is the last ACD counter snapshot handed to OnACDStats,
	// which is what makes that callback a delta rather than a total.
	acdSeen ACDStats

	// fallbacksSeen is the same thing for OnV6Fallback, and
	// prefixesIgnoredSeen for OnV6PrefixesIgnored.
	fallbacksSeen       uint64
	prefixesIgnoredSeen uint64
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

// conflict reports one address conflict to the caller, if this event is
// one.
//
// ONE PREDICATE, ONE CALL SITE PER MANAGER. RFC 5227 conflicts leave
// this library as exactly two events -- Failed{ReasonConflict} when
// nothing was held yet (the probe window) and Lost{ReasonConflict} when
// the address was already in use (section 2.4) -- and the library
// guarantees they are exclusive per conflict, so one bump each is one
// bump per conflict. That guarantee is asserted from this side rather
// than assumed: TestConflict_TheLibraryEmitsExactlyOneEventPerConflict
// drives proto.Machine through both cases.
func (o *DHCPClientOptions) conflict(ev lease.Event) bool {
	if ev.Reason != proto.ReasonConflict {
		return false
	}
	if ev.Kind != lease.Failed && ev.Kind != lease.Lost {
		return false
	}
	if o.OnConflict != nil {
		o.OnConflict(Conflict{Held: ev.Kind == lease.Lost, Addr: bareAddr(ev.Lease), Note: ev.Note})
	}
	return true
}

// Conflict is one address conflict, as much of it as leaves the
// library.
type Conflict struct {
	// Held says the address was already in use by this endpoint when
	// the conflict was found -- RFC 5227 section 2.4's ongoing check --
	// so the container is about to CHANGE address. False is section
	// 2.1's probe window: nothing was configured, and the container
	// simply gets a different address than it would have.
	//
	// It is the operationally important half of the distinction and it
	// is why the two library events are not folded into one bool here.
	Held bool

	// Addr is the address found in use, and it is EMPTY when Held is
	// false. That is a property of the library rather than an
	// omission: Failed carries no lease, because in the probe window
	// no lease was ever held. The address is in the DHCP server's log
	// as the DHCPDECLINE's, which is the outside evidence anyway.
	Addr string

	// Note is the library's own human-readable line for the event.
	Note string
}

// bareAddr renders a lease's address without its prefix length, or ""
// for a lease that has none.
func bareAddr(l lease.Lease) string {
	if !l.Addr.IsValid() {
		return ""
	}
	return l.Addr.Addr().String()
}

// acdReport hands the caller everything the library's RFC 5227 counters
// have gained since the last call.
//
// Called on every event and once more when the manager ends, and
// nowhere else. The probes are sent from a TIMER, so a probe run that
// finishes with no further lease event to ride on stays unreported
// until the next event on that endpoint, however long that is. The lag
// is not one probe interval: MEASURED on the production host
// 2026-09-10, a ConflictAsync endpoint whose probes went out within 9 s
// of the bind had them folded 19h52m later, at the first renewal. The
// call after the drain is what makes the total exact for a manager that
// has finished.
//
// An operator decision does turn on this. acd_probes_sent is what
// pkg/plugin/endpoints.go tells the operator to read before believing
// address_conflicts is zero, and a persistent client's own probe run is
// missing from that reading until its next lease event.
func (o *DHCPClientOptions) acdReport(s lease.Stats) {
	if o.OnACDStats == nil {
		return
	}
	cur := acdStats(s)
	delta := cur.Sub(o.acdSeen)
	o.acdSeen = cur
	if delta.IsZero() {
		return
	}
	o.OnACDStats(delta)
}

// v6ModeReport hands the caller the Mode6Auto fallbacks the library has
// counted since the last call.
//
// Called beside acdReport wherever a DHCPv6 client's statistics are
// read: the persistent client's fold and its final defer, and the v6
// one-shot acquisition. It is NOT beside acdReport's other two sites,
// which are the DHCPv4 acquisition loop, where there is no Mode6 to
// fall back in; a count of "the same places" would be wrong, and the
// number is not the point.
//
// It is read on a timer's schedule and not only on a lease event, for
// the reason acdReport is: the fallback is armed on a TIMER inside the
// machine, so the Step that fires it need not be one that produces a
// lease event this chassis would otherwise look at.
func (o *DHCPClientOptions) v6ModeReport(s lease.Stats) {
	if o.OnV6Fallback == nil {
		return
	}
	// A GUARD IN ONE DIRECTION ONLY, and the other direction is named:
	// this subtracts a remembered total from a later one, so it can
	// only under-report if the library's counter ever went DOWN. It is
	// monotonic per manager (lease.Stats is a running total), and a new
	// manager starts a new DHCPClientOptions, so there is no path on
	// which the remembered value belongs to a different counter.
	if s.SLAACFallbacks <= o.fallbacksSeen {
		return
	}
	delta := s.SLAACFallbacks - o.fallbacksSeen
	o.fallbacksSeen = s.SLAACFallbacks
	o.OnV6Fallback(delta)
}

// v6PrefixReport hands the caller the advertised prefixes the library
// formed no address from since the last call.
//
// SAME SHAPE AND SAME GUARD AS v6ModeReport, and separate from it
// because the two callbacks are set in different modes: the fallback
// exists only in `auto` and this exists in `auto` and `slaac` alike. A
// single callback carrying both numbers would have to be set in every
// forming mode and then report a fallback count that cannot move in one
// of them.
func (o *DHCPClientOptions) v6PrefixReport(s lease.Stats) {
	if o.OnV6PrefixesIgnored == nil {
		return
	}
	if s.SLAACPrefixesIgnored <= o.prefixesIgnoredSeen {
		return
	}
	delta := s.SLAACPrefixesIgnored - o.prefixesIgnoredSeen
	o.prefixesIgnoredSeen = s.SLAACPrefixesIgnored
	o.OnV6PrefixesIgnored(delta)
}

// RAObservation is what this segment's router advertisements said, as
// much of RFC 4861 section 4.2 as a caller with no address needs.
//
// IT IS A DIAGNOSTIC AND NEVER AN INSTRUCTION (D30 Q2). The library
// sends the solicitations and reads the advertisements; nothing here
// decides anything about the exchange from it. What it decides is what
// to TELL THE OPERATOR when an acquisition produced no address, which
// is a question the timeout alone cannot answer -- see
// pkg/plugin/v6_absence.go, the whole of #868.
//
// The zero value means no advertisement was seen, which is the honest
// answer both for a segment with no router and for a v4 endpoint that
// never looked.
type RAObservation struct {
	// Seen is RFC 4861 section 4.2: at least one advertisement arrived
	// on this link.
	Seen bool
	// Managed is the M bit — "addresses are available via DHCPv6".
	Managed bool
	// Other is the O bit — "other configuration information is
	// available via DHCPv6", which is the stateless segment (RFC 9915
	// section 18.2.6) and the reason an endpoint with no address can
	// still have a resolver.
	Other bool
}

// Merge folds another attempt's observation into this one.
//
// OR and not "last wins": the server-policy ladder makes several
// attempts on one link, and an advertisement seen on the first is still
// evidence about the segment when the fourth times out. A flag that
// went back to false because a later attempt was short would report a
// routerless segment for a link that answered.
func (o RAObservation) Merge(other RAObservation) RAObservation {
	return RAObservation{
		Seen:    o.Seen || other.Seen,
		Managed: o.Managed || other.Managed,
		Other:   o.Other || other.Other,
	}
}

// raObservation is the library's router observation in the chassis's
// spelling.
//
// A conversion and not a type alias, because pkg/plugin must not learn
// a library type: the seam's rule is that the chassis is the only
// package that names one (M6b, D22/D23).
func raObservation(r proto.RouterObservation) RAObservation {
	return RAObservation{Seen: r.Seen, Managed: r.Managed, Other: r.Other}
}

// acquireOutcome is what one lease.Event means to a one-shot
// acquisition: whether the acquisition ENDS here, with what address,
// and what to tell the caller if the deadline ends it instead.
type acquireOutcome struct {
	Info Info
	Done bool
	Err  error
}

// acquireStep decides whether a one-shot acquisition ends on ev.
//
// IT IS A FUNCTION AND NOT THREE LINES INSIDE GetIP's SELECT because
// the rule it carries is the one this milestone turns on and the loop
// around it cannot be driven without a raw socket and a netns: an
// acquisition returns on lease.Acquired and on NOTHING else, in every
// proto.ConflictMode.
//
// A conflict found in RFC 5227 section 2.1's probe window arrives as
// Failed{ReasonConflict}. RFC 2131 section 3.1(5) obliges the
// DHCPDECLINE, and the library sends it, waits section 3.1(5)'s "a
// minimum of ten seconds" and starts again from INIT on its own. The
// only thing returning here would achieve is to fail `docker run` for
// a container the library was about to give a perfectly good second
// address to. GetIP's other select arm -- the deadline -- is what ends
// a hopeless attempt, exactly as it does for a silent server.
//
// Err without Done is deliberate and is the whole shape: it names the
// last real cause so the error the caller finally sees is
// "address conflict" or "acquisition failed: <reason>" rather than
// "context deadline exceeded" alone.
func acquireStep(ev lease.Event, conflicted bool, now time.Time) acquireOutcome {
	switch ev.Kind {
	case lease.Acquired:
		// No main prefix: this is the v4 acquisition, and a v4 lease
		// carries no Addrs list to choose from (lease.Lease.Addrs is
		// "empty for v4").
		info, _ := infoFromLease(ev.Lease, ev.Router, now, netip.Prefix{})
		return acquireOutcome{Info: info, Done: true}
	case lease.Failed:
		if conflicted {
			return acquireOutcome{Err: fmt.Errorf("%w: %v", ErrAddressConflict, ev.Lease.Addr)}
		}
		return acquireOutcome{Err: fmt.Errorf("dhcp: acquisition failed: %v", ev.Reason)}
	}
	return acquireOutcome{}
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
		return getIP6(ctx, iface, opts)
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
			opts.acdReport(client.Stats())
			// A CONFLICT IS NOT THE END OF THIS ACQUISITION, in any
			// mode. RFC 2131 section 3.1(5) obliges the DHCPDECLINE
			// and the library sends it, waits section 3.1(5)'s "a
			// minimum of ten seconds" and starts again from INIT on
			// its own; a chassis that returned here would fail
			// `docker run` for a container the library was about to
			// give a perfectly good second address to. The deadline
			// is what ends the attempt, exactly as it does for a
			// silent server.
			//
			// In proto.ConflictWait that arrives as
			// Failed{ReasonConflict}, because nothing was held yet.
			// In proto.ConflictAsync the address was already handed
			// out, so it arrives as Lost{ReasonConflict} and the
			// caller has by then returned -- this arm is the
			// one-shot's window only.
			out := acquireStep(ev, opts.conflict(ev), time.Now())
			if out.Err != nil {
				lastE = out.Err
			}
			if out.Done {
				info = out.Info
				got = true
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
		opts.conflict(ev)
	}
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		log.WithError(err).WithField("iface", iface).Debug("Acquisition manager returned an error")
	}
	final := client.Stats()
	opts.acdReport(final)
	opts.count(manager, final)

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
	iface   string
	opts    DHCPClientOptions
	params  proto.Params
	params6 proto.Params6

	// client and client6 are the two library clients, and EXACTLY ONE
	// IS EVER NON-NIL: the family is fixed at construction and this
	// type never changes it. Two typed fields and not one interface
	// because the family-specific readers differ -- ACDPhase is RFC
	// 5227 and v4-only, DADPhase and Router are RFC 4862/4861 and
	// v6-only -- and an interface wide enough for both would have to
	// carry four methods that half its implementations answer with a
	// zero value.
	client  *dhcpruntime.Client
	client6 *dhcpruntime.Client6
	cancel  context.CancelFunc
	done    chan error
	events  chan Event
	manager string

	// runner is the family-independent half of whichever of the two
	// clients above was built, taken once in Start.
	//
	// Stats() reads it rather than switching on the family again,
	// because the counters are the one thing both families answer
	// identically and a second switch is a second place for the
	// families to drift apart. It is also what lets the renewal fold
	// be driven with no socket: a test can supply a libClient whose
	// Stats() it controls, which is the only way to place a
	// retransmission on this side of the seam without a wire.
	runner libClient

	// renewals turns the library's two renewal counters into the
	// unanswered-request count. Touched from the translate goroutine
	// and from nowhere else.
	renewals renewalWatch

	// pollEvery is how often translate folds the counters with no event
	// to ride on. Zero means renewalPollInterval, which is what
	// production runs; a test sets it so a retransmission can be
	// observed without waiting out RFC 2131's floor.
	pollEvery time.Duration

	// src is the library's event stream, taken once in Start. translate
	// ranges over THIS rather than over c.client.Events() so that the
	// goroutine can be driven without a socket: the wedge this field
	// exists for (X-34) is a property of the goroutine and not of
	// translateOne, and a test that cannot start the goroutine cannot
	// see it. c.client stays nil on that path, which Stats() already
	// tolerates.
	src <-chan lease.Event

	// dropped counts emits this client could not hand to the plugin
	// because nothing was reading. See translate.
	dropped atomic.Uint64

	// view is what the advertisement watch reads, defaulting to
	// c.Lease. It is a seam for the reason src is one: the watch's
	// whole job is to notice a change that arrives with NO library
	// event behind it, and a test that had to produce one on a wire
	// could not drive it at all.
	view func() (lease.Lease, bool)

	// routerView is what the advertisement watch reads about the
	// ROUTERS, defaulting to the library client's own observation. A
	// seam for the same reason view is one.
	routerView func() proto.RouterObservation

	// advert is the advertised configuration this client last reported,
	// and advertKnown says whether it has reported any. Touched from
	// the translate goroutine and from nowhere else.
	advert      Info
	advertKnown bool
}

// eventBuffer is the depth of the channel translate emits on.
//
// DERIVED from the depth the chassis already asked the library for:
// newLibClient sets EventBuffer to the same 16 below, so a burst the
// library was willing to hold is a burst this side can hold too, and a
// smaller number here would start dropping while the library was still
// buffering. (The library's own fallback when nothing is configured is
// 8 — lease/manager.go — so the 16 is this package's choice on both
// sides of the seam, not an inherited default.) The base used 16 here
// for the same reason.
const eventBuffer = 16

// newEventChan builds the channel translate emits on.
//
// A function and not an inline make, because the test that drives
// translate has to obtain its channel from the SAME expression
// production does. MEASURED: while the harness built its own
// `make(chan Event, eventBuffer)`, a mutant that returned Start's
// channel to unbuffered SURVIVED all three tests — they were holding a
// depth they had chosen themselves.
func newEventChan() chan Event { return make(chan Event, eventBuffer) }

// DroppedEvents is how many translated events were discarded because
// the plugin side had stopped reading.
//
// Exported so the drop can be ASSERTED rather than inferred from a log
// line. A silent drop and a wedge look identical from outside the
// package — both produce no event — and the whole of X-34 is that the
// difference matters.
func (c *DHCPClient) DroppedEvents() uint64 { return c.dropped.Load() }

// NewDHCPClient prepares a persistent client. Nothing is opened until
// Start: the socket must be created inside the sandbox namespace, and
// that is a property of the thread Start runs on.
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	if err := checkRouterAdvertGuardShape(opts, false); err != nil {
		return nil, err
	}
	if opts.V6 {
		params6, err := buildParams6(opts, false)
		if err != nil {
			return nil, err
		}
		copied := *opts
		// NO Params SNAPSHOT RIDES A v6 EVENT. See
		// DHCPClientOptions.paramsWritten: the record carries a
		// *proto.Params and there is no Params6 slot, so the only
		// thing available to attach is a zero v4 parameter set.
		copied.params6, copied.paramsWritten = params6, true
		return &DHCPClient{iface: iface, opts: copied, params6: params6}, nil
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
	// ONE VARIABLE OF AN INTERFACE TYPE, ASSIGNED IN THE FAMILY SWITCH
	// AND READ EVERYWHERE BELOW. The alternative -- duplicating the
	// goroutine, the channels and the cancel per family -- is where a
	// v6 client that is started but never cancelled comes from, and
	// the defeat list's "two clients, one cancel" row is exactly that
	// shape.
	var runner libClient
	if c.opts.V6 {
		client6, err := newLibClient6(c.iface, c.params6, &c.opts)
		if err != nil {
			return nil, err
		}
		c.client6, runner = client6, client6
	} else {
		client, err := newLibClient(c.iface, c.params, &c.opts)
		if err != nil {
			return nil, err
		}
		c.client, runner = client, client
	}
	if c.opts.Records != nil {
		c.manager = c.opts.Records.NewManagerID()
	}

	c.runner = runner

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan error, 1)
	c.events = newEventChan()
	c.src = runner.Events()

	go func() { c.done <- runner.Run(ctx) }()
	go c.translate()

	return c.events, nil
}

// libClient is the half of the library's client surface that is the
// same in both families: run it, read its events, read what it holds,
// read its counters.
//
// It is declared HERE and not in the library because it is the
// chassis's demand, not the library's offer: *dhcpruntime.Client and
// *dhcpruntime.Client6 satisfy it without either of them naming it.
type libClient interface {
	Run(ctx context.Context) error
	Events() <-chan lease.Event
	Lease() (lease.Lease, bool)
	Stats() lease.Stats
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
	defer func() {
		final := c.Stats()
		c.opts.acdReport(final)
		c.opts.v6ModeReport(final)
		c.opts.v6PrefixReport(final)
		c.renewals.report(final, c.opts.OnRenewalStats)
		c.opts.count(c.manager, final)
	}()

	// THE FOLD RUNS ON A TIMER AND NOT ONLY ON AN EVENT (#940).
	//
	// A renewal request leaves the host from the library's
	// retransmission timer, and a renewal that is not answered produces
	// no lease event at all: the state machine stays in RENEWING and
	// asks again. Every counter this loop folds on the event arm is
	// therefore frozen for the whole of an outage, and the first thing
	// that moves is dhcp_timeouts, at the end of the lease -- 24 hours
	// after the server went quiet, on a 24 hour lease. acdReport
	// carries the same defect in the other direction and says so: a
	// probe run with no later event stayed unreported for 19h52m,
	// MEASURED on a production host.
	//
	// So the tick is not a convenience. It is the only thing that makes
	// "the server stopped answering" observable while the client is
	// still holding a perfectly good address.
	poll := time.NewTicker(c.renewalPoll())
	defer poll.Stop()

	// The advertisement watch runs on the v6 path only: RFC 4861
	// advertisements are the only source of the five fields it follows,
	// and a v4 client's merged lease cannot change without a DHCPACK,
	// which arrives here as an event. Armed for both families would be
	// a ticker that can never fire on one of them.
	raWatch := newStoppedTicker()
	if c.opts.V6 {
		raWatch = time.NewTicker(raWatchInterval)
	}
	defer raWatch.Stop()

	renewedAt := time.Time{}
	for {
		var ev lease.Event
		select {
		case <-poll.C:
			c.renewals.report(c.Stats(), c.opts.OnRenewalStats)
			continue
		case <-raWatch.C:
			if out, ok := c.takeAdvertChange(time.Now()); ok {
				c.deliver(out)
			}
			continue
		case e, ok := <-c.src:
			if !ok {
				return
			}
			ev = e
		}
		now := time.Now()
		// BEFORE the record is written, so a second restart still finds
		// the resolver in it; see carryResumedConfig6.
		c.opts.carryResumedConfig6(&ev)
		// Written before it is translated. The record is the thing a
		// restart reads, and translateOne drops two kinds on the floor
		// deliberately — the coalesced Changed and the stop — neither
		// of which the record may lose.
		c.opts.record(ev)
		// ONE SNAPSHOT FOR BOTH, and the renewal watch is told the
		// cycle ended after it has folded that snapshot. A lease event
		// is the chassis's evidence that the renewal request in flight
		// is no longer waiting for an answer -- including the endings
		// that never bump RenewalsCompleted, which renewalWatch's
		// comment names. Folding after the reset would forget what the
		// cycle proved; resetting from a second, later reading would
		// forget a request that left the host in between.
		stats := c.Stats()
		c.opts.acdReport(stats)
		c.opts.v6ModeReport(stats)
		c.opts.v6PrefixReport(stats)
		c.renewals.report(stats, c.opts.OnRenewalStats)
		c.renewals.cycleEnded(stats)
		c.opts.conflict(ev)

		out, emit, at := translateOne(ev, now, renewedAt, c.opts.MainPrefix6)
		renewedAt = at
		if !emit {
			continue
		}

		// The baseline the advertisement watch compares against is
		// taken HERE, from the two kinds whose handling applies the
		// advertised configuration to the container. Taking it on
		// every event would let a nak or a timeout, which applies
		// nothing, mark a change as delivered.
		if out.Type == "bound" || out.Type == "renew" {
			c.baselineAdvert(now)
		}
		c.deliver(out)
	}
}

// deliver hands one translated event to the plugin, or drops it.
//
// THE SEND MUST NOT BLOCK, AND THE LOOP MUST NOT STOP (X-34).
//
// The only reader is the per-family goroutine in
// pkg/plugin/dhcp_manager.go, and its other arm returns on stopChan and
// never reads this channel again. A bare send here parks this goroutine
// forever on the first event that arrives in that window -- a Leave
// while a renewal is in flight, a plugin Close over every live endpoint,
// or the legacy dual-stack path where the v6 client refuses and closes
// stopChan under a live v4 client.
//
// WHICH LOSS THIS CHOOSES, AND WHY. A wedge loses far more than the
// event that caused it: the range never advances, so every LATER event
// is lost from the durable record too; deferred close(c.events) never
// runs, so the reader's own "stream closed" arm never fires; deferred
// count() never runs, and it is the only writer of this manager's wire
// counters (P-7's per-endpoint half), so a TICKED parity row silently
// produces nothing for the endpoint; and the goroutine and its client
// leak for the life of the daemon. A drop loses exactly one plugin-side
// event -- one ledger row and its counter bumps -- and nothing else:
// c.opts.record(ev) has ALREADY written the library's event to the
// durable record, unconditionally, before the translation, so the
// record's tail is complete either way. The drop is strictly the
// smaller loss, and it is the loss the base chose too.
//
// WHAT THIS REPLACES. Base pkg/dhcp/client.go:819 made the channel
// `make(chan Event, 16)` and :839-840 sent through a select/default
// commented "A full channel drops events rather than blocking the DHCP
// exchange." The swap deleted both halves and named no replacement.
// This is that guard, restored, plus the half it never had: the base
// dropped SILENTLY, so a drop and a wedge were indistinguishable from
// outside. Every drop is counted on DroppedEvents() and logged at Warn.
func (c *DHCPClient) deliver(out Event) {
	select {
	case c.events <- out:
	default:
		c.dropped.Add(1)
		log.
			WithField("record", c.opts.RecordID).
			WithField("event", out.Type).
			WithField("dropped_total", c.dropped.Load()).
			Warn("The plugin stopped reading this endpoint's DHCP events; the event was " +
				"dropped. The durable record still has it; the ledger row and counters for " +
				"it are lost.")
	}
}

// newStoppedTicker is a ticker that never fires, for the family that
// has no advertisement to watch. A nil *time.Ticker cannot be used: the
// select reads its C, and Stop would dereference nil.
func newStoppedTicker() *time.Ticker {
	t := time.NewTicker(time.Hour)
	t.Stop()
	return t
}

// leaseView is what the advertisement watch reads.
func (c *DHCPClient) leaseView() (lease.Lease, bool) {
	if c.view != nil {
		return c.view()
	}
	return c.Lease()
}

// advertRouterView is the router observation the advertisement watch
// reads, carrying ONLY what is safe to follow live.
//
// THE MTU IS THE ONE THING THE LEASE CANNOT CARRY. DHCPv6 has no MTU
// option at all -- option 26 is DHCPv4's (RFC 2132 section 5.1) -- so
// the advertised link MTU of RFC 4861 section 4.6.4 reaches the plugin
// through the router observation or not at all, and lease.Lease.MTU is
// zero on every DHCPv6 lease ever issued.
//
// The prefixes are left out, which is what keeps the on-link rule at
// Join: see takeAdvertChange for why following them live would take a
// route away from a container because one advertisement happened to be
// shorter. THE BOUND THAT BUYS: an advertisement whose ONLY change is
// its set of on-link prefixes produces no event at all, so
// Info.OnLinkPrefixes is a Join-time answer with no live update, on an
// endpoint that has an address as much as on one that does not. A
// segment that starts or stops advertising a prefix as on-link reaches
// a running container's routing table when the container is recreated
// and not before.
//
// The MTU has no such problem. A router that stops advertising an MTU
// is saying nothing about the MTU, zero is how that is spelled, and a
// zero is the withdrawal propagateMTU acts on.
func (c *DHCPClient) advertRouterView() proto.RouterObservation {
	var r proto.RouterObservation
	if c.routerView != nil {
		r = c.routerView()
	} else if c.client6 != nil {
		r = c.client6.Router()
	}
	return proto.RouterObservation{Seen: r.Seen, MTU: r.MTU}
}

// baselineAdvert records what the routers are advertising WITHOUT
// reporting it, for the caller that has just applied it by another
// route.
func (c *DHCPClient) baselineAdvert(now time.Time) {
	c.takeAdvertChange(now)
}

// takeAdvertChange reports what the routers on this link advertise when
// it differs from the last view this client reported, and nothing when
// it does not.
//
// IT REPORTS A CHANGE AND NEVER A FIRST SIGHT. The first reading is the
// baseline: on the path that matters the lease has just been applied
// through bound, so reporting it again would re-apply a configuration
// the container already has and write a second ledger row for one
// event. A client that somehow reaches its first reading here instead
// is followed from that reading on, which is the same rule seen from
// the other end.
//
// THE VIEW CARRIES NO ON-LINK DETERMINATION, deliberately: the
// RouterObservation passed in carries the advertised MTU and nothing
// else (see advertRouterView), so Info.OnLinkPrefixes is empty on every
// event this produces. On-link determination is
// applied once, at Join, out of the advertisement the acquisition saw;
// the library reports the prefixes of the most recent frame rather than
// a union, so following it live would take a route away from a
// container because one advertisement happened to be shorter.
func (c *DHCPClient) takeAdvertChange(now time.Time) (Event, bool) {
	l, ok := c.leaseView()
	if !ok {
		return Event{}, false
	}
	// The network's main prefix, the same one the bound and renew
	// events are rendered with: this Info is compared against the last
	// one this client reported, and rendering the two through different
	// choices of reported address would make a change out of the
	// choice.
	info, dropped := infoFromLease(l, c.advertRouterView(), now, c.opts.MainPrefix6)
	first := !c.advertKnown
	same := c.advertKnown && !advertisedDiffers(c.advert, info)
	c.advert, c.advertKnown = info, true
	if first || same {
		return Event{}, false
	}
	return Event{
		Type:                "routeradvert",
		Data:                info,
		UnsafeValuesDropped: dropped,
		RouterFlags:         raFlags(c.RA()),
	}, true
}

// advertisedDiffers compares the five fields a Router Advertisement can
// change and NOTHING ELSE.
//
// The address and its lifetimes are deliberately outside it: they move
// on every renewal, and a watch that read them would report a change
// the renewal had already applied, once per lease, forever.
func advertisedDiffers(a, b Info) bool {
	return a.Gateway != b.Gateway ||
		a.MTU != b.MTU ||
		!sameStrings(a.DNSServers, b.DNSServers) ||
		!sameStrings(a.SearchList, b.SearchList) ||
		!sameRoutes(a.Routes, b.Routes)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameRoutes(a, b []Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// translateOne is the whole of the event translation, split out from
// the loop above so the two rules that are easiest to break by accident
// can be driven directly.
//
// It returns the event to emit, whether to emit at all, and the updated
// "last renewal" mark. NOTHING here reads a socket or a clock: `now` is
// supplied, which is what lets a test place a Changed inside and
// outside the coalesce window without sleeping.
func translateOne(ev lease.Event, now, renewedAt time.Time, main netip.Prefix) (Event, bool, time.Time) {
	info, dropped := infoFromLease(ev.Lease, ev.Router, now, main)

	var out Event
	switch ev.Kind {
	case lease.Configured:
		// RFC 9915 section 18.2.6's answer: configuration and NO
		// address. Its own event kind in the library (D30 Q7) and its
		// own type here, because the plugin's handling of it is not a
		// bind with fields missing -- it writes the resolver, counts
		// dhcpv6_config_only, and deliberately does NOT clear the
		// outage deadline, since an Information-reply proves the
		// server is reachable and not that a lease exists.
		cfg, cfgDropped := infoFromConfig(ev.Config)
		dropped = cfgDropped
		out = Event{Type: "config", Data: cfg}
	case lease.Acquired:
		out = Event{Type: "bound", Data: info}
	case lease.Renewed:
		renewedAt = now
		out = Event{Type: "renew", Data: info}
	case lease.Changed:
		if now.Sub(renewedAt) < coalesceWindow {
			// The Renewed for this same ACK has just been delivered;
			// the plugin re-applies a changed address on "renew"
			// already, so there is nothing left to say.
			return Event{}, false, renewedAt
		}
		out = Event{Type: "renew", Data: info}
	case lease.Lost:
		// THE ONE RULE THAT LOOKS LIKE A MISSING CASE. A Lost carrying
		// ReasonStopped is this process cancelling its own manager --
		// every CreateEndpoint one-shot ends with one, and so does
		// every clean Leave. Emitting it would make a successful
		// container start report a lease loss.
		if ev.Reason == proto.ReasonStopped {
			return Event{}, false, renewedAt
		}
		// A CONFLICT IS NOT A LEASE FAILURE AND MUST NOT BE ONE.
		// "leasefail" is what feeds dhcp_timeouts through
		// countOutageTick, and dhcp_timeouts means the DHCP server
		// went quiet -- which is exactly what has NOT happened here:
		// the server answered, the address it named is occupied, and
		// the library is already declining it and asking for another.
		// Counting it as an outage would make a squatted pool
		// indistinguishable from a dead server in the one counter an
		// operator alerts on.
		//
		// Nothing else is lost by dropping it. The conflict is counted
		// through DHCPClientOptions.OnConflict, which the one-shot
		// takes too; the event is already on the durable record,
		// unconditionally, before this function is called; and the
		// address change the library then wins arrives as the ordinary
		// Acquired -> "bound" that reconfigures the container. The
		// existing Lost -> re-acquire path is the whole handling.
		if ev.Reason == proto.ReasonConflict {
			return Event{}, false, renewedAt
		}
		// A FORMED ADDRESS GOING AWAY IS NOT A DHCP OUTAGE. "leasefail"
		// is what feeds dhcp_timeouts through countOutageTick, and that
		// counter means one thing: the DHCP server stopped serving this
		// client. A SLAAC lease was granted by nobody -- RFC 4862
		// section 5.5.3 forms it from an advertised prefix -- so its
		// end is a router that stopped advertising or a valid lifetime
		// that ran out, and an operator alerting on a dead DHCP server
		// would be paged for a working segment being renumbered. It is
		// its own type so the plugin can take the addresses off the
		// link and say which ones, which "leasefail" carries no data
		// for.
		if ev.Lease.SLAAC {
			out = Event{Type: "slaac_lost", Data: info}
			break
		}
		if ev.Reason == proto.ReasonNak {
			out = Event{Type: "nak"}
		} else {
			out = Event{Type: "leasefail"}
		}
	case lease.Failed:
		if ev.Reason == proto.ReasonConflict {
			return Event{}, false, renewedAt
		}
		if ev.Reason == proto.ReasonNak {
			out = Event{Type: "nak"}
		} else {
			out = Event{Type: "leasefail"}
		}
	default:
		return Event{}, false, renewedAt
	}
	out.UnsafeValuesDropped = dropped
	out.RouterFlags = routerFlags(ev.Router)
	return out, true, renewedAt
}

// routerFlags renders RFC 4861 section 4.2's two configuration bits as
// the letters an operator reads in a log line: "M", "O", "MO", or "" for
// an advertisement with neither.
//
// It is "" for a v4 event too, and the two are not distinguishable here
// on purpose: this string is for a human, and the machine-readable form
// is RAObservation, which has a Seen of its own.
func routerFlags(r proto.RouterObservation) string { return raFlags(raObservation(r)) }

// raFlags is routerFlags over the chassis's own spelling of the
// observation, which is what a caller holding an RAObservation has.
func raFlags(r RAObservation) string {
	if !r.Seen {
		return ""
	}
	out := ""
	if r.Managed {
		out += "M"
	}
	if r.Other {
		out += "O"
	}
	return out
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

// raWatchInterval is how often a v6 client re-reads what the routers on
// its link are advertising.
//
// IT EXISTS BECAUSE AN ADVERTISEMENT THAT CHANGES NOTHING ABOUT THE
// LEASE PRODUCES NO LEASE EVENT. MEASURED against dhcp-golib v1.0.0: a
// Changed is stamped from the bound state and from the SLAAC lifetime
// path, both of which compare the DHCPv6 binding; a router that
// withdraws its lifetime, changes its MTU, adds a Route Information
// Option or drops a resolver moves the library's router table and the
// merged lease it hands back, and emits nothing. Waiting for the next
// renewal to carry it would mean a container following its segment at
// the lease's pace -- hours -- which is the whole of what Q4 refused.
//
// DERIVED from the shortest gap the WIRE can produce, the same way
// renewalPollInterval is derived from RFC 2131's renewal floor. RFC
// 4861 section 10's router constants: "MIN_DELAY_BETWEEN_RAS 3
// seconds". A quarter of it places three reads in the shortest gap
// between two advertisements, so the view survives two missed ticks.
const minDelayBetweenRAs = 3 * time.Second

const raWatchInterval = minDelayBetweenRAs / 4

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
	switch {
	case c.client6 != nil:
		return c.client6.Lease()
	case c.client != nil:
		return c.client.Lease()
	}
	return lease.Lease{}, false
}

// ACDPhase is where RFC 5227 has got to for the address this client
// holds. proto.ACDIdle for a client that is not running, which is also
// the answer in conflict_check=off -- read it beside ConflictMode,
// never alone.
func (c *DHCPClient) ACDPhase() proto.ACDPhase {
	if c.client == nil {
		return proto.ACDIdle
	}
	return c.client.ACDPhase()
}

// ErrNoRunningClient is returned by SetHostname when there is no
// started client to hand the name to.
//
// It is its own error rather than a silent no-op because the whole
// point of the call is that a name arrives AFTER the client is
// running: a caller that reaches this has the order wrong, and the
// name would never be sent at all.
var ErrNoRunningClient = errors.New("dhcp: no running client to give a hostname to")

// SetHostname gives the running client the name to put in option 12 and
// makes it tell the server at once (#961).
//
// THE NAME IS SENT, NOT STORED: the library renews early to carry it
// (RFC 2131 section 4.4.5, "A client MAY choose to renew or extend its
// lease prior to T1"), so the server's table has it within one exchange
// instead of at T1. Repeating the same name sends nothing.
//
// A nil error means the name was validated and handed over, and NOT
// that the server answered; see lease.Manager.SetHostname. The failure
// modes belong to the caller: an unsendable name, a client already
// sending option 81 (RFC 4702 section 3.1 forbids option 12 beside it,
// and the library takes option 81 at construction), or a full request
// queue, which is the one a caller may retry.
//
// V6 IS REFUSED HERE RATHER THAN FORWARDED. This library sends no name
// option for DHCPv6 -- proto.Params6 has no hostname field -- so the
// call has nothing to do on that family, and the refusal is what keeps
// a dual-stack caller from reading a nil error as "the name went out
// on both".
func (c *DHCPClient) SetHostname(name string) error {
	if c.opts.V6 {
		return fmt.Errorf("dhcp: %w", lease.ErrHostnameV6)
	}
	if c.client == nil {
		return ErrNoRunningClient
	}
	return c.client.SetHostname(name)
}

// ConflictMode is the RFC 5227 mode this client was started in (D23).
// Read from the params the client was built with rather than from the
// network's stored options, so it is the mode in force and not the
// mode the options would resolve to now.
// A v6 client answers with the zero mode, which is proto.ConflictWait,
// and that is not a claim that it runs RFC 5227: it does not. RFC 9915
// section 18.2.10.1 obliges RFC 4862 duplicate address detection before
// the address is used, the library performs it, and DADPhase is where
// that is reported. Read this beside V6, never alone.
func (c *DHCPClient) ConflictMode() proto.ConflictMode { return c.params.Conflict }

// Stats is the manager's counters, which are the per-endpoint half of
// the health surface (P-7).
func (c *DHCPClient) Stats() lease.Stats {
	if c.runner == nil {
		return lease.Stats{}
	}
	return c.runner.Stats()
}

// DADPhase is where RFC 4862 section 5.4's check stood for the address
// this client holds, and it is proto.DADIdle for a v4 client: that
// family runs RFC 5227 instead and reports it on ACDPhase.
func (c *DHCPClient) DADPhase() proto.DADPhase {
	if c.client6 == nil {
		return proto.DADIdle
	}
	return c.client6.DADPhase()
}

// RA is the last router advertisement this client saw, and the zero
// value for a v4 client, which never looks.
func (c *DHCPClient) RA() RAObservation {
	if c.client6 == nil {
		return RAObservation{}
	}
	return raObservation(c.client6.Router())
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
		EventBuffer: eventBuffer,
	}

	if opts.NetNS == nil {
		return dhcpruntime.NewClient(cfg)
	}

	var (
		client *dhcpruntime.Client
		cerr   error
	)
	if err := inNetNS(*opts.NetNS,
		func() { client, cerr = dhcpruntime.NewClient(cfg) },
		func() {
			if client != nil {
				_ = client.Run(canceledContext())
			}
		},
	); err != nil {
		return nil, err
	}
	if cerr != nil {
		return nil, fmt.Errorf("dhcp: open a DHCP client on %v: %w", iface, cerr)
	}
	return client, nil
}

// inNetNS runs open with the calling thread inside ns, and returns it
// to the namespace it came from.
//
// EXTRACTED SO THE TWO FAMILIES CANNOT DRIFT. Both constructors need
// exactly this dance and the failure handling in it is the part that is
// easy to get subtly wrong; a second hand-written copy for v6 is the
// shape where one family unlocks a contaminated thread and the other
// does not.
//
// open returns nothing and abandon takes nothing: whatever was built
// lives in the caller's own variables, captured by the closures. That
// is what keeps this function free of a type parameter for a difference
// of one pointer type.
//
// abandon is called only when the thread could NOT be returned. What it
// is for: the client was constructed successfully and is about to be
// dropped on the floor, and dropping a library client without running
// it leaks its sockets. It runs while the thread is still in the
// container's namespace, which is the only namespace those sockets mean
// anything in.
func inNetNS(ns netns.NsHandle, open, abandon func()) error {
	runtime.LockOSThread()
	origin, err := netns.Get()
	if err != nil {
		// Nothing has been entered, so the thread is not contaminated
		// and must go back to the scheduler. The base left it locked
		// here, which retired one OS thread per failure for no gain.
		runtime.UnlockOSThread()
		return fmt.Errorf("dhcp: read the current network namespace: %w", err)
	}
	defer func() { _ = origin.Close() }()

	if err := netns.Set(ns); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("dhcp: enter the endpoint's network namespace: %w", err)
	}

	open()

	if err := netns.Set(origin); err != nil {
		// The thread is stranded in the container's namespace. Leaving
		// it locked takes it out of the scheduler's rotation for the
		// life of the process, which costs one OS thread; unlocking it
		// would hand a namespace-contaminated thread to unrelated
		// goroutines, which costs correctness everywhere.
		log.WithError(err).Error("Could not return the thread to the plugin's network namespace; it is retired")
		abandon()
		return fmt.Errorf("dhcp: return from the endpoint's network namespace: %w", err)
	}
	runtime.UnlockOSThread()
	return nil
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
