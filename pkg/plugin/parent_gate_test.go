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
			guard := p.lockParent(context.Background(), "eth0", "test")
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
			guard := p.lockParent(context.Background(), parent, "test")
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
	holder, ok := p.parentGate.acquire(context.Background(), "eth0", time.Second)
	if !ok {
		t.Fatal("could not take an uncontended gate")
	}
	defer holder()

	start := time.Now()
	unlock, got := p.parentGate.acquire(context.Background(), "eth0", 50*time.Millisecond)
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

	holder, ok := p.parentGate.acquire(context.Background(), "eth0", time.Second)
	if !ok {
		t.Fatal("could not take an uncontended gate")
	}
	defer holder()

	// Cancel immediately: lockParent uses the full parentGateBudget, and
	// waiting 4s in a unit test to observe a counter is not worth it. The
	// ctx path and the timer path land on the same branch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.lockParent(ctx, "eth0", "test").Unlock()

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
		p.lockParent(context.Background(), "eth0", "test").Unlock()
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
			p.lockParent(context.Background(), "", "test").Unlock()
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
			g := tc.p.lockParent(tc.ctx, tc.parent, "test")
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

	first := p.lockParent(context.Background(), "eth0", "test")
	first.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.lockParent(context.Background(), "eth0", "test").Unlock()
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
