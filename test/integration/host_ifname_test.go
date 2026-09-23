// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Unit drives stop at what the plugin asked netlink to do, so these read the fixture's link table by the rtnetlink
// dump `ip` uses (#978). Bridge mode only: a macvlan or ipvlan child moves into the container and leaves nothing on
// the host, so network create refuses the option there.

// After a rename the generated name is only an altname, and DeleteEndpoint, EndpointOperInfo and restart recovery all
// resolve the link through it, so the lookup is by that name (#978).

// hostLinkFor returns the host namespace's link for this endpoint, found by its generated name.
func hostLinkFor(t *testing.T, endpointID string) netlink.Link {
	t.Helper()
	generated := generatedHostName(endpointID)
	link, err := netlink.LinkByName(generated)
	if err != nil {
		noLinkAnswers(t, generated, err)
	}
	return link
}

// noLinkAnswers fails the test because no link answers to the generated name.
func noLinkAnswers(t *testing.T, generated string, err error) {
	t.Helper()
	t.Fatalf("no link on this host answers to %q: %v\n"+
		"That name is what the plugin derives from the endpoint ID at teardown, at "+
		"EndpointOperInfo and at restart recovery. A renamed link that no longer answers to it "+
		"is a veth left on the bridge for the life of the host (#978).\n%s",
		generated, err, linkTable(t))
}

// generatedHostName spells out vethPairNames' host half, so the fixture cannot agree with the subject by construction.
func generatedHostName(endpointID string) string {
	return "dh-" + endpointID[:12]
}

func linkTable(t *testing.T) string {
	t.Helper()
	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		return fmt.Sprintf("(link table unreadable: %v)", err)
	}
	var b strings.Builder
	b.WriteString("the host's link table:\n")
	for _, l := range links {
		fmt.Fprintf(&b, "  %-20s type=%-8s altnames=%v\n", l.Attrs().Name, l.Type(), l.Attrs().AltNames)
	}
	return b.String()
}

