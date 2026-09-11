// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// parentGateBudget bounds how long an operation will wait for another
// one to finish with the same parent NIC before giving up and trying
// anyway.
//
// Deliberately below the longest an operation can hold a parent, which
// is now the validate_dhcp probe's DHCP round trip. Waiting out the
// worst case would stall the container-start path long enough for
// Docker's own CreateEndpoint timeout to fire first — so the gate is
// built to absorb the common case (a DORA, ~2-3s observed) and to
// degrade rather than block when it cannot.
//
// The budget used to be sized against the orphaned-lease reclaim's 20s
// ceiling, which was the demanding holder until that mechanism was
// removed in v1.9.0 (#800).
const parentGateBudget = 4 * time.Second

// parentGate serialises the operations that add a child link to a
// parent NIC, one queue per parent.
//
// The constraint it exists for is the kernel's, not ours: a parent NIC
// is a macvlan port or an ipvlan port, never both, because both kinds
// claim the same single rx_handler on the parent netdev. The second
// kind to ask gets EBUSY. Children of the SAME kind coexist happily, so
// this only bites where two networks of different modes share a parent
// — where the plugin's own validate_dhcp probe can be holding one in
// one mode at the moment an endpoint asks for the other.
//
// The original offender (#370) was worse and is gone: the orphaned-lease
// reclaim created children asynchronously, from a goroutine ordered
// against no Docker request, and its temporary link outlived the
// endpoint it belonged to. That mechanism was removed in v1.9.0 (#800).
// The remaining collision is narrower but real, and the gate is what
// keeps it from surfacing as an EBUSY on an unrelated container start.
//
// Per parent rather than one global lock, and that distinction is the
// whole design. A global lock would serialise every endpoint creation
// on the host, including the overwhelmingly common case of unrelated
// networks on unrelated NICs — a far worse trade than the bug it fixes.
//
// LOCK ORDERING. Two locks live here and they are not peers:
//
//   - mu is a leaf. It is held only for the map lookup in tokenFor, never
//     across IO, and nothing else may be acquired while it is held.
//   - the per-parent token (the channel) IS held across blocking IO —
//     that is its job. It must therefore never be taken while holding
//     Plugin.mu, or a slow probe would block every registry read on the
//     plugin. Every current caller takes it with no plugin lock held;
//     keep it that way.
//
// The gate is advisory. Failing to get it is not an error: the caller
// proceeds and the kernel remains the authority, exactly as it was
// before this existed. That keeps a wedged or slow holder from turning
// into a hung container start — the worst case degrades to the EBUSY
// the caller would have got anyway, with a counter to say so.
type parentGate struct {
	mu     sync.Mutex
	tokens map[string]chan struct{}
	// holders is the KIND of child the current holder of each parent is
	// about to attach -- ModeMacvlan or ModeIPvlan -- and it exists for
	// reporting, not for exclusion. The kernel refuses only the CROSS
	// pair; two macvlan children on one parent are legal and common.
	// Without this the gate cannot tell a wait it was right to make
	// from one that protected nothing, and both arrive at the operator
	// as the same health warning.
	holders map[string]string
	// waiters are the callers currently waiting for each parent, and
	// they are the half a point reading cannot replace. The question a
	// give-up asks is about an INTERVAL -- did anything attaching the
	// other kind hold this parent while I waited -- and sampling the
	// holder at each end answers it only if nothing changed twice in
	// between. So the waiter registers itself instead, and every take
	// of a different kind marks it while it waits.
	waiters map[string]map[*parentWaiter]struct{}
}

// parentWaiter is one caller waiting for one parent.
//
// foreign is the whole verdict, and it is WRITE-ONCE-TRUE: set at
// registration if the parent was already held by anything other than
// this caller's kind, and set by any later take of another kind. Once
// set it stays set, so a cross-kind holder that came and went during
// the wait is still visible at the give-up -- the child it attached
// outlives it on the parent, and the kernel can refuse this caller's
// LinkAdd long after that holder is gone.
//
// Keeping it as a flag rather than as a holder sampled at each end is
// what removes the interval this fix is about: there is no second
// reading to be taken at the wrong moment, because there is no second
// reading at all.
type parentWaiter struct {
	kind    string
	foreign bool
}

