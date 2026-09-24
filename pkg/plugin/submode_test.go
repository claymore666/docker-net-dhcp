// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func TestParseMacvlanMode_EveryValueAndTheDefault(t *testing.T) {
	for in, want := range map[string]netlink.MacvlanMode{
		"":         netlink.MACVLAN_MODE_BRIDGE,
		"bridge":   netlink.MACVLAN_MODE_BRIDGE,
		"vepa":     netlink.MACVLAN_MODE_VEPA,
		"private":  netlink.MACVLAN_MODE_PRIVATE,
		"passthru": netlink.MACVLAN_MODE_PASSTHRU,
	} {
		got, err := parseMacvlanMode(in)
		if err != nil || got != want {
			t.Errorf("macvlan_mode=%q parsed to %v, %v; want %v", in, got, err, want)
		}
		if got == netlink.MACVLAN_MODE_DEFAULT {
			t.Errorf("macvlan_mode=%q reached netlink as DEFAULT, which the kernel builds as vepa", in)
		}
	}
	for _, in := range []string{"brigde", "Bridge", "passthrough", "source", " bridge"} {
		_, err := parseMacvlanMode(in)
		if err == nil {
			t.Errorf("macvlan_mode=%q was accepted", in)
			continue
		}
		for _, want := range []string{fmt.Sprintf("%q", in), "bridge", "vepa", "private", "passthru"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
	}
}

func TestParseIPvlanMode_L2OnlyAndL3RefusedWithTheReason(t *testing.T) {
	for _, in := range []string{"", "l2"} {
		if got, err := parseIPvlanMode(in); err != nil || got != netlink.IPVLAN_MODE_L2 {
			t.Errorf("ipvlan_mode=%q parsed to %v, %v; want L2", in, got, err)
		}
	}
	for _, in := range []string{"l3", "l3s"} {
		_, err := parseIPvlanMode(in)
		if err == nil {
			t.Fatalf("ipvlan_mode=%s was accepted; its child sends no DHCPDISCOVER out of the parent", in)
		}
		for _, want := range []string{"ipvlan_mode=" + in, "no broadcast", "l2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
	}
	_, err := parseIPvlanMode("L2")
	if err == nil || !strings.Contains(err.Error(), `"L2"`) || !strings.Contains(err.Error(), "l2") {
		t.Errorf("ipvlan_mode=L2 refusal does not name the value and the accepted one: %v", err)
	}
}

func TestDecodeOpts_SubModesAreRead(t *testing.T) {
	opts, err := decodeOpts(map[string]interface{}{"mode": "macvlan", "parent": "eth0", "macvlan_mode": "vepa", "ipvlan_mode": "l2"})
	if err != nil || opts.MacvlanMode != "vepa" || opts.IPvlanMode != "l2" {
		t.Errorf("decoded %+v, %v; want macvlan_mode vepa and ipvlan_mode l2", opts, err)
	}
}

func subModeCreate(t *testing.T, p *Plugin, opts map[string]interface{}) error {
	t.Helper()
	return mtuCreateNetwork(p, opts)
}

func TestCreateNetwork_SubModeRefusalsAndTheStoredValue(t *testing.T) {
	cases := []struct {
		name  string
		opts  map[string]interface{}
		names []string // nil means accepted
	}{
		{"unknown macvlan value", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "brigde"}, []string{`"brigde"`, "bridge, vepa, private, passthru"}},
		{"unknown ipvlan value", map[string]interface{}{"mode": "ipvlan", "ipvlan_mode": "l9"}, []string{`"l9"`, "l2"}},
		{"ipvlan l3", map[string]interface{}{"mode": "ipvlan", "ipvlan_mode": "l3"}, []string{"ipvlan_mode=l3", "no broadcast"}},
		{"macvlan_mode on ipvlan", map[string]interface{}{"mode": "ipvlan", "macvlan_mode": "bridge"}, []string{"macvlan_mode", "mode=ipvlan"}},
		{"ipvlan_mode on macvlan", map[string]interface{}{"mode": "macvlan", "ipvlan_mode": "l2"}, []string{"ipvlan_mode", "mode=macvlan"}},
		{"macvlan_mode on bridge", map[string]interface{}{"mode": "bridge", "macvlan_mode": "vepa"}, []string{"macvlan_mode", "mode=bridge"}},
		{"ipvlan_mode on bridge", map[string]interface{}{"mode": "bridge", "ipvlan_mode": "l2"}, []string{"ipvlan_mode", "mode=bridge"}},
		{"require_mac beside passthru", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru", "require_mac": "true"}, []string{"require_mac", "passthru"}},
		{"bridge", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "bridge"}, nil},
		{"vepa", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "vepa"}, nil},
		{"private", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "private"}, nil},
		{"passthru", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"}, nil},
		{"l2", map[string]interface{}{"mode": "ipvlan", "ipvlan_mode": "l2"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			opts := map[string]interface{}{"parent": mtuTestParent}
			if c.opts["mode"] == "bridge" {
				opts = map[string]interface{}{"bridge": mtuTestBridge}
			}
			for k, v := range c.opts {
				opts[k] = v
			}
			err := subModeCreate(t, p, opts)
			if c.names == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				stored, err := loadOptions(mtuTestNetwork)
				if err != nil {
					t.Fatalf("loadOptions: %v", err)
				}
				if got := stored.MacvlanMode + stored.IPvlanMode; got != c.name {
					t.Errorf("the state file holds sub-mode %q, want %q; a plugin restart would build another", got, c.name)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v was accepted", c.opts)
			}
			if got := util.ErrToStatus(err); got != http.StatusBadRequest {
				t.Errorf("the refusal %v maps to HTTP %d, want 400", err, got)
			}
			for _, n := range c.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("the refusal %q does not name %q", err, n)
				}
			}
		})
	}
}

