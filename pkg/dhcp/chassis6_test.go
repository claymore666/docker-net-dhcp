// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/vishvananda/netns"
)

// HonorRouterAdverts is a precondition, not a preference: a persistent
// DHCPv6 client cannot start without it, and nothing else can start with
// it.
//
// WHY A REFUSAL AND NOT AN OPERATOR OPTION (D30 Q3). DHCPv6 carries no
// next-hop: RFC 9915 section 21 defines no router option, and RFC 5942
// section 4 rule 1 says an address's prefix is NOT implicitly on-link.
// An endpoint whose kernel is not processing Router Advertisements
// therefore ends up with an address and no route, and it ends up there
// silently -- the lease is fine, the address is on the link, and every
// packet leaves through nothing. There is no configuration in which the
// plugin should start that client, so the field is not a switch; it is
// the caller stating it has done the work, and the four cases below are
// the whole domain of who may state it.
func TestCheckRouterAdvertGuardShape(t *testing.T) {
	ns := netns.NsHandle(-1)
	cases := []struct {
		name    string
		v6      bool
		honor   bool
		netNS   bool
		oneShot bool
		wantErr bool
	}{
		{name: "the persistent v6 client", v6: true, honor: true, netNS: true},
		{name: "a persistent v6 client without the guard", v6: true, netNS: true, wantErr: true},
		{name: "a persistent v6 client with no namespace", v6: true, honor: true, wantErr: true},
		{name: "the v6 one-shot", v6: true, oneShot: true},
		{name: "a v6 one-shot claiming the guard", v6: true, honor: true, netNS: true, oneShot: true, wantErr: true},
		{name: "the persistent v4 client", netNS: true},
		{name: "a v4 client claiming the guard", honor: true, netNS: true, wantErr: true},
		{name: "the v4 one-shot", oneShot: true},
		{name: "a v4 one-shot claiming the guard", honor: true, oneShot: true, wantErr: true},
	}

	// NON-VACUITY on the input domain rather than the row count: three
	// booleans that the function reads, eight inhabitants, and the ninth
	// row above is the same shape twice with a namespace. A row that
	// stopped being here is a shape nothing judges.
	covered := map[[3]bool]bool{}
	for _, tc := range cases {
		covered[[3]bool{tc.v6, tc.honor, tc.oneShot}] = true
	}
	for _, v6 := range []bool{false, true} {
		for _, honor := range []bool{false, true} {
			for _, oneShot := range []bool{false, true} {
				if !covered[[3]bool{v6, honor, oneShot}] {
					t.Fatalf("no row for v6=%v honor=%v oneShot=%v", v6, honor, oneShot)
				}
			}
		}
	}
	// And both verdicts have to appear, or the table is checking one
	// constant.
	var accepts, refuses int
	for _, tc := range cases {
		if tc.wantErr {
			refuses++
		} else {
			accepts++
		}
	}
	if accepts == 0 || refuses == 0 {
		t.Fatalf("the table has %d accepting and %d refusing rows; it needs both", accepts, refuses)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &DHCPClientOptions{V6: tc.v6, HonorRouterAdverts: tc.honor}
			if tc.netNS {
				opts.NetNS = &ns
			}
			err := checkRouterAdvertGuardShape(opts, tc.oneShot)
			if tc.wantErr && err == nil {
				t.Error("accepted, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("refused with %v, want acceptance", err)
			}
		})
	}
}

// The router-discovery window is derived from the parameters the client
// will actually run with, and it is longer than a single solicitation.
//
// WHY IT EXISTS. getIP6 warns when the caller's acquisition budget is
// shorter than this, because a "no router advertisement" verdict reached
// inside the window describes the deadline and not the segment -- and
// that verdict is what the plugin turns into "the segment has no IPv6
// router", an operator-facing claim about their network.
//
// The shape is RFC 4861: up to MAX_RTR_SOLICITATION_DELAY (section 6.3.7,
// 1 second) before the first solicitation, then MAX_RTR_SOLICITATIONS of
// them RTR_SOLICITATION_INTERVAL apart (section 10).
func TestRouterDiscoveryWindow(t *testing.T) {
	def := RouterDiscoveryWindow(proto.DefaultParams6())
	if want := time.Second + 3*4*time.Second; def != want {
		t.Errorf("the default window is %v, want %v (1s delay + 3 solicitations 4s apart)", def, want)
	}

	// A zero in either field is the library's "use the default", not
	// "zero seconds": a window of one second would make the warning fire
	// on every ordinary acquisition and stop meaning anything.
	if got := RouterDiscoveryWindow(proto.Params6{}); got != def {
		t.Errorf("the window for zero parameters is %v, want the default %v", got, def)
	}

	// And a caller that raised either knob gets a longer window, or the
	// warning is measured against a budget the client will overrun.
	slow := proto.DefaultParams6()
	slow.RouterSolicitations = 6
	if got := RouterDiscoveryWindow(slow); got <= def {
		t.Errorf("doubling the solicitations gave %v, not more than the default %v", got, def)
	}
	patient := proto.DefaultParams6()
	patient.RouterSolicitInterval = proto.RtrSolicitationInterval * 2
	if got := RouterDiscoveryWindow(patient); got <= def {
		t.Errorf("doubling the interval gave %v, not more than the default %v", got, def)
	}
}

