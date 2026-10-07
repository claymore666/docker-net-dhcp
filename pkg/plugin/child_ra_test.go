// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// childRAFixture holds par0 in the test namespace and its peer, the router side, in rtrNS.
type childRAFixture struct {
	h     *netlink.Handle
	par   netlink.Link
	rtrNS netns.NsHandle
	host  netns.NsHandle
}

func newChildRAFixture(t *testing.T) *childRAFixture {
	t.Helper()
	runtime.LockOSThread()
	host, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := netns.Set(host); err == nil {
			runtime.UnlockOSThread()
		}
		_ = host.Close()
	})
	rtrNS, err := netns.New()
	if err != nil {
		t.Fatalf("netns.New: %v", err)
	}
	t.Cleanup(func() { _ = rtrNS.Close() })
	if err := netns.Set(host); err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if err := h.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "par0"}, PeerName: "rtr0"}); err != nil {
		t.Fatalf("veth: %v", err)
	}
	writeSysctl(t, "/proc/sys/net/ipv6/conf/par0/disable_ipv6", "1")
	par, _ := h.LinkByName("par0")
	rtr, _ := h.LinkByName("rtr0")
	if err := h.LinkSetNsFd(rtr, int(rtrNS)); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(par); err != nil {
		t.Fatal(err)
	}
	return &childRAFixture{h: h, par: par, rtrNS: rtrNS, host: host}
}

func writeSysctl(t *testing.T, path, val string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(val), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// asBridgePort makes par0 a port of br0, so advertisements flood to the ports the plugin adds.
func (f *childRAFixture) asBridgePort(t *testing.T) {
	t.Helper()
	if err := f.h.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}); err != nil {
		t.Fatalf("bridge: %v", err)
	}
	br, _ := f.h.LinkByName("br0")
	writeSysctl(t, "/proc/sys/net/ipv6/conf/br0/disable_ipv6", "1")
	if err := f.h.LinkSetUp(br); err != nil {
		t.Fatal(err)
	}
	if err := f.h.LinkSetMaster(f.par, br); err != nil {
		t.Fatal(err)
	}
}

