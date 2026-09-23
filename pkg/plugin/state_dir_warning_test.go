// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

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

func TestStateDirWarning_FiresWhenRepointed(t *testing.T) {
	withStateDir(t, t.TempDir())

	lines := captureWarnings(t, warnIfStateDirIsNotThePersistentOne)

	if len(lines) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %v", len(lines), lines)
	}
	for _, want := range []string{"upgrade", "guards nothing", stateDir, manifestStateDir} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the warning does not mention %q: %s", want, lines[0])
		}
	}
}

func TestStateDirWarning_SilentOnTheDefault(t *testing.T) {
	withStateDir(t, manifestStateDir)

	if lines := captureWarnings(t, warnIfStateDirIsNotThePersistentOne); len(lines) != 0 {
		t.Errorf("the default STATE_DIR warned: %v", lines)
	}
}

func TestStateDirDefault_MatchesTheManifest(t *testing.T) {
	if manifestStateDir != "/var/lib/net-dhcp" {
		t.Errorf("manifestStateDir = %q; config.json declares /var/lib/net-dhcp as both "+
			"source and destination of the rbind rw mount", manifestStateDir)
	}
}
