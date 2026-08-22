// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"strings"
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
