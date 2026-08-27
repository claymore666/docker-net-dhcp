// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fixtureSnapshot builds a HealthResponse in which every numeric field
// holds a DISTINCT non-zero value, by reflection rather than by hand.
//
// Distinct matters more than realistic: if the renderer wires
// leases_renewed's value into leases_obtained's series, identical values
// would hide it and the golden file would pass over a real mis-wiring.
// Non-zero matters because a field added to HealthResponse and forgotten
// in metricDefs must show up in the golden diff as a number, not as a 0
// that reads like an untouched counter.
//
// Values are derived from the field's NAME, not its index, and that is
// the whole point of fixtureValue: a field's value must be a property
// of the field and of nothing else.
//
// Values used to descend with field index — (n-i)*10 — which is
// distinct without a maintained list but couples every field to every
// other. Adding one field shifts n and reindexes everything below it,
// so a change of one series presented as a wall of renumbered values:
// measured, inserting a single field in the middle of HealthResponse
// that renders NO series at all (json:"-") moved 74 lines of the
// golden. That is a check whose output is unreadable in exactly the
// situation it exists for, and a diff that large reads as "regenerate
// it" — which is the discharge that ships a defect. It is not
// hypothetical: the golden has already been regenerated twice under
// that scheme (1aef0da, 34ef250), both times by someone reading a
// large diff and judging it fine.
//
// Under name-keying the same insertion moves ZERO lines, and a real
// series change is the only thing in the diff.
//
// Since #730 the family aggregate is no longer rendered as a series of
// its own — the two stored halves are — so what the fixture has to
// guarantee is that the aggregate, the v4 half and the v6 half read as
// three DIFFERENT numbers, or a def that named the wrong one of the
// three would render identically. assertFixtureIsNotDegenerate
// enforces that. Name-keying preserves distinctness rather than
// constructing it, so assertFixtureValuesDoNotCollide makes a hash
// collision a loud failure with a named remedy instead of two fields
// that quietly render the same number.
func fixtureSnapshot(t *testing.T) HealthResponse {
	t.Helper()
	var h HealthResponse
	v := reflect.ValueOf(&h).Elem()
	n := v.NumField()
	for i := 0; i < n; i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Int32, reflect.Int, reflect.Int64:
			f.SetInt(fixtureValue(v.Type().Field(i).Name))
		case reflect.Float64:
			f.SetFloat(1234.5)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.String:
			f.SetString("fixture-instance")
		default:
			t.Fatalf("HealthResponse field %q has kind %s, which fixtureSnapshot cannot populate — teach it, do not skip it",
				v.Type().Field(i).Name, f.Kind())
		}
	}
	assertFixtureValuesDoNotCollide(t)
	assertFixtureIsNotDegenerate(t, h)
	return h
}

// fixtureValue maps a field NAME to the value the fixture gives it.
//
// FNV-1a over the name, folded into a seven-digit range so the values
// are readable in a diff and can never be 0 — a zero would read like an
// idle counter and hide a field that metricDefs forgot.
//
// Written out rather than taken from hash/fnv so that nothing about the
// numbering can change under this test from outside the file. It must
// also stay deterministic across runs and platforms, which rules out
// hash/maphash: that is seeded randomly per process, and a fixture that
// renders different numbers on each run cannot have a golden at all.
func fixtureValue(name string) int64 {
	const offset64 = 2166136261
	const prime = 16777619
	h := uint32(offset64)
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= prime
	}
	return int64(h%9_000_000) + 1_000_000
}

// assertFixtureValuesDoNotCollide fails if two HealthResponse fields
// hash to the same fixture value.
//
// Index numbering made distinctness structural; name numbering makes it
// probable, and probable is not the same thing. Two fields rendering
// the same number is precisely the condition the golden cannot see
// through — a def wired to the wrong one of them would render
// identically, which is the mis-wiring assertFixtureIsNotDegenerate
// exists to catch and would then miss.
//
// So the collision is caught here, loudly, with a remedy, rather than
// being left to chance. It names both fields, because "there is a
// collision" is not actionable and "these two collide" is.
func assertFixtureValuesDoNotCollide(t *testing.T) {
	t.Helper()
	typ := reflect.TypeOf(HealthResponse{})
	byValue := make(map[int64]string, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		switch typ.Field(i).Type.Kind() {
		case reflect.Int32, reflect.Int, reflect.Int64:
		default:
			continue
		}
		val := fixtureValue(name)
		if prev, dup := byValue[val]; dup {
			t.Fatalf("fixture values collide: %s and %s both hash to %d.\n"+
				"  Two fields rendering the same number is the one thing the golden cannot see\n"+
				"  through -- a def wired to the wrong one of the pair renders identically.\n"+
				"  Widen the range in fixtureValue, or rename one of the two fields.",
				prev, name, val)
		}
		byValue[val] = name
	}
}

