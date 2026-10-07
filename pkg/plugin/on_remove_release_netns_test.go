// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// release_lease=on_remove hands the address back after the container is removed, on every network shape (#1249).
func TestOnRemove_EveryCreatedShapeHandsItsAddressBack(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	for _, name := range []string{"orbr0", "orpa0", "orpb0", "orpc0"} {
		var link netlink.Link = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if name == "orbr0" {
			link = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
		}
		addLink(t, h, link)
		got, err := h.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName %s: %v", name, err)
		}
		if err := h.AddrAdd(got, &netlink.Addr{IPNet: mustCIDR(t, "192.168.99.2/24")}); err != nil {
			t.Fatalf("AddrAdd %s: %v", name, err)
		}
		if err := h.LinkSetUp(got); err != nil {
			t.Fatalf("LinkSetUp %s: %v", name, err)
		}
	}
	restore := dhcpGetIP
	dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		return dhcp.Info{IP: "192.168.99.61/24", Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = restore })

	for i, shape := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"bridge", DHCPNetworkOptions{Bridge: "orbr0"}},
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "orpa0"}},
		{"macvlan passthru", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "orpb0", MacvlanMode: "passthru"}},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "orpc0"}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			const network = "n1249"
			opts := shape.opts
			opts.ReleaseLease = ReleaseOnRemove
			if err := saveOptions(network, opts); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			sender := installSender(t, nil)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			p.records = recordingPlugin(t).records
			ep := fmt.Sprintf("%d%063x", i, 1249)
			if _, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: network, EndpointID: ep,
				Interface: &EndpointInterface{}}); err != nil {
				t.Fatalf("CreateEndpoint: %v", err)
			}
			var id string
			p.updateJoinHint(ep, func(h *joinHint) { id = h.RecordID })
			if id == "" {
				t.Fatal("CreateEndpoint filed no lease record, so this test is not driving the record's life")
			}
			// What the renewal client writes between Join and an on_remove Leave, which sends nothing.
			if err := p.records.Observed(id, acquired("192.168.99.61/24", time.Hour), nil); err != nil {
				t.Fatalf("Observed: %v", err)
			}
			if err := p.records.Bound(id); err != nil {
				t.Fatalf("Bound: %v", err)
			}
			p.recordLeft(id)

			if err := p.DeleteEndpoint(t.Context(), DeleteEndpointRequest{NetworkID: network, EndpointID: ep}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}
			p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle))

			if got := sender.callCount(); got != 1 {
				t.Errorf("%d DHCPRELEASE(s) left the host after the container was removed, want 1: the record "+
					"is %v, and only a held record is handed back, so the address stays leased until the "+
					"server expires it", got, recordPhase(t, p, id))
			}
			if got := recordPhase(t, p, id); got != lease.PhaseClosed {
				t.Errorf("the record is %v after the hand-back pass, want CLOSED", got)
			}
		})
	}
}

// The IPAM driver's endpoint takes the same release, by the key its reserved record was folded onto (#1249).
func TestOnRemove_AnIPAMEndpointHandsItsAddressBack(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	mac := f0MAC(0x04)
	p, b, opts := s3Fixture(t, "")
	sender := installSender(t, nil)
	id := f0Created(t, p, mac, f0Addr)
	f0Reservation(p, b, mac, id, f0Addr, nil)
	req := f0CreateRequest(mac, f0Addr)
	if _, err := p.createIPAMEndpoint(t.Context(), time.Now(), req, opts, b); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if err := p.records.Bound(id); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	p.recordLeft(id)

	if err := p.DeleteEndpoint(t.Context(), DeleteEndpointRequest{NetworkID: ipamTestNetwork, EndpointID: req.EndpointID}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle))

	if got := sender.callCount(); got != 1 {
		t.Errorf("%d DHCPRELEASE(s) left the host after the IPAM endpoint was removed, want 1; the record is %v",
			got, recordPhase(t, p, id))
	}
	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("the record is %v after the hand-back pass, want CLOSED", got)
	}
}
