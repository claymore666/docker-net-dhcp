// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
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
	failAt   time.Duration // when recovery_failed becomes 1; negative means never
	abortAt  time.Duration // when recovery_aborted_container_gone becomes 1; negative means never
	deferred int32
	// unreachableFrom is when the plugin stops answering; negative means
	// it always answers.
	unreachableFrom time.Duration

	elapsed  time.Duration
	polls    int
	budgets  []time.Duration
	verified int // how many polls had happened when the bound was checked; -1 means never
}

func newFakeRecovery() *fakeRecovery {
	return &fakeRecovery{flipAt: -1, failAt: -1, abortAt: -1, unreachableFrom: -1, verified: -1}
}

// succeedsAt, failsAt and abortsAt are the three ends one rebuild can
// reach, named so a case cannot accidentally describe a plugin that does
// two of them. failsAt and abortsAt are the classifier's two arms
// (pkg/plugin/plugin.go:2961 and pkg/plugin/plugin.go:2955) and they
// mean opposite things to a
// reader, so each is driven on its own.
func (f *fakeRecovery) succeedsAt(d time.Duration) *fakeRecovery { f.flipAt = d; return f }
func (f *fakeRecovery) failsAt(d time.Duration) *fakeRecovery    { f.failAt = d; return f }
func (f *fakeRecovery) abortsAt(d time.Duration) *fakeRecovery   { f.abortAt = d; return f }

// verify stands in for the live check that the installed plugin agrees
// with the bound this wait is about to spend. It records how much of the
// wait had already happened when it ran.
func (f *fakeRecovery) verify() { f.verified = f.polls }

func (f *fakeRecovery) read() *HealthResponse {
	if f.unreachableFrom >= 0 && f.elapsed >= f.unreachableFrom {
		return nil
	}
	h := &HealthResponse{RecoveryDeferred: f.deferred, InstanceID: "fake"}
	if f.flipAt >= 0 && f.elapsed >= f.flipAt {
		h.RecoveredOK = 1
	}
	if f.failAt >= 0 && f.elapsed >= f.failAt {
		h.RecoveryFailed = 1
	}
	if f.abortAt >= 0 && f.elapsed >= f.abortAt {
		h.RecoveryAbortedContainerGone = 1
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
		// A failed read leaves the previous one standing, the way Await
		// does: the socket can blink during the events these tests
		// provoke.
		if h := f.read(); h != nil {
			last = h
			if h.RecoveredOK >= 1 {
				return h, true
			}
		}
		f.elapsed += awaitPollInterval
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
	f := newFakeRecovery().succeedsAt(late)
	if f.read().RecoveredOK >= 1 {
		t.Fatal("the fake counted the rebuild at t=0, so it cannot show what a single read misses")
	}

	f = newFakeRecovery().succeedsAt(late)
	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
	if !ok {
		t.Errorf("the wait did not see a rebuild counted at %s, inside the %s budget: %s",
			late, RecoveryRebuildBudget, RecoveryRoutes(h))
	}
	if f.polls != 1 {
		t.Errorf("polls=%d, want 1: a rebuild on the normal route must not reach the deferred budget", f.polls)
	}
}

// The last instant the product allows a success: a client counted at
// exactly AWAIT_TIMEOUT is inside what its own context permits. The
// margin the budget carries is spent by the classifier case below, whose
// counter lands at the far end of the budget rather than this one.
func TestAwaitRecoveryRebuild_CountsARebuildAtTheProductsOwnDeadline(t *testing.T) {
	f := newFakeRecovery().succeedsAt(awaitTimeoutDefault)
	if _, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll); !ok {
		t.Errorf("a rebuild counted at %s — the last instant AWAIT_TIMEOUT allows one — was missed "+
			"by a %s budget", awaitTimeoutDefault, RecoveryRebuildBudget)
	}
}

func TestAwaitRecoveryRebuild_FailsWhenTheCounterNeverMoves(t *testing.T) {
	f := newFakeRecovery()
	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
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
		f := newFakeRecovery().succeedsAt(late)
		f.deferred = 1
		var logged []string
		h, ok := awaitRecoveryRebuild(func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		}, "a rebuild", f.verify, f.poll)
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
		f := newFakeRecovery().succeedsAt(late)
		if _, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll); ok {
			t.Error("the wait extended past the normal route's budget for a plugin that never deferred")
		}
		if f.polls != 1 {
			t.Errorf("polls=%d, want 1", f.polls)
		}
	})
}

