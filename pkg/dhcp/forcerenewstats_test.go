// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

func (f *fakeLib) setStats(fn func(*lease.Stats)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.stats)
}

func TestForcerenewStats_EachFieldComesFromItsOwnLibraryCounter(t *testing.T) {
	// Refused and AckRefused sit next to each other in lease.Stats and the library says one is not part of the other
	// (#1119); distinct values and decoys from the router fold catch a swap.
	in := lease.Stats{
		RouterAdvertsSeen:          999,
		RenewalsSent:               998,
		ForcerenewsRenewed:         11,
		ForcerenewsAlreadyRenewing: 22,
		ForcerenewsAckRefused:      33,
		ForcerenewsRefused:         44,
		ReconfiguresAccepted:       55,
		ReconfiguresRefused:        66,
	}
	got := forcerenewStats(in)
	for _, tc := range []struct {
		name string
		got  uint64
		want uint64
	}{
		{"Renewed", got.Renewed, 11},
		{"AlreadyRenewing", got.AlreadyRenewing, 22},
		{"AckRefused", got.AckRefused, 33},
		{"Refused", got.Refused, 44},
		{"ReconfiguresAccepted", got.ReconfiguresAccepted, 55},
		{"ReconfiguresRefused", got.ReconfiguresRefused, 66},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d; it is reading another of lease.Stats' counters", tc.name, tc.got, tc.want)
		}
	}
}

func TestForcerenewStats_SubGainsEachFieldAndSaturates(t *testing.T) {
	cur := ForcerenewStats{Renewed: 10, AlreadyRenewing: 20, AckRefused: 30, Refused: 40,
		ReconfiguresAccepted: 50, ReconfiguresRefused: 60}
	prev := ForcerenewStats{Renewed: 1, AlreadyRenewing: 2, AckRefused: 3, Refused: 4,
		ReconfiguresAccepted: 5, ReconfiguresRefused: 6}
	want := ForcerenewStats{Renewed: 9, AlreadyRenewing: 18, AckRefused: 27, Refused: 36,
		ReconfiguresAccepted: 45, ReconfiguresRefused: 54}
	if got := cur.Sub(prev); got != want {
		t.Errorf("gain = %+v, want %+v; a field the subtraction forgets reports its whole value each time", got, want)
	}
	if got := prev.Sub(cur); !got.IsZero() {
		t.Errorf("counters that went backwards produced %+v, want the zero value", got)
	}
}

func TestForcerenewStats_IsZeroSeesEachFieldAndAnyRefusedOnlyTheRefusals(t *testing.T) {
	if !(ForcerenewStats{}).IsZero() {
		t.Error("the zero value is not zero")
	}
	for _, tc := range []struct {
		field   string
		stats   ForcerenewStats
		refusal bool
	}{
		{"Renewed", ForcerenewStats{Renewed: 1}, false},
		{"AlreadyRenewing", ForcerenewStats{AlreadyRenewing: 1}, false},
		{"ReconfiguresAccepted", ForcerenewStats{ReconfiguresAccepted: 1}, false},
		{"Refused", ForcerenewStats{Refused: 1}, true},
		{"AckRefused", ForcerenewStats{AckRefused: 1}, true},
		{"ReconfiguresRefused", ForcerenewStats{ReconfiguresRefused: 1}, true},
	} {
		if tc.stats.IsZero() {
			t.Errorf("a gain in %s reads as nothing moved", tc.field)
		}
		if got := tc.stats.AnyRefused(); got != tc.refusal {
			t.Errorf("AnyRefused with only %s moved = %v, want %v", tc.field, got, tc.refusal)
		}
	}
}

func TestForcerenewReport_ReportsTheGainAndThenNothing(t *testing.T) {
	var got []ForcerenewStats
	o := DHCPClientOptions{OnForcerenewStats: func(s ForcerenewStats) { got = append(got, s) }}

	o.forcerenewReport(lease.Stats{ForcerenewsRenewed: 3, ForcerenewsRefused: 1})
	o.forcerenewReport(lease.Stats{ForcerenewsRenewed: 3, ForcerenewsRefused: 1})
	o.forcerenewReport(lease.Stats{ForcerenewsRenewed: 5, ForcerenewsRefused: 1})

	if len(got) != 2 {
		t.Fatalf("three folds over two distinct readings produced %d report(s), want 2", len(got))
	}
	if got[0].Renewed != 3 || got[0].Refused != 1 {
		t.Errorf("first report %+v, want 3 renewed and 1 refused", got[0])
	}
	if got[1].Renewed != 2 || got[1].Refused != 0 {
		t.Errorf("second report %+v, want the gain of 2 and no repeat of the refusal", got[1])
	}
}

