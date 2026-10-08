// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"sort"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// The addresses a kernel on kernel 6.12.111 held after one advertisement of fd00:98::/64 reached a link with this MAC at
// autoconf=1: `fd00:98::42:c0ff:fea8:6311/64 scope global dynamic mngtmpaddr proto kernel_ra` (#1268).
const (
	eui64TestMAC        = "02:42:c0:a8:63:11"
	eui64TestKernelAddr = "fd00:98::42:c0ff:fea8:6311/64"
	eui64TestSecondPfx  = "2001:db8:1::42:c0ff:fea8:6311/64"
	eui64TestLinkLocal  = "fe80::42:c0ff:fea8:6311/64"
	eui64TestStableAddr = "fd00:98::9c3e:51d0:7a2b:e4f1/64"
	eui64TestOtherHost  = "fd00:98::42:c0ff:fea8:6312/64"

	ipvlanTestKernelAddr = "fd00:98::242:c000:1a8:6311/64"
	ipvlanTestLinkLocal  = "fe80::242:c000:1a8:6311/64"
	ipvlanTestNoDevID    = "fd00:98::242:c000:a8:6311/64"
	ipvlanTestOtherTail  = "fd00:98::242:c000:1a8:6312/64"
	ipvlanTestOtherHead  = "fd00:98::42:c000:1a8:6311/64"
)

type eui64AddrTable struct {
	t        *testing.T
	k        *v6RenameKernel
	iface    string
	addrs    []string
	listErr  error
	delErr   error
	lists    int
	deletes  int
	autoconf []string
}

func newEUI64AddrTable(t *testing.T, k *v6RenameKernel, iface string, addrs ...string) *eui64AddrTable {
	t.Helper()
	tb := &eui64AddrTable{t: t, k: k, iface: iface}
	for _, s := range addrs {
		tb.addrs = append(tb.addrs, eui64Key(t, s))
	}
	prevList, prevDel := nlAddrList, nlAddrDelCurNS
	nlAddrList = func(link netlink.Link, family int) ([]netlink.Addr, error) {
		tb.observe(link)
		tb.lists++
		if family != unix.AF_INET6 {
			t.Errorf("the address list asked for family %d, want AF_INET6", family)
		}
		if tb.listErr != nil {
			return nil, tb.listErr
		}
		out := make([]netlink.Addr, 0, len(tb.addrs))
		for _, s := range tb.addrs {
			a, err := netlink.ParseAddr(s)
			if err != nil {
				t.Fatalf("parse %s: %v", s, err)
			}
			out = append(out, *a)
		}
		return out, nil
	}
	nlAddrDelCurNS = func(link netlink.Link, addr *netlink.Addr) error {
		tb.observe(link)
		tb.deletes++
		if tb.delErr != nil {
			return tb.delErr
		}
		key := addr.IPNet.String()
		for i, s := range tb.addrs {
			if s == key {
				tb.addrs = append(tb.addrs[:i], tb.addrs[i+1:]...)
				return nil
			}
		}
		return unix.EADDRNOTAVAIL
	}
	t.Cleanup(func() { nlAddrList, nlAddrDelCurNS = prevList, prevDel })
	return tb
}

func (tb *eui64AddrTable) observe(link netlink.Link) {
	tb.t.Helper()
	if !tb.k.inSandbox {
		tb.t.Error("the link's addresses were read or changed outside the sandbox namespace")
	}
	if got := link.Attrs().Index; got != renameTestIndex {
		tb.t.Errorf("netlink call on link index %d, want the container link %d", got, renameTestIndex)
	}
	tb.autoconf = append(tb.autoconf, v6LinkKnob(tb.t, tb.k.dir, tb.iface, "autoconf"))
}

func (tb *eui64AddrTable) held() []string {
	out := append([]string(nil), tb.addrs...)
	sort.Strings(out)
	return out
}

func eui64TestManager(t *testing.T, p *Plugin, iid, mac string) *dhcpManager {
	t.Helper()
	return eui64TestManagerIn(t, p, ModeBridge, iid, mac)
}

func eui64TestManagerIn(t *testing.T, p *Plugin, mode, iid, mac string) *dhcpManager {
	t.Helper()
	var hw net.HardwareAddr
	if mac != "" {
		var err error
		if hw, err = net.ParseMAC(mac); err != nil {
			t.Fatalf("parse MAC: %v", err)
		}
	}
	m := &dhcpManager{
		opts: DHCPNetworkOptions{Mode: mode, IPv6Mode: "slaac", IPv6IID: iid},
		ctrLink: &netlink.Device{LinkAttrs: netlink.LinkAttrs{
			Name: renameTestLocated, Index: renameTestIndex, HardwareAddr: hw,
		}},
	}
	return m.withPlugin(p)
}

// eui64Key spells an address as netlink's IPNet does, which keeps a lone zero group that the constants compress.
func eui64Key(t *testing.T, s string) string {
	t.Helper()
	a, err := netlink.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return a.IPNet.String()
}

func assertHeld(t *testing.T, tb *eui64AddrTable, wantAddrs ...string) {
	t.Helper()
	want := make([]string, 0, len(wantAddrs))
	for _, s := range wantAddrs {
		want = append(want, eui64Key(t, s))
	}
	sort.Strings(want)
	got := tb.held()
	if len(got) != len(want) {
		t.Fatalf("the link holds %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("the link holds %v, want %v", got, want)
		}
	}
}

