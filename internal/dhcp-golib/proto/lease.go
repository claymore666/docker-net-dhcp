package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// Lease is what the machine derived from an ACK.
//
// Every deadline is an Instant on the monotonic clock: the only clock ring 1
// has, and RFC 2131 section 3.3 requires intervals to be measured on a clock
// that does not step. Turning these into a persistable wall-clock expiry is
// ring 2's job — a monotonic reading means nothing to the next process, and
// mixing the two in the pure ring is how a lease survives a restart with the
// wrong deadline.
type Lease struct {
	// Addr is the address with the mask the server gave, so a caller has the
	// prefix in one value. When the server sends no subnet mask the prefix is
	// the address with a /32 — stated rather than guessed at, because
	// inventing a classful mask is how a client ends up with a route it was
	// never given.
	Addr netip.Prefix

	ServerID netip.Addr

	// Router is option 3, AFTER RFC 3442's supersession: it is empty when a
	// usable option 121 arrived, because RFC 3442 says "if the DHCP server
	// returns both a Classless Static Routes option and a Router option, the
	// DHCP client MUST ignore the Router option". The raw option is still in
	// Options for anything that wants to see what was sent.
	Router []netip.Addr

	// Routes is the routing table the server supplied, already resolved: the
	// option 121 routes when there are any, otherwise the option 33 ones.
	// RFC 3442 supersedes 33 as well as 3.
	Routes []wire.Route

	DNS []netip.Addr
	// Domain is option 15; DomainSearch is option 119 (RFC 3397), decoded.
	Domain       string
	DomainSearch []string
	MTU          int

	// Start is the Instant at which the REQUEST that produced this lease was
	// SENT, not the Instant its ACK arrived.
	//
	// RFC 2131 section 4.4.5: "the client computes the lease expiration time
	// as the sum of the time at which the client sent the DHCPREQUEST message
	// and the duration of the lease in the DHCPACK message." Using the ACK
	// arrival is a systematic half-round-trip error in the direction of
	// holding the lease slightly too long, and it is invisible on a fixture
	// where the round trip is microseconds.
	Start Instant

	// LeaseTime, T1 and T2 as the server supplied them. T1 and T2 are zero
	// when the server sent neither, and are NOT defaulted here — see
	// Deadlines, which applies RFC 2131 section 4.4.5's 0.5/0.875 defaults
	// where they belong, at the point of use.
	LeaseTime Duration
	T1        Duration
	T2        Duration

	// Options is every option from the ACK, unparsed. See the requirements
	// document, section 9 choice 1: a forgotten option is recoverable rather
	// than gone.
	Options wire.Options
}

// Expire is the Instant the lease runs out, and whether it has one at all.
func (l Lease) Expire() (Instant, bool) {
	if l.LeaseTime.IsInfinite() {
		return 0, false
	}
	return l.Start.Add(l.LeaseTime), true
}

// Deadlines are the three moments the machine arms timers for.
//
// One derivation, because the alternative was two: RenewAt, RebindAt and
// Expire each computed their own answer, and only one of them could enforce an
// ordering between them. A caller that reads the three separately and a
// machine that arms three timers must not be able to disagree.
type Deadlines struct {
	Renew  Instant
	Rebind Instant
	Expire Instant

	HasRenew  bool
	HasRebind bool
	HasExpire bool

	// Note is empty unless a value was clamped, and says which and why. It
	// goes into the journal: a server sending T1 after T2 is a real
	// misconfiguration and the client silently working around it is how it
	// stays unfixed.
	Note string
}

