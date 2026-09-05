// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"strings"
	"testing"
	"time"
)

// checkBumper moves exactly one of the counters a check is declared on.
//
// The table is keyed by the health field, and TestHealthChecks_
// EveryDeclaredCheckIsDriven reconciles its key set against the
// declaration in metricDefs. That reconciliation is what stops this
// file from being a universal satisfied by an empty domain: a twelfth
// check added without a bumper here fails, rather than being silently
// left undriven by every test below.
type checkBumper struct {
	field string
	bump  func(p *Plugin)
}

func checkBumpers() []checkBumper {
	return []checkBumper{
		{"recovery_failed", func(p *Plugin) { p.recoveryFailed.Add(1) }},
		{"join_start_failures", func(p *Plugin) { p.joinStartFailures.Add(1) }},
		{"tombstone_write_failures", func(p *Plugin) { p.tombstoneWriteFailures.Add(1) }},
		{"tombstone_quarantines", func(p *Plugin) { p.tombstones.quarantines.Add(1) }},
		{"address_conflicts", func(p *Plugin) { p.addressConflicts.Add(1) }},
		{"lease_changed", func(p *Plugin) { p.leaseChangedV4.Add(1) }},
		{"restart_link_up_timeouts", func(p *Plugin) { p.restartLinkUpTimeouts.Add(1) }},
		{"acd_arp_send_failures", func(p *Plugin) { p.acdARPSendFailures.Add(1) }},
		{"acd_resumed_unchecked", func(p *Plugin) { p.acdResumedUnchecked.Add(1) }},
		{"parent_link_wait_timeouts", func(p *Plugin) { p.parentLinkWaitTimeouts.Add(1) }},
		{"ledger_write_failures", func(p *Plugin) { p.ledgerWriteFailures.Add(1) }},
	}
}

// declaredChecks is the field -> status the classification says, read
// from the declaration rather than restated here.
func declaredChecks() map[string]string {
	out := map[string]string{}
	for _, d := range metricDefs() {
		switch {
		case d.healthy:
			out[d.field] = statusFail
		case d.warn:
			out[d.field] = statusWarn
		}
	}
	return out
}

func onlyCheck(t *testing.T, h HealthResponse, field string) HealthCheck {
	t.Helper()
	got, ok := h.Checks[field]
	if !ok {
		t.Fatalf("no check %q in the document; it has %d checks", field, len(h.Checks))
	}
	if len(got) != 1 {
		t.Fatalf("check %q is a %d-element array; the draft's single-element form is one entry", field, len(got))
	}
	return got[0]
}

func TestHealthChecks_EveryDeclaredCheckIsDriven(t *testing.T) {
	declared := declaredChecks()
	driven := map[string]bool{}
	for _, b := range checkBumpers() {
		if _, ok := declared[b.field]; !ok {
			t.Errorf("checkBumpers drives %q, which metricDefs does not declare as a check — "+
				"the bumper is stale or the declaration was dropped", b.field)
		}
		if driven[b.field] {
			t.Errorf("checkBumpers drives %q twice", b.field)
		}
		driven[b.field] = true
	}
	for field := range declared {
		if !driven[field] {
			t.Errorf("metricDefs declares %q as a %s check and nothing here drives it. "+
				"Add it to checkBumpers: the tests below assert on the checks they drive, "+
				"so an undriven check is an unobserved one.", field, declared[field])
		}
	}
}

// The document's `status` and the 1.x `healthy` flag are two renderings
// of one fact, and an operator reading one and alerting on the other has
// to get the same answer. Each of the five healthy-affecting counters is
// driven ALONE — a test that bumped them together would pass with four
// of the five unwired.
func TestHealthChecks_StatusAndHealthyAgreeOnEveryFailCounter(t *testing.T) {
	declared := declaredChecks()

	for _, b := range checkBumpers() {
		if declared[b.field] != statusFail {
			continue
		}
		t.Run(b.field, func(t *testing.T) {
			p := newHealthPlugin()

			clean := p.healthSnapshot()
			if !clean.Healthy || clean.Status != statusPass {
				t.Fatalf("an untouched plugin reads healthy=%v status=%q; want true/%q",
					clean.Healthy, clean.Status, statusPass)
			}

			b.bump(p)
			h := p.healthSnapshot()

			if h.Healthy {
				t.Errorf("%s moved and healthy is still true", b.field)
			}
			if h.Status != statusFail {
				t.Errorf("%s moved and status is %q; healthy says false, so the two disagree", b.field, h.Status)
			}
			if c := onlyCheck(t, h, b.field); c.Status != statusFail {
				t.Errorf("check %s is %q; a healthy-affecting counter is a fail check", b.field, c.Status)
			}
			for field, want := range declared {
				if field == b.field {
					continue
				}
				if c := onlyCheck(t, h, field); c.Status != statusPass {
					t.Errorf("check %s is %q after only %s moved; want %q (declared %s)",
						field, c.Status, b.field, statusPass, want)
				}
			}
		})
	}
}

