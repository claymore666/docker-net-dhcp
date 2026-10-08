// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	dContainer "github.com/moby/moby/api/types/container"
	dNetwork "github.com/moby/moby/api/types/network"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const (
	epNet       = "net-657"
	epBridge    = "dh657br"
	epParent    = "dh657par"
	epParentIdx = 2147480657
	epBridgeIdx = 2147480658
)

var (
	epKernelMAC = net.HardwareAddr{0x02, 0x42, 0x0a, 0x06, 0x57, 0x01}
	epParentMAC = net.HardwareAddr{0x02, 0x42, 0x0a, 0x06, 0x57, 0xff}
	errInjected = errors.New("injected kernel failure")
)

// endpointKernel is the host the endpoint creators see through the seam; calls is the order the plugin asked in (#657).
type endpointKernel struct {
	links   map[string]netlink.Link
	calls   []string
	deleted []string
	macs    map[string]net.HardwareAddr
	fail    map[string]error
	delErr  error
	noMAC   bool
	v4      dhcp.Info
	v4Err   error
	v6      dhcp.Info
	v6Err   error
}

func (k *endpointKernel) step(op, name string) error {
	k.calls = append(k.calls, op+":"+name)
	return k.fail[op+":"+name]
}

func (k *endpointKernel) saw(call string) bool {
	for _, c := range k.calls {
		if c == call {
			return true
		}
	}
	return false
}

func stubEndpointKernel(t *testing.T) *endpointKernel {
	t.Helper()
	k := &endpointKernel{
		links: map[string]netlink.Link{
			epBridge: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: epBridge, Index: epBridgeIdx, Flags: net.FlagUp}},
			epParent: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: epParent, Index: epParentIdx, Flags: net.FlagUp, HardwareAddr: epParentMAC}},
		},
		macs: map[string]net.HardwareAddr{},
		fail: map[string]error{},
		v4:   dhcp.Info{IP: "192.0.2.57/24", Gateway: "192.0.2.1"},
		v6:   dhcp.Info{IP: "2001:db8::57/128"},
	}
	prevBy, prevEp, prevAdd, prevUp, prevMAC := nlLinkByName, nlEndpointLinkByName, nlLinkAdd, nlLinkSetUp, nlLinkSetHardwareAddr
	prevMaster, prevDel, prevMTU, prevGet, prevAlias := nlLinkSetMaster, nlLinkDel, nlHandleLinkSetMTU, dhcpGetIP, nlLinkSetAlias
	t.Cleanup(func() {
		nlLinkByName, nlEndpointLinkByName, nlLinkAdd, nlLinkSetUp, nlLinkSetHardwareAddr = prevBy, prevEp, prevAdd, prevUp, prevMAC
		nlLinkSetMaster, nlLinkDel, nlHandleLinkSetMTU, dhcpGetIP, nlLinkSetAlias = prevMaster, prevDel, prevMTU, prevGet, prevAlias
	})
	byName := func(name string) (netlink.Link, error) {
		if err := k.step("byname", name); err != nil {
			return nil, err
		}
		if l, ok := k.links[name]; ok {
			return l, nil
		}
		return nil, errors.New("Link not found")
	}
	nlLinkByName, nlEndpointLinkByName = byName, byName
	nlLinkAdd = func(l netlink.Link) error {
		if err := k.step("add", l.Attrs().Name); err != nil {
			return err
		}
		made := []netlink.Link{l}
		mac := l.Attrs().HardwareAddr
		if v, ok := l.(*netlink.Veth); ok {
			mac = v.PeerHardwareAddr
			made = append(made, &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: v.PeerName}})
		}
		if _, ok := l.(*netlink.IPVlan); ok {
			mac = epParentMAC
		}
		if len(mac) == 0 && !k.noMAC {
			mac = epKernelMAC
		}
		fresh := made[len(made)-1]
		fresh.Attrs().HardwareAddr = mac
		for _, m := range made {
			k.links[m.Attrs().Name] = m
		}
		return nil
	}
	nlLinkSetUp = func(l netlink.Link) error { return k.step("up", l.Attrs().Name) }
	nlLinkSetHardwareAddr = func(l netlink.Link, mac net.HardwareAddr) error {
		k.macs[l.Attrs().Name] = mac
		return k.step("mac", l.Attrs().Name)
	}
	nlLinkSetMaster = func(l, master netlink.Link) error { return k.step("master", l.Attrs().Name+"->"+master.Attrs().Name) }
	nlLinkSetAlias = func(netlink.Link, string) error { return nil }
	nlHandleLinkSetMTU = func(_ *netlink.Handle, l netlink.Link, _ int) error { return k.step("mtu", l.Attrs().Name) }
	nlLinkDel = func(l netlink.Link) error {
		k.calls = append(k.calls, "del:"+l.Attrs().Name)
		k.deleted = append(k.deleted, l.Attrs().Name)
		if k.delErr != nil {
			return k.delErr
		}
		delete(k.links, l.Attrs().Name)
		return nil
	}
	dhcpGetIP = func(_ context.Context, iface string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		if o.V6 {
			k.calls = append(k.calls, "dhcp6:"+iface)
			return k.v6, dhcp.RAObservation{}, k.v6Err
		}
		k.calls = append(k.calls, "dhcp4:"+iface)
		return k.v4, dhcp.RAObservation{}, k.v4Err
	}
	return k
}