// Deadlines applies RFC 2131 section 4.4.5's defaults — "T1 defaults to (0.5 *
// duration_of_lease). T2 defaults to (0.875 * duration_of_lease)" — and
// enforces its ordering: "T1 MUST be earlier than T2, which, in turn, MUST be
// earlier than the time at which the client's lease will expire."
//
// THAT MUST IS ADDRESSED TO WHOEVER SETS THE VALUES, NOT TO THIS CLIENT. A
// client that trusted it would arm a rebind before a renew, or a renew after
// the address is gone, on nothing worse than a mistyped server config. So the
// values are CLAMPED and the clamp is journalled, rather than the ACK being
// refused: refusing trades a working lease for a conformance point only the
// server can fix.
//
// The fallback for an out-of-order T1 is half of T2 and not the RFC's 0.5 *
// lease, because the server's explicit T2 is evidence about this lease that
// the default is not, and 0.5 * lease can itself be later than a short T2.
func (l Lease) Deadlines() Deadlines {
	var d Deadlines
	infinite := l.LeaseTime.IsInfinite()
	if !infinite {
		d.Expire, d.HasExpire = l.Start.Add(l.LeaseTime), true
	}
	if !infinite && l.LeaseTime <= 0 {
		// A lease of zero seconds, which a server is free to send and which
		// leaseFromAck accepts. It expires at the moment it was granted, so
		// there is no interval in which to renew or rebind: no T1, no T2, and
		// no clamp note about values that were never going to be used.
		//
		// The expiry above is still armed, for zero, so the machine reports
		// the loss at once instead of holding an address with no bound on it.
		return d
	}

	rebind := l.T2
	if rebind <= 0 && !infinite {
		// 0.875 == 7/8. Computed as (x/8)*7 rather than (x*7)/8 to keep the
		// intermediate away from the top of int64 for a near-infinite lease.
		rebind = (l.LeaseTime / 8) * 7
	}
	if !infinite && rebind >= l.LeaseTime {
		was := rebind
		rebind = (l.LeaseTime / 8) * 7
		d.Note = "T2 (" + was.String() + ") is not earlier than the lease (" + l.LeaseTime.String() +
			"): using RFC 2131 4.4.5's 0.875 default of " + rebind.String()
	}

	renew := l.T1
	if renew <= 0 && !infinite {
		renew = l.LeaseTime / 2
	}
	if rebind > 0 && renew >= rebind {
		was := renew
		renew = rebind / 2
		note := "T1 (" + was.String() + ") is not earlier than T2 (" + rebind.String() +
			"): using half of T2, " + renew.String()
		if d.Note == "" {
			d.Note = note
		} else {
			d.Note += "; " + note
		}
	}

	if renew > 0 {
		d.Renew, d.HasRenew = l.Start.Add(renew), true
	}
	if rebind > 0 {
		d.Rebind, d.HasRebind = l.Start.Add(rebind), true
	}
	return d
}

// RenewAt is T1 as an Instant. It derives from Deadlines so a caller reading
// it cannot get an answer the machine's timers disagree with.
func (l Lease) RenewAt() (Instant, bool) {
	d := l.Deadlines()
	return d.Renew, d.HasRenew
}

// RebindAt is T2 as an Instant. See RenewAt.
func (l Lease) RebindAt() (Instant, bool) {
	d := l.Deadlines()
	return d.Rebind, d.HasRebind
}

// Gateway is the default route this lease gives, and whether it gives one.
//
// It reads Routes first because RFC 3442's supersession has already been
// applied when Routes came from option 121: a lease carrying classless routes
// has an empty Router, and a classless route set with no 0.0.0.0/0 entry means
// the server deliberately gave no default route. Falling back to option 3
// there would reinstate exactly the option RFC 3442 says to ignore.
func (l Lease) Gateway() (netip.Addr, bool) {
	for _, r := range l.Routes {
		if r.IsDefault() && !r.OnLink() {
			return r.Router, true
		}
	}
	if len(l.Router) > 0 {
		return l.Router[0], true
	}
	return netip.Addr{}, false
}

// Equal reports whether two leases would configure an interface identically.
//
// Start is NOT compared: a renewal of the same address produces a new Start
// and an identical configuration, and a caller that reapplied the address on
// every renewal would churn the interface for nothing. Nor is Options, the
// pass-through bag: a server reordering an option nobody reads is not a
// changed lease.
func (l Lease) Equal(o Lease) bool {
	if l.Addr != o.Addr || l.ServerID != o.ServerID || l.Domain != o.Domain || l.MTU != o.MTU {
		return false
	}
	if !addrsEqual(l.Router, o.Router) || !addrsEqual(l.DNS, o.DNS) {
		return false
	}
	if !stringsEqual(l.DomainSearch, o.DomainSearch) {
		return false
	}
	return routesEqual(l.Routes, o.Routes)
}

