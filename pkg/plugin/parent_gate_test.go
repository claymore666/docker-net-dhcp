// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParentGate_SerialisesOneParent is the property the whole change
// exists for, expressed without netlink: two operations on the same
// parent never overlap.
//
// Deliberately asserts on observed concurrency rather than on ordering.
// Which one wins the race is not a promise the gate makes; that only one
// is inside at a time is.
//
// Remove the gate — call the body directly instead of through
// lockParent — and this fails: the goroutines are started together and
// the body holds its "inside" state long enough that overlap is certain,
// not probabilistic.
func TestParentGate_SerialisesOneParent(t *testing.T) {
	p := &Plugin{}

	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			guard := p.lockParent(context.Background(), "eth0", ModeMacvlan, "test")
			defer guard.Unlock()

			n := inside.Add(1)
			for {
				old := maxInside.Load()
				if n <= old || maxInside.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inside.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxInside.Load(); got != 1 {
		t.Fatalf("observed %d concurrent operations on one parent, want 1 — the gate is not serialising", got)
	}
	if p.parentLinkWaitTimeouts.Load() != 0 {
		t.Fatalf("parent_link_wait_timeouts = %d, want 0 — nothing here holds the parent anywhere near the budget",
			p.parentLinkWaitTimeouts.Load())
	}
}

// TestParentGate_DifferentParentsDoNotSerialise is requirement 1 of the
// design: per parent, not global. A global lock would pass the test
// above and fail this one.
//
// Each goroutine blocks until every other has arrived. If the gate
// serialised across parents they could not all arrive, and this
// deadlocks into its timeout rather than failing an assertion — so the
// barrier carries its own deadline.
func TestParentGate_DifferentParentsDoNotSerialise(t *testing.T) {
	p := &Plugin{}

	const n = 4
	arrived := make(chan struct{}, n)
	released := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		parent := "eth" + string(rune('0'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			guard := p.lockParent(context.Background(), parent, ModeMacvlan, "test")
			defer guard.Unlock()
			arrived <- struct{}{}
			<-released
		}()
	}

	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-arrived:
		case <-deadline:
			close(released)
			wg.Wait()
			t.Fatalf("only %d of %d parents could be held at once — the gate is global, not per-parent", i, n)
		}
	}
	close(released)
	wg.Wait()
}

// TestParentGate_BudgetExpiryCountsAndProceeds pins the degrade path.
//
// A caller that cannot get the gate must proceed anyway: blocking a
// container start behind a wedged reclaim is worse than the EBUSY it
// replaces. The counter is what tells an operator that happened.
func TestParentGate_BudgetExpiryCountsAndProceeds(t *testing.T) {
	p := &Plugin{}

	// Take the gate directly and hold it, standing in for a reclaim that
	// is not going to finish.
	holder, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeIPvlan, time.Second)
	if !ok {
		t.Fatal("could not take an uncontended gate")
	}
	defer holder()

	start := time.Now()
	unlock, got, _ := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, 50*time.Millisecond)
	waited := time.Since(start)
	unlock() // must be safe on the timeout path

	if got {
		t.Fatal("acquired a gate that was already held")
	}
	if waited < 50*time.Millisecond {
		t.Fatalf("gave up after %v, want at least the 50ms budget", waited)
	}
	if waited > time.Second {
		t.Fatalf("waited %v, far past the 50ms budget — the timer is not bounding the wait", waited)
	}
}

// TestLockParent_TimeoutIsCounted checks the counter wiring on the path
// an operator actually reads, which the test above deliberately bypasses
// by calling acquire directly.
func TestLockParent_TimeoutIsCounted(t *testing.T) {
	p := &Plugin{}

	holder, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeIPvlan, time.Second)
	if !ok {
		t.Fatal("could not take an uncontended gate")
	}
	defer holder()

	// Cancel immediately: lockParent uses the full parentGateBudget, and
	// waiting 4s in a unit test to observe a counter is not worth it. The
	// ctx path and the timer path land on the same branch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.lockParent(ctx, "eth0", ModeMacvlan, "test").Unlock()

	if p.parentLinkWaitTimeouts.Load() != 1 {
		t.Fatalf("parent_link_wait_timeouts = %d, want 1", p.parentLinkWaitTimeouts.Load())
	}
	if p.parentLinkWaits.Load() != 0 {
		t.Fatalf("parent_link_waits = %d, want 0 — a give-up is not a wait", p.parentLinkWaits.Load())
	}
}

