// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Debian and Ubuntu ship a kea-dhcp4 AppArmor profile: config /etc/kea/**, log /var/log/kea/kea-dhcp4.log, leases
// /var/lib/kea/kea-leases4.csv*, PID /run/kea/kea-dhcp4.kea-dhcp4.pid, lock /run/lock/kea/logger_lockfile (read from both
// packages, 2026-10-04). Under enforcement Kea exits with an empty log and only the kernel log shows why, so the fixture
// keeps every file on those paths, as the dhcp6 fixture does (#680, #214, #869). A privileged container still enters
// the profile on exec.

// kea4Dirs are the directories the ephemeral Kea fixture writes under, each one the profile permits (#680).
type kea4Dirs struct {
	// conf is a subdirectory of /etc/kea so the packaged kea-dhcp4.conf is never touched.
	conf, lease, pid, lock, log string
}

// kea4Default is the layout on a host with the packaged profile (#680).
var kea4Default = kea4Dirs{
	conf:  "/etc/kea/dh-itest-v4",
	lease: "/var/lib/kea",
	pid:   "/run/kea",
	lock:  "/run/lock/kea",
	log:   "/var/log/kea",
}

const (
	// kea4ConfFile keeps the basename the profile's PID rule is written for: Kea names the PID file after it.
	kea4ConfFile  = "kea-dhcp4.conf"
	kea4LeaseFile = "kea-leases4.csv"
	kea4PidFile   = "kea-dhcp4.kea-dhcp4.pid"
	kea4LogFile   = "kea-dhcp4.log"
)

func (d kea4Dirs) confPath() string  { return filepath.Join(d.conf, kea4ConfFile) }
func (d kea4Dirs) leasePath() string { return filepath.Join(d.lease, kea4LeaseFile) }
func (d kea4Dirs) logPath() string   { return filepath.Join(d.log, kea4LogFile) }