func TestNewChildLink_BuildsTheStoredSubMode(t *testing.T) {
	la := netlink.NewLinkAttrs()
	la.Name = "dh-sub"
	for in, want := range map[string]netlink.MacvlanMode{
		"":         netlink.MACVLAN_MODE_BRIDGE,
		"bridge":   netlink.MACVLAN_MODE_BRIDGE,
		"vepa":     netlink.MACVLAN_MODE_VEPA,
		"private":  netlink.MACVLAN_MODE_PRIVATE,
		"passthru": netlink.MACVLAN_MODE_PASSTHRU,
	} {
		l, err := newChildLink(DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: in}, la)
		if err != nil {
			t.Fatalf("macvlan_mode=%q: %v", in, err)
		}
		if m := l.(*netlink.Macvlan).Mode; m != want {
			t.Errorf("macvlan_mode=%q built %v, want %v", in, m, want)
		}
	}
	l, err := newChildLink(DHCPNetworkOptions{Mode: ModeIPvlan, IPvlanMode: "l2"}, la)
	if err != nil || l.(*netlink.IPVlan).Mode != netlink.IPVLAN_MODE_L2 {
		t.Errorf("ipvlan_mode=l2 built %v, %v", l, err)
	}
	for _, bad := range []DHCPNetworkOptions{
		{Mode: ModeMacvlan, MacvlanMode: "brigde"},
		{Mode: ModeIPvlan, IPvlanMode: "l3"},
	} {
		if l, err := newChildLink(bad, la); err == nil {
			t.Errorf("%+v built %v; a value the create would refuse must not build a default child", bad, l)
		}
	}
}

func TestNewProbeLink_TakesTheSubModeAndNoMACOnPassthru(t *testing.T) {
	mac, _ := net.ParseMAC("02:11:22:33:44:55")
	l, err := newProbeLink(DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "passthru"}, "dh-probe-a1b2c3", 7, mac)
	if err != nil {
		t.Fatalf("newProbeLink: %v", err)
	}
	if m := l.(*netlink.Macvlan).Mode; m != netlink.MACVLAN_MODE_PASSTHRU {
		t.Errorf("the validate_dhcp probe on a passthru network built %v", m)
	}
	if got := l.Attrs().HardwareAddr; got != nil {
		t.Errorf("the passthru probe carries MAC %v; a MAC set on a passthru child changes the parent's", got)
	}
	l, err = newProbeLink(DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "private"}, "dh-probe-a1b2c3", 7, mac)
	if err != nil || l.(*netlink.Macvlan).Mode != netlink.MACVLAN_MODE_PRIVATE || l.Attrs().HardwareAddr.String() != mac.String() {
		t.Errorf("the private probe is %+v, %v; want private with the probe MAC", l, err)
	}
}

