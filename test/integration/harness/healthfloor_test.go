// Untagged on purpose — see the header of healthfloor.go. This is the
// negative control for the integration suite's end-of-run health
// floor: it proves the floor rejects what it is supposed to reject,
// without having to deliberately break a real run to find out.

package harness

import "testing"

func TestCheckHealthFloor(t *testing.T) {
	// findings are keyed by counter name so the cases read as
	// "which counters, and is the run fatal", not as slice indices.
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
			// A nil can only come from a caller that already handled
			// the unreachable-socket case; the floor must not turn
			// that into a second, misleading failure.
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
			name: "recovery_failed is reported but not fatal until #376",
			in:   &HealthResponse{RecoveryFailed: 2},
			want: want{counters: map[string]int32{"recovery_failed": 2}, fatal: false},
		},
		{
			name: "the benign #373 counter is never a finding",
			// join_aborted_container_gone is the whole point of #373:
			// a container exiting mid-attach is not a plugin fault.
			// A run with a busy failure suite bumps this routinely.
			in:   &HealthResponse{JoinAbortedContainerGone: 7},
			want: want{counters: map[string]int32{}, fatal: false},
		},
		{
			name: "non-healthy-affecting counters are never findings",
			// These move on perfectly good runs — the failure suite
			// exists to make dhcp_timeouts and naks_received rise.
			in: &HealthResponse{
				DHCPTimeouts:         5,
				NAKsReceived:         2,
				LeaseReleaseFailures: 1,
				LedgerWriteFailures:  4,
				LeaseChanged:         3,
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
			name: "all three healthy-affecting counters at once",
			in: &HealthResponse{
				RecoveryFailed:         1,
				JoinStartFailures:      2,
				TombstoneWriteFailures: 3,
			},
			want: want{
				counters: map[string]int32{
					"recovery_failed":          1,
					"join_start_failures":      2,
					"tombstone_write_failures": 3,
				},
				fatal: true,
			},
		},
		{
			name: "healthy:false alone does not fail the floor",
			// The floor reads the counters, not the flag, precisely
			// because the flag is currently unreliable (#376). If
			// this case ever needs to become fatal, that is the
			// signal that #376 has landed and Phase 3 is due.
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

// TestFloorFailedEmpty pins the degenerate inputs separately: a run
// with no findings must not fail, and neither must a nil slice.
func TestFloorFailedEmpty(t *testing.T) {
	if FloorFailed(nil) {
		t.Error("FloorFailed(nil) = true, want false")
	}
	if FloorFailed([]FloorFinding{}) {
		t.Error("FloorFailed(empty) = true, want false")
	}
}
