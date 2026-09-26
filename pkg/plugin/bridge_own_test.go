// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Ifindexes no host has, so a call that reaches the real kernel by index finds nothing (#903).
const (
	bridgeTestParentIndex = 2147480010
	bridgeTestBridgeIndex = 2147480011
	bridgeTestOtherIndex  = 2147480012
	bridgeTestParent      = "dh903par"
	bridgeTestBridge      = "dh903br"
)

func bridgeTestParentLink() *netlink.Device {
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: bridgeTestParent, Index: bridgeTestParentIndex, Flags: net.FlagUp, MTU: 1500}}
}

func bridgeTestBridgeLink(alias string) *netlink.Bridge {
	return &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeTestBridge, Index: bridgeTestBridgeIndex, Flags: net.FlagUp, Alias: alias, MTU: 1500}}
}

func bridgeOwnOpts() DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeBridge, Bridge: bridgeTestBridge, Parent: bridgeTestParent}
}

// bridgeKernel is the host as the seams see it; the fields after links record what the plugin asked of the kernel,
// the only outside evidence a unit test has (#903).
type bridgeKernel struct {
	links     map[string]netlink.Link
	addrs     map[int][]netlink.Addr
	routes    []netlink.Route
	addrErr   map[int]error
	routeErr  error
	deleted   []string
	added     []string
	enslaved  []string
	v6Off     []string
	addErr    error
	aliasErr  error
	v6OffErr  error
	upErr     error
	masterErr error
}

// inFamily reports whether ip belongs to a netlink family; a route with no Dst and no Gw is in every one (#903).
func inFamily(family int, ip net.IP) bool {
	switch {
	case family == netlink.FAMILY_ALL || ip == nil:
		return true
	case ip.To4() != nil:
		return family == netlink.FAMILY_V4
	default:
		return family == netlink.FAMILY_V6
	}
}

func stubBridgeKernel(t *testing.T, links ...netlink.Link) *bridgeKernel {
	t.Helper()
	k := &bridgeKernel{links: map[string]netlink.Link{}, addrs: map[int][]netlink.Addr{}, addrErr: map[int]error{}}
	for _, l := range links {
		k.links[l.Attrs().Name] = l
	}
	prevBy, prevDel, prevUp, prevList, prevAlias, prevMaster := nlLinkByName, nlLinkDel, nlLinkSetUp, nlLinkList, nlLinkSetAlias, nlLinkSetMaster
	prevAddr, prevRoute, prevAdd, prevV6 := nlAddrList, nlRouteListFiltered, bridgeLinkAdd, bridgeHostIPv6Off
	t.Cleanup(func() {
		nlLinkByName, nlLinkDel, nlLinkSetUp, nlLinkList, nlLinkSetAlias, nlLinkSetMaster = prevBy, prevDel, prevUp, prevList, prevAlias, prevMaster
		nlAddrList, nlRouteListFiltered, bridgeLinkAdd, bridgeHostIPv6Off = prevAddr, prevRoute, prevAdd, prevV6
	})
	nlLinkByName = func(name string) (netlink.Link, error) {
		if l, ok := k.links[name]; ok {
			return l, nil
		}
		return nil, netlink.LinkNotFoundError{}
	}
	nlLinkDel = func(l netlink.Link) error {
		k.deleted = append(k.deleted, l.Attrs().Name)
		delete(k.links, l.Attrs().Name)
		// Deleting a bridge releases its ports, measured on Linux 6.12 (#903).
		for _, port := range k.links {
			if port.Attrs().MasterIndex == l.Attrs().Index {
				port.Attrs().MasterIndex, port.Attrs().Promisc = 0, 0
			}
		}
		return nil
	}
	nlLinkSetUp = func(l netlink.Link) error {
		if k.upErr != nil {
			return k.upErr
		}
		l.Attrs().Flags |= net.FlagUp
		return nil
	}
	nlLinkList = func() ([]netlink.Link, error) {
		var out []netlink.Link
		for _, l := range k.links {
			out = append(out, l)
		}
		return out, nil
	}
	nlLinkSetAlias = func(l netlink.Link, alias string) error {
		if k.aliasErr != nil {
			return k.aliasErr
		}
		l.Attrs().Alias = alias
		return nil
	}
	nlLinkSetMaster = func(l, master netlink.Link) error {
		if k.masterErr != nil {
			return k.masterErr
		}
		k.enslaved = append(k.enslaved, l.Attrs().Name+"->"+master.Attrs().Name)
		l.Attrs().MasterIndex, l.Attrs().Promisc = master.Attrs().Index, 1
		master.Attrs().MTU = l.Attrs().MTU
		return nil
	}
	nlAddrList = func(l netlink.Link, family int) ([]netlink.Addr, error) {
		if err := k.addrErr[l.Attrs().Index]; err != nil {
			return nil, err
		}
		var out []netlink.Addr
		for _, a := range k.addrs[l.Attrs().Index] {
			if inFamily(family, a.IP) {
				out = append(out, a)
			}
		}
		return out, nil
	}
	// It honours the family and the table filter as the kernel does, so a read of table local shows up (#903).
	nlRouteListFiltered = func(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
		if k.routeErr != nil {
			return nil, k.routeErr
		}
		var out []netlink.Route
		for _, r := range k.routes {
			table := r.Table
			if table == 0 {
				table = unix.RT_TABLE_MAIN
			}
			// As netlink does: a table filter of RT_TABLE_UNSPEC reads every table (#903).
			if mask&netlink.RT_FILTER_TABLE != 0 && filter.Table != unix.RT_TABLE_UNSPEC && filter.Table != table {
				continue
			}
			if mask&netlink.RT_FILTER_TABLE == 0 && table != unix.RT_TABLE_MAIN {
				continue
			}
			ip := r.Gw
			if r.Dst != nil {
				ip = r.Dst.IP
			}
			if !inFamily(family, ip) {
				continue
			}
			out = append(out, r)
		}
		return out, nil
	}
	bridgeLinkAdd = func(_ *parentGuard, l netlink.Link) error {
		k.added = append(k.added, l.Attrs().Name)
		if k.addErr != nil {
			return k.addErr
		}
		l.Attrs().Index = bridgeTestBridgeIndex
		l.Attrs().MTU = 1500
		k.links[l.Attrs().Name] = l
		return nil
	}
	bridgeHostIPv6Off = func(name string) error {
		state := "down"
		if l, ok := k.links[name]; ok && l.Attrs().Flags&net.FlagUp != 0 {
			state = "up"
		}
		k.v6Off = append(k.v6Off, name+" while "+state)
		return k.v6OffErr
	}
	return k
}

