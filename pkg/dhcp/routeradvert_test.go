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
	"github.com/claymore666/dhcp-golib/wire"
)

func pfx(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse %v: %v", s, err)
	}
	return p
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %v: %v", s, err)
	}
	return a
}

// A default route never reaches Info.Routes, in EITHER family.
//
// v4: RFC 3442 says a 0.0.0.0/0 entry in option 121 supersedes option
// 3, and the library folds it into Lease.Gateway. v6: RFC 4191 section
// 2.3 explicitly allows a Route Information option for ::/0, and the
// plugin's IPv6 gateway comes from the router list -- a ::/0 route
// arriving here as a static route would install a SECOND default route
// beside it, with the winner decided by a metric comparison nobody
// chose.
func TestInfoFromLease_NoDefaultRouteInRoutes(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		routes []wire.Route
		keep   string
	}{
		{"v4 literal default", []wire.Route{
			{Dest: pfx(t, "0.0.0.0/0"), Router: addr(t, "192.0.2.1")},
			{Dest: pfx(t, "10.0.0.0/8"), Router: addr(t, "192.0.2.9")},
		}, "10.0.0.0/8"},
		{"v6 RIO default", []wire.Route{
			{Dest: pfx(t, "::/0"), Router: addr(t, "fe80::1")},
			{Dest: pfx(t, "2001:db8::/48"), Router: addr(t, "fe80::1")},
		}, "2001:db8::/48"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, _ := infoFromLease(lease.Lease{Routes: tc.routes}, proto.RouterObservation{}, now)
			if len(info.Routes) != 1 || info.Routes[0].Destination != tc.keep {
				t.Fatalf("Routes = %v, want only %v", info.Routes, tc.keep)
			}
		})
	}
}

// OnLinkPrefixes is the L flag and nothing else.
//
// WHY IT IS NEEDED AT ALL: the DHCPv6 address goes on the link as a
// /128 (RFC 9915 section 18.2.10.1), and RFC 5942 section 4 forbids
// deriving an on-link prefix from an assigned address, so without this
// nothing in the container's table says the segment's own prefix is
// reachable without a router.
func TestOnLinkPrefixes(t *testing.T) {
	got := onLinkPrefixes(proto.RouterObservation{Prefixes: []wire.PrefixInfo{
		// Taken: L set, live.
		{Prefix: addr(t, "2001:db8::"), PrefixLen: 64, OnLink: true, ValidLifetime: 600},
		// Not taken: A-only. RFC 4861 section 4.6.2 makes the two
		// flags independent, and an A-without-L prefix says how to
		// form an address, not what is reachable.
		{Prefix: addr(t, "2001:db8:1::"), PrefixLen: 64, Autonomous: true, ValidLifetime: 600},
		// Not taken: withdrawn. Valid Lifetime 0 is the withdrawal.
		{Prefix: addr(t, "2001:db8:2::"), PrefixLen: 64, OnLink: true, ValidLifetime: 0},
		// Not taken: link-local, which is on-link by definition and
		// already has a kernel route.
		{Prefix: addr(t, "fe80::"), PrefixLen: 64, OnLink: true, ValidLifetime: 600},
		// Not taken: ::/0 as an on-link prefix would make every
		// destination on-link and black-hole the container.
		{Prefix: addr(t, "::"), PrefixLen: 0, OnLink: true, ValidLifetime: 600},
		// Taken once: the same prefix twice is one route.
		{Prefix: addr(t, "2001:db8::"), PrefixLen: 64, OnLink: true, ValidLifetime: 1800},
		// Taken, masked: a router may send host bits.
		{Prefix: addr(t, "2001:db8:3::5"), PrefixLen: 64, OnLink: true, ValidLifetime: 600},
	}})

	want := []string{"2001:db8::/64", "2001:db8:3::/64"}
	if len(got) != len(want) {
		t.Fatalf("OnLinkPrefixes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OnLinkPrefixes = %v, want %v", got, want)
		}
	}
}

