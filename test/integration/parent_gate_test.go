// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// A parent registers one rx_handler, so it is a macvlan or an ipvlan port and never both, and the second kind gets
// EBUSY (#486, #549). The holder is the validate_dhcp preflight probe on a parent with no DHCP server, which holds the
// gate for its full 8s budget; the reclaim that used to hold it was removed in v1.9.0 (#800). The contender is a raw
// CreateEndpoint on the plugin socket: through Docker, a rival issued as the window opened still reached the gate
// 7.192s later, after the holder had finished (#800).

// TestParentGate_EndpointQueuesBehindAProbe checks that an endpoint on a parent held by a preflight probe queues at the per-parent gate (#486).
func TestParentGate_EndpointQueuesBehindAProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Both names stay under the 15-character kernel limit.
	const (
		parentName = "dh-itest-pgate"
		netName    = "dh-itest-pgnet"
		probeNet   = "dh-itest-pgprobe"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// A dummy carries no L2 traffic, so the probe's DHCPDISCOVER vanishes and the probe runs its full budget; it is
	// dedicated because the contender is made to queue on it (#800).
	la := netlink.NewLinkAttrs()
	la.Name = parentName
	dummy := &netlink.Dummy{LinkAttrs: la}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("LinkAdd dummy parent: %v", err)
	}
	t.Cleanup(func() {
		if err := netlink.LinkDel(dummy); err != nil {
			t.Logf("WARN: LinkDel dummy parent: %v", err)
		}
	})
	if err := netlink.LinkSetUp(dummy); err != nil {
		t.Fatalf("LinkSetUp dummy parent: %v", err)
	}

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Created without validate_dhcp, so creating it neither probes nor waits.
	res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM:   &network.IPAM{Driver: "null"},
		Options: map[string]string{
			"mode":   "macvlan",
			"parent": parentName,
		},
	})
	if err != nil {
		t.Fatalf("NetworkCreate(%s): %v", netName, err)
	}
	netID := res.ID
	t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), netID) })

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"parent_link_waits", "parent_link_wait_timeouts")
	before := w.Before()

	// The probe takes the gate at the top of runDHCPProbe and holds it until its child link is gone, then fails as expected.
	probeDone := make(chan struct{})
	probeStart := time.Now()
	go func() {
		defer close(probeDone)
		r, err := cli.NetworkCreate(context.Background(), probeNet, network.CreateOptions{
			Driver: harness.DriverName,
			IPAM:   &network.IPAM{Driver: "null"},
			Options: map[string]string{
				"mode":          "macvlan",
				"parent":        parentName,
				"validate_dhcp": "true",
			},
		})
		if err == nil {
			_ = cli.NetworkRemove(context.Background(), r.ID)
		}
	}()

	// The probe's child link is created after the gate is taken, so seeing it means the gate is held.
	if !awaitProbeLink(t, 30*time.Second) {
		<-probeDone
		t.Fatalf("no probe link appeared on %s within 30s; the collision window never "+
			"opened, so nothing below would be measuring the gate", parentName)
	}
	t.Logf("probe link present after %v; the parent is held", time.Since(probeStart))

	epID := harness.NewEndpointID(t)
	contendStart := time.Now()
	_, epErr := drv.CreateEndpoint(ctx, netID, epID)
	contended := time.Since(contendStart)
	t.Cleanup(func() { drv.CleanupEndpoint(netID, epID) })
	// The endpoint fails on a dummy parent with no DHCP server; the gate runs long before that.
	t.Logf("contending CreateEndpoint returned after %v (err=%v)", contended, epErr)

	<-probeDone

	after, _ := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.ParentLinkWaits+now.ParentLinkWaitTimeouts >
			before.ParentLinkWaits+before.ParentLinkWaitTimeouts
	})
	w.End()
	if after == nil {
		t.Fatalf("no health snapshot after the contention; nothing below can be judged")
	}

	waits := after.ParentLinkWaits - before.ParentLinkWaits
	timeouts := after.ParentLinkWaitTimeouts - before.ParentLinkWaitTimeouts
	t.Logf("parent_link_waits +%d, parent_link_wait_timeouts +%d", waits, timeouts)

	// An isolated probe holds for 8s and parentGateBudget is 4s, so the contender times out (+1 on
	// parent_link_wait_timeouts, probe link visible 51ms after the window opened, #800). A probe can hold usefully long
	// only with no server, so the wait-without-timeout path is covered by TestParentGate_SerialisesOneParent and
	// TestParentGate_BudgetExpiryCountsAndProceeds in pkg/plugin; the sum keeps this test off the two budget constants.
	if waits+timeouts < 1 {
		t.Errorf("neither parent_link_waits nor parent_link_wait_timeouts advanced while " +
			"an endpoint was created on a parent a probe was holding. The endpoint went " +
			"straight to the kernel: nothing serialised it, and a cross-mode caller in " +
			"its place would have been refused with EBUSY on an operation the user did " +
			"not cause (#486/#549)")
	}

	// No child of ours may outlive the test, so the dummy can be deleted.
	if !awaitProbeLinksGone(t, 30*time.Second) {
		t.Errorf("probe link(s) still on %s after the probe returned; the deferred "+
			"LinkDel did not run, and a child left on a parent blocks the other mode",
			parentName)
	}
}

// awaitProbeLink reports whether the preflight probe's child link, the sign that the gate is held, appeared within budget.
func awaitProbeLink(t *testing.T, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if len(probeLinks(t)) > 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func awaitProbeLinksGone(t *testing.T, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if len(probeLinks(t)) == 0 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func probeLinks(t *testing.T) []string {
	t.Helper()
	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	var found []string
	for _, l := range links {
		if name := l.Attrs().Name; len(name) >= 9 && name[:9] == "dh-probe-" {
			found = append(found, name)
		}
	}
	return found
}
