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

// RFC 3442: a 0.0.0.0/0 option 121 entry supersedes option 3; RFC 4191 section 2.3 allows a ::/0 Route Information
// option, which here would install a second default route (#821).

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
			info, _ := infoFromLease(lease.Lease{Routes: tc.routes}, proto.RouterObservation{}, now, netip.Prefix{})
			if len(info.Routes) != 1 || info.Routes[0].Destination != tc.keep {
				t.Fatalf("Routes = %v, want only %v", info.Routes, tc.keep)
			}
		})
	}
}

// RFC 5942 section 4 forbids deriving an on-link prefix from the /128 of RFC 9915 section 18.2.10.1 (#821).

func TestOnLinkPrefixes(t *testing.T) {
	got := onLinkPrefixes(proto.RouterObservation{Prefixes: []wire.PrefixInfo{
		{Prefix: addr(t, "2001:db8::"), PrefixLen: 64, OnLink: true, ValidLifetime: 600},
		// RFC 4861 section 4.6.2 makes the A and L flags independent.
		{Prefix: addr(t, "2001:db8:1::"), PrefixLen: 64, Autonomous: true, ValidLifetime: 600},
		{Prefix: addr(t, "2001:db8:2::"), PrefixLen: 64, OnLink: true, ValidLifetime: 0},
		{Prefix: addr(t, "fe80::"), PrefixLen: 64, OnLink: true, ValidLifetime: 600},
		{Prefix: addr(t, "::"), PrefixLen: 0, OnLink: true, ValidLifetime: 600},
		{Prefix: addr(t, "2001:db8::"), PrefixLen: 64, OnLink: true, ValidLifetime: 1800},
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
		{"on-link prefixes", func(i Info) Info { i.OnLinkPrefixes = []string{"2001:db8:2::/64"}; return i }, true},
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

	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("the same change was reported twice")
	}
}

// With accept_ra=0 nothing else takes the container's default route away (#821).

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

// RFC 4861 section 10's MIN_DELAY_BETWEEN_RAS holds four watch intervals (#821).

func TestRAWatchInterval_FitsInsideTheMinimumDelayBetweenAdvertisements(t *testing.T) {
	if minDelayBetweenRAs != 3*time.Second {
		t.Errorf("minDelayBetweenRAs = %v, want RFC 4861 section 10's 3s", minDelayBetweenRAs)
	}
	if raWatchInterval <= 0 || raWatchInterval*2 >= minDelayBetweenRAs {
		t.Errorf("raWatchInterval = %v: two of them must fit inside %v", raWatchInterval, minDelayBetweenRAs)
	}
}

// Case, not rule (#818): with no global IPv6 address the engine disables IPv6 on the link and the kernel refuses every
// IPv6 route, so a Join answer carrying one fails the sandbox (lane run 35131643324, #821).

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

// DHCPv6 has no MTU option; option 26 is DHCPv4's (RFC 2132 section 5.1), so RFC 4861 section 4.6.4's MTU option is the
// only source (#821).

func TestInfoFromLease_TheAdvertisedMTUIsTheOnlyMTUIPv6Has(t *testing.T) {
	now := time.Now()

	t.Run("a DHCPv6 lease takes its MTU from the advertisement", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "2001:db8::5/64")}
		got, _ := infoFromLease(l, proto.RouterObservation{Seen: true, MTU: 1280}, now, netip.Prefix{})
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

	// RFC 9915 section 18.2.1's Solicit does not wait for router discovery, so an event can predate the first
	// advertisement.
	t.Run("an event stamped before the first advertisement says so", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "2001:db8::5/64")}
		got, _ := infoFromLease(l, proto.RouterObservation{}, now, netip.Prefix{})
		if got.RouterSeen {
			t.Error("RouterSeen is true with no advertisement observed")
		}
		if got.MTU != 0 {
			t.Errorf("MTU = %d, want 0", got.MTU)
		}
	})

	t.Run("option 26 wins on its own family", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "192.0.2.5/24"), MTU: 9000}
		got, _ := infoFromLease(l, proto.RouterObservation{Seen: true, MTU: 1280}, now, netip.Prefix{})
		if got.MTU != 9000 {
			t.Errorf("MTU = %d, want the lease's own 9000", got.MTU)
		}
	})

	t.Run("no advertisement seen, no MTU", func(t *testing.T) {
		l := lease.Lease{Addr: pfx(t, "192.0.2.5/24")}
		got, _ := infoFromLease(l, proto.RouterObservation{MTU: 1280}, now, netip.Prefix{})
		if got.MTU != 0 {
			t.Errorf("MTU = %d, want 0: nothing advertised one", got.MTU)
		}
	})
}