// stubFirewall sets the D0 reads; the returned func reports whether the policy was read.
func stubFirewall(t *testing.T, nfCall bool, nfErr error, policy uint32, policyErr error) *int {
	t.Helper()
	prevNF, prevPolicy := bridgeNFCallIPTables, readForwardPolicy
	t.Cleanup(func() { bridgeNFCallIPTables, readForwardPolicy = prevNF, prevPolicy })
	reads := new(int)
	bridgeNFCallIPTables = func(string) (bool, error) { return nfCall, nfErr }
	readForwardPolicy = func() (uint32, error) {
		*reads++
		return policy, policyErr
	}
	return reads
}

// storeBridgeOwner saves the record of a network that made the test bridge from parent (#903).
func storeBridgeOwner(t *testing.T, id, parent string) {
	t.Helper()
	opts := bridgeOwnOpts()
	opts.Parent = parent
	if err := saveOptions(id, opts); err != nil {
		t.Fatal(err)
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustAddr(t *testing.T, s string) netlink.Addr {
	t.Helper()
	a, err := netlink.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return *a
}

// TestParentAddressItem is the note's section 4 table: what the kernel puts on an up link with IPv6 on passes, and
// everything a host script or DHCP client puts there refuses (#903).
func TestParentAddressItem(t *testing.T) {
	const p = bridgeTestParentIndex
	fe80 := netlink.Route{LinkIndex: p, Dst: mustCIDR(t, "fe80::/64"), Protocol: unix.RTPROT_KERNEL, Table: unix.RT_TABLE_MAIN}
	localTable := []netlink.Route{
		{LinkIndex: p, Dst: mustCIDR(t, "fe80::1234/128"), Protocol: unix.RTPROT_KERNEL, Table: unix.RT_TABLE_LOCAL, Type: unix.RTN_LOCAL},
		{LinkIndex: p, Dst: mustCIDR(t, "ff00::/8"), Protocol: unix.RTPROT_KERNEL, Table: unix.RT_TABLE_LOCAL},
		{LinkIndex: p, Dst: mustCIDR(t, "192.0.2.9/32"), Protocol: unix.RTPROT_KERNEL, Table: unix.RT_TABLE_LOCAL, Type: unix.RTN_LOCAL},
	}
	for _, tc := range []struct {
		name   string
		addrs  []string
		routes []netlink.Route
		want   string
	}{
		{name: "nothing passes"},
		{name: "fe80 and the kernel's own routes pass", addrs: []string{"fe80::1234/64"}, routes: append([]netlink.Route{fe80}, localTable...)},
		{name: "a route out of another link passes", routes: []netlink.Route{{LinkIndex: bridgeTestOtherIndex, Dst: mustCIDR(t, "10.9.0.0/16")}}},
		{name: "169.254 IPv4 refuses", addrs: []string{"169.254.7.7/16"}, want: "the IPv4 address 169.254.7.7/16"},
		{name: "a global IPv6 refuses", addrs: []string{"fe80::1234/64", "2001:db8::5/64"}, want: "the IPv6 address 2001:db8::5/64"},
		{name: "a ULA refuses", addrs: []string{"fd00::5/64"}, want: "the IPv6 address fd00::5/64"},
		{name: "a script route refuses", routes: []netlink.Route{fe80, {LinkIndex: p, Dst: mustCIDR(t, "10.9.0.0/16"), Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_BOOT}},
			want: "the route 10.9.0.0/16 proto boot"},
		{name: "a default route refuses", routes: []netlink.Route{{LinkIndex: p, Gw: net.ParseIP("192.0.2.1"), Protocol: unix.RTPROT_STATIC}},
			want: "the route default via 192.0.2.1 proto static"},
		{name: "a script's fe80::/64 refuses", routes: []netlink.Route{{LinkIndex: p, Dst: mustCIDR(t, "fe80::/64"), Protocol: unix.RTPROT_BOOT}},
			want: "the route fe80::/64 proto boot"},
		{name: "a kernel route that is not fe80::/64 refuses", routes: []netlink.Route{{LinkIndex: p, Dst: mustCIDR(t, "192.0.2.0/24"), Protocol: unix.RTPROT_KERNEL}},
			want: "the route 192.0.2.0/24 proto kernel"},
		{name: "a multipath nexthop out of it refuses", routes: []netlink.Route{{Dst: mustCIDR(t, "198.51.100.0/24"), Protocol: unix.RTPROT_STATIC,
			MultiPath: []*netlink.NexthopInfo{{LinkIndex: bridgeTestOtherIndex}, {LinkIndex: p}}}},
			want: "the route 198.51.100.0/24 proto static"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := stubBridgeKernel(t)
			for _, a := range tc.addrs {
				k.addrs[p] = append(k.addrs[p], mustAddr(t, a))
			}
			k.routes = tc.routes
			got, err := parentAddressItem(bridgeTestParentLink())
			if err != nil || got != tc.want {
				t.Errorf("item = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("an unreadable address list refuses", func(t *testing.T) {
		k := stubBridgeKernel(t)
		k.addrErr[p] = errors.New("boom")
		if _, err := parentAddressItem(bridgeTestParentLink()); err == nil {
			t.Error("an unread address list passed the parent")
		}
	})
	t.Run("an unreadable route list refuses", func(t *testing.T) {
		k := stubBridgeKernel(t)
		k.routeErr = errors.New("boom")
		if _, err := parentAddressItem(bridgeTestParentLink()); err == nil {
			t.Error("an unread route list passed the parent")
		}
	})
}

func TestRefuseEnslavedParent(t *testing.T) {
	l := bridgeTestParentLink()
	if err := refuseEnslavedParent(l, 0); err != nil {
		t.Errorf("a free parent refused: %v", err)
	}
	l.MasterIndex = bridgeTestBridgeIndex
	if err := refuseEnslavedParent(l, bridgeTestBridgeIndex); err != nil {
		t.Errorf("a port of its own bridge refused: %v", err)
	}
	err := refuseEnslavedParent(l, 0)
	if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "already a port of index 2147480011") {
		t.Errorf("err = %v; want a refusal naming the master: the kernel moves a port without an error (#903)", err)
	}
}

func TestValidateBridgeOwnOptions(t *testing.T) {
	with := func(f func(*DHCPNetworkOptions)) DHCPNetworkOptions {
		o := bridgeOwnOpts()
		f(&o)
		return o
	}
	for _, tc := range []struct {
		name string
		opts DHCPNetworkOptions
		want error
		text string
	}{
		{name: "parent in bridge mode", opts: bridgeOwnOpts()},
		{name: "force_create with parent", opts: with(func(o *DHCPNetworkOptions) { o.ForceCreate = true })},
		{name: "release_lease=never", opts: with(func(o *DHCPNetworkOptions) { o.ReleaseLease = "never" })},
		{name: "bridge mode without parent", opts: DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-lan", ReleaseLease: ReleaseOnStop}},
		{name: "force_create without parent", opts: DHCPNetworkOptions{Bridge: "lan0", ForceCreate: true}, want: util.ErrModeMismatch, text: "force_create"},
		{name: "force_create in macvlan", opts: DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", ForceCreate: true}, want: util.ErrModeMismatch, text: "force_create"},
		{name: "an illegal parent", opts: with(func(o *DHCPNetworkOptions) { o.Parent = "a/b" }), want: util.ErrIPAM, text: "invalid parent"},
		{name: "parent is the bridge", opts: with(func(o *DHCPNetworkOptions) { o.Parent = bridgeTestBridge }), want: util.ErrIPAM, text: "both name"},
		{name: "docker0", opts: with(func(o *DHCPNetworkOptions) { o.Bridge = "docker0" }), want: util.ErrIPAM, text: "docker0 and br-*"},
		{name: "br-*", opts: with(func(o *DHCPNetworkOptions) { o.Bridge = "br-lan" }), want: util.ErrIPAM, text: "docker0 and br-*"},
		{name: "release_lease", opts: with(func(o *DHCPNetworkOptions) { o.ReleaseLease = ReleaseOnStop }), want: util.ErrIPAM, text: "release_lease=" + ReleaseOnStop + " is refused"},
		{name: "an unknown release_lease", opts: with(func(o *DHCPNetworkOptions) { o.ReleaseLease = "soon" }), want: util.ErrIPAM, text: `release_lease "soon" is not one of`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBridgeOwnOptions(tc.opts)
			if tc.want == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v; want %v containing %q", err, tc.want, tc.text)
			}
		})
	}
}

func TestEnsureBridge(t *testing.T) {
	t.Run("an absent bridge is made, marked, kept off IPv6 and takes the parent", func(t *testing.T) {
		k := stubBridgeKernel(t, bridgeTestParentLink())
		created, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
		if err != nil || !created {
			t.Fatalf("created %v, err %v", created, err)
		}
		br, ok := k.links[bridgeTestBridge].(*netlink.Bridge)
		switch {
		case !ok:
			t.Fatalf("links %v; want %s a bridge", k.links, bridgeTestBridge)
		case br.Alias != vlanOwnerAlias:
			t.Errorf("alias %q; want the mark, or a retire leaves it behind", br.Alias)
		case br.Flags&net.FlagUp == 0:
			t.Error("the bridge is down; nothing crosses it")
		case strings.Join(k.v6Off, ",") != bridgeTestBridge+" while down":
			t.Errorf("disable_ipv6 %v; want it written once while down, before the kernel gives the bridge an fe80", k.v6Off)
		case k.links[bridgeTestParent].Attrs().MasterIndex != bridgeTestBridgeIndex:
			t.Error("the parent is not a port of the bridge")
		}
	})
	t.Run("a failed bridge lookup makes no bridge", func(t *testing.T) {
		k := stubBridgeKernel(t, bridgeTestParentLink())
		lookup := nlLinkByName
		nlLinkByName = func(name string) (netlink.Link, error) {
			if name == bridgeTestBridge {
				return nil, errors.New("netlink gone")
			}
			return lookup(name)
		}
		_, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
		if err == nil || !strings.Contains(err.Error(), "failed to look up bridge") || len(k.added)+len(k.enslaved) != 0 {
			t.Errorf("err %v, added %v, enslaved %v; want the lookup error and nothing made", err, k.added, k.enslaved)
		}
	})
	t.Run("a marked bridge holding the parent is used as is", func(t *testing.T) {
		parent := bridgeTestParentLink()
		parent.MasterIndex = bridgeTestBridgeIndex
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		created, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_endpoint")
		if err != nil || created || len(k.added)+len(k.enslaved)+len(k.deleted) != 0 {
			t.Errorf("created %v, err %v, added %v, enslaved %v, deleted %v; want nothing done", created, err, k.added, k.enslaved, k.deleted)
		}
	})
	t.Run("a marked bridge without the parent takes it again", func(t *testing.T) {
		k := stubBridgeKernel(t, bridgeTestParentLink(), bridgeTestBridgeLink(vlanOwnerAlias))
		p := newTestPlugin(t)
		storeBridgeOwner(t, vlanNetA, bridgeTestParent)
		created, err := p.ensureBridge(context.Background(), bridgeOwnOpts(), "create_endpoint")
		if err != nil || created || strings.Join(k.enslaved, ",") != bridgeTestParent+"->"+bridgeTestBridge {
			t.Errorf("created %v, err %v, enslaved %v; want the parent enslaved again", created, err, k.enslaved)
		}
	})
	t.Run("a marked bridge without the parent refuses an addressed parent", func(t *testing.T) {
		k := stubBridgeKernel(t, bridgeTestParentLink(), bridgeTestBridgeLink(vlanOwnerAlias))
		k.addrs[bridgeTestParentIndex] = []netlink.Addr{mustAddr(t, "192.0.2.5/24")}
		p := newTestPlugin(t)
		storeBridgeOwner(t, vlanNetA, bridgeTestParent)
		_, err := p.ensureBridge(context.Background(), bridgeOwnOpts(), "create_endpoint")
		if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "carries the IPv4 address") || len(k.enslaved) != 0 {
			t.Errorf("err %v, enslaved %v; want a refusal and nothing enslaved", err, k.enslaved)
		}
	})
	t.Run("a marked bridge refuses a parent another master holds", func(t *testing.T) {
		parent := bridgeTestParentLink()
		parent.MasterIndex = bridgeTestOtherIndex
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		p := newTestPlugin(t)
		storeBridgeOwner(t, vlanNetA, bridgeTestParent)
		_, err := p.ensureBridge(context.Background(), bridgeOwnOpts(), "create_endpoint")
		if err == nil || !strings.Contains(err.Error(), "already a port") || len(k.enslaved) != 0 {
			t.Errorf("err %v, enslaved %v; want the MasterIndex refusal", err, k.enslaved)
		}
	})
	for _, tc := range []struct {
		name     string
		existing netlink.Link
	}{
		{"an unmarked bridge is refused", bridgeTestBridgeLink("")},
		{"a bridge marked by another tool is refused", bridgeTestBridgeLink("lan")},
		{"a dummy of that name carrying the mark is refused", &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: bridgeTestBridge, Index: bridgeTestBridgeIndex, Alias: vlanOwnerAlias}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := stubBridgeKernel(t, bridgeTestParentLink(), tc.existing)
			_, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
			if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "this plugin did not make it") ||
				len(k.enslaved)+len(k.added)+len(k.deleted) != 0 {
				t.Errorf("err %v, enslaved %v, added %v, deleted %v; want the refusal and nothing touched", err, k.enslaved, k.added, k.deleted)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		parent netlink.Link
		setup  func(*bridgeKernel)
		want   error
		text   string
	}{
		{name: "a parent another bridge holds", parent: func() netlink.Link { l := bridgeTestParentLink(); l.MasterIndex = bridgeTestOtherIndex; return l }(),
			want: util.ErrIPAM, text: "already a port"},
		{name: "an addressed parent", parent: bridgeTestParentLink(),
			setup: func(k *bridgeKernel) {
				k.addrs[bridgeTestParentIndex] = []netlink.Addr{{IPNet: mustCIDR(t, "192.0.2.0/24")}}
			},
			want: util.ErrIPAM, text: "carries the IPv4 address"},
		{name: "a routed parent", parent: bridgeTestParentLink(),
			setup: func(k *bridgeKernel) {
				k.routes = []netlink.Route{{LinkIndex: bridgeTestParentIndex, Gw: net.ParseIP("192.0.2.1"), Protocol: unix.RTPROT_DHCP}}
			},
			want: util.ErrIPAM, text: "carries the route default via 192.0.2.1"},
		{name: "a down parent", parent: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: bridgeTestParent, Index: bridgeTestParentIndex}},
			want: util.ErrParentDown},
		{name: "a bridge as parent", parent: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeTestParent, Index: bridgeTestParentIndex, Flags: net.FlagUp}},
			want: util.ErrParentInvalid},
		{name: "an absent parent", want: nil, text: "failed to lookup parent interface"},
	} {
		t.Run(tc.name+" makes no bridge", func(t *testing.T) {
			var links []netlink.Link
			if tc.parent != nil {
				links = append(links, tc.parent)
			}
			k := stubBridgeKernel(t, links...)
			if tc.setup != nil {
				tc.setup(k)
			}
			_, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) || len(k.added) != 0 {
				t.Errorf("err %v, added %v; want %v containing %q and no bridge", err, k.added, tc.want, tc.text)
			}
		})
	}
	t.Run("a bridge made between the lookup and the add is refused", func(t *testing.T) {
		k := stubBridgeKernel(t, bridgeTestParentLink())
		prev := bridgeLinkAdd
		bridgeLinkAdd = func(g *parentGuard, l netlink.Link) error {
			k.links[bridgeTestBridge] = bridgeTestBridgeLink("")
			return prev(g, l)
		}
		k.addErr = unix.EEXIST
		_, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
		if err == nil || !strings.Contains(err.Error(), "this plugin did not make it") || len(k.deleted) != 0 {
			t.Errorf("err %v, deleted %v; want the not-ours refusal and the foreign bridge left alone", err, k.deleted)
		}
	})
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		set  func(*bridgeKernel)
	}{
		{"the mark", func(k *bridgeKernel) { k.aliasErr = boom }},
		{"disable_ipv6", func(k *bridgeKernel) { k.v6OffErr = boom }},
		{"the link up", func(k *bridgeKernel) { k.upErr = boom }},
		{"the enslave", func(k *bridgeKernel) { k.masterErr = boom }},
	} {
		t.Run("a failed "+tc.name+" removes the bridge", func(t *testing.T) {
			k := stubBridgeKernel(t, bridgeTestParentLink())
			tc.set(k)
			created, err := newTestPlugin(t).ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
			if !errors.Is(err, boom) || created || strings.Join(k.deleted, ",") != bridgeTestBridge {
				t.Errorf("created %v, err %v, deleted %v; want the error and the bridge removed", created, err, k.deleted)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		bridge  bool
		store   func(t *testing.T)
		text    string
		want    error
		allowed bool
	}{
		{name: "an absent bridge another network made from another parent", text: "takes one parent", want: util.ErrIPAM,
			store: func(t *testing.T) { storeBridgeOwner(t, vlanNetB, "dh903oth") }},
		{name: "a marked bridge another network made from another parent", bridge: true, text: "takes one parent", want: util.ErrIPAM,
			store: func(t *testing.T) { storeBridgeOwner(t, vlanNetB, "dh903oth") }},
		{name: "a marked bridge no stored network made", bridge: true, text: "no network of this plugin was made on it", want: util.ErrIPAM},
		{name: "a marked bridge holding the parent's own network and another's", bridge: true, text: "takes one parent", want: util.ErrIPAM,
			store: func(t *testing.T) {
				storeBridgeOwner(t, vlanNetA, bridgeTestParent)
				storeBridgeOwner(t, vlanNetB, "dh903oth")
			}},
		{name: "an unreadable store", text: "cannot read the stored networks",
			store: func(t *testing.T) { withStateDir(t, filepath.Join(t.TempDir(), "absent")) }},
		{name: "a network on the bridge as found and another bridge's owner", allowed: true,
			store: func(t *testing.T) {
				if err := saveOptions(vlanNetB, DHCPNetworkOptions{Mode: ModeBridge, Bridge: bridgeTestBridge}); err != nil {
					t.Fatal(err)
				}
				if err := saveOptions(strings.Repeat("c", 64), DHCPNetworkOptions{Mode: ModeBridge, Bridge: "dh903br2", Parent: "dh903oth"}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a macvlan network on another parent", allowed: true,
			store: func(t *testing.T) {
				if err := saveOptions(vlanNetB, DHCPNetworkOptions{Mode: ModeMacvlan, Bridge: bridgeTestBridge, Parent: "dh903oth"}); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		// One bridge takes one parent: a second NIC enslaved by a create or a child would outlive its network (#903).
		t.Run(tc.name, func(t *testing.T) {
			links := []netlink.Link{bridgeTestParentLink()}
			if tc.bridge {
				links = append(links, bridgeTestBridgeLink(vlanOwnerAlias))
			}
			k := stubBridgeKernel(t, links...)
			p := newTestPlugin(t)
			if tc.store != nil {
				tc.store(t)
			}
			_, err := p.ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
			if tc.allowed {
				if err != nil || len(k.enslaved) != 1 {
					t.Errorf("err %v, enslaved %v; want the parent enslaved", err, k.enslaved)
				}
				return
			}
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) ||
				len(k.added)+len(k.enslaved) != 0 {
				t.Errorf("err %v, added %v, enslaved %v; want %q and nothing made or enslaved", err, k.added, k.enslaved, tc.text)
			}
		})
	}
	t.Run("a network without parent touches nothing", func(t *testing.T) {
		k := stubBridgeKernel(t)
		created, err := newTestPlugin(t).ensureBridge(context.Background(), DHCPNetworkOptions{Bridge: bridgeTestBridge}, "create_network")
		if err != nil || created || len(k.added) != 0 {
			t.Errorf("created %v, err %v, added %v; want nothing", created, err, k.added)
		}
	})
}

// TestEnsureBridge_TakesTheBridgeGateKind reads the kind from the gate: a macvlan holder is a wait the kernel would
// refuse beside the port, a vlan or bridge holder is not (#903).
func TestEnsureBridge_TakesTheBridgeGateKind(t *testing.T) {
	for _, tc := range []struct {
		holder          string
		waits, timeouts int32
	}{
		{parentGateKindBridge, 1, 0},
		{parentGateKindVlan, 1, 0},
		{ModeMacvlan, 0, 1},
		{ModeIPvlan, 0, 1},
	} {
		t.Run(tc.holder, func(t *testing.T) {
			p := newTestPlugin(t)
			stubBridgeKernel(t, bridgeTestParentLink())
			holder := p.lockParent(context.Background(), bridgeTestParent, tc.holder, "test-holder")
			defer holder.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.ensureBridge(ctx, bridgeOwnOpts(), "create_network"); err != nil {
				t.Fatal(err)
			}
			if p.parentLinkWaits.Load() != tc.waits || p.parentLinkWaitTimeouts.Load() != tc.timeouts {
				t.Errorf("waits %d, timeouts %d; want %d, %d", p.parentLinkWaits.Load(), p.parentLinkWaitTimeouts.Load(), tc.waits, tc.timeouts)
			}
		})
	}
}

func TestNFCallIPTablesUnder(t *testing.T) {
	dir := t.TempDir()
	sysctl := filepath.Join(dir, "bridge-nf-call-iptables")
	classDir := filepath.Join(dir, "class")
	flag := filepath.Join(classDir, "lan0", "bridge", "nf_call_iptables")
	if err := os.MkdirAll(filepath.Dir(flag), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, v string) {
		if err := os.WriteFile(path, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, sysctl, flag, bridge string
		want                       bool
	}{
		{name: "no br_netfilter and no bridge", bridge: "absent"},
		{name: "sysctl 1", sysctl: "1", bridge: "absent", want: true},
		{name: "sysctl 0, bridge 0", sysctl: "0", flag: "0", bridge: "lan0"},
		{name: "sysctl 0, bridge 1", sysctl: "0", flag: "1", bridge: "lan0", want: true},
		{name: "no sysctl, bridge 1", flag: "1", bridge: "lan0", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(sysctl)
			_ = os.Remove(flag)
			if tc.sysctl != "" {
				write(sysctl, tc.sysctl)
			}
			if tc.flag != "" {
				write(flag, tc.flag)
			}
			got, err := nfCallIPTablesUnder(sysctl, classDir, tc.bridge)
			if err != nil || got != tc.want {
				t.Errorf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	t.Run("an unreadable sysctl is an error", func(t *testing.T) {
		if _, err := nfCallIPTablesUnder(dir, classDir, "lan0"); err == nil {
			t.Error("a directory read as a sysctl passed")
		}
	})
}

// TestFirewallRefusal is decision D0 (#903).
func TestFirewallRefusal(t *testing.T) {
	const accept = 1
	unread := errors.New("no such file or directory")
	for _, tc := range []struct {
		name      string
		nfCall    bool
		nfErr     error
		policy    uint32
		policyErr error
		refuse    string
		reads     int
	}{
		{name: "br_netfilter on and FORWARD DROP refuses", nfCall: true, policy: nfDrop, refuse: "the policy of the ip filter FORWARD chain is DROP", reads: 1},
		{name: "br_netfilter on and FORWARD ACCEPT passes", nfCall: true, policy: accept, reads: 1},
		{name: "br_netfilter off passes without the policy read", nfCall: false, policy: nfDrop},
		{name: "an unreadable policy refuses", nfCall: true, policyErr: unread, refuse: "cannot be read over nf_tables", reads: 1},
		{name: "an unreadable sysctl refuses", nfErr: unread, refuse: "bridge-nf-call-iptables cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := stubFirewall(t, tc.nfCall, tc.nfErr, tc.policy, tc.policyErr)
			err := firewallRefusal(bridgeOwnOpts())
			if *reads != tc.reads {
				t.Errorf("policy reads %d, want %d", *reads, tc.reads)
			}
			if tc.refuse == "" {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			for _, want := range []string{tc.refuse, "iptables -I DOCKER-USER -i dh903br -o dh903br -j ACCEPT", "-o force_create=true"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v; want it to name %q", err, want)
				}
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("err = %v; want ErrIPAM, a 400 to the daemon", err)
			}
		})
	}
	t.Run("force_create logs the verdict and creates", func(t *testing.T) {
		reads := stubFirewall(t, true, nil, nfDrop, nil)
		opts := bridgeOwnOpts()
		opts.ForceCreate = true
		var err error
		logged := captureLog(t, func() { err = firewallRefusal(opts) })
		if err != nil || *reads != 1 || !strings.Contains(logged, "level=warning") || !strings.Contains(logged, "is DROP") {
			t.Errorf("err %v, reads %d, log %q; want nil, the check run, and its verdict at warning level", err, *reads, logged)
		}
	})
	t.Run("force_create logs a clean verdict too", func(t *testing.T) {
		stubFirewall(t, false, nil, nfDrop, nil)
		opts := bridgeOwnOpts()
		opts.ForceCreate = true
		logged := captureLog(t, func() { _ = firewallRefusal(opts) })
		if !strings.Contains(logged, "level=warning") || !strings.Contains(logged, "found nothing") {
			t.Errorf("log %q; want the clean verdict at warning level", logged)
		}
	})
}

func TestBridgeUsers(t *testing.T) {
	const self = "self"
	stored := map[string]DHCPNetworkOptions{
		self:    bridgeOwnOpts(),
		"other": {Mode: ModeBridge, Bridge: "lan9"},
	}
	for _, tc := range []struct {
		name   string
		stored map[string]DHCPNetworkOptions
		nets   []dNetwork.Summary
		want   int
	}{
		{name: "self alone", stored: stored},
		{name: "a stored network on the bridge", stored: map[string]DHCPNetworkOptions{self: bridgeOwnOpts(), "x": {Bridge: bridgeTestBridge}}, want: 1},
		{name: "a stored macvlan network on the parent is not a user",
			stored: map[string]DHCPNetworkOptions{self: bridgeOwnOpts(), "x": {Mode: ModeMacvlan, Parent: bridgeTestBridge}}},
		{name: "Docker's bridge driver naming it", stored: stored,
			nets: []dNetwork.Summary{{ID: "n", Name: "native", Driver: "bridge", Options: map[string]string{dockerBridgeNameOption: bridgeTestBridge}}}, want: 1},
		{name: "a listed DHCP network on it", stored: stored,
			nets: []dNetwork.Summary{{ID: "n", Name: "unstored", Driver: "ghcr.io/claymore666/docker-net-dhcp:latest", Options: map[string]string{"bridge": bridgeTestBridge}}}, want: 1},
		{name: "a listed DHCP network that does not decode", stored: stored,
			nets: []dNetwork.Summary{{ID: "n", Name: "odd", Driver: "ghcr.io/claymore666/docker-net-dhcp:latest", Options: map[string]string{"lease_timeout": "soon"}}}, want: 1},
		{name: "self and stored ones in the list are not counted twice", stored: stored,
			nets: []dNetwork.Summary{{ID: self, Driver: "ghcr.io/claymore666/docker-net-dhcp:latest", Options: map[string]string{"bridge": bridgeTestBridge}}}},
		{name: "Docker still listing self during its delete, unstored", stored: map[string]DHCPNetworkOptions{},
			nets: []dNetwork.Summary{{ID: self, Name: "self", Driver: "ghcr.io/claymore666/docker-net-dhcp:latest", Options: map[string]string{"bridge": bridgeTestBridge, "parent": bridgeTestParent}}}},
		{name: "another driver's network with a bridge option is not decoded", stored: stored,
			nets: []dNetwork.Summary{{ID: "n", Name: "third", Driver: "ipvlan", Options: map[string]string{"bridge": bridgeTestBridge}}}},
		{name: "a listed DHCP network on another bridge", stored: stored,
			nets: []dNetwork.Summary{{ID: "n", Name: "far", Driver: "ghcr.io/claymore666/docker-net-dhcp:latest", Options: map[string]string{"bridge": "lan9"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgeUsers(bridgeTestBridge, self, tc.stored, tc.nets); len(got) != tc.want {
				t.Errorf("users %v, want %d", got, tc.want)
			}
		})
	}
}

func TestRetireBridge(t *testing.T) {
	for _, tc := range []struct {
		name       string
		alias      string
		pending    bool
		storeOther bool
		nets       []dNetwork.Summary
		listErr    error
		extraPort  bool
		lookupErr  bool
		storeErr   bool
		wantGone   bool
	}{
		{name: "the last user of a marked bridge removes it", alias: vlanOwnerAlias, wantGone: true},
		{name: "an unmarked bridge stays", alias: ""},
		{name: "a create in flight keeps it", alias: vlanOwnerAlias, pending: true},
		{name: "another stored network keeps it", alias: vlanOwnerAlias, storeOther: true},
		{name: "Docker's own bridge network on it keeps it", alias: vlanOwnerAlias,
			nets: []dNetwork.Summary{{ID: "n", Name: "native", Driver: "bridge", Options: map[string]string{dockerBridgeNameOption: bridgeTestBridge}}}},
		{name: "an unreadable Docker list keeps it", alias: vlanOwnerAlias, listErr: errors.New("daemon gone")},
		{name: "a port other than the parent keeps it", alias: vlanOwnerAlias, extraPort: true},
		{name: "a failed lookup keeps it", alias: vlanOwnerAlias, lookupErr: true},
		{name: "an unreadable store keeps it", alias: vlanOwnerAlias, storeErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPlugin(t)
			p.docker = &fakeDocker{listResult: tc.nets, listErr: tc.listErr}
			parent := bridgeTestParentLink()
			parent.MasterIndex, parent.Promisc = bridgeTestBridgeIndex, 1
			links := []netlink.Link{parent, bridgeTestBridgeLink(tc.alias)}
			if tc.extraPort {
				links = append(links, &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "dh-e1", Index: bridgeTestOtherIndex, MasterIndex: bridgeTestBridgeIndex}})
			}
			k := stubBridgeKernel(t, links...)
			if tc.lookupErr {
				nlLinkByName = func(string) (netlink.Link, error) { return nil, errors.New("netlink gone") }
			}
			if tc.storeErr {
				withStateDir(t, filepath.Join(t.TempDir(), "absent"))
			}
			if tc.storeOther {
				if err := saveOptions("other", DHCPNetworkOptions{Mode: ModeBridge, Bridge: bridgeTestBridge}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pending {
				defer p.beginBridgeCreate(bridgeOwnOpts())()
			}
			p.retireBridge(context.Background(), "self", bridgeOwnOpts(), "delete_network")
			if got := len(k.deleted) == 1; got != tc.wantGone {
				t.Errorf("deleted %v, want gone = %v", k.deleted, tc.wantGone)
			}
			if tc.wantGone && (parent.MasterIndex != 0 || parent.Promisc != 0) {
				t.Errorf("parent master %d, promiscuity %d; want the port released", parent.MasterIndex, parent.Promisc)
			}
		})
	}
	t.Run("a network without parent never retires the bridge", func(t *testing.T) {
		p := newTestPlugin(t)
		p.docker = &fakeDocker{}
		k := stubBridgeKernel(t, bridgeTestBridgeLink(vlanOwnerAlias))
		p.retireBridge(context.Background(), "self", DHCPNetworkOptions{Bridge: bridgeTestBridge}, "delete_network")
		if len(k.deleted) != 0 {
			t.Errorf("deleted %v; a bridge the operator named stays", k.deleted)
		}
	})
	t.Run("a pending count ends", func(t *testing.T) {
		p := newTestPlugin(t)
		p.beginBridgeCreate(bridgeOwnOpts())()
		if len(p.bridgePending) != 0 {
			t.Errorf("pending %v after the create ended", p.bridgePending)
		}
	})
}

func bridgeCreate(p *Plugin, opts map[string]interface{}) error {
	return vlanCreate(p, opts, nil)
}

func TestCreateNetwork_BridgeFromParent(t *testing.T) {
	create := map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent}
	t.Run("makes the bridge and stores parent", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		k := stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		if err := bridgeCreate(p, create); err != nil {
			t.Fatal(err)
		}
		if br, ok := k.links[bridgeTestBridge]; !ok || br.Attrs().Alias != vlanOwnerAlias {
			t.Errorf("links %v; want %s made and marked", k.links, bridgeTestBridge)
		}
		if o, err := loadOptions(vlanNetA); err != nil || o.Parent != bridgeTestParent {
			t.Errorf("stored %+v, %v; want parent kept, or the delete forgets it owns the bridge", o, err)
		}
	})
	t.Run("the firewall refusal makes nothing", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, true, nil, nfDrop, nil)
		k := stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := bridgeCreate(p, create)
		if err == nil || !strings.Contains(err.Error(), "DOCKER-USER") || len(k.added) != 0 {
			t.Errorf("err %v, added %v; want D0's refusal and no bridge", err, k.added)
		}
	})
	t.Run("force_create creates past the firewall refusal", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, true, nil, nfDrop, nil)
		k := stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "force_create": "true"})
		if err != nil || len(k.added) != 1 {
			t.Errorf("err %v, added %v; want the network created", err, k.added)
		}
	})
	t.Run("a network without parent reads no firewall", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		reads := stubFirewall(t, true, errors.New("must not be read"), nfDrop, nil)
		stubBridgeKernel(t, bridgeTestBridgeLink(""))
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		if err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge}); err != nil || *reads != 0 {
			t.Errorf("err %v, reads %d; want today's bridge mode unchanged", err, *reads)
		}
	})
	t.Run("a failed create removes the bridge it made", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		k := stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "mtu": "9000"})
		if !errors.Is(err, util.ErrIPAM) || strings.Join(k.deleted, ",") != bridgeTestBridge {
			t.Errorf("err %v, deleted %v; want the mtu refusal and the bridge removed", err, k.deleted)
		}
		if l := k.links[bridgeTestParent]; l.Attrs().MasterIndex != 0 {
			t.Error("the parent is still a port after the rollback")
		}
	})
	t.Run("a delete between the make and the save keeps the bridge", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		k := stubBridgeKernel(t, bridgeTestParentLink())
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		// The address check of the new bridge runs after ensureBridge and before the save; another network on
		// the bridge is deleted there (#903).
		addrs, raced := nlAddrList, false
		nlAddrList = func(l netlink.Link, family int) ([]netlink.Addr, error) {
			if l.Attrs().Name == bridgeTestBridge && !raced {
				raced = true
				p.retireBridge(context.Background(), "other", bridgeOwnOpts(), "delete_network")
			}
			return addrs(l, family)
		}
		if err := bridgeCreate(p, create); err != nil || !raced || len(k.deleted) != 0 {
			t.Errorf("err %v, raced %v, deleted %v; want the bridge kept for the create in flight", err, raced, k.deleted)
		}
	})
	t.Run("a failed create keeps a bridge it adopted", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		parent := bridgeTestParentLink()
		parent.MasterIndex = bridgeTestBridgeIndex
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		if err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "mtu": "9000"}); err == nil || len(k.deleted) != 0 {
			t.Errorf("err %v, deleted %v; want the refusal and the bridge kept", err, k.deleted)
		}
	})
}

