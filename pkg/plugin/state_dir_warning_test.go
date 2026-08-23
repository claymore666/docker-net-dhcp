// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// captureWarnings collects the WARN-and-above lines emitted while fn
// runs, so a startup warning can be asserted on rather than eyeballed.
func captureWarnings(t *testing.T, fn func()) []string {
	t.Helper()
	var buf strings.Builder
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})
	fn()
	var lines []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// Every durability claim in this package is conditional on STATE_DIR
// being the directory config.json bind-mounts from the host. Repointed,
// the options file no longer crosses versions and the schema version
// stamped on it guards nothing -- with no signal until an upgrade months
// later takes the lot (#724).
func TestStateDirWarning_FiresWhenRepointed(t *testing.T) {
	withStateDir(t, t.TempDir())

	lines := captureWarnings(t, warnIfStateDirIsNotThePersistentOne)

	if len(lines) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %v", len(lines), lines)
	}
	// It has to name the consequence, not just the mismatch. A warning
	// that says "STATE_DIR differs" tells an operator nothing they did
	// not already know when they set it.
	for _, want := range []string{"upgrade", "guards nothing", stateDir, manifestStateDir} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the warning does not mention %q: %s", want, lines[0])
		}
	}
}

// And it must stay silent on every ordinary install, or it is noise that
// gets filtered and then does not fire when it matters.
func TestStateDirWarning_SilentOnTheDefault(t *testing.T) {
	withStateDir(t, manifestStateDir)

	if lines := captureWarnings(t, warnIfStateDirIsNotThePersistentOne); len(lines) != 0 {
		t.Errorf("the default STATE_DIR warned: %v", lines)
	}
}

// The default itself must BE the manifest's path. If the two ever drift,
// every install warns and the warning stops meaning anything -- and
// check-plugin-bind-sources.sh holds the manifest to the installers, not
// to this constant.
func TestStateDirDefault_MatchesTheManifest(t *testing.T) {
	if manifestStateDir != "/var/lib/net-dhcp" {
		t.Errorf("manifestStateDir = %q; config.json declares /var/lib/net-dhcp as both "+
			"source and destination of the rbind rw mount", manifestStateDir)
	}
}
