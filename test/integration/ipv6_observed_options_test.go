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

	// A line with all three values is the v6 one: this fixture serves no v4 options 42, 100 or 101. The strings are
	// comma-free because dnsmasq splits a value on commas; 41 and 42 are numeric because dnsmasq 2.91 names neither
	// (dhcp-common.c). Syntax checked with `dnsmasq --test` on 2.91; the wire half runs on the lane (#1033).
	logMark := harness.MarkPluginLog(t, ctx)
	if _, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-v6obs", nil); err != nil {
		t.Fatalf("the container did not start on a managed segment with a DHCPv6 server: %v", err)
	}

	want := []string{
		"posix_tz=" + harness.V6ObservedPosixTZ,
		"tzdb_tz=" + harness.V6ObservedTZDBTZ,
		"ntp=",
		harness.V6ObservedNTPServer,
	}
	deadline := time.Now().Add(persistentV6BindBudget)
	var got string
	for {
		got = harness.ReadPluginLogSince(t, ctx, logMark)
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "DHCP options received") && containsAll(line, want) {
				t.Logf("options line carries %v", want)
				return
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("no single \"DHCP options received\" line carried %v within %s; the fixture's dnsmasq log is dumped "+
		"above, and the plugin log since the mark was:\n%s", want, persistentV6BindBudget, got)
}