// One library event, one verdict about whether the acquisition is over.
//
// THE Configured ROW IS THE ONE THIS TEST EXISTS FOR (D30 Q7). A
// stateless segment answers the Information-request with DNS servers and
// no address, and the library reports that as its own event kind. Ending
// the acquisition there rather than waiting out the deadline is worth a
// full lease_timeout per endpoint on every stateless network -- and the
// verdict is the same one the deadline would reach, because proto's v6
// machine only switches to the Information-request after an
// advertisement with M=0, so an address is no longer coming.
//
// The error carried is a SENTINEL and not a message, because
// classifyV6Absence over in pkg/plugin has to tell "the segment said no
// addresses" from "we ran out of time"; a formatted string would make
// that a substring match.
func TestAcquireStep6(t *testing.T) {
	now := time.Now()
	acquired := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{
		Addr:   netip.MustParsePrefix("2001:db8::5/128"),
		Expire: now.Add(time.Hour),
	}}

	got := acquireStep6(acquired, false)
	if !got.Done {
		t.Error("Acquired did not end the acquisition")
	}
	if got.Err != nil {
		t.Errorf("Acquired carried an error: %v", got.Err)
	}
	if got.Info.IP == "" {
		t.Error("Acquired produced no address")
	}

	got = acquireStep6(lease.Event{Kind: lease.Configured}, false)
	if !got.Done {
		t.Error("Configured did not end the acquisition; the endpoint would wait out " +
			"the whole lease_timeout for an address the segment has already declined to offer")
	}
	if !errors.Is(got.Err, ErrNoV6Address) {
		t.Errorf("Configured carried %v, want ErrNoV6Address: pkg/plugin distinguishes "+
			"\"no addresses here\" from \"out of time\" on this sentinel", got.Err)
	}
	if got.Info.IP != "" {
		t.Errorf("Configured produced an address %q", got.Info.IP)
	}

	got = acquireStep6(lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer}, false)
	if got.Done {
		t.Error("Failed ended the acquisition; the ladder's next attempt is the caller's " +
			"decision and the deadline is what ends it")
	}
	if got.Err == nil {
		t.Error("Failed carried no error, so the caller reports ErrNoLease with no reason")
	}

	// An event that says nothing about the outcome leaves the loop
	// running: Bound, Renewed and the rest arrive on this channel too.
	if got := acquireStep6(lease.Event{Kind: lease.Renewed}, false); got.Done || got.Err != nil {
		t.Errorf("Renewed ended the acquisition or carried an error: %+v", got)
	}
}

// The stateless reply's configuration reaches the container, and it
// goes through the same sanitiser every DHCP-supplied value does.
//
// The v6 path is a NEW source of server-controlled strings reaching
// /etc/resolv.conf, and the sanitiser is what stops a search domain with
// an embedded newline from writing a second directive into that file.
// It applies to the lease path already; this asserts it was not skipped
// on the way in from a Configured event.
func TestInfoFromConfig(t *testing.T) {
	cfg := lease.Configuration{
		DNS:    []netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2")},
		Search: []string{"example.test", "corp.example.test"},
	}
	info, dropped := infoFromConfig(cfg)
	if dropped != 0 {
		t.Errorf("dropped %d clean values", dropped)
	}
	if len(info.DNSServers) != 2 || info.DNSServers[0] != "2001:db8::1" {
		t.Errorf("DNSServers = %v, want both servers in order", info.DNSServers)
	}
	if len(info.SearchList) != 2 || info.SearchList[1] != "corp.example.test" {
		t.Errorf("SearchList = %v, want both domains in order", info.SearchList)
	}
	// A stateless reply has no address by definition; anything that put
	// one here would be the chassis inventing it.
	if info.IP != "" || info.Gateway != "" {
		t.Errorf("a Configured event produced IP %q gateway %q", info.IP, info.Gateway)
	}

	poisoned := lease.Configuration{Search: []string{"ok.test", "bad\nnameserver 10.0.0.1"}}
	info, dropped = infoFromConfig(poisoned)
	if dropped == 0 {
		t.Error("a search domain carrying a newline was not dropped: the plugin writes " +
			"this list into the container's resolv.conf")
	}
	for _, s := range info.SearchList {
		if s != "ok.test" {
			t.Errorf("kept %q", s)
		}
	}

	// The search list is the chassis's own copy: the library's
	// Configuration is the manager's, and a shared backing array is the
	// sanitiser rewriting a value the library will send again.
	cfg = lease.Configuration{Search: []string{"example.test"}}
	info, _ = infoFromConfig(cfg)
	cfg.Search[0] = "rewritten"
	if info.SearchList[0] != "example.test" {
		t.Errorf("infoFromConfig aliases the library's search list: it now reads %q",
			info.SearchList[0])
	}
}

