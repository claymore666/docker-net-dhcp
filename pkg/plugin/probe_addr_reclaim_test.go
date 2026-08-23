// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func addrWith(cidr, label string) netlink.Addr {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	ipnet.IP = ip
	return netlink.Addr{IPNet: ipnet, Label: label}
}

// TestReclaimStaleProbeAddrs_OnlyTakesWhatItCanProveIsOurs is the #723
// regression, and every case that is NOT reclaimed matters more than
// the one that is.
//
// The address being reclaimed sits on the OPERATOR's NIC, an interface
// the plugin does not own. The leak it fixes is slow — one stray /32
// per stop-inside-the-probe-window — and the failure mode of an
// over-eager fix is immediate and is somebody else's connectivity. So
// the reclaim is defined by its refusals.
func TestReclaimStaleProbeAddrs_OnlyTakesWhatItCanProveIsOurs(t *testing.T) {
	const parent = "parent0"
	label := probeAddrLabel(parent)
	if label != "parent0:dh" {
		t.Fatalf("probeAddrLabel(%q) = %q; the rest of this test is written against the real label", parent, label)
	}

	present := []netlink.Addr{
		addrWith("169.254.7.9/32", label),       // ours, left behind
		addrWith("169.254.8.8/32", label),       // ours, LIVE right now
		addrWith("169.254.9.9/32", ""),          // link-local, but not ours
		addrWith("169.254.10.10/32", "parent0"), // the plain interface label, not ours
		addrWith("10.1.2.3/32", label),          // labelled, but outside 169.254/16
		addrWith("169.254.11.0/24", label),      // labelled and in range, but not a /32
	}

	var deleted []string
	restoreList, restoreDel := nlAddrList, nlAddrDel
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) { return present, nil }
	nlAddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, a.IPNet.String())
		return nil
	}
	t.Cleanup(func() { nlAddrList, nlAddrDel = restoreList, restoreDel })

	p := &Plugin{}
	p.holdProbeAddr("169.254.8.8/32")

	p.reclaimStaleProbeAddrs(&fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: parent}})

	want := []string{"169.254.7.9/32"}
	if len(deleted) != len(want) || (len(deleted) > 0 && deleted[0] != want[0]) {
		t.Errorf("deleted %v, want %v", deleted, want)
		for _, d := range deleted {
			switch d {
			case "169.254.8.8/32":
				t.Error("  it deleted a LIVE probe's source address. That is #575 exactly — the failure these " +
					"leftovers come from — reintroduced by the code meant to clean them up")
			case "169.254.9.9/32", "169.254.10.10/32":
				t.Error("  it deleted a link-local the plugin never wrote. The label is the only thing that " +
					"makes reclaim safe on an interface the operator owns")
			case "10.1.2.3/32":
				t.Error("  it deleted an address outside 169.254.0.0/16; a label is a string an operator can type")
			case "169.254.11.0/24":
				t.Error("  it deleted a prefix, not a host address; a borrowed probe source is always a /32 (#575)")
			}
		}
	}

	if got := p.conflictProbeStaleAddrs.Load(); got != 1 {
		t.Errorf("conflictProbeStaleAddrs: got %d, want 1 — the whole finding in #723 was that this repair "+
			"happens silently and no counter moves", got)
	}
}

