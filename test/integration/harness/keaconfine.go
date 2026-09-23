// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Debian and Ubuntu ship an AppArmor profile with kea-dhcp4 that pins config to /etc/kea/**, its log to
// /var/log/kea/kea-dhcp4.log, leases to /var/lib/kea/kea-leases4.csv* and the lock to /run/lock/kea/logger_lockfile
// (#869). The ephemeral fixture runs Kea from a temp directory, so under an enforced profile Kea exits with an empty
// log and the cause shows only in the kernel log; fixed paths would collide between concurrent fixtures. CI runner
// hosts carry no kea package; a privileged container still transitions into a matching profile on exec.

// Vars so the test can point them at fixture files.
var (
	// apparmorProfilesPath is the kernel's root-only list of loaded profiles and their modes.
	apparmorProfilesPath = "/sys/kernel/security/apparmor/profiles"
	// keaProfilePath is the profile as shipped on disk; present does not mean loaded.
	keaProfilePath = "/etc/apparmor.d/usr.sbin.kea-dhcp4"
	// readKernelLog returns the kernel ring buffer, root-only under Debian's kernel.dmesg_restrict=1.
	readKernelLog = func() (string, error) {
		out, err := withCLocale(exec.Command("dmesg")).Output()
		return string(out), err
	}
)

// keaProfileMode returns the kernel-reported mode of the kea-dhcp4 profile, or "" when not loaded, matching the exact
// name: kea-lfc and kea-dhcp4-custom are other profiles (#869).
func keaProfileMode(profiles string) string {
	for _, line := range strings.Split(profiles, "\n") {
		line = strings.TrimSpace(line)
		name, mode, ok := strings.Cut(line, " ")
		if !ok || name != "kea-dhcp4" {
			continue
		}
		return strings.Trim(mode, "()")
	}
	return ""
}

// keaDenialRecord returns the last AppArmor DENIED record naming profile="kea-dhcp4" and a path under runDir, or "".
// The shipped profile ends in `#include <local/usr.sbin.kea-dhcp4>`, so an enforcing profile alone proves no denial.
// Complain mode logs ALLOWED, and an empty runDir matches nothing. Record format measured on a Debian host (#869):
//
//	audit: type=1400 audit(...): apparmor="DENIED" operation="open"
//	class="file" profile="kea-dhcp4" name="/tmp/keaprobe.IGXt/kea.json"
//	pid=386894 comm="kea-dhcp4" requested_mask="r" denied_mask="r" ...
func keaDenialRecord(kernelLog, runDir string) string {
	if runDir == "" {
		return ""
	}
	last := ""
	for _, line := range strings.Split(kernelLog, "\n") {
		if !strings.Contains(line, `apparmor="DENIED"`) ||
			!strings.Contains(line, `profile="kea-dhcp4"`) ||
			!strings.Contains(line, runDir) {
			continue
		}
		last = strings.TrimSpace(line)
	}
	return last
}

// keaConfinement is what the fixture measured about AppArmor when Kea failed, each read with its own outcome flag (#869).
type keaConfinement struct {
	// mode is the kernel-reported mode, "" when not loaded; meaningful only if listRead.
	mode     string
	listRead bool
	// installed is whether a profile is present on disk.
	installed bool
	// kernelLogRead is whether the kernel ring buffer was readable.
	kernelLogRead bool
	// denial is the denial record naming runDir; meaningful only if kernelLogRead.
	denial string
	// logEmpty is whether the Kea log the caller prints is empty.
	logEmpty bool
	// runDir is the fixture's temp directory.
	runDir string
}

