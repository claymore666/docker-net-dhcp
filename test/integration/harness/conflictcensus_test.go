// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"strings"
	"testing"
	"time"
)

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
		logFail int // failures visible in the log, scoped to this process
		// logDispatched/logSettled are the WHOLE-RUN log census (#881).
		// They are what stops a plugin restart emptying this gate's domain.
		logDispatched int
		logSettled    int
		want          []string // fatal counters, in order
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
			h:    &HealthResponse{AddressConflictProbes: 4, LeasesObtained: 4, ConflictProbesDispatched: 4, ConflictProbesSettled: 4},
		},
		{
			// THE motivating run, #527 through #550. A gate that passes
			// this is the gate we already had.
			name:    "one verdict and two failures, none declared, fails",
			h:       &HealthResponse{AddressConflictProbes: 1, ConflictProbeFailures: 2, LeasesObtained: 3, ConflictProbesDispatched: 3, ConflictProbesSettled: 3},
			allowed: 0,
			want:    []string{"conflict_probe_failures"},
		},
		{
			// TestAddressConflict_BareParentIsUndetermined degrades a
			// probe on purpose. Declared, so it is not a finding.
			name:    "a declared deliberate failure is not a finding",
			h:       &HealthResponse{AddressConflictProbes: 3, ConflictProbeFailures: 1, LeasesObtained: 4, ConflictProbesDispatched: 4, ConflictProbesSettled: 4},
			allowed: 1,
			want:    nil,
		},
		{
			name:    "one failure beyond the declared allowance fails",
			h:       &HealthResponse{AddressConflictProbes: 3, ConflictProbeFailures: 2, LeasesObtained: 5, ConflictProbesDispatched: 5, ConflictProbesSettled: 5},
			allowed: 1,
			want:    []string{"conflict_probe_failures"},
		},
		{
			// A shard that leased nothing has nothing to probe. Failing
			// here would make the verdict depend on how the partitioner
			// balanced the run.
			name: "a shard that dispatched no probe is not a failure",
			h:    &HealthResponse{AddressConflictProbes: 0, LeasesObtained: 0},
		},
		{
			// THE #881 REGRESSION CASE, and the one that inverts.
			//
			// Six v4 leases and no probe dispatched. Under the old
			// operand this was FATAL; it is the shape a plugin restart
			// produces every time, because recovery re-attaches an
			// endpoint and its persistent client binds in a process that
			// never created it and so never dispatched a probe for it.
			// Four runs failed on exactly this with every test passing.
			//
			// Restoring `leases > 0` in place of `dispatched > 0` makes
			// this case go red, which is what mutant 1 checks.
			name: "leases across a restart with nothing dispatched is not a finding",
			h:    &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 6},
			want: nil,
		},
		{
			// THE PRESERVATION CONTROL for the case above. Widening the
			// domain must not make the gate decorative: probes really
			// dispatched and not one reaching a verdict is still the
			// detector having stopped, and still fatal.
			name: "probes dispatched and none reaching a verdict still fails",
			h: &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 6,
				ConflictProbesDispatched: 6, ConflictProbesSettled: 6},
			want: []string{"address_conflict_probes"},
		},
		{
			// VACUITY, driven rather than assumed. A plugin restart late
			// in a shard empties the counters, and an emptied domain
			// passes silently — the defect #881 reports, one domain over.
			// The log spans the run and no restart truncates it, so
			// counters at zero over a log that records dispatches is a
			// reset, not a quiet run.
			name: "counters emptied by a restart are judged against the log",
			h: &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 4,
				ConflictProbesDispatched: 0, ConflictProbesSettled: 0},
			logDispatched: 2,
			logSettled:    2,
			want:          []string{"conflict_probes_dispatched"},
		},
		{
			// The honest empty case, and it must stay quiet: nothing
			// dispatched and the log agrees. A gate that fired here would
			// fail every v6-only shard.
			name: "nothing dispatched and the log agrees is the honest empty case",
			h: &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 0,
				ConflictProbesDispatched: 0, ConflictProbesSettled: 0},
			logDispatched: 0,
			logSettled:    0,
			want:          nil,
		},
		{
			// A probe launched somewhere in the run that never came
			// back. The floor joins in-flight probes before it reads, so
			// this is not "not finished yet".
			name: "dispatched in the log and never settled fails",
			h: &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 0, LeasesObtained: 4,
				ConflictProbesDispatched: 2, ConflictProbesSettled: 2},
			logDispatched: 3,
			logSettled:    0,
			want:          []string{"conflict_probes_settled"},
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
			h:       &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 0, LeasesObtained: 2, ConflictProbesDispatched: 2, ConflictProbesSettled: 2},
			allowed: 0,
			logFail: 3,
			want:    []string{"conflict_probe_failures"},
		},
		{
			name:    "the log is judged against the allowance too",
			h:       &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 0, LeasesObtained: 2, ConflictProbesDispatched: 2, ConflictProbesSettled: 2},
			allowed: 1,
			logFail: 1,
			want:    nil,
		},
		{
			// The counter can only under-report; the larger wins.
			name:    "the counter wins when it is the larger of the two",
			h:       &HealthResponse{AddressConflictProbes: 2, ConflictProbeFailures: 4, LeasesObtained: 2, ConflictProbesDispatched: 2, ConflictProbesSettled: 2},
			allowed: 0,
			logFail: 1,
			want:    []string{"conflict_probe_failures"},
		},
		{
			// A probe failure recorded only in the log still means the
			// detector ran, so this is not "never invoked".
			name:    "log failures alone do not also raise the never-ran finding",
			h:       &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 5, ConflictProbesDispatched: 2, ConflictProbesSettled: 2},
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
			h:        &HealthResponse{AddressConflictProbes: 5, ConflictProbeFailures: 1, LeasesObtained: 5, ConflictProbesDispatched: 5, ConflictProbesSettled: 5},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 1, LeasesObtained: 4, ConflictProbesDispatched: 4, ConflictProbesSettled: 4},
			want:     nil,
		},
		{
			// The same baseline must not hide a NEW one. This is the
			// direction that matters: a fix which only ever silences
			// findings is not a fix.
			name:     "a failure after the baseline is still ours",
			h:        &HealthResponse{AddressConflictProbes: 5, ConflictProbeFailures: 2, LeasesObtained: 5, ConflictProbesDispatched: 5, ConflictProbesSettled: 5},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 1, LeasesObtained: 4, ConflictProbesDispatched: 4, ConflictProbesSettled: 4},
			want:     []string{"conflict_probe_failures"},
		},
		{
			// Counters below the baseline mean the plugin restarted and
			// reset. The current value is then already scoped to the
			// restart, so it is used as-is. Clamping to zero here would
			// report a clean run for one in which the plugin died — #385
			// exactly.
			name:     "a counter below the baseline is a restart, not a negative",
			h:        &HealthResponse{AddressConflictProbes: 1, ConflictProbeFailures: 2, LeasesObtained: 1, ConflictProbesDispatched: 1, ConflictProbesSettled: 1},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 9, ConflictProbeFailures: 7, LeasesObtained: 9, ConflictProbesDispatched: 9, ConflictProbesSettled: 9},
			want:     []string{"conflict_probe_failures"},
		},
		{
			// The never-ran check has to be scoped too. Cumulatively the
			// plugin has probed plenty; this process leased addresses and
			// probed none of them, which is the blindness #551 is about.
			name:     "the detector not running in THIS process is still a finding",
			h:        &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 0, LeasesObtained: 7, ConflictProbesDispatched: 7, ConflictProbesSettled: 7},
			allowed:  0,
			baseline: &HealthResponse{AddressConflictProbes: 4, ConflictProbeFailures: 0, LeasesObtained: 4, ConflictProbesDispatched: 4, ConflictProbesSettled: 4},
			want:     []string{"address_conflict_probes"},
		},
		{
			name:    "both faults are reported together, not just the first",
			h:       &HealthResponse{AddressConflictProbes: 0, ConflictProbeFailures: 0, LeasesObtained: 2, ConflictProbesDispatched: 2, ConflictProbesSettled: 2},
			allowed: 0,
			want:    []string{"address_conflict_probes"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fatalCounters(ConflictCensusFindings(tc.h, tc.allowed, tc.logFail, tc.baseline,
				tc.logDispatched, tc.logSettled))
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

	got := ConflictCensusFindings(h, 0, 0, nil, 0, 0)
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

// The JOIN is what stops the floor judging a counter that has not
// finished being written (#881).
//
// checkAddressConflict is dispatched as a goroutine and nothing waits
// for it. Run 33156482028 dispatched the shard's only probe and read
// /Plugin.Health in the same second, then failed the run for the probe
// not having happened. These pin the condition the floor waits on.
func TestProbesOutstanding(t *testing.T) {
	cases := []struct {
		name string
		h    *HealthResponse
		want int32
	}{
		{"nil health has nothing outstanding", nil, 0},
		{"nothing dispatched, nothing outstanding",
			&HealthResponse{}, 0},
		{"all settled",
			&HealthResponse{ConflictProbesDispatched: 4, ConflictProbesSettled: 4}, 0},
		{"one still in flight",
			&HealthResponse{ConflictProbesDispatched: 4, ConflictProbesSettled: 3}, 1},
		{"the whole batch in flight",
			&HealthResponse{ConflictProbesDispatched: 3, ConflictProbesSettled: 0}, 3},
		{
			// A broken invariant, not a reason to wait. Reporting this
			// as outstanding would make the floor burn its whole budget
			// on a plugin bug it should be surfacing instead.
			name: "settled ahead of dispatched is not outstanding",
			h:    &HealthResponse{ConflictProbesDispatched: 1, ConflictProbesSettled: 5},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProbesOutstanding(tc.h); got != tc.want {
				t.Errorf("ProbesOutstanding = %d, want %d", got, tc.want)
			}
		})
	}
}

// Expiry with probes outstanding is FATAL, never a fall-through.
//
// Both alternatives are mistakes this project has already made once:
// judging anyway reports "the detector never ran" for a probe that was
// still running, and staying quiet is an opt-out with a timer on it.
func TestConflictProbeJoinFinding(t *testing.T) {
	t.Run("a completed join says nothing", func(t *testing.T) {
		h := &HealthResponse{ConflictProbesDispatched: 5, ConflictProbesSettled: 5}
		if got := ConflictProbeJoinFinding(h, time.Minute); got != nil {
			t.Errorf("findings = %v, want none — every probe settled", got)
		}
	})

	t.Run("an incomplete join is fatal", func(t *testing.T) {
		h := &HealthResponse{ConflictProbesDispatched: 5, ConflictProbesSettled: 2}
		got := ConflictProbeJoinFinding(h, time.Minute)
		if len(got) != 1 {
			t.Fatalf("findings = %v, want exactly one", got)
		}
		if !got[0].Fatal {
			t.Error("the finding is not fatal; a probe that never returned must fail the run, " +
				"not warn about it")
		}
		if got[0].Counter != "conflict_probes_settled" {
			t.Errorf("counter = %q, want conflict_probes_settled", got[0].Counter)
		}
		// The operator has to be able to tell how far off it was.
		for _, want := range []string{"3 conflict probe(s)", "dispatched=5", "settled=2"} {
			if !strings.Contains(got[0].Why, want) {
				t.Errorf("why missing %q: %s", want, got[0].Why)
			}
		}
	})
}

// The whole-run log census is what survives a plugin restart (#881).
//
// The counters reset when the plugin does, and shard 4 of the 5-way
// split restarts it twice — so a counter-only census judges the last few
// seconds of a three-minute run. The log spans the run.
func TestConflictProbeCensusInLog(t *testing.T) {
	const dispatch = `time="..." level=info msg="[conflict-probe] dispatched" endpoint=abc`
	const settle = `time="..." level=info msg="[conflict-probe] settled" outcome=clean`

	cases := []struct {
		name                string
		log                 string
		wantDisp, wantSettl int
	}{
		{"an empty log counts nothing", "", 0, 0},
		{"a log with neither line counts nothing", "some other line\nand another", 0, 0},
		{"one dispatch and one settle", dispatch + "\n" + settle, 1, 1},
		{"three dispatches, two settled",
			strings.Join([]string{dispatch, dispatch, dispatch, settle, settle}, "\n"), 3, 2},
		{
			// The failure-census messages must NOT be counted here.
			// Folding them together would turn every unrunnable probe
			// into an unexplained failure and fail runs for the opposite
			// reason.
			name:     "a probe-failure line is not a dispatch or a settle",
			log:      `time="..." level=warning msg="[conflict-probe] address-conflict probe could not run"`,
			wantDisp: 0, wantSettl: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, s := ConflictProbeCensusInLog([]byte(tc.log))
			if d != tc.wantDisp || s != tc.wantSettl {
				t.Errorf("census = (%d dispatched, %d settled), want (%d, %d)",
					d, s, tc.wantDisp, tc.wantSettl)
			}
		})
	}
}

// A probe that settles without having been dispatched means a call site
// bypassed the dispatcher (#881).
//
// This is the hole every other finding here reads as the honest empty
// case. It is the fix's own defect class — a gate satisfied by emptying
// its domain — so it is driven rather than reasoned about.
func TestConflictCensus_SettledWithoutDispatchFails(t *testing.T) {
	h := &HealthResponse{AddressConflictProbes: 2, ConflictProbesDispatched: 0, ConflictProbesSettled: 2}

	got := ConflictCensusFindings(h, 0, 0, nil, 0, 2)

	var fatal []string
	for _, f := range got {
		if f.Fatal {
			fatal = append(fatal, f.Counter)
		}
	}
	if len(fatal) != 1 || fatal[0] != "conflict_probes_dispatched" {
		t.Fatalf("fatal counters = %v, want exactly [conflict_probes_dispatched]. "+
			"A probe settling with nothing dispatched leaves the census with no "+
			"population to judge, and going quiet there is this issue's own defect.", fatal)
	}
}
