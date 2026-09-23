// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRecovery counts a rebuilt endpoint at flipAt on a virtual clock, polling exactly as CounterWindow.Await does.
type fakeRecovery struct {
	flipAt   time.Duration // when recovered_ok becomes 1; negative means never
	failAt   time.Duration // when recovery_failed becomes 1; negative means never
	abortAt  time.Duration // when recovery_aborted_container_gone becomes 1; negative means never
	deferred int32
	// unreachableFrom is when the plugin stops answering; negative means it always answers.
	unreachableFrom time.Duration

	elapsed  time.Duration
	polls    int
	budgets  []time.Duration
	verified int // how many polls had happened when the bound was checked; -1 means never
}

func newFakeRecovery() *fakeRecovery {
	return &fakeRecovery{flipAt: -1, failAt: -1, abortAt: -1, unreachableFrom: -1, verified: -1}
}

// succeedsAt, failsAt and abortsAt are the three ends one rebuild can reach; failsAt and abortsAt are the
// classifier's two arms (pkg/plugin/plugin.go:recoverOneEndpoint), each driven on its own (#376).
func (f *fakeRecovery) succeedsAt(d time.Duration) *fakeRecovery { f.flipAt = d; return f }
func (f *fakeRecovery) failsAt(d time.Duration) *fakeRecovery    { f.failAt = d; return f }
func (f *fakeRecovery) abortsAt(d time.Duration) *fakeRecovery   { f.abortAt = d; return f }

// verify stands in for the installed-plugin check and records how many polls preceded it.
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
	// CounterWindow.Await samples only before its deadline, so its last sample lands one interval short of it.
	for f.elapsed < deadline {
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

func TestAwaitRecoveryRebuild_CountsARebuildThatFinishesLate(t *testing.T) {
	const late = 9 * time.Second

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
	if f.elapsed > RecoveryRebuildBudget+awaitPollInterval {
		t.Errorf("the wait spent %s on a plugin that never deferred; the budget for that route is %s",
			f.elapsed, RecoveryRebuildBudget)
	}
}

func TestAwaitRecoveryRebuild_ExtendsOnlyOnTheDeferredRoute(t *testing.T) {
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

// A Start that exhausts AWAIT_TIMEOUT moves recovery_failed only after the classifier's own fresh context (#376).
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

func TestRecoveryRebuildFailure_SaysStillInFlightWhenNothingWasClassified(t *testing.T) {
	got := RecoveryRebuildFailure("a rebuild", &HealthResponse{})
	if !strings.Contains(got, "still in flight") {
		t.Errorf("a wait that ended with every counter at zero does not say so:\n%s", got)
	}
	if strings.Contains(got, "FAILED") {
		t.Errorf("a wait that ended with every counter at zero claims a failure was recorded:\n%s", got)
	}
}

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

// recovery_aborted_container_gone means a Start failed and the container was gone when the plugin looked
// (pkg/plugin/plugin.go:recoverOneEndpoint), so nothing is pending; a verdict keyed on recovery_failed alone would call it a
// rebuild still in flight (#376).
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
	// RecoveryRoutes prints every counter into the failure text, so this asserts on the verdict alone.
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

func TestAwaitRecoveryRebuild_WaitsForTheClassifierOnTheDeferredRouteToo(t *testing.T) {
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

// Each counter's site is cited by the function holding it, once per site, under the counter's own name, so a new,
// moved or deleted site and a citation under the wrong counter all go red (#376, #1056).
func TestRecoveryVerdictCitesEveryIncrementSite(t *testing.T) {
	const pluginSrc = "../../../pkg/plugin/plugin.go"
	sites := recoveryIncrementFuncs(t, pluginSrc)

	for _, tc := range []struct {
		name     string
		verdict  string
		counters []string
	}{
		{
			name:     "the classified verdict",
			verdict:  recoveryVerdict(&HealthResponse{RecoveryFailed: 1}),
			counters: []string{"recovery_failed", "recovery_aborted_container_gone"},
		},
		{
			name:     "the still-in-flight verdict",
			verdict:  recoveryVerdict(&HealthResponse{}),
			counters: []string{"recovered_ok"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cited := map[string]map[string]int{}
			for _, c := range citedFuncs(tc.verdict) {
				if c.name == "" {
					t.Errorf("the verdict cites plugin.go by something other than a function name, and a "+
						"line number goes stale on any edit above it:\n%s", tc.verdict)
					continue
				}
				counter := counterBefore(tc.verdict[:c.at])
				if !slices.Contains(tc.counters, counter) {
					t.Errorf("the verdict cites %s under %q, which is not a counter this verdict "+
						"describes:\n%s", c, counter, tc.verdict)
					continue
				}
				if sites[counter][c.name] == 0 {
					t.Errorf("the verdict cites %s for %s, and that function does not increment it. "+
						"Its reader goes and looks:\n%s", c, counter, tc.verdict)
				}
				if cited[counter] == nil {
					cited[counter] = map[string]int{}
				}
				cited[counter][c.name]++
			}
			for _, counter := range tc.counters {
				for fn, n := range sites[counter] {
					if cited[counter][fn] != n {
						t.Errorf("pkg/plugin/plugin.go:%s moves %s at %d site(s) and the verdict cites it "+
							"%d time(s) for that counter, so the sentence describes a different set of "+
							"ways to reach it than the plugin has:\n%s",
							fn, counter, n, cited[counter][fn], tc.verdict)
					}
				}
			}
		})
	}
}

// recoveryCounters maps each health field the verdicts name to the Plugin field that counts it.
var recoveryCounters = map[string]string{
	"recovery_failed":                 "recoveryFailed",
	"recovery_aborted_container_gone": "recoveryAbortedContainerGone",
	"recovered_ok":                    "recoveredOK",
}

// counterBefore returns the health field named last in text, or "" when it names none.
func counterBefore(text string) string {
	best, at := "", -1
	for field := range recoveryCounters {
		if i := strings.LastIndex(text, field); i > at {
			best, at = field, i
		}
	}
	return best
}

// recoveryIncrementFuncs maps each recovery health field to the plugin functions incrementing it and their site counts.
func recoveryIncrementFuncs(t *testing.T, path string) map[string]map[string]int {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := map[string]map[string]int{}
	for field, counter := range recoveryCounters {
		out[field] = map[string]int{}
		total := 0
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if isIncrementOf(n, counter) {
					out[field][fn.Name.Name]++
					total++
				}
				return true
			})
		}
		// The AST walk is held against the line scan, which shares none of its machinery (#1056).
		if lines := incrementSites(t, path, counter); total != len(lines) {
			t.Fatalf("the AST walk found %d `p.%s.Add(1)` in %s and the line scan %d; a site one of "+
				"them cannot see is judged by nothing", total, counter, path, len(lines))
		}
	}
	return out
}

