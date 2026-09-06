// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"errors"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestIPv6DisablePath(t *testing.T) {
	// The path is per-interface and per-netns; the interface name is
	// the only variable and it belongs in the middle component, not
	// appended to the file.
	if got, want := ipv6DisablePath(ipv6DisableSysctlDir, "eth0"), "/proc/sys/net/ipv6/conf/eth0/disable_ipv6"; got != want {
		t.Errorf("ipv6DisablePath(eth0) = %q, want %q", got, want)
	}
	if got, want := ipv6DisablePath(ipv6DisableSysctlDir, "dh-abc123"), "/proc/sys/net/ipv6/conf/dh-abc123/disable_ipv6"; got != want {
		t.Errorf("ipv6DisablePath(dh-abc123) = %q, want %q", got, want)
	}
}

func TestClearDisableIPv6(t *testing.T) {
	// The two outcomes are the point: an endpoint that DID get an
	// IPv6 address arrives here already enabled, and writing anyway
	// would make every endpoint look like the #868 case in the log.
	tests := []struct {
		name        string
		content     string
		wantChanged bool
		wantFile    string
	}{
		{name: "disabled by the engine", content: "1\n", wantChanged: true, wantFile: "0\n"},
		{name: "already enabled", content: "0\n", wantChanged: false, wantFile: "0\n"},
		{name: "already enabled, no trailing newline", content: "0", wantChanged: false, wantFile: "0"},
	}
	// Non-vacuity, keyed on the outcome: "the two outcomes are the
	// point" is what the comment above claims, and a table reduced to
	// one of them -- or to none -- passes silently against a
	// clearDisableIPv6 that always writes or never does.
	var changed, unchanged int
	for _, tt := range tests {
		if tt.wantChanged {
			changed++
		} else {
			unchanged++
		}
	}
	if changed < 1 || unchanged < 1 {
		t.Fatalf("the table has %d rows expecting a write and %d expecting none; both "+
			"are needed, since writing unconditionally makes every endpoint look like "+
			"the #868 case in the log", changed, unchanged)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disable_ipv6")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			changed, err := clearDisableIPv6(path)
			if err != nil {
				t.Fatalf("clearDisableIPv6: %v", err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if string(got) != tt.wantFile {
				t.Errorf("file = %q, want %q", got, tt.wantFile)
			}
		})
	}
}

func TestClearDisableIPv6_MissingSysctlIsAnError(t *testing.T) {
	// Not a silent success: an absent sysctl means the interface is
	// not the one we think it is, or /proc/sys is not the namespace's
	// own -- either way the DHCPv6 client that follows cannot work,
	// and the caller counts it.
	_, err := clearDisableIPv6(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("clearDisableIPv6 on a missing path returned nil error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error does not name the path it failed on: %v", err)
	}
}

func TestPrepareIPv6Link_RefusesBeforeTouchingAThread(t *testing.T) {
	// Both refusals happen before any thread is locked or any
	// namespace entered. The nil-link one is the important half: it is
	// not a defensive nicety but the difference between an error and a
	// panic, because Attrs() on a nil Link dereferences nil -- and it
	// has to be checked BEFORE the namespace switch, or the panic
	// unwinds a goroutine that is locked to a thread sitting in the
	// container's network namespace.
	tests := []struct {
		name string
		m    *dhcpManager
		want string
	}{
		{name: "link not located", m: &dhcpManager{}, want: "container link not located"},
		{
			name: "namespace handle closed",
			m:    &dhcpManager{ctrLink: &netlink.Device{}, nsHandle: netns.None()},
			want: "namespace handle is closed",
		},
	}
	// Non-vacuity. Both refusals are named in the comment above, and the
	// nil-link one is the difference between an error and a panic that
	// unwinds a goroutine locked to a thread sitting in the container's
	// network namespace. Dropping it leaves this test green over the
	// remaining case.
	if len(tests) != 2 {
		t.Fatalf("the refusal table has %d rows, want both preconditions — each has to "+
			"be refused BEFORE any thread is locked or any namespace entered",
			len(tests))
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, guard, err := tt.m.prepareIPv6Link()
			if err == nil {
				t.Fatalf("prepareIPv6Link returned nil error (changed=%v)", changed)
			}
			// The guard is not attempted on a link that failed its
			// preconditions: a step count above zero here would mean
			// sysctls were written on a link the function has just
			// said it cannot address.
			if guard.Failures != 0 {
				t.Errorf("prepareIPv6Link reported %d guard steps after refusing the link; "+
					"the guard must not run at all on a link it cannot name", guard.Failures)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name the precondition it failed (%q)", err, tt.want)
			}
		})
	}
}

