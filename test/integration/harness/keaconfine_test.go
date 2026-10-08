// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The kernel's profile list on a Debian host with kea installed; kea-lfc is a separate profile kea-dhcp4 transitions to
// (#869).
const sampleProfiles = `/usr/bin/man (enforce)
kea-dhcp4 (enforce)
kea-lfc (enforce)
man_filter (enforce)
`

const profilesWithoutKea = `/usr/bin/man (enforce)
kea-lfc (enforce)
man_filter (enforce)
`

// A real AppArmor denial of the packaged kea-dhcp4 on a Debian host (#869); only the pid and audit serial are altered.
const sampleDenialFmt = `[62803.914006] audit: type=1400 audit(1787871229.672:%d): apparmor="DENIED" ` +
	`operation="open" class="file" profile="kea-dhcp4" name="%s" pid=386894 ` +
	`comm="kea-dhcp4" requested_mask="r" denied_mask="r" fsuid=0 ouid=0`

// denialFor is one denial of name; the serial makes two denials of one path distinct lines, as the audit serial does.
func denialFor(serial int, name string) string {
	return fmt.Sprintf(sampleDenialFmt, serial, name)
}

// capDenialFor is a capability denial of the packaged profile: no name= field, as the kernel logs it (#680).
func capDenialFor(serial int, capname string) string {
	return fmt.Sprintf(`[62803.914006] audit: type=1400 audit(1787871229.672:%d): apparmor="DENIED" operation="capable" `+
		`class="cap" profile="kea-dhcp4" pid=386894 comm="kea-dhcp4" capability=2 capname="%s"`, serial, capname)
}

// denial1 is a denial of a path the fixture needs, in the shape #680 measured on the affected host.
var denial1 = denialFor(1884, "/var/lib/kea/kea-leases4.csv")

func TestKeaProfileMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		profiles string
		want     string
	}{
		{"enforce", sampleProfiles, "enforce"},
		{"complain", "kea-dhcp4 (complain)\n", "complain"},
		{"not loaded", profilesWithoutKea, ""},
		{"empty", "", ""},
		{"lfc alone is not kea-dhcp4", "kea-lfc (enforce)\n", ""},
		{"longer name is not a match", "kea-dhcp4-custom (enforce)\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keaProfileMode(tc.profiles); got != tc.want {
				t.Errorf("keaProfileMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeaDenialRecord(t *testing.T) {
	older := denialFor(1000, "/var/lib/kea/kea-leases4.csv")

	for _, tc := range []struct {
		name    string
		log     string
		before  string
		wantHit bool
	}{
		{"a denial logged after the snapshot", "noise\n" + denial1 + "\n", "noise\n", true},
		{"a denial present in the snapshot is an older run's", older + "\n", older + "\n", false},
		{"no snapshot reads every denial as new", denial1 + "\n", "", true},
		{"empty kernel log", "", "", false},
		// Kea's stdout and stderr once pointed at a tmp file: every start logged this, and no failure is its cause.
		{"a file_inherit of an unrelated path is not blamed", strings.Replace(denialFor(7, "/tmp/dh-itest-ephemeral-1/dhcp-server.log"),
			`operation="open"`, `operation="file_inherit"`, 1), "", false},
		{"a denial without a path is not blamed", strings.Replace(denial1, ` name="/var/lib/kea/kea-leases4.csv"`, "", 1), "", false},
		{"a denial of the conf dir is blamed", denialFor(8, "/etc/kea/dh-itest-v4/kea-dhcp4.conf"), "", true},
		{"a denial of a lease suffix variant is blamed", denialFor(9, "/var/lib/kea/kea-leases4.csv.2"), "", true},
		{"a denial of another file in the lease dir is not", denialFor(10, "/var/lib/kea/other.db"), "", false},
		{"a sibling of the conf dir sharing its prefix is not", denialFor(11, "/etc/kea/dh-itest-v4-other/kea-dhcp4.conf"), "", false},
		{"a dac_read_search denial is blamed", capDenialFor(12, "dac_read_search"), "", true},
		{"a dac_override denial is blamed", capDenialFor(13, "dac_override"), "", true},
		{"another capability denial is not", capDenialFor(14, "net_admin"), "", false},
		{"a dac denial already in the before snapshot is not", capDenialFor(15, "dac_read_search"), capDenialFor(15, "dac_read_search"), false},
		// A complain-mode profile logs ALLOWED and permits the access.
		{
			name:   "an ALLOWED record is not a denial",
			log:    strings.Replace(denial1, `apparmor="DENIED"`, `apparmor="ALLOWED"`, 1),
			before: "", wantHit: false,
		},
		{
			name:   "a kea-lfc denial is not a kea-dhcp4 denial",
			log:    strings.Replace(denial1, `profile="kea-dhcp4"`, `profile="kea-lfc"`, 1),
			before: "", wantHit: false,
		},
		{
			name:   "a kea-dhcp4-custom denial is not a kea-dhcp4 denial",
			log:    strings.Replace(denial1, `profile="kea-dhcp4"`, `profile="kea-dhcp4-custom"`, 1),
			before: "", wantHit: false,
		},
		{
			name:   "a kea-dhcp6 denial is not a kea-dhcp4 denial",
			log:    strings.Replace(denial1, `profile="kea-dhcp4"`, `profile="kea-dhcp6"`, 1),
			before: "", wantHit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keaDenialRecord(tc.log, tc.before)
			if tc.wantHit && got == "" {
				t.Fatalf("want the record returned, got none; log:\n%s", tc.log)
			}
			if !tc.wantHit && got != "" {
				t.Fatalf("want no record, got:\n%s", got)
			}
		})
	}

	t.Run("returns the record itself, without the surrounding noise", func(t *testing.T) {
		got := keaDenialRecord("noise\n"+denial1+"\nmore noise\n", "")
		if got != denial1 {
			t.Errorf("record not returned verbatim, got:\n%s\nwant:\n%s", got, denial1)
		}
	})

	t.Run("returns the last new one when an older one is also present", func(t *testing.T) {
		newer := denialFor(2000, "/var/log/kea/kea-dhcp4.log")
		got := keaDenialRecord(older+"\n"+newer+"\n", older+"\n")
		if got != newer {
			t.Errorf("want the record logged after the snapshot, got:\n%s", got)
		}
	})
}

func TestKeaConfinementHint(t *testing.T) {
	t.Run("a denial record is stated as fact and quoted, whatever the profile list says", func(t *testing.T) {
		for _, c := range []keaConfinement{
			{mode: "enforce", listRead: true, installed: true, kernelLogRead: true, denial: denial1, logEmpty: true},
			// A container without securityfs cannot read the list but still reads the kernel log (#680).
			{listRead: false, kernelLogRead: true, denial: denial1, logEmpty: true},
		} {
			got := keaConfinementHint(c)
			if got == "" {
				t.Fatalf("a denial produced no hint: %+v", c)
			}
			for _, want := range []string{
				"so that is why Kea never started",
				`apparmor="DENIED"`, "/var/lib/kea/kea-leases4.csv", "/var/log/kea/kea-dhcp4.log",
				"apparmor_parser -C -r", "test/integration/README.md",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("hint does not mention %q:\n%s", want, got)
				}
			}
		}
	})

	// The shipped profile ends in `#include <local/usr.sbin.kea-dhcp4>`, so a site override can permit these paths while
	// the profile still enforces (#869).
	t.Run("enforce without a denial record does not assert causation", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "enforce", listRead: true, installed: true, kernelLogRead: true, logEmpty: true,
		})
		if got == "" {
			t.Fatal("a loaded enforcing profile is still worth reporting")
		}
		if strings.Contains(got, "so that is why Kea never started") {
			t.Errorf("causation asserted with no denial record measured:\n%s", got)
		}
		if !strings.Contains(got, "candidate cause") {
			t.Errorf("want the claim downgraded to a candidate:\n%s", got)
		}
		if !strings.Contains(got, "does\n  not clear AppArmor") {
			t.Errorf("want the absent record marked non-exculpatory:\n%s", got)
		}
	})

	t.Run("an unreadable kernel log says so and does not claim a search", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{mode: "enforce", listRead: true, installed: true, logEmpty: true})
		if strings.Contains(got, "No kernel denial record") {
			t.Errorf("claims a search that never happened:\n%s", got)
		}
		if !strings.Contains(got, "kernel log could not be read") {
			t.Errorf("want the unreadable kernel log stated:\n%s", got)
		}
	})

	t.Run("the empty-log claim tracks the log", func(t *testing.T) {
		const claim = "which is why the log above is empty"
		full := keaConfinement{
			mode: "enforce", listRead: true, installed: true, kernelLogRead: true, denial: denial1,
		}
		full.logEmpty = true
		if got := keaConfinementHint(full); !strings.Contains(got, claim) {
			t.Errorf("empty log: want the claim, got:\n%s", got)
		}
		full.logEmpty = false
		if got := keaConfinementHint(full); strings.Contains(got, claim) {
			t.Errorf("non-empty log: the hint claims it is empty:\n%s", got)
		}
		noRecord := keaConfinement{mode: "enforce", listRead: true, installed: true, kernelLogRead: true, logEmpty: true}
		if got := keaConfinementHint(noRecord); strings.Contains(got, claim) {
			t.Errorf("the candidate tier explains an empty log it has no evidence about:\n%s", got)
		}
	})

	t.Run("complain is not a cause", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{mode: "complain", listRead: true, installed: true, kernelLogRead: true})
		if got != "" {
			t.Errorf("complain mode must not be reported as the cause, got:\n%s", got)
		}
	})

	// The suite runs as root, so the root-only profile list reads fine and an empty mode means not loaded (#869).
	t.Run("read fine and not loaded says nothing, even with a profile on disk", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{listRead: true, installed: true, kernelLogRead: true})
		if got != "" {
			t.Errorf("a profile that is installed but measurably NOT loaded is not a cause, got:\n%s", got)
		}
	})

	t.Run("read fine, not loaded, nothing installed says nothing", func(t *testing.T) {
		if got := keaConfinementHint(keaConfinement{listRead: true}); got != "" {
			t.Errorf("want empty hint with no profile, got:\n%s", got)
		}
	})

	t.Run("installed-but-unknown is hedged, not asserted", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{installed: true, kernelLogRead: true})
		if got == "" {
			t.Fatal("a profile on disk should still produce a hint")
		}
		if !strings.Contains(got, "If it is loaded in enforce mode") {
			t.Errorf("weaker tier must be hedged, got:\n%s", got)
		}
		if strings.Contains(got, "that is why Kea") || strings.Contains(got, "candidate cause") {
			t.Errorf("weaker tier must not assert causation, got:\n%s", got)
		}
		if !strings.Contains(got, "could not read") {
			t.Errorf("the hedged tier should say the list was unreadable, got:\n%s", got)
		}
		if strings.Contains(got, "kernel log could not be read either") {
			t.Errorf("claims an unreadable kernel log that was read, got:\n%s", got)
		}
	})

	t.Run("installed-but-unknown with an unreadable kernel log says both", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{installed: true})
		if !strings.Contains(got, "kernel log could not be read either") {
			t.Errorf("want the unreadable kernel log stated, got:\n%s", got)
		}
	})

	t.Run("unreadable list and nothing installed says nothing", func(t *testing.T) {
		if got := keaConfinementHint(keaConfinement{kernelLogRead: true}); got != "" {
			t.Errorf("want empty hint, got:\n%s", got)
		}
	})
}

