// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"sync"
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
}

// add appends a tombstone, pruning expired entries in the same write.
// Returns the save error so the caller owns the health counter; the
// counters stay on Plugin because that is where /Plugin.Health reads
// them, and moving them would change an operator-visible surface for a
// refactor that is supposed to change nothing.
func (s *tombstoneStore) add(networkID, hostname, mac, ipv4, ipv6 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts, err := loadTombstones()
	if err != nil {
		log.WithError(err).Warn("Failed to load tombstones; starting fresh")
		ts = nil
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