func TestEnsureIPv6Enabled_CountsTheFailure(t *testing.T) {
	// The counter is the whole reason this failure is distinguishable
	// from a quiet segment: both otherwise present only as DHCPv6
	// timeouts.
	p := &Plugin{}
	m := (&dhcpManager{}).withPlugin(p)
	m.ensureIPv6Enabled()
	if got := p.ipv6LinkEnableFailures.Load(); got != 1 {
		t.Errorf("ipv6_link_enable_failures = %d after a failed enable, want 1", got)
	}
}

func TestEnsureIPv6Enabled_SurvivesANilPlugin(t *testing.T) {
	// dhcpManager.plugin is nil in unit tests that do not stand up a
	// Plugin; a refusal is still a refusal with no counter to bump.
	m := &dhcpManager{}
	m.ensureIPv6Enabled()
}

// startV6BranchWindow bounds "the same branch" for the gate below: the
// two calls sit within a couple of dozen lines of each other today,
// and a limit keeps the ordering claim from being satisfied by two
// calls in unrelated parts of the file.
const startV6BranchWindow = 40

// TestStart_EnablesIPv6BeforeTheV6Client pins the ORDER, not the
// presence.
//
// Both calls could be present and the fix still be dead: on a link the
// engine disabled, no link-local ever appears, so the client's own wait
// for one spends its whole budget and refuses, and enabling IPv6
// afterwards arrives with the DHCPv6 client already given up on a link
// that had nothing on it. That is precisely the shape the stateless run
// under #868 produced -- "No usable link-local address", then a -6
// client that never emitted a router solicitation -- so the ordering is
// the defect, and presence alone would not have caught it.
//
// Source-reading rather than behavioural because reaching this code
// needs a live container, a sandbox namespace and root; the alternative
// to a gate here is no observer at all. STATED BOUND: it reads the
// spelling of two calls, so a rename or a wrapper is invisible to it.
func TestStart_EnablesIPv6BeforeTheV6Client(t *testing.T) {
	const (
		enable  = "m.ensureIPv6Enabled()"
		client  = "m.setupClient(true)"
		srcFile = "dhcp_manager.go"
	)

	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read %v: %v", srcFile, err)
	}
	lines := strings.Split(string(src), "\n")

	at := func(needle string) []int {
		var hits []int
		for i, l := range lines {
			if strings.Contains(l, needle) {
				hits = append(hits, i+1)
			}
		}
		return hits
	}

	for _, needle := range []string{enable, client} {
		if got := at(needle); len(got) != 1 {
			t.Fatalf("%v: found %q on lines %v, want exactly one -- "+
				"this gate reads the source and cannot arbitrate between copies", srcFile, needle, got)
		}
	}

	// A LINE ORDER IS NOT AN EXECUTION ORDER. `defer m.ensureIPv6Enabled()`
	// and `go m.ensureIPv6Enabled()` leave the call exactly where it is
	// and move when it runs -- the first to after the client has already
	// failed, the second to whenever. Both walked through the version of
	// this gate that only compared line numbers (MEASURED: the mutant
	// survived). So the enable must be a plain statement on its own line.
	// STATED BOUND: a call moved inside a helper that defers it is still
	// invisible here.
	if got := strings.TrimSpace(lines[at(enable)[0]-1]); got != enable {
		t.Errorf("%v line %d is %q, want exactly %q -- a deferred or spawned enable runs "+
			"after or beside the client rather than before it, and the line order below "+
			"cannot tell the difference", srcFile, at(enable)[0], got, enable)
	}

	// setupClient(true) is the unique marker for the IPv6 branch of
	// Start -- there is exactly one persistent DHCPv6 client -- so
	// requiring the enable to sit above it, and close by, says "inside
	// that branch" without depending on how the branch itself is
	// spelled.
	enableLine, clientLine := at(enable)[0], at(client)[0]
	if enableLine >= clientLine {
		t.Errorf("%v: %q is on line %d and %q on %d -- IPv6 must be enabled BEFORE the "+
			"DHCPv6 client starts, or the client waits out its link-local budget on a link "+
			"that cannot have one (#868)",
			srcFile, enable, enableLine, client, clientLine)
	}
	if clientLine-enableLine > startV6BranchWindow {
		t.Errorf("%v: %q (line %d) and %q (line %d) are %d lines apart, more than the %d this gate "+
			"allows -- they are meant to be the same branch of Start, and a gate that tolerates any "+
			"distance stops saying so",
			srcFile, enable, enableLine, client, clientLine, clientLine-enableLine, startV6BranchWindow)
	}
}

