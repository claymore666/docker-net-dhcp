// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/runtime"
	cerrdefs "github.com/containerd/errdefs"
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
	p.forgetSettledHandBacks(rb)

	// DeleteNetwork runs inside the daemon's own removal call, so only the periodic pass asks Docker (#1158).
	askDocker := networkID == ""
	opts := map[string]*DHCPNetworkOptions{}
	removed := map[string]bool{}
	closed := map[string]int{}
	// One probe per next hop per pass: a silent one costs its bound once, not once per lease, inside Docker's call (#1288).
	resolved := runtime.NewLinkResolveCache()
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
			o, removed[id] = p.releaseOptions(id, askDocker)
			opts[id] = o
		}
		if removed[id] {
			p.closeRecord(rec.ID)
			closed[id]++
			continue
		}
		if o == nil {
			undecided++
			continue
		}
		if !o.releasesOnRemove() {
			continue
		}
		if p.handOneRecordBack(rec.ID, due, *o, id, v6, resolved) {
			sent++
		}
	}
	for id, n := range closed {
		log.WithFields(log.Fields{
			"network": shortID(id),
			"closed":  n,
		}).Info("These held addresses belong to a network Docker no longer has, so their records are closed and nothing is sent")
	}
	return sent, undecided
}

// releaseOptions returns nil when the options cannot be read, which leaves the record for a later pass, not `never`
// (#984). An absent file is not a removal on its own: a null-mode network outlives a failed write or backfill
// (network.go saveNetworkAndBind, netOptionsRaw), so removed needs Docker's not-found as well (#1158).
func (p *Plugin) releaseOptions(networkID string, askDocker bool) (o *DHCPNetworkOptions, removed bool) {
	stored, err := loadOptions(networkID)
	if err == nil {
		return &stored, false
	}
	if askDocker && errors.Is(err, fs.ErrNotExist) && p.networkGone(networkID) {
		return nil, true
	}
	log.WithError(err).WithField("network", shortID(networkID)).
		Debug("A held address belongs to a network whose options cannot be read; leaving the record as it is")
	return nil, false
}

// networkGone counts only not-found, as recovery does; an unreachable or slow daemon leaves the record (#1158).
func (p *Plugin) networkGone(networkID string) bool {
	if p.docker == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), recoveryPerNetworkTimeout)
	defer cancel()
	_, err := inspectNetwork(ctx, p.docker, networkID)
	return cerrdefs.IsNotFound(err)
}

// handOneRecordBack makes one attempt and then closes the record whatever happened, as on_stop does (#962, #984). It
// judges the record on a read taken once the record is its own, not on the pass's: a claim written while an earlier
// record was on the wire is seen, and the sweep and DeleteNetwork send it once between them (#1237).
func (p *Plugin) handOneRecordBack(id string, due func(lease.Record) bool, opts DHCPNetworkOptions, networkID string, v6 bool,
	resolved *runtime.LinkResolveCache) bool {
	if !p.takeForHandBack(id) {
		return false
	}
	attempted := false
	defer func() { p.finishHandBack(id, attempted) }()
	if p.runningOnHeldRecord(id) {
		log.WithField("record", id).
			Debug("A container resumed this held record and its bind could not be written, so nothing is handed back while it runs")
		return false
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("record", id).Warn("Could not read the lease records back; this held address is left for the next pass")
		return false
	}
	rec, ok := rb.ByID(id)
	if !ok || rec.Phase != lease.PhaseRetained || !due(rec) {
		return false
	}
	attempted = true

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
	out := releaseFromRecord(rec, opts, v6, held, resolved, fields)
	p.countRelease(v6, out == releaseSent)
	announceReleaseOutcome(log.WithFields(fields).WithField("outcome", string(out)), out)
	p.closeRecord(rec.ID)
	return out == releaseSent
}

// takeForHandBack waits while another pass holds the record, and refuses one whose attempt was already made: a
// record whose close did not land would otherwise be sent again on every tick (#1237).
func (p *Plugin) takeForHandBack(id string) bool {
	for {
		p.handBackMu.Lock()
		if p.handingBack == nil {
			p.handingBack = make(map[string]chan struct{})
		}
		done, taken := p.handingBack[id]
		if !taken {
			p.handingBack[id] = make(chan struct{})
			p.handBackMu.Unlock()
			return true
		}
		select {
		case <-done:
			p.handBackMu.Unlock()
			return false
		default:
		}
		p.handBackMu.Unlock()
		<-done
	}
}

// finishHandBack keeps an attempted record marked until a read shows it no longer held, and frees the rest (#1237).
func (p *Plugin) finishHandBack(id string, attempted bool) {
	p.handBackMu.Lock()
	defer p.handBackMu.Unlock()
	done := p.handingBack[id]
	if !attempted {
		delete(p.handingBack, id)
	}
	close(done)
}

func (p *Plugin) forgetSettledHandBacks(rb lease.Rebuilt) {
	p.handBackMu.Lock()
	defer p.handBackMu.Unlock()
	for id, done := range p.handingBack {
		select {
		case <-done:
		default:
			continue
		}
		if rec, ok := rb.ByID(id); !ok || rec.Phase != lease.PhaseRetained {
			delete(p.handingBack, id)
		}
	}
}

// markRunningOnHeldRecord covers a Join that resumed a held record and could not write the bind that ends its hold,
// the full disk that also refused CreateEndpoint's own record: the record still reads as held (#1237).
func (p *Plugin) markRunningOnHeldRecord(id string) {
	p.handBackMu.Lock()
	defer p.handBackMu.Unlock()
	if p.runningOnHeld == nil {
		p.runningOnHeld = make(map[string]bool)
	}
	p.runningOnHeld[id] = true
}

func (p *Plugin) runningOnHeldRecord(id string) bool {
	p.handBackMu.Lock()
	defer p.handBackMu.Unlock()
	return p.runningOnHeld[id]
}

// clearRunningOnHeldRecord runs at the endpoint's teardown, which lays a new hold or ends the record.
func (p *Plugin) clearRunningOnHeldRecord(id string) {
	p.handBackMu.Lock()
	defer p.handBackMu.Unlock()
	delete(p.runningOnHeld, id)
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
	p.compactRecords(now)
}

// compactRecords runs last, so the records this tick closed are counted from now; tombstoneTTL is the age past
// which no reader of a closed or expired record is still in flight (#1182).
func (p *Plugin) compactRecords(now time.Time) {
	if p.records == nil {
		return
	}
	done, err := p.records.CompactIfDue(now, tombstoneTTL)
	if err != nil {
		log.WithError(err).WithField("file", p.records.Path()).Error("Could not compact the lease record file; the next sweep retries")
		return
	}
	if done {
		log.Debug("Compacted the lease record file")
	}
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
