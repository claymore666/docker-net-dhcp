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

// wedgeBudget bounds every wait in this file.
//
// Not a guess at how long the goroutine "should" take: nothing here
// sleeps, does I/O beyond one append per event, or waits on a clock, so
// the honest expectation is microseconds and this is four orders of
// magnitude of slack for a loaded CI shard. It exists so that the
// failure it is looking for arrives as a FAILED TEST rather than as a
// hung one — a hang is a third verdict, and a mutant that hangs is
// scored REFUSED rather than KILLED.
const wedgeBudget = 5 * time.Second

// TestTranslate_AnEventInFlightAtStopDoesNotWedgeTheGoroutine is X-34.
//
// THE DEFECT. `c.events` was unbuffered and `translate` sent into it
// with a bare `c.events <- out`. Its only reader is the per-family
// goroutine in pkg/plugin/dhcp_manager.go, whose other arm returns on
// `stopChan` and never reads the channel again. Any emitting event
// delivered in that window parked `translate` forever, and everything
// the goroutine still owed was owed forever with it: the range never
// advanced, so every LATER event was lost from the durable record;
// `defer close(c.events)` never ran, so the reader's own "stream
// closed" arm never fired; `defer c.opts.count(...)` never ran, and it
// is the ONLY writer of this manager's wire counters — P-7's
// per-endpoint half — so a TICKED parity row produced nothing for that
// endpoint; and the goroutine plus its client leaked for the life of
// the daemon. Nothing announced any of it.
//
// WHY THIS TEST DRIVES THE GOROUTINE. Every existing test in this
// package drives `translateOne`, which is pure and which no reader can
// wedge. The defect is not in the translation, it is in the delivery,
// and the delivery is only reachable by starting `translate` and taking
// its reader away. That is what X-3 stopped one function short of.
//
// THE THREE WINDOWS. The reader may already be gone when the first
// event lands (plugin Close over a live endpoint, or the legacy
// dual-stack path where the v6 client refuses and closes `stopChan`
// under a live v4 client), it may leave after one event (a Leave with a
// renewal in flight), or it may leave mid-burst. All three are the same
// bug and all three are driven, because a fix keyed on "the channel was
// empty at stop" would pass the first and fail the third.
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
			// Enough events that the buffer cannot absorb them all:
			// with a working fix the surplus is DROPPED, and with the
			// bare send it is the surplus that wedges. A burst equal to
			// the buffer would pass either way.
			const surplus = 8
			sends := tc.read + eventBuffer + surplus

			c, src, path := newTranslateHarness(t)
			go c.translate()

			// The plugin side: reads exactly tc.read events and then
			// returns, exactly as the stopChan arm does. It never reads
			// again and it never signals the chassis.
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
			// The library delivering out of its own buffer. A send that
			// blocks IS the wedge: translate stops consuming its source
			// the moment it parks on the emit. The first tc.read go out
			// while the reader is still there; the reader then leaves,
			// and the rest arrive with nobody on the other end — which
			// is the window the defect lives in.
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

			// The library's stream ending is what must make the
			// goroutine finish. Under the defect it never gets read.
			close(src)

			// The goroutine exits and the channel closes. Draining is
			// how a closed channel is distinguished from an idle one:
			// a range ends only on close.
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

			// The loss is accounted for, and this is the assertion that
			// separates the fix from the base's SILENT drop. Without
			// it, a drop and a wedge both show "no event" from outside
			// and the test above would pass against a guard that says
			// nothing about what it threw away.
			dropped := c.DroppedEvents()
			if dropped == 0 {
				t.Errorf("%d events were pushed at a reader that had stopped, %d were "+
					"delivered, and DroppedEvents() reports 0. Either nothing was dropped "+
					"— in which case this test is not reaching the guard at all — or the "+
					"drop is silent, which is the half of the base's guard this fix exists "+
					"to add.", sends, delivered)
			}
			// Conservation, and it is the assertion that makes the two
			// above more than "something happened": every event the
			// library handed over is either delivered to the plugin —
			// before the reader left, or out of the buffer afterwards —
			// or counted as dropped. An event that is neither is one
			// that vanished without anything saying so, which is the
			// class of defect this whole row is about.
			if got := uint64(tc.read) + uint64(delivered) + dropped; got != uint64(sends) {
				t.Errorf("%d events in; %d read before the reader left + %d drained "+
					"afterwards + %d dropped = %d out. Every event must be one of the "+
					"three.", sends, tc.read, delivered, dropped, got)
			}

			// The two things the wedge silently took away, asserted on
			// the durable record rather than on this package's own
			// bookkeeping.
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

// newTranslateHarness builds a DHCPClient that can run translate with no
// socket: c.src stands in for the library's event stream and c.client
// stays nil, which Stats() already tolerates. The record store is real
// and on disk, so the assertions are against the file a restart reads
// and not against a fake this test also wrote.
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

// recordLine is the part of lease.RecordEvent this test reads. Decoded
// from the file rather than folded through Rebuilt, because the fold
// merges an OpStats into the record and this test needs to know the
// LINE was written.
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

// TestTranslate_AReaderThatKeepsReadingLosesNothing is the preservation
// control for the guard above, and it is not optional.
//
// The fix has two halves — a buffer, and a non-blocking send — and the
// test above only exercises what happens when the reader is GONE. A
// buffer of zero would satisfy every assertion there (nothing wedges,
// the drops are counted, the record is complete) while dropping almost
// every event of an ordinary lease's life: the guard would have stopped
// being an emergency exit and become the normal path. This drives the
// case the plugin actually spends its time in.
//
// Lockstep rather than a burst, deliberately. One event is handed to
// translate and then taken from it before the next is sent, so the
// buffer never holds more than one and NO scheduling order can produce
// a drop. A test that pushed a burst and hoped the reader kept up would
// be a race whose green means "the machine was fast today".
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

// TestTranslate_TheBufferHoldsWhatTheLibraryWasAskedToHold pins the
// other half of the fix, and it exists because a mutant proved the two
// tests above could not see it.
//
// MEASURED: with the channel returned to unbuffered and the
// non-blocking guard left in place, both tests above still passed. That
// shape does not wedge — so the wedge test is satisfied — and in
// lockstep the plugin-side receiver is parked on the channel before
// translate reaches its send, because translate does a clock read, a
// record append and the translation in between; so the preservation
// control was satisfied too. Neither test was wrong; the property they
// hold simply is not this one.
//
// The property IS this: newLibClient asks the library for an
// EventBuffer of eventBuffer, so the library is entitled to hand over
// that many events before anyone reads one. A chassis that cannot hold
// what it asked to be given turns its emergency exit into the ordinary
// path — every event of a burst dropped, the ledger and the counters
// silently short, and the durable record the only place the truth
// survives. Driven with EXACTLY the buffer's depth and no reader at
// all, so no scheduling order can make it pass or fail by luck.
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