func TestTakeAdvertChange_AnMTUChangeIsAChange(t *testing.T) {
	c := &DHCPClient{}
	l := lease.Lease{Addr: pfx(t, "2001:db8::5/64"), Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }
	ra := proto.RouterObservation{
		Seen: true, MTU: 1400,
		Prefixes: []wire.PrefixInfo{{
			Prefix: addr(t, "2001:db8::"), PrefixLen: 64,
			OnLink: true, ValidLifetime: 3600,
		}},
	}
	c.routerView = func() proto.RouterObservation { return ra }

	c.takeAdvertChange(time.Now())
	ra.MTU = 1280
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a router that lowered its advertised MTU was not reported; the " +
			"container keeps the old one for the life of the endpoint")
	}
	if ev.Data.MTU != 1280 {
		t.Errorf("event MTU = %d, want 1280", ev.Data.MTU)
	}

	ra.MTU = 0
	ev, ok = c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a router that stopped advertising an MTU was not reported")
	}
	if ev.Data.MTU != 0 {
		t.Errorf("event MTU = %d, want 0", ev.Data.MTU)
	}

	if len(ev.Data.OnLinkPrefixes) != 1 || ev.Data.OnLinkPrefixes[0] != "2001:db8::/64" {
		t.Errorf("OnLinkPrefixes = %v, want [2001:db8::/64] from the live watch", ev.Data.OnLinkPrefixes)
	}
}

func onLinkPIO(t *testing.T, a string, valid uint32) wire.PrefixInfo {
	t.Helper()
	return wire.PrefixInfo{Prefix: addr(t, a), PrefixLen: 64, OnLink: true, ValidLifetime: valid}
}

// advertWatch is a client holding a lease whose router observation the test swaps frame by frame.
func advertWatch(t *testing.T) (*DHCPClient, *proto.RouterObservation) {
	t.Helper()
	c := &DHCPClient{}
	l := lease.Lease{Addr: pfx(t, "2001:db8::5/128"), Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }
	ra := &proto.RouterObservation{Seen: true}
	c.routerView = func() proto.RouterObservation { return *ra }
	return c, ra
}

func TestTakeAdvertChange_APrefixFirstHeardAfterTheBaselineIsReported(t *testing.T) {
	c, ra := advertWatch(t)
	c.takeAdvertChange(time.Now())

	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)}
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("an advertisement whose only news is an on-link prefix was not reported; a lease bound " +
			"before the first advertisement never gets the prefix's route (#1088)")
	}
	if len(ev.Data.OnLinkPrefixes) != 1 || ev.Data.OnLinkPrefixes[0] != "2001:db8:1::/64" {
		t.Errorf("OnLinkPrefixes = %v, want [2001:db8:1::/64]", ev.Data.OnLinkPrefixes)
	}
}

func TestTakeAdvertChange_AFrameOmittingAPrefixIsNotAChange(t *testing.T) {
	c, ra := advertWatch(t)
	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)}
	c.takeAdvertChange(time.Now())

	// Two routers, or one splitting its options: each frame names a different subset of the link's prefixes.
	for i, frame := range [][]wire.PrefixInfo{nil, {onLinkPIO(t, "2001:db8:1::", 1800)}, nil} {
		ra.Prefixes = frame
		if ev, ok := c.takeAdvertChange(time.Now()); ok {
			t.Fatalf("frame %d omitting or repeating a known prefix was reported as a change: %+v", i, ev.Data)
		}
	}
	if got := c.advert.OnLinkPrefixes; len(got) != 1 || got[0] != "2001:db8:1::/64" {
		t.Fatalf("the known on-link set is %v after frames that only omitted it, want [2001:db8:1::/64]", got)
	}

	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:2::", 1800)}
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a second prefix was not reported")
	}
	if got := ev.Data.OnLinkPrefixes; len(got) != 2 || got[0] != "2001:db8:1::/64" || got[1] != "2001:db8:2::/64" {
		t.Errorf("OnLinkPrefixes = %v, want both, first heard first", got)
	}
}

func TestTakeAdvertChange_AZeroValidLifetimeWithdrawsThePrefix(t *testing.T) {
	c, ra := advertWatch(t)
	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800), onLinkPIO(t, "2001:db8:2::", 1800)}
	c.takeAdvertChange(time.Now())

	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 0)}
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a Valid Lifetime 0 for a known prefix was not reported (RFC 4861 section 6.3.4)")
	}
	if got := ev.Data.WithdrawnOnLinkPrefixes; len(got) != 1 || got[0] != "2001:db8:1::/64" {
		t.Errorf("WithdrawnOnLinkPrefixes = %v, want [2001:db8:1::/64]", got)
	}
	if got := ev.Data.OnLinkPrefixes; len(got) != 1 || got[0] != "2001:db8:2::/64" {
		t.Errorf("OnLinkPrefixes = %v, want only the prefix nothing withdrew", got)
	}
	if _, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatal("the same withdrawal frame, read again, was reported twice")
	}
}