// The other direction: a warn counter must not make the document claim
// a fault, and must not be able to hide one. A classification read
// backwards, or a worst-of that let the later check overwrite the
// earlier, shows up here and not in the test above.
func TestHealthChecks_WarnNeitherClaimsAFaultNorHidesOne(t *testing.T) {
	declared := declaredChecks()

	for _, b := range checkBumpers() {
		if declared[b.field] != statusWarn {
			continue
		}
		t.Run(b.field, func(t *testing.T) {
			p := newHealthPlugin()
			b.bump(p)
			h := p.healthSnapshot()

			if !h.Healthy {
				t.Errorf("%s moved and healthy went false; a warn counter is not a fault", b.field)
			}
			if h.Status != statusWarn {
				t.Errorf("%s moved and status is %q; want %q", b.field, h.Status, statusWarn)
			}
			if c := onlyCheck(t, h, b.field); c.Status != statusWarn {
				t.Errorf("check %s is %q; want %q", b.field, c.Status, statusWarn)
			}

			// Now a real fault beside it. Whichever order the walk
			// visits them in, fail wins.
			p.recoveryFailed.Add(1)
			h = p.healthSnapshot()
			if h.Status != statusFail {
				t.Errorf("status is %q with %s warning and recovery_failed non-zero; a warn check downgraded a fault", h.Status, b.field)
			}
			if c := onlyCheck(t, h, b.field); c.Status != statusWarn {
				t.Errorf("check %s became %q once something else failed; a check reports its own counter", b.field, c.Status)
			}
		})
	}
}

// A check's `time` is when its counter last moved. The flags latch, so
// this is the only thing in the document that separates "faulted an hour
// ago" from "faulting now".
//
// The bracket is what kills the two mutants that look right: a `time`
// frozen at process start reads BEFORE `before`, and a `time` taken from
// the response's own clock reads AFTER `after`.
func TestHealthChecks_TimeIsWhenTheCounterMoved(t *testing.T) {
	for _, b := range checkBumpers() {
		t.Run(b.field, func(t *testing.T) {
			p := newHealthPlugin()
			// Distinguishable from the bump instant on any clock this
			// runs on; without it "moved" and "read" can share a
			// nanosecond and the assertion below proves nothing.
			time.Sleep(2 * time.Millisecond)

			before := time.Now()
			b.bump(p)
			after := time.Now()

			time.Sleep(2 * time.Millisecond)
			h := p.healthSnapshot()

			c := onlyCheck(t, h, b.field)
			at, err := time.Parse(time.RFC3339Nano, c.Time)
			if err != nil {
				t.Fatalf("check %s carries time %q, which is not RFC3339: %v", b.field, c.Time, err)
			}
			if at.Before(before) {
				t.Errorf("check %s says it moved at %s, before the bump at %s — "+
					"the stamp is older than the movement (process start?)", b.field, at, before)
			}
			if at.After(after) {
				t.Errorf("check %s says it moved at %s, after the bump finished at %s — "+
					"the stamp is the time of the READING, not of the movement", b.field, at, after)
			}
		})
	}
}

// A counter that has never moved carries the time of this reading: the
// honest statement for a zero is "nothing observed as of now", not the
// zero instant and not a missing field.
func TestHealthChecks_AnUnmovedCounterIsStampedNow(t *testing.T) {
	before := time.Now()
	p := newHealthPlugin()
	h := p.healthSnapshot()
	after := time.Now()

	for field := range declaredChecks() {
		c := onlyCheck(t, h, field)
		at, err := time.Parse(time.RFC3339Nano, c.Time)
		if err != nil {
			t.Errorf("check %s carries time %q, which is not RFC3339: %v", field, c.Time, err)
			continue
		}
		if at.Before(before) || at.After(after) {
			t.Errorf("check %s has never moved and is stamped %s, outside the reading window %s..%s",
				field, at, before, after)
		}
	}
}

// Every check needs a unit to render an observedValue against and a
// sentence to put in `output` when it fires. Both are declared beside
// the classification, so neither can be forgotten silently.
func TestHealthChecks_EveryCheckIsAnnotated(t *testing.T) {
	for _, d := range metricDefs() {
		if d.healthy && d.warn {
			t.Errorf("metric %q declares both healthy and warn; a check has one status", d.name)
		}
		if !d.healthy && !d.warn {
			if d.unit != "" || d.action != "" {
				t.Errorf("metric %q is not a check but carries unit=%q action=%q; "+
					"an annotation on a non-check is a classification that was meant and not made",
					d.name, d.unit, d.action)
			}
			continue
		}
		if d.unit == "" {
			t.Errorf("check %q has no unit; observedValue without observedUnit is a bare number", d.name)
		}
		if len(d.action) < 30 {
			t.Errorf("check %q has output %q, which is not something an operator can act on", d.name, d.action)
		}
	}
}

