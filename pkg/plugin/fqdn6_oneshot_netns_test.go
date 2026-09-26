// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"testing"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A name known at CreateEndpoint rides the one-shot on both families; buildParams6 turns it into option 39 only with
// register_dns (TestBuildParams6_RegisterDNSPutsTheNameInOption39, #1029).
func TestCreateEndpoint_TheOneShotsCarryAKnownName(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "fq6br0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "fq6pa0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "fq6pb0"}})
	leased := dhcp.Info{IP: "fd00:960::61/64", Gateway: "fe80::1", OnLinkPrefixes: []string{"fd00:960::/64"}}
	for si, shape := range []DHCPNetworkOptions{
		{Bridge: "fq6br0"},
		{Mode: ModeMacvlan, Parent: "fq6pa0"},
		{Mode: ModeIPvlan, Parent: "fq6pb0"},
	} {
		for ri, registerDNS := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d/register_dns=%v", si, registerDNS), func(t *testing.T) {
				withStateDir(t, t.TempDir())
				opts := shape
				opts.IPv6Mode, opts.RegisterDNS = "dhcp", registerDNS
				if err := saveOptions("n1029", opts); err != nil {
					t.Fatalf("saveOptions: %v", err)
				}
				v4, v6 := stubV6OneShot(t, leased, dhcp.RAObservation{Seen: true, Managed: true}, nil)
				ep := fmt.Sprintf("%d%d%062x", si, ri, 1029)
				p := newPluginForTest()
				p.docker = &fakeDocker{
					inspectResult: map[string]dNetwork.Inspect{"n1029": {Containers: map[string]dNetwork.EndpointResource{
						"web1ctr": {EndpointID: ep},
					}}},
					containerResult: map[string]dContainer.InspectResponse{
						"web1ctr": {Config: &dContainer.Config{Hostname: "web1"}},
					},
				}
				p.records = recordingPlugin(t).records
				if _, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: "n1029", EndpointID: ep,
					Interface: &EndpointInterface{Address: "192.168.99.77/24"}}); err != nil {
					t.Fatalf("CreateEndpoint: %v", err)
				}
				for fam, c := range map[string]*oneShotCall{"v4": v4, "v6": v6} {
					if c.opts.Hostname != "web1" || c.opts.FQDN != opts.fqdnMode() {
						t.Errorf("the %s one-shot ran with hostname %q and FQDN %q, want \"web1\" and %q", fam,
							c.opts.Hostname, c.opts.FQDN, opts.fqdnMode())
					}
				}
			})
		}
	}
}