// A Start that fails by exhausting AWAIT_TIMEOUT records nothing at
// that instant: the classifier then inspects the container on a fresh
// context of its own before recovery_failed moves. A budget that ended
// at AWAIT_TIMEOUT would give up inside that gap, call a failed rebuild
// "still in flight", and let the recovery_failed == 0 assertion that
// follows read a document taken before the counter could move.
func TestAwaitRecoveryRebuild_WaitsForTheClassifierToSpeak(t *testing.T) {
	f := newFakeRecovery().failsAt(awaitTimeoutDefault + recoveryPerNetworkTimeoutDefault)
	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
	if ok {
		t.Fatal("the wait reported a rebuild for a plugin that only ever recorded a failure")
	}
	if h == nil || h.RecoveryFailed != 1 {
		t.Fatalf("the wait gave up before the classifier moved: %s. Everything after this reads a "+
			"document in which the failure has not happened yet", RecoveryRoutes(h))
	}
	verdict := recoveryVerdict(h)
	if !strings.Contains(verdict, "recovery_failed=1") || !strings.Contains(verdict, "FAILED") {
		t.Errorf("the verdict does not name the arm that fired:\n%s", verdict)
	}
	got := RecoveryRebuildFailure("a rebuild", h)
	if strings.Contains(got, "still in flight") {
		t.Errorf("the verdict calls a classified failure a rebuild still in flight, which is the "+
			"reading this change exists to separate:\n%s", got)
	}
}

// The opposite direction of the same text: nothing was classified, so
// "still in flight" is the right sentence and the classifier one is not.
func TestRecoveryRebuildFailure_SaysStillInFlightWhenNothingWasClassified(t *testing.T) {
	got := RecoveryRebuildFailure("a rebuild", &HealthResponse{})
	if !strings.Contains(got, "still in flight") {
		t.Errorf("a wait that ended with every counter at zero does not say so:\n%s", got)
	}
	if strings.Contains(got, "FAILED") {
		t.Errorf("a wait that ended with every counter at zero claims a failure was recorded:\n%s", got)
	}
}

// A plugin that answers, records a failure and then stops answering
// inside one poll. CounterWindow.Await keeps the last successful read
// across failed ones (counterwindow_live.go:139-152), so the fake must
// too: a fake that let a failed read erase the good one would report
// "no counter could be read" for a wait that had read the classifier,
// and this case would then be the one place the difference showed.
func TestAwaitRecoveryRebuild_KeepsTheLastReadWhenThePluginGoesAway(t *testing.T) {
	f := newFakeRecovery().failsAt(2 * time.Second)
	f.unreachableFrom = 5 * time.Second

	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
	if ok {
		t.Fatal("the wait reported a rebuild from a plugin that recorded a failure and went away")
	}
	if h == nil || h.RecoveryFailed != 1 {
		t.Fatalf("the failed reads erased the classifier's own read: %s", RecoveryRoutes(h))
	}
	if f.polls != 1 {
		t.Errorf("polls=%d, want 1: this plugin never deferred", f.polls)
	}
}

// A plugin that stops answering during the extension leaves the
// extension with no read of its own. Discarding the first poll's read
// there would quote the normal route's budget for a wait that took the
// deferred one, and quote no counters for a plugin that had published
// recovery_deferred.
func TestAwaitRecoveryRebuild_KeepsTheLastReadWhenTheExtensionGetsNone(t *testing.T) {
	f := newFakeRecovery()
	f.deferred = 1
	f.unreachableFrom = RecoveryRebuildBudget

	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
	if ok {
		t.Fatal("the wait reported a rebuild from a plugin that stopped answering")
	}
	if h == nil || h.RecoveryDeferred != 1 {
		t.Fatalf("the extension discarded the only read there was: %s", RecoveryRoutes(h))
	}
	if got := RecoveryRebuildFailure("a rebuild", h); !strings.Contains(got, RecoveryDeferredRebuildBudget.String()) {
		t.Errorf("the failure text quotes a budget this wait did not spend:\n%s", got)
	}
}

