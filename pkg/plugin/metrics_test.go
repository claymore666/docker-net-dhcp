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
// Values descend with field index so that every v4+v6 aggregate — all of
// which are declared before their _v6 siblings — is larger than its
// sibling, mirroring the real invariant that the aggregate counts both
// families. assertFixtureIsNotDegenerate enforces that rather than
// trusting the declaration order to stay put.
func fixtureSnapshot(t *testing.T) HealthResponse {
	t.Helper()
	var h HealthResponse
	v := reflect.ValueOf(&h).Elem()
	n := v.NumField()
	for i := 0; i < n; i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Int32, reflect.Int, reflect.Int64:
			f.SetInt(int64((n - i) * 10))
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
	assertFixtureIsNotDegenerate(t, h)
	return h
}

// assertFixtureIsNotDegenerate fails if any aggregate is not strictly
// greater than its _v6 sibling.
//
// Without this, reordering HealthResponse could silently make an
// aggregate smaller than its sibling; familySplit would then clamp the
// ipv4 series to 0, the golden file would be regenerated to match, and
// the test that is supposed to prove the derivation works would be
// asserting that it does nothing.
func assertFixtureIsNotDegenerate(t *testing.T, h HealthResponse) {
	t.Helper()
	byTag := healthFieldsByTag(h)
	for _, d := range metricDefs() {
		if d.v6field == "" {
			continue
		}
		total, err := strconv.Atoi(byTag[d.field])
		if err != nil {
			t.Fatalf("%s: aggregate %q not numeric: %v", d.name, d.field, err)
		}
		v6, err := strconv.Atoi(byTag[d.v6field])
		if err != nil {
			t.Fatalf("%s: sibling %q not numeric: %v", d.name, d.v6field, err)
		}
		if total <= v6 {
			t.Fatalf("degenerate fixture: %s aggregate (%d) is not greater than %s (%d); "+
				"the ipv4 series would clamp to zero and the golden file would stop proving the derivation",
				d.field, total, d.v6field, v6)
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

// TestMetrics_FamilySplitDerivesIPv4 checks the derivation in the
// direction an operator relies on: a v6-only event must not appear in the
// ipv4 series.
//
// This is the case that catches the tempting wrong implementation —
// rendering the aggregate straight into family="ipv4" — which would
// double-count every v6 event and inflate a v4 dashboard nobody would
// think to doubt.
func TestMetrics_FamilySplitDerivesIPv4(t *testing.T) {
	// Six v6 acquisitions and nothing else: bumpFamily bumps the
	// aggregate on every event and the sibling on v6 ones, so both read
	// 6 and the v4 share is zero.
	h := HealthResponse{LeasesObtained: 6, LeasesObtainedV6: 6}
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

	// And the mixed case, so the test is not satisfied by a renderer
	// that always emits zero.
	h = HealthResponse{LeasesObtained: 10, LeasesObtainedV6: 4}
	buf.Reset()
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	if want := `net_dhcp_leases_obtained_total{family="ipv4"} 6`; !strings.Contains(buf.String(), want) {
		t.Errorf("want %q in:\n%s", want, buf.String())
	}
}

// TestMetrics_FamilySplitClampsRatherThanEmitNegative pins what happens
// when the aggregate is smaller than its v6 sibling.
//
// That state is a bug in the counter bumps, not a legal reading. But a
// negative counter is not valid exposition, and a scrape that fails to
// parse loses every OTHER metric in the payload — turning one broken
// counter into total blindness. Clamping keeps the rest of the surface
// arriving. This test exists so the choice is recorded rather than
// discovered.
func TestMetrics_FamilySplitClampsRatherThanEmitNegative(t *testing.T) {
	h := HealthResponse{LeasesObtained: 1, LeasesObtainedV6: 5}
	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "-") && strings.Contains(out, `net_dhcp_leases_obtained_total{family="ipv4"} -`) {
		t.Errorf("emitted a negative counter, which no scraper will accept:\n%s", out)
	}
	if want := `net_dhcp_leases_obtained_total{family="ipv4"} 0`; !strings.Contains(out, want) {
		t.Errorf("want %q in:\n%s", want, out)
	}
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
