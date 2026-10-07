// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	dContainer "github.com/moby/moby/api/types/container"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const onRemoveParent = "or-parent0"

// stubOnRemoveParent answers for one parent link that has a MAC, which ipvlan and passthru recovery inherit.
func stubOnRemoveParent(t *testing.T, mac net.HardwareAddr) {
	t.Helper()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: onRemoveParent, Index: 9, HardwareAddr: mac}}
	prevByName, prevList := nlLinkByName, nlAddrList
	nlLinkByName = func(name string) (netlink.Link, error) {
		if name != onRemoveParent {
			return nil, netlink.LinkNotFoundError{}
		}
		return link, nil
	}
	nlAddrList = func(_ netlink.Link, family int) ([]netlink.Addr, error) {
		if family != unix.AF_INET {
			return nil, nil
		}
		a, err := netlink.ParseAddr("192.168.99.2/24")
		if err != nil {
			return nil, err
		}
		return []netlink.Addr{*a}, nil
	}
	t.Cleanup(func() { nlLinkByName, nlAddrList = prevByName, prevList })
}

// An endpoint adopted after a plugin restart hands its address back once removed, with or without a hostname to
// describe it and with or without a MAC from Docker (#1249).
func TestOnRemove_ARecoveredEndpointHandsItsAddressBack(t *testing.T) {
	parentMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x12, 0x49}
	dockerMAC := "02:42:ac:11:00:49"
	hostnameOK := &lockedDocker{containers: map[string]dContainer.InspectResponse{"ctr-1249": withHostname("app-1249")}}
	inspectFails := &lockedDocker{inspectErr: errors.New("connection refused")}
	for i, tc := range []struct {
		name     string
		opts     DHCPNetworkOptions
		dockerMA string
		docker   dockerClient
	}{
		{"bridge, hostname known", DHCPNetworkOptions{Bridge: onRemoveParent}, dockerMAC, hostnameOK},
		{"bridge, inspect failed", DHCPNetworkOptions{Bridge: onRemoveParent}, dockerMAC, inspectFails},
		{"macvlan, inspect failed", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: onRemoveParent}, dockerMAC, inspectFails},
		{"macvlan passthru", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: onRemoveParent, MacvlanMode: "passthru"}, "", hostnameOK},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: onRemoveParent}, "", hostnameOK},
		{"ipvlan, inspect failed", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: onRemoveParent}, "", inspectFails},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const network = "n1249r"
			stubOnRemoveParent(t, parentMAC)
			sender := installSender(t, nil)
			p := withRecords(t, newTestPlugin(t))
			p.docker = tc.docker
			p.awaitTimeout = time.Second
			opts := tc.opts
			opts.ReleaseLease = ReleaseOnRemove
			if err := saveOptions(network, opts); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			ep := fmt.Sprintf("%d%063x", i, 0x1249)
			mac, err := recoveredMAC(opts, tc.dockerMA)
			if err != nil {
				t.Fatalf("recoveredMAC: %v", err)
			}
			// The record a running endpoint left in the journal before the plugin restarted.
			key := endpointRecordKey(opts.effectiveMode(), ep, mac)
			id := p.recordCreated(network, key, dhcp.ClientIdentity(key))
			if id == "" {
				t.Fatal("no record was created")
			}
			if err := p.records.Observed(id, acquired("192.168.99.62/24", time.Hour), nil); err != nil {
				t.Fatalf("Observed: %v", err)
			}
			if err := p.records.Bound(id); err != nil {
				t.Fatalf("Bound: %v", err)
			}

			adopted, err := p.recoverOneEndpoint(context.Background(), "ctr-1249", network, ep, tc.dockerMA,
				"192.168.99.62/24", "", opts)
			if err != nil || !adopted {
				t.Fatalf("recoverOneEndpoint = (%v, %v), want an adoption", adopted, err)
			}
			// The adopted client's Start runs on its own goroutine; it fails here, with no container to enter.
			for end := time.Now().Add(10 * time.Second); p.dhcpManagerExists(ep); {
				if time.Now().After(end) {
					t.Fatal("the recovered client's Start never finished")
				}
				time.Sleep(10 * time.Millisecond)
			}

			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: network, EndpointID: ep}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}
			p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle))

			if got := sender.callCount(); got != 1 {
				t.Errorf("%d DHCPRELEASE(s) left the host after the recovered container was removed, want 1: "+
					"the record is %v, and only a held record is handed back", got, recordPhase(t, p, id))
			}
			if got := recordPhase(t, p, id); got != lease.PhaseClosed {
				t.Errorf("the record is %v after the hand-back pass, want CLOSED", got)
			}
			if mac, _, _, ok := p.consumeTombstone(network, dhcpHostname{name: "some-other-container"}); ok {
				t.Errorf("another container inherited the recovered endpoint's MAC %q", mac)
			}
		})
	}
}