func TestNetOptions_RefusesAStoredBridgeParentTheCreateWouldRefuse(t *testing.T) {
	for _, opts := range []DHCPNetworkOptions{
		{Mode: ModeBridge, Bridge: "br-x", Parent: "eth0"},
		{Mode: ModeBridge, Bridge: "lan0", Parent: "lan0"},
		{Mode: ModeBridge, Bridge: "lan0", ForceCreate: true},
	} {
		withStateDir(t, t.TempDir())
		if err := saveOptions(vlanNetA, opts); err != nil {
			t.Fatal(err)
		}
		p := &Plugin{docker: &fakeDocker{inspectErr: errors.New("docker must not be called")}}
		if _, err := p.netOptions(context.Background(), vlanNetA); err == nil || p.networkOptionsRejected.Load() != 1 {
			t.Errorf("stored %+v was served (err %v, rejected %d)", opts, err, p.networkOptionsRejected.Load())
		}
	}
}

func TestDeleteNetwork_RetiresTheBridge(t *testing.T) {
	for _, c := range []struct {
		name     string
		opts     DHCPNetworkOptions
		wantGone bool
	}{
		{"the last network removes the bridge it made", bridgeOwnOpts(), true},
		{"a network without parent leaves the bridge", DHCPNetworkOptions{Mode: ModeBridge, Bridge: bridgeTestBridge}, false},
		{"a refused stored parent touches nothing", DHCPNetworkOptions{Mode: ModeBridge, Bridge: bridgeTestBridge, Parent: bridgeTestBridge}, false},
		{"a hand-edited release_lease touches nothing", DHCPNetworkOptions{Mode: ModeBridge, Bridge: bridgeTestBridge, Parent: bridgeTestParent, ReleaseLease: "soon"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPlugin(t)
			p.docker = &fakeDocker{}
			k := stubBridgeKernel(t, bridgeTestParentLink(), bridgeTestBridgeLink(vlanOwnerAlias))
			if err := saveOptions(vlanNetA, c.opts); err != nil {
				t.Fatal(err)
			}
			if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: vlanNetA}); err != nil {
				t.Fatal(err)
			}
			if got := len(k.deleted) == 1; got != c.wantGone {
				t.Errorf("deleted %v, want gone = %v", k.deleted, c.wantGone)
			}
		})
	}
}

