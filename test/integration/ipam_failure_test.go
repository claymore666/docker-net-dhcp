// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The two IPAM-driver scenarios that need a DHCP server they are
// allowed to break (#110, design rows 8 and 11). Both run against a
// per-test EphemeralFixture, never the suite-static one, and both are
// named TestFailure_ so they land in the failure suite where a test is
// permitted to spend real seconds waiting.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// ipamReserveLinkPrefix is the name the reserve gives its temporary
// link (pkg/plugin/ipam_reserve.go, ipamReserveLinkName). Transcribed
// rather than imported because this suite asks what the INSTALLED
// plugin left on the host, and a constant imported from the plugin
// would make the question answer itself.
const ipamReserveLinkPrefix = "dh-ipam-"

// assertNoReserveLinksLeft fails if any reservation link is still on
// the host.
//
// The link carries the endpoint's MAC and sits on the parent, so one
// left behind is not merely litter: it answers ARP for an address a
// container is about to be given, and the next reserve on that parent
// meets a name collision it cannot explain. A failed reserve is exactly
// when a cleanup path is least likely to have run, which is why the
// assertion belongs to the failure tests and not to the happy ones.
func assertNoReserveLinksLeft(t *testing.T, when string) {
	t.Helper()
	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	for _, l := range links {
		if strings.HasPrefix(l.Attrs().Name, ipamReserveLinkPrefix) {
			t.Errorf("reservation link %s is still on the host %s. It carries the endpoint's "+
				"MAC on the parent, so it answers ARP for an address nothing holds.",
				l.Attrs().Name, when)
		}
	}
}

// TestFailure_IPAMServerDownFailsInsideTheBudget is design row 8.
//
// With no DHCP server the reserve cannot answer, and WHEN it gives up
// is the whole test. The daemon's IPAM client stops listening at
// `docker plugin enable --timeout` (30s by default) and RE-SENDS the
// same body rather than waiting, so a reserve that ran the network's
// own lease_timeout -- 34s by default, which is the conflict-recovery
// window -- would hand the operator a Docker-side timeout while the
// plugin was still working, and a second DHCP exchange underneath it.
// The reserve is therefore capped to the daemon's budget, and this is
// the test that the cap is real rather than a comment.
//
// The second network drives the cap explicitly: lease_timeout=40s is
// longer than the budget, must be announced in the log, and must not
// change when the failure arrives.
func TestFailure_IPAMServerDownFailsInsideTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The budget the plugin gives one reservation: pluginCallBudget
	// (30s) minus pluginCallMargin (4s). Transcribed, and the
	// assertion below is deliberately looser than the number -- what
	// is being tested is that the failure lands inside the daemon's
	// patience, not that it lands on a particular second.
	const reserveBudget = 26 * time.Second
	// Room for the container create, the network calls around it and a
	// loaded runner. Still far short of the 30s at which the daemon
	// stops listening, which is the boundary that matters.
	const ceiling = reserveBudget + 8*time.Second

	ef := harness.NewEphemeralFixture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Created while the server is up: `docker network create` runs no
	// exchange unless validate_dhcp is set, and a create that failed
	// for its own reasons would tell us nothing about the reserve.
	harness.CreateNetworkIPAM(t, ctx, "dh-itest-ipam-down", "macvlan", "",
		nil, map[string]string{"parent": harness.EphemeralHostVeth})
	harness.CreateNetworkIPAM(t, ctx, "dh-itest-ipam-down-long", "macvlan", "",
		nil, map[string]string{"parent": harness.EphemeralHostVeth, "lease_timeout": "40s"})

	capMark := harness.MarkPluginLog(t, ctx)
	ef.Stop()
	t.Log("DHCP server killed; every reservation from here on has nobody to answer it")

	for _, tc := range []struct {
		name    string
		netName string
	}{
		{"default lease_timeout", "dh-itest-ipam-down"},
		{"lease_timeout=40s, longer than the budget", "dh-itest-ipam-down-long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			err := ipamRunContainerErr(t, ctx, cli, tc.netName, tc.netName+"-ctr", nil)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("a container started on a network whose DHCP server is dead. Docker's " +
					"store would be publishing an address nothing granted.")
			}
			t.Logf("refused after %s: %v", elapsed.Round(time.Millisecond), err)
			if elapsed > ceiling {
				t.Errorf("the failure took %s. The daemon stops listening at 30s and re-sends "+
					"the same request, so a reserve that overruns produces a second DHCP "+
					"exchange for one endpoint instead of a late answer.", elapsed.Round(time.Second))
			}
			if !strings.Contains(err.Error(), "reserve an address") {
				t.Errorf("the error is %q; it does not say the address reservation is what "+
					"failed, which is the one thing the operator needs from it", err)
			}
			assertNoReserveLinksLeft(t, "after a reservation that timed out")
		})
	}

	// The cap is announced for the network that asked for more than the
	// budget. A cap applied silently is a number the operator set and
	// the plugin ignored.
	//
	// Read from the window opened before the server was killed, not
	// from the whole log: another IPAM-mode test on this plugin would
	// otherwise satisfy the assertion for this one.
	const capMarker = "Capping lease_timeout"
	window := harness.AwaitPluginLogSince(t, ctx, capMark, 10*time.Second,
		func(w string) bool { return strings.Contains(w, capMarker) })
	if !strings.Contains(window, capMarker) {
		t.Errorf("the plugin log carries no %q line for a network created with "+
			"lease_timeout=40s against a %s budget. The operator set a number and the plugin "+
			"used a different one.", capMarker, reserveBudget)
	}
}