// endpointTestPlugin answers the hostname lookup at once, which otherwise polls Docker for two seconds (#46).
func endpointTestPlugin(t *testing.T, opts DHCPNetworkOptions, ep string) *Plugin {
	t.Helper()
	withStateDir(t, t.TempDir())
	if err := saveOptions(epNet, opts); err != nil {
		t.Fatal(err)
	}
	p := newPluginForTest()
	p.docker = &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{epNet: {Containers: map[string]dNetwork.EndpointResource{
			"ctr657": {EndpointID: ep},
		}}},
		containerResult: map[string]dContainer.InspectResponse{"ctr657": {Config: &dContainer.Config{Hostname: "web657"}}},
	}
	return p
}

func createTestEndpoint(p *Plugin, ep, mac string) (CreateEndpointResponse, error) {
	return p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: epNet, EndpointID: ep, Interface: &EndpointInterface{MacAddress: mac}})
}

func TestCreateEndpoint_BridgeHappyPathPinsAndAttaches(t *testing.T) {
	k := stubEndpointKernel(t)
	ep := strings.Repeat("a", 64)
	p := endpointTestPlugin(t, DHCPNetworkOptions{Bridge: epBridge, MTU: 1400, Gateway: "192.0.2.254"}, ep)
	host, ctr := vethPairNames(ep)

	res, err := createTestEndpoint(p, ep, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	want := []string{"byname:" + epBridge, "add:" + host, "mtu:" + host, "mtu:" + ctr, "up:" + host, "byname:" + ctr,
		"up:" + ctr, "mac:" + ctr, "master:" + host + "->" + epBridge, "dhcp4:" + ctr}
	if strings.Join(k.calls, " ") != strings.Join(want, " ") {
		t.Errorf("calls %v\nwant  %v", k.calls, want)
	}
	if res.Interface.MacAddress != epKernelMAC.String() || k.macs[ctr].String() != epKernelMAC.String() {
		t.Errorf("reported MAC %q, pinned %v; want both %v", res.Interface.MacAddress, k.macs[ctr], epKernelMAC)
	}
	if res.Interface.Address != "192.0.2.57/24" || len(k.deleted) != 0 {
		t.Errorf("address %q, deleted %v; want the lease and nothing deleted", res.Interface.Address, k.deleted)
	}
	hint, ok := p.takeJoinHint(ep)
	if !ok || hint.Gateway != "192.0.2.254" || hint.MacAddress.String() != epKernelMAC.String() {
		t.Errorf("hint %+v ok=%v; want the gateway override and the pinned MAC", hint, ok)
	}
}

func TestCreateEndpoint_BridgeUserMACIsNeitherPinnedNorReported(t *testing.T) {
	k := stubEndpointKernel(t)
	ep := strings.Repeat("b", 64)
	p := endpointTestPlugin(t, DHCPNetworkOptions{Bridge: epBridge, IPv6: true}, ep)
	_, ctr := vethPairNames(ep)

	res, err := createTestEndpoint(p, ep, "02:42:0a:00:06:57")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if k.saw("mac:"+ctr) || res.Interface.MacAddress != "" {
		t.Errorf("calls %v, reported %q; a user MAC is set at create and reported back by libnetwork", k.calls, res.Interface.MacAddress)
	}
	if k.links[ctr].Attrs().HardwareAddr.String() != "02:42:0a:00:06:57" || !k.saw("dhcp6:"+ctr) {
		t.Errorf("ctr MAC %v, calls %v; want the user MAC at create and a DHCPv6 attempt", k.links[ctr].Attrs().HardwareAddr, k.calls)
	}
	if res.Interface.AddressIPv6 != "2001:db8::57/128" {
		t.Errorf("AddressIPv6 %q, want the DHCPv6 lease", res.Interface.AddressIPv6)
	}
}

func TestCreateEndpoint_BridgeTakesATombstone(t *testing.T) {
	k := stubEndpointKernel(t)
	ep := strings.Repeat("c", 64)
	p := endpointTestPlugin(t, DHCPNetworkOptions{Bridge: epBridge}, ep)
	p.addTombstone(epNet, "web657", "02:42:0a:00:06:58", "192.0.2.58", "2001:db8::58")
	_, ctr := vethPairNames(ep)

	var requested string
	get := dhcpGetIP
	dhcpGetIP = func(ctx context.Context, iface string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		requested = o.RequestedIP
		return get(ctx, iface, o)
	}
	res, err := createTestEndpoint(p, ep, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if got := k.links[ctr].Attrs().HardwareAddr.String(); got != "02:42:0a:00:06:58" || requested != "192.0.2.58" || k.saw("mac:"+ctr) {
		t.Errorf("ctr MAC %s, requested %q, calls %v; want the tombstone's MAC at create and its address requested", got, requested, k.calls)
	}
	if res.Interface.MacAddress != "02:42:0a:00:06:58" {
		t.Errorf("reported MAC %q, want the inherited one, which libnetwork did not send", res.Interface.MacAddress)
	}
}

func TestCreateEndpoint_BridgeFailuresBeforeTheVethMakeNothing(t *testing.T) {
	for _, c := range []struct {
		name, want string
		req        func(*CreateEndpointRequest)
		kernel     func(*endpointKernel)
	}{
		{"bad mac", "MAC", func(r *CreateEndpointRequest) { r.Interface.MacAddress = "zz" }, nil},
		{"bad --ip", "", func(r *CreateEndpointRequest) { r.Interface.Address = "not-an-ip" }, nil},
		{"bad --ip6", "", func(r *CreateEndpointRequest) { r.Interface.AddressIPv6 = "not-an-ip" }, nil},
		{"bad ifname", "", func(r *CreateEndpointRequest) {
			r.Options = map[string]interface{}{"com.docker.network.endpoint.ifname": 7}
		}, nil},
		{"the veth add", "failed to create veth pair", func(*CreateEndpointRequest) {}, func(k *endpointKernel) {
			k.fail["add:dh-"+strings.Repeat("d", 12)] = errInjected
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := stubEndpointKernel(t)
			if c.kernel != nil {
				c.kernel(k)
			}
			p := endpointTestPlugin(t, DHCPNetworkOptions{Bridge: epBridge}, strings.Repeat("d", 64))
			r := CreateEndpointRequest{NetworkID: epNet, EndpointID: strings.Repeat("d", 64), Interface: &EndpointInterface{}}
			c.req(&r)
			_, err := p.CreateEndpoint(context.Background(), r)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %q", err, c.want)
			}
			if len(k.deleted) != 0 || len(k.links) != 2 {
				t.Errorf("deleted %v, links %d; a create that made nothing deletes nothing", k.deleted, len(k.links))
			}
		})
	}
}

// Every error after the veth add removes the pair through its host end, whose delete takes the peer (#657).
func TestCreateEndpoint_BridgeRollsBackTheVethOnEveryLaterFailure(t *testing.T) {
	ep := strings.Repeat("e", 64)
	host, ctr := vethPairNames(ep)
	for _, c := range []struct {
		name, want string
		opts       DHCPNetworkOptions
		kernel     func(*endpointKernel)
	}{
		{"mtu", "failed to set mtu=1400", DHCPNetworkOptions{MTU: 1400}, func(k *endpointKernel) { k.fail["mtu:"+host] = errInjected }},
		{"host up", "failed to set host side link", DHCPNetworkOptions{}, func(k *endpointKernel) { k.fail["up:"+host] = errInjected }},
		{"peer lookup", "failed to find container side", DHCPNetworkOptions{}, func(k *endpointKernel) { k.fail["byname:"+ctr] = errInjected }},
		{"peer up", "failed to set container side link", DHCPNetworkOptions{}, func(k *endpointKernel) { k.fail["up:"+ctr] = errInjected }},
		{"mac pin", "MAC address", DHCPNetworkOptions{}, func(k *endpointKernel) { k.fail["mac:"+ctr] = errInjected }},
		{"bridge attach", "failed to attach host side link", DHCPNetworkOptions{}, func(k *endpointKernel) {
			k.fail["master:"+host+"->"+epBridge] = errInjected
		}},
		{"dhcpv6 identity", "too short", DHCPNetworkOptions{IPv6: true}, func(k *endpointKernel) { k.noMAC = true }},
		{"server policy", "dhcp_servers", DHCPNetworkOptions{DHCPServers: "not-an-ip"}, nil},
		{"conflict check", "conflict_check", DHCPNetworkOptions{ConflictCheck: "sometimes"}, nil},
		{"dhcpv4", "failed to get initial IP address", DHCPNetworkOptions{}, func(k *endpointKernel) { k.v4Err = errInjected }},
		{"dhcpv4 address", "failed to parse initial IP address", DHCPNetworkOptions{}, func(k *endpointKernel) { k.v4.IP = "garbage" }},
		{"dhcpv6 address", "failed to parse initial IPv6 address", DHCPNetworkOptions{IPv6: true}, func(k *endpointKernel) { k.v6.IP = "garbage" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := stubEndpointKernel(t)
			if c.kernel != nil {
				c.kernel(k)
			}
			opts := c.opts
			opts.Bridge = epBridge
			id := ep
			if k.noMAC {
				id = "e657"
				host, _ = vethPairNames(id)
			}
			p := endpointTestPlugin(t, opts, id)
			_, err := createTestEndpoint(p, id, "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %q", err, c.want)
			}
			if !k.saw("add:"+host) || strings.Join(k.deleted, " ") != host {
				t.Errorf("calls %v, deleted %v; want the veth made and then deleted once by its host end %s", k.calls, k.deleted, host)
			}
			if _, ok := p.takeJoinHint(id); ok {
				t.Error("a failed create left its Join hint")
			}
			host, _ = vethPairNames(ep)
		})
	}
}

