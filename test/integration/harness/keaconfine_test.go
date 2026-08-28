// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The kernel's profile list, as it actually looks on a Debian host with
// the kea package installed. kea-lfc is a SEPARATE profile that the
// kea-dhcp4 one transitions to; a substring match would read it as the
// one we care about.
const sampleProfiles = `/usr/bin/man (enforce)
kea-dhcp4 (enforce)
kea-lfc (enforce)
man_filter (enforce)
`

// A list that was read PERFECTLY and positively does not carry the
// kea-dhcp4 profile. This is not the same fact as "the list could not
// be read", and the whole of #869's first fix is keeping them apart.
const profilesWithoutKea = `/usr/bin/man (enforce)
kea-lfc (enforce)
man_filter (enforce)
`

// A real AppArmor denial record, measured on a Debian host by running
// the packaged kea-dhcp4 against a config inside a temp directory. Only
// the pid and the audit serial are altered. The fixture directory is
// substituted per-test so the record names the run it belongs to.
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
		// kea-lfc must not satisfy a lookup for kea-dhcp4. This case
		// kills a substring or "kea"-prefix match. It says NOTHING
		// about a "kea-dhcp4" prefix match, because kea-lfc does not
		// carry that prefix -- see the case below, which is the one
		// that kills it.
		{"lfc alone is not kea-dhcp4", "kea-lfc (enforce)\n", ""},
		// A profile whose name starts with ours. This is the case that
		// kills a kea-dhcp4 prefix match, and the only one that does.
		{"longer name is not a match", "kea-dhcp4-custom (enforce)\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keaProfileMode(tc.profiles); got != tc.want {
				t.Errorf("keaProfileMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestKeaDenialRecord covers the only outside evidence this file has.
// Everything else it reads describes the system; a denial record is the
// one thing that says THIS process was denied.
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
		// A complain-mode profile logs ALLOWED while permitting the
		// access. Reading that as a denial would resurrect the red
		// herring the complain tier exists to avoid.
		{
			name:   "an ALLOWED record is not a denial",
			log:    strings.Replace(denialFor(runDir), `apparmor="DENIED"`, `apparmor="ALLOWED"`, 1),
			runDir: runDir, wantHit: false,
		},
		// Another Kea on the host being denied says nothing about this
		// run's directory.
		{"a denial against another directory", denialFor(other), runDir, false},
		// Same anchoring doctrine as keaProfileMode: the sibling
		// profile the kea-dhcp4 one transitions to is not ours.
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
		// An empty runDir is contained in every string. Without the
		// refusal this becomes a check with one possible verdict: any
		// kea-dhcp4 denial anywhere on the host reads as proof about
		// this run.
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

	// The record is quoted verbatim into the failure message, so the
	// caller must get the whole line back, not a boolean.
	t.Run("returns the record itself", func(t *testing.T) {
		got := keaDenialRecord("noise\n"+denialFor(runDir)+"\nmore noise\n", runDir)
		if !strings.Contains(got, `apparmor="DENIED"`) || !strings.Contains(got, runDir) {
			t.Errorf("record not returned verbatim, got:\n%s", got)
		}
	})

	// A run that failed, was retried and failed again leaves several.
	// The last is the one that belongs to the attempt being reported.
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

	// enforce plus a kernel denial naming this directory is the only
	// case that measured THIS process. It is the only one allowed to
	// state causation.
	t.Run("enforce with a denial record states it as fact and quotes the record", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: true, denial: denialFor(dir), logEmpty: true, runDir: dir,
		})
		if got == "" {
			t.Fatal("enforce mode produced no hint; that is the case this exists for")
		}
		// The README section is the point of the hint: the workaround
		// was already documented, what was missing was the link to it
		// at the moment the failure happens.
		for _, want := range []string{
			"enforce mode", "/etc/kea/**", dir,
			"so that is why Kea never started",
			`apparmor="DENIED"`, // the record itself, not a summary of it
			"apparmor_parser -C -r", "test/integration/README.md",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("hint does not mention %q:\n%s", want, got)
			}
		}
	})

	// A loaded enforcing profile is a fact about the SYSTEM. It is not
	// evidence that this process was denied: the shipped profile ends
	// in `#include <local/usr.sbin.kea-dhcp4>`, so a site override can
	// permit exactly these paths with the profile still enforcing.
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
		// Absence of a record must not read as an all-clear either.
		if !strings.Contains(got, "does not clear") {
			t.Errorf("want the absent record marked non-exculpatory:\n%s", got)
		}
		if !strings.Contains(got, "site override") {
			t.Errorf("want the local-override escape named:\n%s", got)
		}
	})

	// The same collapse #869's first fix removed from the profile list,
	// one read further along: "the kernel log said no" and "the kernel
	// log could not be read" are different facts.
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

	// "which is why the log above is empty" printed under a log that is
	// visibly not empty is the reader losing trust in the whole hint.
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
		// The tier with no denial record must never make the claim, not
		// even when the log IS empty. That the log is empty is known;
		// WHY it is empty is the causal claim this tier cannot support.
		noRecord := keaConfinement{
			mode: "enforce", listRead: true, installed: true,
			kernelLogRead: true, logEmpty: true, runDir: dir,
		}
		if got := keaConfinementHint(noRecord); strings.Contains(got, claim) {
			t.Errorf("the likely-cause tier explains an empty log it has no evidence about:\n%s", got)
		}
	})

	// A complain-mode profile logs denials but PERMITS the access, so
	// it cannot be why Kea failed. Naming it would send the reader
	// after a red herring while the real cause went unexplained.
	t.Run("complain is not a cause", func(t *testing.T) {
		got := keaConfinementHint(keaConfinement{
			mode: "complain", listRead: true, installed: true, runDir: dir,
		})
		if got != "" {
			t.Errorf("complain mode must not be reported as the cause, got:\n%s", got)
		}
	})

	// THE case #869's first fix exists for, and the one that fires in
	// practice: the suite runs as root, so the root-only profile list
	// reads fine, and mode == "" then means "measured, not loaded".
	// An unloaded profile is not a cause, and claiming the list could
	// not be read is a plain falsehood.
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

	// Unreadable profile list plus a profile on disk is a GUESS, and
	// must not be worded as a finding. Guarding the wording matters:
	// the whole point of this diagnostic is that the reader trusts it.
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
		// This sentence is a statement of fact about a read that
		// failed. It may appear ONLY when the read really failed.
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

