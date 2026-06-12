//go:build integration

// Runtime failure-injection tests (#128): what happens AFTER a
// container is bound and the world breaks. Each test runs against a
// per-test EphemeralFixture DHCP server (never the suite-static one —
// every other test depends on that staying up) and documents the
// *intended* degraded-mode behaviour it asserts, so those semantics
// are decided here rather than discovered in production.
//
// These tests cross real DHCP timing boundaries (the fixture lease
// floor is 2m, so T1=60s, T2=105s, expiry=120s) and add ~9 serial
// minutes — they are split out of the main suite into
// `make integration-test-failure` (second CI step).
//
// busybox-udhcpc timing facts the asserts below lean on (defaults:
// -t 3 -T 3 -A 20, see pkg/udhcpc/client.go — no overrides):
//   - a dead server produces NO udhcpc event at T1/T2 (silent unicast
//     /broadcast retries); the first observable event is "leasefail",
//     emitted ~10s after lease EXPIRY when re-DISCOVER times out
//     (3 tries × 3s). dhcp_timeouts therefore moves at ~t+130s, not
//     at T1.
//   - after "leasefail", udhcpc sleeps 20s (-A) and re-DISCOVERs
//     forever — so recovery after the server returns lands within
//     ~30s, and while it's gone, dhcp_timeouts keeps climbing on a
//     ~30s period.
//   - at expiry udhcpc emits "deconfig", which the plugin DELIBERATELY
//     ignores (would wipe copied routes, see dhcp_manager.go) — the
//     container keeps its address through an outage.
package integration

import (
	"bytes"
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// failureHealth polls /Plugin.Health until cond is true or the budget
// is spent, returning the last response either way.
func failureHealth(t *testing.T, ctx context.Context, cli *docker.Client, budget time.Duration, cond func(*harness.HealthResponse) bool) (*harness.HealthResponse, bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last *harness.HealthResponse
	for time.Now().Before(deadline) {
		h, err := harness.PluginHealth(ctx, cli)
		if err == nil {
			last = h
			if cond(h) {
				return h, true
			}
		}
		time.Sleep(time.Second)
	}
	return last, false
}

// containerHasIP reports whether `ip -4 addr` inside the container
// still shows the given address.
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

// TestFailure_ServerLossDuringRenewal: the "router rebooted at 3am"
// scenario. Intended behaviour asserted:
//   - while the server is gone, the container KEEPS its address (the
//     deconfig no-op), the plugin stays Healthy, and dhcp_timeouts
//     records the failure;
//   - when the server returns with its lease DB intact, the client
//     re-binds to the SAME address (lease_changed stays flat) without
//     operator intervention.
func TestFailure_ServerLossDuringRenewal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

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
	defer cli.Close()

	harness.CreateNetwork(t, ctx, "dh-itest-floss", "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, "dh-itest-floss", "dh-itest-floss-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	base, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (baseline): %v", err)
	}

	// Kill the server uncleanly. The persistent client now faces
	// silent T1/T2 retries, expiry, and a failing re-DISCOVER.
	ef.Stop()
	t.Log("server killed; waiting for the post-expiry leasefail (~130s)...")

	h, ok := failureHealth(t, ctx, cli, 170*time.Second, func(h *harness.HealthResponse) bool {
		return h.DHCPTimeouts > base.DHCPTimeouts
	})
	if !ok {
		t.Fatalf("dhcp_timeouts never rose above %d during server outage (last: %+v)", base.DHCPTimeouts, h)
	}
	if !h.Healthy {
		t.Error("plugin went unhealthy during a server outage; a dead DHCP server is a degraded mode, not a plugin failure")
	}
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s during the outage; the deconfig no-op should retain the address", ip)
	}

	// Server returns, lease DB intact: the udhcpc retry loop must
	// re-bind to the same address within ~30s (poll 90s for margin).
	acksBefore := ef.CountLogLines("DHCPACK", mac)
	ef.StartAgain()
	t.Log("server restarted with preserved lease DB; awaiting re-bind...")

	deadline := time.Now().Add(90 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if ef.CountLogLines("DHCPACK", mac) > acksBefore {
			recovered = true
			break
		}
		time.Sleep(time.Second)
	}
	if !recovered {
		t.Fatal("no DHCPACK for the container's MAC within 90s of the server returning")
	}
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container's address changed across the outage; with a preserved lease DB it must re-bind to %s", ip)
	}
	after, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (after): %v", err)
	}
	if after.LeaseChanged != base.LeaseChanged {
		t.Errorf("lease_changed moved %d -> %d across an outage with a preserved lease DB; want flat", base.LeaseChanged, after.LeaseChanged)
	}
}

