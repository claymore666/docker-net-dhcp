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

// The library copies Params6 when a client is built, so nothing exported clears a running client's hint; since
// dhcp-golib v0.1.0 Machine6.solicitHint hints no declined address, which ended the once-a-second Decline loop (#911).

// The hint keeps an endpoint's address across a restart (#213).

func TestPersistentV6Client_IsBuiltWithTheHintThatFeedsTheLoop(t *testing.T) {
	const preferred = "fd00:6470:6865::61"

	// NewDHCPClient opens nothing, so this process's namespace satisfies the guard (#911).
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
		t.Fatalf("the persistent v6 client's hint is %v, want %v -- if this changed, the "+
			"hint was dropped, and an endpoint stops keeping its address across a "+
			"restart (#213)", got, want)
	}
}

// The chassis drops every lease.Failed{ReasonConflict} on the persistent path, with no counter, log or deadline (#911).

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

	select {
	case ev, ok := <-c.events:
		if ok {
			t.Fatalf("translate emitted %+v after a conflict. A conflict is not a lease "+
				"failure and nothing downstream is counting them; if that changed, this "+
				"assertion is what says so", ev)
		}
		t.Fatal("the event stream closed under a burst of conflicts. That would be a bound " +
			"-- read the file header before restoring anything")
	default:
	}

	close(src)
	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("translate did not finish within %v after its source closed", wedgeBudget)
	}
}