// The v6 lease carries two lifetimes and the chassis reports both.
//
// RFC 9915 section 7.1 gives an IA Address a preferred and a valid
// lifetime; RFC 4862 section 5.5.4 makes the preferred one the point at
// which the address stops being used for NEW connections while existing
// ones survive to the valid lifetime. The kernel needs both to deprecate
// rather than delete, so a chassis that reported only the valid lifetime
// would have every v6 address stay preferred until the moment it
// vanished.
//
// A v4 lease has no such split, and PreferredSeconds stays zero there --
// which is what keeps the field's `omitempty` truthful.
func TestInfoFromLease_PreferredSecondsIsV6Only(t *testing.T) {
	now := time.Now()

	v6 := lease.Lease{
		Addr:      netip.MustParsePrefix("2001:db8::5/128"),
		Preferred: now.Add(30 * time.Minute),
		Expire:    now.Add(time.Hour),
	}
	info, _ := infoFromLease(v6, now)
	if info.LeaseSeconds != 3600 {
		t.Errorf("LeaseSeconds = %d, want 3600", info.LeaseSeconds)
	}
	if info.PreferredSeconds != 1800 {
		t.Errorf("PreferredSeconds = %d, want 1800", info.PreferredSeconds)
	}

	// An infinite preferred lifetime is the zero Time, on the library's
	// convention -- not "zero seconds", which would deprecate the
	// address the instant it was installed.
	v6.Preferred = time.Time{}
	info, _ = infoFromLease(v6, now)
	if info.PreferredSeconds != info.LeaseSeconds {
		t.Errorf("an infinite preferred lifetime gave PreferredSeconds = %d with "+
			"LeaseSeconds = %d; a zero here deprecates the address on arrival",
			info.PreferredSeconds, info.LeaseSeconds)
	}

	v4 := lease.Lease{
		Addr:      netip.MustParsePrefix("192.168.0.5/24"),
		Preferred: now.Add(30 * time.Minute),
		Expire:    now.Add(time.Hour),
	}
	if info, _ := infoFromLease(v4, now); info.PreferredSeconds != 0 {
		t.Errorf("a v4 lease produced PreferredSeconds = %d; DHCPv4 has one lifetime "+
			"and the field is omitempty", info.PreferredSeconds)
	}
}