// advertisedDiffers watches the five fields an advertisement can change
// and NOTHING ELSE. The address and its lifetimes move on every
// renewal; a watch that read them would report a change the renewal had
// already applied, once per lease, forever.
func TestAdvertisedDiffers(t *testing.T) {
	base := Info{
		Gateway:    "fe80::1",
		MTU:        1500,
		DNSServers: []string{"2001:db8::53"},
		SearchList: []string{"corp.example"},
		Routes:     []Route{{Destination: "2001:db8:1::/48", Gateway: "fe80::1"}},
	}
	for _, tc := range []struct {
		name string
		to   func(Info) Info
		want bool
	}{
		{"identical", func(i Info) Info { return i }, false},
		{"gateway", func(i Info) Info { i.Gateway = "fe80::2"; return i }, true},
		{"gateway withdrawn", func(i Info) Info { i.Gateway = ""; return i }, true},
		{"mtu", func(i Info) Info { i.MTU = 1280; return i }, true},
		{"dns", func(i Info) Info { i.DNSServers = []string{"2001:db8::54"}; return i }, true},
		{"dns shrinks", func(i Info) Info { i.DNSServers = nil; return i }, true},
		{"search", func(i Info) Info { i.SearchList = []string{"other.example"}; return i }, true},
		{"routes", func(i Info) Info { i.Routes = nil; return i }, true},
		{"route next hop", func(i Info) Info {
			i.Routes = []Route{{Destination: "2001:db8:1::/48", Gateway: "fe80::9"}}
			return i
		}, true},
		{"address", func(i Info) Info { i.IP = "2001:db8::5/128"; return i }, false},
		{"lifetimes", func(i Info) Info { i.LeaseSeconds, i.PreferredSeconds = 99, 88; return i }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := advertisedDiffers(base, tc.to(base)); got != tc.want {
				t.Errorf("advertisedDiffers = %v, want %v", got, tc.want)
			}
		})
	}
}

// takeAdvertChange reports a CHANGE and never a first sight.
//
// The first reading is the baseline: on the path that matters the lease
// has just been applied through bound, so reporting it again would
// re-apply a configuration the container already has and write a second
// ledger row for one event.
func TestTakeAdvertChange_FirstSightIsSilent(t *testing.T) {
	c := &DHCPClient{}
	l := lease.Lease{Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }

	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("the first reading was reported as a change")
	}
	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("an unchanged second reading was reported as a change")
	}

	l.Gateway = addr(t, "fe80::2")
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a new router was not reported")
	}
	if ev.Type != "routeradvert" {
		t.Errorf("event type %q, want routeradvert", ev.Type)
	}
	if ev.Data.Gateway != "fe80::2" {
		t.Errorf("event gateway %q, want fe80::2", ev.Data.Gateway)
	}

	// And it does not repeat: the change was reported once.
	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("the same change was reported twice")
	}
}

// A withdrawal is a change like any other, and it is the one with no
// other mechanism: nothing else takes the container's default route
// away now that its kernel is at accept_ra=0.
func TestTakeAdvertChange_ReportsAWithdrawal(t *testing.T) {
	c := &DHCPClient{}
	l := lease.Lease{Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }
	c.takeAdvertChange(time.Now())

	l.Gateway = netip.Addr{}
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("the router withdrawing itself was not reported")
	}
	if ev.Data.Gateway != "" {
		t.Errorf("event gateway %q, want empty", ev.Data.Gateway)
	}
}

// No lease, no reading. A client whose library has not produced one yet
// must not have its zero value taken as a baseline, or the first real
// advertisement would look like a change from nothing and the one after
// it like nothing at all.
func TestTakeAdvertChange_NoLeaseIsSilentAndKeepsNoBaseline(t *testing.T) {
	c := &DHCPClient{}
	have := false
	l := lease.Lease{Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) {
		if !have {
			return lease.Lease{}, false
		}
		return l, true
	}
	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("a client with no lease reported a change")
	}
	if c.advertKnown {
		t.Fatal("a client with no lease recorded a baseline")
	}
	have = true
	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("the first real reading was reported as a change")
	}
	l.Gateway = addr(t, "fe80::2")
	if _, ok := c.takeAdvertChange(time.Now()); !ok {
		t.Fatal("the change after the first real reading was not reported")
	}
}

