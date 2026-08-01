package harness

import (
	"strings"
	"testing"
)

// Cases below use decodeHealth (healthfloor_test.go) rather than struct
// literals wherever the point turns on what the plugin actually sent:
// it goes through UnmarshalJSON, so the `published` key set is
// populated, and "the plugin sent instance_id" stays distinguishable
// from "the field is at its zero value".

func TestCompareInstances_SameProcess(t *testing.T) {
	a := decodeHealth(t, `{"instance_id":"aaaa1111","uptime_seconds":10}`)
	b := decodeHealth(t, `{"instance_id":"aaaa1111","uptime_seconds":40}`)
	if got := CompareInstances(a, b); got != InstanceSame {
		t.Errorf("got %v, want %v — one process across both reads", got, InstanceSame)
	}
}

func TestCompareInstances_Recycled(t *testing.T) {
	a := decodeHealth(t, `{"instance_id":"aaaa1111","uptime_seconds":300}`)
	b := decodeHealth(t, `{"instance_id":"bbbb2222","uptime_seconds":4}`)
	if got := CompareInstances(a, b); got != InstanceRecycled {
		t.Errorf("got %v, want %v", got, InstanceRecycled)
	}
}

// The case the whole file exists for. An absent JSON string decodes to
// "", and "" == "" — so the obvious implementation reports "same
// process" with full confidence precisely when it has no evidence at
// all. That is the #377 mistake repeated on a new field.
func TestCompareInstances_TwoEmptyIDsAreNotTheSameProcess(t *testing.T) {
	a := decodeHealth(t, `{"instance_id":"","uptime_seconds":10}`)
	b := decodeHealth(t, `{"instance_id":"","uptime_seconds":40}`)
	if got := CompareInstances(a, b); got != InstanceUnknown {
		t.Errorf("got %v, want %v — two empty ids are absence of evidence, "+
			"not evidence the process survived", got, InstanceUnknown)
	}
}

func TestCompareInstances_KeyNeverPublished(t *testing.T) {
	// A plugin predating #405 sends no instance_id. The field is "" and
	// indistinguishable from an empty one by value alone; only the
	// published key set can tell them apart.
	a := decodeHealth(t, `{"uptime_seconds":10,"healthy":true}`)
	b := decodeHealth(t, `{"uptime_seconds":40,"healthy":true}`)
	if got := CompareInstances(a, b); got != InstanceUnknown {
		t.Errorf("got %v, want %v — the plugin never published %s",
			got, InstanceUnknown, instanceIDKey)
	}
}

func TestCompareInstances_OneSideMissing(t *testing.T) {
	known := decodeHealth(t, `{"instance_id":"aaaa1111"}`)
	blank := decodeHealth(t, `{"instance_id":""}`)
	for _, tc := range []struct {
		name          string
		before, after *HealthResponse
	}{
		{"before empty", blank, known},
		{"after empty", known, blank},
		{"before nil", nil, known},
		{"after nil", known, nil},
		{"both nil", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareInstances(tc.before, tc.after); got != InstanceUnknown {
				t.Errorf("got %v, want %v", got, InstanceUnknown)
			}
		})
	}
}

func TestCompareInstances_HandBuiltValuesJudgedOnTheField(t *testing.T) {
	// published == nil means "not decoded from a payload", matching the
	// convention CheckHealthFloor already uses. Unit tests that build a
	// HealthResponse literal must stay usable.
	same := CompareInstances(&HealthResponse{InstanceID: "x"}, &HealthResponse{InstanceID: "x"})
	if same != InstanceSame {
		t.Errorf("hand-built matching ids: got %v, want %v", same, InstanceSame)
	}
	diff := CompareInstances(&HealthResponse{InstanceID: "x"}, &HealthResponse{InstanceID: "y"})
	if diff != InstanceRecycled {
		t.Errorf("hand-built differing ids: got %v, want %v", diff, InstanceRecycled)
	}
	// Even hand-built, empty is still unknown.
	blank := CompareInstances(&HealthResponse{}, &HealthResponse{})
	if blank != InstanceUnknown {
		t.Errorf("hand-built empty ids: got %v, want %v", blank, InstanceUnknown)
	}
}