// The router flags are rendered from an observation, and an observation
// with no advertisement in it renders nothing.
//
// The empty string is load-bearing: this goes into an event the operator
// reads, and "M" on a link where no advertisement ever arrived would be
// a claim about the segment derived from a zero value.
func TestRouterFlags(t *testing.T) {
	cases := []struct {
		r    proto.RouterObservation
		want string
	}{
		{proto.RouterObservation{}, ""},
		{proto.RouterObservation{Managed: true, Other: true}, ""},
		{proto.RouterObservation{Seen: true}, ""},
		{proto.RouterObservation{Seen: true, Managed: true}, "M"},
		{proto.RouterObservation{Seen: true, Other: true}, "O"},
		{proto.RouterObservation{Seen: true, Managed: true, Other: true}, "MO"},
	}
	for _, tc := range cases {
		if got := routerFlags(tc.r); got != tc.want {
			t.Errorf("routerFlags(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// Merge is OR and not last-wins.
//
// The server-policy ladder makes several attempts on one link. An
// advertisement seen on the first attempt is still evidence about the
// segment when the fourth times out, and a last-wins fold would report
// a routerless segment for a link that answered -- which is the
// difference between "your network has no IPv6 router" and "this
// attempt was short", told to the operator as if it were the same thing.
func TestRAObservation_MergeIsMonotonic(t *testing.T) {
	seen := RAObservation{Seen: true, Managed: true, Other: true}
	if got := seen.Merge(RAObservation{}); got != seen {
		t.Errorf("merging a quiet attempt into a seen one gave %+v, want %+v", got, seen)
	}
	if got := (RAObservation{}).Merge(seen); got != seen {
		t.Errorf("merging a seen attempt into a quiet one gave %+v, want %+v", got, seen)
	}
	a := RAObservation{Seen: true, Managed: true}
	b := RAObservation{Seen: true, Other: true}
	want := RAObservation{Seen: true, Managed: true, Other: true}
	if got := a.Merge(b); got != want {
		t.Errorf("%+v merged with %+v gave %+v, want %+v", a, b, got, want)
	}
	if (RAObservation{}).Merge(RAObservation{}) != (RAObservation{}) {
		t.Error("two quiet attempts merged into a seen one")
	}
}

// The library's observation crosses the seam field for field.
//
// pkg/plugin must not name a library type (D22/D23), so this conversion
// is the only place the two spellings meet; a field dropped here is a
// flag the absence classifier never sees.
func TestRAObservation_ConvertsEveryField(t *testing.T) {
	got := raObservation(proto.RouterObservation{Seen: true, Managed: true, Other: true})
	if !got.Seen || !got.Managed || !got.Other {
		t.Errorf("raObservation dropped a field: %+v", got)
	}
	if raObservation(proto.RouterObservation{}) != (RAObservation{}) {
		t.Error("raObservation invented a flag from a zero observation")
	}
	// One field at a time, or a conversion that ORed them together
	// would pass the all-true row.
	if got := raObservation(proto.RouterObservation{Managed: true}); got != (RAObservation{Managed: true}) {
		t.Errorf("raObservation(Managed) = %+v", got)
	}
	if got := raObservation(proto.RouterObservation{Other: true}); got != (RAObservation{Other: true}) {
		t.Errorf("raObservation(Other) = %+v", got)
	}
	if got := raObservation(proto.RouterObservation{Seen: true}); got != (RAObservation{Seen: true}) {
		t.Errorf("raObservation(Seen) = %+v", got)
	}
}

// TestV6AcquisitionWindow_FitsInsideTheDaemonsDeadline is the guard on
// the number #868's fix actually depends on.
//
// The verdict CreateEndpoint draws about a segment is worth nothing if
// it arrives after the daemon has abandoned the request, and the
// deadline the one-shot used to run under -- lease_timeout, whose
// default is ConflictRecoveryWindow -- is longer than that. This pins
// both ends: the window has to cover router discovery plus a real
// Solicit exchange, and it has to end well before the daemon does.
func TestV6AcquisitionWindow_FitsInsideTheDaemonsDeadline(t *testing.T) {
	p := proto.DefaultParams6()
	got := V6AcquisitionWindow(p)

	// The lower end. Below RouterDiscoveryWindow the "no router
	// advertisement" verdict describes the deadline rather than the
	// segment, which is the failure RouterDiscoveryWindow exists to
	// name, and a window with no Solicit allowance at all could not
	// acquire on a managed segment that drops one message.
	if got <= RouterDiscoveryWindow(p) {
		t.Errorf("V6AcquisitionWindow(%s) does not outlast router discovery (%s); "+
			"an absence verdict drawn inside it is about the deadline",
			got, RouterDiscoveryWindow(p))
	}
	// The upper end, and the reason this function exists. moby's plugin
	// client gives a request 30s; the endpoint's v4 half is spent
	// before the v6 half starts.
	const daemonDeadline = 30 * time.Second
	if got >= daemonDeadline {
		t.Errorf("V6AcquisitionWindow(%s) reaches the daemon's %s plugin deadline; "+
			"CreateEndpoint would be abandoned before it could report the segment",
			got, daemonDeadline)
	}
	// And the whole point: it is not lease_timeout.
	if got >= ConflictRecoveryWindow(proto.DefaultParams(nil)) {
		t.Errorf("V6AcquisitionWindow(%s) is not shorter than the v4-derived default "+
			"lease_timeout (%s), so the v6 one-shot still runs on DHCPv4's budget",
			got, ConflictRecoveryWindow(proto.DefaultParams(nil)))
	}
}

// TestV6SolicitWindow_CoversTheRetransmissionsItClaims derives the sum
// independently of the loop that produces it. RFC 9915 section 15
// doubles each timer and section 18.2.1 delays the first: with the
// library's one-second constants that is 1 + 1.1 + 2.2 + 4.4.
func TestV6SolicitWindow_CoversTheRetransmissionsItClaims(t *testing.T) {
	p := proto.DefaultParams6()
	if v6SolicitTransmissions != 4 {
		t.Fatalf("this expectation is written for 4 transmissions, not %d; "+
			"re-derive it rather than adjusting the total", v6SolicitTransmissions)
	}
	want := time.Duration(p.SolMaxDelay) +
		1100*time.Millisecond + 2200*time.Millisecond + 4400*time.Millisecond
	if got := v6SolicitWindow(p); got != want {
		t.Errorf("v6SolicitWindow = %s, want %s", got, want)
	}

	// A zero field means "the library's default" and must not mean
	// "zero": a Params6 built by hand would otherwise fund no Solicit
	// at all and the window would collapse to router discovery.
	if got := v6SolicitWindow(proto.Params6{}); got != want {
		t.Errorf("v6SolicitWindow(zero Params6) = %s, want the default's %s", got, want)
	}
}

// TestAdvertisedNoDHCPv6 is the discriminator behind the early SLAAC
// verdict, over every observation there is. Only one of the eight says
// "the segment has already told us DHCPv6 has nothing here"; the two
// that carry a flag are segments with something to ask for, and the
// four with nothing seen are segments that have not answered yet.
func TestAdvertisedNoDHCPv6(t *testing.T) {
	for _, tc := range []struct {
		ra   RAObservation
		want bool
	}{
		{RAObservation{}, false},
		{RAObservation{Managed: true}, false},
		{RAObservation{Other: true}, false},
		{RAObservation{Managed: true, Other: true}, false},
		{RAObservation{Seen: true}, true},
		{RAObservation{Seen: true, Managed: true}, false},
		{RAObservation{Seen: true, Other: true}, false},
		{RAObservation{Seen: true, Managed: true, Other: true}, false},
	} {
		if got := advertisedNoDHCPv6(tc.ra); got != tc.want {
			t.Errorf("advertisedNoDHCPv6(%+v) = %v, want %v", tc.ra, got, tc.want)
		}
	}
}

// TestErrNoDHCPv6OnSegment_ClassifiesAsNotOffered keeps the early
// verdict and the counter it feeds in step: an acquisition that ends
// this way must reach the operator as "the segment offers none", never
// as a fatal failure or as a missing router.
//
// It lives here rather than beside classifyV6Absence because the
// observation is what decides, and the observation that produces this
// error is the one asserted above.
func TestErrNoDHCPv6OnSegment_IsAnAdvertisedAbsence(t *testing.T) {
	ra := RAObservation{Seen: true}
	if !advertisedNoDHCPv6(ra) {
		t.Fatalf("the observation that produces %v is not an advertised absence", ErrNoDHCPv6OnSegment)
	}
	if errors.Is(ErrNoDHCPv6OnSegment, ErrNoV6Address) {
		t.Error("ErrNoDHCPv6OnSegment must not read as ErrNoV6Address: " +
			"the stateless Reply is an answer from a server, this is an answer from a router")
	}
}

// TestCarryResumedConfig6 is the whole of the Confirm gap.
//
// RFC 9915 section 18.2.13's Reply to a Confirm carries a status and no
// options, so the lease that comes out of a resumed binding has no DNS
// servers on it. The four cases below are the four things that can be
// true when the first event arrives, and the last two are the ones that
// keep the memory from becoming a second source of truth.
func TestCarryResumedConfig6(t *testing.T) {
	dns := func(s ...string) []netip.Addr {
		out := make([]netip.Addr, 0, len(s))
		for _, one := range s {
			out = append(out, netip.MustParseAddr(one))
		}
		return out
	}
	resume := func() *lease.Lease {
		return &lease.Lease{DNS: dns("2001:db8::53"), DomainSearch: []string{"corp.example"}}
	}

	t.Run("a confirmed lease with nothing on it is filled", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		ev := lease.Event{Kind: lease.Acquired}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 1 || ev.Lease.DNS[0].String() != "2001:db8::53" {
			t.Errorf("DNS = %v, want the remembered server", ev.Lease.DNS)
		}
		if len(ev.Lease.DomainSearch) != 1 || ev.Lease.DomainSearch[0] != "corp.example" {
			t.Errorf("DomainSearch = %v, want the remembered list", ev.Lease.DomainSearch)
		}
	})

	t.Run("a lease the server described is left alone", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		ev := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{DNS: dns("2001:db8::9")}}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 1 || ev.Lease.DNS[0].String() != "2001:db8::9" {
			t.Errorf("DNS = %v, want the server's own answer untouched", ev.Lease.DNS)
		}
		// The pair is all-or-nothing: a Reply carrying option 23 and
		// not option 24 has said there is no search list.
		if len(ev.Lease.DomainSearch) != 0 {
			t.Errorf("DomainSearch = %v, want none: the server sent DNS and no search list", ev.Lease.DomainSearch)
		}
	})

	t.Run("the memory is spent on the first lease-bearing event", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		first := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{DNS: dns("2001:db8::9")}}
		o.carryResumedConfig6(&first)
		later := lease.Event{Kind: lease.Renewed}
		o.carryResumedConfig6(&later)
		if len(later.Lease.DNS) != 0 {
			t.Errorf("DNS = %v on a later renewal, want none: the server has spoken since, "+
				"and a memory that keeps applying is a second source of truth", later.Lease.DNS)
		}
	})

	t.Run("an event carrying no lease does not spend it", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		lost := lease.Event{Kind: lease.Lost}
		o.carryResumedConfig6(&lost)
		if len(lost.Lease.DNS) != 0 {
			t.Errorf("a Lost was filled in: %v", lost.Lease.DNS)
		}
		ev := lease.Event{Kind: lease.Acquired}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 1 {
			t.Errorf("DNS = %v after a Lost, want the memory still available", ev.Lease.DNS)
		}
	})

	t.Run("a v4 client never carries one", func(t *testing.T) {
		o := &DHCPClientOptions{Resume: resume()}
		ev := lease.Event{Kind: lease.Acquired}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 0 {
			t.Errorf("DNS = %v on a v4 client: option 6 arrives in every DHCPACK, "+
				"including an INIT-REBOOT's, so there is nothing to carry", ev.Lease.DNS)
		}
	})
}

