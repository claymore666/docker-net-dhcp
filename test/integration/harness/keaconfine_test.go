// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
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
const sampleDenialFmt = `[62803.914006] audit: type=1400 audit(1787871229.672:1884): apparmor="DENIED" ` +
	`operation="open" class="file" profile="kea-dhcp4" name="%s/kea.json" pid=386894 ` +
	`comm="kea-dhcp4" requested_mask="r" denied_mask="r" fsuid=0 ouid=0`

func denialFor(runDir string) string {
	return strings.Replace(sampleDenialFmt, "%s", runDir, 1)
}

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
	const runDir = "/tmp/dh-itest-ephemeral-123"
	const other = "/tmp/dh-itest-ephemeral-999"

	for _, tc := range []struct {
		name    string
		log     string
		runDir  string
		wantHit bool
	}{
		{"a denial naming this fixture dir", denialFor(runDir), runDir, true},
		{"empty kernel log", "", runDir, false},
		// A complain-mode profile logs ALLOWED and permits the access.
		{
			name:   "an ALLOWED record is not a denial",
			log:    strings.Replace(denialFor(runDir), `apparmor="DENIED"`, `apparmor="ALLOWED"`, 1),
			runDir: runDir, wantHit: false,
		},
		{"a denial against another directory", denialFor(other), runDir, false},
		{
			name:   "a kea-lfc denial is not a kea-dhcp4 denial",
			log:    strings.Replace(denialFor(runDir), `profile="kea-dhcp4"`, `profile="kea-lfc"`, 1),
			runDir: runDir, wantHit: false,
		},
		{
			name:   "a kea-dhcp4-custom denial is not a kea-dhcp4 denial",
			log:    strings.Replace(denialFor(runDir), `profile="kea-dhcp4"`, `profile="kea-dhcp4-custom"`, 1),
			runDir: runDir, wantHit: false,
		},
		{"an empty runDir matches nothing", denialFor(runDir), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keaDenialRecord(tc.log, tc.runDir)
			if tc.wantHit && got == "" {
				t.Fatalf("want the record returned, got none; log:\n%s", tc.log)
			}
			if !tc.wantHit && got != "" {
				t.Fatalf("want no record, got:\n%s", got)
			}
		})
	}

	t.Run("returns the record itself", func(t *testing.T) {
		got := keaDenialRecord("noise\n"+denialFor(runDir)+"\nmore noise\n", runDir)
		if !strings.Contains(got, `apparmor="DENIED"`) || !strings.Contains(got, runDir) {
			t.Errorf("record not returned verbatim, got:\n%s", got)
		}
	})

	t.Run("returns the last of several", func(t *testing.T) {
		first := strings.Replace(denialFor(runDir), "kea.json", "kea-leases4.csv", 1)
		got := keaDenialRecord(first+"\n"+denialFor(runDir)+"\n", runDir)
		if !strings.Contains(got, "kea.json") {
			t.Errorf("want the last matching record, got:\n%s", got)
		}
	})
}

