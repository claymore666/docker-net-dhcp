// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// join_attach_slow shipped in v1.4.0 as the only outside-visible sign
// that the #406 grace is doing anything. It read zero on every CI run,
// on all three release-verification runs, and on the production host —
// and nothing anywhere asserted it could move. A constant zero from an
// untested counter is not a measurement: "the daemon-busy window never
// arose" and "the increment never fires" are indistinguishable (#431).
//
// These tests make the increment observable. What they do NOT establish
// is that production ever reaches it: the counter sits behind a
// successful Start, which needs a real network namespace, so whether a
// real host hits a slow-but-successful attach is #403's question, not
// this file's.

func newSlowAttachPlugin(await time.Duration) *Plugin {
	return &Plugin{awaitTimeout: await}
}

var slowAttachReq = JoinRequest{
	NetworkID:  "net-0123456789abcdef",
	EndpointID: "ep-0123456789abcdef",
}

func TestNoteSlowAttach_CountsAnAttachThatOutlastedTheBudget(t *testing.T) {
	p := newSlowAttachPlugin(time.Second)
	if counted := p.noteSlowAttach(slowAttachReq, 5*time.Second); !counted {
		t.Error("an attach taking 5s against a 1s budget was not counted as slow")
	}
	if got := p.joinAttachSlow.Load(); got != 1 {
		t.Errorf("join_attach_slow = %d, want 1 — this is the counter reading zero "+
			"for a window that demonstrably arose, which is the whole of #431", got)
	}
}

func TestNoteSlowAttach_IgnoresAnAttachInsideTheBudget(t *testing.T) {
	p := newSlowAttachPlugin(time.Second)
	if counted := p.noteSlowAttach(slowAttachReq, 250*time.Millisecond); counted {
		t.Error("an attach well inside the budget was counted as slow")
	}
	if got := p.joinAttachSlow.Load(); got != 0 {
		t.Errorf("join_attach_slow = %d, want 0; counting fast attaches would make the "+
			"counter useless in the other direction", got)
	}
}

// The boundary is load-bearing in one direction only: an attach that
// lands exactly on the budget did not need the grace, so counting it
// would overstate how often the grace is carrying work.
func TestNoteSlowAttach_BudgetBoundaryIsExclusive(t *testing.T) {
	p := newSlowAttachPlugin(time.Second)
	if counted := p.noteSlowAttach(slowAttachReq, time.Second); counted {
		t.Error("an attach finishing exactly on budget was counted as slow")
	}
	if counted := p.noteSlowAttach(slowAttachReq, time.Second+time.Nanosecond); !counted {
		t.Error("an attach one nanosecond over budget was not counted")
	}
	if got := p.joinAttachSlow.Load(); got != 1 {
		t.Errorf("join_attach_slow = %d, want exactly 1 across the boundary pair", got)
	}
}

func TestNoteSlowAttach_Accumulates(t *testing.T) {
	// The health surface reports a total, so repeated slow attaches
	// must add up rather than latch at one.
	p := newSlowAttachPlugin(10 * time.Millisecond)
	for i := 0; i < 4; i++ {
		p.noteSlowAttach(slowAttachReq, time.Second)
	}
	if got := p.joinAttachSlow.Load(); got != 4 {
		t.Errorf("join_attach_slow = %d after four slow attaches, want 4", got)
	}
}

// TestNoteSlowAttach_IsTheOnlyIncrementSite keeps the tests above
// meaningful. They exercise one function; if a second increment existed
// elsewhere, the counter could still move for reasons nothing asserts,
// and #431 would quietly reopen.
func TestNoteSlowAttach_IsTheOnlyIncrementSite(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var sites []string
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "joinAttachSlow.Add(") {
				sites = append(sites, name+":"+itoa(i+1))
			}
		}
	}
	if len(sites) != 1 {
		t.Errorf("join_attach_slow is incremented at %d sites (%s); want exactly 1, inside "+
			"noteSlowAttach. Another site would move the counter without any test "+
			"asserting it, which is the state #431 was filed about.",
			len(sites), strings.Join(sites, ", "))
	}
}

// TestJoin_CountsSlowAttachesOnTheSuccessPathOnly pins the wiring the
// unit tests above cannot reach.
//
// noteSlowAttach is only correct where it is called: inside the
// `err == nil` branch of Join's attach goroutine. Called on the failure
// path it would put a genuine fault into a counter documented as not
// healthy-affecting; not called at all, every test above would still
// pass while the counter stayed at zero forever — which is exactly the
// state that shipped.
func TestJoin_CountsSlowAttachesOnTheSuccessPathOnly(t *testing.T) {
	src, err := os.ReadFile("network.go")
	if err != nil {
		t.Fatalf("read network.go: %v", err)
	}
	// The attach goroutine's success branch, as a unit: `err == nil`
	// opening a block that measures the elapsed time ONCE and hands
	// that same value to noteSlowAttach.
	//
	// The elapsed time used to be measured in the call's own argument.
	// #403 added the success-side timing line below it, which needs the
	// same number, and two calls to time.Since would be one fact
	// derived twice: the counter and the log could then disagree about
	// the attach they describe.
	call := regexp.MustCompile(`if err == nil \{\s*\n\s*elapsed := time\.Since\(attachStart\)\s*\n\s*p\.noteSlowAttach\(r, elapsed\)`)
	if !call.Match(src) {
		t.Error("network.go no longer measures the attach once and calls p.noteSlowAttach(r, elapsed) " +
			"at the top of the attach goroutine's `if err == nil` branch.\n" +
			"If the call moved, this guard needs updating; if it was dropped, " +
			"join_attach_slow is back to being a counter nothing can move (#431); " +
			"and if it moved OUT of the success branch, failed attaches are now " +
			"counted as merely slow.")
	}
}

