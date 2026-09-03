// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// recordFileName is the durable lease record inside STATE_DIR: one
// JSON object per line, folded on read.
//
// A SECOND FILE BESIDE THE LEDGER, and the seam design asked for one.
// The design's reason for one file is that the audit question ("which
// address did this container hold last Tuesday") joins the DHCP half
// and the Docker half, and two files need a version handshake at
// restart. It is right, and it is not this milestone: the ledger's line
// format is documented, operators parse it, and folding it into the
// record's event stream changes what those parsers read. The two files
// are written from the same events, so they cannot disagree about what
// happened; what they cost is the join. Recorded in the handover as
// owed, not as done.
const recordFileName = "lease-records.jsonl"

// newRecordID mints a record's primary key.
//
// Random, and NOT the EndpointID, because a record is one BINDING
// ATTEMPT and an EndpointID is not: libnetwork mints a fresh endpoint
// for every container start, so keying on it would make a restarted
// container a stranger to its own address. The tombstone exists to
// bridge exactly that gap and here it is a phase of the same record.
func newRecordID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A record id that repeats folds two endpoints into one
		// record. Falling back to a clock would do exactly that on a
		// machine whose clock is coarse, so the failure is reported
		// and the caller runs without a record instead.
		log.WithError(err).Error("Could not mint a lease record id; this endpoint will not be resumable after a restart")
		return ""
	}
	return hex.EncodeToString(b[:])
}

// recordCreated writes the CREATED record for one endpoint and returns
// its id. An empty return means there is no record for this endpoint —
// every caller treats that as "not resumable", never as an error.
//
// Identity is written HERE and once (D10). It is the option-61 value as
// sent, type byte included, and the fold refuses a second write with
// different bytes: an identity that changes across a restart is a
// different client to the server, which hands out a second address.
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

// recordStore is the record file, or nil. On the manager rather than
// reached through m.plugin directly because m.plugin is nil in unit
// tests that drive a manager without a Plugin.
func (m *dhcpManager) recordStore() *dhcp.Records {
	if m.plugin == nil {
		return nil
	}
	return m.plugin.records
}

// resumeFromRecord finds this endpoint's record, says what its manager
// may ask the server for, and moves the record to JOINED.
//
// The three are one function because they are one decision: the record
// that answers the resume is the record the manager must then write to
// and the record that must be bound. Split apart, a caller could resume
// from one record and journal into another, and the two histories of
// one address would only be seen to differ at the next restart.
//
// The lookup is the scope+MAC index and NOT the id CreateEndpoint put
// in the Join hint, deliberately: recovery after a plugin restart has
// no hint — there was no CreateEndpoint in this process — so a
// hint-first path would leave the index exercised only on the rare
// path, which is the path nobody notices is broken. One mechanism,
// used on every Join.
func (m *dhcpManager) resumeFromRecord() (string, dhcp.Resumption) {
	if m.plugin == nil || m.plugin.records == nil {
		return "", dhcp.Resumption{}
	}
	mac := m.endpointMAC()
	id, res := m.plugin.recordResume(m.joinReq.NetworkID, mac)
	if id == "" {
		return "", dhcp.Resumption{}
	}
	m.plugin.recordBound(id, res.Phase)
	return id, res
}

// recordResume answers what a manager about to start on this identity
// may ask the server for.
//
// It returns the record's id as well, because a manager that resumes a
// record must write its events to THAT record: a second record for one
// identity is two histories of one address, and the older one is what a
// later restart would find first.
func (p *Plugin) recordResume(networkID string, mac net.HardwareAddr) (string, dhcp.Resumption) {
	if p.records == nil || len(mac) == 0 {
		return "", dhcp.Resumption{}
	}
	id, res, ok := p.records.Resume(networkID, mac, time.Now())
	if !ok {
		return "", dhcp.Resumption{}
	}
	return id, res
}

// recordBound moves a record to JOINED when it is not there already.
//
// The conditional is not defensive: the fold accepts a bind only from
// CREATED or ADOPTED, and plugin-restart recovery resumes a record a
// previous process already left JOINED. A bind written unconditionally
// would be refused there — and refused SILENTLY, since a rejected event
// still folds into a record with its Rejects counter bumped and nothing
// else moved.
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

// recordLeft is Leave: the manager stopped and the last lease snapshot
// stays. No RELEASE goes on the wire (D-7, #800).
func (p *Plugin) recordLeft(id string) {
	if p.records == nil || id == "" {
		return
	}
	if err := p.records.Left(id); err != nil {
		log.WithError(err).WithField("record", id).Warn("Could not record the end of the renewal client")
	}
}

// retainRecordFor lays the tombstone on the record for one identity.
//
// The deadline is the tombstone store's own TTL, from now. It is the
// caller's min(lease expiry, tombstone TTL) with the lease half left
// out on purpose: a record whose lease outlives the tombstone is still
// only useful for as long as a re-bind may consume it, and a deadline
// past that would keep answering lookups for an endpoint nothing can
// claim.
func (p *Plugin) retainRecordFor(networkID, mac string) {
	if p.records == nil || mac == "" {
		return
	}
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return
	}
	id, _, ok := p.records.Resume(networkID, hw, time.Now())
	if !ok {
		return
	}
	p.recordRetained(id, time.Now().Add(tombstoneTTL))
}

// closeRecord ends a record outright: CreateEndpoint failed after
// opening one, so there is no endpoint and never was a lease.
//
// CLOSED and not RETAINED, because a tombstone exists to be inherited
// and there is nothing here to inherit: no address was acquired, so the
// record answers no lookup. Leaving it CREATED would be harmless to
// correctness and is still wrong — it is a line that would sit in an
// append-only file for the life of the deployment.
func (p *Plugin) closeRecord(id string) {
	if p.records == nil || id == "" {
		return
	}
	if err := p.records.Closed(id); err != nil {
		log.WithError(err).WithField("record", id).Debug("Could not close the abandoned lease record")
	}
}

// recordRetained is DeleteEndpoint: the tombstone phase, with the
// deadline the tombstone store already computes.
func (p *Plugin) recordRetained(id string, deadline time.Time) {
	if p.records == nil || id == "" {
		return
	}
	if err := p.records.Retained(id, deadline); err != nil {
		log.WithError(err).WithField("record", id).Warn("Could not lay the lease record's tombstone")
	}
}
