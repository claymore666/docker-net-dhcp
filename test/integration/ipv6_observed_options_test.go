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
)

// TestDHCPv6_ObservedOptionsReachTheOptionsLogLine checks that options 41, 42 and 56 become posix_tz, tzdb_tz and ntp (#1033, #859).
func TestDHCPv6_ObservedOptionsReachTheOptionsLogLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	f := harness.NewV6FixtureWithArgs(t, harness.V6Managed,
		append(harness.RangeArgsFor(harness.V6Managed), harness.V6ObservedOptionsArgs()...))
	dumpOnFailure(t, f)

	// The strings are comma-free because dnsmasq splits a value on commas; 41 and 42 are numeric because dnsmasq 2.91
	// names neither (dhcp-common.c). The first v6 line is the bind's, after a Confirm whose Reply carries none of the
	// three, so a later line would pass at T1 (#1033).
	logMark := harness.MarkPluginLog(t, ctx)
	const netName = "dh-itest-v6obs"
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName, nil)
	if err != nil {
		t.Fatalf("the container did not start on a managed segment with a DHCPv6 server: %v", err)
	}
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	ep := ins.NetworkSettings.Networks[netName]
	if ep == nil || len(ep.NetworkID) < 12 {
		t.Fatalf("the container has no endpoint on %s: %+v", netName, ins.NetworkSettings.Networks)
	}
	ours := []string{"DHCP options received", "is_ipv6=true", "network=" + ep.NetworkID[:12]}

	want := []string{
		"posix_tz=" + harness.V6ObservedPosixTZ,
		"tzdb_tz=" + harness.V6ObservedTZDBTZ,
		"ntp=",
		harness.V6ObservedNTPServer,
		harness.V6SearchDomain,
	}
	deadline := time.Now().Add(persistentV6BindBudget)
	var got string
	for {
		got = harness.ReadPluginLogSince(t, ctx, logMark)
		for _, line := range strings.Split(got, "\n") {
			if !containsAll(line, ours) {
				continue
			}
			if !containsAll(line, want) {
				t.Fatalf("the first v6 \"DHCP options received\" line lacks some of %v:\n%s", want, line)
			}
			t.Logf("the bind's options line carries %v", want)
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("no v6 \"DHCP options received\" line within %s; the fixture's dnsmasq log is dumped above, and the "+
		"plugin log since the mark was:\n%s", persistentV6BindBudget, got)
}
