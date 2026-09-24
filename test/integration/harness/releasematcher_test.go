// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// Real dnsmasq-dhcp output from the macvlan fixture while it still released, verbatim.
const dnsmasqReleaseLog = `
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPDISCOVER(dh-itest-mv) 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPOFFER(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPREQUEST(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPACK(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef web1
Aug 20 11:04:19 dnsmasq-dhcp[1]: DHCPRELEASE(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:22 dnsmasq-dhcp[1]: DHCPACK(dh-itest-mv) 192.168.99.35 12:2a:92:35:a0:cb other
`

// Since #800 nothing sends a DHCPRELEASE on a network without release_lease, so TestLeaseRetention_NothingEverReleases
// cannot show its matcher sees one; this canned log is its control, since CountLogLines returns 0 for an unreadable log too.
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

	if got := f.CountLogLines("DHCPRELEASE", otherIP); got != 0 {
		t.Errorf("CountLogLines(DHCPRELEASE, %s) = %d, want 0 — the address filter is "+
			"not being applied, so releases cannot be attributed to an endpoint",
			otherIP, got)
	}

	if got := f.CountLogLines("DHCPACK", releasedIP); got != 1 {
		t.Errorf("CountLogLines(DHCPACK, %s) = %d, want 1", releasedIP, got)
	}

	// The API returns 0 for an unreadable log too, so a caller asserting an absence asserts a presence from the same file first (#800).
	missing := &Fixture{dnsmasqLog: filepath.Join(dir, "does-not-exist.log")}
	if got := missing.CountLogLines("DHCPRELEASE"); got != 0 {
		t.Errorf("CountLogLines on an unreadable log = %d, want 0 (documenting the "+
			"ambiguity, not endorsing it)", got)
	}
}

// The daemon-kill test's bridge matcher asserts an absence too, so it gets the same control, through its own method (#800).
func TestCountBridgeLogLines_SeesADHCPRELEASE(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge-dnsmasq.log")
	if err := os.WriteFile(path, []byte(dnsmasqReleaseLog), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	f := &Fixture{bridgeDnsmasqLog: path}

	const (
		releasedMAC = "1e:c1:60:88:5a:ef"
		otherMAC    = "12:2a:92:35:a0:cb"
	)

	if got := f.CountBridgeLogLines("DHCPRELEASE"); got != 1 {
		t.Errorf("CountBridgeLogLines(DHCPRELEASE) = %d, want 1 — the bridge matcher does "+
			"not see a release in a log that contains one", got)
	}

	// Keyed on the MAC: the address is not preserved across an abrupt daemon death.
	if got := f.CountBridgeLogLines("DHCPRELEASE", releasedMAC); got != 1 {
		t.Errorf("CountBridgeLogLines(DHCPRELEASE, %s) = %d, want 1", releasedMAC, got)
	}
	if got := f.CountBridgeLogLines("DHCPRELEASE", otherMAC); got != 0 {
		t.Errorf("CountBridgeLogLines(DHCPRELEASE, %s) = %d, want 0 — the MAC filter is not "+
			"being applied, so a neighbouring endpoint's release would be blamed on this one",
			otherMAC, got)
	}
	if got := f.CountBridgeLogLines("DHCPACK", otherMAC); got != 1 {
		t.Errorf("CountBridgeLogLines(DHCPACK, %s) = %d, want 1", otherMAC, got)
	}

	if got := (&Fixture{}).CountBridgeLogLines("DHCPRELEASE"); got != 0 {
		t.Errorf("CountBridgeLogLines on an unconfigured fixture = %d, want 0", got)
	}
}
