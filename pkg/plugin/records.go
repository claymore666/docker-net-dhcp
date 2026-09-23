// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// recordFileName is the lease record inside STATE_DIR, a second file beside the ledger because operators parse the
// ledger's lines (#899).
const recordFileName = "lease-records.jsonl"

// newRecordID is random, not the EndpointID: libnetwork mints a fresh endpoint on every container start (#899).
func newRecordID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A clock fallback would repeat ids on a coarse clock, so a failure runs without a record (#899).
		log.WithError(err).Error("Could not mint a lease record id; this endpoint will not be resumable after a restart")
		return ""
	}
	return hex.EncodeToString(b[:])
}

// recordCreated writes the CREATED record once, with the option-61 identity as sent; a changed identity is a new
// client to the server (#899).
func (p *Plugin) recordCreated(networkID string, mac net.HardwareAddr, identity []byte) string {
	if p.records == nil {
		return ""
	}
	id := newRecordID()
	if id == "" {
		return ""
	}
	if err := p.records.Created(id, networkID, mac, identity); err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not write the endpoint's lease record; its address will not survive a plugin restart")
		return ""
	}
	return id
}

// recordCreated6 stores the DUID as bytes, since RFC 9915 section 11 says a DUID "SHOULD NOT change over time" (#911).
func (p *Plugin) recordCreated6(networkID string, mac net.HardwareAddr, id6 dhcp.Identity6) string {
	if p.records == nil {
		return ""
	}
	id := newRecordID()
	if id == "" {
		return ""
	}
	if err := p.records.Created6(id, networkID, mac, id6.Bytes()); err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not write the endpoint's DHCPv6 lease record; its address and its DUID will not survive a plugin restart")
		return ""
	}
	return id
}

func (m *dhcpManager) recordKey() net.HardwareAddr {
	return endpointRecordKey(m.opts.effectiveMode(), m.joinReq.EndpointID, m.endpointMAC())
}

func (m *dhcpManager) recordStore() *dhcp.Records {
	if m.plugin == nil {
		return nil
	}
	return m.plugin.records
}

// resumeFromRecord looks up by the scope+MAC index, not the Join hint: recovery after a plugin restart has no hint
// (#899, #911).
func (m *dhcpManager) resumeFromRecord() (string, dhcp.Resumption) {
	if m.plugin == nil || m.plugin.records == nil {
		return "", dhcp.Resumption{}
	}
	id, res := m.plugin.recordResume(m.joinReq.NetworkID, m.recordKey())
	if id == "" {
		return "", dhcp.Resumption{}
	}
	m.plugin.recordBound(id, res.Phase)
	return id, res
}

// resumeFromRecord6 also returns the stored DUID: a Confirm under a new DUID names another client's binding (RFC 9915
// section 11, #820).
func (m *dhcpManager) resumeFromRecord6() (string, dhcp.Resumption, dhcp.Identity6) {
	if m.plugin == nil || m.plugin.records == nil {
		return "", dhcp.Resumption{}, dhcp.Identity6{}
	}
	key := m.recordKey()
	if len(key) == 0 {
		return "", dhcp.Resumption{}, dhcp.Identity6{}
	}
	id, res, id6, ok := m.plugin.records.Resume6(m.joinReq.NetworkID, key, time.Now())
	if !ok {
		return "", dhcp.Resumption{}, dhcp.Identity6{}
	}
	m.plugin.recordBound(id, res.Phase)
	return id, res, id6
}

func (p *Plugin) recordResume(networkID string, key net.HardwareAddr) (string, dhcp.Resumption) {
	if p.records == nil || len(key) == 0 {
		return "", dhcp.Resumption{}
	}
	id, res, ok := p.records.Resume(networkID, key, time.Now())
	if !ok {
		return "", dhcp.Resumption{}
	}
	return id, res
}

// The fold accepts a bind only from CREATED or ADOPTED, and recovery resumes records already JOINED;
// a refused event is silent, so the bind is conditional (#899).
func (p *Plugin) recordBound(id string, phase string) {
	if p.records == nil || id == "" {
		return
	}
	if phase == "joined" {
		return
	}
	if err := p.records.Bound(id); err != nil {
		log.WithError(err).WithField("record", id).Warn("Could not record the start of the renewal client")
	}
}

// recordLeft: nothing went on the wire, under release_lease=never (#800) or a failed on_stop release (#962), so the
// address stays resumable.
func (p *Plugin) recordLeft(id string) {
	if p.records == nil || id == "" {
		return
	}
	if err := p.records.Left(id); err != nil {
		log.WithError(err).WithField("record", id).Warn("Could not record the end of the renewal client")
	}
}

// settleReleasedRecord writes CLOSED when the lease went back and LEFT when it did not, in one place (#962).
func (p *Plugin) settleReleasedRecord(id string, released bool) {
	if released {
		p.closeRecord(id)
		return
	}
	p.recordLeft(id)
}

// retainRecordFor reads the newest record per scope, not Resume: Resume skips a CLOSED record to an older one,
// which after a release would re-tombstone the address just handed back (#962). The v6 record is a second scope,
// walked separately, so a restart keeps both families (#820).
func (p *Plugin) retainRecordFor(networkID string, key net.HardwareAddr) {
	if p.records == nil || len(key) == 0 {
		return
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not read the lease records back; this endpoint's record keeps no tombstone deadline")
		return
	}
	deadline := time.Now().Add(tombstoneTTL)
	for _, scope := range []string{networkID, dhcp.Scope6(networkID)} {
		matches := rb.ByScopeMAC(scope, key)
		if len(matches) == 0 {
			continue
		}
		last := matches[len(matches)-1]
		if last.Phase == lease.PhaseClosed {
			continue
		}
		p.recordRetained(last.ID, deadline)
	}
}

// closeRecord ends the record as CLOSED: no address was acquired, so there is nothing for a tombstone to hand on
// (#899).
func (p *Plugin) closeRecord(id string) {
	if p.records == nil || id == "" {
		return
	}
	if err := p.records.Closed(id); err != nil {
		log.WithError(err).WithField("record", id).Debug("Could not close the abandoned lease record")
	}
}

func (p *Plugin) recordRetained(id string, deadline time.Time) {
	if p.records == nil || id == "" {
		return
	}
	if err := p.records.Retained(id, deadline); err != nil {
		log.WithError(err).WithField("record", id).Warn("Could not lay the lease record's tombstone")
	}
}
