// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
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
// visible only in dmesg. That cost a real debugging detour, which is
// why this is a check and not a paragraph in a README.
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
// files. appArmorKeaHint is the only part of this file that touches the
// filesystem, and leaving it undrivable left the tier selection it
// performs -- the thing this file exists to get right -- as the one
// piece with no test. Nothing outside the test writes these.
var (
	// apparmorProfilesPath is the kernel's list of LOADED profiles and
	// their modes. Root-only, which the integration suite is.
	apparmorProfilesPath = "/sys/kernel/security/apparmor/profiles"
	// keaProfilePath is the profile as shipped on disk. World-readable,
	// and the weaker signal: present says a profile is installed, not
	// that it is loaded or enforcing.
	keaProfilePath = "/etc/apparmor.d/usr.sbin.kea-dhcp4"
)

// keaProfileMode returns the enforcement mode of the kea-dhcp4 profile
// as the kernel reports it ("enforce", "complain", ...), or "" when the
// profile is not loaded at all.
//
// Lines look like `kea-dhcp4 (enforce)`. Matching is anchored on the
// exact profile name so that kea-lfc -- a separate profile the kea one
// transitions to -- is not mistaken for it.
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

// keaConfinementHint returns a non-empty explanation when AppArmor is
// the reason -- or the likely reason -- that an ephemeral Kea running
// from runDir never started. Empty means AppArmor does not explain it
// and the caller should not say that it does.
//
// The two tiers are deliberate and are phrased differently. A loaded
// enforcing profile is measured and stated as fact; a profile merely
// present on disk is a guess, because it says nothing about whether the
// profile is loaded or which mode it is in, and is stated as one.
//
// A complain-mode profile logs denials but permits the access, so it is
// NOT a cause and returns empty: reporting it would send the reader
// after a red herring while the real failure went unexplained.
func keaConfinementHint(mode string, profileInstalled bool, runDir string) string {
	const remedy = "  Put the profile in complain mode for local runs, and restore it after:\n" +
		"    sudo apparmor_parser -C -r /etc/apparmor.d/usr.sbin.kea-dhcp4   # before\n" +
		"    sudo apparmor_parser -r    /etc/apparmor.d/usr.sbin.kea-dhcp4   # after\n" +
		"  Background, including why a privileged container does NOT escape this:\n" +
		"  test/integration/README.md, \"AppArmor (Debian/Ubuntu hosts only)\".\n"

	switch {
	case mode == "enforce":
		return fmt.Sprintf(
			"APPARMOR: the kea-dhcp4 profile is loaded in enforce mode, and that is why Kea\n"+
				"  never started. It permits a config only under /etc/kea/**, its log only at\n"+
				"  /var/log/kea/kea-dhcp4.log, leases only at /var/lib/kea/kea-leases4.csv* and\n"+
				"  the logger lock only at /run/lock/kea/logger_lockfile. This fixture runs from\n"+
				"  %s, so its config, log, leases and lockfile are all denied and Kea exits\n"+
				"  before writing a line -- which is why the log above is empty.\n"+
				"  Confirm with: sudo dmesg | grep 'apparmor=\"DENIED\".*kea-dhcp4'\n"+remedy,
			runDir)
	case mode == "" && profileInstalled:
		return fmt.Sprintf(
			"APPARMOR: could not read %s to see whether the kea-dhcp4 profile is loaded, but\n"+
				"  a profile IS installed at %s. If it is loaded in enforce mode it confines Kea\n"+
				"  to the distro paths, and this fixture runs from %s, so every path it needs\n"+
				"  would be denied and Kea would exit before writing a line.\n"+
				"  Check with: sudo aa-status | grep kea, or sudo dmesg | grep 'apparmor=\"DENIED\"'\n"+remedy,
			apparmorProfilesPath, keaProfilePath, runDir)
	default:
		// Not loaded and not installed, or loaded in complain mode:
		// AppArmor is not the explanation. Say nothing rather than
		// point the reader at the wrong thing.
		return ""
	}
}

// appArmorKeaHint is keaConfinementHint with the two reads done. Both
// failures degrade to the weaker tier rather than to silence.
func appArmorKeaHint(runDir string) string {
	mode := ""
	if data, err := os.ReadFile(apparmorProfilesPath); err == nil {
		mode = keaProfileMode(string(data))
	}
	_, statErr := os.Stat(keaProfilePath)
	return keaConfinementHint(mode, statErr == nil, runDir)
}