// A check reads a HealthResponse field, and the renderer parses that
// field as an integer. A check declared on a string or a float would
// render as a failing check naming itself, which is loud but wrong;
// this is where it is caught instead.
func TestHealthChecks_EveryCheckFieldIsAnInteger(t *testing.T) {
	h := (&Plugin{joinHints: map[string]joinHint{}, persistentDHCP: map[string]*dhcpManager{}}).healthSnapshot()
	byTag := healthFieldsByTag(h)

	for field := range declaredChecks() {
		raw, ok := byTag[field]
		if !ok {
			t.Errorf("check %q names a health field that the document does not carry", field)
			continue
		}
		if strings.TrimLeft(raw, "-0123456789") != "" {
			t.Errorf("check %q reads field value %q, which is not an integer", field, raw)
		}
	}
}

// Informational counters are NOT checks. The classification has three
// outcomes and the third one is "no entry at all"; a rule that made
// every counter a check would make `status` move on things no operator
// should be paged for.
func TestHealthChecks_InformationalCountersAreNotChecks(t *testing.T) {
	p := newHealthPlugin()
	p.recoveryDeferred.Add(1)
	p.joinAttachSlow.Add(1)
	p.acdProbesSent.Add(7)

	h := p.healthSnapshot()
	if h.Status != statusPass {
		t.Errorf("status is %q after only informational counters moved; want %q", h.Status, statusPass)
	}
	if !h.Healthy {
		t.Error("healthy went false on informational counters")
	}
	for _, field := range []string{"recovery_deferred", "join_attach_slow", "acd_probes_sent"} {
		if _, ok := h.Checks[field]; ok {
			t.Errorf("%q is an informational counter and has a check entry", field)
		}
	}
	if len(h.Checks) != len(declaredChecks()) {
		t.Errorf("the document has %d checks, the declaration has %d", len(h.Checks), len(declaredChecks()))
	}
}

// Every check's stamp exists, belongs to that check, and belongs to no
// other.
//
// WHAT THIS ADDS OVER TestHealthChecks_TimeIsWhenTheCounterMoved, which
// already brackets each check's rendered `time` around its own bump: that
// test drives one counter on a fresh plugin and reads one check, so it
// cannot see a stamp that moves for a counter OTHER than its own. A stamp
// declared `laterOf(own, someone else's)` -- the file's own idiom, since
// lease_changed is legitimately that -- renders correctly for its own bump
// and passes every assertion there, while a neighbour's fault silently
// restamps it. The result is a check reporting a time it never had, which
// is the failure this document was added to end, wearing a plausible
// value. The key-set halves are the cheap diagnosis of the same thing: a
// check with no stamp renders with the time of the READING, so a fault
// latched an hour ago reads as one happening now, and a stamp for a field
// that is not a check is a value nothing will ever read.
func TestHealthChecks_EveryCheckHasAStamp(t *testing.T) {
	declared := declaredChecks()
	stamps := newHealthPlugin().checkStamps()

	for field, want := range declared {
		if _, ok := stamps[field]; !ok {
			t.Errorf("metricDefs declares %q as a %s check and checkStamps has no entry for it; "+
				"it would render with the time of the reading, so a latched fault reads as a fresh one",
				field, want)
		}
	}
	for field := range stamps {
		if _, ok := declared[field]; !ok {
			t.Errorf("checkStamps carries %q, which metricDefs does not declare as a check; "+
				"nothing renders it, so it is a stamp no reader will ever see", field)
		}
	}

	for _, b := range checkBumpers() {
		t.Run(b.field, func(t *testing.T) {
			p := newHealthPlugin()
			for field, at := range p.checkStamps() {
				if !at.IsZero() {
					t.Fatalf("an untouched plugin already stamps %q at %s; "+
						"the isolation below would measure nothing", field, at)
				}
			}

			b.bump(p)
			after := p.checkStamps()

			if after[b.field].IsZero() {
				t.Errorf("%s moved and its stamp is still zero", b.field)
			}
			for field, at := range after {
				if field == b.field || at.IsZero() {
					continue
				}
				t.Errorf("bumping %s moved the stamp for %s as well; %s would report a time "+
					"nothing that check counts ever happened at", b.field, field, field)
			}
		})
	}
}
