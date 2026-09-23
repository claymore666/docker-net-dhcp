// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"errors"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestIPv6DisablePath(t *testing.T) {
	if got, want := ipv6DisablePath(ipv6DisableSysctlDir, "eth0"), "/proc/sys/net/ipv6/conf/eth0/disable_ipv6"; got != want {
		t.Errorf("ipv6DisablePath(eth0) = %q, want %q", got, want)
	}
	if got, want := ipv6DisablePath(ipv6DisableSysctlDir, "dh-abc123"), "/proc/sys/net/ipv6/conf/dh-abc123/disable_ipv6"; got != want {
		t.Errorf("ipv6DisablePath(dh-abc123) = %q, want %q", got, want)
	}
}

func TestClearDisableIPv6(t *testing.T) {
	// A link that already has IPv6 is read and not written, or every endpoint would log as the #868 case.
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
	// An absent sysctl is an error the caller counts, not a silent success (#868).
	_, err := clearDisableIPv6(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("clearDisableIPv6 on a missing path returned nil error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error does not name the path it failed on: %v", err)
	}
}

func TestPrepareIPv6Link_RefusesBeforeTouchingAThread(t *testing.T) {
	// Attrs() on a nil Link panics, so both refusals come before the namespace switch.
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
	p := &Plugin{}
	m := (&dhcpManager{}).withPlugin(p)
	m.ensureIPv6Enabled()
	if got := p.ipv6LinkEnableFailures.Load(); got != 1 {
		t.Errorf("ipv6_link_enable_failures = %d after a failed enable, want 1", got)
	}
}

func TestEnsureIPv6Enabled_SurvivesANilPlugin(t *testing.T) {
	m := &dhcpManager{}
	m.ensureIPv6Enabled()
}

// startV6BranchWindow keeps the order claim inside Start's v6 branch.
const startV6BranchWindow = 40

// A source gate, since Start needs root and a sandbox; a rename or a wrapper of either call is invisible (#868).
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

	// A deferred or go-launched enable keeps its line but runs late, so the enable must be a plain statement (#868).
	if got := strings.TrimSpace(lines[at(enable)[0]-1]); got != enable {
		t.Errorf("%v line %d is %q, want exactly %q -- a deferred or spawned enable runs "+
			"after or beside the client rather than before it, and the line order below "+
			"cannot tell the difference", srcFile, at(enable)[0], got, enable)
	}

	// setupClient(true) is the only persistent DHCPv6 client, so it marks the v6 branch.
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

// linkLocalWaitMarkers are what any netlink link-local wait reads: link scope and the two DAD flags (#911).
var linkLocalWaitMarkers = []string{
	"RT_SCOPE_LINK",
	"IFA_F_TENTATIVE",
	"IFA_F_DADFAILED",
}

// A source scan of pkg/plugin, since Join needs root; a wait in another package or via /proc/net is invisible (#911).
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

	// Non-vacuity: a scan that read nothing is as clean as a package with no wait.
	if scanned < 2 {
		t.Fatalf("scanned %d production sources in this package; the check above measured "+
			"nothing", scanned)
	}
	if len(linkLocalWaitMarkers) == 0 {
		t.Fatal("the marker list is empty, so the loop above asserted nothing")
	}
}

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

// notTheContractValue derives each seed from the guard contract, so a seed never equals what the guard writes (#821).
func notTheContractValue(want string) string {
	if want == "0" {
		return "1"
	}
	return "0"
}

// v6LinkSysctlDir stands in for /proc/sys/net/ipv6/conf: one link with IPv6 off and every guard knob off contract.
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
	for knob, want := range dhcp.RouterAdvertGuardContract() {
		seed := notTheContractValue(want)
		if seed == want {
			t.Fatalf("the seed for %s equals the value the guard writes (%q), so every "+
				"assertion keyed on it is vacuous", knob, want)
		}
		write(knob, seed)
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

func TestPrepareV6LinkUnder_GuardRunsOnlyAfterIPv6IsOn(t *testing.T) {
	const iface = "eth0"
	contract := dhcp.RouterAdvertGuardContract()
	if len(contract) == 0 {
		t.Fatal("the guard contract is empty, so this test observes nothing")
	}

	t.Run("IPv6 can be enabled: the guard runs after it", func(t *testing.T) {
		dir := v6LinkSysctlDir(t, iface)

		changed, res, err := prepareV6LinkUnder(dir, iface, 0)
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
		// A directory at disable_ipv6 makes the read fail as a missing or unreadable sysctl does.
		p := filepath.Join(dir, iface, "disable_ipv6")
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatalf("mkdir over the sysctl: %v", err)
		}

		changed, res, err := prepareV6LinkUnder(dir, iface, 0)
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
		for knob, want := range contract {
			if got := v6LinkKnob(t, dir, iface, knob); got != notTheContractValue(want) {
				t.Errorf("%s reads %q — the guard wrote its knobs on a link with IPv6 "+
					"administratively off. They write and read back truthfully there, so "+
					"router_advert_guard_failures reports zero for an endpoint that can "+
					"process no advertisement at all: one failure, two counters, and the "+
					"one an operator would look at reads clean", knob, got)
			}
		}
	})
}
