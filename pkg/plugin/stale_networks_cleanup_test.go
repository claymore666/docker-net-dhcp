// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func staleCleanupPlugin(t *testing.T, opts DHCPNetworkOptions, release string) (*Plugin, *fakeSender, string) {
	t.Helper()
	withStateDir(t, t.TempDir())
	sender := installSender(t, nil)
	p := withRecords(t, &Plugin{ipamIndex: newIPAMIndex()})
	opts.ReleaseLease = release
	if err := saveNetwork(vlanNetA, opts, nil); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	p.persistedAtStart = rebuildIPAMIndex(p.ipamIndex)
	p.docker = &fakeDocker{inspectErr: networkNotFound()}
	return p, sender, retainedOn(t, p, vlanNetA)
}

func hostAddr(t *testing.T, link netlink.Link, cidr string) {
	t.Helper()
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatalf("ParseAddr %q: %v", cidr, err)
	}
	prev := nlAddrList
	t.Cleanup(func() { nlAddrList = prev })
	nlAddrList = func(l netlink.Link, family int) ([]netlink.Addr, error) {
		if l.Attrs().Index != link.Attrs().Index || family != unix.AF_INET {
			return nil, nil
		}
		return []netlink.Addr{*a}, nil
	}
}

// An own bridge refuses release_lease at create, so its retained record is closed with nothing sent.
func TestStaleNetworks_ADroppedBridgeNetworkHasItsBridgeRetired(t *testing.T) {
	p, sender, recID := staleCleanupPlugin(t, bridgeOwnOpts(), "")
	k := stubBridgeKernel(t, bridgeTestParentLink(), bridgeTestBridgeLink(vlanOwnerAlias))

	recoverOnce(p)

	if got := p.staleNetworksDropped.Load(); got != 1 {
		t.Fatalf("stale_networks_dropped = %d, want 1", got)
	}
	if len(k.deleted) != 1 || k.deleted[0] != bridgeTestBridge {
		t.Errorf("host links deleted = %v, want [%s]", k.deleted, bridgeTestBridge)
	}
	if got := sender.callCount(); got != 0 {
		t.Errorf("RELEASE sent %d times for a network that never asked for one", got)
	}
	if got := recordPhase(t, p, recID); got != lease.PhaseClosed {
		t.Errorf("the retained record is %v, want CLOSED", got)
	}
}

func TestStaleNetworks_ADroppedVlanNetworkIsReleasedAndItsSubInterfaceRetired(t *testing.T) {
	p, sender, recID := staleCleanupPlugin(t, vlanOpts("100"), ReleaseOnRemove)
	sub := vlanTestLink(vlanOwnerAlias)
	k := stubVlanKernel(t, vlanTestParent(), sub)
	hostAddr(t, sub, "192.168.99.2/24")

	recoverOnce(p)

	if got := p.staleNetworksDropped.Load(); got != 1 {
		t.Fatalf("stale_networks_dropped = %d, want 1", got)
	}
	if got := sender.callCount(); got != 1 {
		t.Errorf("RELEASE sent %d times for the retained on_remove record, want 1", got)
	}
	if !k.removed("eth0.100") {
		t.Errorf("the vlan sub-interface is still on the host (deleted: %v)", k.deleted)
	}
	if got := recordPhase(t, p, recID); got != lease.PhaseClosed {
		t.Errorf("the retained record is %v, want CLOSED", got)
	}
}

func TestStaleNetworks_ADroppedNetworkKeepsWhatAnotherNetworkStillUses(t *testing.T) {
	p, _, _ := staleCleanupPlugin(t, bridgeOwnOpts(), "")
	k := stubBridgeKernel(t, bridgeTestParentLink(), bridgeTestBridgeLink(vlanOwnerAlias))
	if err := saveNetwork(vlanNetB, bridgeOwnOpts(), nil); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}

	recoverOnce(p)

	if got := p.staleNetworksDropped.Load(); got != 1 {
		t.Fatalf("stale_networks_dropped = %d, want 1", got)
	}
	if len(k.deleted) != 0 {
		t.Errorf("host links deleted = %v, want none while a stored network uses the bridge", k.deleted)
	}
}
