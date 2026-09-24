// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// Runtime failure injection against a per-test EphemeralFixture (#128). Kea honours any lease, so each test states
// the lease inequality its boundary needs (#356). The library emits Failed{ReasonNoServer} only on acquisition; a bound
// client's dhcp_timeouts rises when the lease lapses (Lost{ReasonExpired}), and renewals_unanswered moves at the first
// retransmission since v2.1.0 (#940). The plugin keeps the address through an outage. Each test waits for the
// persistent client's own bind before the kill, or a failing first acquisition passes for an expiry, and pairs every
// plugin-wide counter with the endpoint's own log line as a delta (#278).
package integration

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// Plugin log messages beside the counter bumps in handleEvent, carrying the endpoint field the counters lack (#278).
const (
	logLeaseFail = "dhcp failed to get a lease"
	logIPChanged = "dhcp renew with changed IP"

	// The first rise is one lease lapsing, measured at t+20s on a 20s lease in run 33773687839.

	// outageRiseBudget bounds the wait for the first dhcp_timeouts rise after a bound client's server dies.
	outageRiseBudget = 120 * time.Second

	// proto.DefaultBackoff() (4s doubling to 64s, four retransmissions) puts sends at +0, +4, +12, +28 and +60, and
	// exhaustion is tested when the next delay fires: 124s worst case plus jitter. Run 33773687839 measured 80s between
	// rises, which raced the old 80s budget (#899).

	// outageRecurBudget bounds the wait for every later dhcp_timeouts rise, one exhausted DISCOVER transaction apart.
	outageRecurBudget = 180 * time.Second
)

// healthy derives from plugin-wide lifetime counters and one plugin serves the whole job, so an earlier unrelated
// fault failed every absolute health check here (#373, #278).

// assertNoNewHealthFaults fails the test if this window introduced a fault, whatever came before it.
func assertNoNewHealthFaults(t *testing.T, w *harness.CounterWindow, what string) {
	t.Helper()
	// Closing the window proves the plugin never restarted across it (#405), and reads the climbing counters last.
	base, now := w.End()
	if base == nil || now == nil {
		return
	}
	if d := now.RecoveryFailed - base.RecoveryFailed; d > 0 {
		t.Errorf("%s: recovery_failed rose by %d during this test", what, d)
	}
	if d := now.JoinStartFailures - base.JoinStartFailures; d > 0 {
		t.Errorf("%s: join_start_failures rose by %d during this test — a running container was left without a renewal client", what, d)
	}
	if d := now.TombstoneWriteFailures - base.TombstoneWriteFailures; d > 0 {
		t.Errorf("%s: tombstone_write_failures rose by %d during this test", what, d)
	}
}

// awaitBoundPersistentClient blocks until the plugin records a bind beyond the window's baseline, so the long-lived client holds its own lease (#278).
func awaitBoundPersistentClient(t *testing.T, w *harness.CounterWindow) {
	t.Helper()
	if _, ok := w.Await(45*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.LeasesObtained > before.LeasesObtained
	}); !ok {
		t.Fatal("persistent client never confirmed its own bind; the failure below would land on an acquiring client, not a bound one (#278)")
	}
	w.End()
}

// Run 33773687839 showed the watchdog strings are emitted nowhere in the tree, so only the leasefail count remains.

// outageLines counts one endpoint's plugin-log leasefail records, the event that bumps dhcp_timeouts.
func outageLines(t *testing.T, ctx context.Context, endpoint string) (leasefail int) {
	t.Helper()
	return harness.CountPluginLogLines(t, ctx, endpoint, logLeaseFail)
}

// containerIPv4 returns the container's first non-loopback IPv4 address as seen inside its own netns, or "" if it has none.
func containerIPv4(t *testing.T, ctx context.Context, ctrID string) string {
	t.Helper()
	for _, f := range strings.Fields(harness.ExecOutput(t, ctx, ctrID, "ip", "-4", "addr")) {
		if !strings.Contains(f, "/") {
			continue
		}
		bare := strings.SplitN(f, "/", 2)[0]
		if ip := net.ParseIP(bare); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return bare
		}
	}
	return ""
}

// containerHasIP reports whether `ip -4 addr` inside the container still shows the given address.
func containerHasIP(t *testing.T, ctx context.Context, ctrID, ip string) bool {
	t.Helper()
	out := harness.ExecOutput(t, ctx, ctrID, "ip", "-4", "addr")
	return strings.Contains(out, ip+"/")
}