func TestHostIfname_TheHostLinkTakesTheContainersName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-hifname"
	// 14 characters, so truncation is not in play.
	ctrName := "dh-itest-hifa"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
			t.Log(linkTable(t))
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli,
		"host_ifnames_applied", "host_ifname_conflicts", "host_ifname_failures")

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"host_ifname": "container_name"})
	id, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	epID := endpointIDOf(t, ctx, cli, id, netName)
	t.Logf("container %s: ip=%s endpoint=%s", ctrName, ipv4, epID)

	// The rename happens after the attach succeeds, so the container can have its address before the link has its name.
	link := waitHostLinkName(t, epID, ctrName, 30*time.Second)
	if got := link.Attrs().Name; got != ctrName {
		t.Fatalf("the host-side link is named %q, want %q. That is what ip link and brctl show print, "+
			"and naming it after the container is the whole of #978.\n%s", got, ctrName, linkTable(t))
	}
	if _, ok := link.(*netlink.Veth); !ok {
		t.Errorf("the link named %q is a %s and not a veth: the rename landed on something other than "+
			"this endpoint's host-side half", ctrName, link.Type())
	}

	generated := generatedHostName(epID)
	if !hasAltName(link, generated) {
		t.Errorf("the link named %q does not carry %q as an altname (altnames %v). Four sites derive "+
			"that name from the endpoint ID and look it up without reading one back, and "+
			"DeleteEndpoint reads a miss as a finished teardown and returns nil, so without the "+
			"altname the veth is left on the bridge",
			ctrName, generated, link.Attrs().AltNames)
	}

	master := masterOf(t, link)
	if master != harness.BridgeName {
		t.Errorf("the renamed link's master is %q, want %q: a rename that detached the link from the "+
			"bridge would take the container's connectivity with it", master, harness.BridgeName)
	}

	after, ok := w.Await(15*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.HostIfnamesApplied > before.HostIfnamesApplied
	})
	before, _ := w.End()
	if !ok {
		t.Errorf("host_ifnames_applied did not advance (before=%d, last seen=%d): the link carries the "+
			"name, so it got there without the plugin counting it, and the counter an operator reads "+
			"to see the option working says nothing", before.HostIfnamesApplied, after.HostIfnamesApplied)
	}
	if got := after.HostIfnameConflicts - before.HostIfnameConflicts; got != 0 {
		t.Errorf("host_ifname_conflicts advanced by %d on a rename that happened", got)
	}
	if got := after.HostIfnameFailures - before.HostIfnameFailures; got != 0 {
		t.Errorf("host_ifname_failures advanced by %d on a rename that happened", got)
	}

	// DeleteEndpoint looks the link up by the generated name and treats a miss as a normal forced teardown, so a broken
	// lookup would leave the link behind silently (#978).
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := netlink.LinkByName(ctrName); err != nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the link named %q is still on this host after its container was removed. "+
				"DeleteEndpoint derives %q from the endpoint ID and returns nil when it does not "+
				"resolve, so a renamed link it cannot find is leaked silently.\n%s",
				ctrName, generated, linkTable(t))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func TestHostIfname_ALongNameIsTruncatedAndATakenOneIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	netName := "dh-itest-hifname2"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
			t.Log(linkTable(t))
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"host_ifname": "container_name"})

	// 27 characters against the kernel's 15; the expected name follows docs/reference.md (first 9 characters, '-', the
	// endpoint's first 5 hex), not the plugin's helper (#978).
	longName := "dh-itest-hifname-truncated1"
	longID, _, _ := harness.RunContainer(t, ctx, netName, longName)
	longEP := endpointIDOf(t, ctx, cli, longID, netName)
	wantTruncated := longName[:9] + "-" + longEP[:5]
	if len(wantTruncated) != 15 {
		t.Fatalf("this cell's own expectation is %d characters, and the kernel takes 15: the fixture "+
			"is wrong before the plugin is asked anything", len(wantTruncated))
	}

	link := waitHostLinkName(t, longEP, wantTruncated, 30*time.Second)
	if got := link.Attrs().Name; got != wantTruncated {
		t.Errorf("a %d-character container name produced the host link %q, want %q. The rule is the "+
			"operator's: docs/reference.md states it and an operator reads ip link expecting it.\n%s",
			len(longName), got, wantTruncated, linkTable(t))
	}

	// Interface names are unique per namespace, and the host's is shared with everything else on the box.
	taken := "dh-itest-taken"
	takenLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: taken}}
	if err := netlink.LinkAdd(takenLink); err != nil {
		t.Fatalf("creating the interface that holds the name first: %v", err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(taken); err == nil {
			_ = netlink.LinkDel(l)
		}
	})

	w := harness.BeginCounterWindow(t, ctx, cli,
		"host_ifname_conflicts", "host_ifnames_applied", "host_ifname_failures")

	clashID, clashIP, _ := harness.RunContainer(t, ctx, netName, taken)
	clashEP := endpointIDOf(t, ctx, cli, clashID, netName)
	if clashIP == "" {
		t.Error("the container whose name was taken has no address: a name it could not have must not " +
			"cost it its lease")
	}

	after, ok := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.HostIfnameConflicts > before.HostIfnameConflicts
	})
	before, _ := w.End()
	if !ok {
		t.Errorf("host_ifname_conflicts did not advance (before=%d, last seen=%d) for a container whose "+
			"name a dummy interface already held. Without it an operator reading ip link sees a "+
			"generated name and has nothing that says why",
			before.HostIfnameConflicts, after.HostIfnameConflicts)
	}
	if got := after.HostIfnameFailures - before.HostIfnameFailures; got != 0 {
		t.Errorf("host_ifname_failures advanced by %d on a name that was simply taken. The two are "+
			"separate because only this one has a remedy an operator can act on", got)
	}

	clashLink := hostLinkFor(t, clashEP)
	if got := clashLink.Attrs().Name; got != generatedHostName(clashEP) {
		t.Errorf("the second container's host link is named %q, want the generated %q: the kernel "+
			"refused the rename, so the link must be exactly as it was created",
			got, generatedHostName(clashEP))
	}
	if l, err := netlink.LinkByName(taken); err != nil {
		t.Errorf("the interface that held the name first is gone: %v", err)
	} else if l.Type() != "dummy" {
		t.Errorf("the interface holding %q is now a %s: the plugin took a name that was not free",
			taken, l.Type())
	}
}

// register_dns needs the name before the client is constructed, so that attach never enters the late-naming path
// (#961); a rename hung on that path would leave this network's links generated.

