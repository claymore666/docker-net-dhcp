// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// TestPreflightProbe_PassesOnReachableServer checks that validate_dhcp=true creates the network against the working fixture.
func TestPreflightProbe_PassesOnReachableServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	netName := "dh-itest-preflight-ok"
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"validate_dhcp": "true",
	})

	t.Logf("network %s created with validate_dhcp=true on the working fixture", netName)
}

// macvlan and ipvlan children cannot share a parent, the second kind gets EBUSY, and the probe was always macvlan. The
// ipvlan container is up before the probe, so this fails every run on the old code; the hosted lane had caught it once
// in nine weekly runs (#486).

// TestPreflightProbe_IPvlanProbeCoexistsWithIPvlanEndpoints checks that an ipvlan network's probe works beside a running ipvlan container (#486).
func TestPreflightProbe_IPvlanProbeCoexistsWithIPvlanEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		occupantNet = "dh-itest-preflight-ipv-occupant"
		occupantCtr = "dh-itest-preflight-ipv-occupant-ctr"
		probeNet    = "dh-itest-preflight-ipv-probe"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, occupantNet, "ipvlan", nil)
	harness.RunContainer(t, ctx, occupantNet, occupantCtr)

	harness.CreateNetwork(t, ctx, probeNet, "ipvlan", map[string]string{
		"validate_dhcp": "true",
	})

	t.Logf("validate_dhcp=true succeeded on an ipvlan network while an ipvlan "+
		"endpoint was live on the same parent (%s)", harness.IpvlanParent)
}

// TestPreflightProbe_FailsWhenServerUnreachable checks that validate_dhcp=true on a dummy parent with no server fails within the probe budget.
func TestPreflightProbe_FailsWhenServerUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	const dummyName = "dh-itest-iso"

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

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	netName := "dh-itest-preflight-fail"
	start := time.Now()
	res, createErr := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM:   &network.IPAM{Driver: "null"},
		Options: map[string]string{
			"mode":          "macvlan",
			"parent":        dummyName,
			"validate_dhcp": "true",
		},
	})
	elapsed := time.Since(start)
	if createErr == nil {
		_ = cli.NetworkRemove(context.Background(), res.ID)
		t.Fatalf("NetworkCreate succeeded against an isolated dummy parent; probe didn't reject")
	}

	// The probe budget is 8 s (#307), plus about 10 s of setup.
	if elapsed > 18*time.Second {
		t.Errorf("probe took %v; should have failed within ~9-13s", elapsed)
	}

	msg := createErr.Error()
	if !strings.Contains(msg, "DHCP OFFER") && !strings.Contains(msg, "validate_dhcp") {
		t.Errorf("error message doesn't mention the probe failure clearly: %q", msg)
	}
	t.Logf("probe failed in %v with: %s", elapsed, msg)
}

// TestPreflightProbe_RejectedInBridgeMode checks that validate_dhcp=true on a bridge network is refused, not ignored.
func TestPreflightProbe_RejectedInBridgeMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	netName := "dh-itest-preflight-bridge-rejected"
	res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM:   &network.IPAM{Driver: "null"},
		Options: map[string]string{
			"mode":          "bridge",
			"bridge":        harness.BridgeName,
			"validate_dhcp": "true",
		},
	})
	if err == nil {
		_ = cli.NetworkRemove(context.Background(), res.ID)
		t.Fatalf("NetworkCreate accepted validate_dhcp=true in bridge mode; should have rejected")
	}
	if !strings.Contains(err.Error(), "validate_dhcp") {
		t.Errorf("rejection message should mention validate_dhcp; got: %q", err.Error())
	}
}
