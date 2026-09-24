// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

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
			f.SetString(fixtureString(v.Type().Field(i)))
		case reflect.Map:
			f.Set(reflect.ValueOf(map[string][]HealthCheck{
				"fixture_check": {{Status: statusWarn, ObservedValue: 1, ObservedUnit: "events", Time: "2026-01-02T03:04:05Z"}},
			}))
		case reflect.Slice:
			f.Set(reflect.ValueOf([]EndpointHealth{{
				Endpoint: "fixtureep", Network: "fixturenet", Mode: "macvlan",
				Address: "192.0.2.7/24", LeaseState: "bound",
			}}))
		default:
			t.Fatalf("HealthResponse field %q has kind %s, which fixtureSnapshot cannot populate — teach it, do not skip it",
				v.Type().Field(i).Name, f.Kind())
		}
	}
	assertFixtureValuesDoNotCollide(t)
	assertFixtureIsNotDegenerate(t, h)
	return h
}

func fixtureString(f reflect.StructField) string {
	tag := strings.Split(f.Tag.Get("json"), ",")[0]
	for _, d := range metricDefs() {
		if d.field == tag && d.values != nil {
			if _, ok := d.values[statusWarn]; ok {
				return statusWarn
			}
			keys := make([]string, 0, len(d.values))
			for k := range d.values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return keys[0]
		}
	}
	return "fixture-" + strings.ToLower(f.Name)
}

// fixtureValue is FNV-1a over the field name, never 0; hash/maphash is seeded per process and could not back a golden
// (#730).
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
	for tag, why := range metricNotExposedFields {
		if len(why) < 30 {
			t.Errorf("health field %q is declared not-exposed with the reason %q, which is not a reason. "+
				"An entry here removes a field from the /metrics surface; the sentence is what a "+
				"reviewer judges that removal by.", tag, why)
		}
		claim(tag, "metricNotExposedFields", t)
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

func TestMetrics_NoSeriesRendersNegative(t *testing.T) {
	hostile := HealthResponse{Status: statusPass, LeasesObtained: 1, LeasesObtainedV6: 5}
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
			if checked < 20 {
				t.Fatalf("only %d series values inspected; the exposition did not render", checked)
			}
		})
	}
}

func TestMetrics_FamilySeriesReadTheStoredHalves(t *testing.T) {
	h := HealthResponse{Status: statusPass, LeasesObtained: 6, LeasesObtainedV4: 0, LeasesObtainedV6: 6}
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

	h = HealthResponse{Status: statusPass, LeasesObtained: 99, LeasesObtainedV4: 6, LeasesObtainedV6: 4}
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
	if bare := "\nnet_dhcp_leases_obtained_total 99\n"; strings.Contains(out, bare) {
		t.Errorf("aggregate emitted as its own series; the two labelled series carry it:\n%s", out)
	}
}

// TestMetrics_FamilySeriesCannotGoBackwards: Prometheus reads a counter decrease as a reset, so one lost unit became
// a rate spike of the whole count (#730).
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

	h := p.healthSnapshot()
	if want := h.LeasesObtainedV4 + h.LeasesObtainedV6; h.LeasesObtained != want {
		t.Errorf("leases_obtained = %d, want v4+v6 = %d", h.LeasesObtained, want)
	}
}

type familyPair struct {
	metric string
	atoms  func(p *Plugin) (v4, v6 intCounter)
	fields func(h HealthResponse) (agg, v4, v6 int32)
}