// tokenLocked returns the queue for one parent, creating it on first
// use. The caller holds g.mu.
//
// Entries are never removed. A host has a handful of NICs and the map
// is keyed by interface name, so it is bounded by the machine rather
// than by traffic; reclaiming entries would need a refcount whose only
// purpose is to free a few dozen bytes.
func (g *parentGate) tokenLocked(parent string) chan struct{} {
	if g.tokens == nil {
		g.tokens = make(map[string]chan struct{})
	}
	tok, ok := g.tokens[parent]
	if !ok {
		tok = make(chan struct{}, 1)
		g.tokens[parent] = tok
	}
	return tok
}

// enterWait registers a caller against one parent AND makes its first
// attempt on the queue, both inside ONE acquisition of the lock.
//
// The two are fused on purpose. Reading the holder after the queue has
// already been tried leaves a window -- between the failed attempt and
// the read -- in which a cross-kind holder can release to a same-kind
// one, and the caller then sees nothing but its own kind at both ends
// of a wait whose LinkAdd the kernel may still refuse. A gap that
// narrow cannot be driven from a test, so it is closed by construction
// instead: there is no separate attempt to move, and reopening the
// window means taking this lock twice.
//
// The non-blocking send is safe under the lock for the reason every
// other use of g.mu is: it cannot block, so mu stays a leaf held across
// no IO.
//
// Returns the queue, the registration, and whether the attempt won.
func (g *parentGate) enterWait(parent, kind string) (chan struct{}, *parentWaiter, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tok := g.tokenLocked(parent)
	// The holder at this instant counts exactly like a take during the
	// wait, including "nothing holds it" and "a holder that named no
	// kind", both of which spell an empty string and neither of which
	// is evidence that nothing can conflict.
	w := &parentWaiter{kind: kind, foreign: g.holders[parent] != kind}
	if g.waiters == nil {
		g.waiters = make(map[string]map[*parentWaiter]struct{})
	}
	if g.waiters[parent] == nil {
		g.waiters[parent] = make(map[*parentWaiter]struct{})
	}
	g.waiters[parent][w] = struct{}{}
	select {
	case tok <- struct{}{}:
		g.noteTakeLocked(parent, kind)
		return tok, w, true
	default:
	}
	return tok, w, false
}

// leaveWait deregisters a caller. Always called, waited or not.
func (g *parentGate) leaveWait(parent string, w *parentWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.waiters[parent], w)
	if len(g.waiters[parent]) == 0 {
		delete(g.waiters, parent)
	}
}

// noteTake records a new holder and marks every waiter it could
// conflict with.
//
// A take of an UNIDENTIFIED kind marks everyone, for the reason an
// unidentified holder keeps the warning: no evidence about a holder is
// not evidence that it cannot conflict.
func (g *parentGate) noteTake(parent, kind string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.noteTakeLocked(parent, kind)
}

// noteTakeLocked is noteTake's body, for the take that happens inside
// enterWait's critical section. The caller holds g.mu.
func (g *parentGate) noteTakeLocked(parent, kind string) {
	if g.holders == nil {
		g.holders = make(map[string]string)
	}
	if kind == "" {
		delete(g.holders, parent)
	} else {
		g.holders[parent] = kind
	}
	for w := range g.waiters[parent] {
		if w.kind != kind {
			w.foreign = true
		}
	}
}

// clearHolder forgets the holder of one parent. It is a RELEASE and
// marks nobody: a holder going away conflicts with no one.
func (g *parentGate) clearHolder(parent string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.holders, parent)
}

// holderKind reports the kind the current holder is attaching, or "" if
// nothing holds this parent right now.
func (g *parentGate) holderKind(parent string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holders[parent]
}

// heldThroughout answers the only question the give-up path may act on:
// was everything that held this parent during the wait attaching the
// same kind of child as the caller?
//
// It reads the registration and nothing else. Anything but "my own kind
// from the moment I registered to now" answers "" -- nothing was
// holding it, a holder could not be identified, or something of the
// other kind held it at any point -- because the conservative reading
// of incomplete evidence is the one that keeps the health warning.
func (g *parentGate) heldThroughout(w *parentWaiter) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if w.foreign {
		return ""
	}
	// A caller that named no kind answers with its own empty string,
	// which the give-up path already reads as no evidence.
	return w.kind
}

