// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func withStateDir(t *testing.T, dir string) {
	t.Helper()
	prev := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = prev })
}

func TestSaveLoadOptions_Roundtrip(t *testing.T) {
	withStateDir(t, t.TempDir())

	want := DHCPNetworkOptions{
		Mode:         ModeMacvlan,
		Parent:       "ens18",
		IPv6:         true,
		LeaseTimeout: 45 * time.Second,
		Gateway:      "192.168.0.1",
	}
	if err := saveOptions("net123", want); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	got, err := loadOptions("net123")
	if err != nil {
		t.Fatalf("loadOptions: %v", err)
	}
	if got != want {
		t.Errorf("roundtrip mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

func TestLoadOptions_Missing(t *testing.T) {
	withStateDir(t, t.TempDir())
	_, err := loadOptions("never-saved")
	if err == nil {
		t.Fatal("expected error for missing options, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist (or wrapped); got %T %v", err, err)
	}
}

func TestLoadOptions_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := loadOptions("bad")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("corrupt JSON should NOT report as ErrNotExist; got %v", err)
	}
}

func TestDeleteOptions_Idempotent(t *testing.T) {
	withStateDir(t, t.TempDir())
	if err := deleteOptions("ghost"); err != nil {
		t.Errorf("deleteOptions on missing file should be nil, got %v", err)
	}
	if err := saveOptions("real", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	if err := deleteOptions("real"); err != nil {
		t.Errorf("first delete: %v", err)
	}
	if err := deleteOptions("real"); err != nil {
		t.Errorf("second delete should still be nil, got %v", err)
	}
}

func TestSaveOptions_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	for i := 0; i < 5; i++ {
		if err := saveOptions("net-many", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
			t.Fatalf("save iter %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSaveOptions_CreatesStateDir(t *testing.T) {
	parent := t.TempDir()
	subdir := filepath.Join(parent, "nested", "state")
	withStateDir(t, subdir)
	if err := saveOptions("net1", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions should auto-create state dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, "net1.json")); err != nil {
		t.Errorf("expected file to exist: %v", err)
	}
}

func newPluginForTest() *Plugin {
	return &Plugin{
		joinHints:            make(map[string]joinHint),
		persistentDHCP:       make(map[string]*dhcpManager),
		endpointFingerprints: make(map[string]endpointFingerprint),
	}
}

func TestTombstones_RoundtripAndConsume(t *testing.T) {
	withStateDir(t, t.TempDir())

	p := newPluginForTest()

	if mac, ip, ipv6, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Errorf("consumeTombstone on empty state returned (%q, %q, %q, true), want (\"\", \"\", \"\", false)", mac, ip, ipv6)
	}

	p.addTombstone("net-A", "", "02:42:ac:11:00:01", "192.168.0.166", "fe80::1")
	mac, ip, ipv6, ok := p.consumeTombstone("net-A", dhcpHostname{})
	if !ok || mac != "02:42:ac:11:00:01" || ip != "192.168.0.166" || ipv6 != "fe80::1" {
		t.Errorf("consumeTombstone net-A: got (%q, %q, %q, %v), want (02:42:ac:11:00:01, 192.168.0.166, fe80::1, true)", mac, ip, ipv6, ok)
	}
	if mac, ip, ipv6, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Errorf("second consumeTombstone returned (%q, %q, %q, true); should be empty after consume", mac, ip, ipv6)
	}
}

func TestTombstones_ConsumedCounter(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()

	if got := p.tombstonesConsumed.Load(); got != 0 {
		t.Fatalf("fresh plugin: tombstones_consumed=%d, want 0", got)
	}

	if _, _, _, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Fatal("consumeTombstone on empty state returned ok=true")
	}
	if got := p.tombstonesConsumed.Load(); got != 0 {
		t.Errorf("a lookup that found no tombstone counted anyway: tombstones_consumed=%d, want 0", got)
	}

	p.addTombstone("net-A", "", "02:42:ac:11:00:01", "192.168.0.166", "")
	if _, _, _, ok := p.consumeTombstone("net-A", dhcpHostname{}); !ok {
		t.Fatal("consumeTombstone did not find the tombstone just added")
	}
	if got := p.tombstonesConsumed.Load(); got != 1 {
		t.Errorf("after one replay: tombstones_consumed=%d, want 1", got)
	}

	if _, _, _, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Fatal("tombstone was consumable twice")
	}
	if got := p.tombstonesConsumed.Load(); got != 1 {
		t.Errorf("a second lookup double-counted: tombstones_consumed=%d, want 1", got)
	}

	p.addTombstone("net-B", "", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "")
	p.addTombstone("net-B", "", "bb:bb:bb:bb:bb:bb", "10.0.0.2", "")
	if _, _, _, ok := p.consumeTombstone("net-B", dhcpHostname{}); ok {
		t.Fatal("two candidates on one network should not resolve")
	}
	if got := p.tombstonesConsumed.Load(); got != 1 {
		t.Errorf("an ambiguous match counted as a replay: tombstones_consumed=%d, want 1", got)
	}
}