func warnsNaming(hook *logtest.Hook, link string) bool {
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "ip link del") && e.Data["link"] == link {
			return true
		}
	}
	return false
}

// A rollback whose delete fails leaves the link on the host; the IPAM paths warn with the command that removes it (#657).
func TestCreateEndpoint_BridgeRollbackWarnsWhenTheDeleteFails(t *testing.T) {
	k := stubEndpointKernel(t)
	ep := strings.Repeat("f", 64)
	host, _ := vethPairNames(ep)
	k.fail["master:"+host+"->"+epBridge] = errInjected
	k.delErr = errors.New("injected delete failure")
	p := endpointTestPlugin(t, DHCPNetworkOptions{Bridge: epBridge}, ep)
	hook := logtest.NewGlobal()
	defer hook.Reset()

	_, err := createTestEndpoint(p, ep, "")
	if !errors.Is(err, errInjected) {
		t.Errorf("err %v, want the attach failure, not the delete's", err)
	}
	if !warnsNaming(hook, host) {
		t.Errorf("no Warn naming `ip link del` and link=%s after a failed rollback delete; the veth stays on the host unreported", host)
	}
}

func TestCreateParentAttachedEndpoint_HappyPaths(t *testing.T) {
	ep := strings.Repeat("1", 64)
	child := subLinkName(ep)
	for _, c := range []struct {
		name    string
		opts    DHCPNetworkOptions
		pinned  net.HardwareAddr
		report  string
		calls   string
		tombIP  string
		userMAC string
	}{
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, MTU: 1400, Gateway: "192.0.2.254"}, epKernelMAC, epKernelMAC.String(),
			"byname:" + epParent + " add:" + child + " byname:" + child + " mtu:" + child + " mac:" + child + " up:" + child + " dhcp4:" + child, "", ""},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, IPv6: true}, nil, "",
			"byname:" + epParent + " add:" + child + " byname:" + child + " up:" + child + " dhcp4:" + child + " dhcp6:" + child, "", ""},
		{"passthru", DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "passthru"}, epParentMAC, "",
			"byname:" + epParent + " add:" + child + " byname:" + child + " mac:" + child + " up:" + child + " dhcp4:" + child, "", ""},
		{"macvlan tombstone", DHCPNetworkOptions{Mode: ModeMacvlan}, nil, "02:42:0a:00:06:59",
			"byname:" + epParent + " add:" + child + " byname:" + child + " up:" + child + " dhcp4:" + child, "192.0.2.59", ""},
		{"macvlan user mac", DHCPNetworkOptions{Mode: ModeMacvlan}, nil, "",
			"byname:" + epParent + " add:" + child + " byname:" + child + " up:" + child + " dhcp4:" + child, "", "02:42:0a:00:06:60"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := stubEndpointKernel(t)
			opts := c.opts
			opts.Parent = epParent
			p := endpointTestPlugin(t, opts, ep)
			if c.tombIP != "" {
				p.addTombstone(epNet, "web657", c.report, c.tombIP, "")
			}
			var requested string
			get := dhcpGetIP
			dhcpGetIP = func(ctx context.Context, iface string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
				if !o.V6 {
					requested = o.RequestedIP
				}
				return get(ctx, iface, o)
			}
			res, err := createTestEndpoint(p, ep, c.userMAC)
			if err != nil {
				t.Fatalf("createParentAttachedEndpoint: %v", err)
			}
			if got := strings.Join(k.calls, " "); got != c.calls {
				t.Errorf("calls %s\nwant  %s", got, c.calls)
			}
			if c.pinned.String() != k.macs[child].String() || res.Interface.MacAddress != c.report || requested != c.tombIP {
				t.Errorf("pinned %v reported %q requested %q; want %v, %q, %q", k.macs[child], res.Interface.MacAddress, requested,
					c.pinned, c.report, c.tombIP)
			}
			if res.Interface.Address != "192.0.2.57/24" || len(k.deleted) != 0 {
				t.Errorf("address %q, deleted %v; want the lease and nothing deleted", res.Interface.Address, k.deleted)
			}
			gw := c.opts.Gateway
			if gw == "" {
				gw = k.v4.Gateway
			}
			if hint, _ := p.takeJoinHint(ep); hint.Gateway != gw {
				t.Errorf("hint gateway %q, want %q", hint.Gateway, gw)
			}
		})
	}
}

