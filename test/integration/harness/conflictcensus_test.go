// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"strings"
	"testing"
)

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
		// baseline is the plugin's counters when this process started; nil means the plugin was started for this run.
		baseline *HealthResponse
	}{
		{
			name: "nil health says nothing",
			h:    nil,
		},
		{
			name: "probes went out and no send was refused",
			h:    &HealthResponse{ACDProbesSent: 4, LeasesObtained: 4, LeasesObtainedV4: 4},
		},
		{
			name:        "one probe out and two sends refused, none declared, fails",
			h:           &HealthResponse{ACDProbesSent: 1, ACDARPSendFailures: 2, LeasesObtained: 3, LeasesObtainedV4: 3},
			allowedSend: 0,
			want:        []string{"acd_arp_send_failures"},
		},
		{
			name:        "a declared deliberate refusal is not a finding",
			h:           &HealthResponse{ACDProbesSent: 3, ACDARPSendFailures: 1, LeasesObtained: 4, LeasesObtainedV4: 4},
			allowedSend: 1,
			want:        nil,
		},
		{
			name:        "one refusal beyond the declared allowance fails",
			h:           &HealthResponse{ACDProbesSent: 3, ACDARPSendFailures: 2, LeasesObtained: 5, LeasesObtainedV4: 5},
			allowedSend: 1,
			want:        []string{"acd_arp_send_failures"},
		},
		{
			name: "a shard that leased no v4 address is not a failure",
			h:    &HealthResponse{ACDProbesSent: 0, LeasesObtained: 0, LeasesObtainedV4: 0},
		},
		{
			name: "leases obtained but the check never ran fails",
			h:    &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 6, LeasesObtainedV4: 6},
			want: []string{"acd_probes_sent"},
		},
		{
			name:        "declared conflict_check=off leases are not a never-ran finding",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 3, LeasesObtainedV4: 3},
			allowedUnpr: 3,
			want:        nil,
		},
		{
			name:        "an undeclared lease among declared ones still fails",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 4, LeasesObtainedV4: 4},
			allowedUnpr: 3,
			want:        []string{"acd_probes_sent"},
		},
		{
			name:        "an over-declared allowance is still not a finding",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 1, LeasesObtainedV4: 1},
			allowedUnpr: 9,
			want:        nil,
		},
		{
			name: "a shard that leased nothing does not trip the never-ran case",
			h:    &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 0, LeasesObtainedV4: 0},
		},
		{
			// RFC 5227's check is ARP, so a v6 lease never produces a probe, while leases_obtained sums both families (#881).
			name: "a v6 lease with no v4 lease is not a never-ran finding",
			h:    &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 1, LeasesObtainedV4: 0},
			want: nil,
		},
		{
			name: "a v4 lease alongside v6 ones still fails when nothing probed",
			h:    &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 4, LeasesObtainedV4: 1},
			want: []string{"acd_probes_sent"},
		},
		{
			// Pinned open (#881): async conflict mode binds first and probes after a uniform(0, PROBE_WAIT) timer (RFC 5227
			// section 2.1), and counters carry no time to tell that window from a plugin that never probes. Fatal by decision.
			name:     "a v4 lease whose probe timer has not fired yet is STILL fatal (P-3)",
			h:        &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 0, LeasesObtained: 1, LeasesObtainedV4: 1},
			baseline: &HealthResponse{ACDProbesSent: 9, ACDARPSendFailures: 0, LeasesObtained: 9, LeasesObtainedV4: 9},
			want:     []string{"acd_probes_sent"},
		},
		{
			name:     "a v6 bind after the baseline does not enter the domain",
			h:        &HealthResponse{ACDProbesSent: 2, LeasesObtained: 5, LeasesObtainedV4: 2},
			baseline: &HealthResponse{ACDProbesSent: 2, LeasesObtained: 2, LeasesObtainedV4: 2},
			want:     nil,
		},
		{
			// The plugin's counters reset on a mid-suite restart and the log does not (#385).
			name:        "counters clean but the log records conflicts still fails",
			h:           &HealthResponse{ACDProbesSent: 2, LeasesObtained: 2, LeasesObtainedV4: 2},
			conflictLog: 3,
			want:        []string{"address_conflicts"},
		},
		{
			name:        "the counter agreeing with the log is not a finding",
			h:           &HealthResponse{ACDProbesSent: 2, AddressConflicts: 4, LeasesObtained: 2, LeasesObtainedV4: 2},
			conflictLog: 1,
			want:        nil,
		},
		{
			name:        "a refused send alone does not also raise the never-ran finding",
			h:           &HealthResponse{ACDProbesSent: 0, ACDARPSendFailures: 2, LeasesObtained: 5, LeasesObtainedV4: 5},
			allowedSend: 9,
			want:        nil,
		},
		{
			name:        "a refusal that predates this process is not ours",
			h:           &HealthResponse{ACDProbesSent: 5, ACDARPSendFailures: 1, LeasesObtained: 5, LeasesObtainedV4: 5},
			allowedSend: 0,
			baseline:    &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 1, LeasesObtained: 4, LeasesObtainedV4: 4},
			want:        nil,
		},
		{
			name:        "a refusal after the baseline is still ours",
			h:           &HealthResponse{ACDProbesSent: 5, ACDARPSendFailures: 2, LeasesObtained: 5, LeasesObtainedV4: 5},
			allowedSend: 0,
			baseline:    &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 1, LeasesObtained: 4, LeasesObtainedV4: 4},
			want:        []string{"acd_arp_send_failures"},
		},
		{
			name:        "a counter below the baseline is a restart, not a negative",
			h:           &HealthResponse{ACDProbesSent: 1, ACDARPSendFailures: 2, LeasesObtained: 1, LeasesObtainedV4: 1},
			allowedSend: 0,
			baseline:    &HealthResponse{ACDProbesSent: 9, ACDARPSendFailures: 7, LeasesObtained: 9, LeasesObtainedV4: 9},
			want:        []string{"acd_arp_send_failures"},
		},
		{
			name:     "the check not running in THIS process is still a finding",
			h:        &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 0, LeasesObtained: 7, LeasesObtainedV4: 7},
			baseline: &HealthResponse{ACDProbesSent: 4, ACDARPSendFailures: 0, LeasesObtained: 4, LeasesObtainedV4: 4},
			want:     []string{"acd_probes_sent"},
		},
		{
			name:        "faults are reported together, not just the first",
			h:           &HealthResponse{ACDProbesSent: 2, ACDARPSendFailures: 3, LeasesObtained: 2, LeasesObtainedV4: 2},
			allowedSend: 0,
			conflictLog: 1,
			want:        []string{"acd_arp_send_failures", "address_conflicts"},
		},
		{
			name:        "staged conflicts the counter lost to a restart are declared, not red",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4, LeasesObtainedV4: 4},
			allowedConf: 2,
			conflictLog: 2,
		},
		{
			// Inside a declaring shard, a conflict lost to a restart and one the seam dropped both read log=n, counter<n; this row
			// cannot tell them apart and the conflict cases' own counter assertions hold #524.
			name:        "inside the declaration a dropped conflict is invisible to the row",
			h:           &HealthResponse{ACDProbesSent: 3, LeasesObtained: 2, LeasesObtainedV4: 2},
			allowedConf: 1,
			conflictLog: 1,
		},
		{
			name:        "one conflict more than declared is still fatal",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4, LeasesObtainedV4: 4},
			allowedConf: 2,
			conflictLog: 3,
			want:        []string{"address_conflicts"},
		},
		{
			name:        "an undeclared conflict the counter never saw is fatal",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4, LeasesObtainedV4: 4},
			conflictLog: 1,
			want:        []string{"address_conflicts"},
		},
		{
			// Measured, run 34600486961: a shard whose last test recycled the plugin read leases_obtained_v4=1 and
			// acd_probes_sent=0 on a process 1 s old; recovered_ok counts endpoints of either family (#881).
			name: "recovered_ok does not excuse an undeclared unprobed lease",
			h:    &HealthResponse{ACDProbesSent: 0, LeasesObtained: 1, LeasesObtainedV4: 1, RecoveredOK: 1},
			want: []string{"acd_probes_sent"},
		},
		{
			name: "recovered endpoints of another family cannot cancel a v4 miss",
			h:    &HealthResponse{ACDProbesSent: 0, LeasesObtained: 1, LeasesObtainedV4: 1, RecoveredOK: 3},
			want: []string{"acd_probes_sent"},
		},
		{
			name:        "a declared resumed lease is not a never-ran finding",
			h:           &HealthResponse{ACDProbesSent: 0, LeasesObtained: 1, LeasesObtainedV4: 1, RecoveredOK: 1},
			allowedUnpr: 1,
		},
		{
			name:        "the counter matching the log is clean with a declaration standing",
			h:           &HealthResponse{ACDProbesSent: 6, LeasesObtained: 4, LeasesObtainedV4: 4, AddressConflicts: 2},
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

func TestACDCensusFindingsAbsentCounter(t *testing.T) {
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

func TestACDCensusFindingsAbsentDomainCounter(t *testing.T) {
	h := decodeHealth(t, `{"healthy":true,"acd_probes_sent":0,"acd_arp_send_failures":0,"leases_obtained":3}`)

	got := ACDCensusFindings(h, 0, 0, 0, 0, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %v, want exactly one", got)
	}
	if got[0].Counter != "leases_obtained_v4" {
		t.Errorf("Counter = %q, want leases_obtained_v4", got[0].Counter)
	}
	if !got[0].Absent || !got[0].Fatal {
		t.Errorf("Absent=%v Fatal=%v, want both true — an unpublished domain operand must not "+
			"be judged as zero", got[0].Absent, got[0].Fatal)
	}
}

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
	if got, want := AllowedARPSendFailures(), beforeSend+3; got != want {
		t.Errorf("AllowUnprobedLeases moved the send-failure allowance: %d, want %d", got, want)
	}
}

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

// The harness runs against an installed plugin, so it matches log text; this reads the source to catch a reworded line.
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
