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
// sameSubnetLeaseBudget bounds the wait for the ACKs to land in the
// server log. It is a budget for a POSITIVE event, so it exits early
// the moment one arrives -- unlike the release check below, which is
// an absence and must spend its wait in full.
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

	// The docs row does not stop at "it fails" -- it tells the reader what
	// the DHCP server will show, because the plugin leases in
	// CreateEndpoint, before libnetwork gets to the refusal. That half of
	// the row is a claim about the wire, and the plugin's own counters
	// cannot settle it: they prove what the plugin believes it did, not
	// what the server issued. Only the server's log does.
	//
	// THE ROW USED TO SAY "SHORT-LIVED ALLOCATION" AND THIS TEST USED TO
	// ASSERT IT, and both were wrong in the direction that costs an
	// operator something. Nothing this plugin runs sends a DHCPRELEASE, on
	// any path -- that is #800, and TestLeaseRetention_NothingEverReleases
	// pins it at this same server log. So the leases taken here are HELD
	// until the server expires them, and a reader told to expect a
	// transient allocation will not go and reclaim two addresses per
	// refused start.
	//
	// The contradiction was invisible for a measurable reason worth
	// recording: this branch was 15 commits behind dev, #800 landed in
	// that gap, and the branch's own tree still emitted the dhcpcd
	// `release` directive dev had removed. A run against the branch
	// worktree therefore saw a release and a run against the merge product
	// did not. Both readings were honest; only the merge product ships.
	// The dev merge that precedes this change is what makes the tree this
	// test is written in the same tree it will run in.
	//
	// Two halves, two shapes:
	//
	//   ACKs      a positive event -- poll, exit early when it lands.
	//   RELEASEs  an absence -- spend the settle window in full, because
	//             an absence declared early is a pass the tree has not
	//             earned. That is #800's own reasoning and its constant.
	deadline := time.Now().Add(sameSubnetLeaseBudget)
	var acks int
	for {
		acks = fixture.CountLogLines("DHCPACK") - acksBefore
		if acks >= 1 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq saw no DHCPACK for the refused container within %v. The "+
				"troubleshooting row in docs/reference.md (#847) tells operators the "+
				"server issues a lease on the way to this failure, because the plugin "+
				"leases in CreateEndpoint before libnetwork refuses. On this run it did "+
				"not, so the plugin no longer leases before the refusal -- fix that, or "+
				"correct the row; do not relax this assertion", sameSubnetLeaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("\u2713 server issued %d lease(s) on the way to the refusal", acks)

	// Now the absence. Sized by #800's own budget rather than a second
	// number of our own: the two tests assert the same property at the
	// same log, so they should not be able to drift apart, and referring
	// to the constant makes deleting that test a compile error here
	// rather than a silent loss of the contract.
	time.Sleep(leaseRetentionSettle)
	if releases := fixture.CountLogLines("DHCPRELEASE") - releasesBefore; releases != 0 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for the refused container, want 0.\n"+
			"A lease is a lease (#800): nothing this plugin runs releases, on any path, "+
			"and TestLeaseRetention_NothingEverReleases asserts the same thing at this "+
			"same log. A release appearing HERE means the refused-endpoint path grew one "+
			"that the other test's paths do not cover -- which is exactly the regression "+
			"#800 exists to catch, arriving through a door it does not watch. Do not "+
			"relax this into >= 0; fix the release, or retire #800 deliberately and "+
			"change both tests and the docs row together", releases)
	}
	t.Logf("\u2713 and released none of them: the addresses stay leased until they expire")
}
