// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"time"
)

// endpointDisposition is what becomes of the address a departing
// endpoint was holding. The two values are mutually exclusive, and that
// exclusivity is the whole point of this file (#800).
//
// Before it, both could happen for the same endpoint at the same time.
// DeleteEndpoint lays a tombstone saying "the next CreateEndpoint on
// this network re-requests exactly this MAC and these addresses", while
// the Join-abort paths spawn an orphan reclaim saying "hand that same
// lease back to the server". Observed on a restart in CI: the tombstone
// handed fd00:6470:6863::3d to the new child while the reclaim's
// release link solicited fd00:6470:6863::3d to give it away.
//
// The visible symptom was a user-facing HTTP 500 out of CreateEndpoint
// — both links are macvlan children of the same parent wearing the same
// MAC (the reclaim wears the recorded one so the server recognises the
// releasing identity), and the kernel refuses the second with
// EADDRINUSE. But the collision is downstream of the contradiction, and
// removing it by widening a lock or lengthening a budget would leave
// the contradiction live: a window in which the server is told the
// lease is free while the restarting container is claiming it.
type endpointDisposition uint8

const (
	// dispositionReserved: a tombstone holds this endpoint's MAC and
	// addresses for the restart that is coming. The lease stays ours,
	// so there is nothing to hand back.
	dispositionReserved endpointDisposition = iota + 1
	// dispositionReleased: the orphan reclaim is handing the lease
	// back, because no persistent client ever bound it and nothing is
	// using the address. Nothing may then promise it to a restart.
	dispositionReleased
)

func (d endpointDisposition) String() string {
	switch d {
	case dispositionReserved:
		return "reserved"
	case dispositionReleased:
		return "released"
	default:
		return "unknown"
	}
}

// dispositionTTL bounds how long a claim is remembered.
//
// tombstoneTTL exactly, and not by coincidence: the window in which the
// two dispositions can contradict each other is the window in which a
// tombstone can still be consumed. After it, the tombstone is gone and
// a reclaim has nothing to contradict.
//
// It is also what keeps the map bounded. Entries are created only when
// an endpoint departs, and nothing removes them at DeleteEndpoint —
// deliberately, because the reclaim is spawned from a goroutine that is
// ordered against DeleteEndpoint neither way, so an entry deleted when
// Docker finishes its teardown would be gone before the loser arrives
// to read it. Pruning on age instead bounds the map by departures per
// minute rather than by uptime.
const dispositionTTL = tombstoneTTL

// endpointDispositionClaim is one endpoint's decided disposition,
// carrying the time it was decided so it can be pruned.
type endpointDispositionClaim struct {
	disposition endpointDisposition
	at          time.Time
}

// claimEndpointDisposition decides, once, what happens to a departing
// endpoint's address, and reports whether the caller's want is what was
// decided.
//
// First caller wins and gets true. A later caller asking for the other
// disposition gets false and must not act. A later caller asking for
// the SAME disposition gets true — the claim is idempotent, so a
// retried or duplicated path is not punished for arriving twice.
//
// Deliberately a compare-and-set rather than two checks. "Is there a
// tombstone yet?" answered from the reclaim, and "is a reclaim running?"
// answered from DeleteEndpoint, are the same question asked from both
// ends, and both answers can be no at once — which is exactly the
// interleaving that produced #800.
//
// Takes p.mu only for the map operation, and holds nothing across IO,
// so it obeys the leaf-lock rule parent_gate.go states: the per-parent
// token may never be taken while a plugin lock is held, and this
// returns long before any caller reaches one.
func (p *Plugin) claimEndpointDisposition(endpointID string, want endpointDisposition) bool {
	if p == nil || endpointID == "" {
		// No plugin to record on, or no endpoint to record against.
		// Refusing here would silently disable the reclaim in the unit
		// tests that build a bare manager, so the claim degrades to
		// "granted" and the pre-#800 behaviour stands.
		return true
	}

	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.endpointDispositions == nil {
		p.endpointDispositions = make(map[string]endpointDispositionClaim)
	}
	p.pruneEndpointDispositionsLocked(now)

	if held, ok := p.endpointDispositions[endpointID]; ok {
		return held.disposition == want
	}
	p.endpointDispositions[endpointID] = endpointDispositionClaim{disposition: want, at: now}
	return true
}

// endpointDispositionOf reports the disposition decided for an
// endpoint, if one has been. For tests and for the health surface; the
// decision itself always goes through claimEndpointDisposition.
func (p *Plugin) endpointDispositionOf(endpointID string) (endpointDisposition, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	held, ok := p.endpointDispositions[endpointID]
	if !ok {
		return 0, false
	}
	return held.disposition, true
}

// pruneEndpointDispositionsLocked drops claims older than dispositionTTL.
// Caller holds p.mu.
func (p *Plugin) pruneEndpointDispositionsLocked(now time.Time) {
	for id, held := range p.endpointDispositions {
		if now.Sub(held.at) > dispositionTTL {
			delete(p.endpointDispositions, id)
		}
	}
}
