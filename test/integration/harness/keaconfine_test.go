// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
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
