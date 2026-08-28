// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Debian and Ubuntu ship an AppArmor profile with the kea-dhcp4
// package, and it pins Kea to the distro's own layout: a config read
// only from /etc/kea/**, its log at /var/log/kea/kea-dhcp4.log, leases
// at /var/lib/kea/kea-leases4.csv*, and the logger lock at
// /run/lock/kea/logger_lockfile. Single fixed paths, not globs over a
// parent directory.
//
// EphemeralFixture runs Kea entirely out of a per-test temporary
// directory, so under an enforced profile every one of those is denied
// and Kea exits before writing a line. What the fixture then reports is
// "did not become ready" with an empty log, and the actual cause is
// visible only in the kernel log. That cost a real debugging detour,
// which is why this is a check and not a paragraph in a README.
//
// The fixture cannot be made to comply. Those paths are fixed, so two
// concurrent fixtures would collide on them and a fixture using them
// would fight a system Kea. Diagnosis is the remedy, not compliance.
//
// The workaround is documented in test/integration/README.md under
// "AppArmor (Debian/Ubuntu hosts only)". What was missing is the link
// from the symptom to that section at the moment it bites, which is
// what this supplies.
//
// CI is unaffected because the runner HOST does not have the kea
// package, so no profile is loaded there -- NOT because the lane runs
// in a container. A privileged container does not escape this: its own
// profile is unconfined, but an unconfined process still transitions
// into a matching profile on exec. That is measured, and recorded in
// the README section above. Install kea on a runner host and CI starts
// failing the same way.

// Vars rather than consts solely so the test can point them at fixture
// files. keaConfinementEvidence is the only part of this file that
// touches the filesystem or execs anything, and leaving it undrivable
// left the tier selection it performs -- the thing this file exists to
// get right -- as the one piece with no test. Nothing outside the test
// writes these.
var (
	// apparmorProfilesPath is the kernel's list of LOADED profiles and
	// their modes. Root-only, which the integration suite is.
	apparmorProfilesPath = "/sys/kernel/security/apparmor/profiles"
	// keaProfilePath is the profile as shipped on disk. World-readable,
	// and the weaker signal: present says a profile is installed, not
	// that it is loaded or enforcing.
	keaProfilePath = "/etc/apparmor.d/usr.sbin.kea-dhcp4"
	// readKernelLog returns the kernel ring buffer. This is where an
	// AppArmor denial actually lands, and it is the ONLY evidence that
	// says THIS process was denied rather than that a profile exists.
	// Root-only on a stock Debian (kernel.dmesg_restrict=1), which the
	// integration suite is.
	readKernelLog = func() (string, error) {
		// Pinned like every other subprocess this harness starts: an
		// unpinned dmesg speaks the host's language, and this reads its
		// output. See clocale.go.
		out, err := withCLocale(exec.Command("dmesg")).Output()
		return string(out), err
	}
)

// keaProfileMode returns the enforcement mode of the kea-dhcp4 profile
// as the kernel reports it ("enforce", "complain", ...), or "" when the
// profile is not loaded at all.
//
// Lines look like `kea-dhcp4 (enforce)`. Matching is anchored on the
// EXACT profile name, and the two loosenings it defends against are
// different mutants killed by different cases:
//
//   - a substring or "kea" prefix match would take `kea-lfc` -- a
//     separate profile the kea-dhcp4 one transitions to -- for ours.
//     That is what the "lfc alone is not kea-dhcp4" case holds.
//   - a `kea-dhcp4` prefix match would take `kea-dhcp4-custom`, a
//     different profile entirely; `kea-lfc` does NOT carry that prefix,
//     so the lfc case says nothing about it. That is what the "longer
//     name is not a match" case holds.
//
// Both are measured: each mutant was applied and only its own case went
// red. Citing one for the other's mutant is how a case comes to look
// load-bearing while the thing it supposedly defends is undefended.
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