// TestReclaimStaleProbeAddrs_DeclinesWhenItCannotMark pins the blind
// spot rather than leaving it to be discovered.
//
// The kernel refuses an IPv4 address label longer than IFNAMSIZ-1, so a
// parent whose name leaves no room for the ":dh" suffix gets an
// unmarked borrowed source. Reclaim must then do NOTHING — not "best
// effort", not "delete link-local /32s and hope". Verified against the
// kernel while writing this: a label built on a 15-character interface
// name is rejected with "Attribute failed policy validation".
func TestReclaimStaleProbeAddrs_DeclinesWhenItCannotMark(t *testing.T) {
	longParent := strings.Repeat("p", unix.IFNAMSIZ-2) // 14 chars: ":dh" cannot fit
	if l := probeAddrLabel(longParent); l != "" {
		t.Fatalf("probeAddrLabel(%q) = %q, want \"\" — %d chars exceeds the kernel's limit of %d",
			longParent, l, len(l), unix.IFNAMSIZ-1)
	}

	listed := false
	restoreList, restoreDel := nlAddrList, nlAddrDel
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) {
		listed = true
		return []netlink.Addr{addrWith("169.254.7.9/32", "")}, nil
	}
	nlAddrDel = func(netlink.Link, *netlink.Addr) error {
		t.Error("reclaim deleted an address on a parent it could not label; nothing on that NIC is provably ours")
		return nil
	}
	t.Cleanup(func() { nlAddrList, nlAddrDel = restoreList, restoreDel })

	p := &Plugin{}
	p.reclaimStaleProbeAddrs(&fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: longParent}})

	if listed {
		t.Error("reclaim listed addresses on a parent it cannot label; there is nothing it could do with them")
	}
	if got := p.conflictProbeStaleAddrs.Load(); got != 0 {
		t.Errorf("conflictProbeStaleAddrs: got %d, want 0", got)
	}
}

// TestReclaimStaleProbeAddrs_ADeleteFailureDoesNotCount keeps the
// counter honest. It reports what was RECLAIMED, so an EADDRNOTAVAIL
// (another process removed it first) must not read as a repair this
// plugin performed.
func TestReclaimStaleProbeAddrs_ADeleteFailureDoesNotCount(t *testing.T) {
	restoreList, restoreDel := nlAddrList, nlAddrDel
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) {
		return []netlink.Addr{addrWith("169.254.7.9/32", "parent0:dh")}, nil
	}
	nlAddrDel = func(netlink.Link, *netlink.Addr) error { return unix.EADDRNOTAVAIL }
	t.Cleanup(func() { nlAddrList, nlAddrDel = restoreList, restoreDel })

	p := &Plugin{}
	p.reclaimStaleProbeAddrs(&fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: "parent0"}})

	if got := p.conflictProbeStaleAddrs.Load(); got != 0 {
		t.Errorf("conflictProbeStaleAddrs: got %d, want 0 — the address is still there, so nothing was reclaimed", got)
	}
}

// TestReclaimStaleProbeAddrs_AListFailureIsNotFatal: reclaim is
// housekeeping for a PREVIOUS run. A probe that cannot look must still
// run, or a transient netlink error turns the repair into an outage.
func TestReclaimStaleProbeAddrs_AListFailureIsNotFatal(t *testing.T) {
	restoreList := nlAddrList
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) { return nil, errors.New("netlink busy") }
	t.Cleanup(func() { nlAddrList = restoreList })

	p := &Plugin{}
	p.reclaimStaleProbeAddrs(&fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: "parent0"}})

	if got := p.conflictProbeStaleAddrs.Load(); got != 0 {
		t.Errorf("conflictProbeStaleAddrs: got %d, want 0", got)
	}
}

// TestPickProbeSource_BorrowedAddressIsLabelled closes the loop: an
// address written without the label can never be reclaimed, so the
// marker has to be applied at the point of borrowing and not only
// looked for at the point of cleaning.
func TestPickProbeSource_BorrowedAddressIsLabelled(t *testing.T) {
	link := &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: "parent0"}}

	src, err := pickProbeSource(link, net.IPv4(192, 168, 0, 50), nil)
	if err != nil {
		t.Fatalf("pickProbeSource: %v", err)
	}
	if !src.borrowed {
		t.Fatal("want a borrowed source when the parent has no on-subnet address")
	}
	if src.addr.Label != "parent0:dh" {
		t.Errorf("borrowed source label = %q, want %q — an unlabelled leftover is unreclaimable forever, "+
			"which is the whole of #723", src.addr.Label, "parent0:dh")
	}
	if !linkLocalV4.Contains(src.addr.IP) {
		t.Errorf("borrowed source %v is outside 169.254.0.0/16, so reclaim will never match it", src.addr.IP)
	}
}

