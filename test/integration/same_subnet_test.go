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

// sameSubnetLeaseBudget bounds the wait for the ACKs to land in the
// server log. It is a budget for a POSITIVE event, so it exits early
// the moment the floor is met -- unlike the release check below, which
// is an absence and must spend its wait in full.
const sameSubnetLeaseBudget = 30 * time.Second

// TestMultiNetwork_SameSubnetRefused pins a limitation the docs now
// state (#847): one container on two plugin networks that lease from
// OVERLAPPING subnets cannot start. libnetwork refuses to program a
// second sandbox address in a subnet the container already routes, and
// its check is containment in either direction, not equality -- this
// test drives the identical-subnet case, which is the one an operator
// reaches by accident.
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
//
// It also drives macvlan, which is the mode in which the docs row's
// configuration has no guard in front of it. Bridge mode -- the
// default -- refuses two networks on one bridge much earlier, at
// CreateNetwork with util.ErrBridgeUsed, so the same-parent phrasing
// of this failure is macvlan/ipvlan's alone unless the operator set
// ignore_conflicts. That boundary is in the row; the test sits on the
// side of it that can actually reach the symptom.
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
	// attachments is the population the docs row's "a real lease per
	// network" claim is quantified over, kept as a list so the ACK floor
	// below is derived from it rather than typed as a literal.
	attachments := []string{netA, netB}
	for _, n := range attachments {
		harness.CreateNetwork(t, ctx, n, "macvlan", nil)
	}

	// Snapshot the server BEFORE anything is created. The shared fixture
	// accumulates every test's traffic, so only a delta says anything
	// about this container.
	//
	// The delta is scoped to time, not to this container: neither count
	// below is keyed on its MAC or its address, because this container
	// never starts and so never has an inspectable one — the endpoints
	// are rolled back with the failed start. The two counts therefore
	// have OPPOSITE polarities under a foreign line, and only one of
	// them is dangerous:
	//
	//	DHCPRELEASE  wants an ABSENCE, so a foreign line can only fail
	//	             a healthy tree — noisy, never silently wrong.
	//	DHCPACK      wants a PRESENCE, so a foreign line SATISFIES it.
	//	             That is a false pass: the test would report "the
	//	             server issued leases for this container" on another
	//	             container's evidence.
	//
	// Latent today, and these are the conditions that make it live:
	// nothing in this suite calls t.Parallel, every test cleans up its
	// own containers, and the fixture lease time (2m) outlasts the 30s
	// window below only for a container that is still running. Add
	// suite parallelism, or a fixture container that lives across
	// tests and renews, and the ACK check starts being satisfiable by
	// traffic that is not ours — at which point it needs the MAC, which
	// means pinning the endpoint MACs at create time so there is one to
	// key on before the start fails.
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
	// This branch is kept rebased on dev for that reason: it is what
	// makes the tree this test is written in the same tree it will run
	// in.
	//
	// Two halves, two shapes:
	//
	//   ACKs      a positive event -- poll, exit early when it lands.
	//   RELEASEs  an absence -- spend the settle window in full, because
	//             an absence declared early is a pass the tree has not
	//             earned. That is #800's own reasoning and its constant.
	//
	// The floor is len(attachments), not 1, and the difference is the
	// whole of the row's claim. "A real lease PER NETWORK" and "a lease
	// on the way to the refusal" are different statements, and only the
	// first supports the operational warning the row then gives (a
	// retried start burns one address per network per attempt). A floor
	// of 1 cannot tell them apart: two networks and one ACK would pass
	// while the row had become wrong.
	//
	// len(attachments) is what the behaviour REQUIRES, not a number
	// observed on a run. The plugin acquires in CreateEndpoint; docker
	// calls CreateEndpoint once per attached network; and the refusal
	// happens later, in libnetwork's sandbox setup for the SECOND
	// network -- the first joins an empty netns and cannot conflict with
	// anything. So every attachment's CreateEndpoint has already run and
	// already leased by the time the start fails, and each of those is a
	// distinct macvlan child with its own MAC, hence a distinct client
	// and a distinct ACK at the server. A retried DISCOVER can only add
	// ACKs, never remove one, which is why this is a floor and not an
	// equality.
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
