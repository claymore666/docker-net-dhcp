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

// parentGateBudget stays below the validate_dhcp probe's round trip, so a start degrades before Docker's own
// CreateEndpoint timeout fires; a DORA takes about 2-3 s (#800).
const parentGateBudget = 4 * time.Second

// parentGate serialises child-link adds per parent NIC (#370): macvlan and ipvlan both claim the parent's single
// rx_handler, so the second kind gets EBUSY while same-kind children coexist. Per parent, since a global lock
// would serialise every endpoint on the host. mu is a leaf held across nothing that waits; a parent's token is held
// across IO and is never taken under Plugin.mu. Advisory: on timeout the caller proceeds and the kernel decides.
type parentGate struct {
	mu     sync.Mutex
	tokens map[string]chan struct{}
	// holders is the kind each holder is attaching, for reporting: the kernel refuses only the cross pair (#110).
	holders map[string]string
	waiters map[string]map[*parentWaiter]struct{}
}

// parentWaiter.foreign is write-once-true, so a cross-kind holder that came and went during the wait is still seen
// (#110).
type parentWaiter struct {
	kind    string
	foreign bool
}

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

// enterWait registers and makes the first non-blocking attempt under one lock, so no holder change falls between
// (#110).
func (g *parentGate) enterWait(parent, kind string) (chan struct{}, *parentWaiter, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tok := g.tokenLocked(parent)
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

func (g *parentGate) leaveWait(parent string, w *parentWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.waiters[parent], w)
	if len(g.waiters[parent]) == 0 {
		delete(g.waiters, parent)
	}
}

func (g *parentGate) noteTake(parent, kind string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.noteTakeLocked(parent, kind)
}

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

func (g *parentGate) clearHolder(parent string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.holders, parent)
}

func (g *parentGate) holderKind(parent string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holders[parent]
}

func (g *parentGate) heldThroughout(w *parentWaiter) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if w.foreign {
		return ""
	}
	return w.kind
}

func (g *parentGate) acquire(ctx context.Context, parent, kind string, budget time.Duration) (func(), bool, string) {
	if parent == "" {
		return func() {}, false, ""
	}

	tok, w, took := g.enterWait(parent, kind)
	defer g.leaveWait(parent, w)
	if took {
		return g.release(parent, tok), true, ""
	}

	return g.waitForParent(ctx, parent, kind, budget, tok, w)
}

func (g *parentGate) take(parent, kind string, tok chan struct{}) func() {
	g.noteTake(parent, kind)
	return g.release(parent, tok)
}

func (g *parentGate) release(parent string, tok chan struct{}) func() {
	return func() {
		g.clearHolder(parent)
		<-tok
	}
}

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

// parentGuard is evidence that a parent's gate is held; scripts/check-parent-gate-accounting.sh refuses one built
// outside this file, and it does not name which parent (#558).
type parentGuard struct {
	release func()
}

// Unlock releases the gate.
func (g *parentGuard) Unlock() {
	if g == nil || g.release == nil {
		return
	}
	g.release()
}

// addChildLink takes an unused guard so the gate is checked by the compiler; parentless bridge links bypass it (#558).
func addChildLink(_ *parentGuard, link netlink.Link) error {
	return netlink.LinkAdd(link)
}

func (p *Plugin) lockParent(ctx context.Context, parent, kind, op string) *parentGuard {
	if p == nil || parent == "" {
		return &parentGuard{}
	}

	start := time.Now()
	release, ok, heldKind := p.parentGate.acquire(ctx, parent, kind, parentGateBudget)
	waited := time.Since(start)

	switch {
	case ok && waited < parentGateContendedFloor:
	case ok:
		p.parentLinkWaits.Add(1)
		log.WithFields(log.Fields{
			"parent": parent,
			"op":     op,
			"waited": waited.String(),
		}).Debug("Waited for another operation to finish with the parent interface")
	case heldKind != "" && heldKind == kind:
		// A wait lost to a holder of the same kind is counted as a wait, not a timeout: the kernel refuses only the
		// cross pair, and `docker compose up` on one macvlan network lands here on every start (#110).
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

// parentGateContendedFloor keeps the microseconds of a non-blocking take out of parent_link_waits.
const parentGateContendedFloor = time.Millisecond
