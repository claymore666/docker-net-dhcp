// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysctlTree lays out a /proc/sys/net/ipv6/conf/<iface>/ with the
// three knobs present and at their kernel defaults, so a test measures
// what the guard WRITES rather than what happened to be there.
func fakeSysctlTree(t *testing.T, iface string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, iface), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The kernel's defaults on a fresh link, and all three are wrong for
	// this plugin: accept_ra 1 stops accepting once forwarding is on,
	// autoconf 1 is right but is written anyway because accept_ra alone
	// does not imply it, keep_addr_on_down 0 drops the address on a
	// carrier flap.
	for knob, def := range map[string]string{
		"accept_ra":         "1",
		"autoconf":          "1",
		"keep_addr_on_down": "0",
	} {
		if err := os.WriteFile(filepath.Join(dir, iface, knob), []byte(def+"\n"), 0o644); err != nil {
			t.Fatalf("seed %v: %v", knob, err)
		}
	}
	return dir
}

// The guard writes exactly the contract, and the contract is what the
// rest of the plugin reads.
//
// TWO ASSERTIONS FROM ONE RUN, deliberately. RouterAdvertGuardContract
// is what the docs and the operator-facing diagnostics quote; the writes
// are what the container's kernel sees. Asserting the writes against a
// hand-written list would let the two drift; asserting them against the
// contract makes the contract the single statement, and the hand-written
// values below pin the contract itself so "both changed together" is
// still a failure.
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

	// The contract itself, spelled out. accept_ra 2 and not 1: the
	// engine turns on forwarding on the container's link for bridge
	// mode, and net.ipv6.conf.<if>.accept_ra=1 means "accept only while
	// forwarding is off". 2 is "accept even as a router", which is the
	// only value that survives what the engine does to the link.
	want := map[string]string{"accept_ra": "2", "autoconf": "1", "keep_addr_on_down": "1"}
	if len(contract) != len(want) {
		t.Errorf("the contract has %d knobs, want %d: %v", len(contract), len(want), contract)
	}
	for knob, v := range want {
		if contract[knob] != v {
			t.Errorf("the contract sets %v=%q, want %q", knob, contract[knob], v)
		}
	}
}

// The contract is a fresh map per call. It is handed to callers that log
// it and to tests that range over it; a shared map is a caller able to
// rewrite what the next one reads.
func TestRouterAdvertGuardContract_IsNotShared(t *testing.T) {
	a := RouterAdvertGuardContract()
	for k := range a {
		delete(a, k)
	}
	if len(RouterAdvertGuardContract()) == 0 {
		t.Error("emptying one caller's contract emptied the next one's")
	}
}

// A tree with no knobs at all is counted, not fatal, and the count is
// bounded by the number of steps.
//
// WHY NOT FATAL. This runs inside the container's namespace on a link
// the engine has just created. A missing knob means the kernel was built
// without something, or /proc/sys is mounted read-only in a rootfs the
// plugin does not control -- neither is a reason to refuse to give the
// container an address. It is a reason for the operator to know the
// endpoint may have an address and no route, which is what the counter
// is for.
func TestApplyRouterAdvertGuard_CountsRatherThanRefuses(t *testing.T) {
	res := ApplyRouterAdvertGuard(t.TempDir(), "eth0")
	if res.Failures == 0 {
		t.Fatal("the guard reported no failures against a tree with no knobs in it")
	}
	if res.Err == nil {
		t.Error("the guard reported failures with no error to log")
	}
	// Two steps per knob -- the write and the read-back -- so the count
	// is bounded, and the bound is what makes the counter comparable
	// between endpoints.
	if max := 2 * len(RouterAdvertGuardContract()); res.Failures > max {
		t.Errorf("Failures = %d, above the %d-step bound", res.Failures, max)
	}
	// Every missing knob is named, so the operator can tell "the kernel
	// has no keep_addr_on_down" from "/proc/sys is read-only".
	for knob := range RouterAdvertGuardContract() {
		if !strings.Contains(res.Err.Error(), knob) {
			t.Errorf("the error does not name %v: %v", knob, res.Err)
		}
	}
}

// A knob that accepts a write and reads back something else is a
// failure. This is the shape a write-only assertion cannot see, and it
// is the shape that actually happens: a sysctl the kernel clamps, or a
// value another agent in the namespace immediately overwrites.
func TestApplyRouterAdvertGuard_ReadsBackWhatItWrote(t *testing.T) {
	const iface = "eth0"
	dir := fakeSysctlTree(t, iface)
	// /dev/null takes any write and reads back empty, which is exactly
	// "the write succeeded and the value did not change".
	p := filepath.Join(dir, iface, "accept_ra")
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(os.DevNull, p); err != nil {
		t.Skipf("cannot symlink %v: %v", os.DevNull, err)
	}

	res := ApplyRouterAdvertGuard(dir, iface)
	if res.Failures != 1 {
		t.Fatalf("Failures = %d, want 1: the write took and the read-back did not, "+
			"which is one step of one knob (%v)", res.Failures, res.Err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "accept_ra") {
		t.Errorf("the error does not name accept_ra: %v", res.Err)
	}
	// The other two still took: one bad knob does not abandon the rest.
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

// The path is per-interface. Every container endpoint in this process
// shares /proc/sys/net/ipv6/conf, and `all` and `default` are siblings
// of the interface directory -- a path built without the interface would
// silently reconfigure the host's defaults from inside a namespace that
// may or may not have its own /proc.
func TestRAGuardPath_IsScopedToTheInterface(t *testing.T) {
	got := raGuardPath("/proc/sys/net/ipv6/conf", "eth0", "accept_ra")
	if want := "/proc/sys/net/ipv6/conf/eth0/accept_ra"; got != want {
		t.Errorf("raGuardPath = %q, want %q", got, want)
	}
	if raGuardPath("/d", "eth0", "accept_ra") == raGuardPath("/d", "eth1", "accept_ra") {
		t.Error("two interfaces share one path")
	}
}

// The production root is the real one. Nothing else in the package names
// it, so a typo here is a guard that silently writes into a directory
// that does not exist and counts six failures on every endpoint.
func TestSysctlIPv6ConfDir(t *testing.T) {
	if want := "/proc/sys/net/ipv6/conf"; sysctlIPv6ConfDir != want {
		t.Errorf("sysctlIPv6ConfDir = %q, want %q", sysctlIPv6ConfDir, want)
	}
}