// TestProbeAddressConflict_ReclaimsBeforeItBorrows is the test the rest
// of this file did not contain, and its absence was found by mutation
// rather than by reading: deleting the reclaim call from
// probeAddressConflict left every other case here green.
//
// That is the difference between a function being tested and a fix
// being covered. reclaimStaleProbeAddrs is exercised five ways above
// and none of them asks whether anything calls it — so a refactor that
// dropped the call would take #723 with it and this file would still
// pass.
//
// The probe is driven to its FIRST failure after the reclaim (the
// borrow, which nlAddrAdd refuses here) rather than to completion:
// everything past that point wants CAP_NET_ADMIN and a live parent, and
// the claim under test is only that the reclaim happens before the
// probe borrows anything. It has to be before — a leftover from a
// previous run is on the parent whether or not this probe borrows, and
// the probe is the only occasion the plugin has to look.
func TestProbeAddressConflict_ReclaimsBeforeItBorrows(t *testing.T) {
	const parent = "parent0"
	stale := addrWith("169.254.7.9/32", probeAddrLabel(parent))

	link := &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: parent}}
	stubLinkByName(t, func(name string) (netlink.Link, error) {
		if name != parent {
			t.Errorf("probe looked up %q, want %q", name, parent)
		}
		return link, nil
	})

	var deleted []string
	borrowAttempted := false
	restoreList, restoreDel, restoreAdd := nlAddrList, nlAddrDel, nlAddrAdd
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) { return []netlink.Addr{stale}, nil }
	nlAddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, a.IPNet.String())
		return nil
	}
	nlAddrAdd = func(netlink.Link, *netlink.Addr) error {
		borrowAttempted = true
		return errors.New("refused by the test, to stop the probe here")
	}
	t.Cleanup(func() { nlAddrList, nlAddrDel, nlAddrAdd = restoreList, restoreDel, restoreAdd })

	p := &Plugin{}
	_, err := p.probeAddressConflict(
		context.Background(), parent,
		net.IPv4(192, 168, 0, 50),
		&net.IPNet{IP: net.IPv4(192, 168, 0, 0), Mask: net.CIDRMask(24, 32)},
		net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
	)
	if err == nil {
		t.Fatal("want the probe to stop at the refused borrow; it got further than this test can account for")
	}

	if !borrowAttempted {
		t.Fatal("the probe never reached the borrow, so this test proves nothing about ordering")
	}
	if len(deleted) != 1 || deleted[0] != "169.254.7.9/32" {
		t.Errorf("deleted %v, want [169.254.7.9/32]: a stale probe source left on the parent by an earlier "+
			"run survived a later probe, which is #723 unfixed however well reclaimStaleProbeAddrs itself works",
			deleted)
	}
	if got := p.conflictProbeStaleAddrs.Load(); got != 1 {
		t.Errorf("conflictProbeStaleAddrs: got %d, want 1", got)
	}
}

