// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestKea6Option17ReachesTheOptionsLogLine checks that two option 17 instances become one vendor_17 entry each in the
// bind's options line (#1203); Kea's wire order is its own, so the entries are compared as a set.
func TestKea6Option17ReachesTheOptionsLogLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	spec := harness.RASpec{
		Managed:        true,
		RouterLifetime: harness.AdvertRouteLifetime,
		Prefixes:       []harness.RAPrefix{advertPrefix(t, harness.V6SubnetV6CIDR, false, 1800, 1800)},
	}
	f, sender := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), spec)
	dumpOnFailure(t, f)
	kea := harness.NewKea6Fixture(t, f, harness.WithKea6VendorOpts(
		harness.Kea6VendorOpt{Enterprise: 9, SubCode: 1, SubData: []byte{0x00, 0xaa}},
		harness.Kea6VendorOpt{Enterprise: 3561, SubCode: 2, SubData: []byte{0xbb, 0xcc}},
	))
	t.Cleanup(func() {
		if t.Failed() {
			kea.DumpLogs(func(s string) { t.Log(s) })
		}
	})
	answerRouterSolicits(t, f.Bridge(), sender, spec)

	// The first v6 line of this network is the bind's, so a later renew line cannot pass for it (#1033).
	logMark := harness.MarkPluginLog(t, ctx)
	const netName = "dh-itest-kea6vo"
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName,
		map[string]string{"ipv6_mode": "dhcp", "propagate_dns": ""})
	if err != nil {
		t.Fatalf("ContainerStart on the Kea segment: %v", err)
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

	want := []string{"3561:bbcc", "9:00aa"}
	deadline := time.Now().Add(persistentV6BindBudget)
	var got string
	for {
		got = harness.ReadPluginLogSince(t, ctx, logMark)
		for _, line := range strings.Split(got, "\n") {
			if !containsAll(line, ours) {
				continue
			}
			if entries := vendor17Entries(line); !equalStrings(entries, want) {
				t.Fatalf("the first v6 \"DHCP options received\" line has vendor_17 entries %q, want %q:\n%s",
					entries, want, line)
			}
			t.Logf("the bind's options line carries vendor_17 entries %q", want)
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("no v6 \"DHCP options received\" line within %s; Kea's log is dumped above, and the plugin log since the "+
		"mark was:\n%s", persistentV6BindBudget, got)
}

// vendor17Entries is the sorted entries of vendor_17="[e1 e2]", or vendor_17=[e1] for one; nil when absent (#1203).
func vendor17Entries(line string) []string {
	_, rest, ok := strings.Cut(line, "vendor_17=")
	if !ok {
		return nil
	}
	rest = strings.TrimPrefix(rest, `"`)
	list, _, ok := strings.Cut(strings.TrimPrefix(rest, "["), "]")
	if !ok {
		return nil
	}
	out := strings.Fields(list)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
