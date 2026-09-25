// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fatalRecorder is a V6FixtureT that records Fatalf and keeps running.
type fatalRecorder struct {
	fatals []string
}

func (r *fatalRecorder) Helper()                   {}
func (r *fatalRecorder) Logf(string, ...any)       {}
func (r *fatalRecorder) Cleanup(func())            {}
func (r *fatalRecorder) Fatalf(f string, a ...any) { r.fatals = append(r.fatals, fmt.Sprintf(f, a...)) }

// TestAwaitServerRA_CountsAnAdvertisementSentDuringTheReadinessWait checks that an RA before EvidenceStartedAt is found.
func TestAwaitServerRA_CountsAnAdvertisementSentDuringTheReadinessWait(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "dnsmasq.log")
	if err := os.WriteFile(logFile, []byte(raLogToken+"dh-itest-br6) fd00:6470:6865::\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Second)
	ra := RAFrame{At: started.Add(33 * time.Millisecond)}
	rec := &fatalRecorder{}
	f := &V6Fixture{
		t:                 rec,
		mode:              V6SLAAC,
		logFile:           logFile,
		startedAt:         started,
		evidenceStartedAt: started.Add(72 * time.Millisecond),
		raCap:             &RACapture{t: rec, iface: V6BridgeName, frames: []RAFrame{ra}},
	}
	if got := f.raCap.FramesAfter(f.EvidenceStartedAt()); len(got) != 0 {
		t.Fatalf("the case is not the lane's: %d frame(s) after the readiness wait, want 0", len(got))
	}

	frames := f.AwaitServerRA(200 * time.Millisecond)
	if len(rec.fatals) != 0 {
		t.Fatalf("AwaitServerRA failed a segment that advertised during the readiness wait: %s", rec.fatals[0])
	}
	if len(frames) != 1 || !frames[0].At.Equal(ra.At) {
		t.Fatalf("AwaitServerRA returned %v, want the one frame at %s", frames, ra.At.Format("15:04:05.000"))
	}
}
