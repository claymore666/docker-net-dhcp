// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"strings"
	"testing"
)

// A failed create gets neither Join nor DeleteEndpoint, so its hint would stay in pending_hints for good (#1183).
func TestCreateEndpoint_AFailedCreateLeavesNoPendingHint(t *testing.T) {
	const network = "net-pending-hint"
	p := newTestPlugin(t)
	if err := saveOptions(network, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-pending-none"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID: network, EndpointID: "ep-failed-create", Interface: &EndpointInterface{},
		Options: map[string]interface{}{"com.docker.network.endpoint.ifname": "lan0"},
	})
	if err == nil || !strings.Contains(err.Error(), "failed to get bridge interface") {
		t.Fatalf("the create did not fail at the bridge lookup, so the test proves nothing: %v", err)
	}
	if _, pending := p.managerSnapshot(); pending != 0 {
		t.Errorf("pending_hints = %d after a failed create, want 0", pending)
	}
	if _, ok := p.takeJoinHint("ep-failed-create"); ok {
		t.Error("a failed create left a Join hint behind")
	}
}

func TestDeleteEndpoint_DropsAnUnjoinedHint(t *testing.T) {
	const netID, epID = "net-unjoined", "abcdef0123456789bbbb"
	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	p.storeJoinHint(epID, joinHint{Ifname: "lan0"})
	if _, pending := p.managerSnapshot(); pending != 1 {
		t.Fatalf("setup: pending_hints = %d, want 1", pending)
	}

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if _, pending := p.managerSnapshot(); pending != 0 {
		t.Errorf("pending_hints = %d after DeleteEndpoint with no Join, want 0", pending)
	}
	if _, ok := p.takeJoinHint(epID); ok {
		t.Error("DeleteEndpoint left the unjoined hint behind")
	}
}