// TestProbeAddressConflict_ABorrowedSourceIsNeverReclaimedWhileLive is
// the one case the rest of this file could not reach: not whether
// reclaim skips a held address -- five cases above already ask that --
// but whether the address is HELD at every instant it is visible.
//
// The in-use set only protects the address while the hold covers the
// window in which reclaim can see it, and that window is the address
// being on the link. Two orderings put it outside:
//
//   - AddrAdd, then hold. The kernel has the address for the length of
//     a netlink round trip while nothing holds it.
//   - release, then AddrDel. The hold is gone while the address is
//     still on the link.
//
// In either one a concurrent probe's reclaim finds all four conditions
// satisfied -- our label, inside 169.254/16, a /32, not in use -- and
// deletes a LIVE source, counting it as a leftover that was never
// stale. The victim then keeps probing from an address no longer on the
// link, and by #524 an ARP probe whose sender is not routable is
// answered by nobody: the probe reports "no conflict" without having
// asked, and the plugin hands out an address someone else is using. A
// false negative in the duplicate-address check is a worse outcome than
// the leak reclaim exists to fix.
//
// The interleaving is injected rather than raced. The reclaim is driven
// synchronously from inside the netlink call that opens each window, so
// the test pins the exact instant it means instead of hoping two
// goroutines land on it; a racing version of this test would be red
// sometimes and green in CI. What a second probe would do at that
// instant is precisely what reclaimStaleProbeAddrs does, so calling it
// there IS the concurrent probe.
//
// Only reclaim's deletions are recorded. The probe's own cleanup
// deletes the same address on its way out, and counting that would make
// the assertion true for the wrong reason.
func TestProbeAddressConflict_ABorrowedSourceIsNeverReclaimedWhileLive(t *testing.T) {
	cases := []struct {
		name   string
		window string
	}{
		{
			name:   "while the kernel holds it and the borrow has not returned",
			window: "add",
		},
		{
			name:   "while the cleanup is removing it and it is still on the link",
			window: "del",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const parent = "parent0"
			link := &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: parent, Index: 0}}
			stubLinkByName(t, func(string) (netlink.Link, error) { return link, nil })

			p := &Plugin{}

			// onLink models the kernel: what a concurrent probe's
			// AddrList would return at this instant. Empty to begin
			// with, so pickProbeSource finds nothing on-subnet and
			// borrows a link-local source.
			var mu sync.Mutex
			var onLink []netlink.Addr
			var borrowed string
			var reclaiming bool
			var reclaimDeleted []string

			restoreList, restoreAdd, restoreDel := nlAddrList, nlAddrAdd, nlAddrDel
			t.Cleanup(func() { nlAddrList, nlAddrAdd, nlAddrDel = restoreList, restoreAdd, restoreDel })

			nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) {
				mu.Lock()
				defer mu.Unlock()
				return append([]netlink.Addr(nil), onLink...), nil
			}
			nlAddrDel = func(l netlink.Link, a *netlink.Addr) error {
				mu.Lock()
				cidr := a.IPNet.String()
				if reclaiming {
					reclaimDeleted = append(reclaimDeleted, cidr)
				}
				kept := onLink[:0]
				for _, existing := range onLink {
					if existing.IPNet.String() != cidr {
						kept = append(kept, existing)
					}
				}
				onLink = append([]netlink.Addr(nil), kept...)
				wantWindow := tc.window == "del" && !reclaiming
				mu.Unlock()

				// The del window: the address is still on the link as
				// far as any other probe can tell, and the buggy
				// ordering has already released the hold.
				if wantWindow {
					mu.Lock()
					onLink = append(onLink, *a)
					reclaiming = true
					mu.Unlock()
					p.reclaimStaleProbeAddrs(l)
					mu.Lock()
					reclaiming = false
					for i, existing := range onLink {
						if existing.IPNet.String() == cidr {
							onLink = append(onLink[:i], onLink[i+1:]...)
							break
						}
					}
					mu.Unlock()
				}
				return nil
			}
			nlAddrAdd = func(l netlink.Link, a *netlink.Addr) error {
				mu.Lock()
				borrowed = a.IPNet.String()
				onLink = append(onLink, *a)
				reclaiming = tc.window == "add"
				run := reclaiming
				mu.Unlock()

				// The add window: the kernel holds the address and the
				// borrow has not returned to its caller yet.
				if run {
					p.reclaimStaleProbeAddrs(l)
					mu.Lock()
					reclaiming = false
					mu.Unlock()
				}
				return nil
			}

			// The probe runs on past the borrow into addProbeRoute,
			// which needs a real link, so it fails there -- which is
			// what makes the deferred cleanup, and with it the del
			// window, run. The error is expected; what matters is that
			// the borrow happened and the cleanup ran.
			_, err := p.probeAddressConflict(
				context.Background(), parent,
				net.IPv4(192, 168, 0, 50),
				&net.IPNet{IP: net.IPv4(192, 168, 0, 0), Mask: net.CIDRMask(24, 32)},
				net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
			)
			if err == nil {
				t.Fatal("want the probe to stop at the route it cannot add; it got further than this test can account for")
			}

			mu.Lock()
			defer mu.Unlock()
			if borrowed == "" {
				t.Fatal("the probe never borrowed a source, so no window was opened and this test proves nothing")
			}
			if len(reclaimDeleted) != 0 {
				t.Errorf("a concurrent reclaim deleted the LIVE borrowed source %v in the %s window.\n"+
					"  The address is visible to another probe while nothing holds it, so all four reclaim\n"+
					"  conditions pass and it is removed mid-probe -- #575 reintroduced by the cleanup written\n"+
					"  for #575. The victim then probes from a source that is not on the link, whose ARP nobody\n"+
					"  answers (#524), and reports no conflict without having asked.\n"+
					"  Hold before the address can be seen and release after it cannot: hold, then AddrAdd;\n"+
					"  AddrDel, then release.",
					reclaimDeleted, tc.window)
			}
			if got := p.conflictProbeStaleAddrs.Load(); got != 0 {
				t.Errorf("conflictProbeStaleAddrs: got %d, want 0 -- a live source was counted as a leftover", got)
			}
			if len(onLink) != 0 {
				t.Errorf("the parent still carries %v after the probe returned; the borrow leaked", onLink)
			}
		})
	}
}

