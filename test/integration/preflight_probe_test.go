//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// TestPreflightProbe_PassesOnReachableServer is the v0.9.0 / T2-5
// happy path: validate_dhcp=true on the standard fixture (dnsmasq
// on the other end of HostVeth) succeeds and the network is created.
//
// Indirectly exercises the macvlan probe-link create + udhcpc
// one-shot DORA + cleanup paths in pkg/plugin/dhcp_probe.go.
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

	// Reaching this point without t.Fatal means CreateNetwork
	// returned 200; t.Cleanup tears the network down. Implicit
	// pass.
	t.Logf("network %s created with validate_dhcp=true on the working fixture", netName)
}

// TestPreflightProbe_FailsWhenServerUnreachable is the negative
// guard: validate_dhcp=true with a parent that has no DHCP server
// reachable must fail within the probe budget (5s + harness slack)
// with a clear error mentioning the parent NIC.
//
// Uses a dummy interface as the parent — dummies don't carry L2
// traffic to anywhere, so the DHCPDISCOVER vanishes into the void
// and udhcpc times out. Cheaper to set up than a fresh veth pair
// with no peer-side dnsmasq, and the test doesn't exercise the
// peer-side code anyway.
func TestPreflightProbe_FailsWhenServerUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// dh-itest-iso < 15 chars; safe for kernel.
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
		// Tear down the bogus network so the next test doesn't
		// inherit it.
		_ = cli.NetworkRemove(context.Background(), res.ID)
		t.Fatalf("NetworkCreate succeeded against an isolated dummy parent; probe didn't reject")
	}

	// The probe budget is 5s; harness allowed ~10s including
	// the udhcpc setup overhead. If we waited ~30s it means the
	// budget is being ignored somewhere.
	if elapsed > 15*time.Second {
		t.Errorf("probe took %v; should have failed within ~6-10s", elapsed)
	}

	msg := createErr.Error()
	if !strings.Contains(msg, "DHCP OFFER") && !strings.Contains(msg, "validate_dhcp") {
		t.Errorf("error message doesn't mention the probe failure clearly: %q", msg)
	}
	t.Logf("probe failed in %v with: %s", elapsed, msg)
}

// TestPreflightProbe_RejectedInBridgeMode pins the v0.9.0 carve-out:
// validate_dhcp=true is documented as macvlan/ipvlan only. A bridge
// network that requests it must fail at validateModeOptions, not
// silently no-op (which would let an operator think their probe ran
// when it didn't).
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
