// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"os/exec"
	"strings"
	"testing"
)

// #780. Every test here answers the same question in a different place:
// can a reader tell the three states apart?
//
//	not wired          — the increment does not exist; the counter reads 0
//	wired, nothing refused — the increment exists; the counter reads 0
//	wired, something refused — the counter reads non-zero
//
// The first two produce an identical number, which is why each test
// below carries a POSITIVE CONTROL in the same run: an input that MUST
// move the counter. Without one, "0 when nothing happened" is satisfied
// just as well by a counter nobody ever wired, and that is the reading
// this issue exists to stop.
//
// The counters are process-global (see refusals.go), so everything here
// reads DELTAS. Absolute values would make these tests depend on which
// other test ran first.

// falseBin is a command that always fails, for driving the failure arm
// of a real mountPrepStep. Present on both Alpine (busybox) and Debian
// (coreutils).
const falseBin = "/bin/false"

// trueBin is its counterpart, for the control.
const trueBin = "/bin/true"

// testMAC is a well-formed address for renderConfig, which derives the
// DUID and IAID from it. Fixed rather than random so a failure message
// is the same on every run.
func testMAC(t *testing.T) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC("02:42:ac:11:00:02")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	return mac
}

// Site 1: a dhcpcd directive dropped for carrying a control character.
//
// Drives renderConfig — the real renderer, not directive() on its own —
// so the counter is reached the way production reaches it.
func TestRenderConfig_CountsARefusedDirective(t *testing.T) {
	// Negative arm first, so the positive arm below is a DELTA against a
	// tree that has just been shown to sit still.
	before, _, _ := RefusalCounts()
	renderConfig(dhcpcdParams{
		Iface:    "eth0",
		MAC:      testMAC(t),
		Hostname: "well-formed",
	})
	quiet, _, _ := RefusalCounts()
	if quiet != before {
		t.Errorf("directives_refused moved by %d on a config with nothing to refuse, want 0",
			quiet-before)
	}

	// Positive control. A hostname carrying a newline cannot be escaped
	// into dhcpcd.conf — it has no quoting — so it is dropped, and the
	// operator's value never reaches the DHCP server.
	cfg := renderConfig(dhcpcdParams{
		Iface:    "eth0",
		MAC:      testMAC(t),
		Hostname: "web1\nduid 00:03:00:01:be:ef:be:ef:be:ef",
	})
	after, _, _ := RefusalCounts()

	if after <= quiet {
		t.Errorf("directives_refused did not move on a refused directive (%d -> %d). "+
			"A counter that reads 0 here reads exactly the same as one that was never "+
			"wired, which is the whole complaint in #780", quiet, after)
	}
	// And the refusal really happened, so the count is about something.
	if strings.Contains(cfg, "duid 00:03:00:01:be:ef") {
		t.Error("the injected directive reached the rendered config; the counter above is " +
			"counting something other than a refusal")
	}
}

// Site 2, shape: every command mountPrep runs reports its own failure.
//
// Asserted by COUNTING rather than by naming the four that exist today.
// A fifth command added next year without a marker fails this; a test
// listing today's four would not, which is the failure mode the sibling
// TestMountPrep_NamesEveryBinaryAbsolutely was written to avoid and the
// same reasoning applies here.
func TestMountPrep_EveryCommandReportsItsFailure(t *testing.T) {
	// Both shapes: the Router-Advertisement guard (#875) appends steps
	// to this same body, and they are prepared steps under the same
	// contract.
	for _, tc := range []struct {
		name   string
		params dhcpcdParams
		marker string
	}{
		{"unguarded", unguardedPrepParams(), mountPrepFailMarker},
		{"guarded", guardedPrepParams(), raGuardFailMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prep := mountPrep(tc.params)

			// PER STATEMENT, not by counting commands against markers.
			//
			// The counting form excluded every `echo` as "the reporting
			// command", which silently exempted any prepared step whose
			// own command happened to be an echo — and #875's guard
			// writes a sysctl with exactly that. A gate that cannot see
			// a whole class of step is the blind spot this file's
			// siblings were written about; keying on "does this
			// statement carry a marker" needs no such exemption and does
			// not care which binary a step runs.
			statements := 0
			for _, stmt := range strings.Split(prep, ";") {
				if len(strings.Fields(stmt)) == 0 || strings.HasPrefix(strings.TrimSpace(stmt), "exec ") {
					continue
				}
				statements++
				if !strings.Contains(stmt, mountPrepFailMarker) &&
					!strings.Contains(stmt, raGuardFailMarker) {
					t.Errorf("prepared step %q carries no failure marker; it fails "+
						"silently and dhcpcd starts anyway\n---\n%s", strings.TrimSpace(stmt), prep)
				}
			}
			if statements == 0 {
				t.Errorf("found no prepared steps — the loop above is satisfied by an "+
					"empty domain and checks nothing\n---\n%s", prep)
			}
			// And the shape's OWN marker family must be present, so a
			// guarded body whose guard vanished cannot pass on the
			// unguarded steps' markers alone.
			if !strings.Contains(prep, tc.marker) {
				t.Errorf("no %q marker in the %v shape\n---\n%s", tc.marker, tc.name, prep)
			}
		})
	}
}

