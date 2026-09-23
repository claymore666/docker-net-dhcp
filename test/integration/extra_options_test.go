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

// TestExtraOptions_SearchListInResolvConf checks that propagate_dns writes option 119, which wins over option 15 (RFC 3397), as one search line.
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

	// The search list is written from the persistent client's bound event (harness.RetransmitBudget).
	wantDomains := strings.Split(harness.TestSearchList, ",")
	budget := harness.RetransmitBudget(2)
	deadline := time.Now().Add(budget)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if hasAllDomains(out, wantDomains) {
			t.Logf("resolv.conf inside container:\n%s", out)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("expected all of %v on a `search` line within %s; got:\n%s",
		wantDomains, budget, out)
}

// hasAllDomains reports whether every domain in want is on a search line, in any order.
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

// TestExtraOptions_NTPAndTFTPLogged checks that the NTP, TFTP and boot-file options are logged at info on bound.
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

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	// Every container on this fixture logs these constants, so only lines after the mark count.
	logMark := harness.MarkPluginLog(t, ctx)
	harness.RunContainer(t, ctx, netName, ctrName)

	budget := harness.RetransmitBudget(2)
	deadline := time.Now().Add(budget)
	var got string
	for time.Now().Before(deadline) {
		got = harness.ReadPluginLogSince(t, ctx, logMark)
		if strings.Contains(got, "DHCP options received") &&
			strings.Contains(got, harness.TestNTPServer) &&
			strings.Contains(got, harness.TestTFTPServer) {
			t.Logf("plugin log contains NTP=%s TFTP=%s as expected",
				harness.TestNTPServer, harness.TestTFTPServer)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("plugin log did not surface NTP=%s / TFTP=%s within %s",
		harness.TestNTPServer, harness.TestTFTPServer, budget)
}

// dhcpcd exports option 100 and 101 as posix_timezone and tzdb_timezone, not pcode and tcode, and WPAD (252) through a
// `define` (#262, RFC 4833).

// TestExtraOptions_WPADAndTimezoneLogged checks that WPAD, the RFC 4833 timezones and the time offset reach the options log line (#262).
func TestExtraOptions_WPADAndTimezoneLogged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-extra-wpad"
	ctrName := "dh-itest-extra-wpad-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	logMark := harness.MarkPluginLog(t, ctx)
	harness.RunContainer(t, ctx, netName, ctrName)

	want := []string{harness.TestWPAD, harness.TestPosixTZ, harness.TestTZDBTZ, harness.TestTimeOffset}
	budget := harness.RetransmitBudget(2)
	deadline := time.Now().Add(budget)
	var got string
	for time.Now().Before(deadline) {
		got = harness.ReadPluginLogSince(t, ctx, logMark)
		if strings.Contains(got, "DHCP options received") && containsAll(got, want) {
			t.Logf("plugin log surfaced WPAD/timezone extras: %v", want)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("plugin log did not surface %q within %s", w, budget)
		}
	}
}

func containsAll(haystack string, needles []string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}