// acquire takes the gate for one parent, waiting up to budget.
//
// kind is the child this caller is about to attach (ModeMacvlan or
// ModeIPvlan). It changes nothing about who waits for whom -- the gate
// stays a plain mutual exclusion -- and is recorded so that a caller
// which GAVE UP waiting can find out whether anything that held the
// parent while it waited could ever have conflicted with it.
//
// Returns a release func that is ALWAYS safe to call — on the timeout
// path it is a no-op, so callers can defer it unconditionally without
// caring whether the wait succeeded. The bool reports whether the gate
// was actually held, which is what the counters key on. The string is
// the kind that held the parent throughout, empty where that could not
// be established, and it is meaningful only when the bool is false.
func (g *parentGate) acquire(ctx context.Context, parent, kind string, budget time.Duration) (func(), bool, string) {
	if parent == "" {
		return func() {}, false, ""
	}

	// One critical section registers this caller and makes its first
	// attempt, which is what leaves no interval between the two. The
	// uncontended path -- nearly all of them -- ends here, with no
	// timer and no wait.
	tok, w, took := g.enterWait(parent, kind)
	defer g.leaveWait(parent, w)
	if took {
		return g.release(parent, tok), true, ""
	}

	return g.waitForParent(ctx, parent, kind, budget, tok, w)
}

// take marks the gate held and returns its release. enterWait does its
// own marking, under the lock it already holds, and calls release
// directly.
func (g *parentGate) take(parent, kind string, tok chan struct{}) func() {
	g.noteTake(parent, kind)
	return g.release(parent, tok)
}

// release is what a holder calls to give the parent back.
func (g *parentGate) release(parent string, tok chan struct{}) func() {
	return func() {
		g.clearHolder(parent)
		<-tok
	}
}

// waitForParent is acquire's contended half: wait out the budget, and
// on giving up report which kind -- if it can be established -- held
// the parent for the whole wait.
//
// w is the caller's registration. It is a parameter and not a fresh
// read, so this half cannot answer on a later reading than the one its
// caller started from.
func (g *parentGate) waitForParent(ctx context.Context, parent, kind string, budget time.Duration, tok chan struct{}, w *parentWaiter) (func(), bool, string) {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case tok <- struct{}{}:
		return g.take(parent, kind, tok), true, ""
	case <-ctx.Done():
		return func() {}, false, g.heldThroughout(w)
	case <-timer.C:
		return func() {}, false, g.heldThroughout(w)
	}
}

// parentGuard is evidence that the gate for one parent NIC is held.
//
// It exists to move the rule out of prose and into the type system. The
// rule — a child link may only be attached to a parent NIC while the
// gate for that parent is held — used to live in a comment beside each
// call site, which means it was enforced by whoever last read the
// comment. Two of the three sites take the gate several frames above
// the LinkAdd, so checking it meant following callers upwards and
// trusting that nobody had added a fourth path.
//
// The accounting gate (.github/linkadd-accounting.txt) catches a new
// netlink.LinkAdd appearing anywhere in pkg/. This catches the other
// half: a child link created on a path that never went through
// lockParent. addChildLink cannot be called without one of these, and
// that much IS the compiler's doing.
//
// WHERE THE COMPILER STOPS. "Only lockParent makes one" is not
// something Go can express. This struct's zero value is valid, so
//
//	addChildLink(&parentGuard{}, link)
//
// compiles and holds nothing — and lockParent returns exactly that
// literal on the no-parent path below, so the shape is already in this
// file as a pattern to copy. The realistic route to it is not malice:
// a new parent-attached call site, a compiler demanding a guard, and
// the zero value sitting right there.
//
// So that half is enforced by scripts/check-parent-gate-accounting.sh,
// which fails the build on a parentGuard built anywhere but here. Two
// mechanisms, and the split is deliberate — claiming the compiler does
// both would be a prose guarantee about a property nothing checks,
// which is the exact thing this type was introduced to replace.
//
// WHAT IT DOES NOT PROVE. The guard does not say WHICH parent it is
// for, so a guard taken on one NIC and handed to a link on another
// compiles. Closing that would mean carrying the parent name and
// comparing it inside addChildLink — a runtime check, trading a compile
// error for a log line, on a mistake no call site can currently make:
// each takes its guard a line or two before using it, for the parent it
// is about to touch. Written down rather than left to be assumed, for
// the same reason the accounting file states its own limit.
type parentGuard struct {
	release func()
}

