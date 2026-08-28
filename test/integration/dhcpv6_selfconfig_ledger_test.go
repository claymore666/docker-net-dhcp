// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// The check on the check.
//
// assertLifetimeRefreshed is the entire discrimination in #875's
// self-configuring arm: everything else in that test is plumbing, and
// this one function decides whether a container's address is being
// refreshed or is quietly counting down to nothing. Reaching it through
// TestDHCPv6_SelfConfiguring_AddressAndRouteSurvive needs root, a
// bridge, a dnsmasq and a container, and takes five minutes -- so
// without this file, the one piece of logic that produces the verdict
// is never executed against a case whose answer is known in advance.
//
// It costs nothing and runs before anything is touched.
//
// THE ROWS ARE THE MEASUREMENTS FROM #875, NOT INVENTED NUMBERS. The
// first two are the two series the issue was opened on, and the first
// thing this file establishes is that they are DIFFERENT to this
// function -- which is what the issue could not establish by comparing
// them to each other, because both are countdowns and the two arms were
// not shown to have been sampled at the same elapsed times.

// lftPoint is one synthetic reading: elapsed seconds, valid_lft,
// preferred_lft. A negative lifetime means `forever`.
type lftPoint struct{ elapsed, valid, pref int }

func synthAddrOutput(p lftPoint) string {
	if p.valid < 0 {
		return "    inet6 " + harness.V6Prefix + "1234/64 scope global \n" +
			"       valid_lft forever preferred_lft forever\n"
	}
	return fmt.Sprintf(
		"    inet6 "+harness.V6Prefix+"1234/64 scope global dynamic mngtmpaddr proto kernel_ra \n"+
			"       valid_lft %dsec preferred_lft %dsec\n", p.valid, p.pref)
}

func synthSamples(series []lftPoint) []selfConfigSample {
	var out []selfConfigSample
	for _, p := range series {
		out = append(out, selfConfigSample{
			elapsed: time.Duration(p.elapsed) * time.Second,
			addr:    synthAddrOutput(p),
		})
	}
	return out
}

// recordingReporter stands in for *testing.T so a verdict can be read
// rather than propagated. Driving the real T would fail THIS test on
// every row whose expected outcome is a failure.
type recordingReporter struct {
	failed bool
	msgs   []string
}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.failed = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Logf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Helper() {}

func TestSelfConfigLedger_DiscriminatesRefreshFromCountdown(t *testing.T) {
	cases := []struct {
		name     string
		series   []lftPoint
		wantFail bool
		why      string
	}{
		{
			name:     "the measured defect series counts down",
			series:   []lftPoint{{0, 1797, 1797}, {10, 1787, 1787}, {150, 1647, 1647}},
			wantFail: true,
			why: "this is the dhcpcd arm measured in #875. The lifetime falls exactly as " +
				"fast as the clock rises, so the ceiling is flat and nothing is refreshing " +
				"the address",
		},
		{
			name:     "the measured control series refreshes",
			series:   []lftPoint{{0, 1797, 1797}, {10, 1796, 1796}, {150, 1705, 1705}},
			wantFail: false,
			why: "the no-dhcpcd control from #875. Read WITHIN the arm -- which is the only " +
				"way it can be read -- its ceiling rises 1797 -> 1806 -> 1855, so an " +
				"advertisement reset it at least once. This row and the one above it are " +
				"the two the issue could not tell apart by comparing them to each other",
		},
		{
			name:     "a literal increase is a refresh",
			series:   []lftPoint{{0, 1790, 1790}, {30, 1760, 1760}, {60, 1795, 1795}},
			wantFail: false,
			why:      "the unambiguous case: the value itself goes back up",
		},
		{
			name:     "one second of sampling jitter is not a refresh",
			series:   []lftPoint{{0, 1797, 1797}, {10, 1788, 1788}, {150, 1648, 1648}},
			wantFail: true,
			why: "the ceiling moves by 1s, which is rounding between `ip`'s whole seconds " +
				"and a separately-read clock. refreshFloor exists so this is not read as " +
				"a refresh; a floor of 0 would pass this row and pass the defect too",
		},
		{
			name:     "preferred refreshes while valid is pinned",
			series:   []lftPoint{{0, 7200, 1790}, {30, 7170, 1760}, {60, 7140, 1795}},
			wantFail: false,
			why: "RFC 4862 5.5.3(e) resets the preferred lifetime unconditionally but resets " +
				"the valid lifetime only under its two conditions. This row is why the " +
				"verdict is keyed on preferred: keyed on valid, it would report a defect " +
				"on a segment that is refreshing correctly",
		},
		{
			name:     "no address in any sample is not a silent pass",
			series:   nil,
			wantFail: true,
			why: "the vacuity direction. With no readings there is no sequence, and a " +
				"function that returned quietly here would report a container that had " +
				"LOST its address as a container whose address is fine",
		},
		{
			name:     "a single sample is not a sequence",
			series:   []lftPoint{{0, 1797, 1797}},
			wantFail: true,
			why:      "one reading cannot show a change; the comparison would be trivially satisfied",
		},
		{
			name:     "a statically applied address is reported, not measured",
			series:   []lftPoint{{0, -1, -1}, {10, -1, -1}},
			wantFail: true,
			why: "`forever` means nobody is ageing this address, so it did not come from an " +
				"advertisement. On a self-configuring segment that is itself wrong, and it " +
				"must not be silently unmeasurable",
		},
	}

	// NON-VACUITY, and it is the reason this file exists at all: a
	// table with no failing row and no passing row does not test a
	// discriminator, it tests a constant. Both polarities are required,
	// and the two MEASURED series must both be present -- they are the
	// pair the whole issue turns on, and a table that dropped either
	// would still look like a thorough test.
	pol := map[bool]int{}
	for _, tc := range cases {
		pol[tc.wantFail]++
	}
	if pol[true] < 1 || pol[false] < 1 {
		t.Fatalf("the table has %d rows expecting a verdict of FAIL and %d expecting PASS; "+
			"both are needed, or this file cannot tell a discriminator from a function "+
			"that always returns the same answer", pol[true], pol[false])
	}
	for _, needed := range []string{
		"the measured defect series counts down",
		"the measured control series refreshes",
	} {
		found := false
		for _, tc := range cases {
			if tc.name == needed {
				found = true
			}
		}
		if !found {
			t.Fatalf("the row %q is missing. The two measured series from #875 are the pair "+
				"this ledger exists to tell apart; a table without both is not checking "+
				"the claim the issue rests on", needed)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingReporter{}
			assertLifetimeRefreshed(r, synthSamples(tc.series), harness.V6SLAAC)
			if r.failed != tc.wantFail {
				verdict, want := "PASS", "FAIL"
				if r.failed {
					verdict, want = "FAIL", "PASS"
				}
				t.Errorf("the ledger returned %s and should have returned %s.\n\nWhy this row "+
					"exists: %s\n\nWhat it reported:\n%s",
					verdict, want, tc.why, strings.Join(r.msgs, "\n"))
			}
		})
	}
}