// keaDenialRecord returns the last kernel AppArmor denial record that
// names both the kea-dhcp4 profile and a path under runDir, or "" when
// the kernel log holds none.
//
// This is the only outside evidence in this file. Everything else here
// describes the SYSTEM -- a profile exists, a profile is loaded, it is
// enforcing -- and none of that says the process that just failed was
// denied anything. A loaded enforcing profile and a confined fixture
// are separable: the shipped profile ends in
// `#include <local/usr.sbin.kea-dhcp4>`, the supported way to permit
// extra paths, so a site override can leave the profile loaded and
// enforcing while permitting exactly the paths this fixture uses.
//
// Keyed on the conjunction of all three facts on purpose:
//
//   - apparmor="DENIED" and not "ALLOWED", which is what a complain-mode
//     profile logs while permitting the access;
//   - profile="kea-dhcp4" quoted in full, so kea-lfc and kea-dhcp4-custom
//     cannot satisfy it, for the same reason keaProfileMode anchors;
//   - the fixture's own run directory, so a denial against some other
//     Kea on the host is not read as evidence about this run.
//
// An empty runDir matches every record, so it is refused rather than
// allowed to turn this into a check with one possible verdict.
//
// Record format measured on a Debian host, provoked by running the
// packaged kea-dhcp4 against a config in a temp directory:
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

// keaConfinement is everything the fixture managed to measure about
// AppArmor at the moment Kea failed to start.
//
// Each read carries its own outcome flag beside its value, because
// "could not look" and "looked, and the answer is no" are different
// facts that produce different sentences. Folding either pair into a
// single zero value is what made the first version of this file state a
// falsehood on the host it was written for: an unreadable profile list
// and a readable list with no kea-dhcp4 line both arrived as mode == ""
// and both printed "could not read ... to see whether the profile is
// loaded". The suite runs as root, where the read succeeds, so the
// branch that fired in practice was the one whose first sentence was
// untrue.
type keaConfinement struct {
	// mode is the kernel-reported enforcement mode, "" when the
	// kea-dhcp4 profile is not loaded. Meaningful only if listRead.
	mode string
	// listRead is whether the kernel's loaded-profile list was readable
	// at all.
	listRead bool
	// installed is whether a profile is present on disk.
	installed bool
	// kernelLogRead is whether the kernel ring buffer was readable.
	kernelLogRead bool
	// denial is the denial record naming runDir, "" when none was
	// found. Meaningful only if kernelLogRead.
	denial string
	// logEmpty is whether the Kea log the caller is about to print is
	// empty. The hint may only blame AppArmor for an empty log when the
	// log the reader can see really is empty.
	logEmpty bool
	// runDir is the fixture's temp directory.
	runDir string
}