func TestForcerenewReport_ANilCallbackIsSilentAndRemembersNothing(t *testing.T) {
	o := DHCPClientOptions{}
	o.forcerenewReport(lease.Stats{ForcerenewsRenewed: 4})
	if o.forcerenewSeen != (ForcerenewStats{}) {
		t.Errorf("a client with nowhere to report to remembered %+v; a client that later gained a callback "+
			"would skip its first gain", o.forcerenewSeen)
	}
}

func TestForcerenewReport_ASecondManagerOnOneOptionsValueIsReportedInFull(t *testing.T) {
	var total uint64
	o := DHCPClientOptions{OnForcerenewStats: func(s ForcerenewStats) { total += s.Renewed }}

	o.managerStarted()
	o.forcerenewReport(lease.Stats{ForcerenewsRenewed: 5})
	// A new manager's counters start at zero; a snapshot left over from the first saturates the second's gain away.
	o.managerStarted()
	o.forcerenewReport(lease.Stats{ForcerenewsRenewed: 3})

	if total != 8 {
		t.Errorf("two managers reported %d renewal(s), want 8; managerStarted does not reset the snapshot", total)
	}
}

func TestTranslate_ARefusedForcerenewIsReportedWithNoLeaseEvent(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan ForcerenewStats, 8)
	c := &DHCPClient{
		iface:     "test0",
		opts:      DHCPClientOptions{OnForcerenewStats: func(s ForcerenewStats) { reports <- s }},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Millisecond,
	}
	go c.translate()

	lib.setStats(func(s *lease.Stats) { s.ForcerenewsRefused = 2 })

	select {
	case s := <-reports:
		if s.Refused != 2 {
			t.Fatalf("the timer reported %d refusal(s), want 2", s.Refused)
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("no refusal was reported within %v with no lease event on the stream. A refused FORCERENEW "+
			"produces no event, so a fold on the event arm alone shows it at the next renewal, up to a "+
			"lease length later", wedgeBudget)
	}

	close(lib.src)
}

func TestTranslate_ALeaseEventCarriesTheForcerenewsSeenSoFar(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan ForcerenewStats, 8)
	c := &DHCPClient{
		iface:     "test0",
		opts:      DHCPClientOptions{OnForcerenewStats: func(s ForcerenewStats) { reports <- s }},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Hour,
	}
	go c.translate()

	lib.setStats(func(s *lease.Stats) { s.ForcerenewsRenewed = 1 })
	lib.src <- lease.Event{Kind: lease.Acquired}

	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("the Acquired event was not translated within %v", wedgeBudget)
	}
	select {
	case s := <-reports:
		if s.Renewed != 1 {
			t.Fatalf("the lease event carried %d renewal(s), want 1", s.Renewed)
		}
	default:
		t.Fatal("a translated lease event was delivered with no FORCERENEW report ahead of it")
	}

	close(lib.src)
}

func TestTranslate_TheLastForcerenewsAreReportedWhenTheClientStops(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan ForcerenewStats, 8)
	c := &DHCPClient{
		iface:     "test0",
		opts:      DHCPClientOptions{OnForcerenewStats: func(s ForcerenewStats) { reports <- s }},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Hour,
	}
	go c.translate()

	lib.setStats(func(s *lease.Stats) { s.ReconfiguresRefused = 4 })
	close(lib.src)

	select {
	case s := <-reports:
		if s.ReconfiguresRefused != 4 {
			t.Fatalf("the stopping client reported %d, want 4", s.ReconfiguresRefused)
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("a client that stopped between two timer ticks reported nothing within %v; the manager's "+
			"counters end with it", wedgeBudget)
	}
}
