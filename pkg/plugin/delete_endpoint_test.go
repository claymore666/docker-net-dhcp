// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"testing"
)

func deleteEndpointPlugin(t *testing.T, networkID string, opts DHCPNetworkOptions) *Plugin {
	t.Helper()

	p := newTestPlugin(t)
	p.docker = &fakeDocker{inspectErr: errors.New("DeleteEndpoint must not call docker")}
	if err := saveOptions(networkID, opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	return p
}

func TestDeleteEndpoint_MissingHostVethIsNotAnError(t *testing.T) {
	p := deleteEndpointPlugin(t, "net-missing", DHCPNetworkOptions{Bridge: "br-test"})

	err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID:  "net-missing",
		EndpointID: "e7a1c0ffee00deadbeef0000000000000000000000000000000000000000abcd",
	})
	if err != nil {
		t.Fatalf("DeleteEndpoint with an absent host veth must succeed, got: %v", err)
	}
}

func TestDeleteEndpoint_WritesTombstoneForBridgeMode(t *testing.T) {
	const netID, epID, hostname = "net-tomb", "abcdef0123456789aaaa", "app-1"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:  "02:42:ac:11:00:01",
		IPv4: "192.168.0.166",
		IPv6: "2001:db8::1",
	}, dhcpHostname{name: hostname})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	mac, ipv4, ipv6, ok := p.consumeTombstone(netID, dhcpHostname{name: hostname})
	if !ok {
		t.Fatal("expected a tombstone for the deleted endpoint, found none")
	}
	if mac != "02:42:ac:11:00:01" || ipv4 != "192.168.0.166" || ipv6 != "2001:db8::1" {
		t.Errorf("tombstone: got (%q, %q, %q), want the fingerprint's MAC/IPv4/IPv6", mac, ipv4, ipv6)
	}

	if _, ok := p.takeEndpoint(epID); ok {
		t.Error("fingerprint should have been taken by DeleteEndpoint")
	}
}

func TestDeleteEndpoint_IPvlanSkipsTombstone(t *testing.T) {
	// ipvlan children share the parent's MAC, so there is no per-container MAC to tombstone.
	const netID, epID, hostname = "net-ipvlan", "bbbbcccc11112222", "app-2"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{
		Mode:   ModeIPvlan,
		Parent: "eth-absent",
	})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:  "02:42:ac:11:00:02",
		IPv4: "192.168.0.167",
	}, dhcpHostname{name: hostname})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint (ipvlan, link absent): %v", err)
	}

	if _, _, _, ok := p.consumeTombstone(netID, dhcpHostname{name: hostname}); ok {
		t.Error("ipvlan must not leave a tombstone — the MAC is the parent's, not the container's")
	}
}

func TestDeleteEndpoint_NetworkOptionsFailurePropagates(t *testing.T) {
	p := newTestPlugin(t)
	p.docker = &fakeDocker{inspectErr: errors.New("inspect boom")}

	err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID:  "net-unknown",
		EndpointID: "ffff0000ffff0000",
	})
	if err == nil {
		t.Fatal("expected an error when the network options cannot be resolved")
	}
}
