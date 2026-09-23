// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCheckHealthFloor(t *testing.T) {
	type want struct {
		counters map[string]int32
		fatal    bool
	}
	cases := []struct {
		name string
		in   *HealthResponse
		want want
	}{
		{
			name: "clean run reports nothing",
			in:   &HealthResponse{Healthy: true},
			want: want{counters: map[string]int32{}, fatal: false},
		},
		{
			name: "nil response is not a failure",
			in:   nil,
			want: want{counters: map[string]int32{}, fatal: false},
		},
		{
			name: "join_start_failures is fatal",
			in:   &HealthResponse{JoinStartFailures: 1},
			want: want{counters: map[string]int32{"join_start_failures": 1}, fatal: true},
		},
		{
			name: "tombstone_write_failures is fatal",
			in:   &HealthResponse{TombstoneWriteFailures: 3},
			want: want{counters: map[string]int32{"tombstone_write_failures": 3}, fatal: true},
		},
		{
			name: "recovery_failed fails the run",
			in:   &HealthResponse{RecoveryFailed: 2},
			want: want{counters: map[string]int32{"recovery_failed": 2}, fatal: true},
		},
		{
			name: "the benign #373 counter is never a finding",
			in:   &HealthResponse{JoinAbortedContainerGone: 7},
			want: want{counters: map[string]int32{}, fatal: false},
		},
		{
			name: "non-healthy-affecting counters are never findings",
			in: &HealthResponse{
				DHCPTimeouts:        5,
				NAKsReceived:        2,
				ClientStopFailures:  1,
				LedgerWriteFailures: 4,
				LeaseChanged:        3,
			},
			want: want{counters: map[string]int32{}, fatal: false},
		},
		{
			name: "a non-fatal finding alongside a fatal one still fails",
			in:   &HealthResponse{RecoveryFailed: 1, JoinStartFailures: 1},
			want: want{
				counters: map[string]int32{"recovery_failed": 1, "join_start_failures": 1},
				fatal:    true,
			},
		},
		{
			name: "every healthy-affecting counter at once",
			in: &HealthResponse{
				RecoveryFailed:         1,
				JoinStartFailures:      2,
				TombstoneWriteFailures: 3,
				TombstoneQuarantines:   4,
				AddressConflicts:       5,
			},
			want: want{
				counters: map[string]int32{
					"recovery_failed":          1,
					"join_start_failures":      2,
					"tombstone_write_failures": 3,
					"tombstone_quarantines":    4,
					"address_conflicts":        5,
				},
				fatal: true,
			},
		},
		{
			name: "healthy:false alone does not fail the floor",
			in:   &HealthResponse{Healthy: false},
			want: want{counters: map[string]int32{}, fatal: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckHealthFloor(tc.in)

			byName := map[string]int32{}
			for _, f := range got {
				if _, dup := byName[f.Counter]; dup {
					t.Errorf("counter %q reported twice", f.Counter)
				}
				byName[f.Counter] = f.Value
				if f.Why == "" {
					t.Errorf("finding for %q has no Why — the failure output would not say what to do about it", f.Counter)
				}
			}

			if len(byName) != len(tc.want.counters) {
				t.Errorf("got findings %v, want %v", byName, tc.want.counters)
			}
			for name, val := range tc.want.counters {
				if byName[name] != val {
					t.Errorf("counter %q: got value %d, want %d", name, byName[name], val)
				}
			}
			if gotFatal := FloorFailed(got); gotFatal != tc.want.fatal {
				t.Errorf("FloorFailed = %v, want %v (findings: %v)", gotFatal, tc.want.fatal, byName)
			}
		})
	}
}

func TestFloorFailedEmpty(t *testing.T) {
	if FloorFailed(nil) {
		t.Error("FloorFailed(nil) = true, want false")
	}
	if FloorFailed([]FloorFinding{}) {
		t.Error("FloorFailed(empty) = true, want false")
	}
}

// decodeHealth goes through the real decoder, since a struct literal cannot tell a zero counter from an absent one (#377).
func decodeHealth(t *testing.T, payload string) *HealthResponse {
	t.Helper()
	var h HealthResponse
	if err := json.Unmarshal([]byte(payload), &h); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	if h.published == nil {
		t.Fatalf("decode %s: published is nil — the presence check would silently do nothing", payload)
	}
	return &h
}

