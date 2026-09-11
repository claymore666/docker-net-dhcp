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

// recordCreated6 opens the DHCPv6 record for one endpoint.
//
// It writes the identity as bytes, which is what makes the DUID
// durable: an identity re-derived on the next start is one that can
// change, and RFC 9915 section 11 says a DUID "SHOULD NOT change over
// time if at all possible".
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

// recordKey is endpointRecordKey for this manager's endpoint: the value
// its records are indexed under.
//
// It is a method rather than a call at each site because the create
// side and the resume side must agree exactly, and they are in
// different files. A manager that resumed under a different key from
// the one CreateEndpoint filed the record under finds nothing, mints a
// fresh identity, and the endpoint quietly becomes a new client.
func (m *dhcpManager) recordKey() net.HardwareAddr {
	return endpointRecordKey(m.opts.effectiveMode(), m.joinReq.EndpointID, m.endpointMAC())
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
	id, res := m.plugin.recordResume(m.joinReq.NetworkID, m.recordKey())
	if id == "" {
		return "", dhcp.Resumption{}
	}
	m.plugin.recordBound(id, res.Phase)
	return id, res
}

// resumeFromRecord6 is resumeFromRecord in the v6 scope, and it hands
// back the stored DHCPv6 identity as well.
//
// THE IDENTITY IS THE HALF THAT MATTERS MOST ACROSS A RESTART. The
// lease makes the first message a Confirm rather than a Solicit (#820);
// the identity is what makes it the SAME client either way, and a
// Confirm sent under a freshly minted DUID names a binding the server
// files under somebody else. RFC 9915 section 11 is the rule and this
// is where it is kept.
//
// A zero identity means the record predates the DUID or could not be
// read back, and the caller mints a fresh one — see setupClient. That
// is a new client to the server, which is worse than resuming and
// better than refusing to start.
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

// recordResume answers what a manager about to start on this identity
// may ask the server for.
//
// It returns the record's id as well, because a manager that resumes a
// record must write its events to THAT record: a second record for one
// identity is two histories of one address, and the older one is what a
// later restart would find first.
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

// settleReleasedRecord writes the teardown phase of one family's
// record: CLOSED when its lease was handed back, LEFT when it was not.
//
// ONE FUNCTION FOR BOTH ANSWERS so the two cannot be written at
// different call sites and drift. CLOSED is the right phase for a
// released lease for the reason closeRecord gives for an abandoned one:
// the record answers no lookup any more. There is a difference worth
// stating -- closeRecord's record never had an address, and this one
// had it and gave it back -- and it makes no difference to what the
// record must now do, which is nothing (#962).
func (p *Plugin) settleReleasedRecord(id string, released bool) {
	if released {
		p.closeRecord(id)
		return
	}
	p.recordLeft(id)
}

// retainRecordFor lays the tombstone on the record for one identity.
//
// The deadline is the tombstone store's own TTL, from now. It is the
// caller's min(lease expiry, tombstone TTL) with the lease half left
// out on purpose: a record whose lease outlives the tombstone is still
// only useful for as long as a re-bind may consume it, and a deadline
// past that would keep answering lookups for an endpoint nothing can
// claim.
func (p *Plugin) retainRecordFor(networkID string, key net.HardwareAddr) {
	if p.records == nil || len(key) == 0 {
		return
	}
	hw := key
	if id, _, ok := p.records.Resume(networkID, hw, time.Now()); ok {
		p.recordRetained(id, time.Now().Add(tombstoneTTL))
	}
	// The v6 record is a SECOND record under a second scope
	// (dhcp.Scope6), so it needs its own tombstone: a dual-stack
	// endpoint whose v4 record was retained and whose v6 record was not
	// keeps its IPv4 address across a restart and loses its IPv6 one,
	// which is exactly the asymmetry #820 exists to remove.
	if id, _, _, ok := p.records.Resume6(networkID, hw, time.Now()); ok {
		p.recordRetained(id, time.Now().Add(tombstoneTTL))
	}
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
