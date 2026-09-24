// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// tombstoneStore owns the tombstones.json read-modify-write and its lock (#643); its zero value is
// usable, and scripts/check-lock-discipline.sh fails any function that holds this lock with p.mu.
type tombstoneStore struct {
	mu sync.Mutex

	// quarantines counts tombstone files found unparseable and moved to tombstones.json.corrupt-<ts> (#724).
	quarantines stampedCounter
}

func (s *tombstoneStore) add(networkID, hostname, mac, ipv4, ipv6 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Only a parse failure may start a fresh list: treating every load error that way once deleted
	// every live tombstone on the host (#724).
	ts, err := loadTombstones()
	switch {
	case err == nil:
	case errors.Is(err, errTombstonesQuarantined):
		s.quarantines.Add(1)
		log.WithError(err).Warn("Tombstone file was corrupt and has been quarantined; restarting containers will pick new MACs and addresses until the window passes")
		ts = nil
	default:
		// A transient read failure such as EIO or EMFILE is refused, since the file may be intact (#724).
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

// consume removes and returns the one fresh tombstone that matches networkID, and hostname when given.
func (s *tombstoneStore) consume(networkID, hostname string) (mac, ipv4, ipv6 string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts, err := loadTombstones()
	if err != nil {
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
		// A tombstone with no hostname, from v0.5.0 or a lookup race, matches any hostname.
		if hostname != "" && t.Hostname != "" && t.Hostname != hostname {
			continue
		}
		matches++
		matchIdx = i
	}

	if matches != 1 {
		// An ambiguous match drops every candidate so the next consume is not poisoned for the whole TTL.
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