// stockKeaDirs is the five directories on a Debian host where the packaged service ran once: _kea:_kea 0750 (#680).
func stockKeaDirs() []keaDirState {
	out := kea4Default.states()
	for i := range out {
		out[i].owner, out[i].uid, out[i].gid, out[i].mode = "_kea:_kea", 107, 107, 0o750
	}
	return out
}

func rootOwns(s *keaDirState) { s.owner, s.uid, s.gid, s.mode = "root:root", 0, 0, 0o755 }

// withDirs applies fix to the directories named by path; the key "" means every directory.
func withDirs(dirs []keaDirState, fixes map[string]func(*keaDirState)) []keaDirState {
	for i := range dirs {
		for path, fix := range fixes {
			if path == "" || path == dirs[i].path {
				fix(&dirs[i])
			}
		}
	}
	return dirs
}

// stockDirHintWants is what a hint over dirs must say for each: the path with owner and mode, and the fix command.
func stockDirHintWants(dirs []keaDirState) []string {
	want := []string{"so that is why Kea never started", `capname="dac_read_search"`, "_kea:_kea 0750"}
	for _, d := range dirs {
		want = append(want, d.path+"  _kea:_kea 0750", "sudo install -d -o root -g root -m 0755 "+d.path+"\n")
	}
	return want
}

