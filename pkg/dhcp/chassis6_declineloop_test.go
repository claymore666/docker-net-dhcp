// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/vishvananda/netns"
)

// THIS FILE PINS A DEFECT AS A CASE. Every assertion below states what
// the shipped build DOES, and the shipped build is wrong; a fix makes
// these tests fail, which is the point of writing them. Named so that a
// reader who reaches a red one knows to delete it rather than to
// restore the behaviour it describes.
//
// THE DEFECT (#911 review round 1, finding 2). An address squatted
// AFTER the endpoint has joined makes the persistent DHCPv6 client
// decline once a second for as long as the endpoint lives. Three facts
// compose it, and each has its own test here:
//
//  1. the persistent client is built with the preferred address as
//     proto.Params6.Hint (the tombstone's address, or the one the
//     one-shot just acquired);
//  2. the library's machine takes a COPY of those params when the
//     client is constructed -- runtime/client6.go's `params := cfg.Params6`
//     handed to the manager as `&params`, read back as the unexported
//     proto.machine6.params -- so nothing exported can clear the hint on
//     a running client;
//  3. the chassis absorbs the resulting conflicts without a bound:
//     translate drops lease.Failed{ReasonConflict} and Start runs the
//     client on context.Background(), so there is no deadline and no
//     counter anywhere on the persistent path.
//
// The CreateEndpoint half of this is fixed, by retryWithoutHint6: there
// the chassis OWNS the loop, so clearing the hint between passes is a
// state change. On the persistent path the loop is inside Client6.Run,
// and the only chassis-side bound available is to stop the client and
// build another one -- a supervisor, on the concurrency path, not a
// bound. THE REMEDY BELONGS IN THE LIBRARY (do not hint an address this
// machine has just declined) and the row is handed to the M7 library
// round by name. docs/reference.md states the escape beside the claim.

// TestPersistentV6Client_IsBuiltWithTheHintThatFeedsTheLoop is fact 1.
//
// It is the CAUSE, and it is not a defect on its own: the hint is what
// makes an endpoint keep its address across a restart (#213), and
// dropping it would hand the container a different address than the one
// CreateEndpoint already told the engine about. It is here because a
// fix that removed the hint would be the wrong fix, and this says so.
func TestPersistentV6Client_IsBuiltWithTheHintThatFeedsTheLoop(t *testing.T) {
	const preferred = "fd00:6470:6865::61"

	// NewDHCPClient opens nothing -- the sockets are Start's, and Start
	// is not called here -- so this process's own namespace satisfies
	// the guard shape's requirement that a persistent v6 client name
	// one.
	ns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer func() { _ = ns.Close() }()

	c, err := NewDHCPClient("test0", &DHCPClientOptions{
		V6:                 true,
		PreferredV6:        preferred,
		HonorRouterAdverts: true,
		NetNS:              &ns,
		Identity6:          Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0, 2}, IAID: 0xac110002},
	})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	if got, want := c.params6.Hint, netip.MustParseAddr(preferred); got != want {
		t.Fatalf("the persistent v6 client's hint is %v, want %v -- if this changed, "+
			"read the file header: either the hint was dropped (which loses #213) or the "+
			"loop this file pins has been fixed and these tests should go", got, want)
	}
}

// TestPersistentV6Client_AbsorbsConflictsWithoutABound is fact 3, and
// it is the defect itself.
//
// A hundred conflicts is not a threshold; it is a number far past any
// bound a fix would plausibly choose, so a fix at two, at five or at
// sixteen all turn this red. The assertion is that the chassis emits
// NOTHING and stops nothing: every conflict is dropped by translate, on
// the reasoning that a conflict is not a lease failure, and no counter
// on this path is watching how many of them there have been.
//
// The outside evidence for the same shape is a DHCPDECLINE count in the
// server's log that grows past any bound; it is not asserted here
// because reproducing it needs a squatter on the segment AFTER Join,
// which is an integration arm the library fix will make unnecessary.
func TestPersistentV6Client_AbsorbsConflictsWithoutABound(t *testing.T) {
	const conflicts = 100

	c, src, _ := newTranslateHarness(t)
	go c.translate()

	for i := 0; i < conflicts; i++ {
		select {
		case src <- lease.Event{
			Kind:   lease.Failed,
			Reason: proto.ReasonConflict,
			Lease:  lease.Lease{Addr: netip.MustParsePrefix("192.168.99.7/24")},
		}:
		case <-time.After(wedgeBudget):
			t.Fatalf("conflict %d of %d could not be delivered within %v; translate has "+
				"parked, which is a different defect from the one this file pins",
				i+1, conflicts, wedgeBudget)
		}
	}

	// Nothing came out, so nothing downstream can be counting.
	select {
	case ev, ok := <-c.events:
		if ok {
			t.Fatalf("translate emitted %+v after a conflict. If the chassis has grown a "+
				"bound on the persistent decline loop, this file is stale: delete it and "+
				"take the row off the library round", ev)
		}
		t.Fatal("the event stream closed under a burst of conflicts. That would be a bound " +
			"-- read the file header before restoring anything")
	default:
	}

	// ...and the client is still running, which is the whole of the
	// defect: it will keep declining for as long as the endpoint lives.
	close(src)
	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("translate did not finish within %v after its source closed", wedgeBudget)
	}
}