func TestNetOptions_RefusesAStoredSubModeTheCreateWouldRefuse(t *testing.T) {
	for _, opts := range []DHCPNetworkOptions{
		{Mode: ModeMacvlan, Parent: "eth0", MacvlanMode: "brigde"},
		{Mode: ModeIPvlan, Parent: "eth0", IPvlanMode: "l3"},
		{Mode: ModeBridge, Bridge: "br0", MacvlanMode: "vepa"},
	} {
		withStateDir(t, t.TempDir())
		if err := saveOptions("n1", opts); err != nil {
			t.Fatalf("saveOptions: %v", err)
		}
		p := &Plugin{docker: &fakeDocker{inspectErr: errors.New("docker must not be called")}}
		if _, err := p.netOptions(context.Background(), "n1"); err == nil {
			t.Errorf("stored %+v was served", opts)
		}
		if got := p.networkOptionsRejected.Load(); got != 1 {
			t.Errorf("networkOptionsRejected: got %d, want 1", got)
		}
	}
}

func TestChildWearsParentMAC(t *testing.T) {
	for _, c := range []struct {
		opts DHCPNetworkOptions
		want bool
	}{
		{DHCPNetworkOptions{Mode: ModeIPvlan}, true},
		{DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "passthru"}, true},
		{DHCPNetworkOptions{Mode: ModeMacvlan}, false},
		{DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "private"}, false},
		{DHCPNetworkOptions{}, false},
	} {
		if got := c.opts.childWearsParentMAC(); got != c.want {
			t.Errorf("%+v: childWearsParentMAC = %v, want %v", c.opts, got, c.want)
		}
	}
}

func subModeNet(id, name, driver string, opts map[string]string) dNetwork.Summary {
	return dNetwork.Summary{ID: id, Name: name, Driver: driver, Options: opts}
}

