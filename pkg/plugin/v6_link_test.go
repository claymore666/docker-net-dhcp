// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestIPv6DisablePath(t *testing.T) {
	// The path is per-interface and per-netns; the interface name is
	// the only variable and it belongs in the middle component, not
	// appended to the file.
	if got, want := ipv6DisablePath("eth0"), "/proc/sys/net/ipv6/conf/eth0/disable_ipv6"; got != want {
		t.Errorf("ipv6DisablePath(eth0) = %q, want %q", got, want)
	}
	if got, want := ipv6DisablePath("dh-abc123"), "/proc/sys/net/ipv6/conf/dh-abc123/disable_ipv6"; got != want {
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

func TestEnableIPv6OnContainerLink_RefusesBeforeTouchingAThread(t *testing.T) {
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
			changed, err := tt.m.enableIPv6OnContainerLink()
			if err == nil {
				t.Fatalf("enableIPv6OnContainerLink returned nil error (changed=%v)", changed)
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
// three calls sit within a couple of dozen lines of each other today,
// and a limit keeps the ordering claim from being satisfied by two
// calls in unrelated parts of the file.
const startV6BranchWindow = 40

// TestStart_EnablesIPv6BeforeWaitingForTheLinkLocal pins the ORDER, not
// the presence.
//
// Both calls could be present and the fix still be dead: on a link the
// engine disabled, the link-local never appears, so awaitLinkLocal
// spends its whole budget and warns, and enabling IPv6 afterwards
// arrives ten seconds late with the DHCPv6 client already started
// against a link that had nothing on it. That is precisely the shape
// the stateless run under #868 produced -- "No usable link-local address;
// starting DHCPv6 client anyway", then a dhcpcd -6 that never emitted a
// router solicitation -- so the ordering is the defect, and presence
// alone would not have caught it.
//
// Source-reading rather than behavioural because reaching this code
// needs a live container, a sandbox namespace and root; the alternative
// to a gate here is no observer at all.
func TestStart_EnablesIPv6BeforeWaitingForTheLinkLocal(t *testing.T) {
	const (
		enable  = "m.ensureIPv6Enabled()"
		await   = "m.awaitLinkLocal(ctx)"
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

	for _, needle := range []string{enable, await, client} {
		if got := at(needle); len(got) != 1 {
			t.Fatalf("%v: found %q on lines %v, want exactly one -- "+
				"this gate reads the source and cannot arbitrate between copies", srcFile, needle, got)
		}
	}

	// setupClient(true) is the unique marker for the IPv6 branch of
	// Start -- there is exactly one persistent DHCPv6 client -- so
	// requiring both calls to sit above it, in order and close by,
	// says "inside that branch" without depending on how the branch
	// itself is spelled.
	enableLine, awaitLine, clientLine := at(enable)[0], at(await)[0], at(client)[0]
	if !(enableLine < awaitLine && awaitLine < clientLine) {
		t.Errorf("%v: %q is on line %d, %q on %d, %q on %d -- IPv6 must be enabled BEFORE the "+
			"link-local wait and both before the DHCPv6 client starts, or the wait burns its "+
			"budget on a link that cannot have a link-local (#868)",
			srcFile, enable, enableLine, await, awaitLine, client, clientLine)
	}
	if clientLine-enableLine > startV6BranchWindow {
		t.Errorf("%v: %q (line %d) and %q (line %d) are %d lines apart, more than the %d this gate "+
			"allows -- they are meant to be the same branch of Start, and a gate that tolerates any "+
			"distance stops saying so",
			srcFile, enable, enableLine, client, clientLine, clientLine-enableLine, startV6BranchWindow)
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