// fakeV6Client is a v6AcquisitionClient with no socket under it.
//
// Run blocks until its context is cancelled and then closes the event
// channel, which is what *dhcpruntime.Client6 does and what the drain
// at the end of runAcquisition6 depends on: a Run that returned without
// closing would park the drain forever.
type fakeV6Client struct {
	events chan lease.Event
	router proto.RouterObservation
}

func (f *fakeV6Client) Run(ctx context.Context) error {
	<-ctx.Done()
	close(f.events)
	return ctx.Err()
}

func (f *fakeV6Client) Events() <-chan lease.Event { return f.events }

func (f *fakeV6Client) Router() proto.RouterObservation { return f.router }

// acquisition6Result runs runAcquisition6 in the background and refuses
// to wait longer than patience for it.
//
// The wait is bounded because the two mutants this file's tests kill --
// the early conclusion disabled, the window replaced by the caller's
// clock -- both express themselves as "later than it should have been",
// and a test that simply called the function would express that as a
// HANG, which is a third verdict rather than a failure.
func acquisition6Result(t *testing.T, ctx context.Context, client v6AcquisitionClient, opts *DHCPClientOptions, hint netip.Addr, window, patience time.Duration) (Info, time.Duration, error) {
	t.Helper()

	type result struct {
		info Info
		err  error
	}
	out := make(chan result, 1)
	start := time.Now()
	go func() {
		info, err := runAcquisition6(ctx, "test0", client, opts, hint, window)
		out <- result{info, err}
	}()

	select {
	case r := <-out:
		return r.info, time.Since(start), r.err
	case <-time.After(patience):
		t.Fatalf("runAcquisition6 did not return within %v; window was %v", patience, window)
		return Info{}, 0, nil
	}
}

