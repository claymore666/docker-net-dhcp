// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// Real dnsmasq-dhcp output, captured from the macvlan fixture while it
// still released. Kept verbatim: this file exists to prove the matcher
// recognises what the server actually writes, so paraphrasing it would
// defeat the point.
const dnsmasqReleaseLog = `
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPDISCOVER(dh-itest-mv) 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPOFFER(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPREQUEST(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPACK(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef web1
Aug 20 11:04:19 dnsmasq-dhcp[1]: DHCPRELEASE(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:22 dnsmasq-dhcp[1]: DHCPACK(dh-itest-mv) 192.168.99.35 12:2a:92:35:a0:cb other
`

// TestCountLogLines_SeesADHCPRELEASE is the control for
// TestLeaseRetention_NothingEverReleases, and it has to live here
// because that test can no longer produce its own.
//
// Since #800 nothing this plugin runs sends a DHCPRELEASE, so the
// integration test asserts an absence with no way to demonstrate that
// the matcher would notice a presence. That is the shape where a
// silently broken check reads exactly like a clean tree: CountLogLines
// returns 0 for a log it cannot read, for a log with no releases, and
// for a matcher that stopped recognising the token — three very
// different states, one number.
//
// So the presence is driven here instead, against a canned log that
// really contains one. If the plugin ever starts releasing again, the
// integration test goes red because THIS test says the matcher works.
func TestCountLogLines_SeesADHCPRELEASE(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dnsmasq.log")
	if err := os.WriteFile(path, []byte(dnsmasqReleaseLog), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	f := &Fixture{dnsmasqLog: path}

	const (
		releasedIP = "192.168.99.34"
		otherIP    = "192.168.99.35"
	)

	if got := f.CountLogLines("DHCPRELEASE"); got != 1 {
		t.Errorf("CountLogLines(DHCPRELEASE) = %d, want 1 — the matcher does not see a "+
			"release in a log that contains one, so an absence proves nothing", got)
	}
	if got := f.CountLogLines("DHCPRELEASE", releasedIP); got != 1 {
		t.Errorf("CountLogLines(DHCPRELEASE, %s) = %d, want 1", releasedIP, got)
	}

	// Keyed on the address, not merely on the token: the shared fixture
	// carries every test's traffic, so a matcher that ignored the IP
	// would let a neighbouring endpoint's release fail this run — or,
	// worse, let this endpoint's release be blamed on a neighbour.
	if got := f.CountLogLines("DHCPRELEASE", otherIP); got != 0 {
		t.Errorf("CountLogLines(DHCPRELEASE, %s) = %d, want 0 — the address filter is "+
			"not being applied, so releases cannot be attributed to an endpoint",
			otherIP, got)
	}

	// And the token the retention test uses for its own positive
	// control resolves against the same reader.
	if got := f.CountLogLines("DHCPACK", releasedIP); got != 1 {
		t.Errorf("CountLogLines(DHCPACK, %s) = %d, want 1", releasedIP, got)
	}

	// An unreadable log must not be mistaken for a clean one by anyone
	// reading these counts. It cannot be made to fail here — the API
	// returns 0 either way — which is exactly why every caller asserting
	// an absence has to assert a presence from the same file first.
	missing := &Fixture{dnsmasqLog: filepath.Join(dir, "does-not-exist.log")}
	if got := missing.CountLogLines("DHCPRELEASE"); got != 0 {
		t.Errorf("CountLogLines on an unreadable log = %d, want 0 (documenting the "+
			"ambiguity, not endorsing it)", got)
	}
}
