// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// holdHostClientPort binds the bridge's address on port 68 as a host DHCP client does, with SO_REUSEADDR and bound to
// the bridge; a release from the bridge must still bind beside it (#1346).
func holdHostClientPort(t *testing.T) {
	t.Helper()
	addr, _, err := net.ParseCIDR(harness.BridgeAddr)
	if err != nil {
		t.Fatalf("ParseCIDR %s: %v", harness.BridgeAddr, err)
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var opErr error
		if err := c.Control(func(fd uintptr) {
			opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			if opErr == nil {
				opErr = syscall.BindToDevice(int(fd), harness.BridgeName)
			}
		}); err != nil {
			return err
		}
		return opErr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(addr.String(), "68"))
	if err != nil {
		t.Fatalf("holding %s:68 as a host DHCP client would: %v", addr, err)
	}
	t.Cleanup(func() { _ = pc.Close() })
}

// TestReleaseLease_ABridgeReleaseLeavesBesideAHostDHCPClient checks that the release_lease=on_remove hand-back reaches
// the server while a host client holds port 68 on the bridge (#1346).
func TestReleaseLease_ABridgeReleaseLeavesBesideAHostDHCPClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-release-hostcl"
		ctrName = "dh-itest-release-hostcl-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"release_lease": "on_remove"})

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)
	if !waitLeaseFile(t, fixture.BridgeLeaseFile(), ip, true) {
		t.Fatalf("the bridge dnsmasq's lease DB has no entry for %s, so a release cannot be "+
			"told from a lease that was never recorded", ip)
	}

	holdHostClientPort(t)
	releasesBefore := fixture.CountBridgeLogLines("DHCPRELEASE", ip)

	if _, err := cli.ContainerStop(ctx, id, docker.ContainerStopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	if _, gone := waitLeaseFileWithin(t, fixture.BridgeLeaseFile(), ip, false, onRemoveVisibleBudget); !gone {
		t.Errorf("%s after the stop, past the release_lease=on_remove window, the bridge dnsmasq "+
			"still holds %s. A host DHCP client holds port 68 on the bridge with SO_REUSEADDR, as "+
			"the common clients do, and the release could not take the port beside it",
			onRemoveVisibleBudget, ip)
	}
	releaseLines := fixture.CountBridgeLogLines("DHCPRELEASE", ip) - releasesBefore
	unknown := fixture.CountBridgeLogLines("DHCPRELEASE", ip, "unknown lease")
	t.Logf("the bridge dnsmasq logged %d DHCPRELEASE line(s) naming %s", releaseLines, ip)
	if releaseLines < 1 {
		t.Errorf("the bridge dnsmasq logged %d DHCPRELEASE line(s) for %s, want at least 1: "+
			"the lease can leave the DB for reasons other than a release", releaseLines, ip)
	}
	if unknown != 0 {
		t.Errorf("the bridge dnsmasq logged %d DHCPRELEASE line(s) for %s as an unknown lease, "+
			"so the release did not name the lease the container held", unknown, ip)
	}
}
