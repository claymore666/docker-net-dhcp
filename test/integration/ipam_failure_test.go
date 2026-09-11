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
	// `--ipam-opt parent=` on the second one, and not for decoration:
	// two subnet-less IPAM networks derive the same PoolID, and the
	// driver refuses the second create with exactly the remedy this
	// line is (design row 6). Without it this test's own setup was the
	// thing that tripped the rule -- MEASURED in integration run
	// 34600486961, failure-2: `network 0c0f76ccc6e5 already holds pool
	// 0.0.0.0/0`. The option goes into the pool identity and nowhere
	// else; the interface the reservation runs on is still the driver
	// option below.
	harness.CreateNetworkIPAM(t, ctx, "dh-itest-ipam-down-long", "macvlan", "",
		map[string]string{"parent": harness.EphemeralHostVeth},
		map[string]string{"parent": harness.EphemeralHostVeth, "lease_timeout": "40s"})

	// One container that SUCCEEDS, before the server is killed.
	//
	// Two jobs, and neither is decoration. It is the preservation
	// control: every assertion below is about a refusal, and a refusal
	// proves nothing unless the same network, the same parent and the
	// same reserve path can be shown to work when the server is there.
	// Without it a network that never could have leased an address --
	// wrong parent, wrong pool -- passes this test perfectly.
	//
	// And it is what the fixture itself requires. EphemeralFixture
	// checks at teardown that the server logged at least one lease
	// allocation (harness/ephemeral.go, checkLeaseGrants, #472),
	// because a fixture that granted nothing is one whose timings were
	// never confirmed against the server. A test whose every exchange
	// is meant to fail has to produce that one grant rather than have
	// the check relaxed for everybody else.
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

// TestFailure_IPAMResentRequestIsRefusedNotServedTwice is design row 11,
// from defeat row 14, and it asserts the opposite of what that row
// predicted.
//
// The row said the daemon RE-SENDS the same RequestAddress body when
// its client timeout expires, and that a reserve treating the second
// body as a new request would run a second DHCP exchange for one
// endpoint. The first half is true and the second cannot happen: the
// daemon encodes the call into a bytes.Buffer and hands the SAME
// reader to every attempt (moby pkg/plugins/client.go, callWithRetry),
// so the first attempt drains it and the re-send arrives with NO BODY.
// There is nothing in it to identify an endpoint with, let alone to
// join an exchange with. MEASURED in integration run 34600486961,
// failure-1: `IpamDriver.RequestAddress: failed to parse request body:
// EOF`, and the container did not start.
//
// So the reachable invariants are these three, and they are what the
// daemon's re-send actually costs an operator:
//
//   - the run FAILS, with a message that names the timeout and the
//     re-send rather than a decoder error;
//   - exactly ONE DHCP client ran for the endpoint, counted as the
//     number of distinct DISCOVER client MACs on the segment. Two
//     would be two leases on the server, one container, and the spare
//     never released;
//   - the address is not wedged: with the server back, starting the
//     same container again comes up.
//
// The provocation is the real one: the plugin is re-enabled with
// `--timeout 5` so the daemon gives up at five seconds, and the server
// is held down for eight so the first exchange is still running when
// the second body arrives.
//
// ipam_reserve_joined is NOT asserted here any more, and cannot be: the
// empty re-send is refused before any handler sees it, so nothing
// reaches the join. The counter stays for the concurrency it was
// written for and the handover records that the daemon's own re-send
// does not reach it.
func TestFailure_IPAMResentRequestIsRefusedNotServedTwice(t *testing.T) {
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
	for _, want := range []string{"no body", "--timeout"} {
		if !strings.Contains(startFailure.Error(), want) {
			t.Errorf("the failure the operator sees does not mention %q. The cause is a call "+
				"that outlived the plugin call timeout; a decoder error in its place reads "+
				"like a protocol defect in the plugin and points at nothing to change.", want)
		}
	}

	// --- outside evidence: how many DHCP clients ran for this endpoint.
	//
	// The count is of distinct DISCOVER client MACs, not of transaction
	// ids. A single client legitimately draws a fresh xid when its
	// retransmission budget runs out and it reverts to INIT (RFC 2131
	// section 3.1(5), and dhcp-golib proto/machine.go beginAcquisition
	// on the exhausted branch), and an eight-second outage is long
	// enough to reach that. The MAC does not move under a client: one
	// reserve builds one link with the endpoint's MAC on it, and a
	// reserve driven off the EMPTY re-sent body has no MAC to use and
	// must invent one -- so a second address served for this endpoint
	// shows up here as a second MAC, which is the defect this test is
	// about.
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

	// Back to the stock timeout BEFORE the retry.
	//
	// MEASURED: at --timeout 5 no reserve can finish, server up or not.
	// The reserve's own ARP Probe schedule (RFC 5227: three probes, 1-2s
	// apart, spread over roughly 6s -- pkg/plugin/conflict.go, roleAcquire
	// under ConflictWait) outlasts the five-second client budget on its
	// own, so the retry below would fail with the same "no body" error
	// and the failure would say nothing about the address being wedged.
	// The wedge this assertion is about is the plugin's; five seconds is
	// the operator's, and leaving it in place would let the operator's
	// setting answer for the plugin's.
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

	// The address is not wedged. A reservation the daemon abandoned is
	// retained rather than closed, so the retry inside the tombstone
	// window claims it back; what is asserted is the user-visible half
	// -- the same container starts -- because which address the server
	// hands a returning client is the server's to decide.
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
