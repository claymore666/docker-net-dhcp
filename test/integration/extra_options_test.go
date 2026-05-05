//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestExtraOptions_SearchListInResolvConf is the v0.9.0 / T2-2
// guard for option 119: when propagate_dns=true, the container's
// /etc/resolv.conf must carry a single `search` line containing
// every domain from harness.TestSearchList. Option 119 wins over
// option 15 per RFC 3397.
//
// Pairs with the unit test TestBuildResolvConf_SearchListPrecedence —
// this one validates the udhcpc → handler → plugin → mount-ns
// pipeline actually produces the rendered file the unit test pins.
func TestExtraOptions_SearchListInResolvConf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-extra-search"
	ctrName := "dh-itest-extra-search-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"propagate_dns": "true",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	// Search list lands from the persistent client's bound event,
	// same path as DNSServers — poll briefly to absorb the gap
	// between RunContainer's "got an IP" return and the bound-event
	// resolv.conf write.
	wantDomains := strings.Split(harness.TestSearchList, ",")
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if hasAllDomains(out, wantDomains) {
			t.Logf("resolv.conf inside container:\n%s", out)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("expected all of %v on a `search` line within 5s; got:\n%s",
		wantDomains, out)
}

// hasAllDomains reports whether every entry in want appears on a
// `search` line in resolvConf. Order is not asserted — busybox /
// dnsmasq are free to reorder, but every element must be present.
func hasAllDomains(resolvConf string, want []string) bool {
	var searchLine string
	for _, line := range strings.Split(resolvConf, "\n") {
		if strings.HasPrefix(line, "search ") {
			searchLine = line
			break
		}
	}
	if searchLine == "" {
		return false
	}
	for _, d := range want {
		if !strings.Contains(searchLine, d) {
			return false
		}
	}
	return true
}

// TestExtraOptions_NTPAndTFTPLogged is the v0.9.0 / T2-2 guard
// for the surface-via-plugin-log path: NTP / TFTP / boot-file
// values aren't applied to the container automatically, but the
// plugin must log them at info level on bound so operators can
// pick them up without flipping LOG_LEVEL=trace.
//
// The persistent client's `bound` event runs after RunContainer
// returns, so we poll the plugin log file. Reuses
// harness.DumpPluginLog's path resolution.
func TestExtraOptions_NTPAndTFTPLogged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-extra-ntp"
	ctrName := "dh-itest-extra-ntp-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// Doesn't need propagate_dns — the log line fires unconditionally
	// when the captured info struct has any of the new fields set.
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	harness.RunContainer(t, ctx, netName, ctrName)

	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = harness.ReadPluginLog(t, ctx)
		if strings.Contains(got, "DHCP options received") &&
			strings.Contains(got, harness.TestNTPServer) &&
			strings.Contains(got, harness.TestTFTPServer) {
			t.Logf("plugin log contains NTP=%s TFTP=%s as expected",
				harness.TestNTPServer, harness.TestTFTPServer)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("plugin log did not surface NTP=%s / TFTP=%s within 5s",
		harness.TestNTPServer, harness.TestTFTPServer)
}
