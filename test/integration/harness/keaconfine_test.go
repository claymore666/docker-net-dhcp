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

// denial1 is a denial in the shape #680 measured on the affected host: the lock file the default dir would have used.
var denial1 = denialFor(1884, "/run/kea/logger_lockfile")

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
	older := denialFor(1000, "/run/kea/logger_lockfile")

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
				`apparmor="DENIED"`, "/run/kea/logger_lockfile",
				"/var/lib/kea/kea-leases4.csv", "/var/log/kea/kea-dhcp4.log",
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
	older := denialFor(1000, "/run/kea/logger_lockfile")

	tests := []struct {
		name                string
		profiles            string
		profilesUnreadable  bool
		profileFile         bool
		before              string
		kernelLog           string
		kernelLogUnreadable bool
		logEmpty            bool
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
			origProfiles, origKea, origKernel := apparmorProfilesPath, keaProfilePath, readKernelLog
			t.Cleanup(func() {
				apparmorProfilesPath, keaProfilePath, readKernelLog = origProfiles, origKea, origKernel
			})

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

			got := appArmorKeaHint(tc.before, tc.logEmpty)
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