// defaultRouteVia returns the default route the namespace takes through name within 3 s of advertisements, or "".
func (f *childRAFixture) defaultRouteVia(t *testing.T, name string) string {
	t.Helper()
	link, err := f.h.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	sendErr := make(chan error, 1)
	go sendRAs(f.rtrNS, stop, sendErr)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-sendErr:
			t.Fatalf("RA sender: %v", err)
		default:
		}
		routes, _ := util.DumpResult(f.h.RouteList(nil, netlink.FAMILY_V6))
		for _, r := range routes {
			if r.LinkIndex == link.Attrs().Index && (r.Dst == nil || r.Dst.String() == "::/0") {
				return r.Gw.String()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

func (f *childRAFixture) addChild(t *testing.T, opts DHCPNetworkOptions, name string) netlink.Link {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.ParentIndex = f.par.Attrs().Index
	link, err := newChildLink(opts, la)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var p *Plugin
	guard := p.lockParent(ctx, "par0", ModeMacvlan, "create_endpoint")
	err = addChildLink(guard, link)
	guard.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := nlEndpointLinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func childIPv6Disabled(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6")
	if err != nil {
		t.Fatal(err)
	}
	return string(b[:len(b)-1])
}

// A child sits in the host namespace until Join and must take no address or default route from a router
// advertisement there; an IPv6 network keeps IPv6 on for its DHCPv6 exchange (#1247).
func childRACase(t *testing.T, opts DHCPNetworkOptions, wantRoute bool) {
	t.Helper()
	f := newChildRAFixture(t)
	name := subLinkName("fedcba9876543210")
	child := f.addChild(t, opts, name)
	got := recordIPv6Off(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := upChildLink(ctx, opts, child, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if !wantRoute {
		checkIPv6OffBeforeUp(t, *got, "dh-")
	}
	via := f.defaultRouteVia(t, name)
	addrs, _ := util.DumpResult(f.h.AddrList(child, netlink.FAMILY_V6))
	if !wantRoute && (via != "" || len(addrs) > 0) {
		t.Fatalf("host took a default route via %q and addresses %v on %s from a router advertisement before the move",
			via, addrs, name)
	}
	if wantRoute && via == "" {
		t.Fatalf("the child %s of an IPv6 network took no router advertisement: its IPv6 was turned off", name)
	}
}

func TestChildHostTakesNoRouterAdvertisement_Macvlan(t *testing.T) {
	if inOwnNetns(t) {
		childRACase(t, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "par0"}, false)
	}
}

func TestChildHostTakesNoRouterAdvertisement_Ipvlan(t *testing.T) {
	if inOwnNetns(t) {
		childRACase(t, DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "par0"}, false)
	}
}

func TestChildOfIPv6NetworkKeepsIPv6_Macvlan(t *testing.T) {
	if inOwnNetns(t) {
		childRACase(t, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "par0", IPv6: true}, true)
	}
}

func TestChildOfIPv6NetworkKeepsIPv6_Ipvlan(t *testing.T) {
	if inOwnNetns(t) {
		childRACase(t, DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "par0", IPv6: true}, true)
	}
}

// The same for the IPAM reservation and endpoint links; in bridge mode the client half of the veth pair is the one (#1247).
func ipamLinkCase(t *testing.T, opts DHCPNetworkOptions, link string, wantRoute bool,
	add func(p *Plugin, ctx context.Context, o DHCPNetworkOptions) (func(), error)) {
	t.Helper()
	f := newChildRAFixture(t)
	if opts.Mode == ModeBridge {
		f.asBridgePort(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var p *Plugin
	got := recordIPv6Off(t)
	remove, err := add(p, ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	if !wantRoute {
		checkIPv6OffBeforeUp(t, *got, link[:3])
	}
	via := f.defaultRouteVia(t, link)
	if !wantRoute && via != "" {
		t.Fatalf("host took a default route via %q on %s from a router advertisement", via, link)
	}
	if wantRoute && via == "" {
		t.Fatalf("%s of an IPv6 network took no router advertisement: its IPv6 was turned off", link)
	}
}

var (
	childRAMAC  = net.HardwareAddr{0x02, 0, 0, 0, 0x12, 0x34}
	childRAOpts = map[string]DHCPNetworkOptions{
		ModeMacvlan: {Mode: ModeMacvlan, Parent: "par0"},
		ModeIPvlan:  {Mode: ModeIPvlan, Parent: "par0"},
		ModeBridge:  {Mode: ModeBridge, Bridge: "br0"},
	}
)

func reserveAdd(mode, peer string) func(*Plugin, context.Context, DHCPNetworkOptions) (func(), error) {
	return func(p *Plugin, ctx context.Context, o DHCPNetworkOptions) (func(), error) {
		return p.addIPAMReserveLink(ctx, "dh-rsv-test", peer, mode, o, childRAMAC)
	}
}

func endpointAdd(mode string) func(*Plugin, context.Context, DHCPNetworkOptions) (func(), error) {
	return func(p *Plugin, ctx context.Context, o DHCPNetworkOptions) (func(), error) {
		return p.addIPAMEndpointLink(ctx, "fedcba9876543210", mode, o, childRAMAC)
	}
}

func TestIPAMReserveLinkTakesNoRouterAdvertisement_Macvlan(t *testing.T) {
	if inOwnNetns(t) {
		ipamLinkCase(t, childRAOpts[ModeMacvlan], "dh-rsv-test", false, reserveAdd(ModeMacvlan, ""))
	}
}

func TestIPAMReserveLinkTakesNoRouterAdvertisement_Ipvlan(t *testing.T) {
	if inOwnNetns(t) {
		ipamLinkCase(t, childRAOpts[ModeIPvlan], "dh-rsv-test", false, reserveAdd(ModeIPvlan, ""))
	}
}

func TestIPAMReserveLinkTakesNoRouterAdvertisement_Bridge(t *testing.T) {
	if inOwnNetns(t) {
		ipamLinkCase(t, childRAOpts[ModeBridge], "dh-rsv-test", false, reserveAdd(ModeBridge, "dh-rsv-peer"))
	}
}

func TestIPAMEndpointLinkTakesNoRouterAdvertisement_Macvlan(t *testing.T) {
	if inOwnNetns(t) {
		ipamLinkCase(t, childRAOpts[ModeMacvlan], subLinkName("fedcba9876543210"), false, endpointAdd(ModeMacvlan))
	}
}

func TestIPAMEndpointLinkTakesNoRouterAdvertisement_Bridge(t *testing.T) {
	_, ctr := vethPairNames("fedcba9876543210")
	if inOwnNetns(t) {
		ipamLinkCase(t, childRAOpts[ModeBridge], ctr, false, endpointAdd(ModeBridge))
	}
}

// An IPv6 network keeps IPv6 on its IPAM endpoint link (#1247).
func TestIPAMEndpointLinkOfIPv6NetworkKeepsIPv6_Macvlan(t *testing.T) {
	if inOwnNetns(t) {
		o := childRAOpts[ModeMacvlan]
		o.IPv6 = true
		ipamLinkCase(t, o, subLinkName("fedcba9876543210"), true, endpointAdd(ModeMacvlan))
	}
}

func TestIPAMEndpointLinkOfIPv6NetworkKeepsIPv6_Bridge(t *testing.T) {
	_, ctr := vethPairNames("fedcba9876543210")
	if inOwnNetns(t) {
		o := childRAOpts[ModeBridge]
		o.IPv6 = true
		ipamLinkCase(t, o, ctr, true, endpointAdd(ModeBridge))
	}
}

// A link entering a namespace gets fresh IPv6 state, so the switch does not follow it into the container, and Join's
// preparation turns IPv6 on where the engine turned it off (#1247).
func TestChildHostIPv6OffDoesNotReachTheContainer(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	f := newChildRAFixture(t)
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "par0"}
	name := subLinkName("0123456789abcdef")
	child := f.addChild(t, opts, name)
	childIPv6Off(name)
	if got := childIPv6Disabled(t, name); got != "1" {
		t.Fatalf("disable_ipv6 on the host-side child = %q, want 1", got)
	}

	ctrNS, err := netns.New()
	if err != nil {
		t.Fatalf("netns.New: %v", err)
	}
	defer func() { _ = ctrNS.Close() }()
	if err := netns.Set(f.host); err != nil {
		t.Fatal(err)
	}
	if err := f.h.LinkSetNsFd(child, int(ctrNS)); err != nil {
		t.Fatal(err)
	}
	if err := netns.Set(ctrNS); err != nil {
		t.Fatal(err)
	}
	if got := childIPv6Disabled(t, name); got != "0" {
		t.Fatalf("disable_ipv6 on the moved link = %q, want 0 (fresh state in the new namespace)", got)
	}

	writeSysctl(t, "/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", "1")
	changed, _, err := prepareV6LinkUnder(ipv6DisableSysctlDir, name, 0)
	if err != nil || !changed {
		t.Fatalf("prepareV6LinkUnder changed=%v err=%v, want it to clear disable_ipv6", changed, err)
	}
	if got := childIPv6Disabled(t, name); got != "0" {
		t.Fatalf("disable_ipv6 after Join's preparation = %q, want 0", got)
	}
}

// sendRAs sends a router advertisement (lifetime 1800 s, autonomous prefix 2001:db8:1::/64) to ff02::1 from rtr0.
func sendRAs(ns netns.NsHandle, stop chan struct{}, errc chan error) {
	runtime.LockOSThread() // never unlocked: this thread leaves its namespace changed and must die with the goroutine.
	if err := netns.Set(ns); err != nil {
		errc <- err
		return
	}
	writeOK := func(p, v string) { _ = os.WriteFile(p, []byte(v), 0o644) }
	writeOK("/proc/sys/net/ipv6/conf/rtr0/accept_dad", "0")
	h, err := netlink.NewHandle()
	if err != nil {
		errc <- err
		return
	}
	rtr, err := h.LinkByName("rtr0")
	if err != nil {
		errc <- err
		return
	}
	if err := h.LinkSetUp(rtr); err != nil {
		errc <- err
		return
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW, unix.IPPROTO_ICMPV6)
	if err != nil {
		errc <- err
		return
	}
	defer unix.Close(fd)
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 255)
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, rtr.Attrs().Index)
	_ = unix.BindToDevice(fd, "rtr0")

	ra := make([]byte, 16+32)
	ra[0] = 134 // type RA
	ra[4] = 64  // cur hop limit
	binary.BigEndian.PutUint16(ra[6:], 1800)
	pi := ra[16:]
	pi[0], pi[1], pi[2], pi[3] = 3, 4, 64, 0xC0 // prefix info, len 4, /64, L|A
	binary.BigEndian.PutUint32(pi[4:], 86400)
	binary.BigEndian.PutUint32(pi[8:], 14400)
	copy(pi[16:], net.ParseIP("2001:db8:1::").To16())

	dst := &unix.SockaddrInet6{ZoneId: uint32(rtr.Attrs().Index)}
	copy(dst.Addr[:], net.ParseIP("ff02::1").To16())
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		_ = unix.Sendto(fd, ra, 0, dst)
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

type ipv6OffWrite struct {
	name     string
	up       bool
	disabled string
}

// recordIPv6Off wraps the switch and records, at the moment of each write, the link's up flag and the value read back.
func recordIPv6Off(t *testing.T) *[]ipv6OffWrite {
	t.Helper()
	var got []ipv6OffWrite
	prev := childHostIPv6Off
	t.Cleanup(func() { childHostIPv6Off = prev })
	childHostIPv6Off = func(name string) error {
		err := prev(name)
		l, lerr := netlink.LinkByName(name)
		if lerr != nil {
			t.Errorf("the link %s is absent when its IPv6 is switched off: %v", name, lerr)
			return err
		}
		b, _ := os.ReadFile("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6")
		got = append(got, ipv6OffWrite{name, l.Attrs().Flags&net.FlagUp != 0, strings.TrimSpace(string(b))})
		return err
	}
	return &got
}

func checkIPv6OffBeforeUp(t *testing.T, got []ipv6OffWrite, prefix string) {
	t.Helper()
	if len(got) != 1 || !strings.HasPrefix(got[0].name, prefix) {
		t.Fatalf("IPv6 was switched off on %+v, want once on a link named %s*", got, prefix)
	}
	if got[0].up || got[0].disabled != "1" {
		t.Errorf("at the switch %s was up=%v with disable_ipv6=%q, want down and 1", got[0].name, got[0].up, got[0].disabled)
	}
}

// The probe link is gone when the probe returns, so the kernel is read at the write: IPv6 off, link still down (#1247).
func TestRunDHCPProbe_TheProbeLinkHasIPv6OffBeforeItComesUp(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p := probeNetnsPlugin(t)
	probeVethPair(t, probeParentName, probeServerName)
	got := recordIPv6Off(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = p.runDHCPProbe(ctx, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeParentName}, serverPolicy{})
	checkIPv6OffBeforeUp(t, *got, "dh-probe-")
}
