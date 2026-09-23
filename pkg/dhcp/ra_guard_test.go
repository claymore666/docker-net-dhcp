// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysctlTree lays out a per-interface IPv6 conf directory with the three knobs at their kernel defaults.
func fakeSysctlTree(t *testing.T, iface string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, iface), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Kernel defaults on a fresh link: accept_ra 1, autoconf 1, keep_addr_on_down 0 (#821).
	seed := map[string]string{
		"accept_ra":         "1",
		"autoconf":          "1",
		"keep_addr_on_down": "0",
	}
	// Every seed differs from the contract, or the read-back would pass with no write (#911).
	contract := RouterAdvertGuardContract()
	if len(seed) != len(contract) {
		t.Fatalf("the fixture seeds %d knobs and the contract has %d: %v vs %v",
			len(seed), len(contract), seed, contract)
	}
	for knob, want := range contract {
		def, ok := seed[knob]
		if !ok {
			t.Fatalf("the contract sets %v and the fixture never seeds it, so "+
				"a guard that skipped %v would still read back correctly", knob, knob)
		}
		if def == want {
			t.Fatalf("%v is seeded %q and the guard writes %q: the read-back "+
				"cannot tell a write from the seed", knob, def, want)
		}
	}
	for knob, def := range seed {
		if err := os.WriteFile(filepath.Join(dir, iface, knob), []byte(def+"\n"), 0o644); err != nil {
			t.Fatalf("seed %v: %v", knob, err)
		}
	}
	return dir
}

func TestApplyRouterAdvertGuard_WritesTheContract(t *testing.T) {
	const iface = "eth0"
	dir := fakeSysctlTree(t, iface)

	res := ApplyRouterAdvertGuard(dir, iface)
	if res.Failures != 0 || res.Err != nil {
		t.Fatalf("guard reported %d failures on a writable tree: %v", res.Failures, res.Err)
	}

	contract := RouterAdvertGuardContract()
	if len(contract) == 0 {
		t.Fatal("the contract is empty: every assertion below is vacuous")
	}
	for knob, want := range contract {
		got, err := os.ReadFile(filepath.Join(dir, iface, knob))
		if err != nil {
			t.Errorf("%v: %v", knob, err)
			continue
		}
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("%v reads %q, want %q", knob, strings.TrimSpace(string(got)), want)
		}
	}

	// accept_ra 0: the plugin's own client reads the advertisement, so a kernel acting on it would add a second default
	// route (#821).
	want := map[string]string{"accept_ra": "0", "autoconf": "0", "keep_addr_on_down": "1"}
	if len(contract) != len(want) {
		t.Errorf("the contract has %d knobs, want %d: %v", len(contract), len(want), contract)
	}
	for knob, v := range want {
		if contract[knob] != v {
			t.Errorf("the contract sets %v=%q, want %q", knob, contract[knob], v)
		}
	}
}

func TestRouterAdvertGuardContract_IsNotShared(t *testing.T) {
	a := RouterAdvertGuardContract()
	for k := range a {
		delete(a, k)
	}
	if len(RouterAdvertGuardContract()) == 0 {
		t.Error("emptying one caller's contract emptied the next one's")
	}
}

// A missing knob is counted, not fatal: a kernel without it or a read-only /proc/sys is no reason to refuse an address
// (#911).

func TestApplyRouterAdvertGuard_CountsRatherThanRefuses(t *testing.T) {
	res := ApplyRouterAdvertGuard(t.TempDir(), "eth0")
	if res.Failures == 0 {
		t.Fatal("the guard reported no failures against a tree with no knobs in it")
	}
	if res.Err == nil {
		t.Error("the guard reported failures with no error to log")
	}
	if max := 2 * len(RouterAdvertGuardContract()); res.Failures > max {
		t.Errorf("Failures = %d, above the %d-step bound", res.Failures, max)
	}
	for knob := range RouterAdvertGuardContract() {
		if !strings.Contains(res.Err.Error(), knob) {
			t.Errorf("the error does not name %v: %v", knob, res.Err)
		}
	}
}

func TestApplyRouterAdvertGuard_ReadsBackWhatItWrote(t *testing.T) {
	const iface = "eth0"
	dir := fakeSysctlTree(t, iface)
	p := filepath.Join(dir, iface, "accept_ra")
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(os.DevNull, p); err != nil {
		t.Fatalf("cannot symlink %v: %v", os.DevNull, err)
	}

	res := ApplyRouterAdvertGuard(dir, iface)
	if res.Failures != 1 {
		t.Fatalf("Failures = %d, want 1: the write took and the read-back did not, "+
			"which is one step of one knob (%v)", res.Failures, res.Err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "accept_ra") {
		t.Errorf("the error does not name accept_ra: %v", res.Err)
	}
	for _, knob := range []string{"autoconf", "keep_addr_on_down"} {
		got, err := os.ReadFile(filepath.Join(dir, iface, knob))
		if err != nil {
			t.Errorf("%v: %v", knob, err)
			continue
		}
		if strings.TrimSpace(string(got)) != RouterAdvertGuardContract()[knob] {
			t.Errorf("%v was left at %q after an earlier knob failed",
				knob, strings.TrimSpace(string(got)))
		}
	}
}

func TestRAGuardPath_IsScopedToTheInterface(t *testing.T) {
	got := raGuardPath("/proc/sys/net/ipv6/conf", "eth0", "accept_ra")
	if want := "/proc/sys/net/ipv6/conf/eth0/accept_ra"; got != want {
		t.Errorf("raGuardPath = %q, want %q", got, want)
	}
	if raGuardPath("/d", "eth0", "accept_ra") == raGuardPath("/d", "eth1", "accept_ra") {
		t.Error("two interfaces share one path")
	}
}

func TestSysctlIPv6ConfDir(t *testing.T) {
	if want := "/proc/sys/net/ipv6/conf"; sysctlIPv6ConfDir != want {
		t.Errorf("sysctlIPv6ConfDir = %q, want %q", sysctlIPv6ConfDir, want)
	}
}