// baselineAdvert takes the reading and reports nothing, for the caller
// that has just applied the same values through the lease path.
func TestBaselineAdvert_SilencesTheNextReading(t *testing.T) {
	c := &DHCPClient{}
	l := lease.Lease{Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }
	c.advert, c.advertKnown = Info{Gateway: "fe80::9"}, true

	c.baselineAdvert(time.Now())
	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("a baselined reading was then reported as a change")
	}
	if c.advert.Gateway != "fe80::1" {
		t.Errorf("the baseline is %q, want the current reading fe80::1", c.advert.Gateway)
	}
}

// The watch runs four times inside RFC 4861 section 10's
// MIN_DELAY_BETWEEN_RAS, so the container's view survives two missed
// frames. A slower watch would make the delay before a container
// follows a renumbered router depend on how often the router talks.
func TestRAWatchInterval_FitsInsideTheMinimumDelayBetweenAdvertisements(t *testing.T) {
	if minDelayBetweenRAs != 3*time.Second {
		t.Errorf("minDelayBetweenRAs = %v, want RFC 4861 section 10's 3s", minDelayBetweenRAs)
	}
	if raWatchInterval <= 0 || raWatchInterval*2 >= minDelayBetweenRAs {
		t.Errorf("raWatchInterval = %v: two of them must fit inside %v", raWatchInterval, minDelayBetweenRAs)
	}
}

// CASE, NOT RULE (#821 -> #818). On a segment that hands out no
// DHCPv6 address the acquisition returns NOTHING, not the router's
// advertisement, and this test pins that wrong-but-required answer so
// it cannot be changed back by accident.
//
// The right answer is the advertisement's gateway, MTU and routes. It
// is not reachable yet: an endpoint with no global IPv6 address has
// IPv6 disabled on its link by the engine, and the kernel refuses
// every IPv6 route on such a link, so a Join answer carrying one fails
// the whole sandbox and the container does not start (MEASURED, lane
// run 35131643324). #818 gives the container a global address; when it
// does, this test is the one that changes.
func TestAcquisitionResult6(t *testing.T) {
	t.Run("no address: nothing comes through, with the reason", func(t *testing.T) {
		got, err := acquisitionResult6(Info{}, nil)
		if !errors.Is(err, ErrNoLease) {
			t.Errorf("err = %v, want ErrNoLease: the caller's absence verdict reads it", err)
		}
		if got.IP != "" || got.Gateway != "" || got.MTU != 0 ||
			len(got.Routes) != 0 || len(got.OnLinkPrefixes) != 0 || len(got.DNSServers) != 0 {
			t.Errorf("the Join answer would carry an IPv6 half on a link the engine has "+
				"disabled IPv6 on, and no container starts on the segment: %+v", got)
		}
	})

	t.Run("no address and a real cause: the cause is kept", func(t *testing.T) {
		cause := errors.New("the segment went quiet")
		_, err := acquisitionResult6(Info{}, cause)
		if !errors.Is(err, cause) {
			t.Errorf("err = %v, want the cause: a segment that offers managed DHCPv6 and "+
				"then goes quiet is still fatal, and the verdict is made from this error", err)
		}
	})

	t.Run("an address: nothing is rewritten", func(t *testing.T) {
		lease := Info{IP: "2001:db8::5/64", Gateway: "fe80::9", MTU: 9000}
		got, err := acquisitionResult6(lease, nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.IP != lease.IP || got.Gateway != lease.Gateway || got.MTU != lease.MTU {
			t.Errorf("the lease path was rewritten: %+v", got)
		}
	})
}

// THE ADVERTISED MTU IS THE ONLY MTU IPv6 HAS, and this is where it
// enters the plugin.
//
// DHCPv6 has no MTU option: option 26 is DHCPv4's (RFC 2132 section
// 5.1) and the library fills Lease.MTU from it alone, so a DHCPv6 lease
// carries MTU 0 forever. RFC 4861 section 4.6.4's MTU option is the
// only source, and until #821 nothing here read it because the
// container's kernel was at accept_ra=2 and applied it itself. With
// accept_ra=0 a zero here is an MTU the container never gets.
func TestInfoFromLease_TheAdvertisedMTUIsTheOnlyMTUIPv6Has(t *testing.T) {
	now := time.Now()

	t.Run("a DHCPv6 lease takes its MTU from the advertisement", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "2001:db8::5/64")}
		got, _ := infoFromLease(l, proto.RouterObservation{Seen: true, MTU: 1280}, now)
		if got.MTU != 1280 {
			t.Errorf("MTU = %d, want the advertised 1280. A DHCPv6 lease has no MTU of "+
				"its own, so a zero here is the container keeping the link MTU Docker "+
				"gave it while the segment asks for another", got.MTU)
		}
		if !got.RouterSeen {
			t.Error("RouterSeen is false on an event that carried an advertisement; " +
				"the plugin cannot then tell a router that went quiet about its MTU " +
				"from one that has not spoken yet")
		}
	})

	// THE DISTINCTION THE WITHDRAWAL RESTS ON. RFC 9915 section 18.2.1's
	// Solicit goes out without waiting for router discovery, so an event
	// can be stamped before the first advertisement arrives on a link
	// that does have a router. The MTU is 0 either way; this flag is
	// what separates "has not spoken" from "stopped saying".
	t.Run("an event stamped before the first advertisement says so", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "2001:db8::5/64")}
		got, _ := infoFromLease(l, proto.RouterObservation{}, now)
		if got.RouterSeen {
			t.Error("RouterSeen is true with no advertisement observed")
		}
		if got.MTU != 0 {
			t.Errorf("MTU = %d, want 0", got.MTU)
		}
	})

	// PRESERVATION CONTROL ONE: a DHCPv4 lease's own option 26 is not
	// overwritten by a router on the same link.
	t.Run("option 26 wins on its own family", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "192.0.2.5/24"), MTU: 9000}
		got, _ := infoFromLease(l, proto.RouterObservation{Seen: true, MTU: 1280}, now)
		if got.MTU != 9000 {
			t.Errorf("MTU = %d, want the lease's own 9000", got.MTU)
		}
	})

	// PRESERVATION CONTROL TWO: a DHCPv4 client never looks at a router
	// advertisement, so its observation is the zero value and no MTU
	// may be invented for it.
	t.Run("no advertisement seen, no MTU", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "192.0.2.5/24")}
		got, _ := infoFromLease(l, proto.RouterObservation{MTU: 1280}, now)
		if got.MTU != 0 {
			t.Errorf("MTU = %d, want 0: nothing advertised one", got.MTU)
		}
	})
}

