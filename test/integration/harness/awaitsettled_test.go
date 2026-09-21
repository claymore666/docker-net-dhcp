// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"errors"
	"testing"
	"time"
)

func TestAwaitSettled_TheOutcomesAreThree(t *testing.T) {
	missing := errors.New("no link answers to that name")

	tests := []struct {
		name      string
		answers   []func() (string, error)
		budget    time.Duration
		wantValue string
		wantOK    bool
		wantErr   error
		wantCalls int
	}{
		{
			name:      "settled at once costs one look",
			answers:   []func() (string, error){func() (string, error) { return "web", nil }},
			budget:    time.Second,
			wantValue: "web",
			wantOK:    true,
			wantCalls: 1,
		},
		{
			name: "a name that is missing and then arrives is not a failure",
			answers: []func() (string, error){
				func() (string, error) { return "", missing },
				func() (string, error) { return "", missing },
				func() (string, error) { return "web", nil },
			},
			budget:    time.Second,
			wantValue: "web",
			wantOK:    true,
			wantCalls: 3,
		},
		{
			name: "a name that answers and never settles comes back unsettled",
			answers: []func() (string, error){
				func() (string, error) { return "dh-a1b2c3d4e5f6", nil },
			},
			budget:    20 * time.Millisecond,
			wantValue: "dh-a1b2c3d4e5f6",
			wantOK:    false,
			wantCalls: -1,
		},
		{
			name: "a name nothing ever answers to comes back as the reason",
			answers: []func() (string, error){
				func() (string, error) { return "", missing },
			},
			budget:    20 * time.Millisecond,
			wantValue: "",
			wantOK:    false,
			wantErr:   missing,
			wantCalls: -1,
		},
		{
			name: "one answer that arrived counts even when later looks fail",
			answers: []func() (string, error){
				func() (string, error) { return "dh-a1b2c3d4e5f6", nil },
				func() (string, error) { return "", missing },
			},
			budget:    20 * time.Millisecond,
			wantValue: "dh-a1b2c3d4e5f6",
			wantOK:    false,
			wantCalls: -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, ok, err := AwaitSettled(tc.budget, time.Millisecond,
				func() (string, error) {
					i := calls
					calls++
					if i >= len(tc.answers) {
						i = len(tc.answers) - 1
					}
					return tc.answers[i]()
				},
				func(s string) bool { return s == "web" })

			if got != tc.wantValue || ok != tc.wantOK || !errors.Is(err, tc.wantErr) {
				t.Fatalf("AwaitSettled = (%q, %v, %v), want (%q, %v, %v)",
					got, ok, err, tc.wantValue, tc.wantOK, tc.wantErr)
			}
			if tc.wantCalls > 0 && calls != tc.wantCalls {
				t.Errorf("the subject was asked %d times, want %d: a settled answer must not be "+
					"waited on and a missing one must be", calls, tc.wantCalls)
			}
			if tc.wantCalls < 0 && calls < 2 {
				t.Errorf("the subject was asked %d times over a %v budget: an unsettled read must "+
					"be retried, not reported after one look", calls, tc.budget)
			}
		})
	}
}

func TestAwaitSettled_AZeroBudgetIsOneLookAndNotNone(t *testing.T) {
	calls := 0
	got, ok, err := AwaitSettled(0, time.Millisecond,
		func() (int, error) { calls++; return 7, nil },
		func(int) bool { return true })
	if calls != 1 || got != 7 || !ok || err != nil {
		t.Fatalf("AwaitSettled with no budget = (%d, %v, %v) after %d looks, want (7, true, nil) after 1",
			got, ok, err, calls)
	}
}