// assertFixtureIsNotDegenerate fails if a family metric's three tags —
// aggregate, v4 half, v6 half — do not hold three distinct non-zero
// values.
//
// Distinctness is what gives the golden file its power here. The two
// labelled series are now READ from stored halves rather than derived
// (#730), so the mis-wirings worth catching are a def whose v4field
// names the aggregate (the pre-#730 double-count), or one whose v4field
// and v6field name the same tag. Both make two numbers equal, and with
// distinct fixture values both move the golden. If the fixture ever
// stops distinguishing them, the golden would pass over the mis-wiring
// and prove nothing.
func assertFixtureIsNotDegenerate(t *testing.T, h HealthResponse) {
	t.Helper()
	byTag := healthFieldsByTag(h)
	for _, d := range metricDefs() {
		if d.v6field == "" {
			continue
		}
		seen := make(map[int]string, 3)
		for _, tag := range []string{d.field, d.v4field, d.v6field} {
			n, err := strconv.Atoi(byTag[tag])
			if err != nil {
				t.Fatalf("%s: %q not numeric: %v", d.name, tag, err)
			}
			if n == 0 {
				t.Fatalf("degenerate fixture: %s reads 0, so the golden could not tell a "+
					"missing series from an idle counter", tag)
			}
			if prev, dup := seen[n]; dup {
				t.Fatalf("degenerate fixture: %s and %s both read %d; a def that wired one "+
					"of them to the other's tag would not move the golden", prev, tag, n)
			}
			seen[n] = tag
		}
	}
}

// TestMetrics_EveryHealthFieldIsExposed is the guard that keeps the two
// views from drifting.
//
// /Plugin.Health and /metrics render from one snapshot, but "renders from
// the same struct" does not mean "exposes the same information": a field
// added to HealthResponse and not added to metricDefs is invisible on the
// metrics side, and nothing about it fails. That is the hole an operator
// discovers when an alert they believed in never fires.
//
// Reflection over the json tags rather than a hand-kept list, because a
// hand-kept list is the exact shape that rots (#542, #636) — it would
// have to be edited by the same person who forgot metricDefs.
func TestMetrics_EveryHealthFieldIsExposed(t *testing.T) {
	claimed := make(map[string]string)
	claim := func(tag, by string, t *testing.T) {
		if prev, dup := claimed[tag]; dup {
			t.Errorf("health field %q is exposed twice: by %q and by %q", tag, prev, by)
		}
		claimed[tag] = by
	}
	for _, d := range metricDefs() {
		claim(d.field, d.name, t)
		if d.v4field != "" {
			claim(d.v4field, d.name+"{family=ipv4}", t)
		}
		if d.v6field != "" {
			claim(d.v6field, d.name+"{family=ipv6}", t)
		}
	}
	for tag, by := range metricLabelOnlyFields {
		claim(tag, by+" label", t)
	}

	typ := reflect.TypeOf(HealthResponse{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if _, ok := claimed[tag]; !ok {
			t.Errorf("HealthResponse field %s (json %q) is not exposed on /metrics.\n"+
				"Add it to metricDefs, or to metricLabelOnlyFields if it belongs on a label. "+
				"Leaving it out means an operator's dashboard silently lacks it.",
				typ.Field(i).Name, tag)
		}
		delete(claimed, tag)
	}
	for tag, by := range claimed {
		t.Errorf("%q exposes health field %q, which HealthResponse does not have", by, tag)
	}
}

// TestMetrics_GoldenExposition pins the whole rendered surface.
//
// Schema drift then shows up as a reviewable diff instead of as a
// dashboard that quietly changed meaning. Regenerate deliberately with
// -update after reading the diff, never to make a red test green.
func TestMetrics_GoldenExposition(t *testing.T) {
	var buf bytes.Buffer
	if err := writeExposition(&buf, fixtureSnapshot(t)); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	golden := filepath.Join("testdata", "metrics_exposition.golden")

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Log("golden updated")
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("exposition drifted from %s.\n--- got ---\n%s\n--- want ---\n%s",
			golden, buf.String(), string(want))
	}
}