// isIncrementOf reports whether n is the call p.<counter>.Add(1).
func isIncrementOf(n ast.Node, counter string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	add, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || add.Sel.Name != "Add" {
		return false
	}
	field, ok := add.X.(*ast.SelectorExpr)
	if !ok || field.Sel.Name != counter {
		return false
	}
	recv, ok := field.X.(*ast.Ident)
	if !ok || recv.Name != "p" {
		return false
	}
	one, ok := call.Args[0].(*ast.BasicLit)
	return ok && one.Value == "1"
}

// incrementSites returns the line numbers of every `p.<counter>.Add(1)` in a Go source file; none is a failure.
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

// The two live waits are behind the `integration` tag, so nothing untagged compiles them.
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

	// TestRecovery_DaemonRestart_PreservesContainer reports every property that held after a failed recycle.
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

// funcBody returns a top-level function's text, from its `func` line to the first-column closing brace.
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

// The citation reader takes the spellings these files use, and a near-miss name reads as itself (#1056).
func TestCitedFuncs_ReadsTheSpellingsTheseFilesUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"the long spelling", "and only then it moves (pkg/plugin/plugin.go:recoverOneEndpoint). A budget",
			[]string{"recoverOneEndpoint"}},
		{"the short spelling", "and only then it moves (plugin.go:recoverOneEndpoint). A budget",
			[]string{"recoverOneEndpoint"}},
		{"two citations in one list", "(pkg/plugin/plugin.go:NewPlugin, pkg/plugin/plugin.go:Listen) but",
			[]string{"NewPlugin", "Listen"}},
		{"a name that extends another is its own", "back (pkg/plugin/plugin.go:recoverEndpointsDeferred).",
			[]string{"recoverEndpointsDeferred"}},
		{"a line number is read as no name", "the #405 shape (pkg/plugin/plugin.go:2944), 15 characters later",
			[]string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := citedFuncs(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("read %v, want %v. A citation this reader cannot see is one nothing below "+
					"judges", got, tc.want)
			}
			for i := range got {
				if got[i].name != tc.want[i] {
					t.Errorf("citation %d names %q, want %q", i, got[i].name, tc.want[i])
				}
			}
		})
	}
}