// TestRunAcquisition6_EndsOnAnAdvertisementThatOffersNoDHCPv6 drives the
// early conclusion and its opposite.
//
// The first arm is the one #868 could not reach: a segment whose router
// says M=0 O=0 has ANSWERED, and waiting the window out to say so is
// what put the verdict past the deadline the daemon keeps on a plugin
// call. The second arm is the preservation control, and it is not
// optional -- concluding on any advertisement at all would end a managed
// acquisition before the server had a chance to reply, which is a
// container with no address on a network that has one for it.
func TestRunAcquisition6_EndsOnAnAdvertisementThatOffersNoDHCPv6(t *testing.T) {
	t.Run("M=0 O=0 ends it without waiting the window out", func(t *testing.T) {
		client := &fakeV6Client{
			events: make(chan lease.Event),
			router: proto.RouterObservation{Seen: true},
		}
		_, took, err := acquisition6Result(t, context.Background(), client,
			&DHCPClientOptions{V6: true}, netip.Addr{}, 3*time.Second, 10*time.Second)

		if !errors.Is(err, ErrNoDHCPv6OnSegment) {
			t.Fatalf("runAcquisition6 returned %v after %v, want ErrNoDHCPv6OnSegment; "+
				"the segment advertised that it has no DHCPv6 and the acquisition "+
				"waited for one anyway", err, took)
		}
		if took > 2*time.Second {
			t.Errorf("the verdict took %v on an advertisement that arrived at once; "+
				"an operator's container start is charged for the whole window", took)
		}
	})

	t.Run("M=1 leaves the acquisition running", func(t *testing.T) {
		client := &fakeV6Client{
			events: make(chan lease.Event, 1),
			router: proto.RouterObservation{Seen: true, Managed: true},
		}
		go func() {
			time.Sleep(400 * time.Millisecond)
			client.events <- lease.Event{
				Kind:  lease.Acquired,
				Lease: lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")},
			}
		}()
		info, _, err := acquisition6Result(t, context.Background(), client,
			&DHCPClientOptions{V6: true}, netip.Addr{}, 5*time.Second, 10*time.Second)

		if err != nil {
			t.Fatalf("runAcquisition6: %v; a managed segment answered and the acquisition "+
				"had already concluded there was nothing to wait for", err)
		}
		if info.IP != "fd00:6470:6865::61/128" {
			t.Errorf("got address %q, want the leased one", info.IP)
		}
	})
}

