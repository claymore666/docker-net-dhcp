// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

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
