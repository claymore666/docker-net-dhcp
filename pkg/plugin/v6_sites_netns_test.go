// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// oneShotCall is what one stubbed one-shot was handed (#960).
type oneShotCall struct {
	iface    string
	deadline time.Time
	opts     dhcp.DHCPClientOptions
}

func stubV6OneShot(t *testing.T, info6 dhcp.Info, ra dhcp.RAObservation, err6 error) (v4, v6 *oneShotCall) {
	t.Helper()
	v4, v6 = &oneShotCall{}, &oneShotCall{}
	restore := dhcpGetIP
	dhcpGetIP = func(ctx context.Context, iface string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		c := v4
		if o.V6 {
			c = v6
		}
		c.iface = iface
		c.deadline, _ = ctx.Deadline()
		c.opts = *o
		if o.V6 {
			return info6, ra, err6
		}
		return dhcp.Info{IP: "192.168.99.61/24", Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = restore })
	return v4, v6
}

func TestCreateEndpoint_TheV6HalfOnEveryShape(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "v6br0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "v6pa0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "v6pb0"}})
	leased := dhcp.Info{IP: "fd00:960::61/64", Gateway: "fe80::1", OnLinkPrefixes: []string{"fd00:960::/64"}}
	managed := dhcp.RAObservation{Seen: true, Managed: true}
	for si, shape := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"bridge", DHCPNetworkOptions{Bridge: "v6br0"}},
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "v6pa0"}},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "v6pb0"}},
	} {
		for ci, tc := range []struct {
			name    string
			mode    string
			info    dhcp.Info
			ra      dhcp.RAObservation
			err     error
			wantErr string
		}{
			{"leases", "dhcp", leased, managed, nil, ""},
			{"no router in dhcp mode is tolerated", "dhcp", dhcp.Info{}, dhcp.RAObservation{}, dhcp.ErrNoLease, ""},
			{"no router in slaac mode is fatal", "slaac", dhcp.Info{}, dhcp.RAObservation{}, dhcp.ErrNoLease,
				"failed to get initial IPv6 address via DHCPv6: "},
			{"a managed segment with no lease is fatal", "dhcp", dhcp.Info{}, managed, dhcp.ErrNoLease,
				"failed to get initial IPv6 address via DHCPv6: "},
			{"an unparseable lease is fatal", "dhcp", dhcp.Info{IP: "fd00:960::61"}, managed, nil,
				"failed to parse initial IPv6 address: "},
		} {
			// ipvlan refuses every address-forming ipv6_mode at network create (#817).
			if shape.opts.Mode == ModeIPvlan && tc.mode != "dhcp" {
				continue
			}
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				withStateDir(t, t.TempDir())
				opts := shape.opts
				opts.IPv6Mode = tc.mode
				if err := saveOptions("n960", opts); err != nil {
					t.Fatalf("saveOptions: %v", err)
				}
				start := time.Now()
				restore := endpointCallStart
				endpointCallStart = func() time.Time { return start }
				t.Cleanup(func() { endpointCallStart = restore })
				v4, v6 := stubV6OneShot(t, tc.info, tc.ra, tc.err)

				p := newPluginForTest()
				p.docker = &fakeDocker{}
				p.records = recordingPlugin(t).records
				ep := fmt.Sprintf("%d%d%062x", si, ci, 0)
				res, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: "n960", EndpointID: ep,
					Interface: &EndpointInterface{Address: "192.168.99.77/24", AddressIPv6: "fd00:960::77/64"}})
				var hint joinHint
				p.updateJoinHint(ep, func(h *joinHint) { hint = *h })

				if want := start.Add(pluginCallBudget - pluginCallMargin); !v6.deadline.Equal(want) {
					t.Errorf("the DHCPv6 one-shot's deadline is %v after the call started, want %v (#911)",
						v6.deadline.Sub(start), want.Sub(start))
				}
				checkV6Wiring(t, opts, ep, v4, v6)
				if tc.wantErr != "" {
					if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
						t.Fatalf("CreateEndpoint err = %v, want the prefix %q", err, tc.wantErr)
					}
					if tc.err != nil && !errors.Is(err, tc.err) {
						t.Errorf("CreateEndpoint err = %v, want it to wrap %v", err, tc.err)
					}
					return
				}
				if err != nil {
					t.Fatalf("CreateEndpoint: %v", err)
				}
				checkV6Outcome(t, opts, res, hint, v6, tc.info)
			})
		}
	}
}