// TestHostIfname_IsRederivedOnRestart checks that a restarted container's host link takes its name again (#978).
func TestHostIfname_IsRederivedOnRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	netName := "dh-itest-hifname3"
	ctrName := "dh-itest-hifb"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
			t.Log(linkTable(t))
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		"host_ifname":  "hostname",
		"register_dns": "true",
	})
	// RunContainer sets --hostname to the container's name, so the two modes differ only in the inspect field read.
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)
	epBefore := endpointIDOf(t, ctx, cli, id, netName)
	waitHostLinkName(t, epBefore, ctrName, 30*time.Second)

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}

	epAfter := waitEndpointID(t, ctx, cli, id, netName, harness.IPAcquisitionBudget)
	link := waitHostLinkName(t, epAfter, ctrName, 30*time.Second)
	if got := link.Attrs().Name; got != ctrName {
		t.Errorf("after a restart the host-side link is named %q, want %q. The name is derived from the "+
			"daemon's answer on every attach and nothing about it is persisted, so a restart that "+
			"comes back generated means the rebuilt endpoint never reached the rename.\n%s",
			got, ctrName, linkTable(t))
	}
	if !hasAltName(link, generatedHostName(epAfter)) {
		t.Errorf("the restarted endpoint's link does not carry %q as an altname (altnames %v)",
			generatedHostName(epAfter), link.Attrs().AltNames)
	}

	// A rebuild that left the previous link behind would make the kernel refuse the second rename, leaving the dead one named.
	if n := countLinksNamed(t, ctrName); n != 1 {
		t.Errorf("%d links on this host are named %q, want 1: a restart that leaves the previous "+
			"endpoint's link behind leaves the new one generated\n%s", n, ctrName, linkTable(t))
	}
}

// Recovery rebuilds every endpoint through the same Start with no sandbox key, so a recycle renames a second time over
// a link carrying both names. Measured on kernel 6.12.107: renaming to the current name succeeds, adding an existing
// altname is EEXIST, and renaming back is EEXIST too, since altnames share the name hash (#978). Counters are read
// absolutely because PluginEnable starts a fresh process.
// Do not parallelize: disabling the plugin takes every plugin-managed container with it.

// TestHostIfname_APluginRecycleLeavesTheNamedLinkAlone checks that a recycle after a container rename moves no failure counter and keeps both names (#978).
func TestHostIfname_APluginRecycleLeavesTheNamedLinkAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	netName := "dh-itest-hifname4"
	ctrName := "dh-itest-hifc"
	renamedTo := "dh-itest-hifd"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
			t.Log(linkTable(t))
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// Registered before the disable, so a failure below still leaves the plugin enabled for the shard.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"host_ifname": "container_name"})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)
	ep := endpointIDOf(t, ctx, cli, id, netName)
	before := waitHostLinkName(t, ep, ctrName, 30*time.Second)
	if !hasAltName(before, generatedHostName(ep)) {
		t.Fatalf("the link is named %q but does not carry %q as an altname before the recycle "+
			"(altnames %v); the recycle would not be measuring what this cell is about",
			before.Attrs().Name, generatedHostName(ep), before.Attrs().AltNames)
	}

	// The rename makes the second pass one where the wanted name moved while the old one is still an altname, so the
	// rename path runs and meets EEXIST (#978); Docker's cleanup is keyed on the container ID.
	if err := cli.ContainerRename(ctx, id, renamedTo); err != nil {
		t.Fatalf("ContainerRename(%s -> %s): %v", ctrName, renamedTo, err)
	}

	w := harness.BeginCounterWindow(t, ctx, cli,
		"host_ifnames_applied", "host_ifname_conflicts", "host_ifname_failures",
		"recovered_ok", "recovery_failed").ExpectRecycle()

	// The recovered endpoint's ARP probe races teardown; one container, one v4 lease.
	harness.AllowUnprobedLeases(1)

	// Dumped only on failure; the rename runs inside the rebuild the counters describe.
	logMark := harness.MarkPluginLog(t, ctx)
	harness.DumpPluginLogOnFailure(t, ctx, logMark, "the plugin was disabled")

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	// The walk spawns each rebuild and the rename runs inside dhcpManager.Start, after which recovered_ok moves.
	const rebuilt = "recovery to rebuild this endpoint's renewal client (recovered_ok >= 1)"
	waited, ok := harness.AwaitRecoveryRebuildWindow(w, rebuilt,
		func(h *harness.HealthResponse) bool { return h.RecoveredOK >= 1 })

	_, after := w.End()
	t.Logf("after the recycle: recovered_ok=%d host_ifnames_applied=%d host_ifname_failures=%d host_ifname_conflicts=%d",
		after.RecoveredOK, after.HostIfnamesApplied, after.HostIfnameFailures, after.HostIfnameConflicts)

	if !ok {
		t.Fatalf("%s\n  Recovery is the path that runs the rename a second time, so a recycle that "+
			"recovered nothing has not measured it.", harness.RecoveryRebuildFailure(rebuilt, waited))
	}
	// A failed rebuild leaves the rename unmeasured.
	if after.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d after the recycle: the endpoint whose link this test renames was "+
			"not rebuilt, so the rename counters below describe some other endpoint. %s",
			after.RecoveryFailed, harness.RecoveryRoutes(after))
	}
	// The container runs across the recycle, so this counter would mean the plugin could not find a container that was there.
	if after.RecoveryAbortedContainerGone != 0 {
		t.Errorf("recovery_aborted_container_gone=%d after the recycle, although this test's container "+
			"was running throughout: recovery gave up on the endpoint whose link this test renames, "+
			"so the rename counters below describe some other endpoint. %s",
			after.RecoveryAbortedContainerGone, harness.RecoveryRoutes(after))
	}
	if after.HostIfnameFailures != 0 {
		t.Errorf("host_ifname_failures=%d after a recycle over a link that is named, on its bridge and "+
			"found by teardown. That counter is a warn check, so this puts the health document in "+
			"warn and tells the operator to remove a healthy link by hand, on every container on "+
			"this network every time the plugin restarts\n%s", after.HostIfnameFailures, linkTable(t))
	}
	if after.HostIfnameConflicts != 0 {
		t.Errorf("host_ifname_conflicts=%d after the recycle: the only name it can have collided with "+
			"is its own", after.HostIfnameConflicts)
	}
	if after.HostIfnamesApplied < 1 {
		t.Errorf("host_ifnames_applied=%d after the recycle, want at least 1: the recovered endpoint "+
			"still carries the name its network asked for, and a zero here makes the two zeros "+
			"above say nothing", after.HostIfnamesApplied)
	}

	link := waitHostLinkName(t, ep, renamedTo, 30*time.Second)
	if got := link.Attrs().Name; got != renamedTo {
		t.Errorf("after the recycle the host-side link is named %q, want %q. The name is re-derived on "+
			"every attach from the daemon's current answer, so a container renamed between two "+
			"passes comes back under the new name\n%s", got, renamedTo, linkTable(t))
	}
	if !hasAltName(link, generatedHostName(ep)) {
		t.Errorf("after the recycle the link no longer carries %q as an altname (altnames %v): every "+
			"lookup this plugin makes of the generated name goes through it",
			generatedHostName(ep), link.Attrs().AltNames)
	}
	if got := masterOf(t, link); got != harness.BridgeName {
		t.Errorf("after the recycle the link's master is %q, want %q", got, harness.BridgeName)
	}
	if n := countLinksNamed(t, renamedTo); n != 1 {
		t.Errorf("%d links on this host are named %q after the recycle, want 1\n%s",
			n, renamedTo, linkTable(t))
	}
	if n := countLinksNamed(t, ctrName); n != 0 {
		t.Errorf("%d links on this host still answer to %q, the name the container had before it was "+
			"renamed\n%s", n, ctrName, linkTable(t))
	}
}

