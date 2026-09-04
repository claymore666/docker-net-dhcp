// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"strings"
	"testing"
)

// TestACDCensusFindings pins the gate behind the census (#551).
//
// The case that matters most is "the motivating run": one probe reached
// a verdict and two could not run. Every naive formulation of this gate
// passes that input, which is why it went unnoticed from #527 to #550
// while the line printed the problem on every single run. The mechanism
// underneath is now RFC 5227 in the DHCP library rather than the
// chassis's datagram probe, so the counters are new; the property, and
// every case below, is the one that was earned.
func TestACDCensusFindings(t *testing.T) {
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
		name        string
		h           *HealthResponse
		allowedSend int32
		allowedUnpr int32
		allowedConf int32
		conflictLog int      // conflicts visible in the log, scoped to this process
		want        []string // fatal counters, in order
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
			name: "probes went out and no send was refused",
			h:    &HealthResponse{ACDProbesSent: 4, LeasesObtained: 4},
		},
		{
			// THE motivating run, #527 through #550. A gate that passes
			// this is the gate we already had.
			name:        "one probe out and two sends refused, none declared, fails",
			h:           &HealthResponse{ACDProbesSent: 1, ACDARPSendFailures: 2, LeasesObtained: 3},
			allowedSend: 0,
			want:        []string{"acd_arp_send_failures"},
		},
		{
			// A test that degrades the ARP socket on purpose declares
			// it, so it is not a finding.
			name:        "a declared deliberate refusal is not a finding",
			h:           &HealthResponse{ACDProbesSent: 3, ACDARPSendFailures: 1, LeasesObtained: 4},
			allowedSend: 1,
			want:        nil,
		},
		{
			name:        "one refusal beyond the declared allowance fails",
			h:           &HealthResponse{ACDProbesSent: 3, ACDARPSendFailures: 2, LeasesObtained: 5},
			allowedSend: 1,
			want:        []string{"acd_arp_send_failures"},
		},
		{
			// A shard that leased nothing has nothing to check. Failing
			// here would make the verdict depend on how the partitioner
			// balanced the run.
			name: "a shard that leased no v4 address is not a failure",
			h:    &HealthResponse{ACDProbesSent: 0, LeasesObtained: 0},
		},
		{
			// Distinct from a refused send: nothing was even attempted.
			name: "leases obtained but the check never ran fails",
			h:    &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 6},
			want: []string{"acd_probes_sent"},
		},
		{
			// NEW, and the reason the never-ran gate needed rebuilding
			// when the datagram probe went away: the check is now
			// opt-out per network. A shard whose leases were all taken
			// on conflict_check=off networks reaches zero probes on the
			// operator's own instruction, and declares it.
			name:        "declared conflict_check=off leases are not a never-ran finding",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 3},
			allowedUnpr: 3,
			want:        nil,
		},
		{
			// The widening's preservation control. Declaring SOME
			// off-mode leases must not excuse the rest: one lease was
			// taken on a network that was supposed to probe, and none
			// did.
			name:        "an undeclared lease among declared ones still fails",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 4},
			allowedUnpr: 3,
			want:        []string{"acd_probes_sent"},
		},
		{
			// And the allowance cannot manufacture a pass by exceeding
			// the leases — the subtraction goes negative, which is not
			// a finding, but neither is it a licence: there was nothing
			// to check either way.
			name:        "an over-declared allowance is still not a finding",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 1},
			allowedUnpr: 9,
			want:        nil,
		},
		{
			// v6 has its own counter; leases_obtained is v4-only, so a
			// v6-only shard reads as "nothing to check", not as a fault.
			name: "a v6-only shard does not trip the never-ran case",
			h:    &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 0},
		},
		{
			// THE reason the log is read at all. The plugin restarts
			// mid-suite and its counters reset with it, so a conflict
			// found before the restart leaves the counter at zero. The
			// log does not reset. A counter-only gate reads this as a
			// clean run — and a conflict is a container up on somebody
			// else's address.
			name:        "counters clean but the log records conflicts still fails",
			h:           &HealthResponse{ACDProbesSent: 2, LeasesObtained: 2},
			conflictLog: 3,
			want:        []string{"address_conflicts"},
		},
		{
			// The counter can only under-report; the larger wins, and
			// when the counter IS the larger there is nothing extra to
			// say.
			name:        "the counter agreeing with the log is not a finding",
			h:           &HealthResponse{ACDProbesSent: 2, AddressConflicts: 4, LeasesObtained: 2},
			conflictLog: 1,
			want:        nil,
		},
		{
			// A refused send recorded nowhere but the counter still
			// means the check ran, so this is not "never invoked".
			name:        "a refused send alone does not also raise the never-ran finding",
			h:           &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 2, LeasesObtained: 5},
			allowedSend: 9,
			want:        nil,
		},
		{
			// THE coverage-lane bug. The main suite declared and caused
			// one refusal, then exited; this process starts with an
			// allowance of 0 against a plugin whose counter is still 1.
			// Judged cumulatively that is an unexplained failure and the
			// release PR goes red; judged against the baseline it is
			// nothing to do with this process.
			name:        "a refusal that predates this process is not ours",
			h:           &HealthResponse{ACDProbesSent: 5, ACDARPSendFailures: 1, LeasesObtained: 5},
			allowedSend: 0,
			baseline:    &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 1, LeasesObtained: 4},
			want:        nil,
		},
		{
			// The same baseline must not hide a NEW one. This is the
			// direction that matters: a fix which only ever silences
			// findings is not a fix.
			name:        "a refusal after the baseline is still ours",
			h:           &HealthResponse{ACDProbesSent: 5, ACDARPSendFailures: 2, LeasesObtained: 5},
			allowedSend: 0,
			baseline:    &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 1, LeasesObtained: 4},
			want:        []string{"acd_arp_send_failures"},
		},
		{
			// Counters below the baseline mean the plugin restarted and
			// reset. The current value is then already scoped to the
			// restart, so it is used as-is. Clamping to zero here would
			// report a clean run for one in which the plugin died — #385
			// exactly.
			name:        "a counter below the baseline is a restart, not a negative",
			h:           &HealthResponse{ACDProbesSent: 1, ACDARPSendFailures: 2, LeasesObtained: 1},
			allowedSend: 0,
			baseline:    &HealthResponse{ACDProbesSent: 9, ACDARPSendFailures: 7, LeasesObtained: 9},
			want:        []string{"acd_arp_send_failures"},
		},
		{
			// The never-ran check has to be scoped too. Cumulatively the
			// plugin has probed plenty; this process leased addresses and
			// probed none of them, which is the blindness #551 is about.
			name:     "the check not running in THIS process is still a finding",
			h:        &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 0, LeasesObtained: 7},
			baseline: &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 0, LeasesObtained: 4},
			want:     []string{"acd_probes_sent"},
		},
		{
			// Every fault is reported, not just the first: the run has
			// unexplained refusals AND a conflict the counter lost.
			name:        "faults are reported together, not just the first",
			h:           &HealthResponse{ACDProbesSent: 2, ACDARPSendFailures: 3, LeasesObtained: 2},
			allowedSend: 0,
			conflictLog: 1,
			want:        []string{"acd_arp_send_failures", "address_conflicts"},
		},
		{
			// The lane case. A conflict case staged two conflicts and
			// declared them; a later test recycled the plugin, so the
			// counter reset out from under the log. Nothing was
			// dropped by the seam and nothing should be red.
			name:        "staged conflicts the counter lost to a restart are declared, not red",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4},
			allowedConf: 2,
			conflictLog: 2,
		},
		{
			// THE PRESERVATION CONTROL, and the reason the allowance is
			// a subtraction rather than an exemption: one more conflict
			// than the shard staged is still the seam dropping an
			// event, which is #524 restored.
			name:        "one conflict more than declared is still fatal",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4},
			allowedConf: 2,
			conflictLog: 3,
			want:        []string{"address_conflicts"},
		},
		{
			// A declaration cannot make an UNDECLARED shard pass: with
			// nothing staged the row is exactly what it was.
			name:        "an undeclared conflict the counter never saw is fatal",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4},
			conflictLog: 1,
			want:        []string{"address_conflicts"},
		},
		{
			// And the counter agreeing with the log is clean whether or
			// not anything was declared — a declaration is a licence to
			// under-report, never a requirement to.
			name:        "the counter matching the log is clean with a declaration standing",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4, AddressConflicts: 2},
			allowedConf: 2,
			conflictLog: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fatalCounters(ACDCensusFindings(tc.h, tc.allowedSend, tc.allowedUnpr, tc.allowedConf, tc.conflictLog, tc.baseline))
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
func TestACDCensusFindingsAbsentCounter(t *testing.T) {
	// A payload that publishes leases but neither census counter.
	h := decodeHealth(t, `{"healthy":true,"leases_obtained":3}`)

	got := ACDCensusFindings(h, 0, 0, 0, 0, nil)
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

// The allowances are additive and survive being declared from more than
// one place, because more than one test may legitimately cause them.
func TestACDAllowancesAccumulate(t *testing.T) {
	beforeSend, beforeUnpr := AllowedARPSendFailures(), AllowedUnprobedLeases()
	t.Cleanup(func() {
		acdAllowance.mu.Lock()
		acdAllowance.sendFail, acdAllowance.unprobed = beforeSend, beforeUnpr
		acdAllowance.mu.Unlock()
	})

	AllowARPSendFailures(1)
	AllowARPSendFailures(2)
	if got, want := AllowedARPSendFailures(), beforeSend+3; got != want {
		t.Errorf("AllowedARPSendFailures() = %d, want %d", got, want)
	}

	AllowUnprobedLeases(4)
	if got, want := AllowedUnprobedLeases(), beforeUnpr+4; got != want {
		t.Errorf("AllowedUnprobedLeases() = %d, want %d", got, want)
	}
	// The two must not be one counter under two names: declaring an
	// off-mode lease would then quietly excuse a refused send.
	if got, want := AllowedARPSendFailures(), beforeSend+3; got != want {
		t.Errorf("AllowUnprobedLeases moved the send-failure allowance: %d, want %d", got, want)
	}
}

// ConflictsInLog must count every line the plugin writes at an
// address_conflicts increment — there are two, and counting only the
// obvious one under-reports exactly the runs worth failing.
func TestConflictsInLog(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want int
	}{
		{name: "empty log", log: "", want: 0},
		{name: "no conflict lines", log: "level=info msg=\"Network created\"\nlevel=trace msg=x\n", want: 0},
		{
			name: "the probe-window line",
			log:  "level=error msg=\"" + conflictProbeMsg + " (RFC 5227).\" network=abc\n",
			want: 1,
		},
		{
			name: "the held-address line counts too",
			log:  "level=error msg=\"" + conflictHeldMsg + " (RFC 5227 section 2.4).\" network=abc\n",
			want: 1,
		},
		{
			name: "both, plus repeats",
			log: "level=error msg=\"" + conflictProbeMsg + "\"\n" +
				"level=error msg=\"" + conflictProbeMsg + "\"\n" +
				"level=error msg=\"" + conflictHeldMsg + "\"\n",
			want: 3,
		},
		{
			// One line must not be counted twice by matching both
			// patterns. The loop breaks on the first hit; this is what
			// notices if that break is removed.
			name: "a line is counted once even if both patterns were to match",
			log:  "level=error msg=\"" + conflictProbeMsg + " / " + conflictHeldMsg + "\"\n",
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConflictsInLog([]byte(tc.log)); got != tc.want {
				t.Errorf("ConflictsInLog = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestConflictMsgsMatchTheSource is the half a copied literal cannot
// give itself.
//
// The harness matches the plugin's log lines by text because it runs
// against an INSTALLED plugin, not against this tree — a compile-time
// constant would be a claim about the source and not about the process
// under test. That is the right trade, and it has one failure mode:
// somebody rewords the log line, the census silently counts zero
// forever, and address_conflicts=0 becomes an alibi again. This reads
// the source and fails on the drift.
//
// It is deliberately a SUBSTRING check against the source file rather
// than an import: it costs nothing, it cannot pull pkg/plugin into the
// harness's dependency graph, and it fails at the moment of the rename
// rather than at the next conflict.
func TestConflictMsgsMatchTheSource(t *testing.T) {
	const src = "../../../pkg/plugin/conflict.go"
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, msg := range conflictMsgs {
		if !strings.Contains(string(data), msg) {
			t.Errorf("%s no longer contains %q.\n"+
				"The census counts conflicts by matching this text in the plugin's log. "+
				"If the log line was reworded, update the constant in healthfloor.go; "+
				"until then every conflict in a run the plugin restarted through is invisible.",
				src, msg)
		}
	}
}