// TestFailure_LeaseRefusedOnRenewal: the site gets renumbered under a
// live lease — the server comes back on a different subnet and the
// container's held address is foreign to it. Two CI iterations showed
// dnsmasq REFUSES such renewals *silently* in several shapes (out-of-
// range REQUEST: ignored; address-taken REQUEST: ignored) rather than
// emitting DHCPNAK, so this test asserts the *intended degraded-mode
// semantics* rather than a specific wire message:
//   - the client re-acquires from the new subnet's pool without
//     operator intervention; lease_changed records the move;
//   - `docker inspect` keeps reporting the ORIGINAL address: libnetwork
//     has no in-place endpoint-IP swap RPC, so the inspect divergence
//     is the DEFINED degraded mode (#104) — lease_changed is the
//     operator's signal, and this assertion is the documentation;
//   - the plugin stays Healthy throughout.
//
// The naks_received counter's contract is pinned in unit tests
// (TestHandleEvent_Counters) — when a server does NAK, that's the
// path that counts it; any NAK observed here is logged for interest.
func TestFailure_LeaseRefusedOnRenewal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

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
	defer cli.Close()

	pre, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (pre): %v", err)
	}

	harness.CreateNetwork(t, ctx, "dh-itest-fref", "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, inspectIP, mac := harness.RunContainer(t, ctx, "dh-itest-fref", "dh-itest-fref-ctr")
	t.Logf("bound: inspect ip=%s mac=%s", inspectIP, mac)

	// Settle: wait for the persistent client's own bound (it can
	// differ from CreateEndpoint's one-shot lease) so the baseline
	// isn't polluted by start-up churn.
	if _, ok := failureHealth(t, ctx, cli, 30*time.Second, func(h *harness.HealthResponse) bool {
		return h.LeasesObtained > pre.LeasesObtained
	}); !ok {
		t.Fatal("persistent client never bound")
	}
	base, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (baseline): %v", err)
	}

	// Renumber the site: new server address, new pool, wiped DB. The
	// unicast T1 renewal dies (the old server address is gone); the
	// T2 broadcast rebind carries a foreign address; re-acquisition
	// follows somewhere between T2 (105s) and expiry+rediscover
	// (~135s).
	ef.RestartOnSubnet(harness.EphemeralAltServerAddr, harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd)
	t.Log("server renumbered; awaiting re-acquisition (T2 ~105s, expiry ~135s)...")

	var liveIP string
	deadline := time.Now().Add(4 * time.Minute)
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
		time.Sleep(2 * time.Second)
	}
	if liveIP == "" {
		t.Fatalf("container never re-acquired from the new subnet's pool %s-%s; ip -4 addr:\n%s",
			harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd,
			harness.ExecOutput(t, ctx, id, "ip", "-4", "addr"))
	}
	t.Logf("re-acquired: live ip=%s", liveIP)

	h, ok := failureHealth(t, ctx, cli, 30*time.Second, func(h *harness.HealthResponse) bool {
		return h.LeaseChanged > base.LeaseChanged
	})
	if !ok {
		t.Errorf("lease_changed never recorded the re-acquisition (last: %+v)", h)
	}
	if h != nil && !h.Healthy {
		t.Error("plugin went unhealthy over a lease re-acquisition; this is a defined, healthy flow")
	}
	if h != nil && h.NAKsReceived > base.NAKsReceived {
		t.Logf("server NAKed on the wire (naks_received %d -> %d)", base.NAKsReceived, h.NAKsReceived)
	}

	// docker inspect still shows the original address: the DEFINED
	// divergence (#104). If this ever fails because inspect tracks
	// the new IP, a re-Join mechanism landed — update the reference
	// manual's troubleshooting row along with this test.
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var nowInspect string
	for _, ep := range ins.NetworkSettings.Networks {
		nowInspect = ep.IPAddress
	}
	if nowInspect != inspectIP {
		t.Errorf("docker inspect reports %s; expected the stale original %s (documented degraded mode, #104)", nowInspect, inspectIP)
	}
}

// TestFailure_LeaseExpiry: the server disappears permanently and the
// lease fully lapses. Intended behaviour asserted: address retention
// is DELIBERATE (deconfig no-op), the endpoint stays L2-reachable on
// the stale address, dhcp_timeouts keeps climbing as the retry loop
// spins, and the plugin reports Healthy — "server gone" is a defined
// degraded mode, not undefined behaviour.
func TestFailure_LeaseExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

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
	defer cli.Close()

	harness.CreateNetwork(t, ctx, "dh-itest-fexp", "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, "dh-itest-fexp", "dh-itest-fexp-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	base, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (baseline): %v", err)
	}

	ef.Stop()
	t.Log("server killed permanently; crossing T2 and full expiry (~130s)...")

	first, ok := failureHealth(t, ctx, cli, 170*time.Second, func(h *harness.HealthResponse) bool {
		return h.DHCPTimeouts > base.DHCPTimeouts
	})
	if !ok {
		t.Fatalf("dhcp_timeouts never rose above %d after lease expiry (last: %+v)", base.DHCPTimeouts, first)
	}

	// The retry loop must keep recording failures (~30s period).
	second, ok := failureHealth(t, ctx, cli, 80*time.Second, func(h *harness.HealthResponse) bool {
		return h.DHCPTimeouts > first.DHCPTimeouts
	})
	if !ok {
		t.Errorf("dhcp_timeouts stalled at %d; the re-DISCOVER loop should keep recording failures (last: %+v)", first.DHCPTimeouts, second)
	}
	if second != nil && !second.Healthy {
		t.Error("plugin went unhealthy on a permanent server loss; this is a defined degraded mode")
	}

	// Address retention past expiry is deliberate...
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s after lease expiry; retention (deconfig no-op) is the defined behaviour", ip)
	}

	// ...and the endpoint stays L2-reachable: ping the container from
	// the server side of the veth pair (the address survives on the
	// link even though dnsmasq is dead).
	ping := exec.Command("ping", "-c", "1", "-W", "2", "-I", ef.ServerIP(), ip)
	if out, err := ping.CombinedOutput(); err != nil {
		t.Errorf("container %s not L2-reachable on its expired-lease address: %v\n%s", ip, err, out)
	}
}