// linkLocalWaitMarkers are the three things a link-local wait in THIS
// package has to read, whatever it is called.
//
// Keyed on the MECHANISM and not on a function name (#911 review round
// 1, finding 5). A wait for a usable IPv6 link-local address over
// netlink has to select on link scope and reject the two duplicate-
// address-detection flags; a wait that does less than that is not
// waiting for a usable address, and one that does it under another name
// still names these three.
var linkLocalWaitMarkers = []string{
	"RT_SCOPE_LINK",
	"IFA_F_TENTATIVE",
	"IFA_F_DADFAILED",
}

// TestTheChassisDoesNotWaitForALinkLocalItself is the observer for "the
// v6 Join path waits for the link-local ONCE".
//
// One fact, one derivation. runtime.InterfaceLinkLocal resolves the
// interface on the calling thread, refuses a tentative or dad-failed
// address, and waits its own derived bound (RFC 4862 section 5.4.2's
// delay plus one probe, plus a stated margin) for a usable one. The
// chassis had a SECOND wait in front of it, on a ten-second budget
// derived from nothing, so a link whose link-local never cleared spent
// fourteen seconds of a thirty-second Join deadline arriving at the
// refusal the library reaches in four -- and newLibClient6's own doc
// comment asserted the wait was not there.
//
// WHY A SOURCE SCAN. The Join path needs root, a sandbox namespace and
// a live container; nothing in the unit lane can execute it. The
// property is an ABSENCE, and an absence is what a scan can actually
// establish over a whole package where a behavioural test can only
// speak for the path it drives.
//
// STATED BOUNDS. It reads production sources under pkg/plugin only:
// a wait added in another package of the chassis, or one written
// against /proc/net/if_inet6 or netip's IsLinkLocalUnicast instead of
// netlink, is invisible to it. It cannot see a wait inside the library
// either, which is the point -- that one is the derivation being kept.
func TestTheChassisDoesNotWaitForALinkLocalItself(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %v: %v", name, err)
		}
		scanned++
		for _, marker := range linkLocalWaitMarkers {
			if !strings.Contains(string(src), marker) {
				continue
			}
			t.Errorf("%v names %s. That is the mechanism of a link-local wait, and the "+
				"chassis must not have one: runtime.InterfaceLinkLocal already waits for a "+
				"non-tentative link-local on the interface the client binds, inside its own "+
				"derived bound. A second wait here is a second derivation of one fact and it "+
				"stacks on top of the library's, on a Join deadline neither of them knows "+
				"about (#911)", name, marker)
		}
	}

	// NON-VACUITY. A scan that read nothing reports the same clean
	// result as a package with no wait in it.
	if scanned < 2 {
		t.Fatalf("scanned %d production sources in this package; the check above measured "+
			"nothing", scanned)
	}
	if len(linkLocalWaitMarkers) == 0 {
		t.Fatal("the marker list is empty, so the loop above asserted nothing")
	}
}

// TestProcSysPrep_NeverStopsTheWrite pins the direction the /proc/sys
// preparation is allowed to fail in.
//
// The remount is best effort: on a runtime where /proc/sys is not a
// separate mount it fails and /proc/sys is already writable, so the
// disable_ipv6 write would have succeeded. Treating the preparation's
// error as the verdict skipped that write and left the container with
// no IPv6 for a mount the host did not need — a guard failing in the
// direction that breaks a working host. pkg/dhcp/client.go carries the
// measurement and makes the same call non-fatal for dhcpcd's argv.
//
// This asserts the disposition rather than the log line, because a
// comment saying "we keep going" is prose and prose satisfies nothing.
func TestProcSysPrep_NeverStopsTheWrite(t *testing.T) {
	for _, err := range []error{
		errors.New("unshare mount namespace: operation not permitted"),
		errors.New("make mount propagation private: invalid argument"),
		errors.New("remount /proc/sys read-write: no such file or directory"),
		unix.EROFS,
		unix.EPERM,
		nil,
	} {
		if procSysPrepIsFatal(err) {
			t.Errorf("a %v from the /proc/sys preparation was treated as fatal. Nothing in "+
				"that step may decide the outcome: clearDisableIPv6 reads and writes the "+
				"real path, and its own error is the report. Stopping here is how a host "+
				"on which the write would have worked ends up with no IPv6", err)
		}
	}
}

// TestClearDisableIPv6_IsTheObserver is the other half: with the
// preparation contributing no verdict, the write must still fail loudly
// when the tree really is unwritable. Otherwise the change above would
// have traded a false negative for a silent one.
func TestClearDisableIPv6_IsTheObserver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disable_ipv6")
	if err := os.WriteFile(path, []byte("1\n"), 0o444); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Log("running as root: a read-only mode bit does not refuse root, so the " +
			"unwritable case is exercised by the missing-path arm below only")
	} else if _, err := clearDisableIPv6(path); err == nil {
		t.Errorf("clearDisableIPv6 reported success writing an unwritable sysctl. It is " +
			"the only observer left after the preparation stopped producing verdicts")
	}
	if _, err := clearDisableIPv6(filepath.Join(dir, "absent", "disable_ipv6")); err == nil {
		t.Errorf("clearDisableIPv6 reported success on a path that does not exist")
	}
}

