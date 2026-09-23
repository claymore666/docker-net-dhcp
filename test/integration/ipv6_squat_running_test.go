// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

// DHCPv6 has no RFC 5227 section 2.4 listener: RFC 4862 section 5.4 detection runs only when an address is taken into
// use, and proto.Machine6 acts on a duplicate only through EvDADResult or EvAddressLost, so a squat under a bound v6
// lease is invisible until the client next takes the address into use. The plugin recycle is that moment:
// continueFromResume calls startDAD on both arms, and the client declines under RFC 9915 section 18.2.8 (PR #930).

// TestDHCPv6_ASquatOnARunningContainerIsCountedAndTheAddressChanges checks that a v6 squat under a running container is declined, counted as v6 and replaced (PR #930).
func TestDHCPv6_ASquatOnARunningContainerIsCountedAndTheAddressChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	netName := "dh-itest-v6squat"
	ctrName := "dh-itest-v6squat-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// Registered before the first disable, so a t.Fatal cannot leave the runner without a plugin.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	held := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if held == "" {
		t.Fatal("no global IPv6 appeared on the container link; there is no address to squat")
	}
	t.Logf("the running container holds %s", held)

	// On the segment bridge, since a macvlan child does not see its parent's traffic; `nodad`, because a duplicate
	// would go dadfailed here and a dadfailed address answers nothing (RFC 4862 section 5.4.3).
	dup := held + "/64"
	if out, err := exec.Command("ip", "-6", "addr", "add", dup, "dev", harness.DHCPSegment, "nodad").CombinedOutput(); err != nil {
		t.Fatalf("could not put a duplicate of %s on %s: %v\n%s", held, harness.DHCPSegment, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "-6", "addr", "del", dup, "dev", harness.DHCPSegment).Run()
	})
	if !awaitAddrSettled(t, harness.DHCPSegment, held, 15*time.Second) {
		t.Fatalf("the duplicate %s on %s never left the tentative state, so it would not have "+
			"answered the client's solicitation and this test would measure nothing",
			held, harness.DHCPSegment)
	}

	// The health floor reads the log across the run but counters only since the last plugin start, and this test
	// restarts the plugin twice; it excuses the counter only, not the assertions below (PR #930).
	harness.AllowStagedConflicts(1)

	declinesBefore := countLogToken(t, fixture.DnsmasqLog(), "DHCPDECLINE")

	// The plugin log survives every restart; on the arm64 lane a whole-log read matched three lines a
	// TestConflictCheck_ test had written ten minutes earlier (#933).
	logMark := harness.MarkPluginLog(t, ctx)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"address_conflicts", "address_conflicts_v4", "address_conflicts_v6").ExpectRecycle()

	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("plugin recycle: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	// The interface is read first, so a counter cannot pass over a container still on the squatted address.
	replacement := awaitOtherGlobalV6(t, ctx, id, held, 2*harness.IPAcquisitionBudget)
	if replacement == "" {
		t.Fatalf("the container's link never carried a global IPv6 other than %s after the "+
			"recycle. The resumed client either did not re-run RFC 4862 section 5.4 on the "+
			"remembered address or did not act on the duplicate, and the container is using "+
			"an address another node on the link holds", held)
	}
	t.Logf("the running container moved from %s to %s", held, replacement)

	_, after := w.End()

	// The decline tells the server the binding is bad (RFC 9915 section 18.2.10).
	if n := countLogToken(t, fixture.DnsmasqLog(), "DHCPDECLINE") - declinesBefore; n < 1 {
		t.Errorf("the server logged no DHCPDECLINE after the squat (%d new lines). The "+
			"container may have moved address for some other reason, and the server still "+
			"believes %s is validly bound to it", n, held)
	}

	if after.AddressConflictsV6 < 1 {
		t.Errorf("address_conflicts_v6=%d after a DHCPv6 conflict on a running container, "+
			"want at least 1. The address changed under the container and no counter says "+
			"why", after.AddressConflictsV6)
	}
	// Duplicate address detection is ICMPv6 neighbour discovery, not ARP, so the v4 half must stay flat (PR #930).
	if after.AddressConflictsV4 != 0 {
		t.Errorf("address_conflicts_v4=%d over a run whose only conflict was found by "+
			"duplicate address detection; no ARP frame was sent for it", after.AddressConflictsV4)
	}
	if after.AddressConflicts != after.AddressConflictsV4+after.AddressConflictsV6 {
		t.Errorf("address_conflicts=%d is not the sum of its halves (%d + %d)",
			after.AddressConflicts, after.AddressConflictsV4, after.AddressConflictsV6)
	}

	// The counter resets with the plugin process and the log does not, so the health floor counts these lines across the run.
	logText := harness.AwaitPluginLogSince(t, ctx, logMark, 5*time.Second, func(window string) bool {
		return strings.Contains(window, "(RFC 4862 section 5.4 Duplicate Address Detection)") &&
			strings.Contains(window, "family=ipv6")
	})
	// The recycled client reports the address as offered (held=false), so the citation, carried by both v6 lines and
	// neither v4 line, identifies family and protocol without pinning the arm (PR #930).
	const wantCitation = "(RFC 4862 section 5.4 Duplicate Address Detection)"
	if !strings.Contains(logText, wantCitation) {
		t.Errorf("the plugin log carries no DHCPv6 conflict line: nothing cites %q.\n"+
			"  The health floor counts conflicts by matching these lines across the whole run, "+
			"so every v6 squat in a run the plugin restarted through would be invisible.", wantCitation)
	}
	if !strings.Contains(logText, "family=ipv6") {
		t.Error("no log line carries family=ipv6; the conflict was reported without saying " +
			"which protocol found it, which is what sends an operator to the wrong tool")
	}
	if strings.Contains(logText, "(RFC 5227 section 2.4)") {
		t.Error("the plugin reported this conflict as RFC 5227 section 2.4, which is ARP. " +
			"Duplicate address detection found it; an operator sent to look for ARP traffic " +
			"will find none")
	}

	// Known wrong, pinned by equality: libnetwork has no in-place endpoint address swap, so Docker still reports the
	// squatted address (#104, #881); a fix or a changed shape fails here and the message says what to write instead.
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	ep, ok := ins.NetworkSettings.Networks[netName]
	if !ok {
		t.Fatalf("container is not attached to %s any more", netName)
	}
	if ep.GlobalIPv6Address != held {
		t.Errorf("KNOWN-WRONG PIN MOVED: docker reports %q for this endpoint, not the "+
			"pre-conflict %s that this round pins.\n"+
			"  The container's interface carries %s. If libnetwork gained an endpoint-address "+
			"swap, or the plugin started using one, the divergence #104 describes has closed "+
			"and this assertion is what should change (v2.1 IPAM). If docker reports neither "+
			"address, the endpoint record is being written by something this test does not "+
			"know about, which is a defect and not a closure.",
			ep.GlobalIPv6Address, held, replacement)
	}

	// The run-level floor would fail this shard for the staged conflict; the recycle ends the floor's stretch and
	// AllowStagedConflicts lets the log-versus-counter census tolerate it (PR #930).
	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("closing plugin recycle: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)
}

// During a lease change the link carries both addresses until the stale one is removed, so waiting on "the first
// address changed" would wait on the deletion, not the acquisition (#930).

// awaitOtherGlobalV6 waits for the container's link to carry a global IPv6 other than `not` and returns it.
func awaitOtherGlobalV6(t *testing.T, ctx context.Context, ctrID, not string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out := harness.ExecOutput(t, ctx, ctrID, "ip", "-6", "addr", "show", "scope", "global")
		for _, f := range strings.Fields(out) {
			if !strings.Contains(f, ":") || !strings.Contains(f, "/") {
				continue
			}
			bare := strings.SplitN(f, "/", 2)[0]
			ip := net.ParseIP(bare)
			if ip == nil || ip.To4() != nil || bare == not {
				continue
			}
			return bare
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}