// TestAppArmorKeaHint drives the composition itself: three reads picked
// apart into the evidence that selects a tier. keaConfinementHint is
// tested above on that evidence directly, but nothing exercised the
// step that PRODUCES it, so a read wired to the wrong path, a stat
// whose error was inverted, or a read outcome folded into its value
// would have gone unnoticed -- and would have shown up as the hint
// being silent, or lying, on precisely the hosts it exists for.
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

	// A missing path, guaranteed unreadable and un-stattable.
	absent := filepath.Join(t.TempDir(), "definitely-not-here")

	tests := []struct {
		name string
		// profiles is the kernel list's CONTENT. profilesUnreadable
		// points the reader at a missing path instead. The two used to
		// share one field, with "" meaning "unreadable" -- which is the
		// same conflation the code under test had, so the table could
		// not express the case that exposed it.
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
			name:        "enforce with a matching denial is stated as the cause",
			profiles:    sampleProfiles,
			profileFile: true,
			kernelLog:   denialFor(runDir),
			logEmpty:    true,
			// Assert on a string ONLY this tier produces. runDir
			// appears in every tier, so asserting on it let this case
			// pass while the fixture was actually selecting the wrong
			// tier -- which is exactly what happened on the first draft
			// of this test.
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
			// A denial against a DIFFERENT directory is on the host's
			// kernel log for reasons of its own. Reading it as evidence
			// about this run is the failure keaDenialRecord's runDir
			// term exists to prevent, and the composition has to carry
			// the fixture's own dir into it for that term to work.
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
			// THE row the table lacked, and the reason the collapse was
			// invisible: a list that READ FINE and does not carry the
			// profile, with the profile package still on disk. The
			// suite runs as root, so this and not the unreadable case
			// is what a Debian host without the profile loaded produces.
			name:        "read fine and not loaded says nothing, even with the profile on disk",
			profiles:    profilesWithoutKea,
			profileFile: true,
			wantEmpty:   true,
		},
		{
			// The tier that exists because /sys/kernel/security is
			// root-only: unreadable profiles, profile package present.
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
			// The empty-log claim has to survive the composition too: a
			// non-empty log with a real denial still gets the hint, but
			// not the sentence about the log.
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
			// Restored overrides are what make this suite
			// host-independent: green on a kea-less runner and green on
			// a kea host. Without them the enforce cases would pass by
			// accident here and fail in CI, or the reverse.
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
			// Never the host's real dmesg: that would make the verdict
			// depend on whatever the machine happens to have logged.
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
