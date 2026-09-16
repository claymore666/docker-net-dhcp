// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// releaseSettle is how long after a record's deadline the address is
// handed back on a `release_lease=on_remove` network (#984).
//
// THE DEADLINE IS NOT THE MOMENT TO SEND. A restart claims the address
// back by consuming the tombstone at CreateEndpoint, which can happen
// at any instant BEFORE the deadline, and the record that consume opens
// is written milliseconds after it -- so a sweep that fired exactly at
// the deadline could look between the two and see an address nothing
// holds. It would then tell the server an address is free that a
// container is at that moment being promised: #524's duplicate
// assignment, with this plugin's own fingerprints on it.
//
// Five seconds is three orders of magnitude more than the gap it
// covers, and it costs only lateness on an address that is going back
// anyway. The tick below is 15 s, so the wall clock from the stop to
// the datagram is 65 to 80 seconds.
const releaseSettle = 5 * time.Second

// sweepDeferredReleases hands back every address whose restart window
// has run out on a `release_lease=on_remove` network.
//
// `now` is a parameter and not a clock read, which is what makes this
// testable without a knob: the caller is the ticker, and a test passes
// the time it wants to ask about. It is the same shape
// sweepIPAMReservations beside it already has.
func (p *Plugin) sweepDeferredReleases(now time.Time) int {
	sent, _ := p.handRetainedRecordsBack("", func(rec lease.Record) bool {
		return !rec.Deadline.IsZero() && !now.Before(rec.Deadline.Add(releaseSettle))
	})
	return sent
}

// releaseNetworkRecords hands back every address this network is still
// holding, at once and whatever the deadline says (#984).
//
// WITHOUT IT THEY LEAK SILENTLY AND NOTHING CAN RECLAIM THEM. The sweep
// above reads a network's `release_lease` and its parent interface from
// the network's stored options, and `docker network rm` removes them;
// the tombstones that could have handed an address to a restarting
// container are keyed by the network id and die with it as well. So a
// held address whose network is removed inside the window has no way
// back to the pool and no way back to a container either: it sits
// leased until the server's own clock frees it, which is the `never`
// outcome on a network that asked for the opposite.
//
// No settle, because nothing can claim an address back on a network
// that no longer exists.
func (p *Plugin) releaseNetworkRecords(networkID string) int {
	sent, undecided := p.handRetainedRecordsBack(networkID, func(lease.Record) bool { return true })

	// THE ONE PLACE WHERE "a later pass can decide" IS FALSE.
	// loadReleaseOptions leaves a record alone when the network's
	// options cannot be read, because a file that is unreadable now may
	// be readable on the next tick. Here there is no next tick: the
	// options file is deleted a few lines below, the tombstones are
	// keyed by this network id and go with it, and the record is left
	// holding an address nothing will ever look at again. Nothing can
	// be sent -- the release needs the parent interface and the value
	// of `release_lease`, and both were in the file -- so the only
	// thing left to do is say so where an operator will find it, at
	// warning level and not at the debug level the recoverable case
	// uses.
	if undecided > 0 {
		log.WithFields(log.Fields{
			"network": networkID,
			"held":    undecided,
		}).Warn("This network's stored options could not be read while it was being removed, so its held addresses could not be handed back; they stay leased until the server's lease expires")
	}
	return sent
}

// handRetainedRecordsBack is one pass over the RETAINED records.
//
// networkID narrows it to one network, or is empty for every network.
// due decides which records this pass is about; everything else --
// which networks release at all, whether the address was claimed back,
// the send, the counters and the record's end -- is the same in both
// callers, because it is the same decision.
//
// It reports what left the host AND how many due records it could not
// decide, which is the count of records on a network whose options
// could not be read. The two callers do different things with the
// second number: the sweep leaves them for the next tick, and the
// network-removal drain has no next tick and warns.
func (p *Plugin) handRetainedRecordsBack(networkID string, due func(lease.Record) bool) (sent int, undecided int) {
	if p.records == nil {
		return 0, 0
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).Warn("Could not read the lease records back; no held address can be handed back this pass")
		return 0, 0
	}

	// Read once per network and not once per record: a dual-stack
	// endpoint is two records and a busy host is many, and the answer
	// cannot change inside one pass. A network whose options could not
	// be read is remembered as such, so the warning is not repeated
	// per record either.
	opts := map[string]*DHCPNetworkOptions{}
	for _, rec := range rb.Records {
		if rec.Phase != lease.PhaseRetained {
			continue
		}
		id, v6 := dhcp.NetworkOfScope(rec.Scope)
		if networkID != "" && id != networkID {
			continue
		}
		if !due(rec) {
			continue
		}
		o, known := opts[id]
		if !known {
			o = loadReleaseOptions(id)
			opts[id] = o
		}
		if o == nil {
			undecided++
			continue
		}
		if !o.releasesOnRemove() {
			continue
		}
		if p.handOneRecordBack(rb, rec, *o, id, v6) {
			sent++
		}
	}
	return sent, undecided
}