func TestKeaConfinementHint(t *testing.T) {
	const dir = "/tmp/dh-itest-ephemeral-123"

	t.Run("enforce with a denial record states it as fact and quotes the record", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: true, denial: denialFor(dir), logEmpty: true, runDir: dir,
		})
		if got == "" {
			t.Fatal("enforce mode produced no hint; that is the case this exists for")
		}
		for _, want := range []string{
			"enforce mode", "/etc/kea/**", dir,
			"so that is why Kea never started",
			`apparmor="DENIED"`,
			"apparmor_parser -C -r", "test/integration/README.md",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("hint does not mention %q:\n%s", want, got)
			}
		}
	})

	// The shipped profile ends in `#include <local/usr.sbin.kea-dhcp4>`, so a site override can permit these paths while
	// the profile still enforces (#869).
	t.Run("enforce without a denial record does not assert causation", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: true, denial: "", logEmpty: true, runDir: dir,
		})
		if got == "" {
			t.Fatal("a loaded enforcing profile is still worth reporting")
		}
		if strings.Contains(got, "so that is why Kea never started") {
			t.Errorf("causation asserted with no denial record measured:\n%s", got)
		}
		if !strings.Contains(got, "most likely") {
			t.Errorf("want the claim downgraded to a likelihood:\n%s", got)
		}
		if !strings.Contains(got, "does not clear") {
			t.Errorf("want the absent record marked non-exculpatory:\n%s", got)
		}
		if !strings.Contains(got, "site override") {
			t.Errorf("want the local-override escape named:\n%s", got)
		}
	})

	t.Run("an unreadable kernel log does not claim no record was found", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: false, denial: "", logEmpty: true, runDir: dir,
		})
		if strings.Contains(got, "No kernel denial record naming this directory was found") {
			t.Errorf("claims a search that never happened:\n%s", got)
		}
		if !strings.Contains(got, "could not be read") {
			t.Errorf("want the unreadable ring buffer stated:\n%s", got)
		}
	})

	t.Run("the empty-log claim tracks the log", func(t *testing.T) {
		const claim = "which is why the log above is empty"
		full := keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: true, denial: denialFor(dir), runDir: dir,
		}
		full.logEmpty = true
		if got := keaConfinementHint(full); !strings.Contains(got, claim) {
			t.Errorf("empty log: want the claim, got:\n%s", got)
		}
		full.logEmpty = false
		if got := keaConfinementHint(full); strings.Contains(got, claim) {
			t.Errorf("non-empty log: the hint claims it is empty:\n%s", got)
		}
		noRecord := keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: true, logEmpty: true, runDir: dir,
		}
		if got := keaConfinementHint(noRecord); strings.Contains(got, claim) {
			t.Errorf("the likely-cause tier explains an empty log it has no evidence about:\n%s", got)
		}
	})

	t.Run("complain is not a cause", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "complain", listRead: true, installed: true, runDir: dir,
		})
		if got != "" {
			t.Errorf("complain mode must not be reported as the cause, got:\n%s", got)
		}
	})

	// The suite runs as root, so the root-only profile list reads fine and an empty mode means not loaded (#869).
	t.Run("read fine and not loaded says nothing, even with a profile on disk", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "", listRead: true, installed: true, runDir: dir,
		})
		if got != "" {
			t.Errorf("a profile that is installed but measurably NOT loaded is not a cause, got:\n%s", got)
		}
	})

	t.Run("read fine, not loaded, nothing installed says nothing", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{listRead: true, runDir: dir})
		if got != "" {
			t.Errorf("want empty hint with no profile, got:\n%s", got)
		}
	})

	t.Run("installed-but-unknown is hedged, not asserted", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{installed: true, runDir: dir})
		if got == "" {
			t.Fatal("a profile on disk should still produce a hint")
		}
		if !strings.Contains(got, "If it is loaded in enforce mode") {
			t.Errorf("weaker tier must be hedged, got:\n%s", got)
		}
		if strings.Contains(got, "that is why Kea") || strings.Contains(got, "most likely") {
			t.Errorf("weaker tier must not assert causation, got:\n%s", got)
		}
		if !strings.Contains(got, "could not read") {
			t.Errorf("the hedged tier should say the list was unreadable, got:\n%s", got)
		}
	})

	t.Run("unreadable list and nothing installed says nothing", func(t *testing.T) {
		if got := keaConfinementHint(keaConfinement{runDir: dir}); got != "" {
			t.Errorf("want empty hint, got:\n%s", got)
		}
	})
}

func TestAppArmorKeaHint(t *testing.T) {
	const runDir = "/tmp/dh-itest-fixturedir"

	writeTemp := func(t *testing.T, name, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	absent := filepath.Join(t.TempDir(), "definitely-not-here")

	tests := []struct {
		name                string
		profiles            string
		profilesUnreadable  bool
		profileFile         bool
		kernelLog           string
		kernelLogUnreadable bool
		logEmpty            bool
		wantContains        []string
		wantNotContains     []string
		wantEmpty           bool
	}{
		{
			name:         "enforce with a matching denial is stated as the cause",
			profiles:     sampleProfiles,
			profileFile:  true,
			kernelLog:    denialFor(runDir),
			logEmpty:     true,
			wantContains: []string{"so that is why Kea never started", `apparmor="DENIED"`},
		},
		{
			name:        "enforce with no denial in the kernel log is the likely cause only",
			profiles:    sampleProfiles,
			profileFile: true,
			kernelLog:   "some unrelated kernel noise\n",
			logEmpty:    true,
			wantContains: []string{
				"most likely",
				"No kernel denial record naming this directory was found",
			},
			wantNotContains: []string{"so that is why Kea never started"},
		},
		{
			name:                "enforce with an unreadable kernel log says so",
			profiles:            sampleProfiles,
			profileFile:         true,
			kernelLogUnreadable: true,
			logEmpty:            true,
			wantContains:        []string{"could not be read"},
			wantNotContains: []string{
				"so that is why Kea never started",
				"No kernel denial record naming this directory was found",
			},
		},
		{
			name:            "a denial against another directory is not this fixture's",
			profiles:        sampleProfiles,
			profileFile:     true,
			kernelLog:       denialFor("/tmp/some-other-fixture"),
			logEmpty:        true,
			wantContains:    []string{"most likely"},
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
			kernelLog:       denialFor(runDir),
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

			got := appArmorKeaHint(runDir, tc.logEmpty)
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