// Unlock releases the gate. Always safe to call, including on a guard
// whose wait timed out and which therefore never held anything.
func (g *parentGuard) Unlock() {
	if g == nil || g.release == nil {
		return
	}
	g.release()
}

// addChildLink creates a link attached to a parent NIC.
//
// The guard parameter is the whole point of this function. It is
// unused, and it is what makes "the gate is held here" something the
// compiler checks instead of something the next reader has to establish
// by following calls up several frames.
//
// Links with no parent do NOT belong here and deliberately still call
// netlink.LinkAdd directly: a bridge network has no parent NIC, nothing
// registers an rx_handler, and routing it through a function that
// demands a guard would ask for a lock on nothing.
func addChildLink(_ *parentGuard, link netlink.Link) error {
	return netlink.LinkAdd(link)
}

// lockParent gates one operation on a parent NIC and records the
// outcome on the health counters.
//
// op names the caller in the log line only; the counters are
// deliberately not split by op, because what an operator needs to know
// is whether this host is contending on a parent at all.
//
// Never returns nil, so a caller can always defer Unlock.
func (p *Plugin) lockParent(ctx context.Context, parent, kind, op string) *parentGuard {
	if p == nil || parent == "" {
		return &parentGuard{}
	}

	start := time.Now()
	release, ok, heldKind := p.parentGate.acquire(ctx, parent, kind, parentGateBudget)
	waited := time.Since(start)

	switch {
	case ok && waited < parentGateContendedFloor:
		// Uncontended. The overwhelmingly common case, and it must stay
		// silent on both the log and the counters or the signal below
		// is worthless.
	case ok:
		p.parentLinkWaits.Add(1)
		log.WithFields(log.Fields{
			"parent": parent,
			"op":     op,
			"waited": waited.String(),
		}).Debug("Waited for another operation to finish with the parent interface")
	case heldKind != "" && heldKind == kind:
		// GAVE UP, AND NOTHING WAS AT STAKE. The gate excludes more
		// than the kernel does: a parent registers one rx_handler, so
		// it refuses the CROSS pair, and children of the same kind
		// coexist. Losing a wait to a holder of one's own kind
		// therefore costs the budget and protects nothing, and the
		// caller proceeds to a LinkAdd the kernel will accept.
		//
		// It is counted as a wait rather than as a timeout because the
		// timeout counter is a health warning whose whole meaning is
		// "a start on this NIC may have been refused". Two containers
		// starting together on one macvlan network -- which is what
		// `docker compose up` is -- land here every time once an
		// address reservation holds a parent across its DHCP exchange,
		// and reporting that as a warning would teach an operator to
		// ignore the counter that names the real collision.
		p.parentLinkWaits.Add(1)
		log.WithFields(log.Fields{
			"parent": parent,
			"op":     op,
			"kind":   kind,
			"budget": parentGateBudget.String(),
		}).Debug("Gave up waiting for the parent interface; the holder is attaching the same kind of child, which the kernel permits alongside this one")
	default:
		p.parentLinkWaitTimeouts.Add(1)
		log.WithFields(log.Fields{
			"parent":      parent,
			"op":          op,
			"kind":        kind,
			"holder_kind": heldKind,
			"budget":      parentGateBudget.String(),
		}).Warn("Gave up waiting for the parent interface; proceeding, the kernel may refuse this")
	}
	return &parentGuard{release: release}
}

// parentGateContendedFloor is the wait below which an acquisition is
// treated as uncontended. A successful non-blocking take still spends a
// few microseconds getting there, and counting those would make
// parent_link_waits climb on every endpoint creation on an idle host.
const parentGateContendedFloor = time.Millisecond
