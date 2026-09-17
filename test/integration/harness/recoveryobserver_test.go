// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeRecovery is a plugin that counts a rebuilt endpoint at flipAt on a
// virtual clock, so the wait can be driven past its own budget without
// spending the wall clock and without a live plugin.
//
// It polls the way CounterWindow.Await polls — sample, test the
// deadline, sleep one interval — because the margin the budgets carry is
// exactly one such interval, and a fake that sampled differently would
// prove nothing about the real loop.
type fakeRecovery struct {
	flipAt   time.Duration // when recovered_ok becomes 1; negative means never
	deferred int32

	elapsed time.Duration
	polls   int
	budgets []time.Duration
}

func (f *fakeRecovery) read() *HealthResponse {
	h := &HealthResponse{RecoveryDeferred: f.deferred, InstanceID: "fake"}
	if f.flipAt >= 0 && f.elapsed >= f.flipAt {
		h.RecoveredOK = 1
	}
	return h
}

func (f *fakeRecovery) poll(budget time.Duration) (*HealthResponse, bool) {
	f.polls++
	f.budgets = append(f.budgets, budget)
	deadline := f.elapsed + budget
	var last *HealthResponse
	// CounterWindow.Await's loop, exactly: it samples only while it is
	// still before its deadline, so the last sample it can take lands one
	// interval short of it. A fake that took one more sample than that
	// would report a budget with no margin as sufficient.
	for f.elapsed < deadline {
		last = f.read()
		if last.RecoveredOK >= 1 {
			return last, true
		}
		f.elapsed += awaitPollInterval
	}
	if last == nil {
		last = f.read()
	}
	return last, false
}

func discardf(string, ...any) {}

// A rebuild counted late is the whole defect: the previous observer read
// the counter once, as early as the socket answered, and called a
// rebuild that had not finished yet a rebuild that never happened.
func TestAwaitRecoveryRebuild_CountsARebuildThatFinishesLate(t *testing.T) {
	const late = 9 * time.Second // inside AWAIT_TIMEOUT, past every plausible single read

	// The previous version, which is the strongest mutant of this
	// change: one read, taken the moment the socket answers.
	f := &fakeRecovery{flipAt: late}
	if f.read().RecoveredOK >= 1 {
		t.Fatal("the fake counted the rebuild at t=0, so it cannot show what a single read misses")
	}

	f = &fakeRecovery{flipAt: late}
	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.poll)
	if !ok {
		t.Errorf("the wait did not see a rebuild counted at %s, inside the %s budget: %s",
			late, RecoveryRebuildBudget, RecoveryRoutes(h))
	}
	if f.polls != 1 {
		t.Errorf("polls=%d, want 1: a rebuild on the normal route must not reach the deferred budget", f.polls)
	}
}

// The margin the budget carries is one poll interval, and this is the
// case that spends it: a client counted at exactly AWAIT_TIMEOUT is
// within what the plugin allows itself, and a poll loop samples only
// while it is before its deadline, so a budget of exactly AWAIT_TIMEOUT
// takes its last sample a quarter of a second too early.
func TestAwaitRecoveryRebuild_CountsARebuildAtTheProductsOwnDeadline(t *testing.T) {
	f := &fakeRecovery{flipAt: awaitTimeoutDefault}
	if _, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.poll); !ok {
		t.Errorf("a rebuild counted at %s — the last instant AWAIT_TIMEOUT allows one — was missed "+
			"by a %s budget", awaitTimeoutDefault, RecoveryRebuildBudget)
	}
}

func TestAwaitRecoveryRebuild_FailsWhenTheCounterNeverMoves(t *testing.T) {
	f := &fakeRecovery{flipAt: -1}
	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.poll)
	if ok {
		t.Fatal("the wait reported a rebuild the fake never counted")
	}
	if h == nil || h.RecoveredOK != 0 {
		t.Errorf("the failing wait returned %s, want the last read with recovered_ok=0", RecoveryRoutes(h))
	}
	// Bounded, and bounded by the SHORT route: a wait that quietly ran
	// to the deferred budget would still pass the assertion above while
	// costing the suite a minute and a half per occurrence.
	if f.elapsed > RecoveryRebuildBudget+awaitPollInterval {
		t.Errorf("the wait spent %s on a plugin that never deferred; the budget for that route is %s",
			f.elapsed, RecoveryRebuildBudget)
	}
}

func TestAwaitRecoveryRebuild_ExtendsOnlyOnTheDeferredRoute(t *testing.T) {
	// Later than the normal route allows, earlier than the deferred one
	// does: the two routes give opposite verdicts on this rebuild, which
	// is what makes the extension observable at all.
	late := RecoveryRebuildBudget + 30*time.Second

	t.Run("deferred", func(t *testing.T) {
		f := &fakeRecovery{flipAt: late, deferred: 1}
		var logged []string
		h, ok := awaitRecoveryRebuild(func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		}, "a rebuild", f.poll)
		if !ok {
			t.Errorf("the wait gave up at the normal route's budget although the plugin reported "+
				"recovery_deferred=1: %s", RecoveryRoutes(h))
		}
		if f.polls != 2 {
			t.Fatalf("polls=%d, want 2 (the normal budget, then the rest of the deferred one)", f.polls)
		}
		if f.budgets[0] != RecoveryRebuildBudget {
			t.Errorf("first budget %s, want %s", f.budgets[0], RecoveryRebuildBudget)
		}
		if want := RecoveryDeferredRebuildBudget - RecoveryRebuildBudget; f.budgets[1] != want {
			t.Errorf("second budget %s, want %s — the extension must take the total to %s, not restart it",
				f.budgets[1], want, RecoveryDeferredRebuildBudget)
		}
		if len(logged) != 1 || !strings.Contains(logged[0], "recovery_deferred=1") {
			t.Errorf("the extension was silent or did not say why: %v", logged)
		}
	})

	t.Run("not deferred", func(t *testing.T) {
		f := &fakeRecovery{flipAt: late}
		if _, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.poll); ok {
			t.Error("the wait extended past the normal route's budget for a plugin that never deferred")
		}
		if f.polls != 1 {
			t.Errorf("polls=%d, want 1", f.polls)
		}
	})
}