// TestMetrics_NoSeriesRendersNegative is what survives of
// TestMetrics_FamilySplitClampsRatherThanEmitNegative (#730).
//
// That test pinned two things. Half of it — that a family series clamps
// to 0 when the aggregate reads below its v6 sibling — describes a
// mechanism this change deletes; there is no aggregate counter left to
// read low. The other half is still a live promise and is kept here: no
// counter this plugin renders may be negative, because no scraper will
// take one.
//
// The input is deliberately the old test's: an aggregate SMALLER than
// its v6 half. Under the current renderer that combination is simply
// not consulted — the ipv4 series is the stored v4 field — so it is
// harmless. It stops being harmless the moment someone reintroduces
// aggregate-minus-v6, which is precisely what this asserts against.
// Mutating writeExposition back to a subtraction makes this test emit
// -4 and fail; that is the mutant it was written to kill.
func TestMetrics_NoSeriesRendersNegative(t *testing.T) {
	hostile := HealthResponse{LeasesObtained: 1, LeasesObtainedV6: 5}
	for _, tc := range []struct {
		name string
		h    HealthResponse
	}{
		{"a v6 half above the aggregate", hostile},
		{"the full fixture", fixtureSnapshot(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeExposition(&buf, tc.h); err != nil {
				t.Fatalf("writeExposition: %v", err)
			}
			checked := 0
			for _, line := range strings.Split(buf.String(), "\n") {
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				sp := strings.LastIndex(line, " ")
				if sp < 0 {
					t.Errorf("malformed series line: %q", line)
					continue
				}
				v, err := strconv.ParseFloat(line[sp+1:], 64)
				if err != nil {
					t.Errorf("series value in %q is not a number: %v", line, err)
					continue
				}
				checked++
				if v < 0 {
					t.Errorf("negative counter, which no scraper will accept: %s", line)
				}
			}
			// Without this the loop passes by having read nothing —
			// an empty render would look exactly like a clean one.
			if checked < 20 {
				t.Fatalf("only %d series values inspected; the exposition did not render", checked)
			}
		})
	}
}

