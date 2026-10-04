// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: the path layout and the cleanup need no root and no Kea, so they run in the unit job (#680).

package harness

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The path rules of the packaged kea-dhcp4 profile, copied from kea-dhcp4-server 2.4.1-3ubuntu0.2 (Ubuntu noble) and
// 2.6.3-1+deb13u1 (Debian trixie), identical in both, read from the package 2026-10-04 (#680).
const packagedProfileRules = `
  /etc/kea/ r,
  /etc/kea/** r,
  /usr/sbin/kea-dhcp4 mr,
  /usr/sbin/kea-lfc Px,

  owner /run/kea/kea-dhcp4.kea-dhcp4.pid rw,
  owner /run/lock/kea/logger_lockfile rwk,

  owner /{tmp,run/kea}/kea4-ctrl-socket w,
  owner /{tmp,run/kea}/kea4-ctrl-socket.lock rwk,

  owner /var/lib/kea/kea-leases4.csv* rw,

  owner /var/log/kea/kea-dhcp4.log rw,
  owner /var/log/kea/kea-dhcp4.log.[0-9]* rw,
  owner /var/log/kea/kea-dhcp4.log.lock rwk,
`

// profileGlob turns an AppArmor path rule into an anchored regexp: ** crosses a slash, * does not, {a,b} alternates.
func profileGlob(rule string) *regexp.Regexp {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(rule); i++ {
		switch c := rule[i]; {
		case c == '*' && i+1 < len(rule) && rule[i+1] == '*':
			sb.WriteString(".*")
			i++
		case c == '*':
			sb.WriteString("[^/]*")
		case c == '{':
			sb.WriteString("(")
		case c == '}':
			sb.WriteString(")")
		case c == ',':
			sb.WriteString("|")
		case c == '[' || c == ']':
			sb.WriteByte(c)
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.MustCompile(sb.String())
}

// profileAllows reports whether any path rule of rules matches path; `owner` and the permissions are not read.
func profileAllows(rules, path string) bool {
	for _, line := range strings.Split(rules, "\n") {
		fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ","))
		for _, f := range fields {
			if strings.HasPrefix(f, "/") && profileGlob(f).MatchString(path) {
				return true
			}
		}
	}
	return false
}

