// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// createEndpointEnding runs CreateEndpoint on a real bridge or macvlan parent with the lease attempt ending in
// leaseErr, and returns the error the engine would get (#1116).
func createEndpointEnding(t *testing.T, ep string, opts DHCPNetworkOptions, leaseErr error) error {
	t.Helper()
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1", opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	restore := dhcpGetIP
	dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		return dhcp.Info{}, dhcp.RAObservation{}, leaseErr
	}
	t.Cleanup(func() { dhcpGetIP = restore })
	withFakeARP(t, newFakeARPLink("02:42:0a:00:00:09"))
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	_, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: "n1", EndpointID: ep, Interface: &EndpointInterface{}})
	return err
}

// The bridge CreateEndpoint call site carries the verdict; macvlan and other failures do not (#1116).
func TestCreateEndpoint_FirewallVerdictAtTheBridgeDeadline(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "fwbr0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "fwpa0"}})
	bridge := DHCPNetworkOptions{Bridge: "fwbr0"}
	macvlan := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "fwpa0"}
	const today = "failed to get initial IP address via DHCP: context deadline exceeded"
	rule := "iptables -A FORWARD -i fwbr0 -j ACCEPT"

	for i, tc := range []struct {
		name     string
		opts     DHCPNetworkOptions
		leaseErr error
		policy   uint32
		verdict  bool
	}{
		{"bridge deadline under DROP", bridge, context.DeadlineExceeded, nfDrop, true},
		{"bridge deadline under ACCEPT", bridge, context.DeadlineExceeded, 1, false},
		{"bridge failing another way under DROP", bridge, errors.New("server NAK"), nfDrop, false},
		{"bridge no-lease under DROP", bridge, dhcp.ErrNoLease, nfDrop, false},
		{"macvlan deadline under DROP", macvlan, context.DeadlineExceeded, nfDrop, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubFirewall(t, true, nil, tc.policy, nil)
			err := createEndpointEnding(t, fmt.Sprintf("f%d%062x", i, 0), tc.opts, tc.leaseErr)
			if err == nil {
				t.Fatal("CreateEndpoint succeeded; want the lease failure")
			}
			if tc.verdict {
				for _, want := range []string{today, "is DROP", rule} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v; want it to carry %q", err, want)
					}
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v; want the deadline still visible to errors.Is", err)
				}
				return
			}
			if strings.Contains(err.Error(), "FORWARD") || !errors.Is(err, tc.leaseErr) {
				t.Errorf("err = %v; want today's error without a firewall verdict", err)
			}
			if tc.leaseErr == context.DeadlineExceeded && tc.opts.Mode != ModeMacvlan && err.Error() != today {
				t.Errorf("err = %q; want exactly %q", err, today)
			}
		})
	}
}
