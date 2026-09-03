package main_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/internal/gates/gatetest"
	"github.com/claymore666/dhcp-golib/internal/gates/rings"
)

// These drive T2's policy tables through the real gate binary. See the same
// file in the t1 package for why table assertions alone are not evidence.

var gateBin string

func TestMain(m *testing.M) {
	b, cleanup, err := gatetest.BuildForMain()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gateBin = b
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func bin(t *testing.T) string {
	t.Helper()
	return gateBin
}

// TestTestRefusedIdentsAreRefusedByTheGate: every identifier on
// TestRefusedIdents is actually refused when a _test.go file names it.
func TestTestRefusedIdentsAreRefusedByTheGate(t *testing.T) {
	if len(rings.TestRefusedIdents) == 0 {
		t.Fatal("TestRefusedIdents is empty; this test would pass having driven nothing")
	}
	for _, pkg := range sortedKeys(rings.TestRefusedIdents) {
		idents := rings.TestRefusedIdents[pkg]
		t.Run(pkg, func(t *testing.T) {
			if len(idents) == 0 {
				t.Fatalf("%q has an empty refusal list", pkg)
			}
			local := pkg[strings.LastIndex(pkg, "/")+1:]
			var b strings.Builder
			fmt.Fprintf(&b, "package proto\n\nimport %q\n\n", pkg)
			for _, id := range idents {
				fmt.Fprintf(&b, "var _ = %s.%s\n", local, id)
			}
			root := gatetest.Fixture(t, map[string]string{"proto/refused_test.go": b.String()})
			code, out := gatetest.Run(t, bin(t), root)
			if code != gatetest.Violate {
				t.Fatalf("exit code = %d, want %d (VIOLATION)\noutput:\n%s", code, gatetest.Violate, out)
			}
			for _, id := range idents {
				if !strings.Contains(out, local+"."+id) {
					t.Errorf("a test named %s.%s and the gate did not report it. "+
						"A test must not wait on the clock.\noutput:\n%s", local, id, out)
				}
			}
		})
	}
}

// TestTestAllowlistIsAccepted is the PRESERVATION CONTROL for T2: a test file
// naming every admitted identifier of every restricted package must PASS.
//
// This is the control that matters most for T2's future. Driving a fake clock
// needs durations and instants, and a gate that refused those would be
// unusable and would be weakened the first week somebody met it — which is
// exactly the failure this milestone exists to prevent.
func TestTestAllowlistIsAccepted(t *testing.T) {
	if len(rings.TestIdents) == 0 {
		t.Fatal("TestIdents is empty; this control would pass having driven nothing")
	}
	var imports, decls strings.Builder
	named := 0
	for _, pkg := range sortedKeys(rings.TestIdents) {
		local := pkg[strings.LastIndex(pkg, "/")+1:]
		fmt.Fprintf(&imports, "\t%q\n", pkg)
		for _, id := range sortedKeys(rings.TestIdents[pkg]) {
			fmt.Fprintf(&decls, "var _ = %s.%s\n", local, id)
			named++
		}
	}
	if named == 0 {
		t.Fatal("no allowlisted identifiers were named; the control would prove nothing")
	}
	src := "package proto\n\nimport (\n" + imports.String() + ")\n\n" + decls.String()

	root := gatetest.Fixture(t, map[string]string{"proto/allowed_test.go": src})
	code, out := gatetest.Run(t, bin(t), root)
	if code != gatetest.Pass {
		t.Fatalf("a legitimate fake-clock test was refused: exit %d, want PASS.\noutput:\n%s", code, out)
	}
	t.Logf("preservation control: %d allowlisted identifiers named across %d packages, PASS",
		named, len(rings.TestIdents))
}

// TestFakeClockTestIsAccepted is the same control in the shape a real test
// will actually take: a table-driven lifecycle test that advances a clock it
// owns. The generated control above proves every allowlisted name is
// individually accepted; this proves the combination reads like real code.
func TestFakeClockTestIsAccepted(t *testing.T) {
	src := `package proto

import (
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestLeaseLifecycle(t *testing.T) {
	c := &fakeClock{now: time.Unix(0, 0)}
	for _, step := range []time.Duration{30 * time.Second, 15 * time.Minute, 2 * time.Hour} {
		c.Advance(step)
		if !c.Now().After(time.Unix(0, 0)) {
			t.Fatal("clock did not advance")
		}
	}
	if elapsed := c.Now().Sub(time.Unix(0, 0)); elapsed != 2*time.Hour+15*time.Minute+30*time.Second {
		t.Fatalf("elapsed = %v", elapsed)
	}
	if c.Now().Before(time.Unix(0, 0)) || c.Now().Equal(time.Unix(0, 0)) {
		t.Fatal("clock went backwards")
	}
	renew, err := time.ParseDuration("12h")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(c.Now().Add(renew)); d < time.Millisecond {
		t.Fatalf("renew window = %v", d)
	}
	// Reading the real clock is allowed and is what a test does to bound its
	// own runtime; waiting on it is not.
	if took := time.Since(time.Now()); took > time.Second {
		t.Fatalf("the test body took %v", took)
	}
	t.Logf("deadline %s", c.Now().Add(renew).UTC().Format(time.RFC3339))
}
`
	root := gatetest.Fixture(t, map[string]string{"proto/lifecycle_test.go": src})
	code, out := gatetest.Run(t, bin(t), root)
	if code != gatetest.Pass {
		t.Fatalf("a realistic fake-clock lifecycle test was refused: exit %d, want PASS.\noutput:\n%s", code, out)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestContextAfterFuncIsRefusedByDefault pins today's answer for the one
// identifier no layer of the policy guards can reach.
//
// context.AfterFunc runs f when ctx is cancelled, not when a clock fires, so
// it is not obviously a T2 violation and it is deliberately NOT on
// rings.TestRefusedIdents — asserting it there would claim an adjudication
// that has not happened. The derived guard cannot reach it either: its
// signature is (ctx Context, f func()) (stop func() bool), which names no
// time.Duration and no time.Time, so TestContextAllowlistExcludesDeadlines
// correctly does not match it.
//
// That leaves its refusal held by nothing but the absence of a key in
// rings.TestIdents["context"] — an absence, and absences are exactly what
// this project keeps finding nobody checked. This case makes the absence a
// behaviour: widening the allowlist to admit AfterFunc turns the gate green
// on the fixture below and this test goes red.
//
// It is a PIN, not a requirement. If a later milestone argues AfterFunc into
// tests, the right move is to delete this case and record the argument — not
// to weaken it, and not to add the key while leaving the case passing by
// some other route.
func TestContextAfterFuncIsRefusedByDefault(t *testing.T) {
	if rings.TestIdents["context"]["AfterFunc"] {
		t.Fatal("rings.TestIdents[\"context\"][\"AfterFunc\"] is true. Admitting " +
			"context.AfterFunc into tests is a policy decision this case pins as " +
			"unmade: delete this test and write down the argument, do not add the key.")
	}
	for _, id := range rings.TestRefusedIdents["context"] {
		if id == "AfterFunc" {
			t.Fatal("context.AfterFunc is on TestRefusedIdents. It fires on cancellation, " +
				"not on a clock; listing it there claims an adjudication that has not happened.")
		}
	}
	src := `package proto

import (
	"context"
	"testing"
)

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { close(done) })
	defer stop()
	cancel()
	<-done
}
`
	root := gatetest.Fixture(t, map[string]string{"proto/cancel_test.go": src})
	code, out := gatetest.Run(t, bin(t), root)
	if code != gatetest.Violate {
		t.Fatalf("context.AfterFunc in a test file: exit %d, want %d (VIOLATION).\n"+
			"Its refusal is held only by its absence from the allowlist; this case is "+
			"what makes that absence observable.\noutput:\n%s", code, gatetest.Violate, out)
	}
	// The exit code alone is not the assertion. The fixture also names
	// context.WithCancel and context.Background, so ANY violation in it
	// satisfies `code == Violate` — including one that never objected to
	// AfterFunc.
	//
	// MEASURED 2026-08-29 by review: removing "Background" from
	// TestIdents["context"] makes the fixture violate for a reason this pin
	// does not own, and this case still passed. That is round 1's finding 2
	// exactly — a case green for the wrong reason — reproduced inside the fix
	// for round 1's findings. Every other gate-driven case in this file and in
	// t1/policy_driven_test.go already asserts the diagnosis names the
	// identifier; this one did not.
	if !strings.Contains(out, "context.AfterFunc") {
		t.Fatalf("the gate refused the fixture but its diagnosis does not name "+
			"context.AfterFunc, so this case is not pinning what it claims to pin.\noutput:\n%s", out)
	}
	// Naming AfterFunc is necessary and NOT sufficient, and the difference is
	// measurable rather than theoretical.
	//
	// The review that raised this proposed exactly the Contains check above as
	// the fix. MEASURED 2026-08-29: it does not kill the review's own mutant.
	// Removing "Background" from TestIdents["context"] makes the gate report
	// TWO findings — AfterFunc AND Background — so the diagnosis still names
	// AfterFunc and the Contains check still passes. The review's supporting
	// sentence ("green over a run that never objected to AfterFunc") does not
	// match what the gate prints; it objects to AfterFunc as finding 1 of 2.
	//
	// The FINDING was right and only the remedy was short. What makes the
	// verdict attributable is that AfterFunc is the ONLY thing wrong here, so
	// the case asserts the rest of the fixture is clean. This is a preservation
	// control living inside the pin: it is what stops the pin from being
	// satisfied by an unrelated violation.
	for _, innocent := range []string{"context.Background", "context.WithCancel"} {
		if strings.Contains(out, innocent) {
			t.Fatalf("the gate also objected to %s. That identifier is legitimate in a "+
				"test and this case does not own it, so the VIOLATION above is no longer "+
				"attributable to context.AfterFunc — the pin would pass for the wrong "+
				"reason.\noutput:\n%s", innocent, out)
		}
	}
}