// TestBridgeSites_ReCreateAVanishedBridge: a host reboot loses the bridge and releases the parent while Docker keeps
// the network, so every site that attaches a veth makes it again (#903).
func TestBridgeSites_ReCreateAVanishedBridge(t *testing.T) {
	for _, s := range []struct {
		name string
		call func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error
	}{
		{"the endpoint", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			if err := saveOptions(vlanNetA, opts); err != nil {
				return err
			}
			_, err := p.CreateEndpoint(ctx, CreateEndpointRequest{NetworkID: vlanNetA, EndpointID: strings.Repeat("e", 64), Interface: &EndpointInterface{}})
			return err
		}},
		{"the IPAM endpoint", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			_, err := p.addIPAMEndpointLink(ctx, strings.Repeat("e", 64), ModeBridge, opts, nil)
			return err
		}},
		{"the IPAM reservation", func(ctx context.Context, p *Plugin, opts DHCPNetworkOptions) error {
			_, err := p.addIPAMReserveLink(ctx, "dh-903-ra", "dh-903-rb", ModeBridge, opts, nil)
			return err
		}},
	} {
		t.Run(s.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			k := stubBridgeKernel(t, bridgeTestParentLink())
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			_ = s.call(context.Background(), p, bridgeOwnOpts())
			if br, ok := k.links[bridgeTestBridge]; !ok || br.Attrs().Alias != vlanOwnerAlias || k.links[bridgeTestParent].Attrs().MasterIndex != bridgeTestBridgeIndex {
				t.Errorf("links %v; want %s made, marked and holding the parent", k.links, bridgeTestBridge)
			}
		})
		t.Run(s.name+" refuses what the create refuses", func(t *testing.T) {
			withStateDir(t, t.TempDir())
			k := stubBridgeKernel(t, bridgeTestParentLink(), bridgeTestBridgeLink(""))
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			err := s.call(context.Background(), p, bridgeOwnOpts())
			if err == nil || !strings.Contains(err.Error(), "this plugin did not make it") || len(k.enslaved) != 0 {
				t.Errorf("err %v, enslaved %v; want the not-ours refusal", err, k.enslaved)
			}
		})
	}
}