// TestLockParent_UncontendedIsSilent guards the counters' signal value.
// If an ordinary endpoint creation on an idle host bumped
// parent_link_waits, the counter would climb forever and mean nothing.
func TestLockParent_UncontendedIsSilent(t *testing.T) {
	p := &Plugin{}

	for i := 0; i < 20; i++ {
		p.lockParent(context.Background(), "eth0", ModeMacvlan, "test").Unlock()
	}

	if got := p.parentLinkWaits.Load(); got != 0 {
		t.Fatalf("parent_link_waits = %d after 20 uncontended acquisitions, want 0", got)
	}
	if got := p.parentLinkWaitTimeouts.Load(); got != 0 {
		t.Fatalf("parent_link_wait_timeouts = %d, want 0", got)
	}
}

// TestLockParent_NoParentIsANoOp covers bridge-mode networks, which have
// no parent NIC at all. They must not queue behind anything, and must
// not register as contention.
func TestLockParent_NoParentIsANoOp(t *testing.T) {
	p := &Plugin{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			p.lockParent(context.Background(), "", ModeMacvlan, "test").Unlock()
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an empty parent blocked; bridge networks must not queue")
	}

	if p.parentLinkWaits.Load() != 0 || p.parentLinkWaitTimeouts.Load() != 0 {
		t.Fatalf("an empty parent touched the counters: waits=%d timeouts=%d",
			p.parentLinkWaits.Load(), p.parentLinkWaitTimeouts.Load())
	}
}

// TestLockParent_GuardIsAlwaysUsable covers the contract the guard type
// depends on: lockParent never returns nil, and Unlock is safe whatever
// happened during the acquisition.
//
// Both matter because every caller defers Unlock unconditionally. The
// no-parent and nil-Plugin paths return a guard that holds nothing, and
// the timeout path returns one whose wait failed; if any of those came
// back nil or panicked on release, the deferred Unlock would take down
// the plugin on a path that is supposed to degrade quietly.
func TestLockParent_GuardIsAlwaysUsable(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name   string
		p      *Plugin
		ctx    context.Context
		parent string
	}{
		{"a real acquisition", &Plugin{}, context.Background(), "eth0"},
		{"no parent (bridge mode)", &Plugin{}, context.Background(), ""},
		{"a nil plugin", nil, context.Background(), "eth0"},
		{"an acquisition that never succeeded", &Plugin{}, cancelled, "eth0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.p.lockParent(tc.ctx, tc.parent, ModeMacvlan, "test")
			if g == nil {
				t.Fatal("lockParent returned nil; every caller defers Unlock on the result")
			}
			g.Unlock()
		})
	}

	// A zero guard is what a future caller would get by declaring one
	// rather than calling lockParent. It must not panic either — the
	// type is a compile-time requirement, not a runtime trap.
	var zero parentGuard
	zero.Unlock()
	var nilGuard *parentGuard
	nilGuard.Unlock()
}

// TestLockParent_GuardIsReleasedNotJustDiscarded proves Unlock actually
// hands the parent on. A guard whose release was dropped would compile,
// satisfy every type check, and deadlock the next endpoint on that NIC
// for the full budget — so the type carrying the release is not on its
// own evidence that the release happens.
func TestLockParent_GuardIsReleasedNotJustDiscarded(t *testing.T) {
	p := &Plugin{}

	first := p.lockParent(context.Background(), "eth0", ModeMacvlan, "test")
	first.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.lockParent(context.Background(), "eth0", ModeMacvlan, "test").Unlock()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second acquisition blocked after the first was unlocked; " +
			"Unlock is not releasing the parent")
	}

	if got := p.parentLinkWaits.Load(); got != 0 {
		t.Fatalf("parent_link_waits = %d, want 0 — the second acquisition should not "+
			"have had to wait at all, so the first was still holding the parent", got)
	}
}