// keaConfinementHint explains why AppArmor did or may have stopped Kea in c.runDir, or returns "" when it does not.
// A denial record states causation, enforce without one is the likely cause, a profile on disk behind an unreadable
// list is hedged; complain mode and a read list without the profile return "" (#869).
func keaConfinementHint(c keaConfinement) string {
	const remedy = "  Put the profile in complain mode for local runs, and restore it after:\n" +
		"    sudo apparmor_parser -C -r /etc/apparmor.d/usr.sbin.kea-dhcp4   # before\n" +
		"    sudo apparmor_parser -r    /etc/apparmor.d/usr.sbin.kea-dhcp4   # after\n" +
		"  Background, including why a privileged container does NOT escape this:\n" +
		"  test/integration/README.md, \"AppArmor (Debian/Ubuntu hosts only)\".\n"

	// readLog returns the whole file across restarts, or an error string, so the empty-log claim needs logEmpty.
	emptyLog := ""
	if c.logEmpty {
		emptyLog = " -- which is why the log above is empty"
	}

	const profilePaths = "  The profile permits a config only under /etc/kea/**, its log only at\n" +
		"  /var/log/kea/kea-dhcp4.log, leases only at /var/lib/kea/kea-leases4.csv* and\n" +
		"  the logger lock only at /run/lock/kea/logger_lockfile. This fixture runs from\n" +
		"  %s, so every path it needs is outside what the packaged\n" +
		"  profile permits.\n"

	switch {
	case c.mode == "enforce" && c.denial != "":
		return fmt.Sprintf(
			"APPARMOR: the kea-dhcp4 profile is loaded in enforce mode and the kernel logged a\n"+
				"  denial against this fixture's own directory, so that is why Kea never started:\n"+
				"    %s\n"+profilePaths+
				"  Kea exits before writing a line%s.\n"+remedy,
			c.denial, c.runDir, emptyLog)

	case c.mode == "enforce":
		unknown := "  No kernel denial record naming this directory was found, which does not clear\n" +
			"  AppArmor: such records are rate-limited, can be routed to auditd instead of the\n" +
			"  kernel ring buffer, and age out of it."
		if !c.kernelLogRead {
			unknown = "  The kernel ring buffer could not be read, so no denial record was consulted;\n" +
				"  this hint rests on the loaded profile alone."
		}
		return fmt.Sprintf(
			"APPARMOR: the kea-dhcp4 profile is loaded in enforce mode, which is the most likely\n"+
				"  reason Kea never started.\n"+profilePaths+unknown+"\n"+
				"  A site override at /etc/apparmor.d/local/usr.sbin.kea-dhcp4 could equally have\n"+
				"  permitted these paths, in which case the cause is elsewhere.\n"+
				"  Confirm with: sudo dmesg | grep 'apparmor=\"DENIED\".*kea-dhcp4'\n"+remedy,
			c.runDir)

	case !c.listRead && c.installed:
		return fmt.Sprintf(
			"APPARMOR: could not read %s to see whether the kea-dhcp4 profile is loaded, but\n"+
				"  a profile IS installed at %s. If it is loaded in enforce mode it confines Kea\n"+
				"  to the distro paths, and this fixture runs from %s, so every path it needs\n"+
				"  would be denied and Kea would exit before writing a line.\n"+
				"  Check with: sudo aa-status | grep kea, or sudo dmesg | grep 'apparmor=\"DENIED\"'\n"+remedy,
			apparmorProfilesPath, keaProfilePath, c.runDir)

	default:
		return ""
	}
}

// keaConfinementEvidence performs the three reads and reports what each established.
func keaConfinementEvidence(runDir string, logEmpty bool) keaConfinement {
	c := keaConfinement{runDir: runDir, logEmpty: logEmpty}

	if data, err := os.ReadFile(apparmorProfilesPath); err == nil {
		c.listRead = true
		c.mode = keaProfileMode(string(data))
	}
	_, statErr := os.Stat(keaProfilePath)
	c.installed = statErr == nil

	// Only an enforcing profile can have produced a denial record.
	if c.mode == "enforce" {
		if kernelLog, err := readKernelLog(); err == nil {
			c.kernelLogRead = true
			c.denial = keaDenialRecord(kernelLog, runDir)
		}
	}
	return c
}

// appArmorKeaHint is keaConfinementHint with the reads done; logEmpty is about the log the caller prints.
func appArmorKeaHint(runDir string, logEmpty bool) string {
	return keaConfinementHint(keaConfinementEvidence(runDir, logEmpty))
}
