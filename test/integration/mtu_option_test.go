// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// optionMTU is neither 1500 nor the fixture's option 26 (harness.TestMTU), so a value read back can only come from mtu.
const optionMTU = 1450

// ctrLinkMTU reads the MTU of the container link holding addr; a bridge-mode link is named by the bridge (DstPrefix), not eth0.
func ctrLinkMTU(t *testing.T, ctx context.Context, id, addr string) int {
	t.Helper()
	shown := harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show")
	ifname := ""
	for _, line := range strings.Split(shown, "\n") {
		f := strings.Fields(line)
		for i := 2; i < len(f) && ifname == ""; i++ {
			if a, _, ok := strings.Cut(f[i], "/"); ok && a == addr {
				ifname = strings.TrimSuffix(f[1], ":")
			}
		}
	}
	if ifname == "" {
		t.Fatalf("no link inside the container holds %s:\n%s", addr, shown)
	}
	out := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+ifname+"/mtu"))
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("%s mtu inside the container: %q", ifname, out)
	}
	return n
}

// hostLinkMTU reads a host link's MTU from the kernel.
func hostLinkMTU(t *testing.T, name string) int {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", name, err)
	}
	return l.Attrs().MTU
}

// dumpMTUOnFailure dumps the fixture and plugin logs when the test fails.
func dumpMTUOnFailure(t *testing.T) {
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

func TestMTUOption_TheContainerLinkCarriesTheOptionInEveryMode(t *testing.T) {
	cases := []struct {
		name, mode string
		ipam       bool
	}{
		{"macvlan", "macvlan", false},
		{"ipvlan", "ipvlan", false},
		{"ipam-macvlan", "macvlan", true},
		{"ipam-bridge", "bridge", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			dumpMTUOnFailure(t)
			netName := "dh-itest-mtuopt-" + tc.name
			opts := map[string]string{"mtu": strconv.Itoa(optionMTU)}
			if tc.ipam {
				harness.CreateNetworkIPAM(t, ctx, netName, tc.mode, "", nil, opts)
			} else {
				harness.CreateNetwork(t, ctx, netName, tc.mode, opts)
			}
			id, addr, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
			if got := ctrLinkMTU(t, ctx, id, addr); got != optionMTU {
				t.Errorf("the container link is at %d, want the mtu option %d", got, optionMTU)
			}
			if tc.mode != "bridge" {
				return
			}
			cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			defer cli.Close()
			host := "dh-" + harness.EndpointShortID(t, ctx, cli, id, netName)
			if got := hostLinkMTU(t, host); got != optionMTU {
				t.Errorf("host veth end %s is at %d, want %d on both ends", host, got, optionMTU)
			}
		})
	}
}

func TestMTUOption_BridgeFollowsASmallerPortOnlyWhileItIsAttached(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dumpMTUOnFailure(t)

	netName := "dh-itest-mtuopt-br"
	before := hostLinkMTU(t, harness.BridgeName)
	if before <= optionMTU {
		t.Fatalf("bridge %s is at %d before the test; the test needs it above %d", harness.BridgeName, before, optionMTU)
	}
	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"mtu": strconv.Itoa(optionMTU)})
	id, addr, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	host := "dh-" + harness.EndpointShortID(t, ctx, cli, id, netName)
	if got := ctrLinkMTU(t, ctx, id, addr); got != optionMTU {
		t.Errorf("the container link is at %d, want %d", got, optionMTU)
	}
	if got := hostLinkMTU(t, host); got != optionMTU {
		t.Errorf("host veth end %s is at %d, want %d", host, got, optionMTU)
	}
	if got := hostLinkMTU(t, harness.BridgeName); got != optionMTU {
		t.Errorf("bridge %s is at %d with the %d port attached; the docs say an unpinned bridge follows its smallest port",
			harness.BridgeName, got, optionMTU)
	}

	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	var got int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got = hostLinkMTU(t, harness.BridgeName); got == before {
			return
		}
	}
	t.Errorf("bridge %s is at %d after the port left, want %d as before it joined", harness.BridgeName, got, before)
}