// TestLockParent_ASameKindHolderIsNotAHealthWarning.
//
// The gate excludes more than the kernel does. A parent NIC registers
// one rx_handler, so it refuses a macvlan child beside an ipvlan one
// and permits any number of the same kind. The gate is one mutex per
// parent and knows none of that, so a caller that gives up waiting for
// a holder of its OWN kind has lost the budget and protected nothing.
//
// It stayed invisible while every holder was brief. An address
// reservation holds the parent across a whole DHCP exchange, so two
// containers starting together on one macvlan network -- `docker
// compose up` -- reach the give-up branch every time. Reported as
// parent_link_wait_timeouts that is a health warning whose action text
// says container starts were refused, and nothing was refused: the
// second start proceeds and the kernel accepts it.
//
// The cross-kind arm is what keeps this from being a way to silence the
// counter. That is the collision the gate exists for, and it must still
// warn.
func TestLockParent_ASameKindHolderIsNotAHealthWarning(t *testing.T) {
	// Cancelled before the wait, so the give-up branch is reached
	// without spending parentGateBudget in a unit test. acquire treats
	// the cancellation and the timer as one branch.
	giveUp := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	t.Run("the same kind is an ordinary wait", func(t *testing.T) {
		p := &Plugin{}
		holder := p.lockParent(context.Background(), "eth0", ModeMacvlan, "ipam_reserve")
		defer holder.Unlock()

		p.lockParent(giveUp(), "eth0", ModeMacvlan, "create_endpoint").Unlock()

		if got := p.parentLinkWaitTimeouts.Load(); got != 0 {
			t.Errorf("parent_link_wait_timeouts = %d, want 0. Both callers attach macvlan "+
				"children, which the kernel permits on one parent, so the caller that gave "+
				"up will succeed. A health warning here is one an operator can do nothing "+
				"about, and it fires on every concurrent start on the network.", got)
		}
		if got := p.parentLinkWaits.Load(); got != 1 {
			t.Errorf("parent_link_waits = %d, want 1. The wait still happened and still cost "+
				"the budget; making it silent as well would hide the contention entirely.", got)
		}
	})

	t.Run("the other kind still warns", func(t *testing.T) {
		p := &Plugin{}
		holder := p.lockParent(context.Background(), "eth0", ModeIPvlan, "preflight_probe")
		defer holder.Unlock()

		p.lockParent(giveUp(), "eth0", ModeMacvlan, "create_endpoint").Unlock()

		if got := p.parentLinkWaitTimeouts.Load(); got != 1 {
			t.Errorf("parent_link_wait_timeouts = %d, want 1. A macvlan child added while an "+
				"ipvlan child is being attached to the same parent is the one pair the "+
				"kernel refuses, and it is the whole reason this gate exists.", got)
		}
	})

	t.Run("an unknown holder still warns", func(t *testing.T) {
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", "", time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		defer release()

		p.lockParent(giveUp(), "eth0", ModeMacvlan, "create_endpoint").Unlock()

		if got := p.parentLinkWaitTimeouts.Load(); got != 1 {
			t.Errorf("parent_link_wait_timeouts = %d, want 1. No evidence about the holder is "+
				"not evidence that it cannot conflict, and a give-up branch that read it the "+
				"other way would answer \"harmless\" to every case it could not identify.", got)
		}
	})
}