// TestMetrics_FamilySeriesReadTheStoredHalves checks the property an
// operator relies on: each family series carries that family's own
// count and nothing else.
//
// It replaces TestMetrics_FamilySplitDerivesIPv4, which checked that
// aggregate-minus-v6 landed in the ipv4 series. That subtraction is
// gone (#730). The wrong implementation it guarded against is still
// worth guarding: rendering the AGGREGATE into family="ipv4" would
// double-count every v6 event and inflate a v4 dashboard nobody would
// think to doubt. The aggregate here is deliberately not the sum of the
// halves, so a renderer that reads it produces a number that appears in
// neither expectation.
func TestMetrics_FamilySeriesReadTheStoredHalves(t *testing.T) {
	// v6-only traffic: the v4 half never moved.
	h := HealthResponse{LeasesObtained: 6, LeasesObtainedV4: 0, LeasesObtainedV6: 6}
	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	out := buf.String()

	wantV4 := `net_dhcp_leases_obtained_total{family="ipv4"} 0`
	wantV6 := `net_dhcp_leases_obtained_total{family="ipv6"} 6`
	if !strings.Contains(out, wantV4) {
		t.Errorf("v6-only traffic leaked into the ipv4 series; want %q in:\n%s", wantV4, out)
	}
	if !strings.Contains(out, wantV6) {
		t.Errorf("missing %q in:\n%s", wantV6, out)
	}

	// Mixed, with three distinct numbers: a renderer that emitted the
	// aggregate, or the wrong half, or a constant zero, fails one of
	// these. 99 is not 6+4 precisely so that reading it stands out.
	h = HealthResponse{LeasesObtained: 99, LeasesObtainedV4: 6, LeasesObtainedV6: 4}
	buf.Reset()
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	out = buf.String()
	for _, want := range []string{
		`net_dhcp_leases_obtained_total{family="ipv4"} 6`,
		`net_dhcp_leases_obtained_total{family="ipv6"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	// The aggregate is claimed by the def but must not be emitted as a
	// bare series; if it were, it would be a third number for the same
	// counter and a dashboard summing the labels would double.
	if bare := "\nnet_dhcp_leases_obtained_total 99\n"; strings.Contains(out, bare) {
		t.Errorf("aggregate emitted as its own series; the two labelled series carry it:\n%s", out)
	}
}

// TestMetrics_FamilySeriesCannotGoBackwards is the regression test for
// #730, written against the interleaving that produced the defect.
//
// A v6 event landing BETWEEN healthSnapshot's two reads of a family pair
// used to give the renderer old-aggregate / new-v6, and the ipv4 series
// — computed as the difference — came out one lower than the previous
// scrape. Prometheus reads any counter decrease as a reset and bills the
// whole accumulated value as an increase on the next scrape, so one
// dropped unit became a rate spike of the entire count.
//
// This drives the real snapshot path rather than a hand-built struct,
// because the defect lived in how the pair was READ and not in how it
// was rendered. What it pins is that the ipv4 series equals the stored
// v4 half at every step — so a renderer that went back to arithmetic
// over two rendered fields is caught the moment the two stop agreeing.
// The interleaving itself needs concurrency and is covered by
// TestMetrics_FamilySeriesSurviveAConcurrentV6Bump below.
func TestMetrics_FamilySeriesCannotGoBackwards(t *testing.T) {
	p := &Plugin{}

	read := func() int {
		t.Helper()
		var buf bytes.Buffer
		if err := writeExposition(&buf, p.healthSnapshot()); err != nil {
			t.Fatalf("writeExposition: %v", err)
		}
		const prefix = `net_dhcp_leases_obtained_total{family="ipv4"} `
		for _, line := range strings.Split(buf.String(), "\n") {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
			if err != nil {
				t.Fatalf("ipv4 series %q is not an integer: %v", line, err)
			}
			return n
		}
		t.Fatalf("no ipv4 series in:\n%s", buf.String())
		return 0
	}

	// A steady v4 count with v6 events arriving throughout. Every
	// scrape must be >= the one before it; the v4 series must also
	// never move on a v6 event, which is what makes "monotonic" more
	// than "we only ever added".
	prev := read()
	for i := 0; i < 64; i++ {
		if i%4 == 0 {
			bumpFamily(&p.leasesObtainedV4, &p.leasesObtainedV6, false)
		}
		bumpFamily(&p.leasesObtainedV4, &p.leasesObtainedV6, true)

		got := read()
		if got < prev {
			t.Fatalf("ipv4 series went backwards at iteration %d: %d -> %d; "+
				"Prometheus reads that as a counter reset and bills the whole value as a rate spike",
				i, prev, got)
		}
		if want := int(p.leasesObtainedV4.Load()); got != want {
			t.Fatalf("ipv4 series = %d at iteration %d, want the stored v4 half %d", got, i, want)
		}
		prev = got
	}

	// The aggregate is the sum of the halves, not a third counter.
	h := p.healthSnapshot()
	if want := h.LeasesObtainedV4 + h.LeasesObtainedV6; h.LeasesObtained != want {
		t.Errorf("leases_obtained = %d, want v4+v6 = %d", h.LeasesObtained, want)
	}
}

// familyPair names one of the six counters that carry a `family`
// label, in every form a test needs to reach it: the two atomics a
// manager goroutine bumps, and the three HealthResponse fields a scrape
// renders from them.
//
// Six pairs, and a mutation only has to survive in ONE of them to be a
// live defect — so anything asserting the load-once property has to say
// it about all six rather than about the one that was convenient.
type familyPair struct {
	metric string
	atoms  func(p *Plugin) (v4, v6 *atomic.Int32)
	fields func(h HealthResponse) (agg, v4, v6 int32)
}

func familyPairs() []familyPair {
	return []familyPair{
		{"net_dhcp_lease_changed_total",
			func(p *Plugin) (*atomic.Int32, *atomic.Int32) { return &p.leaseChangedV4, &p.leaseChangedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.LeaseChanged, h.LeaseChangedV4, h.LeaseChangedV6
			}},
		{"net_dhcp_leases_obtained_total",
			func(p *Plugin) (*atomic.Int32, *atomic.Int32) { return &p.leasesObtainedV4, &p.leasesObtainedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.LeasesObtained, h.LeasesObtainedV4, h.LeasesObtainedV6
			}},
		{"net_dhcp_leases_renewed_total",
			func(p *Plugin) (*atomic.Int32, *atomic.Int32) { return &p.leasesRenewedV4, &p.leasesRenewedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.LeasesRenewed, h.LeasesRenewedV4, h.LeasesRenewedV6
			}},
		{"net_dhcp_dhcp_timeouts_total",
			func(p *Plugin) (*atomic.Int32, *atomic.Int32) { return &p.dhcpTimeoutsV4, &p.dhcpTimeoutsV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.DHCPTimeouts, h.DHCPTimeoutsV4, h.DHCPTimeoutsV6
			}},
		{"net_dhcp_naks_received_total",
			func(p *Plugin) (*atomic.Int32, *atomic.Int32) { return &p.naksReceivedV4, &p.naksReceivedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.NAKsReceived, h.NAKsReceivedV4, h.NAKsReceivedV6
			}},
		{"net_dhcp_client_stop_failures_total",
			func(p *Plugin) (*atomic.Int32, *atomic.Int32) {
				return &p.clientStopFailuresV4, &p.clientStopFailuresV6
			},
			func(h HealthResponse) (int32, int32, int32) {
				return h.ClientStopFailures, h.ClientStopFailuresV4, h.ClientStopFailuresV6
			}},
	}
}

// assertFamilyPairsCoverProduction reconciles the pairs this test walks
// against the family-split metrics production actually renders.
//
// The first form of this guard compared len(pairs) against the literal
// 6. Both halves of that comparison live in this file and are edited
// together, so it could only catch someone editing familyPairs() and
// forgetting the number — it was blind to the case it was written for,
// a SEVENTH family-split metric appearing in metricDefs(). Measured:
// adding one there leaves this test green and reds only
// TestMetrics_GoldenExposition, whose message says to regenerate the
// golden. A signal that names the wrong remedy gets discharged, and
// the new counter ships outside the load-once invariant.
//
// Compare the NAMES in both directions and print the difference, the
// way a golden conflict is resolved. A count — even a derived one —
// would pass two disjoint sets of six.
//
// The name is built here the way the renderer builds it at
// metrics.go:181, and that duplication is DELIBERATE: a shared
// seriesName() helper would make both sides agree by construction —
// a wrong prefix or a missing suffix would move production and this
// reconciliation together, `rendered` would match `walked` perfectly,
// and the test would be green over a broken exposition. That is the
// mirror property this whole change exists to remove. The restatement
// is independent, and independence is the only thing that makes
// disagreement detectable; divergence fires in direction 2 below.
//
// The line moves if the renderer's naming ever grows branches — a
// third suffix, a per-kind rule — because then this stops being a
// restatement and becomes a reimplementation with its own bugs. Two
// lines is not that.
func assertFamilyPairsCoverProduction(t *testing.T, pairs []familyPair) {
	t.Helper()

	rendered := map[string]bool{}
	for _, d := range metricDefs() {
		if d.v4field == "" && d.v6field == "" {
			continue
		}
		name := metricPrefix + d.name
		if d.counter {
			name += "_total"
		}
		rendered[name] = true
	}
	// Absent is not zero: with no family-split metric recognised at
	// all, both directions below pass over nothing and report a clean
	// reconciliation of two empty sets.
	if len(rendered) == 0 {
		t.Fatalf("metricDefs() reported no family-split metrics at all — the two " +
			"directions below would agree vacuously")
	}

	walked := map[string]bool{}
	for _, fp := range pairs {
		walked[fp.metric] = true
	}
	for name := range rendered {
		if !walked[name] {
			t.Errorf("%s is family-split in metricDefs() but absent from familyPairs(), so "+
				"nothing here holds it to the load-once invariant (#730)", name)
		}
	}
	for name := range walked {
		if !rendered[name] {
			t.Errorf("%s is in familyPairs() but production no longer renders it "+
				"family-split — this test is walking a metric that does not exist", name)
		}
	}
}

// TestMetrics_FamilySeriesSurviveConcurrentBumps is the regression test
// for #730, written against the condition that produced the defect: a
// manager goroutine bumping a family counter while a scrape is inside
// healthSnapshot.
//
// It replaces a version that bumped only the v6 half of ONE pair and
// held v4 frozen at 7. That test asserted "each half is loaded exactly
// once" in its comment and could not observe it: with v4 never moving,
// a second .Load() of the v4 atom returns the same number both times.
// Seven mutants survived it — every v4-side re-load, in all six pairs,
// plus both re-loads in the five pairs it did not touch. The v4
// direction is not hypothetical: bumpFamily(&p.leasesObtainedV4, …,
// false) fires on every v4 lease event, and a v4 double-load lets the
// aggregate exceed the halves the same snapshot rendered.
//
// So: both families bump, on all six pairs, and every scrape is judged
// on all six, against three properties.
//
// What each property is worth was MEASURED, by disabling one at a time
// and re-running the mutants, rather than reasoned about. Over the 24
// double-load mutants — both halves of all six pairs, in the aggregate
// and in the rendered half:
//
//	property 1 alone     kills all 24
//	property 2 alone     kills none of them
//	property 3 alone     kills none of them
//	properties 1+2       still 24, so 3 adds nothing here
//
// And over three renderer mutants, which property 1 structurally cannot
// see because it judges the SNAPSHOT and a renderer does not touch it.
// The middle column is THIS DOC BLOCK ONLY; the right one is the whole
// package, because a -run filter narrows the observer set and a claim
// that nothing catches something cannot be made from inside one test:
//
//	                                        of these 3   in the package
//	renders v6 under family="ipv4"          property 2   +Backwards, Golden
//	renders the aggregate under ipv4        property 2   +Backwards, Golden
//	derives ipv4 as (aggregate - v6)        none         ReadTheStoredHalves,
//	                                                     NoSeriesRendersNegative,
//	                                                     Golden
//
// The last row is the interesting one and it says something narrower
// than it looks. Within a snapshot that satisfies property 1,
// `aggregate - v6` IS v4 to the byte -- so nothing in this doc block
// can see that mutant, and nothing here should be expected to. The
// tests that DO catch it are the ones fed a deliberately inconsistent
// snapshot: TestMetrics_NoSeriesRendersNegative hands the renderer a v6
// half ABOVE the aggregate and the mutant emits -4 where the stored
// half says 6, which is #730's literal symptom.
//
// So the honest statement is not "subtraction is safe now". It is that
// the old subtraction was wrong because the two counters could
// DISAGREE, not because subtraction is wrong -- and the coverage for
// the disagreement lives in a different test, with a hostile fixture,
// on purpose. Do not read this block as evidence that that test is
// redundant. It is the one that covers the case #730 actually shipped.
//
// Property 3 killed none of the 27 mutants above — the same scope
// caveat applies, it is a statement about those mutants and not about
// the world — and it stays. It is the only property
// in the operator's terms — Prometheus reads a counter decrease as a
// reset and repays the whole accumulated count as a rate spike — and
// the only one that would still fire if the load-once invariant held
// and a series went backwards for a reason nobody has thought of yet.
// A symptom observer looks idle right up until the day it is the only
// thing watching, and a redundancy measurement is not a deletion
// argument.
//
//  1. INTERNAL CONSISTENCY. aggregate == v4 + v6, on every pair of every
//     snapshot. This is the load-once property stated directly: a second
//     .Load() of EITHER half puts a bump between the sum and the value
//     rendered beside it. Any consumer subtracting one from the other —
//     an operator on the JSON, or a future renderer — inherits #730.
//  2. THE SERIES IS THE STORED HALF. The rendered family series must
//     equal the half in the snapshot it came from, so a renderer that
//     derives rather than reads is caught even when the arithmetic
//     happens to agree.
//  3. NO SERIES GOES BACKWARDS. #730's actual symptom. Both halves only
//     ever increase, so no rendered series may read below the previous
//     scrape; Prometheus takes a decrease as a reset and repays the
//     whole accumulated count as a rate spike.
func TestMetrics_FamilySeriesSurviveConcurrentBumps(t *testing.T) {
	pairs := familyPairs()
	assertFamilyPairsCoverProduction(t, pairs)

	p := &Plugin{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, fp := range pairs {
		v4, v6 := fp.atoms(p)
		for _, v6side := range []bool{false, true} {
			wg.Add(1)
			go func(v6side bool) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						bumpFamily(v4, v6, v6side)
					}
				}
			}(v6side)
		}
	}
	halt := func() {
		close(stop)
		wg.Wait()
	}

	prev := make(map[string]int64, 2*len(pairs))
	const scrapes = 2000
	observed := 0
	for i := 0; i < scrapes; i++ {
		h := p.healthSnapshot()

		for _, fp := range pairs {
			agg, v4, v6 := fp.fields(h)
			if agg != v4+v6 {
				halt()
				t.Fatalf("scrape %d: %s is internally inconsistent: aggregate=%d but v4+v6=%d "+
					"(v4=%d v6=%d); a half was loaded more than once",
					i, fp.metric, agg, v4+v6, v4, v6)
			}
		}

		var buf bytes.Buffer
		if err := writeExposition(&buf, h); err != nil {
			halt()
			t.Fatalf("writeExposition: %v", err)
		}
		for _, fp := range pairs {
			_, v4, v6 := fp.fields(h)
			for _, want := range []struct {
				series string
				stored int32
			}{
				{fp.metric + `{family="ipv4"} `, v4},
				{fp.metric + `{family="ipv6"} `, v6},
			} {
				got, ok := seriesValue(buf.String(), want.series)
				if !ok {
					halt()
					t.Fatalf("scrape %d: %q missing from the exposition", i, want.series)
				}
				observed++
				if got != int64(want.stored) {
					halt()
					t.Fatalf("scrape %d: %s= %d but the snapshot it rendered stored %d; "+
						"the series is being derived rather than read",
						i, want.series, got, want.stored)
				}
				if was, seen := prev[want.series]; seen && got < was {
					halt()
					t.Fatalf("scrape %d: %s went backwards, %d -> %d; Prometheus reads a "+
						"counter decrease as a reset and repays the whole count as a rate spike",
						i, want.series, was, got)
				}
				prev[want.series] = got
			}
		}
	}
	halt()

	// A loop that inspected nothing looks exactly like a loop that found
	// nothing wrong. Twelve series, every scrape.
	if want := scrapes * 2 * len(pairs); observed != want {
		t.Fatalf("inspected %d series readings, want %d — the loop did not run over what it claims",
			observed, want)
	}
	// And the bumpers must actually have run, or every assertion above
	// held over a set of zeros.
	for _, fp := range pairs {
		v4, v6 := fp.atoms(p)
		if v4.Load() == 0 || v6.Load() == 0 {
			t.Fatalf("%s: no concurrent traffic (v4=%d v6=%d); the test proved nothing",
				fp.metric, v4.Load(), v6.Load())
		}
	}
}

// seriesValue returns the integer value rendered for a series line
// beginning with prefix. Reporting whether it found the line at all is
// the point: a caller that treats "absent" as "0" cannot tell a series
// that read zero from a series the renderer never emitted.
func seriesValue(exposition, prefix string) (int64, bool) {
	for _, line := range strings.Split(exposition, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// TestMetrics_LabelValuesAreEscaped covers the one injection surface
// here: instance_id is rendered inside a quoted label value.
//
// It is generated internally today and cannot contain a quote, which is
// exactly why this is worth pinning — the escaping is correct for a
// reason nobody can see, so a later change that makes the id
// operator-supplied would otherwise produce a payload that fails to parse
// with no test to catch it.
func TestMetrics_LabelValuesAreEscaped(t *testing.T) {
	h := HealthResponse{InstanceID: `a"b\c` + "\n" + `d`}
	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	want := `net_dhcp_build_info{instance_id="a\"b\\c\nd"} 1`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("want %q in:\n%s", want, buf.String())
	}
}