// inRange reports whether bare IPv4 ip falls inside [start, end].
func inRange(ip, start, end string) bool {
	v4 := net.ParseIP(ip).To4()
	s := net.ParseIP(start).To4()
	e := net.ParseIP(end).To4()
	if v4 == nil || s == nil || e == nil {
		return false
	}
	return bytes.Compare(v4, s) >= 0 && bytes.Compare(v4, e) <= 0
}

// The outage is provable only after the lease lapsed, when the server no longer holds the address, and the retained
// address looks occupied to a server that probes before offering: the pre-#356 dnsmasq fixture offered .22 to a
// DISCOVER for .21. The same-address contract belongs to TestFailure_ServerReturnsBeforeExpiry (#278).

// TestFailure_ServerLossDuringRenewal checks that through an outage longer than the lease the container keeps its address and the client re-binds when the server returns.
func TestFailure_ServerLossDuringRenewal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-floss"

	// The outage must outlive the lease; outageRiseBudget bounds one lease lapsing, so a shorter lease satisfies it (#278).
	ef := harness.NewEphemeralFixture(t, harness.WithLeaseSeconds(harness.EphemeralOutageLeaseSeconds))
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
	defer cli.Close()

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-floss-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseFail := outageLines(t, ctx, ep)
	if baseFail > 0 {
		// One timeout during initial acquisition on a loaded runner is harmless; the assertion is on the delta.
		t.Logf("endpoint %s carried %d leasefail line(s) from start-up; asserting on the delta", ep, baseFail)
	}

	killed := time.Now()
	ef.Stop()
	t.Logf("server killed; a BOUND lease (fixture lease %ds) has to lapse before the plugin can report a timeout", ef.LeaseSeconds())

	h, ok := faultW.Await(outageRiseBudget, func(h, _ *harness.HealthResponse) bool {
		return h.DHCPTimeouts > base.DHCPTimeouts
	})
	if !ok {
		t.Fatalf("dhcp_timeouts never rose above %d within %s of the server dying (last: %+v)", base.DHCPTimeouts, outageRiseBudget, h)
	}
	nowFail := outageLines(t, ctx, ep)
	t.Logf("dhcp_timeouts %d -> %d at t+%.0fs after the kill; endpoint %s logged +%d leasefail line(s)",
		base.DHCPTimeouts, h.DHCPTimeouts, time.Since(killed).Seconds(), ep, nowFail-baseFail)
	if nowFail-baseFail == 0 {
		t.Errorf("dhcp_timeouts rose but the plugin logged no outage line for endpoint %s: the counter is plugin-wide, so this rise belongs to some other client and says nothing about the endpoint under test (#278)", ep)
	}
	assertNoNewHealthFaults(t, faultW, "a dead DHCP server is a degraded mode, not a plugin failure")
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s during the outage; a lapsed lease is deliberately a no-op and should retain the address", ip)
	}

	acksBefore := ef.CountLogLines("DHCPACK", mac)
	restarted := time.Now()
	ef.StartAgain()
	t.Log("server restarted with preserved lease DB; awaiting re-bind...")

	deadline := time.Now().Add(90 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if ef.CountLogLines("DHCPACK", mac) > acksBefore {
			recovered = true
			break
		}
		// The 90s deadline is unchanged; the poll only shrinks the overshoot past the ACK (#254).
		time.Sleep(250 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("no DHCPACK for the container's MAC within 90s of the server returning")
	}
	t.Logf("re-bound at t+%.0fs after the server came back", time.Since(restarted).Seconds())

	// The address may change after the lease lapsed, but the container and the server must agree on it (#278).
	live := containerIPv4(t, ctx, id)
	if live == "" {
		t.Fatal("container has no IPv4 address after the server returned; the client did not recover")
	}
	if live != ip {
		t.Logf("address changed across the outage: %s -> %s (expected when the outage outlives the lease)", ip, live)
	}
	if acked := ef.LastACKAddress(mac); acked != "" && acked != live {
		t.Errorf("container holds %s but the server's last DHCPACK for %s was %s; the two have diverged", live, mac, acked)
	}

	assertNoNewHealthFaults(t, faultW, "the server returned and the client re-bound")
}

