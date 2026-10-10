// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	dNetwork "github.com/moby/moby/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func ipv4OffRequest(generic map[string]interface{}, engineFlag interface{}) CreateNetworkRequest {
	r := CreateNetworkRequest{NetworkID: mtuTestNetwork, Options: map[string]interface{}{}}
	if generic != nil {
		r.Options[util.OptionsKeyGeneric] = generic
	}
	if engineFlag != nil {
		r.Options[engineEnableIPv4Key] = engineFlag
	}
	return r
}

func TestEngineIPv4Off_ReadsTheMobyShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    map[string]interface{}
		off     bool
		wantErr bool
	}{
		{"absent means on", map[string]interface{}{}, false, false},
		{"true means on", map[string]interface{}{engineEnableIPv4Key: true}, false, false},
		{"false means off", map[string]interface{}{engineEnableIPv4Key: false}, true, false},
		{"string false", map[string]interface{}{engineEnableIPv4Key: "false"}, true, false},
		{"string true", map[string]interface{}{engineEnableIPv4Key: "true"}, false, false},
		{"garbage is refused", map[string]interface{}{engineEnableIPv4Key: "maybe"}, false, true},
		{"a number is refused", map[string]interface{}{engineEnableIPv4Key: 0.0}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off, err := engineIPv4Off(tc.opts)
			if (err != nil) != tc.wantErr || off != tc.off {
				t.Fatalf("engineIPv4Off = %v, %v; want %v, error %v", off, err, tc.off, tc.wantErr)
			}
		})
	}
}