func TestCreateNetwork_RefusesASiblingTheKernelWouldBreak(t *testing.T) {
	cases := []struct {
		name    string
		opts    map[string]interface{}
		sibling dNetwork.Summary
		refused bool
	}{
		{"passthru beside this plugin's macvlan", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"},
			subModeNet("n0", "lan-a", testDHCPDriver, map[string]string{"mode": "macvlan", "parent": mtuTestParent}), true},
		{"bridge beside this plugin's passthru", map[string]interface{}{"mode": "macvlan"},
			subModeNet("n0", "lan-a", testDHCPDriver, map[string]string{"mode": "macvlan", "parent": mtuTestParent, "macvlan_mode": "passthru"}), true},
		{"passthru beside Docker's macvlan", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"},
			subModeNet("n0", "lan-a", "macvlan", map[string]string{"parent": mtuTestParent}), true},
		{"vepa beside Docker's passthru", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "vepa"},
			subModeNet("n0", "lan-a", "macvlan", map[string]string{"parent": mtuTestParent, "macvlan_mode": "passthru"}), true},
		{"l2 beside Docker's l3 ipvlan", map[string]interface{}{"mode": "ipvlan"},
			subModeNet("n0", "lan-a", "ipvlan", map[string]string{"parent": mtuTestParent, "ipvlan_mode": "l3"}), true},
		{"l2 beside Docker's l3s ipvlan", map[string]interface{}{"mode": "ipvlan"},
			subModeNet("n0", "lan-a", "ipvlan", map[string]string{"parent": mtuTestParent, "ipvlan_mode": "l3s"}), true},
		{"l2 beside Docker's default ipvlan", map[string]interface{}{"mode": "ipvlan"},
			subModeNet("n0", "lan-a", "ipvlan", map[string]string{"parent": mtuTestParent}), false},
		{"passthru beside a macvlan on another parent", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"},
			subModeNet("n0", "lan-a", "macvlan", map[string]string{"parent": "eth-other"}), false},
		{"passthru beside Docker's macvlan with a dummy parent", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"},
			subModeNet("n0", "lan-a", "macvlan", map[string]string{}), false},
		{"passthru beside an ipvlan on the same parent", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"},
			subModeNet("n0", "lan-a", "ipvlan", map[string]string{"parent": mtuTestParent}), false},
		{"two bridge macvlan networks", map[string]interface{}{"mode": "macvlan"},
			subModeNet("n0", "lan-a", testDHCPDriver, map[string]string{"mode": "macvlan", "parent": mtuTestParent}), false},
		{"the network itself in the list", map[string]interface{}{"mode": "macvlan", "macvlan_mode": "passthru"},
			subModeNet(mtuTestNetwork, "self", testDHCPDriver, map[string]string{"mode": "macvlan", "parent": mtuTestParent, "macvlan_mode": "passthru"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{listResult: []dNetwork.Summary{c.sibling}}
			opts := map[string]interface{}{"parent": mtuTestParent}
			for k, v := range c.opts {
				opts[k] = v
			}
			err := subModeCreate(t, p, opts)
			if !c.refused {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted; the kernel would refuse the second child or switch the first network's mode")
			}
			for _, want := range []string{"lan-a", mtuTestParent, "another parent"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
			if _, err := loadOptions(mtuTestNetwork); err == nil {
				t.Error("a refused network left state behind")
			}
		})
	}
}

func TestCreateNetwork_AFailedNetworkListDoesNotRefuse(t *testing.T) {
	withStateDir(t, t.TempDir())
	(&mtuKernel{parentMTU: 1500}).install(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{listErr: errors.New("daemon busy"), listErrUntil: 100}
	if err := subModeCreate(t, p, map[string]interface{}{"mode": "macvlan", "parent": mtuTestParent, "macvlan_mode": "passthru"}); err != nil {
		t.Errorf("a failed network list refused the create: %v", err)
	}
}

func TestRetryPassthruAdd(t *testing.T) {
	calls := 0
	waited, err := retryPassthruAdd(context.Background(), time.Second, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return unix.EINVAL
		}
		return nil
	})
	if err != nil || !waited || calls != 3 {
		t.Errorf("EINVAL twice then success: waited %v, err %v, calls %d", waited, err, calls)
	}

	calls = 0
	start := time.Now()
	waited, err = retryPassthruAdd(context.Background(), 50*time.Millisecond, 5*time.Millisecond, func() error { calls++; return unix.EINVAL })
	if !errors.Is(err, unix.EINVAL) || !waited || calls < 2 || time.Since(start) > time.Second {
		t.Errorf("EINVAL throughout: waited %v, err %v, calls %d after %v", waited, err, calls, time.Since(start))
	}

	calls = 0
	waited, err = retryPassthruAdd(context.Background(), time.Second, time.Millisecond, func() error { calls++; return unix.EBUSY })
	if !errors.Is(err, unix.EBUSY) || waited || calls != 1 {
		t.Errorf("EBUSY is not the passthru answer and must not be retried: waited %v, err %v, calls %d", waited, err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	_, err = retryPassthruAdd(ctx, time.Hour, time.Hour, func() error { calls++; return unix.EINVAL })
	if !errors.Is(err, unix.EINVAL) || calls != 1 {
		t.Errorf("a cancelled request kept waiting: err %v, calls %d", err, calls)
	}
}

func TestExplainChildLinkAdd_EINVALOnMacvlanNamesPassthru(t *testing.T) {
	err := explainChildLinkAdd(unix.EINVAL, ModeMacvlan, "eth0", 7)
	if !errors.Is(err, unix.EINVAL) {
		t.Errorf("the explanation lost the kernel's error: %v", err)
	}
	for _, want := range []string{"passthru", "one container", "eth0", "another parent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the explanation %q does not name %q", err, want)
		}
	}
	if msg := explainChildLinkAdd(unix.EINVAL, ModeIPvlan, "eth0", 7).Error(); strings.Contains(msg, "passthru") {
		t.Errorf("an ipvlan EINVAL was explained as passthru: %s", msg)
	}
}