func TestIPv6IID_StablePrivacyRemovesTheKernelFormedEUI64Address(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated,
		eui64TestKernelAddr, eui64TestSecondPfx, eui64TestLinkLocal, eui64TestStableAddr, eui64TestOtherHost)
	p := &Plugin{}

	eui64TestManager(t, p, "stable-privacy", eui64TestMAC).ensureIPv6Enabled()

	assertHeld(t, tb, eui64TestLinkLocal, eui64TestStableAddr, eui64TestOtherHost)
	assertV6Counters(t, p, 0, 0)
	k.assertPrepared(renameTestLocated)
	want := dhcp.RouterAdvertGuardContract()["autoconf"]
	if want != "0" {
		t.Fatalf("the guard contract holds autoconf at %q, so this test observes nothing", want)
	}
	if len(tb.autoconf) == 0 {
		t.Fatal("the link's addresses were never read")
	}
	for i, got := range tb.autoconf {
		if got != want {
			t.Errorf("netlink call %d ran at autoconf=%s: the next advertisement forms the address again", i+1, got)
		}
	}
}

func TestIPv6IID_EUI64NetworkKeepsTheAddressItFormsItself(t *testing.T) {
	for _, iid := range []string{"", "eui64"} {
		k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
		tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr, eui64TestLinkLocal)
		p := &Plugin{}

		eui64TestManager(t, p, iid, eui64TestMAC).ensureIPv6Enabled()

		assertHeld(t, tb, eui64TestKernelAddr, eui64TestLinkLocal)
		if tb.deletes != 0 {
			t.Errorf("ipv6_iid=%q: %d delete(s) on a network whose own address is the EUI-64 one", iid, tb.deletes)
		}
		assertV6Counters(t, p, 0, 0)
	}
}

func TestIPv6IID_FailedEUI64DeleteIsCountedAndKept(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr, eui64TestSecondPfx)
	tb.delErr = errors.New("operation not permitted")
	p := &Plugin{}

	_, guard, err := eui64TestManager(t, p, "stable-privacy", eui64TestMAC).prepareIPv6Link()
	if err != nil {
		t.Fatalf("prepareIPv6Link: %v", err)
	}
	if guard.Failures != 2 || guard.Err == nil {
		t.Errorf("guard: %d failure(s), %v; want one per address the delete refused, with a reason", guard.Failures, guard.Err)
	}
	assertHeld(t, tb, eui64TestKernelAddr, eui64TestSecondPfx)
}

func TestIPv6IID_EUI64AddressGoneBeforeTheDeleteIsNoFailure(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr)
	tb.delErr = unix.EADDRNOTAVAIL
	p := &Plugin{}

	eui64TestManager(t, p, "stable-privacy", eui64TestMAC).ensureIPv6Enabled()

	if tb.deletes != 1 {
		t.Errorf("%d delete(s), want the one EUI-64 address", tb.deletes)
	}
	assertV6Counters(t, p, 0, 0)
}

func TestIPv6IID_UnreadableAddressListIsCounted(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr)
	tb.listErr = errors.New("operation not permitted")
	p := &Plugin{}

	eui64TestManager(t, p, "stable-privacy", eui64TestMAC).ensureIPv6Enabled()

	assertV6Counters(t, p, 0, 1)
	assertHeld(t, tb, eui64TestKernelAddr)
}

func TestIPv6IID_LinkWithoutAMACHasNoEUI64ToRemove(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr)
	p := &Plugin{}

	eui64TestManager(t, p, "stable-privacy", "").ensureIPv6Enabled()

	assertHeld(t, tb, eui64TestKernelAddr)
	assertV6Counters(t, p, 0, 0)
}

func TestIPv6IID_NoRemovalWhereIPv6CouldNotBeEnabled(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr)
	k.failIndex = true
	k.rename(renameTestLocated, "gone")
	p := &Plugin{}

	eui64TestManager(t, p, "stable-privacy", eui64TestMAC).ensureIPv6Enabled()

	if tb.lists != 0 || tb.deletes != 0 {
		t.Errorf("%d list(s) and %d delete(s) on a link whose IPv6 could not be prepared", tb.lists, tb.deletes)
	}
	assertV6Counters(t, p, 1, 0)
}

func TestIPv6IID_UnlocatedLinkIndexRemovesNothing(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated, eui64TestKernelAddr)
	m := eui64TestManager(t, &Plugin{}, "stable-privacy", eui64TestMAC)
	m.ctrLink.Attrs().Index = 0

	m.ensureIPv6Enabled()

	if tb.lists != 0 || tb.deletes != 0 {
		t.Errorf("%d list(s) and %d delete(s) with no link index: a zero index filters nothing", tb.lists, tb.deletes)
	}
}

func TestIPv6IID_StablePrivacyIPvlanRemovesTheDevIDFormedAddress(t *testing.T) {
	k := newV6RenameKernel(t, map[string]int{renameTestLocated: renameTestIndex})
	tb := newEUI64AddrTable(t, k, renameTestLocated,
		ipvlanTestKernelAddr, ipvlanTestLinkLocal, ipvlanTestNoDevID, ipvlanTestOtherTail, ipvlanTestOtherHead,
		eui64TestStableAddr)
	p := &Plugin{}

	eui64TestManagerIn(t, p, ModeIPvlan, "stable-privacy", eui64TestMAC).ensureIPv6Enabled()

	assertHeld(t, tb, ipvlanTestLinkLocal, ipvlanTestNoDevID, ipvlanTestOtherTail, ipvlanTestOtherHead, eui64TestStableAddr)
	assertV6Counters(t, p, 0, 0)
}
