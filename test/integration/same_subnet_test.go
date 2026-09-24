// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// sameSubnetLeaseBudget bounds the wait for the ACKs, a positive event that exits early, unlike the release absence below.
const sameSubnetLeaseBudget = 30 * time.Second

// libnetwork refuses a second sandbox address in a subnet the container already routes, by containment either way;
// measured on two daemons with and without moby/moby#52866 and the ifname option, four cells with one error, so the
// test does not probe and skip (#847). Macvlan, because bridge mode refuses two networks on one bridge earlier, at
// CreateNetwork with util.ErrBridgeUsed, unless ignore_conflicts is set.

// TestMultiNetwork_SameSubnetRefused checks that a container on two plugin networks with the same subnet fails to start with libnetwork's route conflict (#847).
func TestMultiNetwork_SameSubnetRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// Both networks on the suite fixture's parent, so both lease from 192.168.99.0/24.
	netA := "dh-itest-samesubnet-a"
	netB := "dh-itest-samesubnet-b"
	// The ACK floor below is derived from this list.
	attachments := []string{netA, netB}
	for _, n := range attachments {
		harness.CreateNetwork(t, ctx, n, "macvlan", nil)
	}

	// The deltas are scoped to time, not to this container, which never starts and so has no MAC to key on. A foreign
	// DHCPRELEASE can only fail a healthy tree, but a foreign DHCPACK would satisfy the floor; that stays latent while no
	// test calls t.Parallel and the fixture lease (2m) outlasts the 30s window only for running containers (#847).
	acksBefore := fixture.CountLogLines("DHCPACK")
	releasesBefore := fixture.CountLogLines("DHCPRELEASE")

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netA: {}, netB: {},
		}}, nil, "dh-itest-samesubnet-ctr")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, create.ID, container.StopOptions{})
		_ = cli.ContainerRemove(bg, create.ID, container.RemoveOptions{Force: true})
	})

	err = cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	if err == nil {
		// Recorded before the evidence, because harness.ExecOutput calls t.Fatalf on an exec error.
		t.Errorf("two networks on one subnet attached successfully — libnetwork no longer " +
			"refuses a second address in an already-routed subnet. Update the " +
			"troubleshooting row in docs/reference.md (#847) rather than this test.")
		t.Logf("container addresses:\n%s",
			harness.ExecOutput(t, ctx, create.ID, "ip", "-o", "-4", "addr"))
		t.FailNow()
	}

	// Any broken fixture also fails ContainerStart, so the reason is asserted.
	if !strings.Contains(err.Error(), "conflicts with existing route") {
		t.Fatalf("ContainerStart failed for the wrong reason.\nwant substring: %q\ngot: %v",
			"conflicts with existing route", err)
	}
	t.Logf("✓ refused as documented: %v", err)

	// The plugin leases in CreateEndpoint, once per network, before libnetwork refuses the second network's sandbox
	// address, so each attachment has its own macvlan child, MAC and ACK: the floor is len(attachments), and a retry can
	// only add ACKs. Nothing sends a DHCPRELEASE without release_lease (#800, #962), so the leases are held until they
	// expire, as TestLeaseRetention_NothingEverReleases pins at the same log (#847).
	wantACKs := len(attachments)
	deadline := time.Now().Add(sameSubnetLeaseBudget)
	var acks int
	for {
		acks = fixture.CountLogLines("DHCPACK") - acksBefore
		if acks >= wantACKs {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq logged %d DHCPACK(s) for the refused container within %v, want at "+
				"least %d -- one per attached network. The troubleshooting row in "+
				"docs/reference.md (#847) tells operators the server issues a lease PER "+
				"NETWORK on the way to this failure, because the plugin leases in "+
				"CreateEndpoint and docker calls it once per attachment before libnetwork "+
				"refuses the second address. It also tells them a retried start burns one "+
				"address per network per attempt, which is only true if that is so. On this "+
				"run it was not -- fix the plugin, or correct the row and this floor "+
				"together; do not relax this assertion to a bare >= 1, which cannot tell "+
				"the row's claim from its negation",
				acks, sameSubnetLeaseBudget, wantACKs)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("\u2713 server issued %d lease(s) on the way to the refusal, for %d attached network(s)",
		acks, len(attachments))

	// leaseRetentionSettle is #800's own budget, so the two tests cannot drift apart.
	time.Sleep(leaseRetentionSettle)
	if releases := fixture.CountLogLines("DHCPRELEASE") - releasesBefore; releases != 0 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for the refused container, want 0.\n"+
			"A lease is a lease (#800): nothing releases on a network that does not set "+
			"release_lease, and this one does not, "+
			"and TestLeaseRetention_NothingEverReleases asserts the same thing at this "+
			"same log. A release appearing HERE means the refused-endpoint path grew one "+
			"that the other test's paths do not cover -- which is exactly the regression "+
			"#800 exists to catch, arriving through a door it does not watch. Do not "+
			"relax this into >= 0; fix the release, or retire #800 deliberately and "+
			"change both tests and the docs row together", releases)
	}
	t.Logf("\u2713 and released none of them: the addresses stay leased until they expire")
}
