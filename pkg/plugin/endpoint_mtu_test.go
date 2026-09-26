// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	mtuTestNetwork  = "net-mtu-1"
	mtuTestEndpoint = "ep-mtu-0123456789abcdef"
	mtuTestBridge   = "br-mtu"
	mtuTestParent   = "eth-mtu"
)

// mtuKernel stands in for the parent lookup and the MTU seam, recording what the seam was asked.
type mtuKernel struct {
	parentMTU int
	sets      []string
	setErr    error
}

func (k *mtuKernel) install(t *testing.T) {
	t.Helper()
	prevByName, prevSet := nlLinkByName, nlHandleLinkSetMTU
	t.Cleanup(func() { nlLinkByName, nlHandleLinkSetMTU = prevByName, prevSet })
	nlLinkByName = func(name string) (netlink.Link, error) {
		switch name {
		case mtuTestBridge:
			return &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 7, MTU: k.parentMTU}}, nil
		case mtuTestParent:
			return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 8, MTU: k.parentMTU, Flags: net.FlagUp}}, nil
		}
		return nil, netlink.LinkNotFoundError{}
	}
	nlHandleLinkSetMTU = func(_ *netlink.Handle, l netlink.Link, mtu int) error {
		if k.setErr != nil {
			return k.setErr
		}
		k.sets = append(k.sets, l.Attrs().Name+"="+strconv.Itoa(mtu))
		return nil
	}
}

func mtuCreateNetwork(p *Plugin, opts map[string]interface{}) error {
	return p.CreateNetwork(CreateNetworkRequest{
		NetworkID: mtuTestNetwork,
		Options:   map[string]interface{}{util.OptionsKeyGeneric: opts},
		IPv4Data:  []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
}

func TestDecodeOpts_MTUIsReadAsADecimalInteger(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{{"1400", 1400}, {"01400", 1400}, {"9000", 9000}} {
		opts, err := decodeOpts(map[string]interface{}{"mtu": c.in})
		if err != nil || opts.MTU != c.want {
			t.Errorf("mtu=%s decoded to %d, %v; want %d", c.in, opts.MTU, err, c.want)
		}
	}
	// mapstructure's weak decode is base 0: without the hook "0x5dc" is 1500 and "0o2574" is 1404.
	for _, in := range []string{"0x5dc", "0o2574", "0b10101111000", "1400.0", "1e3", "abc", "1400 "} {
		opts, err := decodeOpts(map[string]interface{}{"mtu": in})
		if err == nil {
			t.Errorf("mtu=%q was accepted as %d; only a decimal integer is an MTU", in, opts.MTU)
			continue
		}
		if !strings.Contains(err.Error(), "'mtu'") || !strings.Contains(err.Error(), in) {
			t.Errorf("the refusal of mtu=%q does not name the option and the value: %v", in, err)
		}
	}
	if opts, err := decodeOpts(map[string]interface{}{"mtu": ""}); err != nil || opts.MTU != 0 {
		t.Errorf("-o mtu= decoded to %d, %v; an empty option is unset", opts.MTU, err)
	}
}

func TestCreateNetwork_MTURefusals(t *testing.T) {
	cases := []struct {
		name  string
		opts  map[string]interface{}
		names []string // nil means accepted
	}{
		{"67 is under the kernel's minimum", map[string]interface{}{"mtu": "67"}, []string{"mtu=67", "68..65535"}},
		{"1 is under the kernel's minimum", map[string]interface{}{"mtu": "1"}, []string{"mtu=1"}},
		{"a negative value", map[string]interface{}{"mtu": "-1400"}, []string{"mtu=-1400"}},
		{"65536 is over the kernel's maximum", map[string]interface{}{"mtu": "65536"}, []string{"mtu=65536", "68..65535"}},
		{"beside propagate_mtu=true", map[string]interface{}{"mtu": "1400", "propagate_mtu": "true"}, []string{"mtu=1400", "propagate_mtu=true"}},
		{"above the bridge's MTU", map[string]interface{}{"mtu": "1501"}, []string{"mtu=1501", mtuTestBridge, "1500"}},
		{"a non-integer", map[string]interface{}{"mtu": "jumbo"}, []string{"'mtu'", "jumbo"}},
		{"68 is the minimum", map[string]interface{}{"mtu": "68"}, nil},
		{"the bridge's own MTU", map[string]interface{}{"mtu": "1500"}, nil},
		{"unset", map[string]interface{}{}, nil},
		{"beside propagate_mtu=false", map[string]interface{}{"mtu": "1400", "propagate_mtu": "false"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			opts := map[string]interface{}{"bridge": mtuTestBridge}
			for k, v := range c.opts {
				opts[k] = v
			}
			err := mtuCreateNetwork(p, opts)
			if c.names == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v was accepted", c.opts)
			}
			for _, n := range c.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("the refusal %q does not name %q", err, n)
				}
			}
		})
	}
}

func TestCreateNetwork_MTUAboveTheParentIsRefusedInEveryMode(t *testing.T) {
	for _, mode := range []string{ModeMacvlan, ModeIPvlan, ModeBridge} {
		t.Run(mode, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1400}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			opts := map[string]interface{}{"mode": mode, "parent": mtuTestParent, "mtu": "1401"}
			parent := mtuTestParent
			if mode == ModeBridge {
				opts = map[string]interface{}{"bridge": mtuTestBridge, "mtu": "1401"}
				parent = mtuTestBridge
			}
			err := mtuCreateNetwork(p, opts)
			if err == nil {
				t.Fatalf("mtu=1401 on a %v whose MTU is 1400 was accepted", parent)
			}
			for _, n := range []string{"mtu=1401", parent, "1400"} {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("the refusal %q does not name %q", err, n)
				}
			}
			opts["mtu"] = "1400"
			if err := mtuCreateNetwork(p, opts); err != nil {
				t.Errorf("mtu equal to the parent's was refused: %v", err)
			}
		})
	}
}