// The headline number is derived from this tree's manifest, and the
// plugin that answered may have been set to another value. That is a
// recorded failure by then, but the failure text is what gets read, and
// a bound quoted as though it were the product's is the same mistake
// one level up: a document taken under a premise that does not hold,
// printed as though it did.
func TestRecoveryRebuildFailure_SaysWhereItsBudgetComesFrom(t *testing.T) {
	got := RecoveryRebuildFailure("a rebuild", &HealthResponse{})
	for _, want := range []string{"AWAIT_TIMEOUT", awaitTimeoutDefault.String(), "drift check"} {
		if !strings.Contains(got, want) {
			t.Errorf("the failure text quotes a budget without saying %q, so its first line reads "+
				"as a fact about the plugin that answered:\n%s", want, got)
		}
	}
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

// The classifier's OTHER arm, driven on its own. A recycle that records
// recovery_aborted_container_gone is one where a Start failed and the
// container was gone when the plugin looked afterwards
// (pkg/plugin/plugin.go:2955): the endpoint is not coming back and
// nothing is pending. A verdict keyed on recovery_failed alone reports
// that as a rebuild still in flight, which sends its reader looking for
// a hang that is not there — and the counters printed beside it say
// nothing, because the reader has just been told which sentence to
// believe.
func TestAwaitRecoveryRebuild_NamesTheContainerGoneArm(t *testing.T) {
	f := newFakeRecovery().abortsAt(awaitTimeoutDefault + recoveryPerNetworkTimeoutDefault)
	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
	if ok {
		t.Fatal("the wait reported a rebuild for a plugin whose container had already exited")
	}
	if h == nil || h.RecoveryAbortedContainerGone != 1 {
		t.Fatalf("the wait gave up before the classifier moved: %s", RecoveryRoutes(h))
	}
	if h.RecoveryFailed != 0 {
		t.Fatalf("the fake moved both arms, so this case cannot show what the second one adds: %s",
			RecoveryRoutes(h))
	}
	// On the verdict alone, not on the whole failure text: RecoveryRoutes
	// prints every counter into that same string, so an assertion there
	// would be satisfied by the routes line whether the verdict named
	// this arm or not.
	verdict := recoveryVerdict(h)
	if !strings.Contains(verdict, "recovery_aborted_container_gone=1") {
		t.Errorf("the verdict does not name the arm that fired:\n%s", verdict)
	}
	if !strings.Contains(verdict, "gone when the plugin looked afterwards") {
		t.Errorf("the verdict names the counter but not what it means, and the two arms want "+
			"different next steps:\n%s", verdict)
	}
	if strings.Contains(verdict, "still in flight") {
		t.Errorf("the verdict calls an endpoint whose Start failed against a departed container a "+
			"rebuild that is still running:\n%s", verdict)
	}
	if got := RecoveryRebuildFailure("a rebuild", h); !strings.Contains(got, verdict) {
		t.Errorf("the call-site text does not carry the verdict:\n%s", got)
	}
}

// The deferred route needs the classifier term too. A Start that fails
// by exhausting AWAIT_TIMEOUT records nothing at that instant whichever
// route reached it, so a deferred budget written out longhand without
// that term gives up inside exactly the gap the normal route's budget
// was widened to cover: the same defect, fixed on one route and left
// standing on the other.
func TestAwaitRecoveryRebuild_WaitsForTheClassifierOnTheDeferredRouteToo(t *testing.T) {
	// The far end of what the deferred route permits: the wait for the
	// daemon, then the walk's own budget, then a Start that burns
	// AWAIT_TIMEOUT, then the classifier on its fresh context.
	latest := recoveryDeferredDaemonWaitDefault + recoveryBudgetDefault +
		awaitTimeoutDefault + recoveryPerNetworkTimeoutDefault
	f := newFakeRecovery().failsAt(latest)
	f.deferred = 1

	h, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll)
	if ok {
		t.Fatal("the wait reported a rebuild for a plugin that only ever recorded a failure")
	}
	if h == nil || h.RecoveryFailed != 1 {
		t.Fatalf("the deferred budget gave up before the classifier could speak at %s: %s. The "+
			"recovery_failed == 0 assertion that follows would then read a document taken before "+
			"the counter could move", latest, RecoveryRoutes(h))
	}
	if f.polls != 2 {
		t.Errorf("polls=%d, want 2: this rebuild is only reachable through the extension", f.polls)
	}
	if got := RecoveryRebuildFailure("a rebuild", h); strings.Contains(got, "still in flight") {
		t.Errorf("the verdict calls a classified failure on the deferred route a rebuild still in "+
			"flight:\n%s", got)
	}
}