func TestCreateParentAttachedEndpoint_FailuresBeforeTheChildMakeNothing(t *testing.T) {
	ep := strings.Repeat("2", 64)
	for _, c := range []struct {
		name, want, mac string
		opts            DHCPNetworkOptions
		req             func(*CreateEndpointRequest)
		kernel          func(*endpointKernel)
	}{
		{"ipvlan user mac", "ipvlan does not support a custom MAC", "02:42:0a:00:06:61", DHCPNetworkOptions{Mode: ModeIPvlan}, nil, nil},
		{"bad mac", "MAC", "zz", DHCPNetworkOptions{Mode: ModeMacvlan}, nil, nil},
		{"bad --ip", "", "", DHCPNetworkOptions{Mode: ModeMacvlan}, func(r *CreateEndpointRequest) { r.Interface.Address = "not-an-ip" }, nil},
		{"bad --ip6", "", "", DHCPNetworkOptions{Mode: ModeMacvlan}, func(r *CreateEndpointRequest) { r.Interface.AddressIPv6 = "not-an-ip" }, nil},
		{"bad vlan", "vlan", "", DHCPNetworkOptions{Mode: ModeMacvlan, Vlan: "x"}, nil, nil},
		{"bad sub-mode", "macvlan_mode", "", DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "sometimes"}, nil, nil},
		{"parent lookup", "failed to lookup parent", "", DHCPNetworkOptions{Mode: ModeMacvlan}, nil, func(k *endpointKernel) {
			k.fail["byname:"+epParent] = errInjected
		}},
		{"the child add", errInjected.Error(), "", DHCPNetworkOptions{Mode: ModeMacvlan}, nil, func(k *endpointKernel) {
			k.fail["add:"+subLinkName(ep)] = errInjected
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := stubEndpointKernel(t)
			if c.kernel != nil {
				c.kernel(k)
			}
			opts := c.opts
			opts.Parent = epParent
			p := endpointTestPlugin(t, opts, ep)
			r := CreateEndpointRequest{NetworkID: epNet, EndpointID: ep, Interface: &EndpointInterface{MacAddress: c.mac}}
			if c.req != nil {
				c.req(&r)
			}
			_, err := p.createParentAttachedEndpoint(context.Background(), time.Now(), r, opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %q", err, c.want)
			}
			if len(k.deleted) != 0 || len(k.links) != 2 {
				t.Errorf("deleted %v, links %d; a create that made nothing deletes nothing", k.deleted, len(k.links))
			}
		})
	}
}