func TestIpamRefusePassthru(t *testing.T) {
	err := ipamRefusePassthru(DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: "passthru"})
	if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "--ipam-driver null") {
		t.Errorf("passthru in IPAM mode: %v", err)
	}
	for _, sub := range []string{"", "bridge", "vepa", "private"} {
		if err := ipamRefusePassthru(DHCPNetworkOptions{Mode: ModeMacvlan, MacvlanMode: sub}); err != nil {
			t.Errorf("macvlan_mode=%q was refused in IPAM mode: %v", sub, err)
		}
	}
}

func TestCreateNetwork_IPAMModeRefusesPassthru(t *testing.T) {
	withStateDir(t, t.TempDir())
	(&mtuKernel{parentMTU: 1500}).install(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	err := p.CreateNetwork(CreateNetworkRequest{
		NetworkID: mtuTestNetwork,
		Options:   map[string]interface{}{util.OptionsKeyGeneric: map[string]interface{}{"mode": "macvlan", "parent": mtuTestParent, "macvlan_mode": "passthru"}},
		IPv4Data:  []*IPAMData{{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool}},
	})
	if err == nil || !strings.Contains(err.Error(), "passthru") || !strings.Contains(err.Error(), "--ipam-driver null") {
		t.Errorf("an IPAM-mode passthru network: %v", err)
	}
}

func TestCreateEndpoint_PassthruRefusesAUserMAC(t *testing.T) {
	withStateDir(t, t.TempDir())
	(&mtuKernel{parentMTU: 1500}).install(t)
	const network = "net-passthru"
	if err := saveOptions(network, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: mtuTestParent, MacvlanMode: "passthru"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: network, EndpointID: "ep-passthru-mac",
		Interface: &EndpointInterface{MacAddress: "02:42:aa:bb:cc:01"},
	})
	if !errors.Is(err, util.ErrMACAddress) || !strings.Contains(err.Error(), "passthru") || !strings.Contains(err.Error(), "parent's") {
		t.Errorf("a user MAC on a passthru network: %v", err)
	}
}

func TestReacquireEndpoint_PassthruAsksDockerForNoMAC(t *testing.T) {
	for _, c := range []struct {
		sub  string
		asks bool
	}{{"passthru", false}, {"bridge", true}} {
		withStateDir(t, t.TempDir())
		opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "dh-905-nosuch", MacvlanMode: c.sub}
		if err := saveOptions("n1", opts); err != nil {
			t.Fatalf("saveOptions: %v", err)
		}
		p := &Plugin{docker: &fakeDocker{inspectErr: errors.New("inspect refused")}, joinHints: make(map[string]joinHint)}
		err := p.reacquireEndpoint(context.Background(), JoinRequest{NetworkID: "n1", EndpointID: "ep-1"}, opts)
		if err == nil {
			t.Fatalf("macvlan_mode=%s: the replay succeeded against a parent that does not exist", c.sub)
		}
		if asked := strings.Contains(err.Error(), "original endpoint MAC"); asked != c.asks {
			t.Errorf("macvlan_mode=%s: asked Docker for the endpoint MAC = %v, want %v (%v)", c.sub, asked, c.asks, err)
		}
	}
}

func TestRecoveredMAC_PassthruReadsTheParent(t *testing.T) {
	_, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "dh-905-nosuch", MacvlanMode: "passthru"}, "")
	if err == nil || errors.Is(err, errNoRecoveryMAC) || !strings.Contains(err.Error(), "dh-905-nosuch") {
		t.Errorf("passthru recovery with no Docker MAC did not look up the parent: %v", err)
	}
	if _, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "dh-905-nosuch"}, ""); !errors.Is(err, errNoRecoveryMAC) {
		t.Errorf("a bridge-mode macvlan endpoint with no Docker MAC recovered: %v", err)
	}
	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		t.Skipf("no link list to read: %v", err)
	}
	for _, l := range links {
		if hw := l.Attrs().HardwareAddr; len(hw) == 6 {
			mac, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: l.Attrs().Name, MacvlanMode: "passthru"}, "")
			if err != nil || mac.String() != hw.String() {
				t.Errorf("passthru recovery MAC = %v, %v; want the parent's %v", mac, err, hw)
			}
			return
		}
	}
	t.Skip("no link with an Ethernet address in this namespace; the parent read above still ran")
}
