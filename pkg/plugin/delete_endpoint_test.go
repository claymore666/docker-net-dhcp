// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"testing"
)

// DeleteEndpoint had no unit coverage at all before #338: the
// integration suite only ever exercises the path where the host veth
// still exists, so the not-found arm — the one that keeps a forced
// teardown from wedging `docker network rm` — was carried entirely by
// code review. These tests cover it without root, since LinkByName on
// a name that was never created fails the same way for any user.

// deleteEndpointPlugin builds a plugin whose network options resolve
// from disk, so DeleteEndpoint never reaches for the Docker API. The
// fakeDocker errors on every call to prove that.
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
	// A veth pair dies whole when the container side's netns goes away
	// (`docker rm -f`, OOM-kill, a host reboot racing teardown), so by
	// the time libnetwork calls DeleteEndpoint the host side is often
	// already gone. Returning an error there 500s the RPC and can wedge
	// `docker network rm` — the network keeps an endpoint it can never
	// delete.
	p := deleteEndpointPlugin(t, "net-missing", DHCPNetworkOptions{Bridge: "br-test"})

	// An endpoint ID that was never created: vethPairNames derives
	// "dh-<first 12 chars>", which cannot exist on the host.
	err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID:  "net-missing",
		EndpointID: "e7a1c0ffee00deadbeef0000000000000000000000000000000000000000abcd",
	})
	if err != nil {
		t.Fatalf("DeleteEndpoint with an absent host veth must succeed, got: %v", err)
	}
}

func TestDeleteEndpoint_WritesTombstoneForBridgeMode(t *testing.T) {
	// The tombstone is what carries MAC/IP across a container restart.
	// It has to be laid down even when the link is already gone, or a
	// forced teardown silently costs the container its address on the
	// way back up.
	const netID, epID, hostname = "net-tomb", "abcdef0123456789aaaa", "app-1"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:      "02:42:ac:11:00:01",
		IPv4:     "192.168.0.166",
		IPv6:     "2001:db8::1",
		Hostname: hostname,
	}, true)

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	mac, ipv4, ipv6, ok := p.consumeTombstone(netID, hostname, true)
	if !ok {
		t.Fatal("expected a tombstone for the deleted endpoint, found none")
	}
	if mac != "02:42:ac:11:00:01" || ipv4 != "192.168.0.166" || ipv6 != "2001:db8::1" {
		t.Errorf("tombstone: got (%q, %q, %q), want the fingerprint's MAC/IPv4/IPv6", mac, ipv4, ipv6)
	}

	// The fingerprint is consumed, not merely copied — a second delete
	// must not resurrect it.
	if _, ok := p.takeEndpoint(epID); ok {
		t.Error("fingerprint should have been taken by DeleteEndpoint")
	}
}

func TestDeleteEndpoint_IPvlanSkipsTombstone(t *testing.T) {
	// ipvlan children share the parent's MAC, so a tombstone would only
	// hand back a MAC the kernel assigns anyway — recording one implies
	// a per-container identity that does not exist in this mode.
	const netID, epID, hostname = "net-ipvlan", "bbbbcccc11112222", "app-2"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{
		Mode:   ModeIPvlan,
		Parent: "eth-absent",
	})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:      "02:42:ac:11:00:02",
		IPv4:     "192.168.0.167",
		Hostname: hostname,
	}, true)

	// The parent-attached delete path is best-effort about an absent
	// link for the same reason the bridge path is.
	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint (ipvlan, link absent): %v", err)
	}

	if _, _, _, ok := p.consumeTombstone(netID, hostname, true); ok {
		t.Error("ipvlan must not leave a tombstone — the MAC is the parent's, not the container's")
	}
}

func TestDeleteEndpoint_NetworkOptionsFailurePropagates(t *testing.T) {
	// Disk miss plus a failing Docker inspect means the mode is unknown,
	// so the handler cannot tell which teardown to run. That has to
	// surface rather than be guessed at.
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