// A wait whose every read failed holds no document at all, and it is the
// wait most likely to be read by someone who has just lost the plugin.
// Both halves of that path are load-bearing: RecoveryRebuildFailure
// reads recovery_deferred off the document to pick the budget it quotes,
// and the verdict reads the counters back to choose its sentence. Either
// one taken on nothing is a crash in the failure path of another test,
// which is where a crash is least legible.
func TestRecoveryRebuildFailure_SaysSoWhenNoReadEverSucceeded(t *testing.T) {
	got := RecoveryRebuildFailure("a rebuild", nil)
	if !strings.Contains(got, "No counter could be read") {
		t.Errorf("a wait that never got a health read does not say so, so its absent counters read "+
			"as measured zeroes:\n%s", got)
	}
	for _, unwanted := range []string{"still in flight", "FAILED"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a wait that never got a health read claims %q about counters it never read:"+
				"\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, RecoveryRebuildBudget.String()) {
		t.Errorf("the failure text quotes no budget, so it does not say how long it waited:\n%s", got)
	}
}

// The manifest is what the plugin is BUILT from; `docker plugin set`
// decides what it RUNS with, and AWAIT_TIMEOUT is settable. The
// coupling test above reads the manifest and would stay green through
// exactly that override, which puts the original flake back with a wait
// in front of it. These are the readings of the installed value, and
// only one of them lets the wait proceed.
func TestInstalledAwaitTimeoutDrift(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		wantOK  bool
		mustSay string
	}{
		{
			name:   "the installed value is the one the budgets are derived from",
			env:    []string{"LOG_LEVEL=trace", "AWAIT_TIMEOUT=10s", "STATE_DIR=/var/lib/net-dhcp"},
			wantOK: true,
		},
		{
			name:    "docker plugin set raised it",
			env:     []string{"AWAIT_TIMEOUT=30s"},
			mustSay: "30s",
		},
		{
			name:    "docker plugin set lowered it",
			env:     []string{"AWAIT_TIMEOUT=2s"},
			mustSay: "2s",
		},
		{
			name:    "the setting is not a duration",
			env:     []string{"AWAIT_TIMEOUT=forever"},
			mustSay: "not a duration",
		},
		{
			name:    "the plugin publishes no such setting",
			env:     []string{"LOG_LEVEL=trace"},
			mustSay: "publishes no AWAIT_TIMEOUT",
		},
		{
			name:    "no settings at all",
			env:     nil,
			mustSay: "publishes no AWAIT_TIMEOUT",
		},
		{
			// Both ends, because a match on one of them is not a match.
			// Either impostor read as this setting would let a plugin
			// that never published AWAIT_TIMEOUT pass the check, and
			// with a value that is not the cap anything runs under.
			name:    "another setting whose name ends the same way",
			env:     []string{"EXTRA_AWAIT_TIMEOUT=10s"},
			mustSay: "publishes no AWAIT_TIMEOUT",
		},
		{
			name:    "another setting whose name starts the same way",
			env:     []string{"AWAIT_TIMEOUT_MS=10000"},
			mustSay: "publishes no AWAIT_TIMEOUT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := InstalledAwaitTimeoutDrift(tc.env)
			if tc.wantOK {
				if got != "" {
					t.Errorf("a plugin running the value the budgets are derived from is reported as "+
						"drift, which reds every recycle test on a correct lane:\n%s", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("env %v is passed as agreeing with %s, so the wait below spends a bound "+
					"the installed plugin contradicts", tc.env, awaitTimeoutDefault)
			}
			if !strings.Contains(got, tc.mustSay) {
				t.Errorf("the message does not say %q, so a reader cannot tell what disagreed:\n%s",
					tc.mustSay, got)
			}
			if !strings.Contains(got, awaitTimeoutDefault.String()) {
				t.Errorf("the message never quotes the value the budgets are derived from:\n%s", got)
			}
		})
	}
}