// loadReleaseOptions reads one network's persisted options, or nil when
// they cannot be read.
//
// A NIL IS "LEAVE THIS RECORD ALONE" AND NOT "THIS NETWORK DOES NOT
// RELEASE". The two are the same inaction and they are not the same
// fact: an unreadable options file is a network whose `release_lease`
// is unknown, and the record keeps its address and its RETAINED phase
// so that a later pass, or a plugin that can read the file again, can
// still decide. Guessing `never` here would close nothing and send
// nothing, which looks identical until the file becomes readable.
func loadReleaseOptions(networkID string) *DHCPNetworkOptions {
	o, err := loadOptions(networkID)
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Debug("A held address belongs to a network whose options cannot be read; leaving the record as it is")
		return nil
	}
	return &o
}

// handOneRecordBack is the whole of what happens to one held record of
// one family, and it reports whether a datagram left the host.
//
// EVERY OUTCOME ENDS THE RECORD. One attempt, then CLOSED, whatever
// happened: that is `on_stop`'s rule, and the alternative is a parent
// with no IPv6 address warning every 15 seconds for the life of the
// deployment. A CLOSED record answers no lookup and Resume walks past
// it, so a later restart cannot ask the server to confirm an address
// this pass has handed back.
func (p *Plugin) handOneRecordBack(rb lease.Rebuilt, rec lease.Record, opts DHCPNetworkOptions, networkID string, v6 bool) bool {
	fields := log.Fields{
		"network": shortID(networkID),
		"record":  rec.ID,
		"is_ipv6": v6,
	}
	if addr, ok := rec.Addr(); ok {
		fields["ip"] = addr.String()
	}

	if holder, kind := recordClaimedBack(rb, rec); kind != claimNone {
		// ONLY A LIVE HOLDER IS A RECLAIM, and the counter says so
		// because an operator reads it as "the window did its job".
		// The other two kinds are not that: a newer held record of the
		// same address is a second stop that will decide the address
		// itself, and an acquisition in flight is an address left to
		// expire on the server's clock because it MIGHT be handed to
		// that exchange. Counting either as a reclaim would report a
		// restart that never happened.
		if kind == claimLive {
			bumpFamily(&p.releasesReclaimedV4, &p.releasesReclaimedV6, v6)
		}
		p.closeRecord(rec.ID)
		log.WithFields(fields).WithField("holder", holder).Debug(claimReasons[kind])
		return false
	}

	held, _ := rec.Addr()
	log.WithFields(fields).
		Info("No container claimed this address back inside the restart window, so release_lease=on_remove is handing it back")
	out := releaseFromRecord(rec, opts, v6, held, fields)
	p.countRelease(v6, out == releaseSent)
	announceReleaseOutcome(log.WithFields(fields).WithField("outcome", string(out)), out)
	p.closeRecord(rec.ID)
	return out == releaseSent
}

// recordClaimedBack answers the only question that decides whether a
// held address may go back: is something else still using it?
//
// IT IS KEYED ON THE ADDRESS AND NOT ON THE MAC, and that is the whole
// of the predicate. A MAC-keyed check reads "this identity is in use
// again", which is not the same claim: a container pinned with
// `--mac-address` that restarts after the window comes back under the
// same MAC and is given a DIFFERENT address, and a check that stopped
// at the MAC would call the old address claimed and leave it leased
// with nothing ever handing it back. The question is about the address.
//
// NEWER MEANS LATER IN THE SLICE. lease.Record carries no creation
// time; Rebuild returns records in the order they were created and
// ByScopeAddr and ByScopeMAC preserve it, so slice position is the only
// ordering there is and it is the one the library documents.
//
// The holder's id is returned so the log line names it: "claimed back"
// with nothing to look at is a sentence an operator cannot check.
func recordClaimedBack(rb lease.Rebuilt, rec lease.Record) (string, claimKind) {
	if addr, ok := rec.Addr(); ok {
		if holder, kind := addressHolder(rb, rec, addr); kind != claimNone {
			return holder, kind
		}
	}
	return acquisitionInFlight(rb, rec)
}

