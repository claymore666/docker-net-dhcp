// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

// wedgeBudget bounds every wait so a wedge fails the test instead of hanging it (#899).
const wedgeBudget = 5 * time.Second

// The dhcp_manager.go reader returns on stopChan and never reads again, which parked a bare send forever (#899).

func TestTranslate_AnEventInFlightAtStopDoesNotWedgeTheGoroutine(t *testing.T) {
	for _, tc := range []struct {
		name string
		read int
	}{
		{"the reader is already gone when the first event lands", 0},
		{"the reader leaves after one event", 1},
		{"the reader leaves mid-burst", eventBuffer / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// More events than the buffer holds, so a bare send would wedge (#899).
			const surplus = 8
			sends := tc.read + eventBuffer + surplus

			c, src, path := newTranslateHarness(t)
			go c.translate()

			readerGone := make(chan struct{})
			go func() {
				defer close(readerGone)
				for i := 0; i < tc.read; i++ {
					select {
					case <-c.events:
					case <-time.After(wedgeBudget):
						return
					}
				}
			}()
			send := func(from, to int) {
				t.Helper()
				for i := from; i < to; i++ {
					ev := lease.Event{
						Kind:  lease.Acquired,
						Lease: lease.Lease{Addr: netip.MustParsePrefix(fmt.Sprintf("192.168.99.%d/24", i+1))},
					}
					select {
					case src <- ev:
					case <-time.After(wedgeBudget):
						t.Fatalf("the library's event %d of %d could not be delivered within %v. "+
							"translate has parked on the emit and stopped consuming its source: "+
							"every later event is lost from the durable record, the event channel "+
							"is never closed, and the deferred counter write never runs.",
							i+1, sends, wedgeBudget)
					}
				}
			}

			send(0, tc.read)
			select {
			case <-readerGone:
			case <-time.After(wedgeBudget):
				t.Fatal("the plugin-side reader never got its events; the goroutine was " +
					"already stuck before the reader was taken away")
			}
			send(tc.read, sends)

			close(src)

			drained := make(chan int, 1)
			go func() {
				n := 0
				for range c.events {
					n++
				}
				drained <- n
			}()
			var delivered int
			select {
			case delivered = <-drained:
			case <-time.After(wedgeBudget):
				t.Fatalf("c.events was still open %v after the library's stream ended. "+
					"defer close(c.events) never ran, so the plugin's reader would never "+
					"learn the client had stopped.", wedgeBudget)
			}

			dropped := c.DroppedEvents()
			if dropped == 0 {
				t.Errorf("%d events were pushed at a reader that had stopped, %d were "+
					"delivered, and DroppedEvents() reports 0. Either nothing was dropped "+
					"— in which case this test is not reaching the guard at all — or the "+
					"drop is silent, which is the half of the base's guard this fix exists "+
					"to add.", sends, delivered)
			}
			// Every event is delivered or counted dropped (#899).
			if got := uint64(tc.read) + uint64(delivered) + dropped; got != uint64(sends) {
				t.Errorf("%d events in; %d read before the reader left + %d drained "+
					"afterwards + %d dropped = %d out. Every event must be one of the "+
					"three.", sends, tc.read, delivered, dropped, got)
			}

			lines := recordLines(t, path)
			var observed, stats int
			for _, l := range lines {
				if l.Op == uint8(lease.OpStats) {
					stats++
					if l.Manager != c.manager {
						t.Errorf("the counter line names manager %q, want %q", l.Manager, c.manager)
					}
					continue
				}
				observed++
			}
			if observed != sends {
				t.Errorf("the durable record holds %d event lines for %d events. A DROPPED "+
					"emit must still be recorded: c.opts.record(ev) runs before the "+
					"translation and unconditionally, which is precisely why dropping is a "+
					"smaller loss than blocking.", observed, sends)
			}
			if stats != 1 {
				t.Errorf("the record holds %d counter lines, want exactly 1. "+
					"defer c.opts.count(...) is the only writer of this manager's wire "+
					"counters — P-7's per-endpoint half — and a TICKED parity row that "+
					"produces nothing for an endpoint is the silent half of this defect.",
					stats)
			}
		})
	}
}

// newTranslateHarness builds a DHCPClient that runs translate with no socket over a real on-disk record.
func newTranslateHarness(t *testing.T) (*DHCPClient, chan lease.Event, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "records.jsonl")
	r, err := OpenRecords(path, "instance-translate")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("closing the record store: %v", err)
		}
	})

	src := make(chan lease.Event)
	c := &DHCPClient{
		iface:   "test0",
		opts:    DHCPClientOptions{Records: r, RecordID: "ep-translate"},
		events:  newEventChan(),
		src:     src,
		manager: r.NewManagerID(),
	}
	return c, src, path
}

// recordLine is the part of lease.RecordEvent this test reads from the file.
type recordLine struct {
	Op      uint8  `json:"op"`
	Manager string `json:"manager"`
}