// TestJoin_LogsThePhaseTimingOfASuccessfulAttach pins the only observer
// #403's distribution has.
//
// No unit test in this package can reach a successful Start: it needs a
// live network namespace. So the per-attach timing line is asserted
// where it is written rather than where it is read, and the integration
// lane asserts that the line appears once per attach. Without the line
// the distribution of Join durations is join_attach_slow alone, which
// counts only the attaches that outran the budget and is therefore the
// tail offered as the whole.
func TestJoin_LogsThePhaseTimingOfASuccessfulAttach(t *testing.T) {
	src, err := os.ReadFile("network.go")
	if err != nil {
		t.Fatalf("read network.go: %v", err)
	}
	// ANCHORED ON THE WHOLE BRANCH, not on the two lines separately.
	// The first draft matched `if err == nil {` and the Debug call with
	// a lazy any-character span between them, and a mutant that closed
	// the branch and re-opened the block unconditionally SURVIVED it:
	// the span simply reached across the closing brace. Every line
	// between is therefore required to be inside the branch — three
	// tabs or deeper, or blank — and the branch is required to close
	// immediately after the Debug call.
	line := regexp.MustCompile(`\n\t\tif err == nil \{\n(?:\t{3,}[^\n]*\n|\n)*?\t{4}"phases": +m\.startPhases,\n(?:\t{3,}[^\n]*\n|\n)*?\t{3}\}\)\.Debug\("Attach completed"\)\n\t\t\}\n`)
	if !line.Match(src) {
		t.Error("network.go no longer logs m.startPhases for a successful attach as the last " +
			"statement of the attach goroutine's `if err == nil` branch.\n" +
			"The #403 distribution is read off that line. Without it the only per-attach " +
			"duration a run carries is join_attach_slow, which counts the tail and says " +
			"nothing about where the body of the distribution sits; outside the branch it " +
			"would also describe attaches that failed.")
	}
}