// TestFailure_ServerReturnsBeforeExpiry checks that a short outage inside the lease re-binds the same address with lease_changed and dhcp_timeouts flat.
func TestFailure_ServerReturnsBeforeExpiry(t *testing.T) {
	// T1 (15s) < outage (25s) < lease (60s): the outage must cross T1 so a renewal fails, and end inside the lease so the
	// server still holds the entry (#356). T1/T2 are explicit so the lease can stay long.
	const (
		leaseSeconds = 60
		renewT1      = 15
		renewT2      = 45 // rebind, past the outage window
		outage       = 25 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const netName = "dh-itest-fshort"

	ef := harness.NewEphemeralFixture(t,
		harness.WithLeaseSeconds(leaseSeconds),
		harness.WithRenewTimes(renewT1, renewT2),
	)
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
	defer cli.Close()

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-fshort-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseFail := outageLines(t, ctx, ep)

	acksBefore := ef.CountLogLines("DHCPACK", mac)
	killed := time.Now()
	ef.Stop()
	t.Logf("server stopped inside the lease; restarting in %s (T1=%ds, lease=%ds, so the client fails one renewal and the lease stays live throughout)",
		outage, renewT1, leaseSeconds)

	select {
	case <-time.After(outage):
	case <-ctx.Done():
		t.Fatal("context expired during the short outage")
	}
	ef.StartAgain()
	t.Logf("server back at t+%.0fs, still inside the lease; awaiting the renewal ACK...", time.Since(killed).Seconds())

	deadline := time.Now().Add(90 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if ef.CountLogLines("DHCPACK", mac) > acksBefore {
			recovered = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("no DHCPACK for the container's MAC within 90s of the server returning inside the lease")
	}
	t.Logf("re-ACKed at t+%.0fs after the kill", time.Since(killed).Seconds())

	// The server's own ACK and the container must both carry the address; the container alone passes on retention.
	if acked := ef.LastACKAddress(mac); acked != ip {
		t.Errorf("server's last DHCPACK for %s was %s, want %s; the lease was still live and must have been returned", mac, acked, ip)
	}
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container's address changed across an outage the lease outlived; the server still held %s and must have returned it", ip)
	}

	_, after := faultW.End()
	if after.LeaseChanged != base.LeaseChanged {
		t.Errorf("lease_changed moved %d -> %d across an outage shorter than the lease; want flat", base.LeaseChanged, after.LeaseChanged)
	}
	nowFail := outageLines(t, ctx, ep)
	if d := nowFail - baseFail; d != 0 {
		t.Errorf("endpoint %s logged %d outage line(s) for an outage that never reached lease+grace; the watchdog fired early", ep, d)
	}
	assertNoNewHealthFaults(t, faultW, "an outage the plugin should have ridden out silently")
}

// The pre-#356 dnsmasq fixture refused foreign renewals silently in several shapes, so no wire message is asserted.
// libnetwork has no in-place endpoint-IP swap, so docker inspect keeping the original address is the defined degraded
// mode and lease_changed the operator's signal (#104); TestHandleEvent_Counters pins naks_received.

// TestFailure_LeaseRefusedOnRenewal checks that after a renumbering the client re-acquires from the new subnet, lease_changed records it and the plugin stays healthy.
func TestFailure_LeaseRefusedOnRenewal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-fref"

	// A short lease lets re-acquisition happen sooner; the held address is foreign from the first renewal (#356).
	ef := harness.NewEphemeralFixture(t, harness.WithLeaseSeconds(harness.EphemeralOutageLeaseSeconds))
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
	defer cli.Close()

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, inspectIP, mac := harness.RunContainer(t, ctx, netName, "dh-itest-fref-ctr")
	t.Logf("bound: inspect ip=%s mac=%s", inspectIP, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseChanged := harness.CountPluginLogLines(t, ctx, ep, logIPChanged)

	// The unicast renewal dies with the old server address, the broadcast rebind carries a foreign address, and
	// re-acquisition follows between T2 (~17.5s) and expiry (20s) plus re-DISCOVER (#356).
	renumbered := time.Now()
	ef.RestartOnSubnet(harness.EphemeralAltServerAddr, harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd)
	t.Logf("server renumbered; awaiting re-acquisition (lease %ds, so T2 ~%.1fs, expiry %ds)...",
		ef.LeaseSeconds(), float64(ef.LeaseSeconds())*0.875, ef.LeaseSeconds())

	var liveIP string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr")
		for _, f := range strings.Fields(out) {
			if !strings.Contains(f, "/") {
				continue
			}
			bare := strings.SplitN(f, "/", 2)[0]
			if inRange(bare, harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd) {
				liveIP = bare
			}
		}
		if liveIP != "" {
			break
		}
		// Each iteration is a docker exec, so the floor is 500ms (#254).
		time.Sleep(500 * time.Millisecond)
	}
	if liveIP == "" {
		t.Fatalf("container never re-acquired from the new subnet's pool %s-%s; ip -4 addr:\n%s",
			harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd,
			harness.ExecOutput(t, ctx, id, "ip", "-4", "addr"))
	}
	t.Logf("re-acquired: live ip=%s at t+%.0fs after the renumbering", liveIP, time.Since(renumbered).Seconds())

	h, ok := faultW.Await(30*time.Second, func(h, _ *harness.HealthResponse) bool {
		return h.LeaseChanged > base.LeaseChanged
	})
	if !ok {
		t.Errorf("lease_changed never recorded the re-acquisition (last: %+v)", h)
	}
	// The plugin's log line for this endpoint ties the plugin-wide counter to it (#278).
	if nowChanged := harness.CountPluginLogLines(t, ctx, ep, logIPChanged); nowChanged == baseChanged {
		t.Errorf("endpoint %s re-addressed to %s but the plugin logged no lease-change line for it (%d before, %d after)", ep, liveIP, baseChanged, nowChanged)
	}
	assertNoNewHealthFaults(t, faultW, "a lease re-acquisition is a defined, healthy flow")
	if h != nil && h.NAKsReceived > base.NAKsReceived {
		t.Logf("server NAKed on the wire (naks_received %d -> %d)", base.NAKsReceived, h.NAKsReceived)
	}

	// Inspect keeping the original address is the defined divergence (#104); a failure here means a re-Join landed, and
	// the reference manual's troubleshooting row changes with it.
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var nowInspect string
	for _, epView := range ins.NetworkSettings.Networks {
		nowInspect = epView.IPAddress
	}
	if nowInspect != inspectIP {
		t.Errorf("docker inspect reports %s; expected the stale original %s (documented degraded mode, #104)", nowInspect, inspectIP)
	}
}