// The verdict describes what each counter MEANS, and that description
// is keyed on the sites that move the counter, not on the counter's
// name. recovery_failed has three of them and only one is a failing
// Start: the other two are a walk-level failure before any Start
// reached the endpoint, and a deferred walk whose daemon never came
// back. A verdict that tells its reader to look at Start on either of
// those sends them to the one thing that did not happen, which is the
// same class of wrong answer this branch exists to stop.
//
// So this is a case per SITE, read out of the product: every line that
// increments one of the three counters must be cited by the verdict
// that names it, and every line the verdict cites must still be one of
// them. A new site added to the plugin fails this, and so does a
// citation left behind by one that moved.
func TestRecoveryVerdictCitesEveryIncrementSite(t *testing.T) {
	const pluginSrc = "../../../pkg/plugin/plugin.go"
	failed := incrementSites(t, pluginSrc, "recoveryFailed")
	gone := incrementSites(t, pluginSrc, "recoveryAbortedContainerGone")
	ok := incrementSites(t, pluginSrc, "recoveredOK")

	for _, tc := range []struct {
		name    string
		verdict string
		want    []int
	}{
		{
			name:    "the classified verdict",
			verdict: recoveryVerdict(&HealthResponse{RecoveryFailed: 1}),
			want:    append(append([]int{}, failed...), gone...),
		},
		{
			name:    "the still-in-flight verdict",
			verdict: recoveryVerdict(&HealthResponse{}),
			want:    ok,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := map[int]string{}
			for _, line := range tc.want {
				want[line] = "wanted"
			}
			cited := map[int]bool{}
			for _, c := range citedLines(tc.verdict) {
				for n := c.from; n <= c.through; n++ {
					cited[n] = true
				}
				if !c.satisfiedBy(want) {
					t.Errorf("the verdict cites %s, which no longer increments a counter this "+
						"verdict names. A citation that has moved is worse than none: it sends its "+
						"reader to a line chosen by a previous version of the plugin:\n%s",
						c, tc.verdict)
				}
			}
			for _, line := range tc.want {
				if !cited[line] {
					t.Errorf("pkg/plugin/plugin.go:%d moves one of the counters this verdict names "+
						"and the verdict does not cite it, so the sentence describes fewer ways to "+
						"reach that counter than the plugin has:\n%s", line, tc.verdict)
				}
			}
		})
	}
}

// incrementSites returns the line numbers of every `p.<counter>.Add(1)`
// in a Go source file. None found is a failure for the same reason it is
// in constDuration: the point of the helper is that the sites live
// elsewhere, so "not found" is the drift it exists to catch.
func incrementSites(t *testing.T, path, counter string) []int {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	re := regexp.MustCompile(`^\s*p\.` + regexp.QuoteMeta(counter) + `\.Add\(1\)\s*$`)
	var lines []int
	for i, line := range strings.Split(string(src), "\n") {
		if re.MatchString(line) {
			lines = append(lines, i+1)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no `p.%s.Add(1)` in %s. Either the counter was renamed, is incremented some "+
			"other way, or is gone, and in every case the verdict's description of it is "+
			"unverified", counter, path)
	}
	return lines
}

// The bound is checked before any of it is spent. A check that runs
// after the wait has given its answer is not a check: the budget it
// would have rejected has already been spent and the verdict already
// printed. This drives the order instead of reading it, because the two
// spellings that break it, a deferred call and a call moved below the
// poll, look identical to a source scan of the live file.
func TestAwaitRecoveryRebuild_ChecksTheBoundBeforeSpendingIt(t *testing.T) {
	f := newFakeRecovery().succeedsAt(time.Second)
	if _, ok := awaitRecoveryRebuild(discardf, "a rebuild", f.verify, f.poll); !ok {
		t.Fatal("the wait missed a rebuild counted at 1s")
	}
	if f.verified != 0 {
		t.Errorf("the installed bound was checked after %d poll(s) had already run, want 0. By then "+
			"the wait has spent the budget the check exists to reject", f.verified)
	}
}