// TestJoin_RecordsTheAttachDurationWhereTheLogCannotBeRead pins the
// counter half of #403 to the same branch and the same number as the
// log line above.
//
// The timing line is Debug and config.json ships LOG_LEVEL=info, so on
// a host nobody has reconfigured the counters are the whole of the
// distribution. Called on the failure path they would count faults as
// successful attaches; called with a second time.Since they would
// describe a different attach from the line beside them.
func TestJoin_RecordsTheAttachDurationWhereTheLogCannotBeRead(t *testing.T) {
	src, err := os.ReadFile("network.go")
	if err != nil {
		t.Fatalf("read network.go: %v", err)
	}
	// Anchored on the whole branch for the reason the guard above
	// carries: a span that may cross the closing brace is satisfied by
	// a call that sits outside it.
	call := regexp.MustCompile(`\n\t\tif err == nil \{\n(?:\t{3,}[^\n]*\n|\n)*?\t{3}p\.noteAttachDuration\(elapsed\)\n(?:\t{3,}[^\n]*\n|\n)*?\t{3}\}\)\.Debug\("Attach completed"\)\n\t\t\}\n`)
	if !call.Match(src) {
		t.Error("network.go no longer calls p.noteAttachDuration(elapsed) inside the attach " +
			"goroutine's `if err == nil` branch.\n" +
			"join_attach_completed and its buckets are the only per-attach duration a host " +
			"running the shipped LOG_LEVEL carries; outside the branch they would count " +
			"failed attaches as successes, and with a freshly measured elapsed they would " +
			"describe a different attach from the timing line (#403).\n" +
			"The ORDER inside the branch is part of this pattern and is load-bearing beyond " +
			"the counter: the lane cross-reads the two records over one window and treats a " +
			"window with more lines than the counter counted as a counter that missed " +
			"attaches, which holds only while the counter is incremented first " +
			"(test/integration/join_duration_test.go).")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// join_attach_completed and its two buckets are the half of #403 a
// shipped plugin can answer. The timing line beside them is Debug and
// config.json ships LOG_LEVEL=info, so on a host nobody has
// reconfigured and restarted, join_attach_slow — the tail — was the
// only per-attach duration in existence. These drive the body.

func TestNoteAttachDuration_PartitionsEveryAttachAcrossTheBuckets(t *testing.T) {
	p := newSlowAttachPlugin(10 * time.Second)
	for _, d := range []time.Duration{
		120 * time.Millisecond, 999 * time.Millisecond, // under 1s
		time.Second, 4 * time.Second, 10 * time.Second, // 1s..budget
		11 * time.Second, // over budget, counted by noteSlowAttach
	} {
		p.noteSlowAttach(slowAttachReq, d)
		p.noteAttachDuration(d)
	}

	completed := p.joinAttachCompleted.Load()
	under, toBudget, slow := p.joinAttachUnder1s.Load(), p.joinAttach1sToBudget.Load(), p.joinAttachSlow.Load()
	if completed != 6 {
		t.Errorf("join_attach_completed = %d after six successful attaches, want 6", completed)
	}
	if under != 2 || toBudget != 3 || slow != 1 {
		t.Errorf("buckets under_1s=%d 1s_to_budget=%d slow=%d, want 2/3/1", under, toBudget, slow)
	}
	// The property the buckets are read through: a reader who sees a
	// zero bucket must be able to tell a missing increment from a lane
	// that attached nothing, and that only works if the three account
	// for the whole population.
	if got := under + toBudget + slow; got != completed {
		t.Errorf("the buckets sum to %d but join_attach_completed is %d; a bucket that is not "+
			"part of a partition cannot bound a percentile", got, completed)
	}
}

func TestNoteAttachDuration_BucketBoundariesSitEitherSideOfTheBudget(t *testing.T) {
	// One second belongs above the first bucket and the budget itself
	// belongs below the tail, so that join_attach_slow keeps counting
	// exactly what noteSlowAttach counts: strictly over.
	p := newSlowAttachPlugin(2 * time.Second)
	p.noteAttachDuration(999999 * time.Microsecond)
	p.noteAttachDuration(time.Second)
	p.noteAttachDuration(2 * time.Second)
	p.noteAttachDuration(2*time.Second + time.Millisecond)

	if under, toBudget := p.joinAttachUnder1s.Load(), p.joinAttach1sToBudget.Load(); under != 1 || toBudget != 2 {
		t.Errorf("under_1s=%d 1s_to_budget=%d across the two boundaries, want 1/2", under, toBudget)
	}
	if got := p.joinAttachCompleted.Load(); got != 4 {
		t.Errorf("join_attach_completed = %d, want 4: an attach outside both buckets is still an attach", got)
	}
}

func TestNoteAttachDuration_MaxKeepsTheWorstAttach(t *testing.T) {
	p := newSlowAttachPlugin(10 * time.Second)
	p.noteAttachDuration(300 * time.Millisecond)
	p.noteAttachDuration(7 * time.Second)
	p.noteAttachDuration(120 * time.Millisecond)

	if got := p.joinAttachMsMax.Load(); got != 7000 {
		t.Errorf("join_attach_ms_max = %d after a 7s attach followed by a fast one, want 7000: "+
			"#403 asks how close a host came to AwaitTimeout, and a maximum a later attach can "+
			"lower answers a different question", got)
	}
}

// TestNoteAttachDuration_PartitionHoldsWhenTheBudgetIsBelowASecond is
// the boundary the partition claim has and did not state.
//
// The first bucket is keyed on a literal second and the tail on
// AwaitTimeout, so the two orderings are only compatible while the
// budget is at least a second. AWAIT_TIMEOUT is settable —
// cmd/net-dhcp/main.go reads it with durationEnv and NewPlugin refuses
// only a non-positive value — so a budget below a second is a
// configuration the shipped manifest permits. At 500ms an attach of
// 700ms was counted twice, once as under a second and once as slow,
// and the three buckets summed to 2 over a population of 1.
//
// The sum is not decoration: it is how a reader tells a missing
// increment from a host that attached nothing.
func TestNoteAttachDuration_PartitionHoldsWhenTheBudgetIsBelowASecond(t *testing.T) {
	p := newSlowAttachPlugin(500 * time.Millisecond)

	// Over the budget and under a second: the case that overlapped.
	p.noteSlowAttach(slowAttachReq, 700*time.Millisecond)
	p.noteAttachDuration(700 * time.Millisecond)
	// Inside the budget, so still the body and still under a second.
	p.noteSlowAttach(slowAttachReq, 300*time.Millisecond)
	p.noteAttachDuration(300 * time.Millisecond)

	completed := p.joinAttachCompleted.Load()
	under, toBudget, slow := p.joinAttachUnder1s.Load(), p.joinAttach1sToBudget.Load(), p.joinAttachSlow.Load()
	if got := under + toBudget + slow; got != completed {
		t.Errorf("with a %s budget the buckets sum to %d over %d attaches (under_1s=%d "+
			"1s_to_budget=%d slow=%d). An attach counted in two buckets breaks the one "+
			"property a reader uses to tell a missing increment from a quiet host.",
			p.awaitTimeout, got, completed, under, toBudget, slow)
	}
	if under != 1 || slow != 1 {
		t.Errorf("under_1s=%d slow=%d, want 1/1: the 300ms attach is the body and the 700ms "+
			"attach outran the budget, so the tail owns it whatever the literal second says",
			under, slow)
	}
	if toBudget != 0 {
		t.Errorf("1s_to_budget=%d, want 0: no attach can be at least a second and inside a %s "+
			"budget, so the bucket is empty by construction here", toBudget, p.awaitTimeout)
	}
}
