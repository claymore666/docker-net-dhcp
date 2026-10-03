// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// With ipv6_iid=stable-privacy the plugin forms the identifier from the secret in STATE_DIR (RFC 7217, #1032), so the
// address is not the modified EUI-64 of the MAC, and the same inputs form the same address when the endpoint is built
// again. The secret lives in the plugin's state, out of the test's reach, so the test asserts both properties on the
// container's own link and not the value.

// lowHalf returns the interface identifier of an address the container's `ip` printed, with or without a prefix length.
func lowHalf(t *testing.T, addr string) []byte {
	t.Helper()
	bare, _, _ := strings.Cut(addr, "/")
	a, err := netip.ParseAddr(bare)
	if err != nil {
		t.Fatalf("parse %q: %v", addr, err)
	}
	b := a.As16()
	return b[8:]
}

// TestSLAAC_StablePrivacyFormsAnAddressThatIsNotTheMACsAndSurvivesARestart checks the identifier against the MAC's EUI-64 and across docker restart (#1032).
func TestSLAAC_StablePrivacyFormsAnAddressThatIsNotTheMACsAndSurvivesARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6SLAAC)
	dumpOnFailure(t, f)
	prefix := v6SegmentPrefix(t)

	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")
	const netName = "dh-itest-iid1"
	id, err := startOnV6SegmentAs(t, ctx, cli, f, onV6Bridge, netName,
		map[string]string{"ipv6": "", "ipv6_mode": "slaac", "ipv6_iid": "stable-privacy"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_iid=stable-privacy segment: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, onV6Bridge)
	_, mac1 := ipamNetworkAddress(t, ctx, cli, id, netName)

	before, flags := awaitPluginAppliedV6(t, ctx, w, id, prefix, slaacAddrBudget(), v6InstalledSinceBaseline)
	assertHealthyFormedAddress(t, before, flags)
	if got, eui := lowHalf(t, before), lowHalf(t, eui64(t, prefix, mac1)); string(got) == string(eui) {
		t.Errorf("the container holds %s, whose identifier is the modified EUI-64 of its MAC %s: ipv6_iid=stable-privacy "+
			"did not reach the client (#1032)", before, mac1)
	}
	if shown := inspectV6(t, ctx, cli, id, netName); shown != before {
		t.Errorf("docker inspect reports %q and the container holds %q", shown, before)
	}

	// CounterWindow refuses a window that spans a restart, so the first one closes here (#1032).
	if b, a := w.End(); a.IPv6SLAACAddresses-b.IPv6SLAACAddresses != 1 {
		t.Errorf("ipv6_slaac_addresses moved by %d before the restart, want 1", a.IPv6SLAACAddresses-b.IPv6SLAACAddresses)
	}

	w2 := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")
	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	_, mac2 := ipamNetworkAddress(t, ctx, cli, id, netName)
	if !strings.EqualFold(mac1, mac2) {
		t.Fatalf("the endpoint came back on MAC %s, it held %s: the restart does not exercise a formation from the "+
			"same inputs, so this run says nothing about stability (#1032)", mac2, mac1)
	}
	after, flags := awaitPluginAppliedV6(t, ctx, w2, id, prefix, slaacAddrBudget(), v6InstalledSinceBaseline)
	assertHealthyFormedAddress(t, after, flags)
	if _, a := w2.End(); a.IPv6SLAACAddresses < 1 {
		t.Errorf("ipv6_slaac_addresses = %d on the plugin process that came back, want at least 1: the formation "+
			"after the restart is this process's own (#1032)", a.IPv6SLAACAddresses)
	}
	if after != before {
		t.Errorf("the container held %s before docker restart and %s after it, on the same MAC and prefix: a "+
			"stable-privacy identifier has to come back unchanged (RFC 7217 section 5, #1032)", before, after)
	}
	f.AssertExchange(30 * time.Second)
}