// TestFailure_IPAMResentRequestJoinsTheReserve is design row 11, from
// defeat row 14.
//
// The daemon's IPAM client does not wait: when its timeout expires it
// RE-SENDS the same RequestAddress body, after 1s, 2s, 4s, until 30s
// have passed. A reserve that treated the second body as a new request
// would run a second DHCP exchange for one endpoint -- two DISCOVERs,
// two leases on the server, one container, and the second lease never
// released because nothing knows it exists.
//
// The provocation is the real one: the plugin is re-enabled with
// `--timeout 5` so the daemon gives up at five seconds, and the server
// is held down for eight so the first exchange is still running when
// the second body arrives.
//
// THE EVIDENCE IS THE WIRE. RFC 2131 section 4.1 requires a client to
// retransmit under the same 'xid', so the number of distinct
// transaction ids among the DISCOVERs from this endpoint's MAC is the
// number of DHCP clients that ran for it. One is the whole assertion.
func TestFailure_IPAMResentRequestJoinsTheReserve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-ipam-resend"
	const ctrName = "dh-itest-ipam-resend-ctr"
	// Shorter than the outage below, so the daemon's client really does
	// give up while the exchange is still in flight. That inequality is
	// the test: raise it above the outage and the re-send never happens
	// and this test passes having provoked nothing.
	const pluginTimeout = 5
	const outage = 8 * time.Second

	ef := harness.NewEphemeralFixture(t)
	// Opened before anything starts: a capture opened later could not
	// tell "this endpoint ran one exchange" from "this instrument never
	// saw this endpoint".
	wire := ef.StartDHCPCapture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			wire.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Registered before the disable, so a failure anywhere below still
	// leaves the plugin enabled at the stock timeout for every test
	// that follows. Idempotent; already-enabled is fine.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil &&
			!strings.Contains(err.Error(), "already enabled") {
			t.Logf("WARN: cleanup PluginEnable: %v", err)
		}
		_ = harness.WaitPluginEnabled(bg, cli, true, 30*time.Second)
	})

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: pluginTimeout}); err != nil {
		t.Fatalf("PluginEnable with --timeout %d: %v", pluginTimeout, err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)
	t.Logf("plugin re-enabled with a %ds client timeout", pluginTimeout)

	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "",
		nil, map[string]string{"parent": harness.EphemeralHostVeth})

	// The counter window opens on the plugin that will serve the
	// request, after the recycle, so the delta below belongs to one
	// process.
	w := harness.BeginCounterWindow(t, ctx, cli, "ipam_reserve_joined")

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, create.ID, container.StopOptions{})
		_ = cli.ContainerRemove(bg, create.ID, container.RemoveOptions{Force: true})
	})

	ef.Stop()
	startErr := make(chan error, 1)
	go func() {
		// No t.Fatalf from here: a test helper called off the test
		// goroutine reports against whichever test is running when it
		// fires, which is how a failure ends up blamed on its
		// neighbour.
		startErr <- cli.ContainerStart(context.Background(), create.ID, container.StartOptions{})
	}()

	time.Sleep(outage)
	ef.StartAgain()
	t.Logf("server back after %s; the daemon has re-sent RequestAddress at least once by now", outage)

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("the container never started: %v\nThe point of this test is a container "+
				"that comes up DESPITE the re-sent request; a failure here is the feature "+
				"missing, not the provocation failing.", err)
		}
	case <-ctx.Done():
		t.Fatal("ContainerStart never returned")
	}

	addr, mac := ipamNetworkAddress(t, ctx, cli, create.ID, netName)
	t.Logf("container up at %s with MAC %s", addr, mac)

	// --- outside evidence: how many DHCP clients ran for this MAC.
	frames := wire.FramesFrom(mac)
	xids := map[uint32]int{}
	for _, m := range frames {
		if m.Type == harness.DHCPDiscover {
			xids[m.XID]++
		}
	}
	if len(xids) == 0 {
		wire.Dump(func(s string) { t.Log(s) })
		t.Fatalf("no DISCOVER from %s reached the wire at all, so this instrument never saw "+
			"the endpoint under test and the count below would be a statement about nothing. "+
			"The capture holds %d client message(s).", mac, len(wire.Frames()))
	}
	if len(xids) != 1 {
		wire.Dump(func(s string) { t.Log(s) })
		t.Errorf("%d distinct DISCOVER transaction ids from %s (%v). RFC 2131 section 4.1 "+
			"requires retransmissions to carry the same xid, so more than one means more than "+
			"one DHCP client ran for a single endpoint: two leases on the server, one "+
			"container, and the spare never released.", len(xids), mac, xids)
	}

	before, after := w.End()
	if after.IPAMReserveJoined <= before.IPAMReserveJoined {
		t.Errorf("ipam_reserve_joined did not move (%d -> %d). With a %ds client timeout and a "+
			"%s outage the daemon must have re-sent the request, so either the provocation "+
			"did not happen -- in which case the wire assertion above proved nothing -- or "+
			"the second body was served without joining the first exchange.",
			before.IPAMReserveJoined, after.IPAMReserveJoined, pluginTimeout, outage)
	}
	assertNoReserveLinksLeft(t, "after a reservation that was joined by a re-sent request")
}