func TestApplyEndpointMTU_EveryLinkGetsTheValueAndARefusalNamesIt(t *testing.T) {
	host := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "dh-host"}}
	peer := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "host-dh"}}

	k := &mtuKernel{}
	k.install(t)
	if err := applyEndpointMTU(0, host, peer); err != nil || len(k.sets) != 0 {
		t.Fatalf("mtu=0: err %v, seam asked %v; want nothing asked", err, k.sets)
	}
	if err := applyEndpointMTU(1400, host, peer); err != nil {
		t.Fatalf("mtu=1400: %v", err)
	}
	if got, want := strings.Join(k.sets, ","), "dh-host=1400,host-dh=1400"; got != want {
		t.Errorf("seam asked %q, want %q", got, want)
	}

	k.sets, k.setErr = nil, errors.New("invalid argument")
	err := applyEndpointMTU(1400, host, peer)
	if err == nil || !strings.Contains(err.Error(), "mtu=1400") || !errors.Is(err, k.setErr) {
		t.Errorf("a refused set returned %v; want an error naming mtu=1400 and wrapping the kernel's", err)
	}
}

// mtuManager is a joined endpoint's manager whose link reads back what the seam last wrote.
func mtuManager(t *testing.T, mtu, linkMTU int) (*dhcpManager, *Plugin, *fakeLink, *[]int, *error) {
	t.Helper()
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}, DHCPNetworkOptions{MTU: mtu}).withPlugin(p)
	link := &fakeLink{attrs: netlink.LinkAttrs{Name: "eth0", Index: 3, MTU: linkMTU}}
	m.netHandle = &netlink.Handle{}
	m.ctrLink = &fakeLink{attrs: link.attrs}
	var sets []int
	var setErr error
	prevIdx, prevSet := nlLinkByIndex, nlHandleLinkSetMTU
	t.Cleanup(func() { nlLinkByIndex, nlHandleLinkSetMTU = prevIdx, prevSet })
	nlLinkByIndex = func(_ *netlink.Handle, i int) (netlink.Link, error) {
		if i != link.attrs.Index {
			return nil, netlink.LinkNotFoundError{}
		}
		return &fakeLink{attrs: link.attrs}, nil
	}
	nlHandleLinkSetMTU = func(_ *netlink.Handle, _ netlink.Link, v int) error {
		if setErr != nil {
			return setErr
		}
		sets = append(sets, v)
		link.attrs.MTU = v
		return nil
	}
	return m, p, link, &sets, &setErr
}

func TestPropagateMTU_TheMTUOptionIsTheOnlySource(t *testing.T) {
	m, p, link, sets, _ := mtuManager(t, 1450, 1450)
	refused := p.mtuRefused.Load()
	out := captureLog(t, func() {
		m.propagateMTU(true, dhcp.Info{MTU: 1500})
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1280})
		m.propagateMTU(false, dhcp.Info{MTU: 9000})
		m.propagateMTU(false, dhcp.Info{MTU: 300})
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1280})
	})
	if len(*sets) != 0 {
		t.Errorf("a supplied MTU reached the link: the seam was asked %v", *sets)
	}
	if got := link.attrs.MTU; got != 1450 {
		t.Errorf("link MTU %d, want the option's 1450", got)
	}
	if n := strings.Count(out, "the MTU the network supplied is not applied"); n != 1 {
		t.Errorf("the not-applied line appeared %d times, want once per endpoint:\n%s", n, out)
	}
	if !strings.Contains(out, "supplied_mtu=1280") || !strings.Contains(out, "mtu=1450") || strings.Contains(out, "supplied_mtu=1500") {
		t.Errorf("the line does not carry both values:\n%s", out)
	}
	if got := p.mtuRefused.Load(); got != refused {
		t.Errorf("mtu_refused moved by %d for a value the option overrides", got-refused)
	}
}

func TestPropagateMTU_ALinkThatMovedIsSetBackOnEveryRenew(t *testing.T) {
	m, _, link, sets, setErr := mtuManager(t, 1450, 1450)
	link.attrs.MTU = 9000 // an ipvlan child followed its parent
	m.propagateMTU(false, dhcp.Info{})
	if got := link.attrs.MTU; got != 1450 || len(*sets) != 1 {
		t.Fatalf("after a renew the link is at %d with seam calls %v; want it set back to 1450 once", got, *sets)
	}
	for want := 2; want <= 3; want++ {
		link.attrs.MTU = 9000
		m.propagateMTU(false, dhcp.Info{MTU: 9000})
		if got := link.attrs.MTU; got != 1450 || len(*sets) != want {
			t.Fatalf("renew %d carrying option 26 leaves the link at %d with seam calls %v; want it set back", want, got, *sets)
		}
	}

	link.attrs.MTU = 1400 // a macvlan child clamped under a lowered parent
	*setErr = errors.New("invalid argument")
	warned := 0
	for i := 0; i < 2; i++ {
		out := captureLog(t, func() { m.propagateMTU(false, dhcp.Info{}) })
		if strings.Contains(out, "refused to set the link back") && strings.Contains(out, "link_mtu=1400") {
			warned++
		}
	}
	if warned != 2 {
		t.Errorf("a refused re-apply warned %d times over two renews, want once each", warned)
	}
}