// TestMetrics_HelpKeepsQuotesUnescaped pins the asymmetry that a single
// shared escaper would get wrong: HELP escapes backslash and newline but
// NOT the double quote, while a label value escapes all three.
func TestMetrics_HelpKeepsQuotesUnescaped(t *testing.T) {
	if got := escapeHelp(`say "hi"` + "\n" + `and \ that`); got != `say "hi"\nand \\ that` {
		t.Errorf("escapeHelp = %q", got)
	}
	if got := escapeLabelValue(`say "hi"`); got != `say \"hi\"` {
		t.Errorf("escapeLabelValue = %q", got)
	}
}

// TestMetrics_EveryFamilyIsWellFormed asserts the structural rules a
// scraper enforces, over the real table rather than a sample: HELP and
// TYPE precede every family, each name appears once, and counters carry
// the _total suffix.
func TestMetrics_EveryFamilyIsWellFormed(t *testing.T) {
	var buf bytes.Buffer
	if err := writeExposition(&buf, fixtureSnapshot(t)); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}

	seenHelp := map[string]bool{}
	seenType := map[string]string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name := strings.Fields(line)[2]
			if seenHelp[name] {
				t.Errorf("duplicate HELP for %s", name)
			}
			seenHelp[name] = true
			if rest := strings.TrimSpace(strings.TrimPrefix(line, "# HELP "+name)); rest == "" {
				t.Errorf("%s has an empty HELP; it is operator-facing documentation", name)
			}
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			seenType[f[2]] = f[3]
		case strings.HasPrefix(line, "#"):
			t.Errorf("unrecognised comment line: %q", line)
		default:
			name := line
			if i := strings.IndexAny(line, "{ "); i >= 0 {
				name = line[:i]
			}
			if !seenHelp[name] {
				t.Errorf("series %s appears before its HELP line", name)
			}
			kind, ok := seenType[name]
			if !ok {
				t.Errorf("series %s appears before its TYPE line", name)
			}
			if kind == "counter" && !strings.HasSuffix(name, "_total") {
				t.Errorf("counter %s is missing the _total suffix", name)
			}
			if kind == "gauge" && strings.HasSuffix(name, "_total") {
				t.Errorf("gauge %s carries the _total suffix, which reads as a counter", name)
			}
		}
	}
	if len(seenHelp) != len(metricDefs())+1 { // +1 for build_info
		t.Errorf("rendered %d families, table declares %d (+build_info)", len(seenHelp), len(metricDefs()))
	}
}