// TestFailure_LeaseExpiry checks that after a permanent server loss the container keeps its address, stays reachable, dhcp_timeouts keeps climbing and the plugin stays healthy.
func TestFailure_LeaseExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-fexp"

	// The lease must lapse and two exhausted DISCOVER transactions must fit the budgets; see outageRecurBudget (#278).
	ef := harness.NewEphemeralFixture(t, harness.WithLeaseSeconds(harness.EphemeralOutageLeaseSeconds))
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
	defer cli.Close()

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-fexp-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseFail := outageLines(t, ctx, ep)
	if baseFail > 0 {
		t.Logf("endpoint %s carried %d leasefail line(s) from start-up; asserting on the delta", ep, baseFail)
	}

	killed := time.Now()
	ef.Stop()
	t.Logf("server killed permanently; a BOUND lease (fixture lease %ds) now has to cross T2 and full expiry", ef.LeaseSeconds())

	first, ok := faultW.Await(outageRiseBudget, func(h, _ *harness.HealthResponse) bool {
		return h.DHCPTimeouts > base.DHCPTimeouts
	})
	if !ok {
		t.Fatalf("dhcp_timeouts never rose above %d within %s of the kill (last: %+v)", base.DHCPTimeouts, outageRiseBudget, first)
	}
	firstFail := outageLines(t, ctx, ep)
	t.Logf("first dhcp_timeouts rise %d -> %d at t+%.0fs after the kill; endpoint %s logged +%d leasefail line(s)",
		base.DHCPTimeouts, first.DHCPTimeouts, time.Since(killed).Seconds(), ep, firstFail-baseFail)
	if firstFail-baseFail == 0 {
		t.Errorf("dhcp_timeouts rose but the plugin logged no outage line for endpoint %s: the counter is plugin-wide, so this rise belongs to some other client (#278)", ep)
	}

	second, ok := faultW.Await(outageRecurBudget, func(h, _ *harness.HealthResponse) bool {
		return h.DHCPTimeouts > first.DHCPTimeouts
	})
	if !ok {
		t.Errorf("dhcp_timeouts stalled at %d; the re-DISCOVER loop should keep recording failures (last: %+v)", first.DHCPTimeouts, second)
	}
	secondFail := outageLines(t, ctx, ep)
	t.Logf("second dhcp_timeouts rise at t+%.0fs after the kill; endpoint %s now +%d leasefail line(s) since baseline",
		time.Since(killed).Seconds(), ep, secondFail-baseFail)
	if secondFail-firstFail == 0 {
		t.Errorf("endpoint %s logged no further outage line while the server stayed down; the recurring signal is not recurring for this client (#278)", ep)
	}
	assertNoNewHealthFaults(t, faultW, "a permanent server loss is a defined degraded mode")

	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s after lease expiry; retention (deconfig no-op) is the defined behaviour", ip)
	}

	// The server address lives in the fixture's namespace, so the ping goes through the fixture.
	if out, err := ef.PingFromServer(ip); err != nil {
		t.Errorf("container %s not L2-reachable on its expired-lease address: %v\n%s", ip, err, out)
	}
}
