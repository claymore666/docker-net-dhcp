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
// kea-dhcp4 one transitions to; a prefix match would read it as the
// one we care about.
const sampleProfiles = `/usr/bin/man (enforce)
kea-dhcp4 (enforce)
kea-lfc (enforce)
man_filter (enforce)
`

func TestKeaProfileMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		profiles string
		want     string
	}{
		{"enforce", sampleProfiles, "enforce"},
		{"complain", "kea-dhcp4 (complain)\n", "complain"},
		{"not loaded", "/usr/bin/man (enforce)\nkea-lfc (enforce)\n", ""},
		{"empty", "", ""},
		// kea-lfc must not satisfy a lookup for kea-dhcp4. Without an
		// exact-name match this returns "enforce" and the fixture
		// blames a profile that is not confining it.
		{"lfc alone is not kea-dhcp4", "kea-lfc (enforce)\n", ""},
		// A profile whose name merely starts with ours, same reason.
		{"longer name is not a match", "kea-dhcp4-custom (enforce)\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keaProfileMode(tc.profiles); got != tc.want {
				t.Errorf("keaProfileMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeaConfinementHint(t *testing.T) {
	const dir = "/tmp/dh-itest-ephemeral-123"

	t.Run("enforce states it as fact and names the fixture dir", func(t *testing.T) {
		got := keaConfinementHint("enforce", true, dir)
		if got == "" {
			t.Fatal("enforce mode produced no hint; that is the case this exists for")
		}
		// The README section is the point of the hint: the workaround
		// was already documented, what was missing was the link to it
		// at the moment the failure happens.
		for _, want := range []string{
			"enforce mode", "/etc/kea/**", dir,
			"apparmor_parser -C -r", "test/integration/README.md",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("hint does not mention %q:\n%s", want, got)
			}
		}
	})

	// A complain-mode profile logs denials but PERMITS the access, so
	// it cannot be why Kea failed. Naming it would send the reader
	// after a red herring while the real cause went unexplained.
	t.Run("complain is not a cause", func(t *testing.T) {
		if got := keaConfinementHint("complain", true, dir); got != "" {
			t.Errorf("complain mode must not be reported as the cause, got:\n%s", got)
		}
	})

	t.Run("no profile at all says nothing", func(t *testing.T) {
		if got := keaConfinementHint("", false, dir); got != "" {
			t.Errorf("want empty hint with no profile, got:\n%s", got)
		}
	})

	// Unreadable profile list plus a profile on disk is a GUESS, and
	// must not be worded as a finding. Guarding the wording matters:
	// the whole point of this diagnostic is that the reader trusts it.
	t.Run("installed-but-unknown is hedged, not asserted", func(t *testing.T) {
		got := keaConfinementHint("", true, dir)
		if got == "" {
			t.Fatal("a profile on disk should still produce a hint")
		}
		if !strings.Contains(got, "If it is loaded in enforce mode") {
			t.Errorf("weaker tier must be hedged, got:\n%s", got)
		}
		if strings.Contains(got, "and that is why Kea") {
			t.Errorf("weaker tier must not assert causation, got:\n%s", got)
		}
	})
}

// TestAppArmorKeaHint drives the composition itself: two filesystem
// reads picked apart into the mode/installed pair that selects a tier.
// keaConfinementHint is tested above on that pair directly, but nothing
// exercised the step that PRODUCES it, so a read wired to the wrong
// path or a stat whose error was inverted would have gone unnoticed --
// and would have shown up as the hint being silent on precisely the
// hosts it exists for.
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
		name         string
		profiles     string // "" means point at an absent path
		profileFile  bool
		wantContains string
		wantEmpty    bool
	}{
		{
			name:        "enforce is stated as the cause",
			profiles:    sampleProfiles,
			profileFile: true,
			// Assert on a string ONLY the enforce tier produces. runDir
			// appears in the hedged tier too, so asserting on it let
			// this case pass while the fixture was actually selecting
			// the wrong tier -- which is exactly what happened on the
			// first draft of this test.
			wantContains: "is loaded in enforce mode, and that is why Kea",
		},
		{
			name:        "complain is not a cause, so no hint",
			profiles:    "kea-dhcp4 (complain)\n",
			profileFile: true,
			wantEmpty:   true,
		},
		{
			// The tier that exists because /sys/kernel/security is
			// root-only: unreadable profiles, profile package present.
			name:         "unreadable profiles but installed profile is hedged",
			profiles:     "",
			profileFile:  true,
			wantContains: "If it is loaded in enforce mode",
		},
		{
			name:      "no profile installed and none loaded says nothing",
			profiles:  "",
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origProfiles, origKea := apparmorProfilesPath, keaProfilePath
			t.Cleanup(func() { apparmorProfilesPath, keaProfilePath = origProfiles, origKea })

			if tc.profiles == "" {
				apparmorProfilesPath = absent
			} else {
				apparmorProfilesPath = writeTemp(t, "profiles", tc.profiles)
			}
			if tc.profileFile {
				keaProfilePath = writeTemp(t, "usr.sbin.kea-dhcp4", "# profile\n")
			} else {
				keaProfilePath = absent
			}

			got := appArmorKeaHint(runDir)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("want no hint, got:\n%s", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("want a hint containing %q, got none", tc.wantContains)
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Fatalf("hint missing %q:\n%s", tc.wantContains, got)
			}
		})
	}
}