// The rename is two kernel calls and the generated name resolves to nothing between them, since the kernel refuses an
// altname equal to the current name; the plugin keeps its own readers out of that window (#1051), an outside reader
// waits it out, so the first claim is due at the deadline.

// waitHostLinkName waits for the endpoint's host link to answer to its generated name and to be named want.
func waitHostLinkName(t *testing.T, endpointID, want string, budget time.Duration) netlink.Link {
	t.Helper()
	generated := generatedHostName(endpointID)
	link, _, err := harness.AwaitSettled(budget, 250*time.Millisecond,
		func() (netlink.Link, error) { return netlink.LinkByName(generated) },
		func(l netlink.Link) bool { return l.Attrs().Name == want })
	if err != nil {
		noLinkAnswers(t, generated, err)
	}
	return link
}

func waitEndpointID(t *testing.T, ctx context.Context, cli *docker.Client, ctrID, netName string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		ins, err := cli.ContainerInspect(ctx, ctrID)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ep, ok := ins.NetworkSettings.Networks[netName]; ok && ep.EndpointID != "" {
			return ep.EndpointID
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("docker reports no endpoint id for %s on %s within %v", ctrID, netName, budget)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func hasAltName(link netlink.Link, name string) bool {
	for _, a := range link.Attrs().AltNames {
		if a == name {
			return true
		}
	}
	return false
}

func countLinksNamed(t *testing.T, name string) int {
	t.Helper()
	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	n := 0
	for _, l := range links {
		if l.Attrs().Name == name {
			n++
		}
	}
	return n
}

func masterOf(t *testing.T, link netlink.Link) string {
	t.Helper()
	idx := link.Attrs().MasterIndex
	if idx == 0 {
		return ""
	}
	master, err := netlink.LinkByIndex(idx)
	if err != nil {
		t.Fatalf("LinkByIndex(%d) for the renamed link's master: %v", idx, err)
	}
	return master.Attrs().Name
}
