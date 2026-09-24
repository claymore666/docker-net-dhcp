// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const renumberNetnsChildEnv = "DND_RENUMBER_NETNS_CHILD"

// inOwnNetns re-runs the calling test in a fresh user and network namespace and returns false in the parent; the
// child returns true and runs the body. uid 0 is mapped because capabilities are recomputed at execve (#1081).
func inOwnNetns(t *testing.T) bool {
	t.Helper()
	if os.Getenv(renumberNetnsChildEnv) == "1" {
		return true
	}
	name := t.Name()
	cmd := exec.Command(os.Args[0], "-test.run", "^"+name+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), renumberNetnsChildEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if os.Getuid() != 0 {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	text := out.String()
	switch {
	case runErr != nil && (errors.Is(runErr, unix.EPERM) || errors.Is(runErr, unix.ENOSPC) || errors.Is(runErr, unix.EACCES)):
		t.Skipf("no unprivileged user namespace on this host: %v", runErr)
	case strings.Contains(text, "--- SKIP: "+name+" "):
		t.Skipf("the namespaced run skipped:\n%s", text)
	case runErr != nil || !strings.Contains(text, "--- PASS: "+name+" "):
		t.Fatalf("the namespaced run of %s did not report its own PASS (%v):\n%s", name, runErr, text)
	}
	return false
}

type renumberLink struct {
	h    *netlink.Handle
	link netlink.Link
}

// newRenumberLink brings up a dummy link holding addr with a default route via gw, the shape Docker leaves after Join.
func newRenumberLink(t *testing.T, promote int, addr, gw string) renumberLink {
	t.Helper()
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("netlink handle: %v", err)
	}
	t.Cleanup(h.Close)
	name := "rn0"
	if old, err := h.LinkByName(name); err == nil {
		if err := h.LinkDel(old); err != nil {
			t.Fatalf("LinkDel: %v", err)
		}
	}
	if err := h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("no CAP_NET_ADMIN in the user namespace (an AppArmor userns restriction?): %v", err)
		}
		t.Fatalf("LinkAdd: %v", err)
	}
	link, err := h.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}
	// The kernel applies conf.all OR the link's value, so both are written and read back.
	for _, conf := range []struct {
		dev string
		val int
	}{{"all", 0}, {name, promote}} {
		sysctl := filepath.Join("/proc/sys/net/ipv4/conf", conf.dev, "promote_secondaries")
		if err := os.WriteFile(sysctl, []byte(fmt.Sprint(conf.val)), 0o644); err != nil {
			t.Fatalf("write %s: %v", sysctl, err)
		}
		if got, err := os.ReadFile(sysctl); err != nil || strings.TrimSpace(string(got)) != fmt.Sprint(conf.val) {
			t.Fatalf("%s reads %q (%v), want %d", sysctl, got, err, conf.val)
		}
	}
	if err := h.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp: %v", err)
	}
	if addr != "" {
		a, _ := netlink.ParseAddr(addr)
		if err := h.AddrAdd(link, a); err != nil {
			t.Fatalf("AddrAdd %s: %v", addr, err)
		}
		if err := h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gw)}); err != nil {
			t.Fatalf("default route via %s: %v", gw, err)
		}
		_, dst, _ := net.ParseCIDR("172.30.0.0/16")
		static := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Gw: net.ParseIP("192.168.99.254")}
		if err := h.RouteAdd(static); err != nil {
			t.Fatalf("static route: %v", err)
		}
		policy := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gw), Table: renumberPolicyTable}
		if err := h.RouteAdd(policy); err != nil {
			t.Fatalf("table %d default route: %v", renumberPolicyTable, err)
		}
	}
	return renumberLink{h: h, link: link}
}

func (l renumberLink) manager(t *testing.T, last string) *dhcpManager {
	t.Helper()
	a, err := netlink.ParseAddr(last)
	if err != nil {
		t.Fatalf("ParseAddr %s: %v", last, err)
	}
	m := &dhcpManager{netHandle: l.h, ctrLink: l.link}
	m.setLastIP(strings.Contains(last, ":"), a)
	return m
}

func (l renumberLink) addrs(t *testing.T, family int) []string {
	t.Helper()
	list, err := util.DumpResult(l.h.AddrList(l.link, family))
	if err != nil {
		t.Fatalf("AddrList: %v", err)
	}
	var out []string
	for _, a := range list {
		if a.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, a.IPNet.String())
	}
	sort.Strings(out)
	return out
}

// routes lists the link's main-table v4 routes as "default via G", "P via G" or a connected prefix P.
func (l renumberLink) routes(t *testing.T) []string {
	t.Helper()
	return l.tableRoutes(t, unix.RT_TABLE_MAIN)
}

// renumberPolicyTable holds a default route the way policy routing leaves one outside the main table.
const renumberPolicyTable = 100

