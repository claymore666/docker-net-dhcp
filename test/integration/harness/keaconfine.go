// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	// readKeaDirs is the owner and mode of the directories Kea uses.
	readKeaDirs = func() []keaDirState { return statKeaDirs(kea4Default) }
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
// snapshot taken before Kea started and whose name="..." is a path the fixture needs (kea4Dirs.needs), or "". Another
// path is not this fixture's failure, for instance a file_inherit of an fd. No pid keys it (a container sees its own,
// the audit line the host's); audit lines carry a unique timestamp and serial. An enforcing profile alone proves no denial (it ends in an #include of
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
			seen[line] || !(kea4Default.needs(auditName(line)) || isDACDenial(line)) {
			continue
		}
		last = line
	}
	return last
}

// isDACDenial reports a capability denial that carries no path: root Kea denied dac_read_search or dac_override, which
// the packaged profile never grants, so a 0750 _kea directory (all five on a stock host) is closed to it (#680).
func isDACDenial(line string) bool {
	return strings.Contains(line, `operation="capable"`) &&
		(strings.Contains(line, `capname="dac_read_search"`) || strings.Contains(line, `capname="dac_override"`))
}

// auditName is the path of an audit line's name="..." field, or "".
func auditName(line string) string {
	const key = ` name="`
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	return rest[:strings.IndexByte(rest+`"`, '"')]
}

// needs reports whether name is a path a fixture run reads or writes: its conf dir, or one of the packaged files (#680).
func (d kea4Dirs) needs(name string) bool {
	if name == d.conf || strings.HasPrefix(name, d.conf+"/") {
		return true
	}
	dir, base := filepath.Split(name)
	for _, f := range []struct{ dir, prefix string }{
		{d.lease, kea4LeaseFile}, {d.log, kea4LogFile}, {d.pid, kea4PidFile}, {d.lock, "logger_lockfile"},
	} {
		if filepath.Clean(dir) == f.dir && strings.HasPrefix(base, f.prefix) {
			return true
		}
	}
	return false
}

// keaDirState is a fixture directory's owner and mode as stat reported them; write is set where Kea creates files.
type keaDirState struct {
	path     string
	owner    string
	uid, gid uint32
	mode     os.FileMode
	write    bool
}

// states lists the directories Kea must enter (/etc/kea, above the fixture's conf dir) or write (#680).
func (d kea4Dirs) states() []keaDirState {
	return []keaDirState{{path: filepath.Dir(d.conf)}, {path: d.lease, write: true}, {path: d.log, write: true},
		{path: d.pid, write: true}, {path: d.lock, write: true}}
}

// statKeaDirs reads owner and mode of each of d.states; an absent one is skipped, the fixture creates it as root.
func statKeaDirs(d kea4Dirs) []keaDirState {
	var out []keaDirState
	for _, s := range d.states() {
		fi, err := os.Stat(s.path)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		s.uid, s.gid, s.mode = st.Uid, st.Gid, fi.Mode()
		uid, gid := strconv.Itoa(int(st.Uid)), strconv.Itoa(int(st.Gid))
		if u, err := user.LookupId(uid); err == nil {
			uid = u.Username
		}
		if g, err := user.LookupGroupId(gid); err == nil {
			gid = g.Name
		}
		s.owner = uid + ":" + gid
		out = append(out, s)
	}
	return out
}

// rootCannotUse reports whether root Kea, holding neither dac_override nor dac_read_search, lacks x (and w where Kea
// creates files) on s: the kernel picks the owner, group or other bits by uid 0 and gid 0 (#680).
func rootCannotUse(s keaDirState) bool {
	bits := s.mode.Perm()
	switch {
	case s.uid == 0:
		bits >>= 6
	case s.gid == 0:
		bits >>= 3
	}
	need := os.FileMode(1)
	if s.write {
		need |= 2
	}
	return bits&need != need
}

// keaDirFix is the command that gives root the owner bits on s, with the owner and mode it sets.
func keaDirFix(s keaDirState) string {
	return "sudo install -d -o root -g root -m 0755 " + s.path
}