// A comment that names a counter or an arm just before a citation claims the cited function moves it; the claim is
// judged against the counter named last, and an empty sweep is checked by a byte count and a per-file table (#1056).
func TestEveryCitedArmIsAnIncrementSite(t *testing.T) {
	const pluginSrc = "../../../pkg/plugin/plugin.go"
	sites := recoveryIncrementFuncs(t, pluginSrc)
	// "arm" in lower case only: a heading shouting THE FAILURE ARM is not a claim about a function.
	claims := []string{"recovery_failed", "recovery_aborted_container_gone", "recovered_ok", "arm"}

	for _, f := range []struct {
		path       string
		wantClaims bool
	}{
		{"recoveryobserver.go", true},
		{"recoveryobserver_live.go", false},
		{"recoveryobserver_test.go", true},
	} {
		t.Run(f.path, func(t *testing.T) {
			// Comments only: TestRecoveryVerdictCitesEveryIncrementSite judges the verdict strings.
			parsed, claimed := 0, 0
			for _, prose := range commentBlocks(t, f.path) {
				for _, c := range citedFuncs(prose) {
					parsed++
					if c.name == "" {
						t.Errorf("%s cites plugin.go by line number, which goes stale on any edit above "+
							"it; cite the function", f.path)
						continue
					}
					runUp := prose[max(0, c.at-90):c.at]
					if !containsAny(runUp, claims) {
						continue
					}
					claimed++
					counter := counterBefore(runUp)
					moves := sites[counter][c.name] > 0
					if counter == "" {
						for field := range recoveryCounters {
							moves = moves || sites[field][c.name] > 0
						}
					}
					if !moves {
						t.Errorf("%s says ...%q and cites %s for it, but that function increments no "+
							"such counter. Its reader goes and looks", f.path, runUp, c)
					}
				}
			}
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

// Every function the observer and the recovery tests cite, in comments and strings, exists in plugin.go (#1056).
func TestEveryCitedFuncExistsInThePlugin(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../../../pkg/plugin/plugin.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing plugin.go: %v", err)
	}
	funcs := map[string]bool{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			funcs[fn.Name.Name] = true
		}
	}
	for _, path := range []string{"recoveryobserver.go", "recoveryobserver_live.go",
		"../recovery_test.go", "../recovery_daemon_test.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, c := range citedFuncs(string(src)) {
			if !funcs[c.name] {
				t.Errorf("%s cites %s, which is not a function in pkg/plugin/plugin.go", path, c)
			}
		}
	}
}

// commentBlocks returns a file's comment groups, each as one line of text without the markers.
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

// commentAnchors counts the citations in a file's comment lines off the raw bytes.
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

// The budgets copy unexported plugin constants the harness cannot import, so this parses them; a rename matches
// nothing and fails (#376).
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

// constDuration reads `const <name> = N * time.Unit` from a Go source file, failing the test when it is absent.
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

// citedFuncs returns every plugin.go citation in text; a citation not followed by a Go identifier has no name.
func citedFuncs(text string) []citation {
	var out []citation
	for _, m := range citationPattern.FindAllStringSubmatchIndex(text, -1) {
		c := citation{at: m[0]}
		if m[2] >= 0 {
			c.name = text[m[2]:m[3]]
		}
		out = append(out, c)
	}
	return out
}

var citationPattern = regexp.MustCompile(`(?:pkg/plugin/)?plugin\.go:([A-Za-z_][A-Za-z0-9_]*)?`)

// citation is one cited plugin.go function and the offset of its citation in the text.
type citation struct {
	at   int
	name string
}

func (c citation) String() string { return "pkg/plugin/plugin.go:" + c.name }
