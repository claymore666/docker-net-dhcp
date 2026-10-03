// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"io/fs"
	"sort"

	log "github.com/sirupsen/logrus"
)

// pendingCreate is a CreateNetwork that has not returned; seq orders the entries so that of two creates, only the
// later one sees the earlier (#1187).
type pendingCreate struct {
	seq  uint64
	opts DHCPNetworkOptions
}

// beginCreate enters the network into the creates in flight, which dockerd lists only after its driver returned, and
// returns the function that leaves them; only its own entry is removed, not a retry's (#1187).
func (p *Plugin) beginCreate(networkID string, opts DHCPNetworkOptions) func() {
	p.createMu.Lock()
	if p.creating == nil {
		p.creating = map[string]pendingCreate{}
	}
	p.createSeq++
	mine := p.createSeq
	p.creating[networkID] = pendingCreate{seq: mine, opts: opts}
	p.createMu.Unlock()
	return func() {
		p.createMu.Lock()
		if cur, ok := p.creating[networkID]; ok && cur.seq == mine {
			delete(p.creating, networkID)
		}
		p.createMu.Unlock()
	}
}

// earlierCreates returns the creates in flight, other than networkID's own, that entered before it. Two creates that
// each saw the other would both be refused, so the first to enter is the one that goes through (#1187).
func (p *Plugin) earlierCreates(networkID string) map[string]DHCPNetworkOptions {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	own, registered := p.creating[networkID]
	out := make(map[string]DHCPNetworkOptions, len(p.creating))
	for id, c := range p.creating {
		if id == networkID || (registered && c.seq > own.seq) {
			continue
		}
		out[id] = c.opts
	}
	return out
}

// storedSiblings returns the stored records of the networks other than networkID, the ones a create that has returned
// leaves behind before dockerd lists it. A record of a network Docker no longer has lives until dropStaleNetworks at
// the next start, as in bridgeUsers; an unreadable store is logged and the in-flight set and the list remain (#1187).
func storedSiblings(networkID string) map[string]DHCPNetworkOptions {
	stored, err := storedNetworkOptions()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not read the stored network records to look for siblings; checking the in-flight creates and the Docker list only")
	}
	delete(stored, networkID)
	return stored
}

// sortedIDs gives the check a fixed order, so the sibling a refusal names does not depend on map order (#1187).
func sortedIDs(m map[string]DHCPNetworkOptions) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// siblingsBeforeList returns the creates in flight and the stored records, in that order and both before the caller
// reads Docker's list. A create enters the set, saves its record, leaves the set and is listed last, so a sibling that
// left the set before the first read has its record on disk by the second (#1187).
func (p *Plugin) siblingsBeforeList(networkID string) (inflight, stored map[string]DHCPNetworkOptions) {
	inflight = p.earlierCreates(networkID)
	return inflight, storedSiblings(networkID)
}