// TestParentGate_AHolderSwapIsNotEvidenceOfSafety is the INTERVAL the
// same-kind branch is really about.
//
// The branch acts on "nothing that held this parent while I waited
// could ever have conflicted with me", and that is a statement about a
// stretch of time, not about a moment. Sampling the holder at each end
// of the wait answers it only while nothing changed twice in between: a
// cross-kind holder that releases to a same-kind one reads as the
// caller's own kind at both ends, and the child it attached is still on
// the parent, which is precisely where the kernel refuses. So the
// caller registers itself before its first attempt and every take of
// another kind marks it, and these are the arms of that rule.
func TestParentGate_AHolderSwapIsNotEvidenceOfSafety(t *testing.T) {
	t.Run("one holder of my own kind, start to finish", func(t *testing.T) {
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		defer release()

		_, w, _ := p.parentGate.enterWait("eth0", ModeMacvlan)
		defer p.parentGate.leaveWait("eth0", w)
		if got := p.parentGate.heldThroughout(w); got != ModeMacvlan {
			t.Errorf("heldThroughout = %q for a holder of my own kind that never changed, want "+
				"%q. This is the ordinary case the branch exists for, and if it cannot be "+
				"identified the branch is unreachable and the change is a no-op.", got, ModeMacvlan)
		}
	})

	t.Run("the other kind held it when I arrived", func(t *testing.T) {
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeIPvlan, time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		defer release()

		_, w, _ := p.parentGate.enterWait("eth0", ModeMacvlan)
		defer p.parentGate.leaveWait("eth0", w)
		if got := p.parentGate.heldThroughout(w); got != "" {
			t.Errorf("heldThroughout = %q against an ipvlan holder, want empty. That is the one "+
				"pair the kernel refuses and the whole reason the gate exists.", got)
		}
	})

	t.Run("the other kind took it during the wait", func(t *testing.T) {
		// The swap, staged as the waiter experiences it: the holder
		// changes while the waiter is registered and still waiting. It
		// cannot be staged through the token, since a waiter blocked on
		// it would take it the moment it came free.
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		_, w, _ := p.parentGate.enterWait("eth0", ModeMacvlan)
		defer p.parentGate.leaveWait("eth0", w)

		release()
		swap, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeIPvlan, time.Second)
		if !ok {
			t.Fatal("could not take the gate after the release")
		}
		defer swap()

		if got := p.parentGate.heldThroughout(w); got != "" {
			t.Errorf("heldThroughout = %q, want empty. A macvlan caller that started waiting "+
				"behind a macvlan holder, and had an ipvlan one take the parent under it, is "+
				"exactly the caller whose LinkAdd the kernel may refuse: the ipvlan child stays "+
				"on the parent after its holder releases. Answering with the kind it started "+
				"beside drops the warning on the one path that earns it.", got)
		}
	})

	t.Run("the other kind released to my own kind under me", func(t *testing.T) {
		// The swap the reviewer named, in its worst order: the caller
		// arrives behind the other kind, that holder LEAVES, and a
		// holder of the caller's own kind takes the parent in its
		// place. Every reading taken after that moment says "same kind,
		// harmless", and the ipvlan child is still on the parent.
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeIPvlan, time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		_, w, _ := p.parentGate.enterWait("eth0", ModeMacvlan)
		defer p.parentGate.leaveWait("eth0", w)

		release()
		swap, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, time.Second)
		if !ok {
			t.Fatal("could not take the gate after the release")
		}
		defer swap()

		if got := p.parentGate.heldThroughout(w); got != "" {
			t.Errorf("heldThroughout = %q after the ipvlan holder left and a macvlan one took "+
				"the parent, want empty. A release conflicts with nobody, but it also clears "+
				"nothing: the child the departed holder attached stays on the parent, and it "+
				"is the reason the kernel can still refuse this caller.", got)
		}
	})

	t.Run("an unidentified holder took it during the wait", func(t *testing.T) {
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		_, w, _ := p.parentGate.enterWait("eth0", ModeMacvlan)
		defer p.parentGate.leaveWait("eth0", w)

		release()
		unknown, ok, _ := p.parentGate.acquire(context.Background(), "eth0", "", time.Second)
		if !ok {
			t.Fatal("could not take the gate after the release")
		}
		defer unknown()

		if got := p.parentGate.heldThroughout(w); got != "" {
			t.Errorf("heldThroughout = %q after a holder that named no kind, want empty. No "+
				"evidence about a holder is not evidence that it cannot conflict, and the "+
				"give-up path reads this the same way it reads an unknown holder at the start.", got)
		}
	})

	t.Run("end to end through acquire, both verdicts", func(t *testing.T) {
		// The arms above reach heldThroughout directly. This one drives
		// the entry point, so the registration that decides the verdict
		// is the one acquire makes for itself, at the instant it first
		// tries the queue -- the two are one critical section, and a
		// budget this short leaves no room for a reading taken later.
		for _, tc := range []struct {
			name   string
			holder string
			want   string
		}{
			{"a holder of my own kind is named", ModeMacvlan, ModeMacvlan},
			{"a holder of the other kind is not", ModeIPvlan, ""},
			{"a holder that named no kind is not", "", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := &Plugin{}
				release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", tc.holder, time.Second)
				if !ok {
					t.Fatal("could not take an uncontended gate")
				}
				defer release()

				_, ok, kind := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, time.Millisecond)
				if ok {
					t.Fatal("the second acquire took a gate that was already held")
				}
				if kind != tc.want {
					t.Errorf("acquire gave up reporting %q, want %q. This is the string "+
						"lockParent switches on, and the wrong one either drops the health "+
						"warning on a collision the kernel will refuse or raises it on a pair "+
						"the kernel permits.", kind, tc.want)
				}
			})
		}
	})

	t.Run("the give-up path answers on the registration, not a fresh read", func(t *testing.T) {
		// End to end through waitForParent, with the parent held by the
		// other kind and the caller's context already cancelled.
		p := &Plugin{}
		release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeIPvlan, time.Second)
		if !ok {
			t.Fatal("could not take an uncontended gate")
		}
		defer release()

		tok, w, _ := p.parentGate.enterWait("eth0", ModeMacvlan)
		defer p.parentGate.leaveWait("eth0", w)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, got, kind := p.parentGate.waitForParent(ctx, "eth0", ModeMacvlan, time.Second, tok, w)
		if got {
			t.Fatal("the wait succeeded while the parent was held")
		}
		if kind != "" {
			t.Errorf("the give-up path answered %q, want empty: it answered on a reading of its "+
				"own rather than on what its caller registered.", kind)
		}
	})
}

// TestParentGate_TheHolderKindIsCleared. The record of who holds a
// parent is a map entry written on acquire, and an entry left behind by
// a release would make the NEXT waiter compare itself against a holder
// that has been gone for hours -- which reads as "same kind, harmless"
// for every caller of the kind that last ran.
func TestParentGate_TheHolderKindIsCleared(t *testing.T) {
	p := &Plugin{}
	release, ok, _ := p.parentGate.acquire(context.Background(), "eth0", ModeMacvlan, time.Second)
	if !ok {
		t.Fatal("could not take an uncontended gate")
	}
	if got := p.parentGate.holderKind("eth0"); got != ModeMacvlan {
		t.Fatalf("holderKind = %q while the gate is held, want %q", got, ModeMacvlan)
	}
	release()
	if got := p.parentGate.holderKind("eth0"); got != "" {
		t.Errorf("holderKind = %q after the release, want empty. A stale entry makes every "+
			"later give-up by a caller of this kind read as harmless.", got)
	}
}