// The live half of the same fact: a router that changes ONLY its MTU
// has changed the container's configuration, and the watch is the only
// thing that can notice -- no lease event happens, because the address
// did not move.
func TestTakeAdvertChange_AnMTUChangeIsAChange(t *testing.T) {
	c := &DHCPClient{}
	l := lease.Lease{Addr: pfx(t, "2001:db8::5/64"), Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }
	// The prefixes are in the fixture precisely so their EXCLUSION is
	// driven: a view that passed the whole observation through would
	// carry them, and the last assertion below is what catches it.
	ra := proto.RouterObservation{
		Seen: true, MTU: 1400,
		Prefixes: []wire.PrefixInfo{{
			Prefix: addr(t, "2001:db8::"), PrefixLen: 64,
			OnLink: true, ValidLifetime: 3600,
		}},
	}
	c.routerView = func() proto.RouterObservation { return ra }

	c.takeAdvertChange(time.Now()) // the baseline
	ra.MTU = 1280
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a router that lowered its advertised MTU was not reported; the " +
			"container keeps the old one for the life of the endpoint")
	}
	if ev.Data.MTU != 1280 {
		t.Errorf("event MTU = %d, want 1280", ev.Data.MTU)
	}

	// The withdrawal, which is the case propagateMTU's zero branch
	// exists for: a router that stops advertising an MTU is saying
	// nothing about it any more.
	ra.MTU = 0
	ev, ok = c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a router that stopped advertising an MTU was not reported")
	}
	if ev.Data.MTU != 0 {
		t.Errorf("event MTU = %d, want 0", ev.Data.MTU)
	}

	// THE PREFIXES STILL DO NOT FOLLOW. The router view carries the MTU
	// and nothing else, so on-link determination stays where it is
	// decided once, at Join.
	if len(ev.Data.OnLinkPrefixes) != 0 {
		t.Errorf("OnLinkPrefixes = %v, want none from the live watch", ev.Data.OnLinkPrefixes)
	}
}
