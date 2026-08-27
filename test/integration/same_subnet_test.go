// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestMultiNetwork_SameSubnetRefused pins a limitation the docs now
// state (#847): one container on two plugin networks that lease from
// the SAME subnet cannot start. libnetwork refuses to program a second
// sandbox address in a subnet the container already routes.
//
// This is not our refusal and not a defect here, which is exactly why
// it needs a check rather than a paragraph. The troubleshooting row in
// docs/reference.md is a claim about someone else's behaviour; if
// libnetwork ever permits this, that row silently becomes wrong. This
// test is what goes red instead.
//
// It deliberately does NOT probe the engine and skip. The refusal was
// measured on two daemons differing only in moby/moby#52866 and with
// and without the endpoint interface-name option — four cells, one
// identical error — so there is no engine on which this is expected to
// pass, and a skip here would only hide a change we want to hear about.
// sameSubnetLeaseBudget bounds the wait for the lease-and-release pair
// to land in the server log. The release is handed back from the
// plugin's orphan path once the container goes away, so it trails the
// ContainerStart error rather than preceding it.
const sameSubnetLeaseBudget = 30 * time.Second

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

	// Both networks on the suite fixture's parent, so both lease from
	// 192.168.99.0/24. That sameness is the whole point of the test.
	netA := "dh-itest-samesubnet-a"
	netB := "dh-itest-samesubnet-b"
	harness.CreateNetwork(t, ctx, netA, "macvlan", nil)
	harness.CreateNetwork(t, ctx, netB, "macvlan", nil)

	// Snapshot the server BEFORE anything is created. The shared fixture
	// accumulates every test's traffic, so only a delta says anything
	// about this container.
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
		// Record the finding BEFORE gathering evidence for it. This arm
		// fires exactly when someone else's behaviour changed under us,
		// and harness.ExecOutput calls t.Fatalf itself on a client,
		// ExecCreate or ExecAttach error -- which aborts this goroutine
		// on the spot. Gathering first would mean that on any exec
		// stumble the reader sees "ExecCreate: ..." and never sees the
		// sentence this whole test exists to print. Errorf keeps the
		// message even if the exec below aborts.
		t.Errorf("two networks on one subnet attached successfully — libnetwork no longer " +
			"refuses a second address in an already-routed subnet. Update the " +
			"troubleshooting row in docs/reference.md (#847) rather than this test.")
		t.Logf("container addresses:\n%s",
			harness.ExecOutput(t, ctx, create.ID, "ip", "-o", "-4", "addr"))
		t.FailNow()
	}

	// Assert on the reason, not merely on failure: any broken fixture
	// also fails ContainerStart, and that would let this test pass
	// while proving nothing.
	if !strings.Contains(err.Error(), "conflicts with existing route") {
		t.Fatalf("ContainerStart failed for the wrong reason.\nwant substring: %q\ngot: %v",
			"conflicts with existing route", err)
	}
	t.Logf("✓ refused as documented: %v", err)

	// The docs row does not stop at "it fails" — it tells the reader the
	// DHCP server will show a short-lived allocation, because the plugin
	// leases in CreateEndpoint, before libnetwork gets to the refusal.
	// That half of the row is a claim about the wire, and the plugin's
	// own counters cannot settle it: they prove the plugin believes it
	// leased and released, not that the server issued and reclaimed.
	// Only the server's log does. Without this the row could go wrong in
	// the direction that matters -- an operator told to expect a
	// transient allocation, chasing one that never appears -- and
	// nothing here would notice.
	deadline := time.Now().Add(sameSubnetLeaseBudget)
	for {
		acks := fixture.CountLogLines("DHCPACK") - acksBefore
		releases := fixture.CountLogLines("DHCPRELEASE") - releasesBefore
		if acks >= 1 && releases >= 1 {
			t.Logf("✓ server saw the short-lived allocation: %d DHCPACK, %d DHCPRELEASE", acks, releases)
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq saw %d DHCPACK and %d DHCPRELEASE for the refused container "+
				"within %v, want at least one of each. The troubleshooting row in "+
				"docs/reference.md (#847) tells operators the server will show a "+
				"short-lived allocation on the way to this failure; on this run it did "+
				"not, so either the lease is now leaked (no release) or the plugin no "+
				"longer leases before the refusal. Fix whichever it is, or correct that "+
				"sentence in the row -- do not relax this assertion",
				acks, releases, sameSubnetLeaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