func familyPairs() []familyPair {
	return []familyPair{
		{"net_dhcp_address_conflicts_total",
			func(p *Plugin) (intCounter, intCounter) {
				return &p.addressConflictsV4, &p.addressConflictsV6
			},
			func(h HealthResponse) (int32, int32, int32) {
				return h.AddressConflicts, h.AddressConflictsV4, h.AddressConflictsV6
			}},
		{"net_dhcp_lease_changed_total",
			func(p *Plugin) (intCounter, intCounter) { return &p.leaseChangedV4, &p.leaseChangedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.LeaseChanged, h.LeaseChangedV4, h.LeaseChangedV6
			}},
		{"net_dhcp_leases_obtained_total",
			func(p *Plugin) (intCounter, intCounter) { return &p.leasesObtainedV4, &p.leasesObtainedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.LeasesObtained, h.LeasesObtainedV4, h.LeasesObtainedV6
			}},
		{"net_dhcp_leases_renewed_total",
			func(p *Plugin) (intCounter, intCounter) { return &p.leasesRenewedV4, &p.leasesRenewedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.LeasesRenewed, h.LeasesRenewedV4, h.LeasesRenewedV6
			}},
		{"net_dhcp_renewals_unanswered_total",
			func(p *Plugin) (intCounter, intCounter) {
				return &p.renewalsUnansweredV4, &p.renewalsUnansweredV6
			},
			func(h HealthResponse) (int32, int32, int32) {
				return h.RenewalsUnanswered, h.RenewalsUnansweredV4, h.RenewalsUnansweredV6
			}},
		{"net_dhcp_dhcp_timeouts_total",
			func(p *Plugin) (intCounter, intCounter) { return &p.dhcpTimeoutsV4, &p.dhcpTimeoutsV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.DHCPTimeouts, h.DHCPTimeoutsV4, h.DHCPTimeoutsV6
			}},
		{"net_dhcp_naks_received_total",
			func(p *Plugin) (intCounter, intCounter) { return &p.naksReceivedV4, &p.naksReceivedV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.NAKsReceived, h.NAKsReceivedV4, h.NAKsReceivedV6
			}},
		{"net_dhcp_client_stop_failures_total",
			func(p *Plugin) (intCounter, intCounter) {
				return &p.clientStopFailuresV4, &p.clientStopFailuresV6
			},
			func(h HealthResponse) (int32, int32, int32) {
				return h.ClientStopFailures, h.ClientStopFailuresV4, h.ClientStopFailuresV6
			}},
		{"net_dhcp_releases_sent_total",
			func(p *Plugin) (intCounter, intCounter) { return &p.releasesSentV4, &p.releasesSentV6 },
			func(h HealthResponse) (int32, int32, int32) {
				return h.ReleasesSent, h.ReleasesSentV4, h.ReleasesSentV6
			}},
		{"net_dhcp_release_failures_total",
			func(p *Plugin) (intCounter, intCounter) {
				return &p.releaseFailuresV4, &p.releaseFailuresV6
			},
			func(h HealthResponse) (int32, int32, int32) {
				return h.ReleaseFailures, h.ReleaseFailuresV4, h.ReleaseFailuresV6
			}},
		{"net_dhcp_releases_reclaimed_total",
			func(p *Plugin) (intCounter, intCounter) {
				return &p.releasesReclaimedV4, &p.releasesReclaimedV6
			},
			func(h HealthResponse) (int32, int32, int32) {
				return h.ReleasesReclaimed, h.ReleasesReclaimedV4, h.ReleasesReclaimedV6
			}},
	}
}

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

	if want := scrapes * 2 * len(pairs); observed != want {
		t.Fatalf("inspected %d series readings, want %d — the loop did not run over what it claims",
			observed, want)
	}
	for _, fp := range pairs {
		v4, v6 := fp.atoms(p)
		if v4.Load() == 0 || v6.Load() == 0 {
			t.Fatalf("%s: no concurrent traffic (v4=%d v6=%d); the test proved nothing",
				fp.metric, v4.Load(), v6.Load())
		}
	}
}

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

func TestMetrics_LabelValuesAreEscaped(t *testing.T) {
	h := HealthResponse{Status: statusPass, InstanceID: `a"b\c` + "\n" + `d`}
	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	want := `net_dhcp_build_info{instance_id="a\"b\\c\nd",version="",commit="",library=""} 1`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("want %q in:\n%s", want, buf.String())
	}
}

func TestMetrics_HelpKeepsQuotesUnescaped(t *testing.T) {
	if got := escapeHelp(`say "hi"` + "\n" + `and \ that`); got != `say "hi"\nand \\ that` {
		t.Errorf("escapeHelp = %q", got)
	}
	if got := escapeLabelValue(`say "hi"`); got != `say \"hi\"` {
		t.Errorf("escapeLabelValue = %q", got)
	}
}

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
	// +2: build_info and engine_info (#670) are not counters, so they are not in metricDefs.
	if len(seenHelp) != len(metricDefs())+2 {
		t.Errorf("rendered %d families, table declares %d (+build_info, +engine_info)", len(seenHelp), len(metricDefs()))
	}
}

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

func TestMetricsExposition_NoPerEndpointIdentifiers(t *testing.T) {
	// instance_id, family and the three build labels are host-wide; engine_version and api_version name the host's
	// Docker (#670), as SECURITY.md states.
	allowed := map[string]bool{
		"instance_id": true, "family": true,
		"version": true, "commit": true, "library": true,
		"engine_version": true, "api_version": true,
	}

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
			continue
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

func TestMetricHelpMatchesHealthyField(t *testing.T) {
	const (
		affirm  = "Healthy-affecting."
		negForm = "Not healthy-affecting:"
	)
	for _, d := range metricDefs() {
		lower := strings.ToLower(d.help)
		mentions := strings.Contains(lower, "healthy-affecting")

		if d.healthy {
			if !strings.Contains(d.help, affirm) {
				t.Errorf("%s: healthy: true but help does not contain %q.\n"+
					"  help: %s\n"+
					"  The field is what the gate reads; the sentence is what an\n"+
					"  operator reads. Both, or they drift.", d.name, affirm, d.help)
			}
			if strings.Contains(lower, strings.ToLower(negForm)) {
				t.Errorf("%s: healthy: true but help also says %q", d.name, negForm)
			}
			continue
		}

		if !mentions {
			continue
		}
		if !strings.Contains(d.help, negForm) {
			t.Errorf("%s: healthy is false and help mentions healthy-affecting,\n"+
				"  but not in the one sanctioned negative form %q.\n"+
				"  help: %s\n"+
				"  Rephrase to that exact form, or set healthy: true. Free-form\n"+
				"  negation is what made this property unreadable twice; the\n"+
				"  spelling is fixed so that no reader -- human or gate -- has\n"+
				"  to interpret it.", d.name, negForm, d.help)
		}
		// `Not healthy-affecting:` contains the affirmative token, so occurrences are counted (#826, #854).
		if strings.Count(lower, "healthy-affecting") != strings.Count(strings.ToLower(negForm), "healthy-affecting") {
			t.Errorf("%s: healthy is false but help mentions healthy-affecting %d time(s);\n"+
				"  only the leading %q may mention it.\n  help: %s",
				d.name, strings.Count(lower, "healthy-affecting"), negForm, d.help)
		}
	}
}