// mkdirAll creates every directory Kea needs: it creates none itself and exits before reporting a config error (#356).
func (d kea4Dirs) mkdirAll() error {
	for _, dir := range []string{d.conf, d.lease, d.pid, d.lock, d.log} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// removeState removes what a run creates (conf dir, lease and log with their suffixes, the PID file a SIGKILL leaves),
// at setup and teardown. The shared logger_lockfile and every dhcp6 file stay (#680).
func (d kea4Dirs) removeState() {
	_ = os.RemoveAll(d.conf)
	leases, _ := filepath.Glob(filepath.Join(d.lease, kea4LeaseFile+"*"))
	logs, _ := filepath.Glob(filepath.Join(d.log, kea4LogFile+"*"))
	for _, p := range append(append(leases, logs...), filepath.Join(d.pid, kea4PidFile)) {
		_ = os.Remove(p)
	}
}

// Vars so the test can point them at fixture files.
var (
	// apparmorProfilesPath is the kernel's root-only list of loaded profiles and their modes.
	apparmorProfilesPath = "/sys/kernel/security/apparmor/profiles"
	// keaProfilePath is the profile as shipped on disk; present does not mean loaded.
	keaProfilePath = "/etc/apparmor.d/usr.sbin.kea-dhcp4"
	// readKernelLog returns the kernel ring buffer, root-only under Debian's kernel.dmesg_restrict=1.
	readKernelLog = func() (string, error) { return firstKernelLog(kernelLogSources) }
)

// kernelLogSources: dmesg reads /dev/kmsg; journalctl -k is the fallback when that is closed to the process (#680).
var kernelLogSources = []func() (string, error){
	func() (string, error) {
		out, err := withCLocale(exec.Command("dmesg")).Output()
		return string(out), err
	},
	func() (string, error) {
		out, err := withCLocale(exec.Command("journalctl", "-k", "--no-pager")).Output()
		return string(out), err
	},
}

// firstKernelLog returns the first source that reads, or the last error.
func firstKernelLog(sources []func() (string, error)) (string, error) {
	err := fmt.Errorf("no kernel log source")
	for _, read := range sources {
		out, e := read()
		if e == nil {
			return out, nil
		}
		err = e
	}
	return "", err
}

// keaProfileMode returns the kernel-reported mode of the kea-dhcp4 profile, or "" when not loaded, matching the exact
// name: kea-lfc and kea-dhcp4-custom are other profiles (#869).
func keaProfileMode(profiles string) string { return profileModeOf(profiles, "kea-dhcp4") }

// kea6ProfileMode is keaProfileMode for the kea-dhcp6 profile (#214).
func kea6ProfileMode(profiles string) string { return profileModeOf(profiles, "kea-dhcp6") }

func profileModeOf(profiles, want string) string {
	for _, line := range strings.Split(profiles, "\n") {
		line = strings.TrimSpace(line)
		name, mode, ok := strings.Cut(line, " ")
		if !ok || name != want {
			continue
		}
		return strings.Trim(mode, "()")
	}
	return ""
}

// keaDenialRecord returns the last AppArmor DENIED record naming profile="kea-dhcp4" that is not in the kernel log
// snapshot taken before Kea started, or "". No pid keys it (a container sees its own, the audit line the host's); audit
// lines carry a unique timestamp and serial. An enforcing profile alone proves no denial (it ends in an #include of
// local/usr.sbin.kea-dhcp4) and complain mode logs ALLOWED. Record format measured on a Debian host (#869):
//
//	audit: type=1400 audit(...): apparmor="DENIED" operation="open"
//	class="file" profile="kea-dhcp4" name="/tmp/keaprobe.IGXt/kea.json"
//	pid=386894 comm="kea-dhcp4" requested_mask="r" denied_mask="r" ...
func keaDenialRecord(kernelLog, before string) string {
	seen := map[string]bool{}
	for _, line := range strings.Split(before, "\n") {
		seen[strings.TrimSpace(line)] = true
	}
	last := ""
	for _, line := range strings.Split(kernelLog, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, `apparmor="DENIED"`) ||
			!strings.Contains(line, `profile="kea-dhcp4"`) ||
			seen[line] {
			continue
		}
		last = line
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
	// denial is the kea-dhcp4 denial logged since the fixture started Kea; meaningful only if kernelLogRead.
	denial string
	// logEmpty is whether the Kea log the caller prints is empty.
	logEmpty bool
}

// keaConfinementHint explains why AppArmor did or may have stopped Kea, or returns "" when it does not. A denial record
// states causation whatever the profile list says (a container may not see securityfs), enforce without one is a
// candidate, a profile on disk behind an unreadable list is hedged; complain mode and a read list without the profile
// return "" (#869, #680).
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

	const profilePaths = "  The fixture keeps every file where the packaged profile permits it: config under\n" +
		"  /etc/kea/, leases at /var/lib/kea/kea-leases4.csv, the log at /var/log/kea/kea-dhcp4.log,\n" +
		"  the PID file in /run/kea and the lock in /run/lock/kea. The profile loaded on this host\n" +
		"  therefore differs from the packaged one (a site override or another version).\n"

	switch {
	case c.denial != "":
		return fmt.Sprintf(
			"APPARMOR: the kernel logged a kea-dhcp4 denial after this fixture started Kea,\n"+
				"  so that is why Kea never started:\n"+
				"    %s\n"+profilePaths+
				"  Kea exits before writing a line%s.\n"+remedy,
			c.denial, emptyLog)

	case c.mode == "enforce":
		unknown := "  No kernel denial record logged since the fixture started Kea was found, which does\n" +
			"  not clear AppArmor: such records are rate-limited and can be routed to auditd instead\n" +
			"  of the kernel ring buffer."
		if !c.kernelLogRead {
			unknown = "  The kernel log could not be read (dmesg and journalctl -k both failed), so no\n" +
				"  denial record was consulted; this hint rests on the loaded profile alone."
		}
		return "APPARMOR: the kea-dhcp4 profile is loaded in enforce mode, so it is a candidate cause\n" +
			"  of Kea never starting.\n" + profilePaths + unknown + "\n" +
			"  Confirm with: sudo dmesg | grep 'apparmor=\"DENIED\".*kea-dhcp4'\n" + remedy

	case !c.listRead && c.installed:
		kernel := ""
		if !c.kernelLogRead {
			kernel = "  The kernel log could not be read either (dmesg and journalctl -k both failed).\n"
		}
		return fmt.Sprintf(
			"APPARMOR: could not read %s to see whether the kea-dhcp4 profile is loaded, but\n"+
				"  a profile IS installed at %s. If it is loaded in enforce mode and a site\n"+
				"  override changed its paths, Kea would exit before writing a line.\n"+kernel+
				"  Check with: sudo aa-status | grep kea, or sudo dmesg | grep 'apparmor=\"DENIED\"'\n"+remedy,
			apparmorProfilesPath, keaProfilePath)

	default:
		return ""
	}
}

// keaConfinementEvidence performs the reads and reports what each established (#680).
func keaConfinementEvidence(before string, logEmpty bool) keaConfinement {
	c := keaConfinement{logEmpty: logEmpty}

	if data, err := os.ReadFile(apparmorProfilesPath); err == nil {
		c.listRead = true
		c.mode = keaProfileMode(string(data))
	}
	_, statErr := os.Stat(keaProfilePath)
	c.installed = statErr == nil

	// Read whatever the profile list says: a container without securityfs cannot read it but still sees the denial.
	if kernelLog, err := readKernelLog(); err == nil {
		c.kernelLogRead = true
		c.denial = keaDenialRecord(kernelLog, before)
	}
	return c
}