// TestRunAcquisition6_HasItsOwnWindow drives the deadline this function
// keeps for itself.
//
// The caller's context is given a deadline far longer than the daemon
// will wait on a plugin call -- which is exactly the shape lease_timeout
// produced, and exactly what #868 saw -- so an acquisition that honours
// only the caller's clock never returns in time to say anything.
func TestRunAcquisition6_HasItsOwnWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := &fakeV6Client{events: make(chan lease.Event)}
	_, took, err := acquisition6Result(t, ctx, client,
		&DHCPClientOptions{V6: true}, netip.Addr{}, 300*time.Millisecond, 10*time.Second)

	if err == nil {
		t.Fatal("runAcquisition6 produced an address from a client that never said anything")
	}
	if took > 5*time.Second {
		t.Errorf("the acquisition ran for %v under a 300ms window; it is on the caller's "+
			"clock, and the caller's clock outlives the deadline the daemon keeps on "+
			"the plugin call", took)
	}
}

// TestAcquireStep6_AConflictOnAHintedAddressEndsTheAttempt drives the
// arm that makes the difference between a container that starts and a
// container that does not.
//
// MEASURED on the lane 2026-09-06 (run 34058213252): with the preferred
// address held by another node, the exchange ran Solicit -> Advertise ->
// Request -> Reply -> DAD -> Decline about once a second for sixteen
// seconds and then the daemon gave up on the plugin call. Every one of
// those rounds asked for the same address, because the hint is set when
// the client is built and a Decline does not clear it.
//
// THE TWO CONTROLS ARE NOT OPTIONAL. Ending on any conflict at all
// would take away the library's own recovery -- a conflict on a
// server-chosen address is answered by restarting discovery, and the
// next address is a different one -- and ending on any Failed at all
// would turn every transient refusal into a second acquisition.
func TestAcquireStep6_AConflictOnAHintedAddressEndsTheAttempt(t *testing.T) {
	conflict := lease.Event{Kind: lease.Failed, Reason: proto.ReasonConflict, Note: "in use"}

	got := acquireStep6(conflict, true)
	if !got.Done {
		t.Error("a conflict on the address this attempt ASKED for did not end it; the " +
			"library restarts discovery with the same hint, the server hands back the " +
			"same address, and the loop runs until the daemon's deadline")
	}
	if !errors.Is(got.Err, errV6HintInUse) {
		t.Errorf("the conflict carried %v, want errV6HintInUse: getIP6 decides on this "+
			"sentinel whether a second attempt is worth running", got.Err)
	}

	if got := acquireStep6(conflict, false); got.Done {
		t.Error("a conflict on a SERVER-CHOSEN address ended the attempt; there is no " +
			"loop to break there -- the library asks again and is given a different " +
			"address -- and ending it costs the endpoint a whole second acquisition")
	}
	if got := acquireStep6(lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer}, true); got.Done {
		t.Error("a Failed that is not a conflict ended the attempt on a hinted " +
			"acquisition; the ladder's next attempt is the caller's decision")
	}
}