// tableRoutes lists the link's v4 routes in one table, written as routes does.
func (l renumberLink) tableRoutes(t *testing.T, table int) []string {
	t.Helper()
	list, err := util.DumpResult(l.h.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{LinkIndex: l.link.Attrs().Index, Table: table}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE))
	if err != nil {
		t.Fatalf("RouteListFiltered table %d: %v", table, err)
	}
	var out []string
	for _, r := range list {
		if isDefaultRoute(r) {
			out = append(out, "default via "+r.Gw.String())
		} else if r.Gw != nil {
			out = append(out, r.Dst.String()+" via "+r.Gw.String())
		} else {
			out = append(out, r.Dst.String())
		}
	}
	sort.Strings(out)
	return out
}

// assertPolicyRouteKept fails unless the table-100 default route newRenumberLink installed is still there.
func (l renumberLink) assertPolicyRouteKept(t *testing.T) {
	t.Helper()
	if got := l.tableRoutes(t, renumberPolicyTable); !equalStrings(got, []string{"default via 192.168.99.1"}) {
		t.Errorf("table %d routes = %v, want [default via 192.168.99.1]", renumberPolicyTable, got)
	}
}

// countAddrWrites counts address writes through the seam for the rest of the test.
func (l renumberLink) countAddrWrites(t *testing.T) *int {
	t.Helper()
	prev := nlHandleAddrReplace
	t.Cleanup(func() { nlHandleAddrReplace = prev })
	n := new(int)
	nlHandleAddrReplace = func(h *netlink.Handle, link netlink.Link, a *netlink.Addr) error {
		*n++
		return prev(h, link, a)
	}
	return n
}

// assertNothingNames fails when a route in any table still names ip as destination, source or gateway (#128).
func (l renumberLink) assertNothingNames(t *testing.T, ip string) {
	t.Helper()
	list, err := util.DumpResult(l.h.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_UNSPEC},
		netlink.RT_FILTER_TABLE))
	if err != nil {
		t.Fatalf("RouteListFiltered: %v", err)
	}
	for _, r := range list {
		if strings.Contains(fmt.Sprint(r.Dst, " ", r.Src, " ", r.Gw), ip) {
			t.Errorf("route %s still names %s", r, ip)
		}
	}
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// The static route stands in for an option 121 route Docker installed at Join; renew never re-installs it.
const renumberStaticRoute = "172.30.0.0/16 via 192.168.99.254"

func TestRenew_ARenumberLeavesExactlyTheNewAddressAndRoutes(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	const old, same, other = "192.168.99.61/24", "192.168.99.10/24", "192.168.100.10/24"
	kept := []string{renumberStaticRoute, "192.168.99.0/24", "default via 192.168.99.1"}
	for _, tc := range []struct {
		name          string
		promote       int
		newIP, offer  string
		pinned, extra string
		wantRoutes    []string
		wantWrites    int
	}{
		{name: "same subnet, promote_secondaries=0", promote: 0, newIP: same, offer: "192.168.99.1", wantRoutes: kept, wantWrites: 2},
		{name: "same subnet, promote_secondaries=1", promote: 1, newIP: same, offer: "192.168.99.1", wantRoutes: kept, wantWrites: 1},
		{name: "same subnet, pinned gateway, promote_secondaries=0", promote: 0, newIP: same, offer: "192.168.99.1", pinned: "192.168.99.1", wantRoutes: kept, wantWrites: 2},
		{name: "same subnet, pinned gateway, promote_secondaries=1", promote: 1, newIP: same, offer: "192.168.99.1", pinned: "192.168.99.1", wantRoutes: kept, wantWrites: 1},
		{name: "same subnet, no gateway option, promote_secondaries=0", promote: 0, newIP: same, wantRoutes: kept, wantWrites: 2},
		{name: "same subnet, no gateway option, promote_secondaries=1", promote: 1, newIP: same, wantRoutes: kept, wantWrites: 1},
		{name: "same subnet, a second address in another subnet, promote_secondaries=0", promote: 0, newIP: same, offer: "192.168.99.1", extra: "10.9.9.9/24", wantRoutes: append([]string{"10.9.9.0/24"}, kept...), wantWrites: 2},
		{name: "cross subnet (#128), promote_secondaries=0", promote: 0, newIP: other, offer: "192.168.100.1", wantRoutes: []string{renumberStaticRoute, "192.168.100.0/24", "default via 192.168.100.1"}, wantWrites: 1},
		{name: "cross subnet (#128), promote_secondaries=1", promote: 1, newIP: other, offer: "192.168.100.1", wantRoutes: []string{renumberStaticRoute, "192.168.100.0/24", "default via 192.168.100.1"}, wantWrites: 1},
		{name: "cross subnet, pinned gateway, promote_secondaries=0", promote: 0, newIP: other, offer: "192.168.100.1", pinned: "192.168.99.1", wantRoutes: []string{renumberStaticRoute, "192.168.100.0/24", "default via 192.168.99.1"}, wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newRenumberLink(t, tc.promote, old, "192.168.99.1")
			wantAddrs := []string{tc.newIP}
			if tc.extra != "" {
				a, _ := netlink.ParseAddr(tc.extra)
				if err := l.h.AddrAdd(l.link, a); err != nil {
					t.Fatalf("AddrAdd %s: %v", tc.extra, err)
				}
				wantAddrs = append(wantAddrs, tc.extra)
				sort.Strings(wantAddrs)
			}
			m := l.manager(t, old)
			m.opts.Gateway = tc.pinned
			writes := l.countAddrWrites(t)
			if err := m.renew(false, dhcp.Info{IP: tc.newIP, Gateway: tc.offer}); err != nil {
				t.Fatalf("renew: %v", err)
			}
			if got := l.addrs(t, netlink.FAMILY_V4); !equalStrings(got, wantAddrs) {
				t.Errorf("link addresses = %v, want %v", got, wantAddrs)
			}
			want := append([]string(nil), tc.wantRoutes...)
			sort.Strings(want)
			if got := l.routes(t); !equalStrings(got, want) {
				t.Errorf("link routes = %v, want %v", got, want)
			}
			l.assertPolicyRouteKept(t)
			if *writes != tc.wantWrites {
				t.Errorf("address writes = %d, want %d", *writes, tc.wantWrites)
			}
			l.assertNothingNames(t, "192.168.99.61")
		})
	}
}