func TestCreateParentAttachedEndpoint_RollsBackTheChildOnEveryLaterFailure(t *testing.T) {
	ep := strings.Repeat("3", 64)
	child := subLinkName(ep)
	for _, c := range []struct {
		name, want, id string
		opts           DHCPNetworkOptions
		kernel         func(*endpointKernel)
	}{
		{"re-fetch", "failed to re-fetch macvlan link", ep, DHCPNetworkOptions{Mode: ModeMacvlan}, func(k *endpointKernel) {
			k.fail["byname:"+child] = errInjected
		}},
		{"mtu", "failed to set mtu=1400", ep, DHCPNetworkOptions{Mode: ModeMacvlan, MTU: 1400}, func(k *endpointKernel) { k.fail["mtu:"+child] = errInjected }},
		{"mac pin", "failed to pin macvlan link MAC", ep, DHCPNetworkOptions{Mode: ModeMacvlan}, func(k *endpointKernel) { k.fail["mac:"+child] = errInjected }},
		{"link up", "failed to set ipvlan link up", ep, DHCPNetworkOptions{Mode: ModeIPvlan}, func(k *endpointKernel) { k.fail["up:"+child] = errInjected }},
		{"dhcpv6 identity", "too short", "e657", DHCPNetworkOptions{Mode: ModeIPvlan, IPv6: true}, nil},
		{"server policy", "dhcp_servers", ep, DHCPNetworkOptions{Mode: ModeMacvlan, DHCPServers: "not-an-ip"}, nil},
		{"conflict check", "conflict_check", ep, DHCPNetworkOptions{Mode: ModeMacvlan, ConflictCheck: "sometimes"}, nil},
		{"dhcpv4", "failed to get initial IP address", ep, DHCPNetworkOptions{Mode: ModeMacvlan}, func(k *endpointKernel) { k.v4Err = errInjected }},
		{"dhcpv4 address", "failed to parse initial IP address", ep, DHCPNetworkOptions{Mode: ModeMacvlan}, func(k *endpointKernel) { k.v4.IP = "garbage" }},
		{"dhcpv6 address", "failed to parse initial IPv6 address", ep, DHCPNetworkOptions{Mode: ModeMacvlan, IPv6: true}, func(k *endpointKernel) {
			k.v6.IP = "garbage"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := stubEndpointKernel(t)
			if c.kernel != nil {
				c.kernel(k)
			}
			opts := c.opts
			opts.Parent = epParent
			p := endpointTestPlugin(t, opts, c.id)
			_, err := createTestEndpoint(p, c.id, "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want one naming %q", err, c.want)
			}
			made := subLinkName(c.id)
			if !k.saw("add:"+made) || strings.Join(k.deleted, " ") != made {
				t.Errorf("calls %v, deleted %v; want the child made and then deleted once, %s", k.calls, k.deleted, made)
			}
		})
	}
}

