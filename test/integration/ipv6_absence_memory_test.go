// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The saving is the 6 s window (the library's timer adds the Solicit delay to it) less 3 s, three times the 0.96 s
// attach noise: 28 pool pairs, runs 37091993592 to 37117065629, saving mean 6.46 s, sd 1.00 s (#1172).
const (
	absenceWindow       = time.Duration(proto.DefaultAutoFallback)
	attachNoise         = 3 * time.Second
	absenceMemorySaving = absenceWindow - attachNoise
)

// solicitQuiet outlasts one client's whole Solicit run: the 1 s delay, the 6 s window and a retransmission (#1038).
const solicitQuiet = 10 * time.Second

// TestSLAAC_AutoRemembersASilentServer checks that a second auto endpoint on a network whose server stayed silent
// skips the Solicit at both of its clients, saving the fallback window (#1038).
func TestSLAAC_AutoRemembersASilentServer(t *testing.T) {
	testSLAAC_AutoRemembersASilentServer(t, onV6Bridge)
}

func TestSLAAC_AutoRemembersASilentServer_Macvlan(t *testing.T) {
	testSLAAC_AutoRemembersASilentServer(t, onV6Macvlan)
}

func TestSLAAC_AutoRemembersASilentServer_IPAM(t *testing.T) {
	testSLAAC_AutoRemembersASilentServer(t, onV6IPAMBridge)
}

func testSLAAC_AutoRemembersASilentServer(t *testing.T, at v6Attach) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6AutoFallback)
	dumpOnFailure(t, f)

	netName := at.net("dh-itest-absmem")
	opts := map[string]string{"ipv6_mode": "auto", "propagate_dns": "true"}
	if at.mode == "macvlan" {
		opts["parent"] = f.MacvlanParent()
	} else {
		opts["bridge"] = f.Bridge()
	}
	at.createNet(t, ctx, netName, opts)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"dhcpv6_auto_fallbacks", "dhcpv6_absence_remembered", "ipv6_slaac_addresses")

	first, firstWall := startTimedOn(t, ctx, cli, netName, netName+"-ctr1")
	assertAttachedAs(t, ctx, f, first, at)
	awaitPluginAppliedV6(t, ctx, w, first, v6SegmentPrefix(t), slaacAddrBudget(), v6InstalledSinceBaseline)
	f.AssertExchange(60 * time.Second)

	// The first endpoint's persistent client runs auto too and solicits once more; the count is read once it is done.
	solicits := awaitSolicitsQuiet(t, f)

	second, secondWall := startTimedOn(t, ctx, cli, netName, netName+"-ctr2")
	addr, _ := awaitPluginAppliedV6(t, ctx, w, second, v6SegmentPrefix(t), slaacAddrBudget(),
		func(now, before *harness.HealthResponse) bool {
			return now.IPv6SLAACAddresses >= before.IPv6SLAACAddresses+2
		})
	t.Logf("first attach %s, second attach %s", firstWall.Round(100*time.Millisecond),
		secondWall.Round(100*time.Millisecond))
	if saved := firstWall - secondWall; saved < absenceMemorySaving {
		t.Errorf("the second endpoint started %s faster than the first, want at least %s (the %s "+
			"fallback window less %s of attach noise). The first one waited out the window on "+
			"a server that answers nothing; the second one is on the same network inside "+
			"DHCPV6_ABSENCE_MEMORY and should not have solicited at all",
			saved, absenceMemorySaving, absenceWindow, attachNoise)
	}

	if after := awaitSolicitsQuiet(t, f); after != solicits {
		t.Errorf("the server logged %d DHCPv6 Solicits after the second endpoint started, want "+
			"none: the network remembered that nothing answers, so neither that endpoint's "+
			"attach nor its persistent client should ask again", after-solicits)
	}
	if now, _ := awaitContainerV6(t, ctx, second, v6SegmentPrefix(t), formedAddrReadFloor); now != addr {
		t.Errorf("the second endpoint's address moved from %s to %s after a settle; it formed "+
			"from the advertised prefix and nothing on this segment hands out another", addr, now)
	}

	before, after := w.End()
	if n := after.DHCPv6AbsenceRemembered - before.DHCPv6AbsenceRemembered; n != 1 {
		t.Errorf("dhcpv6_absence_remembered moved by %d, want 1: one endpoint attached while "+
			"the network remembered its silent server", n)
	}
	if n := after.DHCPv6AutoFallbacks - before.DHCPv6AutoFallbacks; n != 1 {
		t.Errorf("dhcpv6_auto_fallbacks moved by %d, want 1: only the first endpoint solicited "+
			"and fell back", n)
	}
}

// startTimedOn starts a container on an existing network and returns its ID and how long ContainerStart took, which
// is where the engine calls CreateEndpoint and the endpoint's DHCPv6 runs (#1038).
func startTimedOn(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName string) (string, time.Duration) {
	t.Helper()
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", ctrName, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})
	start := time.Now()
	if err := cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(%s) on an ipv6_mode=auto segment with a silent server and an "+
			"autonomous prefix: %v", ctrName, err)
	}
	return create.ID, time.Since(start)
}

// awaitSolicitsQuiet returns the server's Solicit count once it has not moved for solicitQuiet (#1038).
func awaitSolicitsQuiet(t *testing.T, f *harness.V6Fixture) int {
	t.Helper()
	deadline := time.Now().Add(6 * solicitQuiet)
	n, since := f.CountLogLines("DHCPSOLICIT"), time.Now()
	for time.Since(since) < solicitQuiet {
		if !time.Now().Before(deadline) {
			t.Fatalf("the server's Solicit count was still moving after %s (now %d)", 6*solicitQuiet, n)
		}
		time.Sleep(500 * time.Millisecond)
		if m := f.CountLogLines("DHCPSOLICIT"); m != n {
			n, since = m, time.Now()
		}
	}
	return n
}
