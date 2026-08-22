// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// tombstoneStore owns the tombstones.json read-modify-write path and the
// lock that serialises it (#643).
//
// # WHAT THIS BUYS, AND WHAT IT DOES NOT
//
// It does NOT make the locking rule compiler-enforced. Go's unexported
// identifiers are package-visible, so `p.tombstones.mu` is reachable
// from every file in this package exactly as `p.tombstoneMu` was. Only a
// separate package would change that, and the store needs the tombstone
// type, the state dir and the TTL, so the boundary would cost more than
// it returns. Saying so here because the opposite is easy to assume from
// the shape of the code — and an assumed guarantee is worse than a
// documented gap.
//
// What it does buy is that the lock and every operation that may hold it
// are now one small file with a two-method surface, instead of a mutex
// declared on Plugin next to `mu` — where the next person adding a field
// sees two locks side by side and no indication that combining them
// deadlocks. The rule itself is enforced by
// scripts/check-lock-discipline.sh, which goes red if any function locks
// both. That is the half a comment cannot hold.
//
// The zero value is usable, deliberately: tests construct &Plugin{}
// directly, and a store that needed a constructor would turn those into
// nil-pointer panics — a behaviour change smuggled in by a refactor
// whose entire premise is that it changes nothing.
type tombstoneStore struct {
	mu sync.Mutex

	// quarantines counts times the tombstone file was found unparseable
	// and moved aside as tombstones.json.corrupt-<ts> (#724). Published
	// as tombstone_quarantines and reported on /Plugin.Health, where it
	// is healthy-affecting.
	//
	// Named for what it counts, not for where it was noticed. It is NOT
	// "load failures": a transient EIO is a load failure and is not
	// counted here, because it leaves nothing on disk to look at and
	// nothing lost. A quarantine is a persistent, operator-actionable
	// event with a file to read.
	//
	// It is deliberately NOT folded into tombstoneWriteFailures either.
	// The two have different remedies -- a write failure means the disk
	// is full or read-only, a quarantine means a file to inspect -- and
	// merging them leaves an operator unable to tell which one they are
	// being paged for.
	//
	// It lives here rather than on Plugin so the zero value stays usable
	// -- tests construct &Plugin{} directly, and a counter that needed a
	// constructor would turn those into nil-pointer panics.
	quarantines atomic.Int32
}

// add appends a tombstone, pruning expired entries in the same write.
// Returns the save error so the caller owns the health counter; the
// counters stay on Plugin because that is where /Plugin.Health reads
// them, and moving them would change an operator-visible surface for a
// refactor that is supposed to change nothing.
func (s *tombstoneStore) add(networkID, hostname, mac, ipv4, ipv6 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// This function is the only one that can destroy the file. It used
	// to treat EVERY load error as "start fresh" -- set the list to nil
	// and save -- which rewrote the file with a single entry and
	// silently deleted every other live tombstone in it, at Warn, on a
	// path nobody watches. The blast radius is the product's headline
	// promise: every container mid-restart on this host loses its MAC
	// and its IP hint (#724).
	//
	// So the two failures are separated. Only one of them is safe to
	// continue past.
	ts, err := loadTombstones()
	switch {
	case err == nil:
	case errors.Is(err, errTombstonesQuarantined):
		// The contents were unparseable and are now saved aside under
		// their own name. There is nothing to preserve and nothing left
		// to overwrite, so continuing with a fresh list is correct.
		// Counted rather than only logged: a log line is not a surface
		// anyone alerts on, and every tombstone that file held is gone
		// from the live set.
		s.quarantines.Add(1)
		log.WithError(err).Warn("Tombstone file was corrupt and has been quarantined; restarting containers will pick new MACs and addresses until the window passes")
		ts = nil
	default:
		// A transient read failure -- EIO, EMFILE, a read racing a
		// writer. The file may be perfectly good, so writing here would
		// destroy live data because a descriptor was briefly
		// unavailable. Refuse instead. The caller counts it and this
		// one endpoint loses its restart stability, which is the same
		// outcome as a failed write and is what tombstone_write_failures
		// already means.
		return fmt.Errorf("refusing to rewrite tombstones after a failed read: %w", err)
	}
	ts = append(pruneTombstones(ts), tombstone{
		NetworkID:   networkID,
		Hostname:    hostname,
		MacAddress:  mac,
		IPAddress:   ipv4,
		IPv6Address: ipv6,
		DeletedAt:   time.Now(),
	})
	return saveTombstones(ts)
}

// consume returns and removes a tombstone for networkID iff EXACTLY one
// fresh entry matches. When hostname is non-empty the match is narrowed
// to NetworkID+Hostname so a sequential `compose restart` of several
// containers on one network cannot swap identities between them. When
// hostname is empty it falls back to NetworkID-only matching, preserving
// the v0.5.0 contract for hostname-less containers and for races where
// the lookup did not return in time. The "exactly one" rule still
// applies after filtering.
func (s *tombstoneStore) consume(networkID, hostname string) (mac, ipv4, ipv6 string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts, err := loadTombstones()
	if err != nil {
		// consume writes nothing on a load failure and always got this
		// right; it is add that used to destroy the file. The quarantine
		// is still counted here, because it is the same event with the
		// same remedy whichever reader trips it first -- and on an idle
		// host a consume can easily be the one that does.
		if errors.Is(err, errTombstonesQuarantined) {
			s.quarantines.Add(1)
		}
		log.WithError(err).Warn("Failed to load tombstones; treating as empty")
		return "", "", "", false
	}
	preLen := len(ts)
	ts = pruneTombstones(ts)
	pruned := len(ts) != preLen

	matchIdx, matches := -1, 0
	for i, t := range ts {
		if t.NetworkID != networkID {
			continue
		}
		// When the caller knows the hostname, only match tombstones
		// whose hostname agrees. Tombstones written by a v0.5.0 build
		// (or with hostname-lookup races) have empty Hostname; treat
		// them as "matches anything" so we don't regress those.
		if hostname != "" && t.Hostname != "" && t.Hostname != hostname {
			continue
		}
		matches++
		matchIdx = i
	}

	if matches != 1 {
		// More than one match → ambiguous. Drop *all* matches so the
		// next consume for this network/hostname doesn't keep hitting
		// the same poisoned set for the rest of the TTL window. Zero
		// matches is harmless; the prune still gets persisted iff it
		// changed something.
		dirty := pruned
		if matches > 1 {
			kept := ts[:0]
			for _, t := range ts {
				if t.NetworkID == networkID {
					if hostname != "" && t.Hostname != "" && t.Hostname != hostname {
						kept = append(kept, t)
						continue
					}
					continue
				}
				kept = append(kept, t)
			}
			ts = kept
			dirty = true
		}
		// Skip the rewrite when nothing changed (I-10 in the 2026-05-05
		// review): the common no-op consume on a quiet network used to
		// fsync a file write per CreateEndpoint.
		if dirty {
			if err := saveTombstones(ts); err != nil {
				log.WithError(err).Debug("Failed to persist pruned tombstones")
			}
		}
		return "", "", "", false
	}

	mac = ts[matchIdx].MacAddress
	ipv4 = ts[matchIdx].IPAddress
	ipv6 = ts[matchIdx].IPv6Address
	ts = append(ts[:matchIdx], ts[matchIdx+1:]...)
	if err := saveTombstones(ts); err != nil {
		log.WithError(err).Warn("Failed to persist tombstones after consume")
	}
	return mac, ipv4, ipv6, true
}
