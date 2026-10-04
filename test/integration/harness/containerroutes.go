// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"net/netip"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// ContainerRoute is one IPv6 route of the container's main table as the kernel holds it.
type ContainerRoute struct {
	Dst         netip.Prefix
	Unreachable bool
	DHCP        bool
	Dev         string
}

// ContainerV6Routes reads the routes through a netlink socket in the container's namespace: busybox ip neither
// filters on nor prints the protocol, and the plugin's delegated-prefix routes are told apart by it (#214).
func ContainerV6Routes(t *testing.T, ctx context.Context, containerID string) []ContainerRoute {
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
	ns, err := netns.GetFromPid(ins.State.Pid)
	if err != nil {
		t.Fatalf("netns of container %s: %v", containerID, err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatalf("netlink handle in container %s: %v", containerID, err)
	}
	defer h.Close()
	routes, err := util.DumpResult(h.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: unix.RT_TABLE_MAIN},
		netlink.RT_FILTER_TABLE))
	if err != nil {
		t.Fatalf("list the routes of container %s: %v", containerID, err)
	}
	out := make([]ContainerRoute, 0, len(routes))
	for _, r := range routes {
		if r.Dst == nil {
			continue
		}
		addr, ok := netip.AddrFromSlice(r.Dst.IP)
		if !ok {
			continue
		}
		ones, _ := r.Dst.Mask.Size()
		cr := ContainerRoute{Dst: netip.PrefixFrom(addr.Unmap(), ones), Unreachable: r.Type == unix.RTN_UNREACHABLE,
			DHCP: r.Protocol == unix.RTPROT_DHCP}
		if l, err := h.LinkByIndex(r.LinkIndex); err == nil {
			cr.Dev = l.Attrs().Name
		}
		out = append(out, cr)
	}
	return out
}

// DelegatedAggregates are the routes the plugin installs for delegated prefixes: unreachable, protocol dhcp (#214).
func DelegatedAggregates(routes []ContainerRoute) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range routes {
		if r.Unreachable && r.DHCP {
			out = append(out, r.Dst)
		}
	}
	return out
}