func checkV6Wiring(t *testing.T, opts DHCPNetworkOptions, ep string, v4, v6 *oneShotCall) {
	t.Helper()
	mode, _ := opts.ipv6Mode()
	id6, err := resolveIdentity6(opts, ep, v6.opts.MAC)
	if err != nil {
		t.Fatalf("resolveIdentity6: %v", err)
	}
	switch {
	case !v6.opts.V6 || v6.iface == "" || v6.iface != v4.iface || len(v6.opts.MAC) == 0 || !bytes.Equal(v6.opts.MAC, v4.opts.MAC):
		t.Errorf("the DHCPv6 one-shot ran with V6 %v on %q with MAC %v, want true and the v4 one-shot's %q and %v",
			v6.opts.V6, v6.iface, v6.opts.MAC, v4.iface, v4.opts.MAC)
	case !bytes.Equal(v6.opts.Identity6.DUID, id6.DUID) || v6.opts.Identity6.IAID != id6.IAID:
		t.Errorf("the DHCPv6 identity is %+v, want %+v (#895)", v6.opts.Identity6, id6)
	case v6.opts.RecordID == "" || v6.opts.RecordID == v4.opts.RecordID:
		t.Errorf("the DHCPv6 record is %q beside the v4 record %q, want its own (#899)", v6.opts.RecordID, v4.opts.RecordID)
	case v6.opts.PreferredV6 != "fd00:960::77" || v6.opts.Mode6 != mode:
		t.Errorf("the DHCPv6 one-shot asked for %q in mode %v, want the --ip6 address in %v (#213, #817)",
			v6.opts.PreferredV6, v6.opts.Mode6, mode)
	case v4.opts.RequestedIP != "192.168.99.77" || v6.opts.RequestedIP != "":
		t.Errorf("the one-shots asked for v4 %q and v6-side %q, want the --ip address on v4 alone (#213)",
			v4.opts.RequestedIP, v6.opts.RequestedIP)
	}
}

func checkV6Outcome(t *testing.T, opts DHCPNetworkOptions, res CreateEndpointResponse, hint joinHint, v6 *oneShotCall, info dhcp.Info) {
	t.Helper()
	if info.IP == "" {
		if res.Interface.AddressIPv6 != "" || hint.IPv6 != nil || hint.GatewayIPv6 != "" {
			t.Errorf("a tolerated absence answered %q with hint %v via %q, want no IPv6 at all",
				res.Interface.AddressIPv6, hint.IPv6, hint.GatewayIPv6)
		}
		return
	}
	if res.Interface.AddressIPv6 != info.IP || hint.IPv6 == nil || hint.IPv6.String() != info.IP {
		t.Errorf("CreateEndpoint answered %q with hint %v, want %q in both", res.Interface.AddressIPv6, hint.IPv6, info.IP)
	}
	if !bytes.Equal(hint.MacAddress, v6.opts.MAC) {
		t.Errorf("the Join hint carries MAC %v, want the one-shot's %v", hint.MacAddress, v6.opts.MAC)
	}
	var join JoinResponse
	p := newPluginForTest()
	p.applyV6JoinHint(opts, JoinRequest{}, hint, &join)
	if join.GatewayIPv6 != info.Gateway || len(join.StaticRoutes) != 1 || join.StaticRoutes[0].Destination != "fd00:960::/64" {
		t.Errorf("Join would answer gateway %q and routes %v, want %q and the on-link fd00:960::/64 (#821)",
			join.GatewayIPv6, describeStaticRoutes(join.StaticRoutes), info.Gateway)
	}
}