// appArmorKeaHint is keaConfinementHint with the reads done; logEmpty is about the log the caller prints.
func appArmorKeaHint(before string, logEmpty bool) string {
	return keaConfinementHint(keaConfinementEvidence(before, logEmpty))
}

// kea6DenialRecord returns the last AppArmor DENIED record naming profile="kea-dhcp6", or "". The fixture's state sits
// on the profile's own paths, so unlike the dhcp4 fixture there is no private directory to key the record on (#214).
func kea6DenialRecord(kernelLog string) string {
	last := ""
	for _, line := range strings.Split(kernelLog, "\n") {
		if strings.Contains(line, `apparmor="DENIED"`) && strings.Contains(line, `profile="kea-dhcp6"`) {
			last = strings.TrimSpace(line)
		}
	}
	return last
}

// kea6Confinement is what the fixture measured about the kea-dhcp6 profile (#214).
type kea6Confinement struct {
	// mode is the kernel-reported mode, "" when not loaded; meaningful only if listRead.
	mode          string
	listRead      bool
	installed     bool
	kernelLogRead bool
	denial        string
}

// String is the measured state in one line, for the fixture's start log.
func (c kea6Confinement) String() string {
	switch {
	case !c.listRead:
		return fmt.Sprintf("kea-dhcp6 AppArmor profile: loaded list unreadable, profile file present=%t", c.installed)
	case c.mode == "":
		return fmt.Sprintf("kea-dhcp6 AppArmor profile: not loaded, profile file present=%t", c.installed)
	}
	return "kea-dhcp6 AppArmor profile: " + c.mode
}

// kea6ConfinementHint explains why AppArmor did or may have stopped kea-dhcp6, or returns "" (#214).
func kea6ConfinementHint(c kea6Confinement) string {
	const paths = "  The fixture keeps every file where the profile allows it: config under /etc/kea/, leases at\n" +
		"  /var/lib/kea/kea-leases6.csv, the log at /var/log/kea/kea-dhcp6.log, the PID file at /run/kea and the lock at\n" +
		"  /run/lock/kea. Kea's own stdout and stderr go to a temp file the profile may deny; readiness does not read it.\n"
	switch {
	case c.mode == "enforce" && c.denial != "":
		return "APPARMOR: the kea-dhcp6 profile is enforcing and the kernel logged a denial:\n    " + c.denial + "\n" + paths
	case c.mode == "enforce" && !c.kernelLogRead:
		return "APPARMOR: the kea-dhcp6 profile is enforcing and the kernel log could not be read, so no denial was consulted.\n" + paths
	case c.mode == "enforce":
		return "APPARMOR: the kea-dhcp6 profile is enforcing; no denial record was found, which does not clear it (records are\n" +
			"  rate-limited and age out).\n" + paths
	case !c.listRead && c.installed:
		return "APPARMOR: a kea-dhcp6 profile is installed and the list of loaded profiles could not be read.\n" + paths
	}
	return ""
}

// keaProc is one running process as read from /proc.
type keaProc struct {
	pid  int
	args []string
}

// keaHostConflict decides whether the fixture may delete the packaged lease and log files (#680). A kea-dhcp4 whose
// config is under confDir is a fixture's own, or its leftover; any other kea-dhcp4 process, or an active kea-dhcp4
// unit, is a real Kea whose state removeState would destroy. It returns "" when nothing conflicts.
func keaHostConflict(procs []keaProc, activeUnits []string, confDir string) string {
	var found []string
	for _, p := range procs {
		if len(p.args) == 0 || filepath.Base(p.args[0]) != "kea-dhcp4" {
			continue
		}
		conf := ""
		for i, a := range p.args {
			if a == "-c" && i+1 < len(p.args) {
				conf = p.args[i+1]
			}
		}
		if strings.HasPrefix(conf, confDir+"/") {
			continue
		}
		found = append(found, fmt.Sprintf("process kea-dhcp4 pid %d", p.pid))
	}
	for _, u := range activeUnits {
		found = append(found, "active unit "+u)
	}
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf("a Kea this fixture did not start is running (%s) and the fixture would delete its lease file and "+
		"log (%s*, %s*); nothing was touched. Stop it, or run the suite on a host without a kea-dhcp4 service (#680)",
		strings.Join(found, ", "), filepath.Join(kea4Default.lease, kea4LeaseFile), filepath.Join(kea4Default.log, kea4LogFile))
}

// readKeaProcs lists the processes under procRoot (/proc); an unreadable entry is a process that exited meanwhile.
func readKeaProcs(procRoot string) []keaProc {
	entries, _ := os.ReadDir(procRoot)
	var procs []keaProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		procs = append(procs, keaProc{pid: pid, args: strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")})
	}
	return procs
}