// Both live waits must pass a check that does something. The parameter
// makes the ORDER observable above; this is the other half, that what
// gets passed is the real check. There is no local control for it: the
// two waits are behind the `integration` build tag, so a closure that
// does nothing compiles and every local test stays green.
func TestBothWaitsPassTheInstalledTimeoutCheck(t *testing.T) {
	const liveSrc = "recoveryobserver_live.go"
	for _, fn := range []string{"AwaitRecoveryRebuildWindow", "AwaitRecoveryRebuildOn"} {
		t.Run(fn, func(t *testing.T) {
			if body := funcBody(t, liveSrc, fn); !strings.Contains(body, "checkInstalledAwaitTimeout(") {
				t.Errorf("%s spends the recovery budget without checking it against the plugin the "+
					"lane installed, so `docker plugin set AWAIT_TIMEOUT` moves the product's cap "+
					"and this wait keeps the old bound", fn)
			}
		})
	}

	// The check REPORTS, it does not stop the test. One of the three
	// sites, TestRecovery_DaemonRestart_PreservesContainer, is written
	// so that a failed recycle still says which properties held: a
	// switch over the two preservation paths, then the IP and the MAC.
	// A fatal check at the top of its wait would replace all of that
	// with one line, and it would buy nothing, because the drift is
	// already recorded as a failure.
	t.Run("it reports without stopping the test", func(t *testing.T) {
		body := funcBody(t, liveSrc, "checkInstalledAwaitTimeout")
		if strings.Contains(body, "Fatal") {
			t.Errorf("checkInstalledAwaitTimeout stops the test it fails. The daemon-restart recycle "+
				"is written to keep reporting after a failure, and this runs before its first "+
				"assertion:\n%s", body)
		}
		if !strings.Contains(body, "t.Errorf(") {
			t.Errorf("checkInstalledAwaitTimeout reports nothing that fails a test, so a bound the "+
				"installed plugin contradicts is spent in silence:\n%s", body)
		}
	})
}

// funcBody returns the text of a top-level function, from its `func`
// line to the closing brace in the first column.
func funcBody(t *testing.T, path, name string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	start := strings.Index(string(src), "\nfunc "+name+"(")
	if start < 0 {
		t.Fatalf("no top-level func %s in %s. It was renamed or moved, and what called it is "+
			"unverified here", name, path)
	}
	rest := string(src)[start+1:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("func %s in %s has no closing brace in the first column", name, path)
	}
	return rest[:end]
}