// The child is still in the host namespace here: the engine moves it only after CreateEndpoint returns (#657).
func TestCreateParentAttachedEndpoint_RollbackWarnsWhenTheDeleteFails(t *testing.T) {
	k := stubEndpointKernel(t)
	ep := strings.Repeat("4", 64)
	k.v4Err = errInjected
	k.delErr = errors.New("injected delete failure")
	p := endpointTestPlugin(t, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: epParent}, ep)
	hook := logtest.NewGlobal()
	defer hook.Reset()

	_, err := createTestEndpoint(p, ep, "")
	if !errors.Is(err, errInjected) {
		t.Errorf("err %v, want the lease failure, not the delete's", err)
	}
	if !warnsNaming(hook, subLinkName(ep)) {
		t.Errorf("no Warn naming `ip link del` and link=%s after a failed rollback delete; the child stays on the host unreported", subLinkName(ep))
	}
}

func TestIPAMReserveLink_MacvlanSetUpFailureWarnsWhenTheDeleteFails(t *testing.T) {
	k := stubEndpointKernel(t)
	const name = "dh-657-ra"
	k.fail["up:"+name] = errInjected
	k.delErr = errors.New("injected delete failure")
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	hook := logtest.NewGlobal()
	defer hook.Reset()

	_, err := p.addIPAMReserveLink(context.Background(), name, "dh-657-rb", ModeMacvlan,
		DHCPNetworkOptions{Mode: ModeMacvlan, Parent: epParent}, nil)
	if !errors.Is(err, errInjected) || strings.Join(k.deleted, " ") != name {
		t.Errorf("err %v, deleted %v; want the set-up failure and one delete of %s", err, k.deleted, name)
	}
	if !warnsNaming(hook, name) {
		t.Errorf("no Warn naming `ip link del` and link=%s after a failed delete; the reservation link stays on the parent", name)
	}
}