// TestRetryWithoutHint6 drives the decision AND its bound.
//
// The bound is the point: the second pass must not be able to ask for a
// third, and it is held by the method's own state rather than by a
// counter at the call site, so it is checked here by asking twice.
func TestRetryWithoutHint6(t *testing.T) {
	hint := netip.MustParseAddr("fd00:6470:6863::90")
	inUse := fmt.Errorf("dhcp: %w: in use", errV6HintInUse)

	opts := &DHCPClientOptions{V6: true, Resume: &lease.Lease{}}
	opts.params6 = proto.Params6{Hint: hint}

	declined, again := opts.retryWithoutHint6(inUse)
	if !again {
		t.Fatal("a hinted attempt refused for a duplicate was not retried; the endpoint " +
			"fails with a deadline it could have avoided by asking for any other address")
	}
	if declined != hint {
		t.Errorf("the retry named %v as the declined address, want %v", declined, hint)
	}
	if opts.params6.Hint.IsValid() {
		t.Errorf("the second attempt still carries the hint %v, which is the address "+
			"another node holds: the retry would fetch it again", opts.params6.Hint)
	}
	if opts.Resume != nil {
		t.Error("the second attempt still carries the resumed binding, which names the " +
			"declined address: its Confirm asks the server to bless exactly what the " +
			"node just refused (RFC 9915 section 18.2.12)")
	}

	if _, again := opts.retryWithoutHint6(inUse); again {
		t.Error("a THIRD pass was offered. The retry is bounded by the hint it clears; " +
			"if it is not, a segment with a squatter turns CreateEndpoint into a loop")
	}

	// Preservation control: a hinted attempt that failed for any other
	// reason keeps both the hint and the resumption, and gets no second
	// pass. Widening this to every error would drop #213's preferred
	// address on any transient refusal.
	keep := &DHCPClientOptions{V6: true, Resume: &lease.Lease{}}
	keep.params6 = proto.Params6{Hint: hint}
	if _, again := keep.retryWithoutHint6(ErrNoLease); again {
		t.Error("an attempt that failed for a reason other than a duplicate was retried")
	}
	if keep.params6.Hint != hint || keep.Resume == nil {
		t.Errorf("the hint or the resumption was dropped by a refusal that was not a "+
			"duplicate (hint %v, resume %v)", keep.params6.Hint, keep.Resume)
	}

	// And an unhinted attempt has nothing to retry differently.
	none := &DHCPClientOptions{V6: true}
	if _, again := none.retryWithoutHint6(inUse); again {
		t.Error("an attempt that asked for no particular address was retried without one")
	}
}

// TestRunAcquisition6_AHintedConflictEndsTheLoop is the same decision
// one level up: the loop must return on the conflict rather than sit
// out the window, because sitting it out IS the defect.
func TestRunAcquisition6_AHintedConflictEndsTheLoop(t *testing.T) {
	hint := netip.MustParseAddr("fd00:6470:6863::90")
	conflict := lease.Event{Kind: lease.Failed, Reason: proto.ReasonConflict, Note: "in use"}

	client := &fakeV6Client{
		events: make(chan lease.Event, 1),
		router: proto.RouterObservation{Seen: true, Managed: true},
	}
	client.events <- conflict
	_, took, err := acquisition6Result(t, context.Background(), client,
		&DHCPClientOptions{V6: true}, hint, 5*time.Second, 10*time.Second)
	if !errors.Is(err, errV6HintInUse) {
		t.Fatalf("runAcquisition6 returned %v after %v, want errV6HintInUse", err, took)
	}
	if took > 2*time.Second {
		t.Errorf("the conflict took %v to reach the caller under a 5s window; the "+
			"remaining budget is what the second attempt has to run in", took)
	}

	// The control at this level: with no hint asked for, the same event
	// leaves the loop running for the library to recover in.
	unhinted := &fakeV6Client{
		events: make(chan lease.Event, 1),
		router: proto.RouterObservation{Seen: true, Managed: true},
	}
	unhinted.events <- conflict
	_, took, err = acquisition6Result(t, context.Background(), unhinted,
		&DHCPClientOptions{V6: true}, netip.Addr{}, time.Second, 10*time.Second)
	if errors.Is(err, errV6HintInUse) {
		t.Fatalf("an unhinted conflict produced errV6HintInUse after %v", took)
	}
	if took < time.Second {
		t.Errorf("the unhinted acquisition ended after %v, before its window was out; "+
			"the library's own recovery never got to run", took)
	}
}
