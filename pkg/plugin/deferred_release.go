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

// releaseSettle: a restart consumes the tombstone before the deadline and its record is written milliseconds later,
// so releasing at the deadline could free an address being promised (#524). With the 15 s tick, a stop reaches the
// wire 65 to 80 s later (#984).
const releaseSettle = 5 * time.Second

func (p *Plugin) sweepDeferredReleases(now time.Time) int {
	sent, _ := p.handRetainedRecordsBack("", func(rec lease.Record) bool {
		return !rec.Deadline.IsZero() && !now.Before(rec.Deadline.Add(releaseSettle))
	})
	return sent
}

// releaseNetworkRecords releases at once: `docker network rm` deletes the options and tombstones the sweep needs,
// so held addresses would stay leased until the server's own expiry (#984).
func (p *Plugin) releaseNetworkRecords(networkID string) int {
	sent, undecided := p.handRetainedRecordsBack(networkID, func(lease.Record) bool { return true })

	// The options file is deleted below, so a record whose options cannot be read is warned about, not retried (#984).
	if undecided > 0 {
		log.WithFields(log.Fields{
			"network": networkID,
			"held":    undecided,
		}).Warn("This network's stored options could not be read while it was being removed, so its held addresses could not be handed back; they stay leased until the server's lease expires")
	}
	return sent
}

func (p *Plugin) handRetainedRecordsBack(networkID string, due func(lease.Record) bool) (sent int, undecided int) {
	if p.records == nil {
		return 0, 0
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).Warn("Could not read the lease records back; no held address can be handed back this pass")
		return 0, 0
	}

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

// loadReleaseOptions returns nil when the options cannot be read, which leaves the record for a later pass, not
// `never` (#984).
func loadReleaseOptions(networkID string) *DHCPNetworkOptions {
	o, err := loadOptions(networkID)
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Debug("A held address belongs to a network whose options cannot be read; leaving the record as it is")
		return nil
	}
	return &o
}

// handOneRecordBack makes one attempt and then closes the record whatever happened, as on_stop does (#962, #984).
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
		// Only a live holder counts as a reclaim; the other two kinds are not a restart (#984).
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

// recordClaimedBack keys on the address, not the MAC: a --mac-address container restarting after the window gets a
// different address. Newer means later in the slice, since Rebuild returns records in creation order (#984).
func recordClaimedBack(rb lease.Rebuilt, rec lease.Record) (string, claimKind) {
	if addr, ok := rec.Addr(); ok {
		if holder, kind := addressHolder(rb, rec, addr); kind != claimNone {
			return holder, kind
		}
	}
	return acquisitionInFlight(rb, rec)
}

type claimKind int

const (
	claimNone claimKind = iota
	claimLive
	claimNewerHold
	claimInFlight
)

var claimReasons = map[claimKind]string{
	claimLive:      "A container is running on this address, so it was claimed back inside the restart window and nothing is handed back",
	claimNewerHold: "A newer record holds this same address with its own deadline, so this record is closed and the newer one decides when the address goes back",
	claimInFlight:  "An address acquisition is in flight under this endpoint's key, so this address is left to expire instead of being handed back from under it",
}

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

// acquisitionInFlight: a newer record under the same MAC with no address yet may be handed this one, so it is not
// released. If that exchange lands elsewhere, this address expires on the server's clock once (#984).
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

// The reservation pass runs first because it creates candidates for the deferred pass (#984).
func (p *Plugin) sweepRecords(now time.Time) {
	p.sweepIPAMReservations(now)
	p.sweepDeferredReleases(now)
}

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