// keaConfinementHint returns a non-empty explanation when AppArmor is
// the reason -- or the likely reason -- that an ephemeral Kea running
// from c.runDir never started. Empty means AppArmor does not explain it
// and the caller should not say that it does.
//
// The tiers are deliberate and are phrased differently, and each says
// only what was actually measured:
//
//   - a loaded enforcing profile WITH a kernel denial record naming this
//     fixture's directory is a measurement of this process, and is the
//     only case that states causation as fact;
//   - a loaded enforcing profile with no such record is the likely cause,
//     said as one -- absence of a record is not exculpatory, and it is
//     also not proof;
//   - a profile merely present on disk while the loaded-profile list
//     could not be read is a guess about the system, and is hedged.
//
// A complain-mode profile logs denials but permits the access, so it is
// NOT a cause and returns empty: reporting it would send the reader
// after a red herring while the real failure went unexplained. So does
// a list that was read and does not carry the profile at all -- that is
// a positive finding of "not loaded", not an unknown.
func keaConfinementHint(c keaConfinement) string {
	const remedy = "  Put the profile in complain mode for local runs, and restore it after:\n" +
		"    sudo apparmor_parser -C -r /etc/apparmor.d/usr.sbin.kea-dhcp4   # before\n" +
		"    sudo apparmor_parser -r    /etc/apparmor.d/usr.sbin.kea-dhcp4   # after\n" +
		"  Background, including why a privileged container does NOT escape this:\n" +
		"  test/integration/README.md, \"AppArmor (Debian/Ubuntu hosts only)\".\n"

	// Only claimable when the log the caller is about to print is in
	// fact empty. readLog returns the WHOLE file, appended to across
	// every Stop/StartAgain cycle, and returns a non-empty "(could not
	// read ...)" string on error -- so this sentence used to be printed
	// directly beneath a log that was visibly not empty.
	emptyLog := ""
	if c.logEmpty {
		emptyLog = " -- which is why the log above is empty"
	}

	// Purely descriptive: what the packaged profile permits, and where
	// this fixture runs. It carries no verb about what happened, so
	// both tiers can state it -- only the tier with a denial record has
	// grounds to say the paths were actually refused.
	const profilePaths = "  The profile permits a config only under /etc/kea/**, its log only at\n" +
		"  /var/log/kea/kea-dhcp4.log, leases only at /var/lib/kea/kea-leases4.csv* and\n" +
		"  the logger lock only at /run/lock/kea/logger_lockfile. This fixture runs from\n" +
		"  %s, so every path it needs is outside what the packaged\n" +
		"  profile permits.\n"

	switch {
	case c.mode == "enforce" && c.denial != "":
		// The one measurement of THIS process: the kernel says it
		// denied this fixture's own directory to the kea-dhcp4 profile.
		return fmt.Sprintf(
			"APPARMOR: the kea-dhcp4 profile is loaded in enforce mode and the kernel logged a\n"+
				"  denial against this fixture's own directory, so that is why Kea never started:\n"+
				"    %s\n"+profilePaths+
				"  Kea exits before writing a line%s.\n"+remedy,
			c.denial, c.runDir, emptyLog)

	case c.mode == "enforce":
		// Loaded and enforcing is a fact about the system, not about
		// this process, so the causal claim is downgraded to match.
		// Absence of a record is not exculpatory and the reader is told
		// why, so this does not read as an all-clear.
		unknown := "  No kernel denial record naming this directory was found, which does not clear\n" +
			"  AppArmor: such records are rate-limited, can be routed to auditd instead of the\n" +
			"  kernel ring buffer, and age out of it."
		if !c.kernelLogRead {
			unknown = "  The kernel ring buffer could not be read, so no denial record was consulted;\n" +
				"  this hint rests on the loaded profile alone."
		}
		// No empty-log clause here on purpose. Whether the log is empty
		// is known, but WHY it is empty is exactly the causal claim
		// this tier has no evidence for.
		return fmt.Sprintf(
			"APPARMOR: the kea-dhcp4 profile is loaded in enforce mode, which is the most likely\n"+
				"  reason Kea never started.\n"+profilePaths+unknown+"\n"+
				"  A site override at /etc/apparmor.d/local/usr.sbin.kea-dhcp4 could equally have\n"+
				"  permitted these paths, in which case the cause is elsewhere.\n"+
				"  Confirm with: sudo dmesg | grep 'apparmor=\"DENIED\".*kea-dhcp4'\n"+remedy,
			c.runDir)

	case !c.listRead && c.installed:
		// The list really was unreadable. This sentence is true only
		// here, which is the whole point of carrying listRead.
		return fmt.Sprintf(
			"APPARMOR: could not read %s to see whether the kea-dhcp4 profile is loaded, but\n"+
				"  a profile IS installed at %s. If it is loaded in enforce mode it confines Kea\n"+
				"  to the distro paths, and this fixture runs from %s, so every path it needs\n"+
				"  would be denied and Kea would exit before writing a line.\n"+
				"  Check with: sudo aa-status | grep kea, or sudo dmesg | grep 'apparmor=\"DENIED\"'\n"+remedy,
			apparmorProfilesPath, keaProfilePath, c.runDir)

	default:
		// Loaded in complain mode; or the list was READ and does not
		// carry the profile, whether or not one sits on disk; or
		// nothing is installed at all. AppArmor is not the explanation.
		// Say nothing rather than point the reader at the wrong thing.
		return ""
	}
}

// keaConfinementEvidence performs the three reads and reports what each
// one actually established, keeping "could not look" distinct from
// "looked, and the answer is no" in both cases where that distinction
// exists.
func keaConfinementEvidence(runDir string, logEmpty bool) keaConfinement {
	c := keaConfinement{runDir: runDir, logEmpty: logEmpty}

	if data, err := os.ReadFile(apparmorProfilesPath); err == nil {
		c.listRead = true
		c.mode = keaProfileMode(string(data))
	}
	_, statErr := os.Stat(keaProfilePath)
	c.installed = statErr == nil

	// Only worth the exec when a loaded enforcing profile has already
	// made a denial possible; the other tiers never quote a record.
	if c.mode == "enforce" {
		if kernelLog, err := readKernelLog(); err == nil {
			c.kernelLogRead = true
			c.denial = keaDenialRecord(kernelLog, runDir)
		}
	}
	return c
}

// appArmorKeaHint is keaConfinementHint with the reads done. logEmpty
// must be the emptiness of the log the CALLER is about to print, not a
// second read of the file.
func appArmorKeaHint(runDir string, logEmpty bool) string {
	return keaConfinementHint(keaConfinementEvidence(runDir, logEmpty))
}