func TestCheckHealthFloorPresence(t *testing.T) {
	// Cases drop keys from this complete payload, so a counter added to floorCounters without a key here fails.
	const complete = `{
		"healthy": true,
		"uptime_seconds": 42,
		"join_start_failures": 0,
		"tombstone_write_failures": 0,
		"tombstone_quarantines": 0,
		"recovery_failed": 0,
		"address_conflicts": 0
	}`

	cases := []struct {
		name        string
		payload     string
		wantAbsent  []string
		wantValues  map[string]int32
		wantFatal   bool
		wantMissing bool // expect at least one Absent finding
	}{
		{
			name:       "every checked counter present and zero is clean",
			payload:    complete,
			wantValues: map[string]int32{},
		},
		{
			name:        "a counter the plugin does not publish is fatal",
			payload:     `{"healthy": true, "tombstone_write_failures": 0, "tombstone_quarantines": 0, "recovery_failed": 0, "address_conflicts": 0}`,
			wantAbsent:  []string{"join_start_failures"},
			wantValues:  map[string]int32{},
			wantFatal:   true,
			wantMissing: true,
		},
		{
			name:    "an empty payload means every counter is unchecked",
			payload: `{}`,
			wantAbsent: []string{
				"join_start_failures", "tombstone_write_failures", "tombstone_quarantines", "recovery_failed",
				"address_conflicts",
				"healthy",
			},
			wantValues:  map[string]int32{},
			wantFatal:   true,
			wantMissing: true,
		},
		{
			name: "a null payload is treated as publishing nothing",
			// json.Unmarshal of `null` into a map yields nil without an error.
			payload: `null`,
			wantAbsent: []string{
				"join_start_failures", "tombstone_write_failures", "tombstone_quarantines", "recovery_failed",
				"address_conflicts",
				"healthy",
			},
			wantValues:  map[string]int32{},
			wantFatal:   true,
			wantMissing: true,
		},
		{
			name:        "a missing counter and a real fault are both reported",
			payload:     `{"healthy": false, "tombstone_write_failures": 2, "tombstone_quarantines": 0, "recovery_failed": 0, "address_conflicts": 0}`,
			wantAbsent:  []string{"join_start_failures"},
			wantValues:  map[string]int32{"tombstone_write_failures": 2},
			wantFatal:   true,
			wantMissing: true,
		},
		{
			name:       "recovery_failed alone fails the run",
			payload:    `{"healthy": false, "join_start_failures": 0, "tombstone_write_failures": 0, "tombstone_quarantines": 0, "recovery_failed": 3, "address_conflicts": 0}`,
			wantValues: map[string]int32{"recovery_failed": 3},
			wantFatal:  true,
		},
		{
			name:       "an unhealthy plugin fails even when every known counter is clean",
			payload:    `{"healthy": false, "join_start_failures": 0, "tombstone_write_failures": 0, "tombstone_quarantines": 0, "recovery_failed": 0, "address_conflicts": 0}`,
			wantValues: map[string]int32{},
			wantFatal:  true,
		},
		{
			name:       "a healthy plugin with clean counters passes",
			payload:    `{"healthy": true, "join_start_failures": 0, "tombstone_write_failures": 0, "tombstone_quarantines": 0, "recovery_failed": 0, "address_conflicts": 0}`,
			wantValues: map[string]int32{},
			wantFatal:  false,
		},
		{
			name:        "an unpublished non-fatal counter is still fatal",
			payload:     `{"healthy": true, "join_start_failures": 0, "tombstone_write_failures": 0, "tombstone_quarantines": 0, "address_conflicts": 0}`,
			wantAbsent:  []string{"recovery_failed"},
			wantValues:  map[string]int32{},
			wantFatal:   true,
			wantMissing: true,
		},
		{
			name:       "counters outside the floor may be absent freely",
			payload:    complete,
			wantValues: map[string]int32{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckHealthFloor(decodeHealth(t, tc.payload))

			absent := map[string]bool{}
			values := map[string]int32{}
			seen := map[string]bool{}
			for _, f := range got {
				if seen[f.Counter] {
					t.Errorf("counter %q reported twice", f.Counter)
				}
				seen[f.Counter] = true
				if f.Why == "" {
					t.Errorf("finding for %q has no Why", f.Counter)
				}
				if f.Absent {
					absent[f.Counter] = true
					if f.Value != 0 {
						t.Errorf("absent finding for %q carries value %d; a value that was never reported must not be printed as one", f.Counter, f.Value)
					}
					if !f.Fatal {
						t.Errorf("absent finding for %q is not fatal; an unreadable counter proves nothing about the run", f.Counter)
					}
					continue
				}
				if f.Flag {
					continue
				}
				values[f.Counter] = f.Value
			}

			if len(absent) != len(tc.wantAbsent) {
				t.Errorf("absent counters: got %v, want %v", keys(absent), tc.wantAbsent)
			}
			for _, name := range tc.wantAbsent {
				if !absent[name] {
					t.Errorf("counter %q was not published but produced no absent finding", name)
				}
			}
			if len(values) != len(tc.wantValues) {
				t.Errorf("value findings: got %v, want %v", values, tc.wantValues)
			}
			for name, want := range tc.wantValues {
				if values[name] != want {
					t.Errorf("counter %q: got value %d, want %d", name, values[name], want)
				}
			}
			if gotFatal := FloorFailed(got); gotFatal != tc.wantFatal {
				t.Errorf("FloorFailed = %v, want %v", gotFatal, tc.wantFatal)
			}
			if tc.wantMissing && len(absent) == 0 {
				t.Error("expected at least one absent finding, got none")
			}
		})
	}
}