func recordLines(t *testing.T, path string) []recordLine {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record file: %v", err)
	}
	var out []recordLine
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var rec recordLine
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("record line %q: %v", l, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestTranslate_AReaderThatKeepsReadingLosesNothing(t *testing.T) {
	const events = 64

	c, src, path := newTranslateHarness(t)
	go c.translate()

	for i := 0; i < events; i++ {
		ev := lease.Event{
			Kind:  lease.Acquired,
			Lease: lease.Lease{Addr: netip.MustParsePrefix(fmt.Sprintf("192.168.99.%d/24", i+1))},
		}
		select {
		case src <- ev:
		case <-time.After(wedgeBudget):
			t.Fatalf("event %d could not be handed to translate within %v", i+1, wedgeBudget)
		}
		select {
		case got := <-c.events:
			if got.Type != "bound" {
				t.Fatalf("event %d arrived as %q, want %q", i+1, got.Type, "bound")
			}
		case <-time.After(wedgeBudget):
			t.Fatalf("event %d was never delivered to the plugin within %v. With a reader "+
				"present and one event in flight, the emit cannot legitimately fail: a "+
				"guard that fires here has become the normal path rather than the "+
				"emergency one.", i+1, wedgeBudget)
		}
	}

	if dropped := c.DroppedEvents(); dropped != 0 {
		t.Errorf("%d of %d events were dropped with the plugin reading one at a time. The "+
			"non-blocking send is there for a reader that has GONE; firing it against a "+
			"reader that is present loses lease events in ordinary operation, which is a "+
			"worse defect than the wedge it replaces.", dropped, events)
	}

	close(src)
	select {
	case _, open := <-c.events:
		if open {
			t.Error("an extra event arrived after the library's stream ended")
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("c.events was still open %v after the library's stream ended", wedgeBudget)
	}

	lines := recordLines(t, path)
	var observed, stats int
	for _, l := range lines {
		if l.Op == uint8(lease.OpStats) {
			stats++
			continue
		}
		observed++
	}
	if observed != events || stats != 1 {
		t.Errorf("the record holds %d event lines and %d counter lines; want %d and 1",
			observed, stats, events)
	}
}

func TestTranslate_TheBufferHoldsWhatTheLibraryWasAskedToHold(t *testing.T) {
	c, src, _ := newTranslateHarness(t)
	go c.translate()

	for i := 0; i < eventBuffer; i++ {
		ev := lease.Event{
			Kind:  lease.Acquired,
			Lease: lease.Lease{Addr: netip.MustParsePrefix(fmt.Sprintf("192.168.99.%d/24", i+1))},
		}
		select {
		case src <- ev:
		case <-time.After(wedgeBudget):
			t.Fatalf("event %d of %d could not be handed to translate within %v",
				i+1, eventBuffer, wedgeBudget)
		}
	}
	close(src)

	drained := make(chan int, 1)
	go func() {
		n := 0
		for range c.events {
			n++
		}
		drained <- n
	}()
	var delivered int
	select {
	case delivered = <-drained:
	case <-time.After(wedgeBudget):
		t.Fatalf("c.events was still open %v after the library's stream ended", wedgeBudget)
	}

	if dropped := c.DroppedEvents(); dropped != 0 {
		t.Errorf("%d of %d events were dropped with nothing yet read. The chassis asks the "+
			"library for an EventBuffer of %d (newLibClient), so the library may hand over "+
			"that many before anyone reads one; a channel that cannot hold them drops in "+
			"ordinary operation rather than only when the reader has gone.",
			dropped, eventBuffer, eventBuffer)
	}
	if delivered != eventBuffer {
		t.Errorf("%d of %d events survived the buffer", delivered, eventBuffer)
	}
}

// RFC 9915 section 18.2.13's Reply to a Confirm carries a status and nothing else (#911).

func TestTranslate_CarriesTheResumedV6ResolverIntoTheRecord(t *testing.T) {
	c, src, path := newTranslateHarness(t)
	c.opts.V6 = true
	c.opts.Resume = &lease.Lease{
		DNS:          []netip.Addr{netip.MustParseAddr("fd00:6470:6865::1")},
		DomainSearch: []string{"lan.example"},
	}

	go c.translate()

	select {
	case src <- lease.Event{
		Kind:  lease.Acquired,
		Lease: lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")},
	}:
	case <-time.After(wedgeBudget):
		t.Fatalf("the event could not be handed to translate within %v", wedgeBudget)
	}
	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("translate emitted nothing within %v", wedgeBudget)
	}
	close(src)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record file: %v", err)
	}
	if !strings.Contains(string(raw), "fd00:6470:6865::1\"") {
		t.Errorf("the record written for a resumed v6 lease does not carry the DNS "+
			"server the binding was granted; a second plugin restart resumes an "+
			"endpoint with no resolver. Record file:\n%s", raw)
	}
	if !strings.Contains(string(raw), "lan.example") {
		t.Errorf("the record written for a resumed v6 lease does not carry the search "+
			"list the binding was granted. Record file:\n%s", raw)
	}
}