// claimKind is WHY a held address is not going back, and the three
// values are three different facts about the segment.
//
// They are separate because the counter and the log line have to be
// separate. `releases_reclaimed` is the option's promise kept -- a
// container came back inside the window and got its address -- and only
// claimLive is that. claimNewerHold is one address stopped twice, where
// the newer record carries its own deadline and will decide it.
// claimInFlight is the one-sided cost written at acquisitionInFlight
// below: an address given up to the server's clock because an exchange
// under the same key might be about to be handed it.
type claimKind int

const (
	claimNone claimKind = iota
	claimLive
	claimNewerHold
	claimInFlight
)

// claimReasons is what the operator reads, one sentence per kind. A
// single "claimed back" line for all three would make the two that are
// not a reclaim unreadable in the log as well as in the counter.
var claimReasons = map[claimKind]string{
	claimLive:      "A container is running on this address, so it was claimed back inside the restart window and nothing is handed back",
	claimNewerHold: "A newer record holds this same address with its own deadline, so this record is closed and the newer one decides when the address goes back",
	claimInFlight:  "An address acquisition is in flight under this endpoint's key, so this address is left to expire instead of being handed back from under it",
}

// addressHolder is the address arm: another record in the same scope
// carrying the same address, either live now or opened after this one.
//
// TWO KINDS OF HOLDER AND BOTH MATTER. A live record -- the phases a
// RequestAddress replay may be answered from -- is a container that has
// the address at this moment, which is the bridge and macvlan restart
// case: the tombstone was consumed, a new record was opened under the
// inherited MAC and the exchange wrote this same address into it. A
// NEWER RETAINED record is a second stop of the same address, and it
// carries its own deadline; handing the address back from the older one
// would send the datagram twice and would send the first one while the
// newer record's own window is still open for a restart to claim.
func addressHolder(rb lease.Rebuilt, rec lease.Record, addr netip.Addr) (string, claimKind) {
	matches := rb.ByScopeAddr(rec.Scope, addr)
	self := -1
	for i, m := range matches {
		if m.ID == rec.ID {
			self = i
		}
	}
	for i, m := range matches {
		if m.ID == rec.ID {
			continue
		}
		if ipamPhaseAnswers(m.Phase) {
			return m.ID, claimLive
		}
		if i > self && m.Phase != lease.PhaseClosed {
			return m.ID, claimNewerHold
		}
	}
	return "", claimNone
}

// acquisitionInFlight is the second arm, and it is the one the settle
// alone cannot close.
//
// A record opened after this one under the same scope and MAC that
// holds NO address yet is an inheritance in progress: CreateEndpoint
// consumed the tombstone, opened the record and has not finished the
// one-shot exchange. The address this pass is looking at is one of the
// two the server may hand that exchange, so it must not be declared
// free while the exchange is running.
//
// A record under the same MAC that holds a DIFFERENT address is NOT a
// claim: that restart was given something else and this address is
// genuinely idle. Only "no address yet" defers.
//
// STATE THE COST, because this arm can be wrong in one direction. If
// the exchange in flight lands on a different address, this address is
// closed without a release and is left to expire on the server's clock
// -- the `never` outcome, for one address, once. The other direction
// would be a release for an address the server is at that moment
// handing to a running container, which is the fault the whole option
// is built to avoid.
func acquisitionInFlight(rb lease.Rebuilt, rec lease.Record) (string, claimKind) {
	matches := rb.ByScopeMAC(rec.Scope, rec.CHAddr)
	self := -1
	for i, m := range matches {
		if m.ID == rec.ID {
			self = i
		}
	}
	for i, m := range matches {
		if i <= self || m.ID == rec.ID || m.Phase == lease.PhaseClosed {
			continue
		}
		if _, ok := m.Addr(); ok {
			continue
		}
		return m.ID, claimInFlight
	}
	return "", claimNone
}

// sweepRecords is one tick: both passes, in one place, so that a test
// can drive what the ticker drives.
//
// The reservation pass runs first because it CREATES candidates for the
// second: a reservation Docker never built an endpoint for is retained
// here with a deadline, and on a `release_lease=on_remove` network that
// is the record whose address the deferred pass hands back when the
// deadline runs out. Ordered the other way the address would wait a
// whole tick longer for no reason.
func (p *Plugin) sweepRecords(now time.Time) {
	p.sweepIPAMReservations(now)
	p.sweepDeferredReleases(now)
}

// recordSweeper runs the record sweeps until the plugin shuts down.
//
// ONE TICKER FOR BOTH, because they are two questions about the same
// file asked at the same rate: which reservations Docker never created
// an endpoint for, and which held addresses nobody claimed back. A
// second goroutine on a second ticker would read the same records at a
// different instant and could act on two different views of one record.
func (p *Plugin) recordSweeper(stop <-chan struct{}) {
	t := time.NewTicker(ipamSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			p.sweepRecords(now)
		}
	}
}
