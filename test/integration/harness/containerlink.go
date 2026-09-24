// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"fmt"
	"testing"

	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// ChildLinkMode reads a container's link from the kernel inside the container's network namespace, the attributes
// `ip -d link` prints: the kind ("macvlan", "ipvlan"), the sub-mode as the kernel spells it, and the MAC (#905).
func ChildLinkMode(t *testing.T, ctx context.Context, containerID, ifName string) (kind, mode, mac string) {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	ins, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", containerID, err)
	}
	if ins.State == nil || ins.State.Pid == 0 {
		t.Fatalf("container %s has no running process to read the namespace of", containerID)
	}
	link, err := linkInPidNetns(ins.State.Pid, ifName)
	if err != nil {
		t.Fatalf("read %s in container %s: %v", ifName, containerID, err)
	}
	mac = link.Attrs().HardwareAddr.String()
	switch l := link.(type) {
	case *netlink.Macvlan:
		return "macvlan", macvlanModeName(l.Mode), mac
	case *netlink.IPVlan:
		return "ipvlan", ipvlanModeName(l.Mode), mac
	}
	return link.Type(), "", mac
}

// linkInPidNetns reads the link through a netlink socket opened in the namespace of pid, so the test process never
// joins the container's namespace (#905).
func linkInPidNetns(pid int, ifName string) (netlink.Link, error) {
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return nil, fmt.Errorf("netns of pid %d: %w", pid, err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, fmt.Errorf("netlink handle in pid %d's netns: %w", pid, err)
	}
	defer h.Close()
	return h.LinkByName(ifName)
}

// The names are iproute2's, which is what docs/reference.md and the option values use (#905).
func macvlanModeName(m netlink.MacvlanMode) string {
	return map[netlink.MacvlanMode]string{
		netlink.MACVLAN_MODE_PRIVATE:  "private",
		netlink.MACVLAN_MODE_VEPA:     "vepa",
		netlink.MACVLAN_MODE_BRIDGE:   "bridge",
		netlink.MACVLAN_MODE_PASSTHRU: "passthru",
		netlink.MACVLAN_MODE_SOURCE:   "source",
	}[m]
}

func ipvlanModeName(m netlink.IPVlanMode) string {
	return map[netlink.IPVlanMode]string{
		netlink.IPVLAN_MODE_L2:  "l2",
		netlink.IPVLAN_MODE_L3:  "l3",
		netlink.IPVLAN_MODE_L3S: "l3s",
	}[m]
}
