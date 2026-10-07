// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	docker "github.com/moby/moby/client"
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

// containerHandle opens a netlink handle in the container's own network namespace; the caller closes it.
func containerHandle(t *testing.T, ctx context.Context, containerID string) *netlink.Handle {
	t.Helper()
	cli, err := NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	ins, err := cli.ContainerInspect(ctx, containerID, docker.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", containerID, err)
	}
	if ins.Container.State == nil || ins.Container.State.Pid == 0 {
		t.Fatalf("container %s has no running process to read the namespace of", containerID)
	}
	ns, err := netns.GetFromPid(ins.Container.State.Pid)
	if err != nil {
		t.Fatalf("netns of container %s: %v", containerID, err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatalf("netlink handle in container %s: %v", containerID, err)
	}
	return h
}

// ContainerV6Routes reads the routes through a netlink socket in the container's namespace: busybox ip neither
// filters on nor prints the protocol, and the plugin's delegated-prefix routes are told apart by it (#214).
func ContainerV6Routes(t *testing.T, ctx context.Context, containerID string) []ContainerRoute {
	t.Helper()
	h := containerHandle(t, ctx, containerID)
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

// AddForeignAggregate installs, inside the container, an unreachable proto dhcp route for prefix: what a DHCPv6-PD
// client running in the container would leave, and what the plugin must never claim as its own (#214).
func AddForeignAggregate(t *testing.T, ctx context.Context, containerID string, prefix netip.Prefix) {
	t.Helper()
	h := containerHandle(t, ctx, containerID)
	defer h.Close()
	dst := &net.IPNet{IP: prefix.Addr().AsSlice(), Mask: net.CIDRMask(prefix.Bits(), 128)}
	err := h.RouteAdd(&netlink.Route{Dst: dst, Type: unix.RTN_UNREACHABLE, Protocol: unix.RTPROT_DHCP,
		Family: netlink.FAMILY_V6, Table: unix.RT_TABLE_MAIN, Scope: unix.RT_SCOPE_UNIVERSE})
	if err != nil {
		t.Fatalf("add the foreign aggregate %s in container %s: %v", prefix, containerID, err)
	}
}
