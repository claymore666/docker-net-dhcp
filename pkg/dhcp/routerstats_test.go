// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"math"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

// TestRouterStats_EachFieldComesFromItsOwnLibraryCounter is the test a
// projection needs and a review cannot replace.
//
// Six fields copied out of a struct with six neighbouring fields of the
// same type is the shape in which a wrong source compiles, passes every
// other test in this package, and publishes a number that moves
// plausibly and means something else -- RouterAdvertsSeen taking
// NDSeen, which counts every Neighbor Discovery frame on the link and
// is always the larger. Six DISTINCT values are what make that
// visible; six copies of 1 would not.
func TestRouterStats_EachFieldComesFromItsOwnLibraryCounter(t *testing.T) {
	// Values that share no digits, so a swapped pair cannot pass by
	// coincidence, and NDSeen set to something else entirely: it is the
	// neighbour a sighting count is most likely to be taken from.
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

// A manager that has seen nothing projects to the zero value, which is
// what makes IsZero the "nothing moved" test rather than "no router
// advertisement was ever possible".
func TestRouterStats_AQuietManagerIsZero(t *testing.T) {
	if got := routerStats(lease.Stats{}); !got.IsZero() {
		t.Errorf("a manager with no counters projects to %+v, want the zero value", got)
	}
	if routerStats(lease.Stats{RouterSolicitsSent: 1}).IsZero() {
		t.Error("one solicitation reads as nothing moved")
	}
}

// Sub is a difference of unsigned counters, so the direction that must
// never happen is the one to drive: a smaller current value would
// otherwise wrap into a delta of billions and add it to a plugin
// counter that may only rise.
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

// TestRouterReport_ReportsTheGainAndThenNothing.
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

// Each manager remembers its own snapshot, which is what makes the
// process-wide counter the sum of the deltas rather than a total
// repeated once per manager.
//
// The failure this drives is not hypothetical: the remembered value
// lives on DHCPClientOptions and a manager builds its own, so a version
// that put it on the Plugin or in a package variable would report the
// second manager's whole total again on its first fold. Here the second
// manager has seen fewer advertisements than the first, which is the
// ordinary case on a host where one container has been up for a day
// and another has just started.
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

// A nil callback is the unit-test and probe shape, and it must not
// panic or consume the snapshot: a client built without a plugin still
// runs the fold at every site.
func TestRouterReport_ANilCallbackIsSilent(t *testing.T) {
	o := DHCPClientOptions{}
	o.routerReport(lease.Stats{RouterAdvertsSeen: 4})
	if o.routerSeen != (RouterStats{}) {
		t.Errorf("a client with nowhere to report to remembered %+v; the snapshot belongs to "+
			"the callback and a client that later gained one would skip its first gain",
			o.routerSeen)
	}
}

// TestTranslate_AnAdvertisementIsReportedWithNoLeaseEvent is the defect
// this counter would otherwise carry, driven at the seam it lives at.
//
// THE DEFECT. An advertisement arrives from the LINK. A router that is
// advertising every few seconds on a segment whose lease is not moving
// produces no lease event at all: the library's router table changes,
// the merged lease does not, and nothing is emitted. A fold on the
// event arm alone therefore reads zero for the whole life of a quiet
// lease -- the same shape #940 found for renewals and acdReport
// documents for probes (a probe run unreported for 19h52m, MEASURED on
// a production host).
//
// So this test delivers NO lease event and no change of any kind. It
// moves the library's counters from underneath, the way the wire does,
// and asserts the plugin side is told. Deleting the routerReport line
// from translate's advertisement-watch arm leaves every other test in
// this package green and kills this one.
func TestTranslate_AnAdvertisementIsReportedWithNoLeaseEvent(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RouterStats, 8)
	c := &DHCPClient{
		iface: "test0",
		opts: DHCPClientOptions{
			V6:            true,
			OnRouterStats: func(s RouterStats) { reports <- s },
		},
		events: newEventChan(),
		src:    lib.src,
		runner: lib,
		// The renewal fold is not what is under test and would
		// otherwise be the only tick in the loop.
		pollEvery: time.Hour,
	}
	go c.translate()

	// Three advertisements on a link whose lease never moves.
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

// The other direction, and the one that would make an IPv4-only host
// publish router-discovery numbers it cannot have.
//
// The advertisement watch is armed on the v6 path only, so a v4
// client's loop has no arm to fold in. The library's counters are moved
// anyway here -- a v4 manager can never raise them, and the point is
// that the chassis does not read them even if something did.
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

// A saturating addUint64 has an unreachable arm, and the counter it
// guards is the one that may never go backwards. Driven here rather
// than left to the comment: the health surface is int32 and the library
// counts in uint64, so a cast is the natural thing to write and turns
// two billion advertisements into a negative number, which a
// Prometheus consumer reads as a counter reset.
func TestRouterStats_SubHoldsAValueTooLargeForTheHealthSurface(t *testing.T) {
	cur := RouterStats{AdvertsSeen: math.MaxInt32 + 1}
	if got := cur.Sub(RouterStats{}).AdvertsSeen; got != math.MaxInt32+1 {
		t.Fatalf("the delta was %d, want %d; the clamp belongs to the plugin's fold and not here, "+
			"where the value is still the library's own", got, uint64(math.MaxInt32)+1)
	}
}
