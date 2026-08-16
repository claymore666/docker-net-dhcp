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
// Deliberately far below orphanReleaseBudget (20s), which is the
// longest a reclaim can hold a parent. Waiting out the worst case would
// put a 20-second stall on the container-start path, and Docker's own
// CreateEndpoint timeout would fire first — so the gate is built to
// absorb the common case (a reclaim's DORA, ~2-3s observed) and to
// degrade rather than block when it cannot.
const parentGateBudget = 4 * time.Second

// parentGate serialises the operations that add a child link to a
// parent NIC, one queue per parent.
//
// The constraint it exists for is the kernel's, not ours: a parent NIC
// is a macvlan port or an ipvlan port, never both, because both kinds
// claim the same single rx_handler on the parent netdev. The second
// kind to ask gets EBUSY. Children of the SAME kind coexist happily, so
// this only bites where two networks of different modes share a parent
// — and the plugin creates children of its own accord, asynchronously,
// in the orphaned-lease reclaim (#370). That reclaim's temporary link
// outlives the endpoint it belongs to, so it can be holding the parent
// in one mode at the moment an unrelated endpoint asks for the other.
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
//     Plugin.mu, or a slow reclaim would block every registry read on
//     the plugin. Every current caller takes it with no plugin lock
//     held; keep it that way.
//
// The gate is advisory. Failing to get it is not an error: the caller
// proceeds and the kernel remains the authority, exactly as it was
// before this existed. That keeps a wedged or slow reclaim from turning
// into a hung container start — the worst case degrades to the EBUSY
// the caller would have got anyway, with a counter to say so.
type parentGate struct {
	mu     sync.Mutex
	tokens map[string]chan struct{}
}

// tokenFor returns the queue for one parent, creating it on first use.
//
// Entries are never removed. A host has a handful of NICs and the map
// is keyed by interface name, so it is bounded by the machine rather
// than by traffic; reclaiming entries would need a refcount whose only
// purpose is to free a few dozen bytes.
func (g *parentGate) tokenFor(parent string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
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

// acquire takes the gate for one parent, waiting up to budget.
//
// Returns a release func that is ALWAYS safe to call — on the timeout
// path it is a no-op, so callers can defer it unconditionally without
// caring whether the wait succeeded. The bool reports whether the gate
// was actually held, which is what the counters key on.
func (g *parentGate) acquire(ctx context.Context, parent string, budget time.Duration) (func(), bool) {
	if parent == "" {
		return func() {}, false
	}
	tok := g.tokenFor(parent)

	// The uncontended path, which is nearly all of them: no timer, no
	// allocation, no wait.
	select {
	case tok <- struct{}{}:
		return func() { <-tok }, true
	default:
	}

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case tok <- struct{}{}:
		return func() { <-tok }, true
	case <-ctx.Done():
		return func() {}, false
	case <-timer.C:
		return func() {}, false
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
// lockParent is the only thing that makes one, so the requirement is
// discharged by the compiler rather than by review.
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
func (p *Plugin) lockParent(ctx context.Context, parent, op string) *parentGuard {
	if p == nil || parent == "" {
		return &parentGuard{}
	}

	start := time.Now()
	release, ok := p.parentGate.acquire(ctx, parent, parentGateBudget)
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
	default:
		p.parentLinkWaitTimeouts.Add(1)
		log.WithFields(log.Fields{
			"parent": parent,
			"op":     op,
			"budget": parentGateBudget.String(),
		}).Warn("Gave up waiting for the parent interface; proceeding, the kernel may refuse this")
	}
	return &parentGuard{release: release}
}

// parentGateContendedFloor is the wait below which an acquisition is
// treated as uncontended. A successful non-blocking take still spends a
// few microseconds getting there, and counting those would make
// parent_link_waits climb on every endpoint creation on an idle host.
const parentGateContendedFloor = time.Millisecond