func TestMTUOption_AnIPvlanChildReturnsToTheOptionOnTheNextRenew(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	dumpMTUOnFailure(t)

	netName := "dh-itest-mtuopt-ipv-renew"
	harness.CreateNetwork(t, ctx, netName, "ipvlan", map[string]string{"mtu": strconv.Itoa(optionMTU)})
	id, addr, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	if got := ctrLinkMTU(t, ctx, id, addr); got != optionMTU {
		t.Fatalf("the container link is at %d before the parent moved, want %d", got, optionMTU)
	}

	parent, err := netlink.LinkByName(harness.IpvlanParent)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", harness.IpvlanParent, err)
	}
	was := parent.Attrs().MTU
	t.Cleanup(func() {
		if err := netlink.LinkSetMTU(parent, was); err != nil {
			t.Errorf("restore %s to mtu %d: %v", harness.IpvlanParent, was, err)
		}
	})
	if err := netlink.LinkSetMTU(parent, 9000); err != nil {
		t.Fatalf("LinkSetMTU %s 9000: %v", harness.IpvlanParent, err)
	}
	moved := time.Now()
	if got := ctrLinkMTU(t, ctx, id, addr); got != 9000 {
		t.Fatalf("the container link is at %d after the ipvlan parent went to 9000; the kernel no longer moves the child with its parent", got)
	}

	// The fixture's 2 minute lease renews at T1, about 60 s after the bind (RFC 2131 section 4.4.5).
	budget := 60*time.Second + harness.RetransmitBudget(2)
	var got int
	for time.Since(moved) < budget {
		if got = ctrLinkMTU(t, ctx, id, addr); got == optionMTU {
			t.Logf("the container link is back at %d %s after the parent moved", got, time.Since(moved).Round(time.Second))
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("the container link is at %d %s after the parent moved, want the mtu option %d after a renew", got, budget, optionMTU)
}

func TestMTUOption_AMacvlanParentBelowTheOptionFailsTheEndpointAndLeavesNoLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dumpMTUOnFailure(t)

	const dummyName = "dh-itest-mtud"
	la := netlink.NewLinkAttrs()
	la.Name = dummyName
	dummy := &netlink.Dummy{LinkAttrs: la}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("LinkAdd dummy: %v", err)
	}
	t.Cleanup(func() {
		if err := netlink.LinkDel(dummy); err != nil {
			t.Logf("WARN: LinkDel dummy: %v", err)
		}
	})
	if err := netlink.LinkSetUp(dummy); err != nil {
		t.Fatalf("LinkSetUp dummy: %v", err)
	}

	netName := "dh-itest-mtuopt-refused"
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"parent": dummyName, "mtu": strconv.Itoa(optionMTU)})
	startRefusedLeavesNoChild(t, ctx, netName, dummy)
}

func TestMTUOption_AnIPAMMacvlanParentBelowTheOptionFailsTheEndpointAndLeavesNoLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dumpMTUOnFailure(t)

	parent, err := netlink.LinkByName(harness.HostVeth)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", harness.HostVeth, err)
	}
	was := parent.Attrs().MTU
	t.Cleanup(func() {
		if err := netlink.LinkSetMTU(parent, was); err != nil {
			t.Errorf("restore %s to mtu %d: %v", harness.HostVeth, was, err)
		}
	})
	netName := "dh-itest-mtuopt-ipam-refused"
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "", nil, map[string]string{"mtu": strconv.Itoa(optionMTU)})
	startRefusedLeavesNoChild(t, ctx, netName, parent)
}

// startRefusedLeavesNoChild lowers parent below the option, then requires a refused start naming it and no new child of parent.
func startRefusedLeavesNoChild(t *testing.T, ctx context.Context, netName string, parent netlink.Link) {
	t.Helper()
	name := parent.Attrs().Name
	if err := netlink.LinkSetMTU(parent, optionMTU-50); err != nil {
		t.Fatalf("lower %s to %d: %v", name, optionMTU-50, err)
	}
	children := func() map[string]string {
		links, err := util.DumpResult(netlink.LinkList())
		if err != nil {
			t.Fatalf("LinkList: %v", err)
		}
		out := map[string]string{}
		for _, l := range links {
			if l.Attrs().ParentIndex == parent.Attrs().Index {
				out[l.Attrs().Name] = l.Type()
			}
		}
		return out
	}
	before := children()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, netName+"-ctr")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})
	startErr := cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	if startErr == nil {
		t.Fatalf("the container started with mtu=%d on a parent at %d", optionMTU, optionMTU-50)
	}
	if want := "mtu=" + strconv.Itoa(optionMTU); !strings.Contains(startErr.Error(), want) {
		t.Errorf("ContainerStart error does not name %q: %v", want, startErr)
	}
	for n, typ := range children() {
		if _, ok := before[n]; !ok {
			t.Errorf("link %s (%s) of parent %s is still on the host after the refused endpoint", n, typ, name)
		}
	}
}