func TestRecoveryRebuildFailure_QuotesTheBudgetOfTheRouteTaken(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    *HealthResponse
		want time.Duration
	}{
		{"normal route", &HealthResponse{}, RecoveryRebuildBudget},
		{"deferred route", &HealthResponse{RecoveryDeferred: 1}, RecoveryDeferredRebuildBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RecoveryRebuildFailure("a rebuild", tc.h)
			if !strings.Contains(got, tc.want.String()) {
				t.Errorf("the failure text does not quote %s, so it claims a wait it did not "+
					"perform:\n%s", tc.want, got)
			}
		})
	}
}

// The failure text is the only evidence a reader gets, and recovered_ok
// reading 0 has several causes that are told apart by nothing else.
func TestRecoveryRoutes_NamesEveryRoute(t *testing.T) {
	h := &HealthResponse{
		RecoveredOK: 1, RecoveryFailed: 2, RecoveryAbortedContainerGone: 3,
		RecoveryDeferred: 4, RecoveryAlreadyManaged: 5, RecoveryNetworkGone: 6,
		TombstonesConsumed: 7,
	}
	got := RecoveryRoutes(h)
	for _, want := range []string{
		"recovered_ok=1", "recovery_failed=2", "recovery_aborted_container_gone=3",
		"recovery_deferred=4", "recovery_already_managed=5", "recovery_network_gone=6",
		"tombstones_consumed=7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from the failure text, so that route cannot be told from the "+
				"others:\n%s", want, got)
		}
	}
	if RecoveryRoutes(nil) == "" {
		t.Error("a nil read renders as nothing, which reads as a document with every counter at zero")
	}
}

// The budgets are the load-bearing parameter of the wait, and they are
// copies of numbers that live in the plugin. A copy nobody checks goes
// stale silently and puts the flake back — with a wait in front of it,
// which is worse than no wait, because the test then looks patched.
//
// This reads the declarations rather than trusting them: the harness is
// behind the `integration` build tag and the plugin's constants are
// unexported, so there is no import that would make the drift a compile
// error. A rename on the plugin side fails this test too, because the
// pattern then matches nothing and no-match is a failure here.
func TestRecoveryBudgetsTrackTheProduct(t *testing.T) {
	const pluginSrc = "../../../pkg/plugin/plugin.go"
	for _, tc := range []struct {
		decl string
		want time.Duration
	}{
		{"defaultAwaitTimeout", awaitTimeoutDefault},
		{"recoveryBudget", recoveryBudgetDefault},
		{"recoveryDeferredDaemonWait", recoveryDeferredDaemonWaitDefault},
	} {
		t.Run(tc.decl, func(t *testing.T) {
			if got := constDuration(t, pluginSrc, tc.decl); got != tc.want {
				t.Errorf("%s is %s in %s and %s in the harness. The recovery budgets are derived "+
					"from it, so the wait in the recycle tests is now bounded by the wrong number",
					tc.decl, got, pluginSrc, tc.want)
			}
		})
	}

	t.Run("AWAIT_TIMEOUT in config.json", func(t *testing.T) {
		if got := configEnvDuration(t, "../../../config.json", "AWAIT_TIMEOUT"); got != awaitTimeoutDefault {
			t.Errorf("AWAIT_TIMEOUT ships as %s and the harness budget is derived from %s; the "+
				"installed plugin caps each recovery Start at the shipped value", got, awaitTimeoutDefault)
		}
	})
}

// constDuration reads `const <name> = N * time.Unit` out of a Go source
// file. A declaration it cannot find fails the test: the point of this
// helper is that the value is elsewhere, so "not found" is the drift it
// is looking for, not an excuse to pass.
func constDuration(t *testing.T, path, name string) time.Duration {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	re := regexp.MustCompile(`(?m)^const ` + regexp.QuoteMeta(name) + ` = (\d+) \* time\.(\w+)$`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `const %s = N * time.Unit` in %s. Either it moved, was renamed, or is no "+
			"longer a constant — in every case the harness budget derived from it is unverified", name, path)
	}
	d, err := time.ParseDuration(string(m[1]) + unitSuffix(string(m[2])))
	if err != nil {
		t.Fatalf("parsing %s = %s * time.%s: %v", name, m[1], m[2], err)
	}
	return d
}

func unitSuffix(goUnit string) string {
	switch goUnit {
	case "Nanosecond":
		return "ns"
	case "Microsecond":
		return "us"
	case "Millisecond":
		return "ms"
	case "Second":
		return "s"
	case "Minute":
		return "m"
	case "Hour":
		return "h"
	}
	return "<unknown unit " + goUnit + ">"
}

func configEnvDuration(t *testing.T, path, name string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg struct {
		Env []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	for _, e := range cfg.Env {
		if e.Name != name {
			continue
		}
		d, err := time.ParseDuration(e.Value)
		if err != nil {
			t.Fatalf("%s=%q in %s is not a duration: %v", name, e.Value, path, err)
		}
		return d
	}
	t.Fatalf("no %s entry in %s, so the value the installed plugin runs with is unknown here", name, path)
	return 0
}