func TestProfileAllows_Matcher(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/etc/kea/dh-itest-v4/kea-dhcp4.conf", true},
		{"/var/lib/kea/kea-leases4.csv", true},
		{"/var/lib/kea/kea-leases4.csv.completed", true},
		{"/var/log/kea/kea-dhcp4.log.2", true},
		{"/tmp/kea4-ctrl-socket", true},
		{"/tmp/dh-itest-ephemeral-1/leases4.csv", false},
		{"/tmp/dh-itest-ephemeral-1/kea-dhcp4.json", false},
		{"/run/kea/logger_lockfile", false},
		{"/var/lib/kea/leases4.csv", false},
		{"/var/log/kea/kea-dhcp4.logx", false},
		{"/run/kea/kea-dhcp6.kea-dhcp6.pid", false},
	} {
		if got := profileAllows(packagedProfileRules, tc.path); got != tc.want {
			t.Errorf("profileAllows(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}

// Every file the fixture makes Kea create or open must match a rule of the packaged profile; moving one back to a temp
// directory, or renaming the config, turns this red (#680).
func TestKea4Default_EveryFilePathIsPermittedByThePackagedProfile(t *testing.T) {
	d := kea4Default
	for name, path := range map[string]string{
		"config":         d.confPath(),
		"lease file":     d.leasePath(),
		"PID file":       filepath.Join(d.pid, kea4PidFile),
		"logger lock":    filepath.Join(d.lock, "logger_lockfile"),
		"log":            d.logPath(),
		"log lock":       d.logPath() + ".lock",
		"lease rotation": d.leasePath() + ".completed",
	} {
		if !profileAllows(packagedProfileRules, path) {
			t.Errorf("%s %s is outside what the packaged kea-dhcp4 profile permits", name, path)
		}
		if !d.needs(path) {
			t.Errorf("%s %s is not in kea4Dirs.needs, so a denial of it would not be blamed on AppArmor", name, path)
		}
	}
	// Kea names the PID file after the config's basename; the profile permits exactly one name.
	if want := strings.TrimSuffix(kea4ConfFile, ".conf") + ".kea-dhcp4.pid"; kea4PidFile != want {
		t.Errorf("PID file %q does not follow from config file %q (Kea would write %q)", kea4PidFile, kea4ConfFile, want)
	}
}

func testKea4Dirs(t *testing.T) kea4Dirs {
	t.Helper()
	root := t.TempDir()
	return kea4Dirs{
		conf: filepath.Join(root, "etc/kea/dh-itest-v4"), lease: filepath.Join(root, "var/lib/kea"),
		pid: filepath.Join(root, "run/kea"), lock: filepath.Join(root, "run/lock/kea"), log: filepath.Join(root, "var/log/kea"),
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func TestKea4Dirs_MkdirAllCreatesEveryDirectory(t *testing.T) {
	d := testKea4Dirs(t)
	if err := d.mkdirAll(); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	for _, dir := range []string{d.conf, d.lease, d.pid, d.lock, d.log} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Errorf("%s was not created: %v", dir, err)
		}
	}
	if err := d.mkdirAll(); err != nil {
		t.Errorf("a second mkdirAll must succeed over existing directories: %v", err)
	}
}

func TestKea4Dirs_RemoveStateRemovesExactlyWhatARunCreates(t *testing.T) {
	d := testKea4Dirs(t)
	created := []string{
		d.confPath(), filepath.Join(d.conf, "probe-"+kea4ConfFile),
		d.leasePath(), d.leasePath() + ".completed", d.leasePath() + ".output", d.leasePath() + ".pid", d.leasePath() + ".2",
		d.logPath(), d.logPath() + ".1", d.logPath() + ".lock",
		filepath.Join(d.pid, kea4PidFile),
	}
	// Not ours: the shared logger lock, the dhcp6 fixture's files and the packaged config.
	kept := []string{
		filepath.Join(d.lock, "logger_lockfile"),
		filepath.Join(d.lease, "kea-leases6.csv"), filepath.Join(d.lease, "kea-dhcp6-serverid"),
		filepath.Join(d.log, "kea-dhcp6.log"),
		filepath.Join(d.pid, "kea-dhcp6.kea-dhcp6.pid"),
		filepath.Join(filepath.Dir(d.conf), "kea-dhcp4.conf"), filepath.Join(filepath.Dir(d.conf), "dh-itest", "kea-dhcp6.conf"),
	}
	for _, p := range append(append([]string{}, created...), kept...) {
		touch(t, p)
	}

	d.removeState()

	for _, p := range created {
		if exists(p) {
			t.Errorf("%s survived removeState", p)
		}
	}
	for _, p := range kept {
		if !exists(p) {
			t.Errorf("removeState removed %s, which the fixture did not create", p)
		}
	}
	d.removeState()
}

func TestKea4Dirs_RemoveStateToleratesNothingThere(t *testing.T) {
	testKea4Dirs(t).removeState()
}

func TestFirstKernelLog(t *testing.T) {
	boom := errors.New("denied")
	ok := func(s string) func() (string, error) { return func() (string, error) { return s, nil } }
	fail := func() (string, error) { return "", boom }
	never := func() (string, error) { t.Error("a source after a successful one was read"); return "", boom }

	if got, err := firstKernelLog([]func() (string, error){ok("dmesg text"), never}); err != nil || got != "dmesg text" {
		t.Errorf("first source readable: got %q, %v", got, err)
	}
	if got, err := firstKernelLog([]func() (string, error){fail, ok("journal text")}); err != nil || got != "journal text" {
		t.Errorf("fall back to the second source: got %q, %v", got, err)
	}
	if got, err := firstKernelLog([]func() (string, error){ok(""), never}); err != nil || got != "" {
		t.Errorf("empty but readable: got %q, %v", got, err)
	}
	if _, err := firstKernelLog([]func() (string, error){fail, fail}); !errors.Is(err, boom) {
		t.Errorf("no source readable: want the source's error, got %v", err)
	}
	if _, err := firstKernelLog(nil); err == nil {
		t.Error("no sources at all must be an error, not an empty log")
	}
}

// Static, like the hint guard: the constructor needs Kea and a netns, so the unit job reads the source (#680).
func TestEphemeralKea_ConstructorAndTeardownUseTheProfilePaths(t *testing.T) {
	raw, err := os.ReadFile("ephemeral.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		"ef.leaseFile = kea4Default.leasePath()",
		"ef.configFile = kea4Default.confPath()",
		"ef.keaLog = kea4Default.logPath()",
	} {
		if strings.Count(src, want) != 1 {
			t.Errorf("ephemeral.go must contain exactly one %q: the file would leave the profile's paths", want)
		}
	}
	// Kea's stdout and stderr go to its own log: an fd inherited from a temp dir is denied by the profile (#680).
	if strings.Count(src, "os.OpenFile(ef.keaLog,") != 1 {
		t.Error("startKea must open ef.keaLog for Kea's stdout and stderr, not a file in the temp dir")
	}
	if n := strings.Count(src, "kea4Default.removeState()"); n != 2 {
		t.Errorf("kea4Default.removeState() appears %d times in ephemeral.go, want 2 (setup and teardown)", n)
	}
}

func TestKeaHostConflict(t *testing.T) {
	conf := "/etc/kea/dh-itest-v4"
	own := keaProc{pid: 10, args: []string{"kea-dhcp4", "-c", conf + "/kea-dhcp4.conf"}}
	hostKea := keaProc{pid: 20, args: []string{"/usr/sbin/kea-dhcp4", "-c", "/etc/kea/kea-dhcp4.conf"}}
	cases := []struct {
		name  string
		procs []keaProc
		units []string
		want  []string // substrings of the message; nil means no conflict
	}{
		{"nothing running", nil, nil, nil},
		{"only the fixture's own leftover", []keaProc{own}, nil, nil},
		{"a real Kea process", []keaProc{hostKea}, nil, []string{"pid 20", "nothing was touched"}},
		{"the real one beside the fixture's own", []keaProc{own, hostKea}, nil, []string{"pid 20"}},
		{"a Kea with no -c", []keaProc{{pid: 30, args: []string{"kea-dhcp4"}}}, nil, []string{"pid 30"}},
		{"a config dir that only shares the prefix", []keaProc{{pid: 40, args: []string{"kea-dhcp4", "-c", conf + "-other/x.conf"}}}, nil, []string{"pid 40"}},
		{"another daemon", []keaProc{{pid: 50, args: []string{"kea-dhcp6", "-c", "/etc/kea/kea-dhcp6.conf"}}, {pid: 51, args: []string{"vim", "kea-dhcp4"}}, {pid: 52}}, nil, nil},
		{"an installed unit and no process", nil, []string{"isc-kea-dhcp4-server.service (disabled)"}, []string{"installed unit isc-kea-dhcp4-server.service (disabled)"}},
		{"unit and process both named", []keaProc{hostKea}, []string{"kea-dhcp4.service (enabled)"}, []string{"pid 20", "kea-dhcp4.service (enabled)"}},
	}
	for _, c := range cases {
		got := keaHostConflict(c.procs, c.units, conf)
		if c.want == nil {
			if got != "" {
				t.Errorf("%s: want no conflict, got %q", c.name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: message %q lacks %q", c.name, got, w)
			}
		}
	}
}

func TestReadKeaProcs_ParsesCmdlineAndSkipsTheRest(t *testing.T) {
	root := t.TempDir()
	write := func(name, cmdline string) {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("123", "/usr/sbin/kea-dhcp4\x00-c\x00/etc/kea/kea-dhcp4.conf\x00")
	write("124", "") // a kernel thread
	write("self", "kea-dhcp4\x00")
	if err := os.MkdirAll(filepath.Join(root, "125"), 0o755); err != nil { // exited: no cmdline
		t.Fatal(err)
	}
	procs := readKeaProcs(root)
	if len(procs) != 1 || procs[0].pid != 123 || len(procs[0].args) != 3 || procs[0].args[2] != "/etc/kea/kea-dhcp4.conf" {
		t.Fatalf("procs = %+v, want only pid 123 with three args", procs)
	}
	if msg := keaHostConflict(procs, nil, "/etc/kea/dh-itest-v4"); !strings.Contains(msg, "pid 123") {
		t.Errorf("a parsed real Kea must conflict, got %q", msg)
	}
}

func TestEphemeralKea_GuardRunsBeforeTheFirstDelete(t *testing.T) {
	raw, err := os.ReadFile("ephemeral.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	guard, del := strings.Index(src, "foreignKea()"), strings.Index(src, "kea4Default.removeState()")
	if guard < 0 || del < 0 || guard > del {
		t.Errorf("foreignKea() must precede the first kea4Default.removeState() in ephemeral.go (guard at %d, delete at %d)", guard, del)
	}
}

func TestParseKeaUnitFiles(t *testing.T) {
	out := "isc-kea-dhcp4-server.service enabled  enabled\n" +
		"kea-dhcp4-server.service     masked   enabled\n" +
		"kea-dhcp4.service            masked-runtime enabled\n" +
		"isc-kea-dhcp4-server@.service static   -\n" +
		"kea-dhcp6-server.service     enabled  enabled\n" +
		"garbage\n\n"
	got := parseKeaUnitFiles(out)
	want := []string{"isc-kea-dhcp4-server.service (enabled)", "isc-kea-dhcp4-server@.service (static)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("units = %q, want %q (masked and masked-runtime excluded, dhcp6 ignored)", got, want)
	}
}

func TestForeignKea_ReadsTheHostsUnitFiles(t *testing.T) {
	old := systemctlUnitFiles
	t.Cleanup(func() { systemctlUnitFiles = old })
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"an enabled unit", "isc-kea-dhcp4-server.service enabled enabled\n", nil, true},
		{"a disabled unit, stopped or failed all the same", "kea-dhcp4-server.service disabled enabled\n", nil, true},
		{"a masked unit, as the hosted lane leaves it", "isc-kea-dhcp4-server.service masked enabled\n", nil, false},
		{"no systemctl", "", errors.New("exec: systemctl: not found"), false},
		{"no kea unit files", "", nil, false},
	} {
		systemctlUnitFiles = func() (string, error) { return tc.out, tc.err }
		got := foreignKea()
		if tc.want && !strings.Contains(got, "installed unit") {
			t.Errorf("%s: want a unit conflict, got %q", tc.name, got)
		}
		if !tc.want && strings.Contains(got, "installed unit") {
			t.Errorf("%s: want no unit conflict, got %q", tc.name, got)
		}
	}
}