func TestTombstones_DifferentNetworksDoNotMix(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "")
	p.addTombstone("net-B", "", "bb:bb:bb:bb:bb:bb", "10.0.0.2", "fe80::2")

	if mac, ip, ipv6, ok := p.consumeTombstone("net-A", dhcpHostname{}); !ok || mac != "aa:aa:aa:aa:aa:aa" || ip != "10.0.0.1" || ipv6 != "" {
		t.Errorf("net-A consume: got (%q, %q, %q, %v)", mac, ip, ipv6, ok)
	}
	if mac, ip, ipv6, ok := p.consumeTombstone("net-B", dhcpHostname{}); !ok || mac != "bb:bb:bb:bb:bb:bb" || ip != "10.0.0.2" || ipv6 != "fe80::2" {
		t.Errorf("net-B consume: got (%q, %q, %q, %v)", mac, ip, ipv6, ok)
	}
}

func TestTombstones_TwoOnSameNetworkBothSkipped(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "")
	p.addTombstone("net-A", "", "bb:bb:bb:bb:bb:bb", "10.0.0.2", "")

	if mac, ip, ipv6, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Errorf("consumeTombstone with 2 candidates should return ok=false, got (%q, %q, %q, true)", mac, ip, ipv6)
	}
}

func TestTombstones_AmbiguousMatchesDropped(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "")
	p.addTombstone("net-A", "", "bb:bb:bb:bb:bb:bb", "10.0.0.2", "")

	if _, _, _, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Fatal("first consume must return ok=false (ambiguous)")
	}
	ts, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones: %v", err)
	}
	for _, ts := range ts {
		if ts.NetworkID == "net-A" {
			t.Errorf("net-A tombstone survived ambiguous consume: %+v", ts)
		}
	}
}

func TestTombstones_HostnameNarrowsMatch(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "alpha", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "")
	p.addTombstone("net-A", "bravo", "bb:bb:bb:bb:bb:bb", "10.0.0.2", "")

	mac, ip, _, ok := p.consumeTombstone("net-A", dhcpHostname{name: "alpha"})
	if !ok || mac != "aa:aa:aa:aa:aa:aa" || ip != "10.0.0.1" {
		t.Fatalf("alpha consume: got (%q, %q, %v), want alpha's tombstone", mac, ip, ok)
	}
	mac, ip, _, ok = p.consumeTombstone("net-A", dhcpHostname{name: "bravo"})
	if !ok || mac != "bb:bb:bb:bb:bb:bb" || ip != "10.0.0.2" {
		t.Fatalf("bravo consume: got (%q, %q, %v), want bravo's tombstone", mac, ip, ok)
	}
}

func TestTombstones_EmptyHostnameMatchesAny(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "")
	mac, _, _, ok := p.consumeTombstone("net-A", dhcpHostname{name: "alpha"})
	if !ok || mac != "aa:aa:aa:aa:aa:aa" {
		t.Errorf("v0.5.0 tombstone should still match: got (%q, %v)", mac, ok)
	}
}

func TestTombstones_ConcurrentAddDoesNotLose(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			p.addTombstone(
				fmt.Sprintf("net-%d", i),
				"",
				fmt.Sprintf("aa:bb:cc:dd:%02x:%02x", i>>8, i&0xff),
				fmt.Sprintf("10.0.0.%d", (i%254)+1),
				"",
			)
		}(i)
	}
	wg.Wait()

	ts, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones: %v", err)
	}
	if len(ts) != N {
		t.Errorf("lost tombstones under concurrent adds: got %d, want %d", len(ts), N)
	}
}

