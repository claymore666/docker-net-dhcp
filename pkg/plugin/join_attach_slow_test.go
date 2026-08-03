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
	// opening a block whose body calls noteSlowAttach with the measured
	// elapsed time.
	call := regexp.MustCompile(`if err == nil \{\s*\n\s*p\.noteSlowAttach\(r, time\.Since\(attachStart\)\)\s*\n\s*\}`)
	if !call.Match(src) {
		t.Error("network.go no longer calls p.noteSlowAttach(r, time.Since(attachStart)) " +
			"directly inside the attach goroutine's `if err == nil` branch.\n" +
			"If the call moved, this guard needs updating; if it was dropped, " +
			"join_attach_slow is back to being a counter nothing can move (#431); " +
			"and if it moved OUT of the success branch, failed attaches are now " +
			"counted as merely slow.")
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