func TestCounterWindowError_AcceptsOnlyTheTwoIntendedShapes(t *testing.T) {
	a := &HealthResponse{InstanceID: "aaaa1111"}
	b := &HealthResponse{InstanceID: "bbbb2222"}

	if got := CounterWindowError(InstanceSame, false, a, a); got != "" {
		t.Errorf("same instance with no recycle expected should pass, got:\n%s", got)
	}
	if got := CounterWindowError(InstanceRecycled, true, a, b); got != "" {
		t.Errorf("recycle that was expected should pass, got:\n%s", got)
	}
	if got := CounterWindowError(InstanceRecycled, false, a, b); got == "" {
		t.Error("an unexpected recycle must fail — it is the whole point of #405")
	}
	if got := CounterWindowError(InstanceSame, true, a, a); got == "" {
		t.Error("a recycle that was expected and did not happen must fail")
	}
	if got := CounterWindowError(InstanceUnknown, false, a, a); got == "" {
		t.Error("an unverifiable window must fail, not pass quietly")
	}
	if got := CounterWindowError(InstanceUnknown, true, a, b); got == "" {
		t.Error("unknown must fail even when a recycle was expected — " +
			"expecting one is not evidence one happened")
	}
}

func TestCounterWindowError_NamesTheVoidCounters(t *testing.T) {
	got := CounterWindowError(InstanceRecycled, false,
		&HealthResponse{InstanceID: "aaaa1111", UptimeSeconds: 300},
		&HealthResponse{InstanceID: "bbbb2222", UptimeSeconds: 4},
		"leases_obtained", "dhcp_timeouts")
	for _, want := range []string{"leases_obtained", "dhcp_timeouts", "aaaa1111", "bbbb2222", "300s", "4s"} {
		if !strings.Contains(got, want) {
			t.Errorf("message should mention %q; got:\n%s", want, got)
		}
	}
}

func TestCounterWindowError_UnknownSaysWhichSideFailed(t *testing.T) {
	known := decodeHealth(t, `{"instance_id":"aaaa1111"}`)
	old := decodeHealth(t, `{"healthy":true}`)

	got := CounterWindowError(InstanceUnknown, false, old, known)
	if !strings.Contains(got, "opening") {
		t.Errorf("should identify the opening read as the unidentified one; got:\n%s", got)
	}
	got = CounterWindowError(InstanceUnknown, false, known, old)
	if !strings.Contains(got, "closing") {
		t.Errorf("should identify the closing read as the unidentified one; got:\n%s", got)
	}
	got = CounterWindowError(InstanceUnknown, false, nil, known)
	if !strings.Contains(got, "opening health read is missing") {
		t.Errorf("should say the opening read is missing; got:\n%s", got)
	}
}

func TestInstanceVerdict_StringIsUnambiguous(t *testing.T) {
	// These land in failure output; "unknown" must never render as
	// something a reader could mistake for a clean result.
	for v, want := range map[InstanceVerdict]string{
		InstanceSame:     "same-instance",
		InstanceRecycled: "recycled",
		InstanceUnknown:  "unknown",
	} {
		if got := v.String(); got != want {
			t.Errorf("InstanceVerdict(%d).String() = %q, want %q", int(v), got, want)
		}
	}
}

// The zero value must be the cautious one. If InstanceSame were zero, a
// verdict variable that never got assigned would read as "fine".
func TestInstanceVerdict_ZeroValueIsUnknown(t *testing.T) {
	var v InstanceVerdict
	if v != InstanceUnknown {
		t.Errorf("zero InstanceVerdict is %v, want %v — the default must be "+
			"'cannot tell', never 'same process'", v, InstanceUnknown)
	}
}