func TestAppArmorKeaHint(t *testing.T) {
	writeTemp := func(t *testing.T, name, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	absent := filepath.Join(t.TempDir(), "definitely-not-here")
	older := denialFor(1000, "/var/lib/kea/kea-leases4.csv")

	tests := []struct {
		name                string
		profiles            string
		profilesUnreadable  bool
		profileFile         bool
		before              string
		beforeUnread        bool
		kernelLog           string
		kernelLogUnreadable bool
		logEmpty            bool
		dirs                []keaDirState
		wantContains        []string
		wantNotContains     []string
		wantEmpty           bool
	}{
		{
			name:         "enforce with a new denial is stated as the cause",
			profiles:     sampleProfiles,
			profileFile:  true,
			kernelLog:    denial1,
			logEmpty:     true,
			wantContains: []string{"so that is why Kea never started", `apparmor="DENIED"`},
		},
		{
			name:               "a new denial is stated even when the profile list is unreadable",
			profilesUnreadable: true,
			kernelLog:          denial1,
			logEmpty:           true,
			wantContains:       []string{"so that is why Kea never started", denial1},
		},
		{
			name:            "the only new denial is a file_inherit on an unrelated path: no AppArmor blame",
			profiles:        sampleProfiles,
			profileFile:     true,
			kernelLog:       strings.Replace(denialFor(7, "/tmp/dh-itest-ephemeral-1/dhcp-server.log"), `operation="open"`, `operation="file_inherit"`, 1),
			logEmpty:        true,
			wantContains:    []string{"candidate cause", "If it is the cause, the profile loaded on this host differs"},
			wantNotContains: []string{"so that is why Kea never started", "Therefore", "file_inherit"},
		},
		{
			name:         "a dac_read_search line on a stock host names all five 0750 _kea directories and a fix for each",
			profiles:     sampleProfiles,
			profileFile:  true,
			kernelLog:    capDenialFor(16, "dac_read_search"),
			logEmpty:     true,
			dirs:         stockKeaDirs(),
			wantContains: stockDirHintWants(stockKeaDirs()),
			wantNotContains: []string{
				"differs from the packaged", "candidate cause", "None of the directories", "chmod 0755 /etc/kea",
			},
		},
		{
			name:        "a dac_override line with only two directories closed names those two and no other",
			profiles:    sampleProfiles,
			profileFile: true,
			kernelLog:   capDenialFor(17, "dac_override"),
			logEmpty:    true,
			dirs: withDirs(stockKeaDirs(), map[string]func(*keaDirState){
				"/etc/kea": rootOwns, "/var/log/kea": rootOwns, "/run/lock/kea": rootOwns,
			}),
			wantContains:    []string{"/var/lib/kea  _kea:_kea 0750", "/run/kea  _kea:_kea 0750", "-m 0755 /var/lib/kea", "-m 0755 /run/kea"},
			wantNotContains: []string{"/etc/kea  ", "/var/log/kea  ", "/run/lock/kea  ", "-m 0755 /etc/kea", "None of the directories"},
		},
		{
			name:         "a dac line with every directory open to root says the cause is elsewhere and offers no fix",
			profiles:     sampleProfiles,
			profileFile:  true,
			kernelLog:    capDenialFor(18, "dac_read_search"),
			logEmpty:     true,
			dirs:         withDirs(stockKeaDirs(), map[string]func(*keaDirState){"": rootOwns}),
			wantContains: []string{"so that is why Kea never started", "None of the directories Kea uses is closed to root"},
			wantNotContains: []string{
				"install -d", "differs from the packaged", "candidate cause",
			},
		},
		{
			name:            "no before snapshot: a denial cannot be told from an old one, so none is blamed",
			profiles:        sampleProfiles,
			profileFile:     true,
			beforeUnread:    true,
			kernelLog:       denial1,
			logEmpty:        true,
			wantContains:    []string{"candidate cause", "kernel log could not be read"},
			wantNotContains: []string{"so that is why Kea never started"},
		},
		{
			name:        "enforce with no denial in the kernel log is a candidate only",
			profiles:    sampleProfiles,
			profileFile: true,
			kernelLog:   "some unrelated kernel noise\n",
			logEmpty:    true,
			wantContains: []string{
				"candidate cause",
				"No kernel denial record logged since the fixture started Kea",
			},
			wantNotContains: []string{"so that is why Kea never started"},
		},
		{
			name:                "enforce with an unreadable kernel log says so",
			profiles:            sampleProfiles,
			profileFile:         true,
			kernelLogUnreadable: true,
			logEmpty:            true,
			wantContains:        []string{"kernel log could not be read"},
			wantNotContains: []string{
				"so that is why Kea never started",
				"No kernel denial record",
			},
		},
		{
			name:            "a denial from before the fixture started Kea is not this run's",
			profiles:        sampleProfiles,
			profileFile:     true,
			before:          older + "\n",
			kernelLog:       older + "\n",
			logEmpty:        true,
			wantContains:    []string{"candidate cause"},
			wantNotContains: []string{"so that is why Kea never started"},
		},
		{
			name:        "complain is not a cause, so no hint",
			profiles:    "kea-dhcp4 (complain)\n",
			profileFile: true,
			wantEmpty:   true,
		},
		{
			name:        "read fine and not loaded says nothing, even with the profile on disk",
			profiles:    profilesWithoutKea,
			profileFile: true,
			wantEmpty:   true,
		},
		{
			// /sys/kernel/security is root-only.
			name:               "unreadable profiles but installed profile is hedged",
			profilesUnreadable: true,
			profileFile:        true,
			wantContains:       []string{"If it is loaded in enforce mode", "could not read"},
		},
		{
			name:               "no profile installed and none loaded says nothing",
			profilesUnreadable: true,
			wantEmpty:          true,
		},
		{
			name:            "a non-empty log is not blamed on AppArmor",
			profiles:        sampleProfiles,
			profileFile:     true,
			kernelLog:       denial1,
			logEmpty:        false,
			wantContains:    []string{"so that is why Kea never started"},
			wantNotContains: []string{"which is why the log above is empty"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origProfiles, origKea, origKernel, origDirs := apparmorProfilesPath, keaProfilePath, readKernelLog, readKeaDirs
			t.Cleanup(func() {
				apparmorProfilesPath, keaProfilePath, readKernelLog, readKeaDirs = origProfiles, origKea, origKernel, origDirs
			})
			readKeaDirs = func() []keaDirState { return tc.dirs }

			if tc.profilesUnreadable {
				apparmorProfilesPath = absent
			} else {
				apparmorProfilesPath = writeTemp(t, "profiles", tc.profiles)
			}
			if tc.profileFile {
				keaProfilePath = writeTemp(t, "usr.sbin.kea-dhcp4", "# profile\n")
			} else {
				keaProfilePath = absent
			}
			kernelLog, kernelLogUnreadable := tc.kernelLog, tc.kernelLogUnreadable
			readKernelLog = func() (string, error) {
				if kernelLogUnreadable {
					return "", os.ErrPermission
				}
				return kernelLog, nil
			}

			got := appArmorKeaHint(tc.before, !tc.beforeUnread, tc.logEmpty)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("want no hint, got:\n%s", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("want a hint containing %q, got none", tc.wantContains)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("hint missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.wantNotContains {
				if strings.Contains(got, unwanted) {
					t.Fatalf("hint must not contain %q:\n%s", unwanted, got)
				}
			}
		})
	}
}

const bothProfiles = `kea-dhcp4 (enforce)
kea-dhcp6 (complain)
kea-lfc (enforce)
`

func TestKea6ProfileModeIsNotTheDhcp4Mode(t *testing.T) {
	if got := kea6ProfileMode(bothProfiles); got != "complain" {
		t.Errorf("kea6ProfileMode = %q, want complain", got)
	}
	if got := keaProfileMode(bothProfiles); got != "enforce" {
		t.Errorf("keaProfileMode = %q, want enforce: the dhcp6 line must not be read as dhcp4", got)
	}
	for _, none := range []string{"", "kea-dhcp4 (enforce)\n", "kea-dhcp6-custom (enforce)\n", "kea-lfc (enforce)\n"} {
		if got := kea6ProfileMode(none); got != "" {
			t.Errorf("kea6ProfileMode(%q) = %q, want not loaded", none, got)
		}
	}
}

func TestKea6DenialRecordNamesOnlyTheDhcp6Profile(t *testing.T) {
	log := `audit: apparmor="DENIED" operation="open" profile="kea-dhcp4" name="/tmp/x"` + "\n" +
		`audit: apparmor="DENIED" operation="mknod" profile="kea-dhcp6" name="/var/lib/kea/other.csv"` + "\n" +
		`audit: apparmor="ALLOWED" operation="open" profile="kea-dhcp6" name="/etc/kea/a"` + "\n"
	got := kea6DenialRecord(log)
	if !strings.Contains(got, "/var/lib/kea/other.csv") {
		t.Errorf("kea6DenialRecord = %q, want the dhcp6 denial", got)
	}
	if kea6DenialRecord(`apparmor="DENIED" profile="kea-dhcp4" name="/tmp/x"`) != "" {
		t.Error("a dhcp4 denial was read as a dhcp6 denial")
	}
}

func TestKea6ConfinementHint(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    kea6Confinement
		want string
	}{
		{"enforce with a denial", kea6Confinement{mode: "enforce", listRead: true, kernelLogRead: true, denial: "DENIED x"}, "DENIED x"},
		{"enforce, log unreadable", kea6Confinement{mode: "enforce", listRead: true}, "could not be read"},
		{"enforce, nothing logged", kea6Confinement{mode: "enforce", listRead: true, kernelLogRead: true}, "does not clear it"},
		{"list unreadable, profile installed", kea6Confinement{installed: true}, "could not be read"},
		{"complain says nothing", kea6Confinement{mode: "complain", listRead: true}, ""},
		{"not loaded says nothing", kea6Confinement{listRead: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := kea6ConfinementHint(tc.c)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("hint = %q, want it to contain %q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		c    kea6Confinement
		want string
	}{
		{kea6Confinement{mode: "complain", listRead: true}, "complain"},
		{kea6Confinement{listRead: true}, "not loaded"},
		{kea6Confinement{}, "unreadable"},
	} {
		if got := tc.c.String(); !strings.Contains(got, tc.want) {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestRootCannotUse(t *testing.T) {
	kea := func(mode os.FileMode, write bool) keaDirState {
		return keaDirState{uid: 107, gid: 107, mode: mode, write: write}
	}
	tests := []struct {
		name string
		s    keaDirState
		want bool
	}{
		{"stock _kea 0750 conf dir", kea(0o750, false), true},
		{"stock _kea 0750 write dir", kea(0o750, true), true},
		{"_kea 0755 conf dir: other bits give root x", kea(0o755, false), false},
		{"_kea 0755 write dir: other bits give root no w", kea(0o755, true), true},
		{"_kea 0777 write dir", kea(0o777, true), false},
		{"_kea 0700 conf dir", kea(0o700, false), true},
		{"root 0750 write dir: owner bits", keaDirState{mode: 0o750, write: true}, false},
		{"root 0555 write dir: owner has no w", keaDirState{mode: 0o555, write: true}, true},
		{"root 0555 conf dir", keaDirState{mode: 0o555}, false},
		{"root 0055 conf dir: owner bits win over the open group and other bits", keaDirState{mode: 0o055}, true},
		{"gid 0 owned by _kea, 0070: group bits", keaDirState{uid: 107, mode: 0o070, write: true}, false},
		{"gid 0 owned by _kea, 0750: group r-x lacks w", keaDirState{uid: 107, mode: 0o750, write: true}, true},
		{"gid 0 owned by _kea, 0705: other bits are not read", keaDirState{uid: 107, mode: 0o705}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rootCannotUse(tc.s); got != tc.want {
				t.Errorf("rootCannotUse(%+v) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

// The advised command must leave no closed directory closed: the model of what it sets is checked against the predicate.
func TestKeaDirFixGivesRootEveryDirectory(t *testing.T) {
	for _, s := range stockKeaDirs() {
		fix := keaDirFix(s)
		if want := "sudo install -d -o root -g root -m 0755 " + s.path; fix != want {
			t.Errorf("fix for %s = %q, want %q", s.path, fix, want)
		}
		after := s
		rootOwns(&after)
		if rootCannotUse(after) {
			t.Errorf("a root-owned 0755 %s is still closed to root Kea", s.path)
		}
	}
}

func TestKeaDirStatesAreTheFixtureDirectories(t *testing.T) {
	var got []string
	for _, s := range kea4Default.states() {
		got = append(got, fmt.Sprintf("%s write=%v", s.path, s.write))
	}
	want := []string{"/etc/kea write=false", "/var/lib/kea write=true", "/var/log/kea write=true",
		"/run/kea write=true", "/run/lock/kea write=true"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("states = %v, want %v", got, want)
	}
}

func TestStatKeaDirs_ReadsModeAndSkipsWhatIsAbsent(t *testing.T) {
	root := t.TempDir()
	d := kea4Dirs{conf: filepath.Join(root, "etc", "dh-itest-v4"), lease: filepath.Join(root, "lease"),
		log: filepath.Join(root, "log"), pid: filepath.Join(root, "pid"), lock: filepath.Join(root, "lock")}
	for _, p := range []string{filepath.Dir(d.conf), d.lease} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	got := statKeaDirs(d)
	if len(got) != 2 || got[0].path != filepath.Dir(d.conf) || got[1].path != d.lease {
		t.Fatalf("statKeaDirs = %+v, want the two directories that exist, conf parent first", got)
	}
	for _, s := range got {
		if s.mode.Perm() != 0o750 || s.uid != uint32(os.Getuid()) || s.gid != uint32(os.Getgid()) || s.owner == "" {
			t.Errorf("%s read as %+v, want mode 0750 owned by the test user", s.path, s)
		}
	}
}