func routesEqual(a, b []wire.Route) bool {
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

func stringsEqual(a, b []string) bool {
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

func addrsEqual(a, b []netip.Addr) bool {
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

func (l Lease) String() string {
	return fmt.Sprintf("%s from %s for %s", l.Addr, l.ServerID, l.LeaseTime)
}

// leaseFromAck builds a Lease from an ACK and the Instant the REQUEST was sent.
//
// It returns ok=false when the message cannot describe a lease at all: no
// yiaddr, or no lease time. Both are the server's obligation and a message
// missing either is not something to half-apply.
//
// The middle return is a note for the journal, empty when nothing was
// anomalous. The one anomaly tolerated here — a non-contiguous subnet mask —
// changes the lease handed back, and a silent change is the kind diagnosed as
// "the plugin used the wrong prefix" months later.
func leaseFromAck(m *wire.Message, sentAt Instant) (Lease, string, bool) {
	if !m.YIAddr.Is4() || m.YIAddr.IsUnspecified() {
		return Lease{}, "", false
	}
	secs, ok := m.Uint32(wire.OptLeaseTime)
	if !ok {
		return Lease{}, "", false
	}
	note := ""
	bits := 32
	if mask, ok := m.Addr4(wire.OptSubnetMask); ok {
		if n, ok := maskBits(mask); ok {
			bits = n
		} else {
			// A non-contiguous mask is not a prefix. Refusing the whole lease
			// over it would be worse than using a host route, so the address
			// is kept at /32 and the anomaly is journalled. Silently rounding
			// it to the nearest prefix is the option not taken.
			bits = 32
			note = "subnet mask " + mask.String() + " is not contiguous: address kept at /32"
		}
	}
	l := Lease{
		Addr:      netip.PrefixFrom(m.YIAddr, bits),
		Start:     sentAt,
		LeaseTime: SecondsToDuration(secs),
		Options:   m.Options.Clone(),
	}
	if sid, ok := m.Addr4(wire.OptServerID); ok {
		l.ServerID = sid
	}
	if r, ok := m.Addrs4(wire.OptRouter); ok {
		l.Router = r
	}
	if d, ok := m.Addrs4(wire.OptDNSServer); ok {
		l.DNS = d
	}
	if s, ok := m.Text(wire.OptDomainName); ok {
		l.Domain = s
	}
	if mtu, ok := m.Uint16(wire.OptInterfaceMTU); ok {
		l.MTU = int(mtu)
	}
	if t1, ok := m.Uint32(wire.OptRenewalTime); ok {
		l.T1 = SecondsToDuration(t1)
	}
	if t2, ok := m.Uint32(wire.OptRebindingTime); ok {
		l.T2 = SecondsToDuration(t2)
	}
	note = joinNotes(note, l.takeRoutes(m.Options))
	note = joinNotes(note, l.takeDomainSearch(m.Options))
	return l, note, true
}

// takeRoutes applies RFC 3442's precedence: option 121 supersedes both the
// router option (3) and the static-route option (33).
//
// A malformed option 121 FALLS BACK to 3 and 33 rather than superseding them,
// and the note says so. The supersession rule in RFC 3442 is written about a
// server that "returns both a Classless Static Routes option and a Router
// option"; a value that does not decode is not a route list, and a host with
// no default route at all is a worse answer than one with the route the older
// option gave. The fallback is journalled because the two outcomes are
// otherwise indistinguishable from the outside.
func (l *Lease) takeRoutes(o wire.Options) string {
	classless, err := o.ClasslessRoutes()
	if err != nil {
		l.takeStaticRoutes(o)
		return err.Error() + ": falling back to the router and static-route options (RFC 3442 supersession does not apply to a value that does not decode)"
	}
	if len(classless) > 0 {
		l.Routes = classless
		note := ""
		if len(l.Router) > 0 {
			note = "option 121 supersedes the router option (RFC 3442): ignoring " + addrsText(l.Router)
			l.Router = nil
		}
		if _, ok := o[wire.OptStaticRoute]; ok {
			note = joinNotes(note, "option 121 supersedes the static-route option (RFC 3442)")
		}
		return note
	}
	return l.takeStaticRoutes(o)
}

func (l *Lease) takeStaticRoutes(o wire.Options) string {
	static, err := o.StaticRoutes()
	if err != nil {
		return err.Error() + ": no static routes taken from it"
	}
	l.Routes = static
	return ""
}

func (l *Lease) takeDomainSearch(o wire.Options) string {
	names, err := o.DomainSearch()
	if err != nil {
		return err.Error() + ": the search list is left empty"
	}
	l.DomainSearch = names
	return ""
}

func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func addrsText(a []netip.Addr) string {
	out := ""
	for i, x := range a {
		if i > 0 {
			out += ","
		}
		out += x.String()
	}
	return out
}

// maskBits converts a dotted subnet mask to a prefix length, refusing a
// non-contiguous mask rather than silently accepting it.
func maskBits(mask netip.Addr) (int, bool) {
	if !mask.Is4() {
		return 0, false
	}
	v := mask.As4()
	u := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	n := 0
	for n < 32 && u&(1<<uint(31-n)) != 0 {
		n++
	}
	// Everything below the run of ones must be zero.
	if n < 32 && u<<uint(n) != 0 {
		return 0, false
	}
	return n, true
}