// The parser has to read the spellings these files ALREADY use, not the
// one the last fix happened to produce. The stale citation this sweep
// exists for wrote its wrong number bare, after a comma, and a parser
// keyed on the full path in front of every number would have seen the
// defect only once it had been rewritten. The last case is the live
// citation in recoveryobserver.go that carries both shapes at once.
func TestCitedLines_ReadsTheSpellingsTheseFilesUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []citation
	}{
		{
			// The spelling of the stale citation. A fixture is not an
			// assertion about the plugin, which is why the sweep below
			// reads comments and leaves string literals alone.
			name: "the stale citation, in its own spelling",
			text: "and only then it moves, at (pkg/plugin/plugin.go:2936, 2941). A budget",
			want: []citation{{2936, 2936}, {2941, 2941}},
		},
		{
			name: "a range followed by a bare number",
			text: "before Listen binds the socket (pkg/plugin/plugin.go:3186-3188, 3249), so a test",
			want: []citation{{3186, 3188}, {3249, 3249}},
		},
		{
			name: "the path repeated after a conjunction",
			text: "two arms (pkg/plugin/plugin.go:2936 and pkg/plugin/plugin.go:2930) and they",
			want: []citation{{2936, 2936}, {2930, 2930}},
		},
		{
			name: "two separate citations are two claims, not one list",
			text: "still present (pkg/plugin/plugin.go:2936), a walk-level failure (pkg/plugin/plugin.go:2532).",
			want: []citation{{2936, 2936}, {2532, 2532}},
		},
		{
			// The defect's own spelling. The long form is what the fix
			// wrote; this is what the next hand writes.
			name: "the short spelling, with no package directory",
			text: "and only then it moves, at (plugin.go:2936, 2941). A budget",
			want: []citation{{2936, 2936}, {2941, 2941}},
		},
		{
			name: "the short spelling repeated after a conjunction",
			text: "two arms (plugin.go:2936 and plugin.go:2930) and they",
			want: []citation{{2936, 2936}, {2930, 2930}},
		},
		{
			name: "a number that is not a citation is not taken",
			text: "the #405 shape (pkg/plugin/plugin.go:2944), 15 characters later",
			want: []citation{{2944, 2944}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := citedLines(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("read %v, want %v. A citation this parser cannot see is one the sweep "+
					"below reports nothing about", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("citation %d is %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A cited line NUMBER is a claim about that line, and nothing checked
// one. The verdict test above checks that each counter NAME is cited at
// every site that moves it; it says nothing about the citations in the
// prose around them, and two of those named pkg/plugin/plugin.go:2941 as
// a classifier arm when :2941 is a cleanup call. A reader sent there
// finds removeDHCPManagerIfSame and no counter at all.
//
// So: wherever a comment names one of these counters, or an arm, just
// before a line citation, that line must still be one that moves one of
// them. The sweep is keyed on the subject and reads the run-up to the
// citation, because that run-up is what makes the citation a claim about
// an arm instead of a pointer at some other line.
//
// A sweep is a universal, and a universal is satisfied by an empty
// domain. Two things empty this one without touching a word of the
// prose: an extractor that returns nothing, and an anchor that has
// stopped matching a spelling these files use. Both are checked below,
// against a count taken off the raw source and against a per-file
// statement of what the answer should be.
func TestEveryCitedArmIsAnIncrementSite(t *testing.T) {
	const pluginSrc = "../../../pkg/plugin/plugin.go"
	site := map[int]string{}
	for _, counter := range []string{"recoveryFailed", "recoveryAbortedContainerGone", "recoveredOK"} {
		for _, line := range incrementSites(t, pluginSrc, counter) {
			site[line] = counter
		}
	}
	// "arm" in lower case only: a heading shouting THE FAILURE ARM is
	// not a claim about a line.
	claims := []string{"recovery_failed", "recovery_aborted_container_gone", "recovered_ok", "arm"}

	// What each file is expected to answer. An empty result reads the
	// same whether the file is wiring that claims nothing or the sweep
	// has gone blind, and those want opposite reactions, so each file
	// says which one it is.
	for _, f := range []struct {
		path       string
		wantClaims bool
	}{
		{"recoveryobserver.go", true},
		{"recoveryobserver_live.go", false},
		{"recoveryobserver_test.go", true},
	} {
		t.Run(f.path, func(t *testing.T) {
			// Comments only. The citations in string literals are the
			// verdict text, and TestRecoveryVerdictCitesEveryIncrementSite
			// reads those in both directions already: every line it cites
			// moves a counter it names, and every such site is cited.
			// Sweeping them here too would add nothing but this file's
			// own parser fixtures, which are spellings and not claims.
			//
			// One comment group at a time, because a counter named in the
			// block above a citation, with code in between, is not that
			// citation's run-up.
			parsed, claimed := 0, 0
			for _, prose := range commentBlocks(t, f.path) {
				for _, loc := range citationAnchor.FindAllStringIndex(prose, -1) {
					// One anchor can carry a list, and the numbers after
					// the first are written bare. Parse from the anchor
					// so every number in the list is judged, not only the
					// one the path is spelled in front of.
					cites, _ := parseCitationList(prose[loc[1]:])
					parsed += len(cites)
					runUp := prose[max(0, loc[0]-90):loc[0]]
					if !containsAny(runUp, claims) {
						continue
					}
					for _, c := range cites {
						claimed++
						if !c.satisfiedBy(site) {
							t.Errorf("%s says ...%q and cites %s for it, but no counter is incremented "+
								"there. A citation that points at the wrong line is worse than none: "+
								"its reader goes and looks", f.path, runUp, c)
						}
					}
				}
			}
			// The reader above is held against a second one that shares
			// none of its machinery: `plugin.go:` counted off the raw
			// comment lines. If the sweep sees fewer citations than the
			// file plainly contains, the ones it missed are asserted
			// about by nothing at all.
			if anchors := commentAnchors(t, f.path); parsed < anchors {
				t.Errorf("%s carries %d `plugin.go:` citations in its comments and the sweep read "+
					"%d. Whatever it could not read, it is silent about, and silence here passes",
					f.path, anchors, parsed)
			}
			switch {
			case f.wantClaims && claimed == 0:
				t.Errorf("%s makes no claim about an arm anywhere near a citation, and this table "+
					"says it should. Either the prose stopped making them or the sweep stopped "+
					"seeing them. If the prose really changed, change the table with it", f.path)
			case !f.wantClaims && claimed > 0:
				t.Errorf("%s makes %d such claims and this table says it makes none. They were "+
					"judged above; the table is what is wrong, and it has to be right for an "+
					"empty result anywhere else to mean anything", f.path, claimed)
			}
		})
	}
}

// commentBlocks reads a Go file's comment groups, each as one line of
// running text with the markers taken out, so a sentence wrapped across
// several lines reads as one and two groups never run together.
func commentBlocks(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	for _, group := range file.Comments {
		out = append(out, strings.Join(strings.Fields(group.Text()), " "))
	}
	return out
}

// commentAnchors counts the citations in a file's comment lines the
// blunt way, off the bytes, so that it cannot go quiet for any of the
// reasons commentBlocks and the citation pattern can.
func commentAnchors(t *testing.T, path string) int {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			n += strings.Count(line, "plugin.go:")
		}
	}
	return n
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
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
		{"recoveryPerNetworkTimeout", recoveryPerNetworkTimeoutDefault},
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

// citedLines pulls every plugin.go line number out of a text.
//
// A citation is not always spelled with the path in front of the
// number, and the path is not always spelled in full. These files
// already write `(pkg/plugin/plugin.go:3186-3188, 3249)`, and the stale
// citation this test exists for was `(pkg/plugin/plugin.go:2936,
// 2941)`: the wrong number was the BARE one. The package directory is
// optional in the same way, because `plugin.go:2941` is what a hand
// writes when it is already talking about the plugin, and a reader that
// only saw the long form would see the defect only in the spelling the
// fix happened to use, which is the spelling least likely to be wrong
// next time. So after the path, this keeps taking numbers for as long
// as they are joined by a comma, a semicolon, a range dash, the word
// "and", or a repeat of the path in either spelling.
//
// A range is treated as the block it names: `A-B` is satisfied when
// any line in A..B qualifies, because a range cites a passage and not a
// statement. A comma list is the opposite: each number is its own
// claim, and each has to hold.
func citedLines(text string) []citation {
	var out []citation
	consumedTo := 0
	for _, loc := range citationAnchor.FindAllStringIndex(text, -1) {
		// An anchor inside a list already read is part of that list, not
		// a second claim: "A and pkg/plugin/plugin.go:B" is one citation
		// of two lines.
		if loc[0] < consumedTo {
			continue
		}
		cites, n := parseCitationList(text[loc[1]:])
		out = append(out, cites...)
		consumedTo = loc[1] + n
	}
	return out
}

var (
	citationAnchor = regexp.MustCompile(`(?:pkg/plugin/)?plugin\.go:`)
	citationNumber = regexp.MustCompile(`^(\d+)`)
	citationJoiner = regexp.MustCompile(`^(\s*-\s*|\s*[,;]\s*|\s+and\s+)((?:pkg/plugin/)?plugin\.go:)?`)
)

// parseCitationList reads ONE citation list, starting immediately after
// the path. It stops at the first thing that is not another number in
// the same list, so a citation elsewhere in the text is a separate
// claim and is not swept into this one.
func parseCitationList(rest string) ([]citation, int) {
	var out []citation
	consumed, ranged := 0, false
	for {
		m := citationNumber.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			break
		}
		if ranged && len(out) > 0 {
			out[len(out)-1].through = n
		} else {
			out = append(out, citation{from: n, through: n})
		}
		rest = rest[len(m[1]):]
		consumed += len(m[1])
		j := citationJoiner.FindStringSubmatch(rest)
		if j == nil {
			break
		}
		ranged = strings.Contains(j[1], "-")
		rest = rest[len(j[0]):]
		consumed += len(j[0])
	}
	return out, consumed
}

// citation is one cited place: a single line, or a range naming a block.
type citation struct{ from, through int }

func (c citation) String() string {
	if c.from == c.through {
		return fmt.Sprintf("pkg/plugin/plugin.go:%d", c.from)
	}
	return fmt.Sprintf("pkg/plugin/plugin.go:%d-%d", c.from, c.through)
}

// satisfiedBy reports whether any line this citation names is in lines.
func (c citation) satisfiedBy(lines map[int]string) bool {
	for n := c.from; n <= c.through; n++ {
		if lines[n] != "" {
			return true
		}
	}
	return false
}
