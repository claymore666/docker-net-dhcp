// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import "testing"

// TestConflictCensusFindings pins the gate behind the census (#551).
//
// The case that matters most is "the motivating run": one probe reached
// a verdict and two could not run. Every naive formulation of this gate
// passes that input, which is why it went unnoticed from #527 to #550
// while the line printed the problem on every single run.
func TestConflictCensusFindings(t *testing.T) {
	fatalCounters := func(fs []FloorFinding) []string {
		var out []string
		for _, f := range fs {
			if f.Fatal {
				out = append(out, f.Counter)
			}
		}
		return out
	}

	cases := []struct {
		name    string
		h       *HealthResponse
		allowed int32
		logFail int      // failures visible in the log, scoped to this process
		want    []string // fatal counters, in order
		// baseline is the plugin's counters when THIS process started.
		// nil means the plugin was started for this run, which is what
		// every sharded lane gives us and what the coverage lane does
		// not — it drives one plugin through both suites.
		baseline *HealthResponse
	}{
		{
			name: "nil health says nothing",
			h:    nil,
		},
		{
			name: "probes reached verdicts and none failed",
			h:    &HealthResponse{AddressConflictProbes: 4, LeasesObtained: 4},
		},
		{
			// THE motivating run, #527 through #550. A gate that passes
			// this is the gate we already had.
			name:    "one verdict and two failures, none declared, fails",
			h:       &HealthResponse{AddressConflictProbes: 1, ConflictProbeFailures: 2, LeasesObtained: 3},
			allowed: 0,
			want:    []string{"conflict_probe_failures"},
		},
		{
			// TestAddressConflict_BareParentIsUndetermined degrades a
			// probe on purpose. Declared, so it is not a finding.
			name:    "a declared deliberate failure is not a finding",
			h:       &HealthResponse{AddressConflictProbes: 3, ConflictProbeFailures: 1, LeasesObtained: 4},
			allowed: 1,
			want:    nil,
		},
		{
			name:    "one failure beyond the declared allowance fails",
			h:       &HealthResponse{AddressConflictProbes: 3, ConflictProbeFailures: 2, LeasesObtained: 5},
			allowed: 1,
			want:    []string{"conflict_probe_failures"},
		},
		{
			// A shard that leased nothing has nothing to probe. Failing
			// here would make the verdict depend on how the partitioner
			// balanced the run.
			name: "a shard that leased no v4 address is not a failure",
			h:    &HealthResponse{AddressConflictProbes: 0, LeasesObtained: 0},
		},
		{
			// Distinct from a failed probe: nothing was even attempted.
			name: "leases obtained but the detector never ran fails",
			h:    &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 6},
			want: []string{"address_conflict_probes"},
		},
		{
			// v6 has its own counter; leases_obtained is v4-only, so a
			// v6-only shard reads as "nothing to probe", not as a fault.
			name: "a v6-only shard does not trip the never-ran case",
			h:    &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 0},
		},
		{
			// THE reason the log is read at all. The plugin restarts
			// mid-suite and its counters reset with it, so a probe that
			// failed before the restart leaves the counter at zero. The
			// log does not reset. A counter-only gate reads this as a
			// clean run.
			name:    "counters clean but the log records failures still fails",
			h:       &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 0, LeasesObtained: 2},
			allowed: 0,
			logFail: 3,
			want:    []string{"conflict_probe_failures"},
		},
		{
			name:    "the log is judged against the allowance too",
			h:       &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 0, LeasesObtained: 2},
			allowed: 1,
			logFail: 1,
			want:    nil,
		},
		{
			// The counter can only under-report; the larger wins.
			name:    "the counter wins when it is the larger of the two",
			h:       &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 4, LeasesObtained: 2},
			allowed: 0,
			logFail: 1,
			want:    []string{"conflict_probe_failures"},
		},
		{
			// A probe failure recorded only in the log still means the
			// detector ran, so this is not "never invoked".
			name:    "log failures alone do not also raise the never-ran finding",
			h:       &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 5},
			allowed: 9,
			logFail: 2,
			want:    nil,
		},
		{
			// THE coverage-lane bug. The main suite declared and caused
			// one probe failure, then exited; this process starts with an
			// allowance of 0 against a plugin whose counter is still 1.
			// Judged cumulatively that is an unexplained failure and the
			// release PR goes red; judged against the baseline it is
			// nothing to do with this process.
			name:     "a failure that predates this process is not ours",
			h:        &HealthResponse{AddressConflictProbes: 5, ConflictProbeFailures: 1, LeasesObtained: 5},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 1, LeasesObtained: 4},
			want:     nil,
		},
		{
			// The same baseline must not hide a NEW one. This is the
			// direction that matters: a fix which only ever silences
			// findings is not a fix.
			name:     "a failure after the baseline is still ours",
			h:        &HealthResponse{AddressConflictProbes: 5, ConflictProbeFailures: 2, LeasesObtained: 5},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 1, LeasesObtained: 4},
			want:     []string{"conflict_probe_failures"},
		},
		{
			// Counters below the baseline mean the plugin restarted and
			// reset. The current value is then already scoped to the
			// restart, so it is used as-is. Clamping to zero here would
			// report a clean run for one in which the plugin died — #385
			// exactly.
			name:     "a counter below the baseline is a restart, not a negative",
			h:        &HealthResponse{AddressConflictProbes: 1, ConflictProbeFailures: 2, LeasesObtained: 1},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 9, ConflictProbeFailures: 7, LeasesObtained: 9},
			want:     []string{"conflict_probe_failures"},
		},
		{
			// The never-ran check has to be scoped too. Cumulatively the
			// plugin has probed plenty; this process leased addresses and
			// probed none of them, which is the blindness #551 is about.
			name:     "the detector not running in THIS process is still a finding",
			h:        &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 0, LeasesObtained: 7},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 0, LeasesObtained: 4},
			want:     []string{"address_conflict_probes"},
		},
		{
			name:    "both faults are reported together, not just the first",
			h:       &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 2},
			allowed: 0,
			want:    []string{"address_conflict_probes"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fatalCounters(ConflictCensusFindings(tc.h, tc.allowed, tc.logFail, tc.baseline))
			if len(got) != len(tc.want) {
				t.Fatalf("fatal counters = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("fatal counter[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// An absent counter is not a zero. If the plugin never published these,
// the census cannot be judged, and reading <not reported> as 0 would
// rebuild exactly the blindness this gate closes.
func TestConflictCensusFindingsAbsentCounter(t *testing.T) {
	// A payload that publishes leases but neither census counter.
	h := decodeHealth(t, `{"healthy":true,"leases_obtained":3}`)

	got := ConflictCensusFindings(h, 0, 0, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %v, want exactly one", got)
	}
	if !got[0].Absent {
		t.Errorf("Absent = false, want true — an unpublished counter must not be judged as zero")
	}
	if !got[0].Fatal {
		t.Errorf("Fatal = false, want true")
	}
}

// The allowance is additive and survives being declared from more than
// one place, because more than one test may legitimately degrade a probe.
func TestAllowConflictProbeFailuresAccumulates(t *testing.T) {
	before := AllowedConflictProbeFailures()
	t.Cleanup(func() {
		conflictAllowance.mu.Lock()
		conflictAllowance.n = before
		conflictAllowance.mu.Unlock()
	})

	AllowConflictProbeFailures(1)
	AllowConflictProbeFailures(2)
	if got, want := AllowedConflictProbeFailures(), before+3; got != want {
		t.Errorf("AllowedConflictProbeFailures() = %d, want %d", got, want)
	}
}

// ConflictProbeFailuresInLog must count every line the plugin writes at
// a conflict_probe_failures increment — there are three, and counting
// only the obvious one under-reports exactly the runs worth failing.
func TestConflictProbeFailuresInLog(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want int
	}{
		{name: "empty log", log: "", want: 0},
		{name: "no probe lines", log: "level=info msg=\"Network created\"\nlevel=trace msg=x\n", want: 0},
		{
			name: "the probe-could-not-run line",
			log:  "level=warning msg=\"[conflict-probe] address-conflict probe could not run\"\n",
			want: 1,
		},
		{
			name: "the unparseable-address line counts too",
			log:  "level=warning msg=\"[conflict-probe] cannot parse leased address; address conflict not checked\"\n",
			want: 1,
		},
		{
			name: "the unparseable-MAC line counts too",
			log:  "level=warning msg=\"[conflict-probe] cannot parse endpoint MAC; address conflict not checked\"\n",
			want: 1,
		},
		{
			name: "all three, plus repeats",
			log: "level=warning msg=\"[conflict-probe] address-conflict probe could not run\"\n" +
				"level=warning msg=\"[conflict-probe] address-conflict probe could not run\"\n" +
				"level=warning msg=\"[conflict-probe] cannot parse leased address; address conflict not checked\"\n" +
				"level=warning msg=\"[conflict-probe] cannot parse endpoint MAC; address conflict not checked\"\n",
			want: 4,
		},
		{
			// The success and cleanup lines share the [conflict-probe]
			// prefix and must not be counted as failures.
			name: "a clean verdict and a cleanup warning are not failures",
			log: "level=debug msg=\"[conflict-probe] leased address is not held by another device\"\n" +
				"level=warning msg=\"[conflict-probe] could not remove the temporary probe address; remove it with `ip addr del`\"\n",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConflictProbeFailuresInLog([]byte(tc.log)); got != tc.want {
				t.Errorf("ConflictProbeFailuresInLog = %d, want %d", got, tc.want)
			}
		})
	}
}