// TestMetrics_UnknownFieldIsAnError proves the table cannot silently name
// a field that does not exist — the failure mode a rename would produce.
func TestMetrics_UnknownFieldIsAnError(t *testing.T) {
	var buf bytes.Buffer
	err := writeExpositionWith(&buf, HealthResponse{}, []metricDef{
		{name: "nope", help: "h", field: "no_such_field"},
	})
	if err == nil {
		t.Fatal("a metric naming a nonexistent health field rendered without error")
	}
	if !strings.Contains(err.Error(), "no_such_field") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

// TestMetricsExposition_NoPerEndpointIdentifiers pins the property the
// project's security posture rests on: /metrics is an aggregate counter
// surface, and discloses nothing about WHICH container holds which
// lease.
//
// SECURITY.md states it as a promise to operators ("No endpoint IDs,
// container names, addresses or MACs appear in it"), docs/reference.md
// repeats it, and warnOnWildcardMetricsBind's text depends on it to
// describe what a wildcard bind actually leaks. Three prose copies of
// one claim, and prose is what rots: the wildcard warning shipped
// asserting the opposite — MACs and leased IPs — and contradicted
// SECURITY.md in the same release without anything going red.
//
// The check is deliberately an ALLOW-LIST of label names rather than a
// scan for address-shaped values. A regex for MACs and IPs only catches
// the identifiers someone thought of; requiring that every label name
// be one of two known-safe ones means a per-endpoint label of ANY shape
// — a container name, an endpoint ID, a network ID — fails here, and
// adding one deliberately forces whoever does it past this comment and
// back to SECURITY.md.
func TestMetricsExposition_NoPerEndpointIdentifiers(t *testing.T) {
	// instance_id is a per-process UUID, not a container identifier;
	// family is "ipv4"/"ipv6". Both are documented as safe.
	allowed := map[string]bool{"instance_id": true, "family": true}

	var buf bytes.Buffer
	if err := writeExposition(&buf, fixtureSnapshot(t)); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}

	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		open := strings.Index(line, "{")
		if open < 0 {
			continue // an unlabelled series
		}
		close := strings.LastIndex(line, "}")
		if close < open {
			t.Errorf("malformed labelled series: %q", line)
			continue
		}
		for _, pair := range strings.Split(line[open+1:close], ",") {
			eq := strings.Index(pair, "=")
			if eq < 0 {
				t.Errorf("malformed label %q in %q", pair, line)
				continue
			}
			name := strings.TrimSpace(pair[:eq])
			if !allowed[name] {
				t.Errorf("exposition carries label %q, which is not one of the "+
					"identifiers SECURITY.md promises are absent.\n"+
					"If this label is genuinely safe, add it to the allow-list "+
					"here AND update SECURITY.md and docs/reference.md in the "+
					"same change — the promise and the code must move together.\n"+
					"line: %s", name, line)
			}
		}
	}
}
