// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestCreateNetwork_BridgeFailedCreateReleasesReenslavedParent(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]interface{}
		want string
	}{
		{"mtu refusal", map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "mtu": "9000"}, "above the MTU"},
		{"bridge used", map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent}, "already in use"},
	} {
		t.Run(tc.name+" releases the parent the create enslaved again", func(t *testing.T) {
			withStateDir(t, t.TempDir())
			stubFirewall(t, false, nil, nfDrop, nil)
			parent := bridgeTestParentLink() // released before the call, as by an operator or a reboot
			k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
			storeBridgeOwner(t, vlanNetB, bridgeTestParent)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			err := bridgeCreate(p, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v; want the create to fail with %q", err, tc.want)
			}
			if strings.Join(k.enslaved, ",") != bridgeTestParent+"->"+bridgeTestBridge {
				t.Fatalf("enslaved %v; the scenario needs the create to enslave the parent first", k.enslaved)
			}
			if parent.MasterIndex != 0 || strings.Join(k.released, ",") != bridgeTestParent || len(k.deleted) != 0 {
				t.Errorf("parent master %d, released %v, deleted %v; want the parent released and the bridge of the stored network kept",
					parent.MasterIndex, k.released, k.deleted)
			}
		})
	}
	t.Run("a failed create that did not enslave the parent releases nothing", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		parent := bridgeTestParentLink()
		parent.MasterIndex = bridgeTestBridgeIndex
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		storeBridgeOwner(t, vlanNetB, bridgeTestParent)
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent})
		if err == nil || parent.MasterIndex != bridgeTestBridgeIndex || len(k.released)+len(k.enslaved) != 0 {
			t.Errorf("err %v, master %d, released %v, enslaved %v; want the failure and the port left as it was", err, parent.MasterIndex, k.released, k.enslaved)
		}
	})
	t.Run("a successful create keeps the parent a port", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		parent := bridgeTestParentLink()
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		storeBridgeOwner(t, vlanNetB, bridgeTestParent)
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "ignore_conflicts": "true"})
		if err != nil || parent.MasterIndex != bridgeTestBridgeIndex || len(k.released) != 0 {
			t.Errorf("err %v, master %d, released %v; want the create to succeed with the port kept", err, parent.MasterIndex, k.released)
		}
	})
	t.Run("a failed release leaves the create's own error", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		stubFirewall(t, false, nil, nfDrop, nil)
		parent := bridgeTestParentLink()
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		k.noMasterErr = errors.New("device busy")
		storeBridgeOwner(t, vlanNetB, bridgeTestParent)
		p := newPluginForTest()
		p.docker = &fakeDocker{}
		err := bridgeCreate(p, map[string]interface{}{"bridge": bridgeTestBridge, "parent": bridgeTestParent, "mtu": "9000"})
		if err == nil || !strings.Contains(err.Error(), "above the MTU") || strings.Contains(err.Error(), "busy") {
			t.Errorf("err %v; want the MTU refusal, not the release failure", err)
		}
	})
	t.Run("a call that relied on the port in between keeps it", func(t *testing.T) {
		parent := bridgeTestParentLink()
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		p := newTestPlugin(t)
		storeBridgeOwner(t, vlanNetB, bridgeTestParent)
		if _, taken, err := p.ensureBridgeTaking(context.Background(), bridgeOwnOpts(), "create_network"); err != nil || !taken {
			t.Fatalf("taken %v, err %v; want the parent enslaved again", taken, err)
		}
		if _, err := p.ensureBridge(context.Background(), bridgeOwnOpts(), "create_endpoint"); err != nil {
			t.Fatal(err)
		}
		p.releaseParent(context.Background(), bridgeOwnOpts(), "create_network_failed")
		if parent.MasterIndex != bridgeTestBridgeIndex || len(k.released) != 0 {
			t.Errorf("master %d, released %v; want the port left for the endpoint that relies on it", parent.MasterIndex, k.released)
		}
	})
}

func takenPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := newTestPlugin(t)
	p.bridgeTaken = map[string]bool{bridgeTestBridge: true}
	return p
}

func TestReleaseReenslavedParent(t *testing.T) {
	enslavedParent := func(master int) *netlink.Device {
		l := bridgeTestParentLink()
		l.MasterIndex, l.Promisc = master, 1
		return l
	}
	t.Run("releases a parent still held by the bridge", func(t *testing.T) {
		parent := enslavedParent(bridgeTestBridgeIndex)
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		takenPlugin(t).releaseParent(context.Background(), bridgeOwnOpts(), "create_network_failed")
		if parent.MasterIndex != 0 || strings.Join(k.released, ",") != bridgeTestParent {
			t.Errorf("master %d, released %v; want the parent released", parent.MasterIndex, k.released)
		}
	})
	t.Run("leaves a parent another master took meanwhile", func(t *testing.T) {
		parent := enslavedParent(bridgeTestOtherIndex)
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		takenPlugin(t).releaseParent(context.Background(), bridgeOwnOpts(), "create_network_failed")
		if parent.MasterIndex != bridgeTestOtherIndex || len(k.released) != 0 {
			t.Errorf("master %d, released %v; want another master's port left alone", parent.MasterIndex, k.released)
		}
	})
	t.Run("releases nothing a create did not enslave", func(t *testing.T) {
		parent := enslavedParent(bridgeTestBridgeIndex)
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		newTestPlugin(t).releaseParent(context.Background(), bridgeOwnOpts(), "create_network_failed")
		if parent.MasterIndex != bridgeTestBridgeIndex || len(k.released) != 0 {
			t.Errorf("master %d, released %v; want a port no create of this call took left alone", parent.MasterIndex, k.released)
		}
	})
	t.Run("leaves a bridge the plugin did not mark", func(t *testing.T) {
		parent := enslavedParent(bridgeTestBridgeIndex)
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(""))
		takenPlugin(t).releaseParent(context.Background(), bridgeOwnOpts(), "create_network_failed")
		if len(k.released) != 0 {
			t.Errorf("released %v; want an unmarked bridge's port left alone", k.released)
		}
	})
	t.Run("a failed release is logged and does not panic", func(t *testing.T) {
		parent := enslavedParent(bridgeTestBridgeIndex)
		k := stubBridgeKernel(t, parent, bridgeTestBridgeLink(vlanOwnerAlias))
		k.noMasterErr = errors.New("device busy")
		takenPlugin(t).releaseParent(context.Background(), bridgeOwnOpts(), "create_network_failed")
		if parent.MasterIndex != bridgeTestBridgeIndex {
			t.Errorf("master %d; the stub refused the release", parent.MasterIndex)
		}
	})
}

// The lookup right after the add fails; the bridge it made stays unmarked and blocks the next create (#1242).
func TestEnsureBridge_LookupFailsAfterAddRemovesTheBridge(t *testing.T) {
	k := stubBridgeKernel(t, bridgeTestParentLink())
	byName := nlLinkByName
	nlLinkByName = func(name string) (netlink.Link, error) {
		if name == bridgeTestBridge && len(k.added) > 0 {
			return nil, errors.New("netlink receive: interrupted")
		}
		return byName(name)
	}
	p := newTestPlugin(t)
	created, err := p.ensureBridge(context.Background(), bridgeOwnOpts(), "create_network")
	if err == nil || created {
		t.Fatalf("created %v, err %v; want the failure", created, err)
	}
	if strings.Join(k.deleted, ",") != bridgeTestBridge {
		t.Errorf("deleted %v, links %v; want the bridge the create made removed", k.deleted, k.links)
	}
}