// keaDirsClosed is the states root Kea cannot use.
func keaDirsClosed(dirs []keaDirState) []keaDirState {
	var out []keaDirState
	for _, s := range dirs {
		if rootCannotUse(s) {
			out = append(out, s)
		}
	}
	return out
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
	// dirs is the owner and mode of the directories Kea uses, read when Kea failed.
	dirs []keaDirState
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
		"  the PID file in /run/kea and the lock in /run/lock/kea.\n"
	const differs = "the profile loaded on this host differs from the packaged one (a site override or another version).\n"

	dacCause := dacCauseOf(keaDirsClosed(c.dirs))

	switch {
	case isDACDenial(c.denial):
		return fmt.Sprintf(
			"APPARMOR: the kernel logged a kea-dhcp4 denial after this fixture started Kea,\n"+
				"  so that is why Kea never started:\n"+
				"    %s\n%s"+
				"  Kea exits before writing a line%s.\n"+remedy,
			c.denial, dacCause, emptyLog)

	case c.denial != "":
		return fmt.Sprintf(
			"APPARMOR: the kernel logged a kea-dhcp4 denial after this fixture started Kea,\n"+
				"  so that is why Kea never started:\n"+
				"    %s\n"+profilePaths+"  Therefore "+differs+
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
			"  of Kea never starting.\n" + profilePaths + "  If it is the cause, " + differs + unknown + "\n" +
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

// dacCauseOf names the directories closed to root Kea, each with its owner and mode and a command that opens it.
func dacCauseOf(closed []keaDirState) string {
	const head = "  This is a capability denial with no path: Kea runs as root and the packaged profile grants no\n" +
		"  dac_read_search or dac_override"
	if len(closed) == 0 {
		return head + ". None of the directories Kea uses is closed to root (owner and mode read just\n" +
			"  now), so the cause is not one of them.\n"
	}
	var b strings.Builder
	b.WriteString(head + ", and these directories are closed to root (owner and mode read just now):\n")
	for _, s := range closed {
		fmt.Fprintf(&b, "    %s  %s %04o\n", s.path, s.owner, s.mode.Perm())
	}
	b.WriteString("  A stock kea install makes them _kea 0750, which the packaged profile enforces as shipped. Fix, once:\n")
	for _, s := range closed {
		b.WriteString("    " + keaDirFix(s) + "\n")
	}
	return b.String()
}

// keaConfinementEvidence performs the reads and reports what each established (#680).
func keaConfinementEvidence(before string, beforeRead, logEmpty bool) keaConfinement {
	c := keaConfinement{logEmpty: logEmpty, dirs: readKeaDirs()}

	if data, err := os.ReadFile(apparmorProfilesPath); err == nil {
		c.listRead = true
		c.mode = keaProfileMode(string(data))
	}
	_, statErr := os.Stat(keaProfilePath)
	c.installed = statErr == nil

	// Read whatever the profile list says: a container without securityfs cannot read it but still sees the denial.
	// Without a before snapshot an old denial cannot be told from a new one, so the log counts as unread (#680).
	if kernelLog, err := readKernelLog(); err == nil && beforeRead {
		c.kernelLogRead = true
		c.denial = keaDenialRecord(kernelLog, before)
	}
	return c
}

// appArmorKeaHint is keaConfinementHint with the reads done; logEmpty is about the log the caller prints.
func appArmorKeaHint(before string, beforeRead, logEmpty bool) string {
	return keaConfinementHint(keaConfinementEvidence(before, beforeRead, logEmpty))
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
// config is under confDir is a fixture's own, or its leftover; any other kea-dhcp4 process, or an installed kea-dhcp4
// unit, is a real Kea whose state removeState would destroy. It returns "" when nothing conflicts.
func keaHostConflict(procs []keaProc, units []string, confDir string) string {
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
	for _, u := range units {
		found = append(found, "installed unit "+u)
	}
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf("a Kea this fixture did not start is running or installed (%s) and the fixture would delete its lease file and "+
		"log (%s*, %s*); nothing was touched. Stop and mask its unit, or run the suite on a host without a kea-dhcp4 service (#680)",
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

// systemctlUnitFiles lists the installed kea-dhcp4 service unit files; a var so a test can stand in for systemctl.
var systemctlUnitFiles = func() (string, error) {
	out, err := withCLocale(exec.Command("systemctl", "list-unit-files", "--no-legend", "--plain",
		"--type=service", "*kea-dhcp4*")).Output()
	return string(out), err
}

// parseKeaUnitFiles returns "unit (state)" for every listed unit file that is not masked: a masked unit cannot start,
// and the hosted lane masks what it installs, while any other state (enabled, disabled, static, ...) is a real Kea (#680).
func parseKeaUnitFiles(out string) []string {
	var units []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.Contains(f[0], "kea-dhcp4") || strings.HasPrefix(f[1], "masked") {
			continue
		}
		units = append(units, f[0]+" ("+f[1]+")")
	}
	return units
}

// foreignKea reads the host and returns keaHostConflict's verdict; a host without systemctl has no units (#680).
func foreignKea() string {
	out, _ := systemctlUnitFiles()
	return keaHostConflict(readKeaProcs("/proc"), parseKeaUnitFiles(out), kea4Default.conf)
}
