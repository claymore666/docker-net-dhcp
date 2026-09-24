// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"math"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

func TestRouterStats_EachFieldComesFromItsOwnLibraryCounter(t *testing.T) {
	// Distinct values, and NDSeen counts every Neighbor Discovery frame, the likeliest wrong source (#814).
	in := lease.Stats{
		NDSeen:                     999,
		NDIgnored:                  998,
		RouterSolicitsSent:         11,
		RouterAdvertsSeen:          22,
		RouterAdvertsRefused:       33,
		RouterAdvertOptionsIgnored: 44,
		RouterTableEntriesDropped:  55,
		RouterTableEntriesEvicted:  66,
	}
	got := routerStats(in)
	for _, tc := range []struct {
		name string
		got  uint64
		want uint64
	}{
		{"SolicitsSent", got.SolicitsSent, 11},
		{"AdvertsSeen", got.AdvertsSeen, 22},
		{"AdvertsRefused", got.AdvertsRefused, 33},
		{"OptionsIgnored", got.OptionsIgnored, 44},
		{"EntriesDropped", got.EntriesDropped, 55},
		{"EntriesEvicted", got.EntriesEvicted, 66},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d; it is reading another of lease.Stats' counters",
				tc.name, tc.got, tc.want)
		}
	}
}

func TestRouterStats_AQuietManagerIsZero(t *testing.T) {
	if got := routerStats(lease.Stats{}); !got.IsZero() {
		t.Errorf("a manager with no counters projects to %+v, want the zero value", got)
	}
	if routerStats(lease.Stats{RouterSolicitsSent: 1}).IsZero() {
		t.Error("one solicitation reads as nothing moved")
	}
}

func TestRouterStats_SubSaturatesRatherThanWrapping(t *testing.T) {
	cur := RouterStats{SolicitsSent: 2, AdvertsSeen: 5}
	prev := RouterStats{SolicitsSent: 9, AdvertsSeen: 1}
	got := cur.Sub(prev)
	if got.SolicitsSent != 0 {
		t.Errorf("a counter that went backwards produced a delta of %d, want 0", got.SolicitsSent)
	}
	if got.AdvertsSeen != 4 {
		t.Errorf("the gain was %d, want 4", got.AdvertsSeen)
	}
}

func TestRouterReport_ReportsTheGainAndThenNothing(t *testing.T) {
	var got []RouterStats
	o := DHCPClientOptions{OnRouterStats: func(s RouterStats) { got = append(got, s) }}

	o.routerReport(lease.Stats{RouterAdvertsSeen: 3, RouterSolicitsSent: 1})
	o.routerReport(lease.Stats{RouterAdvertsSeen: 3, RouterSolicitsSent: 1})
	o.routerReport(lease.Stats{RouterAdvertsSeen: 5, RouterSolicitsSent: 1})

	if len(got) != 2 {
		t.Fatalf("three folds over two distinct readings produced %d report(s), want 2", len(got))
	}
	if got[0].AdvertsSeen != 3 || got[0].SolicitsSent != 1 {
		t.Errorf("first report %+v, want 3 seen and 1 solicit", got[0])
	}
	if got[1].AdvertsSeen != 2 || got[1].SolicitsSent != 0 {
		t.Errorf("second report %+v, want the gain of 2 and no repeat of the solicitation", got[1])
	}
}

func TestRouterReport_EachManagerReportsOnlyItsOwn(t *testing.T) {
	var total uint64
	fold := func(s RouterStats) { total += s.AdvertsSeen }

	first := DHCPClientOptions{OnRouterStats: fold}
	first.routerReport(lease.Stats{RouterAdvertsSeen: 100})

	second := DHCPClientOptions{OnRouterStats: fold}
	second.routerReport(lease.Stats{RouterAdvertsSeen: 4})
	second.routerReport(lease.Stats{RouterAdvertsSeen: 7})

	if total != 107 {
		t.Errorf("two managers reported %d advertisement(s) between them, want 107 "+
			"(100 from the first, then 4 and a gain of 3 from the second)", total)
	}
}

func TestRouterReport_ANilCallbackIsSilent(t *testing.T) {
	o := DHCPClientOptions{}
	o.routerReport(lease.Stats{RouterAdvertsSeen: 4})
	if o.routerSeen != (RouterStats{}) {
		t.Errorf("a client with nowhere to report to remembered %+v; the snapshot belongs to "+
			"the callback and a client that later gained one would skip its first gain",
			o.routerSeen)
	}
}

// A quiet router produces no lease event; acdReport's event-only fold left a probe run unreported for 19h52m on a
// production host (#814).

func TestTranslate_AnAdvertisementIsReportedWithNoLeaseEvent(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RouterStats, 8)
	c := &DHCPClient{
		iface: "test0",
		opts: DHCPClientOptions{
			V6:            true,
			OnRouterStats: func(s RouterStats) { reports <- s },
		},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Hour,
	}
	go c.translate()

	lib.advertsSeen(3)

	select {
	case s := <-reports:
		if s.AdvertsSeen != 3 {
			t.Fatalf("the advertisement watch reported %d advertisement(s), want 3", s.AdvertsSeen)
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("no advertisement was reported within %v, with no lease event on the stream. "+
			"That is the defect: the fold runs on the event arm only, so a link whose routers "+
			"are advertising and whose lease is quiet reads the same as a link with no router "+
			"on it.", wedgeBudget)
	}

	close(lib.src)
}