func TestUnmarshalKeepsDecodingValues(t *testing.T) {
	h := decodeHealth(t, `{
		"healthy": false,
		"uptime_seconds": 12.5,
		"join_start_failures": 4,
		"join_aborted_container_gone": 9,
		"tombstone_write_failures": 1,
		"recovery_failed": 2,
		"leases_obtained": 7
	}`)

	if h.Healthy {
		t.Error("Healthy: got true, want false")
	}
	if h.UptimeSeconds != 12.5 {
		t.Errorf("UptimeSeconds: got %v, want 12.5", h.UptimeSeconds)
	}
	for name, got := range map[string]int32{
		"join_start_failures":         h.JoinStartFailures,
		"join_aborted_container_gone": h.JoinAbortedContainerGone,
		"tombstone_write_failures":    h.TombstoneWriteFailures,
		"recovery_failed":             h.RecoveryFailed,
		"leases_obtained":             h.LeasesObtained,
	} {
		if got == 0 {
			t.Errorf("%s decoded as 0 — values were dropped", name)
		}
	}
}

func TestFloorCounterNamesMatchJSONTags(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(HealthResponse{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		tags[strings.Split(tag, ",")[0]] = true
	}
	if len(tags) == 0 {
		t.Fatal("HealthResponse has no json tags — this test would pass vacuously")
	}
	for _, c := range floorCounters {
		if !tags[c.name] {
			t.Errorf("floorCounters entry %q has no matching json tag on HealthResponse; the presence check would report it absent on every run", c.name)
		}
		if c.read == nil {
			t.Errorf("floorCounters entry %q has no read function", c.name)
		}
		if c.why == "" {
			t.Errorf("floorCounters entry %q has no why", c.name)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestFloorEvidence(t *testing.T) {
	const (
		errLine  = `time="2026-07-31T16:40:00Z" level=error msg="Failed to start persistent DHCP client" endpoint=abc123`
		warnLine = `time="2026-07-31T16:40:01Z" level=warning msg="Failed to persist tombstone"`
		infoLine = `time="2026-07-31T16:40:02Z" level=info msg="Lease acquired" ip=192.168.99.5`
	)

	t.Run("fault lines are pulled out wherever they fall", func(t *testing.T) {
		var lines []string
		lines = append(lines, errLine, warnLine)
		for i := 0; i < 500; i++ {
			lines = append(lines, infoLine)
		}
		got := FloorEvidence([]byte(strings.Join(lines, "\n")), 10)

		if !strings.Contains(got, "Failed to start persistent DHCP client") {
			t.Error("error line missing from evidence")
		}
		if !strings.Contains(got, "Failed to persist tombstone") {
			t.Error("warning line missing from evidence; the tombstone counter logs at warn, so dropping warnings would hide it")
		}
		if !strings.Contains(got, "--- 2 error/warning lines ---") {
			t.Errorf("fault count heading missing or wrong:\n%s", firstLines(got, 3))
		}
	})

	t.Run("the tail is included for context", func(t *testing.T) {
		lines := []string{errLine}
		for i := 0; i < 50; i++ {
			lines = append(lines, fmt.Sprintf(`time="t" level=info msg="line %d"`, i))
		}
		got := FloorEvidence([]byte(strings.Join(lines, "\n")), 5)
		if !strings.Contains(got, "--- last 5 lines ---") {
			t.Errorf("tail heading missing:\n%s", got)
		}
		if !strings.Contains(got, `msg="line 49"`) {
			t.Error("last line of the log is not in the tail")
		}
		if strings.Contains(got, `msg="line 10"`) {
			t.Error("tail is not bounded to tailLines")
		}
	})

	t.Run("a flood of faults is bounded, and says so", func(t *testing.T) {
		lines := make([]string, 0, 500)
		for i := 0; i < 500; i++ {
			lines = append(lines, fmt.Sprintf(`time="t" level=error msg="fault %d"`, i))
		}
		got := FloorEvidence([]byte(strings.Join(lines, "\n")), 0)
		if !strings.Contains(got, fmt.Sprintf("--- last %d of 500 error/warning lines ---", floorEvidenceMaxFaultLines)) {
			t.Errorf("truncation is not announced:\n%s", firstLines(got, 3))
		}
		if !strings.Contains(got, `msg="fault 499"`) {
			t.Error("truncation dropped the most recent fault")
		}
		if strings.Contains(got, `msg="fault 0"`) {
			t.Error("truncation kept the oldest fault instead of the newest")
		}
	})

	t.Run("a counter that moved without logging is called out", func(t *testing.T) {
		got := FloorEvidence([]byte(infoLine), 0)
		if !strings.Contains(got, "without logging") {
			t.Errorf("a log with no error/warning lines should say so explicitly, got:\n%s", got)
		}
	})

	t.Run("an empty log does not produce empty output", func(t *testing.T) {
		for _, in := range [][]byte{nil, []byte(""), []byte("\n")} {
			got := FloorEvidence(in, 10)
			if !strings.Contains(got, "empty") {
				t.Errorf("FloorEvidence(%q) = %q; want an explicit empty-log note", in, got)
			}
		}
	})
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestFloorCleanLine(t *testing.T) {
	t.Run("full coverage says so plainly", func(t *testing.T) {
		got := FloorCleanLine(&HealthResponse{UptimeSeconds: 95, Healthy: true}, 92)
		if !strings.Contains(got, "whole 92s run") {
			t.Errorf("the run's duration is the suite's, got:\n%s", got)
		}
		if !strings.Contains(got, "plugin up 95s") {
			t.Errorf("uptime should still be reported, labelled as uptime, got:\n%s", got)
		}
		if strings.Contains(got, "restarted mid-suite") {
			t.Error("full coverage should not carry the partial-coverage caveat")
		}
	})

	// Measured (#474): a local run against a plugin up for hours printed "whole 15479s run" for a 61 s suite.
	t.Run("a plugin that long predates the suite does not lend it its uptime", func(t *testing.T) {
		got := FloorCleanLine(&HealthResponse{UptimeSeconds: 15479, Healthy: true}, 61)

		if strings.Contains(got, "15479s run") {
			t.Errorf("uptime is being reported as the run's duration (#474):\n%s", got)
		}
		if !strings.Contains(got, "whole 61s run") {
			t.Errorf("the run is the 61s suite, not the plugin's lifetime:\n%s", got)
		}
		if !strings.Contains(got, "15479s") {
			t.Errorf("uptime should still appear, as uptime:\n%s", got)
		}
		if !strings.Contains(got, "predates this run by 15418s") {
			t.Errorf("a plugin far older than the suite should be disclosed:\n%s", got)
		}
		if strings.Contains(got, "restarted mid-suite") {
			t.Error("uptime exceeding the suite is full coverage, not partial")
		}
	})

	t.Run("a plugin installed for the run does not carry the predates note", func(t *testing.T) {
		got := FloorCleanLine(&HealthResponse{UptimeSeconds: 700, Healthy: true}, 611)
		if strings.Contains(got, "predates") {
			t.Errorf("ordinary install-then-run slack should not be flagged as history:\n%s", got)
		}
		if !strings.Contains(got, "whole 611s run") {
			t.Errorf("the run is still the suite's duration:\n%s", got)
		}
	})

	t.Run("a mid-suite restart is disclosed with the numbers", func(t *testing.T) {
		got := FloorCleanLine(&HealthResponse{UptimeSeconds: 78, Healthy: true}, 611)
		for _, want := range []string{"last 78s", "611s run", "13%", "restarted mid-suite"} {
			if !strings.Contains(got, want) {
				t.Errorf("partial-coverage verdict is missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "whole") {
			t.Error("a partial-coverage verdict must not claim the whole run")
		}
	})

	t.Run("an unmeasurable suite duration drops the qualifier rather than inventing one", func(t *testing.T) {
		for _, suite := range []float64{0, -1} {
			got := FloorCleanLine(&HealthResponse{UptimeSeconds: 78, Healthy: true}, suite)
			if strings.Contains(got, "%") {
				t.Errorf("FloorCleanLine(_, %v) claimed a coverage ratio it cannot know:\n%s", suite, got)
			}
		}
	})

	t.Run("healthy is reported either way", func(t *testing.T) {
		for _, suite := range []float64{92, 611} {
			got := FloorCleanLine(&HealthResponse{UptimeSeconds: 78, Healthy: false}, suite)
			if !strings.Contains(got, "healthy=false") {
				t.Errorf("healthy=false disappeared from the clean line (suite %v):\n%s", suite, got)
			}
		}
	})

	t.Run("no panic on a nil response", func(t *testing.T) {
		if got := FloorCleanLine(nil, 92); got != "" {
			t.Errorf("FloorCleanLine(nil, _) = %q; want empty", got)
		}
	})
}

func TestJoinFailureCensus(t *testing.T) {
	// Verbatim from the run #401 was filed on, trimmed to the fields that matter.
	const realLog = `time="2026-07-31T18:07:08Z" level=error msg="Failed to start persistent DHCP client; lease will not be renewed" endpoint=64527feea371 error="failed to get Docker container info: context deadline exceeded" network=62826ec6f0b8
time="2026-07-31T18:07:25Z" level=error msg="Failed to start persistent DHCP client; lease will not be renewed" endpoint=f3f9d6712b9e error="failed to get Docker container info: context deadline exceeded" network=6bef7628c5ba
time="2026-07-31T18:08:01Z" level=error msg="Failed to start persistent DHCP client; lease will not be renewed" endpoint=b4e40afce1d8 error="failed to get sandbox network namespace: context deadline exceeded (last attempt: no such file or directory)" network=bae01f6c30ef
time="2026-07-31T18:08:02Z" level=info msg="Container went away during attach; no persistent client needed" endpoint=aaaaaaaaaaaa
time="2026-07-31T18:09:03Z" level=warning msg="Caller error while processing request" error="parent interface is down: dh-itest-host" status=400`

	got := JoinFailureCensus([]byte(realLog))

	if !strings.Contains(got, "JOIN FAILURES: 3 across the whole run") {
		t.Errorf("wrong total; want 3:\n%s", got)
	}
	if !strings.Contains(got, "  2  failed to get Docker container info: context deadline exceeded") {
		t.Errorf("causes are not grouped and counted:\n%s", got)
	}
	if strings.Contains(got, "went away during attach") {
		t.Error("the benign container-gone case was counted as a failure")
	}
	if strings.Contains(got, "parent interface is down") {
		t.Error("an unrelated warning was counted as a Join failure")
	}

	t.Run("a clean run stays silent", func(t *testing.T) {
		for _, in := range []string{"", `time="t" level=info msg="Lease acquired"`} {
			if got := JoinFailureCensus([]byte(in)); got != "" {
				t.Errorf("JoinFailureCensus(%q) = %q; want silence", in, got)
			}
		}
	})

	t.Run("a line with no error field is still counted", func(t *testing.T) {
		got := JoinFailureCensus([]byte(`level=error msg="Failed to start persistent DHCP client"`))
		if !strings.Contains(got, "JOIN FAILURES: 1") {
			t.Errorf("a failure without an error field vanished from the count:\n%s", got)
		}
		if !strings.Contains(got, "(no error field)") {
			t.Errorf("want an explicit placeholder rather than an empty cause:\n%s", got)
		}
	})

	t.Run("an escaped quote inside the error does not truncate it", func(t *testing.T) {
		line := `level=error msg="Failed to start persistent DHCP client" error="open \"/proc/1/ns/net\": no such file"`
		got := JoinFailureCensus([]byte(line))
		if !strings.Contains(got, `no such file`) {
			t.Errorf("cause was truncated at the escaped quote:\n%s", got)
		}
	})
}

func TestJoinFailureCount_MatchesTheCensus(t *testing.T) {
	log := []byte(strings.Join([]string{
		`time="1" level=error msg="Failed to start persistent DHCP client; lease will not be renewed" error="context deadline exceeded"`,
		`time="2" level=info msg="Container went away during attach; no persistent client needed" error="no such container"`,
		`time="3" level=error msg="Failed to start persistent DHCP client; lease will not be renewed" error="context deadline exceeded"`,
		`time="4" level=warning msg="Caller error while processing request" error="parent interface is down"`,
	}, "\n"))

	if got := JoinFailureCount(log); got != 2 {
		t.Errorf("JoinFailureCount = %d, want 2 (the benign twin and the caller error must not count)", got)
	}
	if census := JoinFailureCensus(log); !strings.Contains(census, "JOIN FAILURES: 2") {
		t.Errorf("census says %q; the verdict counted 2", census)
	}
}

func TestJoinFailureCount_ZeroIsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
	}{
		{"empty", ""},
		{"only benign", `msg="Container went away during attach; no persistent client needed"`},
		{"unrelated errors", `level=error msg="something else entirely"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := JoinFailureCount([]byte(tc.log)); got != 0 {
				t.Errorf("JoinFailureCount = %d, want 0", got)
			}
		})
	}
}

// The #406 failures are intermittent (6, 5, 3 and 0 on unchanged code), so a run with no Join failures is ambiguous.
func TestAttachGraceLine_DistinguishesQuietFromFixed(t *testing.T) {
	t.Run("grace used and nothing failed is evidence", func(t *testing.T) {
		got := AttachGraceLine(&HealthResponse{JoinAttachSlow: 4}, 0)
		if !strings.Contains(got, "This is the fix working") {
			t.Errorf("a run where the grace carried 4 attaches should say so; got %q", got)
		}
	})

	t.Run("quiet run is explicitly not evidence", func(t *testing.T) {
		got := AttachGraceLine(&HealthResponse{JoinAttachSlow: 0}, 0)
		if !strings.Contains(got, "not evidence either way") {
			t.Errorf("a run where the condition never arose must not read as a pass; got %q", got)
		}
	})

	t.Run("failures without the grace point elsewhere", func(t *testing.T) {
		got := AttachGraceLine(&HealthResponse{JoinAttachSlow: 0}, 3)
		if !strings.Contains(got, "look elsewhere") {
			t.Errorf("failures with no slow attach means a different mechanism; got %q", got)
		}
	})

	t.Run("grace used but failures remain says the fix is partial", func(t *testing.T) {
		got := AttachGraceLine(&HealthResponse{JoinAttachSlow: 2}, 1)
		if !strings.Contains(got, "not\n  sufficient") && !strings.Contains(got, "not sufficient") {
			t.Errorf("a partially working grace must not read as success; got %q", got)
		}
	})
}

func TestACDCensusLine(t *testing.T) {
	cases := []struct {
		name string
		h    *HealthResponse
		want []string // substrings that must appear
		deny []string // substrings that must not
	}{
		{
			name: "nil health says nothing",
			h:    nil,
			want: nil,
		},
		{
			name: "no probes is explicitly not evidence",
			h:    &HealthResponse{},
			want: []string{"not\n  evidence", "absence of a measurement"},
			deny: []string{"check ran and the segment was clean"},
		},
		{
			name: "probes ran clean is stated as observed",
			h:    &HealthResponse{ACDProbesSent: 7, ACDAnnouncementsSent: 14},
			want: []string{"7 ARP Probe(s)", "14 Announcement(s)", "observed, not inferred"},
			deny: []string{"not\n  evidence"},
		},
		{
			name: "a conflict is reported over the probe count",
			h:    &HealthResponse{AddressConflicts: 1, ACDProbesSent: 4},
			want: []string{"1 leased address(es)", "out of 4 ARP Probe(s)"},
			deny: []string{"check ran and the segment was clean"},
		},
		{
			name: "partial coverage is not a clean bill",
			h:    &HealthResponse{ACDProbesSent: 3, ACDARPSendFailures: 2},
			want: []string{"3 ARP Probe(s)", "2\n  send(s) were refused", "only what was actually asked"},
			deny: []string{"observed, not inferred"},
		},
		{
			name: "a conflict outranks refused sends",
			h:    &HealthResponse{AddressConflicts: 2, ACDProbesSent: 5, ACDARPSendFailures: 1},
			want: []string{"2 leased address(es)"},
			deny: []string{"send(s) were refused"},
		},
		{
			name: "the no-probe line names conflict_check=off as a reason",
			h:    &HealthResponse{LeasesObtained: 3},
			want: []string{"conflict_check=off"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ACDCensusLine(tc.h)
			if tc.h == nil {
				if got != "" {
					t.Errorf("nil health produced %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatal("produced no line; every non-nil case must say something")
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("line missing %q:\n%s", w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("line wrongly contains %q:\n%s", d, got)
				}
			}
		})
	}
}