func TestCreateNetwork_IPv4OffIsStoredAndTheOptionFormIsRefused(t *testing.T) {
	type tc struct {
		name   string
		opts   map[string]interface{}
		flag   interface{}
		wantIn []string // nil means accepted
	}
	for _, c := range []tc{
		{"off with dhcp is stored", map[string]interface{}{"ipv6_mode": "dhcp"}, false, nil},
		{"off with slaac is stored", map[string]interface{}{"ipv6_mode": "slaac"}, false, nil},
		{"on is the default", map[string]interface{}{}, nil, nil},
		{"off with ipv6_mode off is refused", map[string]interface{}{}, false, []string{"ipv6_mode=", "--ipv4=false"}},
		{"off beside ipv6=true is stored", map[string]interface{}{"ipv6": "true"}, false, nil},
		{"the engine key under -o is refused", map[string]interface{}{engineEnableIPv4Key: "false", "ipv6_mode": "dhcp"}, nil, []string{"--ipv4=false", "Docker 28"}},
		{"ipv4_off under -o is refused", map[string]interface{}{"ipv4_off": "true", "ipv6_mode": "dhcp"}, nil, []string{"--ipv4=false"}},
		{"ipv4 under -o is refused", map[string]interface{}{"IPv4": "false", "ipv6_mode": "dhcp"}, nil, []string{"--ipv4=false"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			generic := map[string]interface{}{"bridge": mtuTestBridge}
			for k, v := range c.opts {
				generic[k] = v
			}
			err := p.CreateNetwork(ipv4OffRequest(generic, c.flag))
			if c.wantIn != nil {
				if err == nil {
					t.Fatalf("accepted")
				}
				for _, n := range c.wantIn {
					if !strings.Contains(err.Error(), n) {
						t.Errorf("the refusal %q does not name %q", err, n)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			stored, err := loadOptions(mtuTestNetwork)
			if err != nil {
				t.Fatalf("loadOptions: %v", err)
			}
			if want := c.flag == false; stored.IPv4Off != want || stored.ipv4Enabled() == want {
				t.Errorf("stored IPv4Off = %v, want %v", stored.IPv4Off, want)
			}
		})
	}
}

func TestCreateNetwork_IPv4OffOnIpvlanNamesDHCPWhenSLAACIsAsked(t *testing.T) {
	withStateDir(t, t.TempDir())
	(&mtuKernel{parentMTU: 1500}).install(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	generic := map[string]interface{}{"mode": ModeIPvlan, "parent": mtuTestParent, "ipv6_mode": "slaac"}
	err := p.CreateNetwork(ipv4OffRequest(generic, false))
	if err == nil || !strings.Contains(err.Error(), "ipv6_mode=dhcp") {
		t.Fatalf("ipvlan with v4 off and slaac: %v; want a refusal that names ipv6_mode=dhcp", err)
	}
	generic["ipv6_mode"] = "dhcp"
	if err := p.CreateNetwork(ipv4OffRequest(generic, false)); err != nil {
		t.Fatalf("ipvlan with v4 off and dhcp was refused: %v", err)
	}
}

func TestRefuseExplicitV4_OnlyOnAV4OffNetwork(t *testing.T) {
	off := DHCPNetworkOptions{IPv4Off: true, IPv6: true}
	if err := refuseExplicitV4(off, "10.1.2.3/24"); err == nil || !strings.Contains(err.Error(), "--ip") {
		t.Errorf("an explicit address on a v4-off network: %v; want a refusal naming --ip", err)
	}
	if err := refuseExplicitV4(off, ""); err != nil {
		t.Errorf("no address on a v4-off network was refused: %v", err)
	}
	if err := refuseExplicitV4(DHCPNetworkOptions{}, "10.1.2.3/24"); err != nil {
		t.Errorf("an explicit address on an ordinary network was refused: %v", err)
	}
}

func TestJoin_AV4OffEndpointGetsNoV4GatewayAndNoGatewayBridge(t *testing.T) {
	withStateDir(t, t.TempDir())
	_, dst, _ := net.ParseCIDR("10.88.0.0/16")
	stubKernelRouteTable(t, []netlink.Route{
		{Dst: netlinkDefaultDst(false), Gw: net.ParseIP("192.168.99.1")},
		{Dst: dst, Gw: net.ParseIP("192.168.99.253")},
	}, nil, nil)
	if err := saveOptions("n1135", DHCPNetworkOptions{Bridge: "lo", IPv4Off: true, IPv6: true}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	mac, _ := net.ParseMAC("02:42:0a:00:00:09")
	for _, tc := range []struct {
		name        string
		gwV6        string
		engine28    bool
		wantNoGWSvc bool
	}{
		{"no v6 gateway either", "", true, true},
		{"an engine that leaves the v6 gateway to the plugin", "fe80::1", false, true},
		{"a v6 gateway keeps the engine's hands off already", "fe80::1", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
				joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
			if tc.engine28 {
				onWaitingEngine(p)
			}
			// A hint carrying the v4 half a stale or hostile record could leave must not reach the answer.
			p.storeJoinHint("e1135", joinHint{MacAddress: mac, GatewayIPv6: tc.gwV6, Gateway: "192.168.99.1",
				Routes: []*StaticRoute{{Destination: "10.77.0.0/16", NextHop: "192.168.99.254"}}})
			res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n1135", EndpointID: "e1135"})
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			p.mu.Lock()
			m := p.persistentDHCP["e1135"]
			p.mu.Unlock()
			if m != nil {
				defer abortJoinAttach(t, p, "e1135", m)
			}
			if res.Gateway != "" {
				t.Errorf("Join named the v4 gateway %q on a network with no IPv4", res.Gateway)
			}
			for _, r := range res.StaticRoutes {
				if strings.Contains(r.Destination, ".") {
					t.Errorf("Join returned the v4 route %v", describeStaticRoutes(res.StaticRoutes))
				}
			}
			if res.DisableGatewayService != tc.wantNoGWSvc {
				t.Errorf("DisableGatewayService = %v, want %v", res.DisableGatewayService, tc.wantNoGWSvc)
			}
		})
	}
}

func TestUnboundState_AV4OffEndpointReportsIPv4OffWithNoAddress(t *testing.T) {
	m := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{IPv4Off: true, IPv6: true})
	if state, addr := m.unboundState(); state != "ipv4_off" || addr != "" {
		t.Errorf("unboundState = %q, %q; want ipv4_off and no address", state, addr)
	}
}

func TestStop_AV4OffManagerHasNoV4ClientToDrain(t *testing.T) {
	p := &Plugin{}
	m := stoppingManager(t, p, DHCPNetworkOptions{IPv4Off: true, IPv6: true}, nil, nil)
	m.errChan = nil // a v4-off manager never starts the v4 client; a drain of it would block forever
	m.boundV4.Store(false)
	done := make(chan error, 1)
	go func() { done <- m.stop(true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop blocks on the v4 client a v4-off manager never started")
	}
}

func TestNetOptions_TheDockerFallbackRecoversTheV4OffFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		engine  string
		enabled bool
		corrupt bool
		off     bool
	}{
		{"absent file, engine 28 reports IPv4 off", "28.5.2", false, false, true},
		{"corrupt file, engine 28 reports IPv4 off", "28.5.2", false, true, true},
		{"engine 28 reports IPv4 on", "28.5.2", true, false, false},
		{"engine 27 does not report the field", "27.5.1", false, false, false},
		{"no engine answered", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			withStateDir(t, dir)
			if tc.corrupt {
				if err := os.WriteFile(dir+"/n1.json", []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := &Plugin{docker: &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
				"n1": {ID: "n1", Driver: testDHCPDriver, EnableIPv4: tc.enabled, Options: map[string]string{"bridge": "br9"}},
			}}}
			if tc.engine != "" {
				p.engine.Store(&engineIdentity{Version: tc.engine, APIVersion: "1.51"})
			}
			got, err := p.netOptions(context.Background(), "n1")
			if err != nil {
				t.Fatalf("netOptions: %v", err)
			}
			if got.IPv4Off != tc.off {
				t.Errorf("IPv4Off = %v, want %v", got.IPv4Off, tc.off)
			}
			if !tc.corrupt {
				if stored, err := loadOptions("n1"); err != nil || stored.IPv4Off != tc.off {
					t.Errorf("the backfilled file reads IPv4Off = %v, %v; want %v", stored.IPv4Off, err, tc.off)
				}
			}
		})
	}
}

func TestCreateNetwork_AV4OffNetworkSkipsTheDHCPv4Probe(t *testing.T) {
	withStateDir(t, t.TempDir())
	(&mtuKernel{parentMTU: 1500}).install(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	generic := map[string]interface{}{"mode": ModeMacvlan, "parent": mtuTestParent, "ipv6_mode": "dhcp", "validate_dhcp": "true"}
	if err := p.CreateNetwork(ipv4OffRequest(generic, false)); err != nil {
		t.Fatalf("the v4 probe ran against a network with no IPv4: %v", err)
	}
}

func TestNameTheRunningClient_AV4OffManagerHasNoV4ClientToName(t *testing.T) {
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{IPv4Off: true, IPv6: true}).withPlugin(p)
	name := "web"
	if !m.nameTheRunningClient(&joinPhases{}, func() error { return nil }, &name) {
		t.Error("nameTheRunningClient reported the daemon silent")
	}
	if got := p.hostnameApplyFailures.Load(); got != 0 {
		t.Errorf("hostname_apply_failures = %d: the manager looked for a v4 client it never starts", got)
	}
}

func TestCreateEndpoint_IPv4OffRefusesAnExplicitAddress(t *testing.T) {
	withStateDir(t, t.TempDir())
	(&mtuKernel{parentMTU: 1500}).install(t)
	const network = "net-ipv4-off-ip"
	if err := saveOptions(network, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: mtuTestParent, IPv4Off: true, IPv6: true, IPv6Mode: "dhcp"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: network, EndpointID: "ep-ipv4-off-ip",
		Interface: &EndpointInterface{Address: "10.1.2.3/24"},
	})
	if err == nil || !strings.Contains(err.Error(), "--ip") {
		t.Errorf("--ip on a v4-off network: %v; want a refusal naming --ip", err)
	}
}