// TestProbeAddressConflict_AFailedBorrowLeavesNoHold covers the error
// path the ordering fix creates, which nothing else reaches: hold now
// happens BEFORE AddrAdd, so an AddrAdd that fails leaves a hold with
// no probe behind it.
//
// Dropping the release there is invisible in every other test and in
// production for the life of one process: the address is unreclaimable
// forever, because reclaim skips anything held and nothing will ever
// release it. That is the leak #723 exists to fix, moved out of the
// kernel and into a map -- worse than the original, since `ip addr del`
// cannot reach it.
//
// The assertion is not "the map is empty" but "a later reclaim can
// still take the address", which is the property operators have.
func TestProbeAddressConflict_AFailedBorrowLeavesNoHold(t *testing.T) {
	const parent = "parent0"
	link := &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Name: parent}}
	stubLinkByName(t, func(string) (netlink.Link, error) { return link, nil })

	p := &Plugin{}

	var attempted string
	var onLink []netlink.Addr
	var deleted []string

	restoreList, restoreAdd, restoreDel := nlAddrList, nlAddrAdd, nlAddrDel
	t.Cleanup(func() { nlAddrList, nlAddrAdd, nlAddrDel = restoreList, restoreAdd, restoreDel })

	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) {
		return append([]netlink.Addr(nil), onLink...), nil
	}
	nlAddrDel = func(_ netlink.Link, a *netlink.Addr) error {
		deleted = append(deleted, a.IPNet.String())
		return nil
	}
	nlAddrAdd = func(_ netlink.Link, a *netlink.Addr) error {
		attempted = a.IPNet.String()
		// The kernel took it and then failed -- the worst case for a
		// dropped release, because the address really is on the link.
		onLink = append(onLink, *a)
		return errors.New("refused by the test, after the address landed")
	}

	if _, err := p.probeAddressConflict(
		context.Background(), parent,
		net.IPv4(192, 168, 0, 50),
		&net.IPNet{IP: net.IPv4(192, 168, 0, 0), Mask: net.CIDRMask(24, 32)},
		net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
	); err == nil {
		t.Fatal("want the refused borrow to fail the probe")
	}
	if attempted == "" {
		t.Fatal("the probe never attempted a borrow, so this test proves nothing")
	}

	// A later probe on the same parent, in the same process.
	p.reclaimStaleProbeAddrs(link)

	if len(deleted) != 1 || deleted[0] != attempted {
		t.Errorf("a later reclaim deleted %v, want [%s].\n"+
			"  The failed borrow left its hold in place, so the address it put on the parent can never be\n"+
			"  reclaimed by this process -- and being in a map rather than on the link, `ip addr del` does\n"+
			"  not reach it either.\n"+
			"  Release the hold on the AddrAdd error path.",
			deleted, attempted)
	}
}
