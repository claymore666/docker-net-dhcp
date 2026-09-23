// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The IPAM-driver scenarios that break their DHCP server (#110, design rows 8 and 11) run on a per-test
// EphemeralFixture, in the failure suite.

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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Transcribed from pkg/plugin/ipam_reserve.go (ipamReserveLinkName), so the installed plugin cannot answer for itself.

// ipamReserveLinkPrefix is the name prefix of the reserve's temporary link.
const ipamReserveLinkPrefix = "dh-ipam-"

// A leftover link carries the endpoint's MAC on the parent, answers ARP for an address about to be given, and makes
// the next reserve's name collide (#110).

// assertNoReserveLinksLeft fails if any reservation link is still on the host.
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

// The daemon's IPAM client stops listening at `docker plugin enable --timeout` (30s default) and re-sends a call with
// no body, so a reserve running the default 34s lease_timeout would answer too late; the reserve is capped to the
// daemon's budget, and a lease_timeout of 40s must be announced and not change when the failure arrives (#110).

// TestFailure_IPAMServerDownFailsInsideTheBudget checks that with no DHCP server the reserve fails inside the daemon's IPAM budget (#110).
func TestFailure_IPAMServerDownFailsInsideTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// pluginCallBudget (30s) minus pluginCallMargin (4s), transcribed; the assertion is looser than the number.
	const reserveBudget = 26 * time.Second
	// Room for the calls around the reserve on a loaded runner, still short of the daemon's 30s.
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

	// Network create runs no exchange unless validate_dhcp is set.
	harness.CreateNetworkIPAM(t, ctx, "dh-itest-ipam-down", "macvlan", "",
		nil, map[string]string{"parent": harness.EphemeralHostVeth})
	// Two subnet-less IPAM networks derive the same PoolID and the second create is refused (design row 6); run
	// 34600486961 failure-2 hit `already holds pool 0.0.0.0/0` here. The option enters only the pool identity (#110).
	harness.CreateNetworkIPAM(t, ctx, "dh-itest-ipam-down-long", "macvlan", "",
		map[string]string{"parent": harness.EphemeralHostVeth},
		map[string]string{"parent": harness.EphemeralHostVeth, "lease_timeout": "40s"})

	// The preservation control, since a network that could never lease passes every refusal; and EphemeralFixture
	// requires one lease grant at teardown (checkLeaseGrants, #472).
	const controlName = "dh-itest-ipam-down-control"
	if err := ipamRunContainerErr(t, ctx, cli, "dh-itest-ipam-down", controlName, nil); err != nil {
		t.Fatalf("the control container could not start while the DHCP server was UP: %v\n"+
			"Nothing below would mean anything: the refusals this test is about would be "+
			"indistinguishable from a network that never worked.", err)
	}
	controlAddr, _ := ipamNetworkAddress(t, ctx, cli, controlName, "dh-itest-ipam-down")
	t.Logf("control: the same network leased %s while the server was up", controlAddr)
	if !strings.HasPrefix(controlAddr, "192.168.101.") {
		t.Errorf("the control came up at %s, which is not from the ephemeral fixture's pool, "+
			"so the exchange it proves ran against some other server", controlAddr)
	}

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
				t.Errorf("the failure took %s. The daemon stops listening at 30s and the call "+
					"it re-sends carries no body, so a reserve that overruns gives the "+
					"operator a parse error instead of a late answer.", elapsed.Round(time.Second))
			}
			if !strings.Contains(err.Error(), "reserve an address") {
				t.Errorf("the error is %q; it does not say the address reservation is what "+
					"failed, which is the one thing the operator needs from it", err)
			}
			assertNoReserveLinksLeft(t, "after a reservation that timed out")
		})
	}

	// Read from the window opened before the kill, since another IPAM test would satisfy a whole-log read.
	const capMarker = "Capping lease_timeout"
	window := harness.AwaitPluginLogSince(t, ctx, capMark, 10*time.Second,
		func(w string) bool { return strings.Contains(w, capMarker) })
	if !strings.Contains(window, capMarker) {
		t.Errorf("the plugin log carries no %q line for a network created with "+
			"lease_timeout=40s against a %s budget. The operator set a number and the plugin "+
			"used a different one.", capMarker, reserveBudget)
	}
}

// The daemon hands the same drained reader to every attempt (moby pkg/plugins/client.go, callWithRetry), so the
// re-send arrives with no body; run 34600486961 failure-1 logged `failed to parse request body: EOF` (#110). The
// invariants are a failure naming the timeout, one DHCP client per endpoint by distinct DISCOVER MACs, and a
// container that starts once the server is back; ipam_reserve_duplicate_mac is not reached by this re-send.