func TestTranslate_ABoundEventCarriesThePrefixesTheBaselineRead(t *testing.T) {
	c, ra := advertWatch(t)
	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)}
	src := make(chan lease.Event, 1)
	c.src, c.events = src, newEventChan()

	// The event's own router reading predates the frame the baseline then reads.
	src <- lease.Event{Kind: lease.Acquired, Lease: lease.Lease{Addr: pfx(t, "2001:db8::5/128")}}
	close(src)
	c.translate()

	ev := <-c.events
	if ev.Type != "bound" {
		t.Fatalf("event %q, want bound", ev.Type)
	}
	if got := ev.Data.OnLinkPrefixes; len(got) != 1 || got[0] != "2001:db8:1::/64" {
		t.Errorf("OnLinkPrefixes = %v, want the baseline's [2001:db8:1::/64]: the watch now counts it as "+
			"known and reports it no more", got)
	}
}

// boundAcross is the bound event whose own router reading is event while the baseline then reads baseline (#1088).
func boundAcross(t *testing.T, event, baseline []wire.PrefixInfo) (*DHCPClient, *proto.RouterObservation, Event) {
	t.Helper()
	c, ra := advertWatch(t)
	ra.Prefixes = baseline
	src := make(chan lease.Event, 1)
	c.src, c.events = src, newEventChan()
	src <- lease.Event{Kind: lease.Acquired, Lease: lease.Lease{Addr: pfx(t, "2001:db8::5/128")},
		Router: proto.RouterObservation{Seen: true, Prefixes: event}}
	close(src)
	c.translate()
	return c, ra, <-c.events
}

func TestTranslate_ABoundEventHonoursAWithdrawalTheBaselineRead(t *testing.T) {
	const p = "2001:db8:1::/64"
	c, _, ev := boundAcross(t,
		[]wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)},
		[]wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 0)})
	if ev.Type != "bound" {
		t.Fatalf("event %q, want bound", ev.Type)
	}
	if containsString(ev.Data.OnLinkPrefixes, p) || !containsString(ev.Data.WithdrawnOnLinkPrefixes, p) {
		t.Errorf("bound OnLinkPrefixes = %v, Withdrawn = %v; the newer frame withdrew %s (RFC 4861 section 6.3.4)",
			ev.Data.OnLinkPrefixes, ev.Data.WithdrawnOnLinkPrefixes, p)
	}
	if containsString(c.advert.OnLinkPrefixes, p) {
		t.Errorf("the watch still counts the withdrawn %s as known: %v", p, c.advert.OnLinkPrefixes)
	}
}

func TestTranslate_ABoundEventKeepsAPrefixTheBaselineStillAdvertises(t *testing.T) {
	const p = "2001:db8:1::/64"
	c, ra, ev := boundAcross(t,
		[]wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 0)},
		[]wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)})
	if !containsString(ev.Data.OnLinkPrefixes, p) || containsString(ev.Data.WithdrawnOnLinkPrefixes, p) {
		t.Errorf("bound OnLinkPrefixes = %v, Withdrawn = %v; the newer frame advertises %s",
			ev.Data.OnLinkPrefixes, ev.Data.WithdrawnOnLinkPrefixes, p)
	}
	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 0)}
	w, ok := c.takeAdvertChange(time.Now())
	if !ok || !containsString(w.Data.WithdrawnOnLinkPrefixes, p) {
		t.Errorf("a later withdrawal of %s was not reported (%v, %+v)", p, ok, w.Data)
	}
}

func TestTranslate_ABoundEventPrefixTheBaselineOmitsIsKnownToTheWatch(t *testing.T) {
	const p = "2001:db8:1::/64"
	c, ra, ev := boundAcross(t, []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)}, nil)
	if !containsString(ev.Data.OnLinkPrefixes, p) {
		t.Fatalf("bound OnLinkPrefixes = %v, want %s from the event's own frame", ev.Data.OnLinkPrefixes, p)
	}
	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 0)}
	w, ok := c.takeAdvertChange(time.Now())
	if !ok || !containsString(w.Data.WithdrawnOnLinkPrefixes, p) {
		t.Errorf("the withdrawal of %s, installed from the bound event, was not reported (%v, %+v)", p, ok, w.Data)
	}
}

func TestWithdrawnOnLinkPrefixes_AZeroLifetimeWithoutTheLFlagWithdrawsNothing(t *testing.T) {
	r := proto.RouterObservation{Seen: true, Prefixes: []wire.PrefixInfo{
		{Prefix: addr(t, "2001:db8:1::"), PrefixLen: 64, Autonomous: true, ValidLifetime: 0},
	}}
	if got := withdrawnOnLinkPrefixes(r); len(got) != 0 {
		t.Errorf("withdrawn = %v; an option without the L flag says nothing about on-link (RFC 4861 section 4.6.2)",
			got)
	}

	c, ra := advertWatch(t)
	ra.Prefixes = []wire.PrefixInfo{onLinkPIO(t, "2001:db8:1::", 1800)}
	c.takeAdvertChange(time.Now())
	ra.Prefixes = r.Prefixes
	if ev, ok := c.takeAdvertChange(time.Now()); ok {
		t.Errorf("an L-clear zero-lifetime option was reported as a change: %+v", ev.Data)
	}
	if !containsString(c.advert.OnLinkPrefixes, "2001:db8:1::/64") {
		t.Errorf("the on-link set lost the prefix: %v", c.advert.OnLinkPrefixes)
	}
}
