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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

// TestDHCPv6_ASquatOnARunningContainerIsCountedAndTheAddressChanges is
// the RUNNING-container half of the DHCPv6 conflict, which
// TestDHCPv6_ADuplicateOnTheSegmentIsRefused does not cover: that one
// restarts the container, so the conflict is found by a CreateEndpoint
// one-shot before any address is in use. Here the container never
// stops.
//
// WHAT THE PATH ACTUALLY IS, and it is not the v4 one. RFC 5227's
// section 2.4 listener keeps watching an IPv4 address for the life of
// the lease, so a v4 squat is noticed while the container runs with no
// other event needed. DHCPv6 has no such listener: RFC 4862 section
// 5.4's duplicate-address detection runs when an address is taken into
// use, and proto.Machine6 acts on a duplicate only through EvDADResult
// or EvAddressLost. Nothing in this plugin reports either for an
// address already bound, so a squat that appears under a bound v6
// lease is invisible until the client next takes the address into use.
// The plugin recycle below is that moment: the resumed client runs
// section 5.4 again on the remembered address (proto.Machine6's
// continueFromResume calls startDAD on both the confirmed and the
// unconfirmed arm), finds the squatter, declines under RFC 9915
// section 18.2.8 and acquires a replacement -- while the container is
// up and using the address throughout.
//
// WHAT IS ASSERTED, IN ORDER OF WHAT IT PROVES. The server's own log
// carries the DHCPDECLINE, which is the outside evidence that the
// event happened at all; the container's interface carries a different
// address, which is what the endpoint got out of it; the plugin's log
// carries the DHCPv6 conflict line and address_conflicts_v6 has moved,
// which is what an operator would see. The ARP-shaped counters must
// NOT move: nothing here sends an ARP frame, and address_conflicts_v4
// is the half acd_conflicts_detected is compared against.
//
// WHAT DOCKER SEES IS PINNED AS A KNOWN-WRONG ANSWER, deliberately.
// libnetwork has no in-place endpoint-address swap, so the container's
// NetworkSettings still name the address the squatter holds. That is
// the same truthfulness gap lease_changed reports for IPv4 (#104) and
// it is a v2.1 IPAM matter; it is pinned here so that closing it
// arrives as a failing test rather than as nobody noticing.
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

	// Registered before the first disable, so a t.Fatal between the
	// disable and the enable cannot leave the runner without a plugin.
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

	// The squatter, on the segment bridge and not on the parent: a
	// macvlan child does not see its own parent's traffic, so an
	// address there would be invisible to the node under test. `nodad`
	// is load-bearing for the reason the sibling test records -- the
	// address IS a duplicate by construction, so this side's own
	// duplicate-address detection would mark it dadfailed and a
	// dadfailed address answers nothing (RFC 4862 section 5.4.3).
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

	// Declared where the squatter is planted, for the reason the v4
	// conflict tests declare it: the health floor reads the log across
	// the whole run and the counters only since the last plugin start,
	// and this test restarts the plugin twice. It excuses the counter
	// under-reporting a conflict this shard caused on purpose; it
	// excuses nothing about the assertions below, which are the
	// conflict.
	harness.AllowStagedConflicts(1)

	declinesBefore := countLogToken(t, fixture.DnsmasqLog(), "DHCPDECLINE")

	w := harness.BeginCounterWindow(t, ctx, cli,
		"address_conflicts", "address_conflicts_v4", "address_conflicts_v6").ExpectRecycle()

	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("plugin recycle: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	// The interface is the endpoint's own evidence and it is read
	// first: a counter that moved over a container still sitting on the
	// squatted address would be the plugin reporting an action it did
	// not take.
	replacement := awaitOtherGlobalV6(t, ctx, id, held, 2*harness.IPAcquisitionBudget)
	if replacement == "" {
		t.Fatalf("the container's link never carried a global IPv6 other than %s after the "+
			"recycle. The resumed client either did not re-run RFC 4862 section 5.4 on the "+
			"remembered address or did not act on the duplicate, and the container is using "+
			"an address another node on the link holds", held)
	}
	t.Logf("the running container moved from %s to %s", held, replacement)

	_, after := w.End()

	// The server's log. The counter is the plugin's belief; this is
	// what happened on the wire, and it is what tells the server the
	// binding is bad (RFC 9915 section 18.2.10).
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
	// The other direction. Duplicate address detection is ICMPv6
	// neighbour discovery, not ARP: a v6 conflict that landed on the v4
	// half would make acd_conflicts_detected read lower than
	// address_conflicts_v4 and report a seam defect that has not
	// happened (see TestConflictCheck_SquattedOfferIsDeclined).
	if after.AddressConflictsV4 != 0 {
		t.Errorf("address_conflicts_v4=%d over a run whose only conflict was found by "+
			"duplicate address detection; no ARP frame was sent for it", after.AddressConflictsV4)
	}
	if after.AddressConflicts != after.AddressConflictsV4+after.AddressConflictsV6 {
		t.Errorf("address_conflicts=%d is not the sum of its halves (%d + %d)",
			after.AddressConflicts, after.AddressConflictsV4, after.AddressConflictsV6)
	}

	// The operator's line. The counter resets with the plugin process
	// and the log does not, which is why the health floor counts these
	// lines across the whole run; a conflict that moved a counter and
	// wrote nothing would be invisible to it.
	logText := harness.ReadPluginLog(t, ctx)
	// The citation rather than a whole sentence, because WHICH of the
	// two DHCPv6 lines is written is not this test's business and is
	// not fixed by the construction: the recycled client runs detection
	// on an address it has not confirmed yet, so the library reports it
	// as offered (held=false) even though the container had been using
	// it all run. Both v6 lines carry this citation and neither v4 line
	// does, so it identifies the family and the protocol that found the
	// conflict without pinning the arm.
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

	// PINNED, KNOWN WRONG, AND NAMED AS SUCH. Docker still reports the
	// address the squatter holds: libnetwork has no in-place endpoint
	// address swap, so nothing the plugin can do here updates it
	// (#104). This is the v2.1 IPAM question, not a defect this test
	// wants fixed silently -- if it ever starts passing, the assertion
	// below is the notification.
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	ep, ok := ins.NetworkSettings.Networks[netName]
	if !ok {
		t.Fatalf("container is not attached to %s any more", netName)
	}
	if ep.GlobalIPv6Address != held {
		t.Logf("KNOWN-WRONG PIN MOVED: docker now reports %q for this endpoint, not the "+
			"pre-conflict %s. If libnetwork gained an endpoint-address swap, or the plugin "+
			"started using one, this test is the place that records it (#104, v2.1 IPAM)",
			ep.GlobalIPv6Address, held)
	}

	// The conflict this test staged is real and the counter is
	// healthy-affecting, so the run-level floor would fail this shard
	// for it. The floor covers the stretch since the last plugin start
	// by design; the recycle here is what ends the stretch, and
	// AllowStagedConflicts above is what lets the log-versus-counter
	// census tolerate the difference. Everything this test asserts was
	// asserted before this line.
	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("closing plugin recycle: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)
}

// awaitOtherGlobalV6 waits for the container's link to carry a global
// IPv6 that is not `not`, and returns it.
//
// It is not linkGlobalV6 with a comparison bolted on: that helper
// returns the FIRST global address it finds, and during a lease change
// the link carries both for as long as the stale one takes to be
// removed. A test built on "the first address changed" would then be
// waiting on the deletion rather than on the acquisition, and would
// fail on a plugin that acquired the replacement correctly and lost
// the cleanup.
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