// The Router-Advertisement guard's failures reach their OWN counter, and
// not the mount-prep one.
//
// Built with the product's own step builder for the reason its sibling
// gives: a test that transcribes the marker proves the transcription.
// Two counters that are wired to the same atomic look identical from a
// single-counter test, so both are read on every arm here.
func TestRAGuardWatcher_CountsARealFailedStepSeparately(t *testing.T) {
	run := func(t *testing.T, body string) (mountPrep, raGuard int32) {
		t.Helper()
		_, mBefore, rBefore := RefusalCounts()

		var w mountPrepWatcher
		cmd := exec.Command(shBin, "-c", body)
		cmd.Stderr = &w
		_ = cmd.Run()

		_, mAfter, rAfter := RefusalCounts()
		return mAfter - mBefore, rAfter - rBefore
	}

	// Negative arm: the same real step shape with a command that
	// succeeds. One variable apart from the positive arm.
	if m, r := run(t, prepStep(trueBin, raGuardFailMarker, "accept_ra-write")); m != 0 || r != 0 {
		t.Errorf("counters moved by mount_prep=%d ra_guard=%d for a step that SUCCEEDED, want 0/0", m, r)
	}

	// Positive control, and the separation.
	if m, r := run(t, prepStep(falseBin, raGuardFailMarker, "accept_ra-write")); m != 0 || r != 1 {
		t.Errorf("counters moved by mount_prep=%d ra_guard=%d for one failed guard step, "+
			"want 0/1. A 0 in the second is indistinguishable from a counter that was "+
			"never wired; a 1 in the first means the two families share an atomic", m, r)
	}

	// The other direction: a mount-prep failure must not land on the
	// guard's counter.
	if m, r := run(t, mountPrepStep(falseBin, "state-tmpfs")); m != 1 || r != 0 {
		t.Errorf("counters moved by mount_prep=%d ra_guard=%d for one failed mount-prep "+
			"step, want 1/0", m, r)
	}

	// Counts STEPS: a guard that failed to write and to shield one knob
	// reads 2, not 1.
	body := prepStep(falseBin, raGuardFailMarker, "accept_ra-write") +
		prepStep(falseBin, raGuardFailMarker, "accept_ra-shield")
	if _, r := run(t, body); r != 2 {
		t.Errorf("ra_guard moved by %d for two failed steps, want 2", r)
	}
}

// Site 2, effect: a real failing step, through a real shell, through the
// real watcher.
//
// The step is built by mountPrepStep — the product's own builder — with
// a command that always fails, rather than by writing the expected line
// into the test. A test that transcribes the format it is checking
// proves the transcription: change the marker in the product and a
// transcribing test goes on passing while nothing counts any more.
func TestMountPrepWatcher_CountsARealFailedStep(t *testing.T) {
	run := func(t *testing.T, body string) int32 {
		t.Helper()
		_, mBefore, _ := RefusalCounts()

		var w mountPrepWatcher
		cmd := exec.Command(shBin, "-c", body)
		cmd.Stderr = &w
		// The body's exit status is not the subject — a chain of `;`
		// deliberately does not propagate one — so it is ignored rather
		// than asserted on.
		_ = cmd.Run()

		_, mAfter, _ := RefusalCounts()
		return mAfter - mBefore
	}

	// Negative arm: the same real step shape, with a command that
	// succeeds. Varies exactly one thing from the positive arm below.
	if got := run(t, mountPrepStep(trueBin, "control-step")); got != 0 {
		t.Errorf("mount_prep_failures moved by %d for a step that SUCCEEDED, want 0", got)
	}

	// Positive control.
	if got := run(t, mountPrepStep(falseBin, "state-tmpfs")); got != 1 {
		t.Errorf("mount_prep_failures moved by %d for one failed step, want 1. "+
			"A 0 here is indistinguishable from a counter that was never wired", got)
	}

	// Two failed steps count two: this counts COMMANDS, not clients, and
	// a watcher that latched on the first would read 1 for a namespace
	// that failed to prepare in every respect.
	body := mountPrepStep(falseBin, "state-tmpfs") + mountPrepStep(falseBin, "run-tmpfs")
	if got := run(t, body); got != 2 {
		t.Errorf("mount_prep_failures moved by %d for two failed steps, want 2", got)
	}
}

// The watcher must not manufacture a count from ordinary client noise.
//
// dhcpcd is chatty on stderr and every byte of it reaches this writer.
// A marker chosen loosely — "failed", say — would turn a routine
// diagnostic into a namespace-preparation failure, and an operator
// alerting on the counter would be paged for nothing.
func TestMountPrepWatcher_IgnoresOrdinaryStderr(t *testing.T) {
	_, before, _ := RefusalCounts()

	var w mountPrepWatcher
	for _, line := range []string{
		"eth0: soliciting a DHCP lease\n",
		"eth0: probing for an IPv4LL address\n",
		"eth0: failed to renew, rebinding\n",
		"dhcpcd exited\n",
	} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("watcher Write: %v", err)
		}
	}

	_, after, _ := RefusalCounts()
	if after != before {
		t.Errorf("mount_prep_failures moved by %d on ordinary dhcpcd stderr, want 0",
			after-before)
	}
}

// A marker split across two Writes still counts once.
//
// A Writer receives arbitrary chunks; exec's copy goroutine has no
// obligation to hand over whole lines. Matching per-chunk would miss
// this and, on a boundary inside one line, could count it twice.
func TestMountPrepWatcher_CountsAMarkerSplitAcrossWrites(t *testing.T) {
	_, before, _ := RefusalCounts()

	var w mountPrepWatcher
	full := mountPrepFailMarker + " state-tmpfs\n"
	for i := 1; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		if _, err := w.Write([]byte(full[i-1 : end-1])); err != nil {
			t.Fatalf("watcher Write: %v", err)
		}
	}
	if _, err := w.Write([]byte(full[len(full)-1:])); err != nil {
		t.Fatalf("watcher Write: %v", err)
	}

	_, after, _ := RefusalCounts()
	if got := after - before; got != 1 {
		t.Errorf("a marker delivered in 7-byte chunks counted %d times, want 1", got)
	}
}