// TestFailure_IPAMResentRequestIsRefusedNotServedTwice checks that the daemon's body-less re-send is refused without a second DHCP client (#110).
func TestFailure_IPAMResentRequestIsRefusedNotServedTwice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-ipam-resend"
	const ctrName = "dh-itest-ipam-resend-ctr"
	// Shorter than the outage, or the re-send never happens.
	const pluginTimeout = 5
	const outage = 8 * time.Second

	ef := harness.NewEphemeralFixture(t)
	// Opened before anything starts, so one exchange is told apart from a capture that never saw the endpoint.
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

	// Registered before the disable, so a failure below still restores the stock timeout.
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
		// No t.Fatalf off the test goroutine: it would report against whichever test is running.
		startErr <- cli.ContainerStart(context.Background(), create.ID, container.StartOptions{})
	}()

	time.Sleep(outage)
	ef.StartAgain()
	t.Logf("server back after %s; the daemon has re-sent RequestAddress at least once by now", outage)

	var startFailure error
	select {
	case err := <-startErr:
		startFailure = err
	case <-ctx.Done():
		t.Fatal("ContainerStart never returned")
	}
	if startFailure == nil {
		t.Fatalf("the container started. The daemon gave up at %ds and re-sent a call whose "+
			"body it had already spent, so there was nothing for the plugin to answer; a "+
			"container that came up anyway holds an address no request in this exchange "+
			"asked for.", pluginTimeout)
	}
	t.Logf("refused, as it must be: %v", startFailure)
	// The plugin never learns the --timeout value, so the message must say it is below the 26s a reservation needs (#110).
	for _, want := range []string{"no body", "--timeout", "30s", "BELOW"} {
		if !strings.Contains(startFailure.Error(), want) {
			t.Errorf("the failure the operator sees does not mention %q. The cause is a call "+
				"that outlived the plugin call timeout; a decoder error in its place reads "+
				"like a protocol defect in the plugin and points at nothing to change.", want)
		}
	}

	// A client draws a fresh xid when it reverts to INIT (RFC 2131 section 3.1(5); dhcp-golib beginAcquisition), but its
	// MAC does not move, and a reserve driven off the empty body would have to invent one; so clients are counted by MAC.
	frames := wire.Frames()
	macs := map[string]int{}
	xids := map[uint32]struct{}{}
	for _, m := range frames {
		if m.Type == harness.DHCPDiscover {
			macs[m.ClientMAC.String()]++
			xids[m.XID] = struct{}{}
		}
	}
	if len(macs) == 0 {
		wire.Dump(func(s string) { t.Log(s) })
		t.Fatalf("no DISCOVER reached the wire at all, so this instrument never saw the "+
			"endpoint under test and the count below would be a statement about nothing. "+
			"The capture holds %d client message(s).", len(frames))
	}
	t.Logf("%d DISCOVER MAC(s) %v over %d transaction id(s)", len(macs), macs, len(xids))
	if len(macs) != 1 {
		wire.Dump(func(s string) { t.Log(s) })
		t.Errorf("%d distinct DISCOVER client MACs (%v) for ONE endpoint. Each reserve puts "+
			"the endpoint's own MAC on its link, so a second MAC is a second reserve: two "+
			"leases on the server, one container, and the spare never released.",
			len(macs), macs)
	}
	assertNoReserveLinksLeft(t, "after a reservation the daemon stopped waiting for")

	// At --timeout 5 no reserve can finish: RFC 5227's three probes 1 to 2s apart span about 6s (roleAcquire under
	// ConflictWait), so the retry needs the stock timeout (#110).
	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable before the retry: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state before the retry: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable back at the stock timeout: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable at the stock timeout: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)
	t.Log("plugin back at the stock 30s client timeout for the retry")

	// An abandoned reservation is retained and its records live on disk, so the recycled process still finds the
	// candidate; the assertion is the container starting, since the address is the server's choice (#110).
	if err := cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		t.Fatalf("the container did not start on the retry, with the server back: %v\n"+
			"A reservation the daemon gave up on has left this endpoint unable to get an "+
			"address at all, which is worse than the failed run it came from.", err)
	}
	addr, mac := ipamNetworkAddress(t, ctx, cli, create.ID, netName)
	t.Logf("the retry came up at %s with MAC %s", addr, mac)
	if !strings.HasPrefix(addr, "192.168.101.") {
		t.Errorf("the retry published %s, which is not from the ephemeral fixture's pool", addr)
	}
}