func TestRenew_ARenumberWithTheOldAddressAlreadyGoneStillBinds(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, promote := range []int{0, 1} {
		t.Run(fmt.Sprintf("promote_secondaries=%d", promote), func(t *testing.T) {
			l := newRenumberLink(t, promote, "", "")
			m := l.manager(t, "192.168.99.61/24")
			var err error
			logged := captureLog(t, func() {
				err = m.renew(false, dhcp.Info{IP: "192.168.99.10/24", Gateway: "192.168.99.1"})
			})
			if err != nil {
				t.Fatalf("renew: %v", err)
			}
			if !strings.Contains(logged, "Failed to remove stale address after lease change") {
				t.Errorf("no warning for the failed delete; log:\n%s", logged)
			}
			if got := l.addrs(t, netlink.FAMILY_V4); !equalStrings(got, []string{"192.168.99.10/24"}) {
				t.Errorf("link addresses = %v, want [192.168.99.10/24]", got)
			}
			if got := l.routes(t); !equalStrings(got, []string{"192.168.99.0/24", "default via 192.168.99.1"}) {
				t.Errorf("link routes = %v", got)
			}
		})
	}
}

func TestApplyAddressChange_ARefusedNewAddressKeepsTheOldOne(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, tc := range []struct {
		promote, refuse, writes int
	}{
		{promote: 1, refuse: 1, writes: 1},
		{promote: 0, refuse: 1, writes: 1},
		{promote: 0, refuse: 2, writes: 3},
	} {
		t.Run(fmt.Sprintf("promote_secondaries=%d, write %d refused", tc.promote, tc.refuse), func(t *testing.T) {
			l := newRenumberLink(t, tc.promote, "192.168.99.61/24", "192.168.99.1")
			m := l.manager(t, "192.168.99.61/24")
			prev := nlHandleAddrReplace
			t.Cleanup(func() { nlHandleAddrReplace = prev })
			calls := 0
			nlHandleAddrReplace = func(h *netlink.Handle, link netlink.Link, a *netlink.Addr) error {
				calls++
				if calls == tc.refuse {
					return unix.EINVAL
				}
				return prev(h, link, a)
			}
			newIP, _ := netlink.ParseAddr("192.168.99.10/24")
			if err := m.applyAddressChange(false, newIP, dhcp.Info{}); !errors.Is(err, unix.EINVAL) {
				t.Fatalf("applyAddressChange = %v, want the refusal", err)
			}
			if calls != tc.writes {
				t.Errorf("address writes = %d, want %d", calls, tc.writes)
			}
			if got := l.addrs(t, netlink.FAMILY_V4); !equalStrings(got, []string{"192.168.99.61/24"}) {
				t.Errorf("link addresses = %v, want [192.168.99.61/24]", got)
			}
			want := []string{renumberStaticRoute, "192.168.99.0/24", "default via 192.168.99.1"}
			if got := l.routes(t); !equalStrings(got, want) {
				t.Errorf("link routes = %v, want %v", got, want)
			}
			l.assertPolicyRouteKept(t)
		})
	}
}

func TestApplyAddressChange_AV6RenumberLeavesOnlyTheNewAddress(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, promote := range []int{0, 1} {
		t.Run(fmt.Sprintf("promote_secondaries=%d", promote), func(t *testing.T) {
			l := newRenumberLink(t, promote, "", "")
			m := &dhcpManager{netHandle: l.h, ctrLink: l.link}
			for _, ip := range []string{"fd00:6470::61/64", "fd00:6470::10/64"} {
				a, _ := netlink.ParseAddr(ip)
				v6AddrAttrs(a, 0, 0, false)
				if err := m.applyAddressChange(true, a, dhcp.Info{IP: ip}); err != nil {
					t.Fatalf("applyAddressChange %s: %v", ip, err)
				}
				m.setLastIP(true, a)
			}
			if got := l.addrs(t, netlink.FAMILY_V6); !equalStrings(got, []string{"fd00:6470::10/64"}) {
				t.Errorf("link v6 addresses = %v, want [fd00:6470::10/64]", got)
			}
		})
	}
}
