// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

// lockedDocker is a dockerClient whose bookkeeping survives being
// touched from two goroutines at once. recoverOneEndpoint spawns the
// manager's Start before it returns, so that goroutine is still calling
// into the client while the test body drives DeleteEndpoint against the
// same one. The shared fakeDocker counts its calls with plain ints and
// would report that as a data race instead of the behaviour under test.
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

// withHostname builds the ContainerInspect answer recovery reads: a
// running container whose Config carries the hostname.
func withHostname(h string) dContainer.InspectResponse {
	return dContainer.InspectResponse{
		ContainerJSONBase: &dContainer.ContainerJSONBase{
			State: &dContainer.State{Running: true, Status: "running", Pid: 4242},
		},
		Config: &dContainer.Config{Hostname: h},
	}
}

// recoveryPlugin is a plugin whose network options resolve from disk, so
// DeleteEndpoint never needs the Docker API, and whose Docker client
// answers the one ContainerInspect recovery makes.
func recoveryPlugin(t *testing.T, networkID string, docker dockerClient) *Plugin {
	t.Helper()
	p := newTestPlugin(t)
	p.docker = docker
	if err := saveOptions(networkID, DHCPNetworkOptions{Bridge: "br-test"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	return p
}

// TestRecoverOneEndpoint_RecordsFingerprint is the regression test for
// #721.
//
// rememberEndpoint had exactly two call sites, both on the
// CreateEndpoint path. recoverOneEndpoint rebuilt the DHCP manager for
// an already-attached endpoint and recorded nothing, and DeleteEndpoint
// writes a tombstone only for an endpoint it holds a fingerprint for.
// So every endpoint that had lived through a plugin or daemon restart
// silently lost address stability across its next `docker restart`: no
// tombstone, a fresh MAC, and in general a different address from the
// DHCP server. Nothing said so — tombstones_consumed simply stayed
// flat.
//
// The assertion is deliberately made at the far end, on the tombstone,
// rather than on the fingerprint map: the fingerprint is an
// implementation detail and the tombstone is the thing the next
// CreateEndpoint actually inherits.
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

	gotMAC, gotIPv4, gotIPv6, ok := p.consumeTombstone(netID, hostname, true)
	if !ok {
		t.Fatal("a recovered endpoint left no tombstone when it was deleted; the next docker restart gets a fresh MAC")
	}
	// Bare addresses, not the CIDR recovery was handed: the tombstone is
	// replayed into a DHCP request (option 50), which takes an address.
	if gotMAC != mac || gotIPv4 != "192.168.0.166" || gotIPv6 != "2001:db8::1" {
		t.Errorf("tombstone: got (%q, %q, %q), want (%q, 192.168.0.166, 2001:db8::1)", gotMAC, gotIPv4, gotIPv6, mac)
	}
}

// TestRecoverOneEndpoint_NoHostnameNoWildcardTombstone is the other
// direction, and it is the reason the fix records nothing rather than
// recording an empty hostname.
//
// tombstoneStore.consume reads a tombstone with an empty Hostname as
// "matches any container on this network" — a deliberate carve-out for
// v0.5.0 tombstones and for the CreateEndpoint/container-registration
// race, both honest absences. A recovery that recorded a fingerprint
// with no hostname would therefore not be laying a weaker tombstone, it
// would be laying a wildcard one, and the next container to attach to
// this network would inherit a MAC and an address that were never its
// own. That is the shape #693 closed on the consuming side; closing
// #721 must not reopen it on the writing side.
//
// Each case ends in "no tombstone at all", which is exactly the
// behaviour these endpoints have today — the direction that cannot cost
// a container that did nothing wrong its identity.
func TestRecoverOneEndpoint_NoHostnameNoWildcardTombstone(t *testing.T) {
	const (
		epID  = "abcdef0123456789cccc"
		ctrID = "ctr-1"
		mac   = "02:42:ac:11:00:08"
	)

	// wantSkipped and wantRejected are asserted as a PAIR because the two
	// counters are deliberately disjoint: recovery_fingerprints_skipped
	// is "the daemon would not tell me", unsafe_hostnames_rejected is "a
	// container sent a hostname nobody should send". Collapsing them
	// would leave an operator unable to tell a degraded daemon from a
	// hostile container, which is the whole reason there are two.
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

			// Asked with a DIFFERENT container's hostname: that is the
			// theft this guards against. A wildcard tombstone answers it.
			if gotMAC, gotIPv4, _, ok := p.consumeTombstone(netID, "some-other-container", true); ok {
				t.Errorf("another container inherited mac=%q ipv4=%q from a hostname-less recovery — %s", gotMAC, gotIPv4, tc.reason)
			}

			// The skip must be VISIBLE. Without this the fix inherits
			// the invisibility of the bug it closes: no fingerprint
			// means no tombstone means an endpoint that silently loses
			// its address on its next restart, with tombstones_consumed
			// staying flat — which is what a quiet host looks like too.
			if got := p.recoveryFingerprintsSkipped.Load(); got != tc.wantSkipped {
				t.Errorf("recovery_fingerprints_skipped = %d, want %d", got, tc.wantSkipped)
			}
			if got := p.unsafeHostnamesRejected.Load(); got != tc.wantRejected {
				t.Errorf("unsafe_hostnames_rejected = %d, want %d", got, tc.wantRejected)
			}
		})
	}
}

// TestRecoverOneEndpoint_LosingTheRaceRecordsNothing pins the placement
// of the recording, not just its existence. A Join that beat recovery to
// the endpoint owns it, and its CreateEndpoint replay records its own
// fingerprint; stamping ours over it would hand that endpoint's
// tombstone our idea of the hostname.
func TestRecoverOneEndpoint_LosingTheRaceRecordsNothing(t *testing.T) {
	const (
		netID = "net-raced"
		epID  = "abcdef0123456789dddd"
		ctrID = "ctr-1"
	)

	p := recoveryPlugin(t, netID, &lockedDocker{
		containers: map[string]dContainer.InspectResponse{ctrID: withHostname("app-1")},
	})
	// What the winning Join left behind.
	p.registerDHCPManager(epID, &dhcpManager{})
	p.rememberEndpoint(epID, endpointFingerprint{MAC: "02:42:ac:11:00:09", Hostname: "app-1"})

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