func TestReleaseLease_AV4OffEndpointHasNoV4LeaseToHandBack(t *testing.T) {
	p := &Plugin{}
	installSender(t, nil)
	m := releasingManager(t, p, ReleaseOnStop, true)
	m.opts.IPv4Off = true

	releasedV4, _ := m.releaseHeldLeases()
	if releasedV4 {
		t.Error("a v4 release was reported on a network with no DHCPv4")
	}
	if got := p.releasesSentV4.Load() + p.releaseFailuresV4.Load(); got != 0 {
		t.Errorf("the v4 release counters moved by %d on a v4-off network", got)
	}
}

func TestStart_AV4OffNetworkOpensOnlyTheV6Client(t *testing.T) {
	r := newStartRig(t)
	r.m.opts.IPv4Off = true
	// The engine renames the link after moving it; with no v4 client, the v6 set-up is the one that keeps the new name.
	prevByIndex := nlLinkByIndex
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: index, Name: "eth0", Flags: net.FlagUp}}, nil
	}
	t.Cleanup(func() { nlLinkByIndex = prevByIndex })
	if err := r.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = within(t, "Stop", r.stopAsync()) })

	if r.m.ctrLink == nil || r.m.ctrLink.Attrs().Name != "eth0" {
		t.Errorf("the manager kept link %v, want the re-read one named eth0 (#1135)", r.m.ctrLink)
	}
	if got := strings.Join(r.order, ", "); got != "sandbox prep, v6 client" {
		t.Errorf("set-up order = %q, want the link prepared for v6 and no v4 client", got)
	}
	if r.m.errChan != nil {
		t.Error("a v4-off manager holds a v4 exit channel")
	}
	if r.m.ctrLink == nil {
		t.Error("the manager lost the link the v6 client was opened on")
	}
}

func TestStart_AV4OffNetworkWhoseV6SetupFailsDoesNotWaitOnAV4Client(t *testing.T) {
	r := newStartRig(t)
	r.m.opts.IPv4Off = true
	r.failV6 = errors.New("no v6 socket")
	done := make(chan error, 1)
	go func() { done <- r.start(t) }()
	if err := within(t, "Start", done); err == nil || !strings.Contains(err.Error(), "DHCPv6") {
		t.Errorf("Start error = %v, want the v6 client failure", err)
	}
}
