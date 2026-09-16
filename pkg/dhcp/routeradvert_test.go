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

// infoFromRouter is the only reader of an advertisement on a segment
// that hands out no DHCPv6 address, so everything an advertisement
// carries has to come through it.
func TestInfoFromRouter(t *testing.T) {
	got, dropped := infoFromRouter(proto.RouterObservation{
		Seen:    true,
		Router:  netip.MustParseAddr("fe80::dead"),
		Routers: []netip.Addr{netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fe80::2")},
		MTU:     1400,
		DNS:     []netip.Addr{netip.MustParseAddr("2001:db8::53")},
		Search:  []string{"example.test"},
		Routes: []wire.Route{
			{Dest: netip.MustParsePrefix("::/0"), Router: netip.MustParseAddr("fe80::1")},
			{Dest: netip.MustParsePrefix("2001:db8:1::/48"), Router: netip.MustParseAddr("fe80::1")},
		},
		Prefixes: []wire.PrefixInfo{{
			Prefix: netip.MustParseAddr("2001:db8::"), PrefixLen: 64,
			OnLink: true, ValidLifetime: 3600,
		}},
	})
	if dropped != 0 {
		t.Errorf("dropped %d values from an advertisement carrying none", dropped)
	}
	if got.IP != "" {
		t.Errorf("IP = %q, want empty: forming an address from the prefix is SLAAC (#818), "+
			"and a caller's \"did this produce an address\" test reads this field", got.IP)
	}
	// THE GATEWAY IS Routers[0] AND NOT Router. Router is "who last
	// spoke" and never expires; Routers is the Default Router List,
	// which a Router Lifetime of 0 empties. Reading Router would hand
	// back a withdrawn router as a gateway forever, and the fixture
	// makes the two different so the wrong one cannot pass.
	if got.Gateway != "fe80::1" {
		t.Errorf("Gateway = %q, want fe80::1 (the first default router, not fe80::dead, "+
			"which is only the last speaker)", got.Gateway)
	}
	if got.MTU != 1400 {
		t.Errorf("MTU = %d, want 1400", got.MTU)
	}
	if len(got.DNSServers) != 1 || got.DNSServers[0] != "2001:db8::53" {
		t.Errorf("DNSServers = %v", got.DNSServers)
	}
	if len(got.SearchList) != 1 || got.SearchList[0] != "example.test" {
		t.Errorf("SearchList = %v", got.SearchList)
	}
	// ::/0 IS the default route (RFC 4191 allows it in a Route
	// Information option), so exporting it here as well would install
	// the default twice.
	if len(got.Routes) != 1 || got.Routes[0].Destination != "2001:db8:1::/48" {
		t.Errorf("Routes = %v, want only the non-default one", got.Routes)
	}
	if len(got.OnLinkPrefixes) != 1 || got.OnLinkPrefixes[0] != "2001:db8::/64" {
		t.Errorf("OnLinkPrefixes = %v", got.OnLinkPrefixes)
	}
}

// A router that withdrew itself leaves no gateway, which is what makes
// the withdrawal reach the container on a segment with no lease.
func TestInfoFromRouter_AWithdrawnRouterIsNoGateway(t *testing.T) {
	got, _ := infoFromRouter(proto.RouterObservation{
		Seen:   true,
		Router: netip.MustParseAddr("fe80::dead"),
		MTU:    1400,
	})
	if got.Gateway != "" {
		t.Errorf("Gateway = %q with an empty default router list, want empty", got.Gateway)
	}
	if got.MTU != 1400 {
		t.Errorf("MTU = %d: a withdrawn router does not un-say the link's MTU", got.MTU)
	}
}

// An acquisition that produced no address still hands back what the
// router said, and an acquisition that produced one is untouched.
//
// This is the whole of the #821 regression on a stateless or SLAAC
// segment: the guard turns the container's kernel off, so if this
// function returns the zero Info the container ends with a link-local
// and nothing else, where its kernel used to give it a route.
func TestAcquisitionResult6(t *testing.T) {
	ra := proto.RouterObservation{
		Seen:    true,
		Routers: []netip.Addr{netip.MustParseAddr("fe80::1")},
		MTU:     1400,
	}

	t.Run("no address: the advertisement comes through, with the error", func(t *testing.T) {
		got, err := acquisitionResult6(Info{}, ra, nil)
		if !errors.Is(err, ErrNoLease) {
			t.Errorf("err = %v, want ErrNoLease: the caller's absence verdict reads it", err)
		}
		if got.IP != "" {
			t.Errorf("IP = %q, want empty", got.IP)
		}
		if got.Gateway != "fe80::1" || got.MTU != 1400 {
			t.Errorf("the advertisement was dropped: gateway=%q mtu=%d", got.Gateway, got.MTU)
		}
	})

	t.Run("no address and a real cause: the cause is kept", func(t *testing.T) {
		cause := errors.New("the segment went quiet")
		_, err := acquisitionResult6(Info{}, ra, cause)
		if !errors.Is(err, cause) {
			t.Errorf("err = %v, want the cause: a segment that offers managed DHCPv6 and "+
				"then goes quiet is still fatal, and the verdict is made from this error", err)
		}
	})

	t.Run("an address: nothing is rewritten", func(t *testing.T) {
		lease := Info{IP: "2001:db8::5/64", Gateway: "fe80::9", MTU: 9000}
		got, err := acquisitionResult6(lease, ra, nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.IP != lease.IP || got.Gateway != lease.Gateway || got.MTU != lease.MTU {
			t.Errorf("the lease path was rewritten from the router table: %+v", got)
		}
	})
}
