// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

	dTypes "github.com/docker/docker/api/types"
	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

type lockedDocker struct {
	mu         sync.Mutex
	containers map[string]dContainer.InspectResponse
	inspectErr error
}

func (d *lockedDocker) NetworkList(context.Context, dNetwork.ListOptions) ([]dNetwork.Summary, error) {
	return nil, nil
}

func (d *lockedDocker) NetworkInspect(context.Context, string, dNetwork.InspectOptions) (dNetwork.Inspect, error) {
	return dNetwork.Inspect{}, errors.New("no network detail in this fixture")
}

func (d *lockedDocker) ContainerInspect(_ context.Context, id string) (dContainer.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inspectErr != nil {
		return dContainer.InspectResponse{}, d.inspectErr
	}
	return d.containers[id], nil
}

func (d *lockedDocker) Close() error { return nil }

// These fixtures skip the engine probe in NewPlugin, so the fake answers as an unreachable daemon.
func (d *lockedDocker) Ping(context.Context) (dTypes.Ping, error) {
	return dTypes.Ping{}, errors.New("no daemon in this fixture")
}

func (d *lockedDocker) ServerVersion(context.Context) (dTypes.Version, error) {
	return dTypes.Version{}, errors.New("no daemon in this fixture")
}

func (d *lockedDocker) ClientVersion() string { return "" }

func withHostname(h string) dContainer.InspectResponse {
	return dContainer.InspectResponse{
		ContainerJSONBase: &dContainer.ContainerJSONBase{
			State: &dContainer.State{Running: true, Status: "running", Pid: 4242},
		},
		Config: &dContainer.Config{Hostname: h},
	}
}

func recoveryPlugin(t *testing.T, networkID string, docker dockerClient) *Plugin {
	t.Helper()
	p := newTestPlugin(t)
	p.docker = docker
	if err := saveOptions(networkID, DHCPNetworkOptions{Bridge: "br-test"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	return p
}

func TestRecoverOneEndpoint_RecordsFingerprint(t *testing.T) {
	const (
		netID    = "net-recovered"
		epID     = "abcdef0123456789bbbb"
		ctrID    = "ctr-1"
		hostname = "app-1"
		mac      = "02:42:ac:11:00:07"
	)

	p := recoveryPlugin(t, netID, &lockedDocker{
		containers: map[string]dContainer.InspectResponse{ctrID: withHostname(hostname)},
	})

	adopted, err := p.recoverOneEndpoint(
		context.Background(), ctrID, netID, epID,
		mac, "192.168.0.166/24", "2001:db8::1/64",
		DHCPNetworkOptions{Bridge: "br-test"},
	)
	if err != nil {
		t.Fatalf("recoverOneEndpoint: %v", err)
	}
	if !adopted {
		t.Fatal("recoverOneEndpoint did not adopt an unmanaged endpoint")
	}

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	gotMAC, gotIPv4, gotIPv6, ok := p.consumeTombstone(netID, dhcpHostname{name: hostname})
	if !ok {
		t.Fatal("a recovered endpoint left no tombstone when it was deleted; the next docker restart gets a fresh MAC")
	}
	// Bare addresses: the tombstone is replayed as option 50 (RFC 2132 section 9.1).
	if gotMAC != mac || gotIPv4 != "192.168.0.166" || gotIPv6 != "2001:db8::1" {
		t.Errorf("tombstone: got (%q, %q, %q), want (%q, 192.168.0.166, 2001:db8::1)", gotMAC, gotIPv4, gotIPv6, mac)
	}
}

// An empty-hostname tombstone matches any container (#693), so recovery records none when the hostname is unknown
// (#721).
func TestRecoverOneEndpoint_NoHostnameNoWildcardTombstone(t *testing.T) {
	const (
		epID  = "abcdef0123456789cccc"
		ctrID = "ctr-1"
		mac   = "02:42:ac:11:00:08"
	)

	cases := []struct {
		name         string
		docker       dockerClient
		wantSkipped  int32
		wantRejected int32
		reason       string
	}{
		{
			name: "a refused hostname",
			docker: &lockedDocker{containers: map[string]dContainer.InspectResponse{
				ctrID: withHostname("attacker-host\x01"),
			}},
			wantSkipped:  0,
			wantRejected: 1,
			reason:       "safeHostname refuses it, and a refusal must not become the matcher's wildcard",
		},
		{
			name:         "an inspect the daemon never answered",
			docker:       &lockedDocker{inspectErr: errors.New("connection refused")},
			wantSkipped:  1,
			wantRejected: 0,
			reason:       "an unknown hostname is not a hostname that matches everything",
		},
		{
			name: "a container with no hostname",
			docker: &lockedDocker{containers: map[string]dContainer.InspectResponse{
				ctrID: withHostname(""),
			}},
			wantSkipped:  1,
			wantRejected: 0,
			reason:       "an empty hostname is the wildcard; it can never be recorded as one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const netID = "net-nohost"
			p := recoveryPlugin(t, netID, tc.docker)

			if _, err := p.recoverOneEndpoint(
				context.Background(), ctrID, netID, epID,
				mac, "192.168.0.170/24", "",
				DHCPNetworkOptions{Bridge: "br-test"},
			); err != nil {
				t.Fatalf("recoverOneEndpoint: %v", err)
			}

			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
				NetworkID: netID, EndpointID: epID,
			}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}

			if gotMAC, gotIPv4, _, ok := p.consumeTombstone(netID, dhcpHostname{name: "some-other-container"}); ok {
				t.Errorf("another container inherited mac=%q ipv4=%q from a hostname-less recovery — %s", gotMAC, gotIPv4, tc.reason)
			}

			if got := p.recoveryFingerprintsSkipped.Load(); got != tc.wantSkipped {
				t.Errorf("recovery_fingerprints_skipped = %d, want %d", got, tc.wantSkipped)
			}
			if got := p.unsafeHostnamesRejected.Load(); got != tc.wantRejected {
				t.Errorf("unsafe_hostnames_rejected = %d, want %d", got, tc.wantRejected)
			}
		})
	}
}

func TestRecoverOneEndpoint_LosingTheRaceRecordsNothing(t *testing.T) {
	const (
		netID = "net-raced"
		epID  = "abcdef0123456789dddd"
		ctrID = "ctr-1"
	)

	p := recoveryPlugin(t, netID, &lockedDocker{
		containers: map[string]dContainer.InspectResponse{ctrID: withHostname("app-1")},
	})
	p.registerDHCPManager(epID, &dhcpManager{})
	p.rememberEndpoint(epID, endpointFingerprint{MAC: "02:42:ac:11:00:09"}, dhcpHostname{name: "app-1"})

	if _, err := p.recoverOneEndpoint(
		context.Background(), ctrID, netID, epID,
		"02:42:ac:11:00:0a", "192.168.0.171/24", "",
		DHCPNetworkOptions{Bridge: "br-test"},
	); err != nil {
		t.Fatalf("recoverOneEndpoint: %v", err)
	}

	fp, ok := p.takeEndpoint(epID)
	if !ok {
		t.Fatal("the winner's fingerprint was removed")
	}
	if fp.MAC != "02:42:ac:11:00:09" {
		t.Errorf("fingerprint MAC = %q, want the winning Join's 02:42:ac:11:00:09", fp.MAC)
	}
}