func TestTranslate_AV4ClientReportsNoAdvertisements(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RouterStats, 8)
	c := &DHCPClient{
		iface: "test0",
		opts: DHCPClientOptions{
			OnRouterStats: func(s RouterStats) { reports <- s },
		},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Hour,
	}
	go c.translate()

	lib.advertsSeen(3)

	select {
	case s := <-reports:
		t.Fatalf("a DHCPv4 client reported %+v; that family opens no Neighbor Discovery socket "+
			"and its advertisement watch is a ticker that never fires", s)
	case <-time.After(3 * raWatchInterval):
	}

	close(lib.src)
}

// The health surface is int32 and the library counts in uint64; a negative value reads as a Prometheus reset (#814).

func TestRouterStats_SubHoldsAValueTooLargeForTheHealthSurface(t *testing.T) {
	cur := RouterStats{AdvertsSeen: math.MaxInt32 + 1}
	if got := cur.Sub(RouterStats{}).AdvertsSeen; got != math.MaxInt32+1 {
		t.Fatalf("the delta was %d, want %d; the clamp belongs to the plugin's fold and not here, "+
			"where the value is still the library's own", got, uint64(math.MaxInt32)+1)
	}
}

func TestTranslate_TheLastAdvertisementsAreReportedWhenTheClientStops(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RouterStats, 8)
	c := &DHCPClient{
		iface: "test0",
		opts: DHCPClientOptions{
			V6:            true,
			OnRouterStats: func(s RouterStats) { reports <- s },
		},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Hour,
	}
	go c.translate()

	lib.advertsSeen(4)
	close(lib.src)

	select {
	case s := <-reports:
		if s.AdvertsSeen != 4 {
			t.Fatalf("the stopping client reported %d advertisement(s), want 4", s.AdvertsSeen)
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("a client that stopped between two advertisement watch ticks reported nothing "+
			"within %v. What it was holding is not late, it is lost: the manager is finished and "+
			"its counters end with it, so the process-wide total is permanently short by "+
			"everything that arrived since the last tick", wedgeBudget)
	}
}

func TestTranslate_ALeaseEventCarriesTheAdvertisementsSeenSoFar(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RouterStats, 8)
	c := &DHCPClient{
		iface: "test0",
		opts: DHCPClientOptions{
			V6:            true,
			OnRouterStats: func(s RouterStats) { reports <- s },
		},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Hour,
	}
	go c.translate()

	lib.advertsSeen(2)
	lib.src <- lease.Event{Kind: lease.Acquired}

	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("the Acquired event was not translated within %v", wedgeBudget)
	}

	select {
	case s := <-reports:
		if s.AdvertsSeen != 2 {
			t.Fatalf("the lease event carried %d advertisement(s), want 2", s.AdvertsSeen)
		}
	default:
		t.Fatalf("a translated lease event was delivered with no router-discovery report ahead " +
			"of it. The numbers an operator reads beside that event are then up to one " +
			"advertisement watch tick stale, so a segment that just advertised can read zero at " +
			"the moment its container came up without a gateway")
	}

	close(lib.src)
}

func TestManagerStarted_ASecondManagerOnOneOptionsValueIsReportedInFull(t *testing.T) {
	var (
		router    []RouterStats
		fallbacks []uint64
		ignored   []uint64
	)
	opts := &DHCPClientOptions{
		OnRouterStats:       func(s RouterStats) { router = append(router, s) },
		OnV6Fallback:        func(n uint64) { fallbacks = append(fallbacks, n) },
		OnV6PrefixesIgnored: func(n uint64) { ignored = append(ignored, n) },
	}

	pass := lease.Stats{
		RouterSolicitsSent:   3,
		RouterAdvertsSeen:    2,
		SLAACFallbacks:       1,
		SLAACPrefixesIgnored: 1,
	}

	opts.managerStarted()
	opts.routerReport(pass)
	opts.v6ModeReport(pass)
	opts.v6PrefixReport(pass)

	// errV6HintInUse reruns acquireOnce6 through the same options value with a fresh manager (#814).
	opts.managerStarted()
	opts.routerReport(pass)
	opts.v6ModeReport(pass)
	opts.v6PrefixReport(pass)

	var solicits, adverts uint64
	for _, s := range router {
		solicits += s.SolicitsSent
		adverts += s.AdvertsSeen
	}
	if solicits != 6 || adverts != 4 {
		t.Errorf("two acquisitions of one segment reported %d solicitation(s) and %d "+
			"advertisement(s), want 6 and 4. The second pass's manager starts its counters at "+
			"zero and the snapshot on the shared options value does not, so a saturating "+
			"subtraction takes the whole of the second acquisition away and has no direction "+
			"to complain in. The docs row says these are counted across every DHCPv6 client "+
			"this process ran, and this is the path where that stops being true", solicits, adverts)
	}

	var fell, skipped uint64
	for _, n := range fallbacks {
		fell += n
	}
	for _, n := range ignored {
		skipped += n
	}
	if fell != 2 {
		t.Errorf("the same two acquisitions reported %d SLAAC fallback(s), want 2: "+
			"v6ModeReport carries the same construction and the same defect", fell)
	}
	if skipped != 2 {
		t.Errorf("the same two acquisitions reported %d refused prefix(es), want 2: "+
			"v6PrefixReport carries the same construction and the same defect", skipped)
	}
}
