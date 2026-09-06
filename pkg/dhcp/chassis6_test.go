// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
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

	got := acquireStep6(acquired)
	if !got.Done {
		t.Error("Acquired did not end the acquisition")
	}
	if got.Err != nil {
		t.Errorf("Acquired carried an error: %v", got.Err)
	}
	if got.Info.IP == "" {
		t.Error("Acquired produced no address")
	}

	got = acquireStep6(lease.Event{Kind: lease.Configured})
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

	got = acquireStep6(lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer})
	if got.Done {
		t.Error("Failed ended the acquisition; the ladder's next attempt is the caller's " +
			"decision and the deadline is what ends it")
	}
	if got.Err == nil {
		t.Error("Failed carried no error, so the caller reports ErrNoLease with no reason")
	}

	// An event that says nothing about the outcome leaves the loop
	// running: Bound, Renewed and the rest arrive on this channel too.
	if got := acquireStep6(lease.Event{Kind: lease.Renewed}); got.Done || got.Err != nil {
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
