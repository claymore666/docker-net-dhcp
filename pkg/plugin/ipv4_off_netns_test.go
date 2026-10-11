// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A v4-off network runs the DHCPv6 one-shot only, and the Join hint still carries the MAC and no IPv4 (#1135).
func TestCreateEndpoint_AV4OffNetworkRunsOnlyTheV6OneShot(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "v4offbr0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "v4offpa0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "v4offpb0"}})
	leased := dhcp.Info{IP: "fd00:960::61/64", Gateway: "fe80::1", OnLinkPrefixes: []string{"fd00:960::/64"}}
	managed := dhcp.RAObservation{Seen: true, Managed: true}
	for si, shape := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"bridge", DHCPNetworkOptions{Bridge: "v4offbr0"}},
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "v4offpa0"}},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "v4offpb0"}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			opts := shape.opts
			opts.IPv4Off, opts.IPv6, opts.IPv6Mode = true, true, "dhcp"
			if err := saveOptions("n1135", opts); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			v4, v6 := stubV6OneShot(t, leased, managed, nil)

			p := newPluginForTest()
			p.docker = &fakeDocker{}
			p.records = recordingPlugin(t).records
			ep := fmt.Sprintf("%d%063x", si, 0)
			res, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: "n1135", EndpointID: ep,
				Interface: &EndpointInterface{}})
			if err != nil {
				t.Fatalf("CreateEndpoint: %v", err)
			}
			var hint joinHint
			p.updateJoinHint(ep, func(h *joinHint) { hint = *h })

			if v4.iface != "" {
				t.Errorf("the DHCPv4 one-shot ran on %q on a network with no IPv4", v4.iface)
			}
			if v6.iface == "" {
				t.Error("the DHCPv6 one-shot did not run")
			}
			if res.Interface.Address != "" || hint.IPv4 != nil || hint.Gateway != "" || hint.RecordID != "" {
				t.Errorf("IPv4 leaked into the answer %q or the hint (%v, %q, record %q)",
					res.Interface.Address, hint.IPv4, hint.Gateway, hint.RecordID)
			}
			if len(hint.MacAddress) == 0 || !bytes.Equal(hint.MacAddress, v6.opts.MAC) {
				t.Errorf("the Join hint carries MAC %v, want the one-shot's %v", hint.MacAddress, v6.opts.MAC)
			}
			if res.Interface.AddressIPv6 != leased.IP {
				t.Errorf("CreateEndpoint answered %q, want the v6 lease %q", res.Interface.AddressIPv6, leased.IP)
			}
		})
	}
}