func TestTombstones_ExpiredEntriesPruned(t *testing.T) {
	withStateDir(t, t.TempDir())
	old := []tombstone{{
		NetworkID:   "net-A",
		MacAddress:  "ff:ff:ff:ff:ff:ff",
		IPAddress:   "10.0.0.99",
		IPv6Address: "fe80::99",
		DeletedAt:   time.Now().Add(-2 * tombstoneTTL),
	}}
	if err := saveTombstones(old); err != nil {
		t.Fatalf("saveTombstones: %v", err)
	}
	p := newPluginForTest()
	if mac, ip, ipv6, ok := p.consumeTombstone("net-A", dhcpHostname{}); ok {
		t.Errorf("expired tombstone should not be consumed, got (%q, %q, %q, true)", mac, ip, ipv6)
	}
}

func TestRememberAndTakeEndpoint(t *testing.T) {
	p := newPluginForTest()
	p.rememberEndpoint("ep-1", endpointFingerprint{MAC: "02:42:ac:11:00:02", IPv4: "192.168.0.10", IPv6: "fe80::10"}, dhcpHostname{})
	fp, ok := p.takeEndpoint("ep-1")
	if !ok || fp.MAC != "02:42:ac:11:00:02" || fp.IPv4 != "192.168.0.10" || fp.IPv6 != "fe80::10" {
		t.Errorf("take after remember: got (%+v, %v)", fp, ok)
	}
	if _, ok := p.takeEndpoint("ep-1"); ok {
		t.Errorf("take must remove the entry it returned")
	}
	p.rememberEndpoint("ep-2", endpointFingerprint{MAC: "", IPv4: "10.0.0.1"}, dhcpHostname{})
	if _, ok := p.takeEndpoint("ep-2"); ok {
		t.Errorf("rememberEndpoint with empty MAC must be a no-op")
	}
}

func TestUpdateEndpointIPs_PreservesUnsetField(t *testing.T) {
	p := newPluginForTest()
	p.rememberEndpoint("ep-1", endpointFingerprint{MAC: "aa:bb:cc:dd:ee:ff", IPv4: "10.0.0.1", IPv6: "fe80::1"}, dhcpHostname{})

	p.updateEndpointIPs("ep-1", "10.0.0.2", "")
	fp, _ := p.takeEndpoint("ep-1")
	if fp.IPv4 != "10.0.0.2" || fp.IPv6 != "fe80::1" {
		t.Errorf("v4-only update lost v6: %+v", fp)
	}

	p.rememberEndpoint("ep-2", endpointFingerprint{MAC: "aa:bb:cc:dd:ee:ff", IPv4: "10.0.0.1", IPv6: "fe80::1"}, dhcpHostname{})
	p.updateEndpointIPs("ep-2", "", "fe80::2")
	fp, _ = p.takeEndpoint("ep-2")
	if fp.IPv4 != "10.0.0.1" || fp.IPv6 != "fe80::2" {
		t.Errorf("v6-only update lost v4: %+v", fp)
	}
}

func TestStateFilePath_RejectsPathInjection(t *testing.T) {
	withStateDir(t, t.TempDir())

	for _, bad := range []string{
		"", "..", "../../etc/passwd", "a/b", `a\b`, "foo.bar",
		"net id", "with/slash", "./rel",
	} {
		if _, err := stateFilePath(bad); err == nil {
			t.Errorf("stateFilePath(%q) = nil error, want rejection", bad)
		}
	}

	for _, ok := range []string{"net123", "never-saved", "net_many", "abcDEF0123"} {
		p, err := stateFilePath(ok)
		if err != nil {
			t.Errorf("stateFilePath(%q) unexpectedly rejected: %v", ok, err)
			continue
		}
		if filepath.Dir(p) != filepath.Clean(stateDir) {
			t.Errorf("stateFilePath(%q) = %q, not directly under stateDir %q", ok, p, stateDir)
		}
	}
}

func TestOptionsOps_RejectInvalidNetworkID(t *testing.T) {
	withStateDir(t, t.TempDir())

	bad := "../../escape"
	if err := saveOptions(bad, DHCPNetworkOptions{Bridge: "br0"}); err == nil {
		t.Error("saveOptions accepted a traversal network id")
	}
	if _, err := loadOptions(bad); err == nil {
		t.Error("loadOptions accepted a traversal network id")
	}
	if err := deleteOptions(bad); err == nil {
		t.Error("deleteOptions accepted a traversal network id")
	}
	entries, _ := os.ReadDir(stateDir)
	if len(entries) != 0 {
		t.Errorf("expected empty stateDir after rejected ops, found %d entries", len(entries))
	}
}
