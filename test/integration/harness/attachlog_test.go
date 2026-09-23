// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"testing"
	"time"
)

// Real logrus text output shape: a value with a space is quoted, one without is bare.
const (
	goodLine = `time="2026-09-11T08:04:06Z" level=debug msg="Attach completed" endpoint=ep-0123 ` +
		`network=net-0123 phase_total=1.23s phases="netns=120ms dhcp=1.1s" took=1.234s`
	noPhasesLine = `time="2026-09-11T08:04:06Z" level=debug msg="Attach completed" endpoint=ep-0124 ` +
		`network=net-0123 phase_total=0s phases= took=980ms`
	noPhaseCompletedLine = `time="2026-09-11T08:04:06Z" level=debug msg="Attach completed" endpoint=ep-0125 ` +
		`network=net-0123 phase_total=0s phases="no phase completed" took=12ms`
	unparsableLine = `time="2026-09-11T08:04:06Z" level=debug msg="Attach completed" endpoint=ep-0126 ` +
		`network=net-0123 phases="netns=1ms" took=soon`
	otherLine = `time="2026-09-11T08:04:06Z" level=info msg="Lease bound" endpoint=ep-0127 took=4s`
)

func TestAttachDurations_ReadsTheQuotedAndTheBareForm(t *testing.T) {
	got := AttachDurations(goodLine + "\n" + noPhasesLine + "\n" + otherLine + "\n")
	if len(got) != 2 {
		t.Fatalf("read %d durations from two attach lines and one unrelated line, want 2: %v", len(got), got)
	}
	if got[0] != 1234*time.Millisecond || got[1] != 980*time.Millisecond {
		t.Errorf("durations %v, want [1.234s 980ms]", got)
	}
}

func TestAttachDurations_IgnoresOtherLinesThatCarryATook(t *testing.T) {
	if got := AttachDurations(otherLine + "\n"); len(got) != 0 {
		t.Errorf("a non-attach line carrying took= produced %v, want none", got)
	}
}

func TestAttachLinesWithoutPhases_CountsEmptyAndTheNoPhaseText(t *testing.T) {
	log := goodLine + "\n" + noPhasesLine + "\n" + noPhaseCompletedLine + "\n"
	if got := AttachLinesWithoutPhases(log); got != 2 {
		t.Errorf("counted %d attach lines without phases, want 2", got)
	}
}

func TestAttachLinesWithoutPhases_AQuotedSummaryIsPresent(t *testing.T) {
	if got := AttachLinesWithoutPhases(goodLine + "\n"); got != 0 {
		t.Errorf("a line carrying phases=\"netns=120ms dhcp=1.1s\" counted as missing (%d)", got)
	}
}

func TestMalformedAttachLines_CountsWhatTheDurationsDropped(t *testing.T) {
	log := goodLine + "\n" + unparsableLine + "\n"
	if got := MalformedAttachLines(log); got != 1 {
		t.Errorf("counted %d unparsable attach lines, want 1", got)
	}
	if got := AttachDurations(log); len(got) != 1 {
		t.Errorf("durations %v, want only the parsable one: a dropped line must be counted "+
			"somewhere, or the figures are over a subset nobody declared", got)
	}
}

func TestPercentile_NearestRankAndTheEmptyCase(t *testing.T) {
	sorted := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}
	for _, c := range []struct {
		p    int
		want string
	}{{50, "2s"}, {90, "4s"}, {99, "4s"}, {100, "4s"}} {
		if got := Percentile(sorted, c.p); got != c.want {
			t.Errorf("p%d = %s, want %s", c.p, got, c.want)
		}
	}
	if got := Percentile(nil, 50); got != "n/a" {
		t.Errorf("p50 over an empty slice = %s, want n/a: a percentile over nothing must not "+
			"render as a number", got)
	}
}