// A failed create hands the tombstone back, or the retry after one transient failure gets a new MAC and address;
// a successful one keeps it consumed (#657).
func TestCreateEndpoint_FailedCreateKeepsTheTombstone(t *testing.T) {
	for _, c := range []struct {
		name string
		opts DHCPNetworkOptions
		fail func(k *endpointKernel, ep string)
	}{
		{"bridge, link add", DHCPNetworkOptions{Bridge: epBridge}, func(k *endpointKernel, ep string) {
			host, _ := vethPairNames(ep)
			k.fail["add:"+host] = errInjected
		}},
		{"bridge, dhcpv4", DHCPNetworkOptions{Bridge: epBridge}, func(k *endpointKernel, _ string) { k.v4Err = errInjected }},
		{"macvlan, link add", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: epParent}, func(k *endpointKernel, ep string) {
			k.fail["add:"+subLinkName(ep)] = errInjected
		}},
		{"macvlan, dhcpv4", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: epParent}, func(k *endpointKernel, _ string) { k.v4Err = errInjected }},
		{"bridge, success", DHCPNetworkOptions{Bridge: epBridge}, nil},
		{"macvlan, success", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: epParent}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := stubEndpointKernel(t)
			ep := strings.Repeat("d", 64)
			p := endpointTestPlugin(t, c.opts, ep)
			p.addTombstone(epNet, "web657", "02:42:0a:00:06:59", "192.0.2.59", "2001:db8::59")
			if c.fail == nil {
				if _, err := createTestEndpoint(p, ep, ""); err != nil {
					t.Fatalf("CreateEndpoint: %v", err)
				}
				if _, _, _, ok := p.tombstones.consume(epNet, "web657"); ok {
					t.Error("tombstone still there after a successful create; the next restart would inherit it twice")
				}
				return
			}
			c.fail(k, ep)
			if _, err := createTestEndpoint(p, ep, ""); err == nil || !strings.Contains(err.Error(), errInjected.Error()) {
				t.Fatalf("CreateEndpoint error %v, want the injected one", err)
			}
			mac, ip, ip6, ok := p.tombstones.consume(epNet, "web657")
			if !ok || mac != "02:42:0a:00:06:59" || ip != "192.0.2.59" || ip6 != "2001:db8::59" {
				t.Errorf("tombstone after the failed create: %q %q %q %v; want it handed back for the retry", mac, ip, ip6, ok)
			}
		})
	}
}
