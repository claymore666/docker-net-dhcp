// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/netip"

	dContainer "github.com/moby/moby/api/types/container"
	dNetwork "github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"
)

// dockerClient is the part of the Docker client the plugin calls, all GET or HEAD
// so the read-only transport passes them (#670).
type dockerClient interface {
	Ping(ctx context.Context, options docker.PingOptions) (docker.PingResult, error)
	ServerVersion(ctx context.Context, options docker.ServerVersionOptions) (docker.ServerVersionResult, error)
	ClientVersion() string
	NetworkList(ctx context.Context, options docker.NetworkListOptions) (docker.NetworkListResult, error)
	NetworkInspect(ctx context.Context, networkID string, options docker.NetworkInspectOptions) (docker.NetworkInspectResult, error)
	ContainerInspect(ctx context.Context, containerID string, options docker.ContainerInspectOptions) (docker.ContainerInspectResult, error)
	Close() error
}

func inspectNetwork(ctx context.Context, c dockerClient, networkID string) (dNetwork.Inspect, error) {
	res, err := c.NetworkInspect(ctx, networkID, docker.NetworkInspectOptions{})
	return res.Network, err
}

func listNetworks(ctx context.Context, c dockerClient) ([]dNetwork.Summary, error) {
	res, err := c.NetworkList(ctx, docker.NetworkListOptions{})
	return res.Items, err
}

func inspectContainer(ctx context.Context, c dockerClient, containerID string) (dContainer.InspectResponse, error) {
	res, err := c.ContainerInspect(ctx, containerID, docker.ContainerInspectOptions{})
	return res.Container, err
}

// prefixString is "" for a zero prefix, which String renders as "invalid Prefix" (#178).
func prefixString(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}
	return p.String()
}

var errNoSubnet = errors.New("no valid subnet")

// prefixIPNet is what net.ParseCIDR returned for the same text: the masked network address and its mask (#178).
func prefixIPNet(p netip.Prefix) (*net.IPNet, bool) {
	if !p.IsValid() {
		return nil, false
	}
	m := p.Masked()
	return &net.IPNet{IP: m.Addr().AsSlice(), Mask: net.CIDRMask(m.Bits(), m.Addr().BitLen())}, true
}