// v6LinkSysctlDir builds a stand-in for /proc/sys/net/ipv6/conf with
// one interface directory holding disable_ipv6 and the guard's three
// knobs, all at values a real sandbox link starts from: IPv6 off, and
// the guard's knobs at the kernel defaults the guard has to move.
//
// Starting them at the defaults rather than at the guard's own values
// is what makes the second assertion below discriminating: knobs that
// already read the right value would be indistinguishable from knobs
// the guard wrote.
func v6LinkSysctlDir(t *testing.T, iface string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, iface), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, iface, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	write("disable_ipv6", "1")
	for knob := range dhcp.RouterAdvertGuardContract() {
		write(knob, "0")
	}
	return dir
}

func v6LinkKnob(t *testing.T, dir, iface, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, iface, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// TestPrepareV6LinkUnder_GuardRunsOnlyAfterIPv6IsOn drives the ORDER,
// which is the whole claim of prepareIPv6Link and was the part no test
// could reach: the namespace entry around it needs root and a sandbox,
// so a mutant that applied the guard on a link whose IPv6 could not be
// turned on survived the entire unit lane, and so did one that never
// applied the guard at all.
//
// The two directions are the test. On a link that can be enabled the
// guard's knobs must end up at the contract's values — otherwise the
// container has a DHCPv6 address and no route, because DHCPv6 carries
// no next hop. On a link that cannot, the guard must not have run:
// its knobs write and read back perfectly well on a link with IPv6
// administratively off, so a guard applied there reports success for
// an endpoint on which no advertisement can be processed at all, and
// router_advert_guard_failures reads zero for the one endpoint that
// most needs it to read something.
func TestPrepareV6LinkUnder_GuardRunsOnlyAfterIPv6IsOn(t *testing.T) {
	const iface = "eth0"
	contract := dhcp.RouterAdvertGuardContract()
	if len(contract) == 0 {
		t.Fatal("the guard contract is empty, so this test observes nothing")
	}

	t.Run("IPv6 can be enabled: the guard runs after it", func(t *testing.T) {
		dir := v6LinkSysctlDir(t, iface)

		changed, res, err := prepareV6LinkUnder(dir, iface)
		if err != nil {
			t.Fatalf("prepareV6LinkUnder: %v", err)
		}
		if !changed {
			t.Error("disable_ipv6 read 1 and the call reports it wrote nothing")
		}
		if got := v6LinkKnob(t, dir, iface, "disable_ipv6"); got != "0" {
			t.Errorf("disable_ipv6 reads %q after the call, want 0", got)
		}
		if res.Failures != 0 || res.Err != nil {
			t.Errorf("the guard reported %d failure(s) on a writable directory: %v",
				res.Failures, res.Err)
		}
		for knob, want := range contract {
			if got := v6LinkKnob(t, dir, iface, knob); got != want {
				t.Errorf("%s reads %q after the call, want %q — the guard did not run, "+
					"and a container on this link gets a DHCPv6 address with no default "+
					"route to use it with", knob, got, want)
			}
		}
	})

	t.Run("IPv6 cannot be enabled: the guard does not run", func(t *testing.T) {
		dir := v6LinkSysctlDir(t, iface)
		// A directory where disable_ipv6 should be: the read fails, and
		// it fails the way a sysctl that is not there or not readable
		// does, without needing a read-only mount or a non-root user.
		p := filepath.Join(dir, iface, "disable_ipv6")
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatalf("mkdir over the sysctl: %v", err)
		}

		changed, res, err := prepareV6LinkUnder(dir, iface)
		if err == nil {
			t.Fatal("prepareV6LinkUnder succeeded with no readable disable_ipv6")
		}
		if changed {
			t.Error("the call reports it enabled IPv6 on a link where it could not")
		}
		if res.Failures != 0 || res.Err != nil {
			t.Errorf("the guard produced a result (%d failure(s), %v) on a link whose "+
				"IPv6 could not be turned on", res.Failures, res.Err)
		}
		for knob := range contract {
			if got := v6LinkKnob(t, dir, iface, knob); got != "0" {
				t.Errorf("%s reads %q — the guard wrote its knobs on a link with IPv6 "+
					"administratively off. They write and read back truthfully there, so "+
					"router_advert_guard_failures reports zero for an endpoint that can "+
					"process no advertisement at all: one failure, two counters, and the "+
					"one an operator would look at reads clean", knob, got)
			}
		}
	})
}
