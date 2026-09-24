// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// ipamReserveBudget is the daemon's `docker plugin enable --timeout` (default 30s, moby plugin/manager_linux.go),
// not the network's lease_timeout. Past it the daemon re-sends the call with an already-drained body
// (moby pkg/plugins/client.go callWithRetry), which fails as a parse error (#110).
func ipamReserveBudget() time.Duration { return pluginCallBudget - pluginCallMargin }

// ipamLeaseTimeout caps lease_timeout to the reserve budget and announces the cap; the default 34s outlives the
// daemon's wait. The cap never crosses CheckLeaseTimeout's floor; ipamLeaseTimeoutFloorHolds checks it (#110).
func (p *Plugin) ipamLeaseTimeout(opts DHCPNetworkOptions, poolID string) time.Duration {
	timeout := defaultLeaseTimeout
	if opts.LeaseTimeout != 0 {
		timeout = opts.LeaseTimeout
	}
	budget := ipamReserveBudget()
	if timeout <= budget {
		return timeout
	}
	log.WithFields(log.Fields{
		"pool":          poolID,
		"lease_timeout": timeout,
		"capped_to":     budget,
	}).Info("Capping lease_timeout to the daemon's plugin-call budget for this reservation; a longer wait would be answered to nobody")
	return budget
}

// ipamReserveLinkNames names both halves of the reservation link: IFNAMSIZ is 16 with the terminator, so 15
// characters is the ceiling, and a peer name glued on at LinkAdd failed with ERANGE (#110).
func ipamReserveLinkNames() (name, peer string, err error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	h := hex.EncodeToString(b[:])
	return "dh-ipam-" + h, h + "-ipam-dh", nil
}

type ipamReservation struct {
	done chan struct{}

	addr    netip.Prefix
	info    dhcp.Info
	record  string
	err     error
	started time.Time
	// rebound marks a record taken over from a removed endpoint, which a refused endpoint hands back (#1036).
	rebound bool
}

// ipamReserveKey is the pool and MAC pair, since two networks on one host can be handed the same generated MAC.
func ipamReserveKey(poolID string, mac net.HardwareAddr) string {
	return poolID + "\x00" + mac.String()
}

// ipamReserves is the in-flight half of one MAC to one exchange; ipamLiveRecordForMAC is the settled half. A DHCP
// server files its lease per hardware address, so a second exchange under one MAC leaves a second lease no
// DHCPRELEASE ever frees (#962). The second call comes from two endpoints with one operator-set MAC, since the
// daemon's re-send carries no body and is refused first; it is refused, not parked (#110).
type ipamReserves struct {
	mu sync.Mutex
	m  map[string]*ipamReservation
}

func newIPAMReserves() *ipamReserves { return &ipamReserves{m: map[string]*ipamReservation{}} }

func (s *ipamReserves) begin(key string, now time.Time) (*ipamReservation, bool) {
	if s == nil {
		return &ipamReservation{done: make(chan struct{}), started: now}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[key]; ok {
		return r, false
	}
	r := &ipamReservation{done: make(chan struct{}), started: now}
	s.m[key] = r
	return r, true
}

func (s *ipamReserves) finish(key string, r *ipamReservation, out ipamReservation, err error) {
	r.addr, r.info, r.record, r.err, r.rebound = out.addr, out.info, out.record, err, out.rebound
	close(r.done)
	if s == nil {
		return
	}
	if err != nil {
		// A failed reservation is removed, so the next attempt runs a fresh exchange.
		s.mu.Lock()
		delete(s.m, key)
		s.mu.Unlock()
	}
}

func (s *ipamReserves) take(key string) (*ipamReservation, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[key]
	if !ok {
		return nil, false
	}
	select {
	case <-r.done:
	default:
		return nil, false
	}
	delete(s.m, key)
	return r, true
}

func (s *ipamReserves) inFlight(key string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[key]
	if !ok {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

func (s *ipamReserves) stale(now time.Time, age time.Duration) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, r := range s.m {
		select {
		case <-r.done:
		default:
			continue
		}
		if r.err == nil && now.Sub(r.started) > age {
			out = append(out, k)
		}
	}
	return out
}

func (s *ipamReserves) len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// ipamReserveAddress runs CreateEndpoint's exchange on a temporary link carrying libnetwork's MAC and writes a
// RESERVED record, so CreateEndpoint binds to this lease. The parent cannot host it: the library binds by ifindex
// without promiscuous mode, so a server unicasting its OFFER to the asked MAC is never heard (#110).
func (p *Plugin) ipamReserveAddress(ctx context.Context, networkID string, sn storedNetwork, mac net.HardwareAddr, requestedIP string) (*ipamReservation, error) {
	key := ipamReserveKey(sn.Binding.PoolID, mac)
	res, mine := p.ipamReserves.begin(key, time.Now())
	if !mine {
		return nil, p.refuseDuplicateMAC(networkID, mac, "an address request under it is still running")
	}

	out, err := p.runIPAMReserve(ctx, networkID, sn, mac, requestedIP)
	p.ipamReserves.finish(key, res, out, err)
	return res, err
}

// refuseDuplicateMAC is the one refusal both halves return. A previous endpoint's address comes back only while
// its tombstone is the single re-bind candidate, and an unused reservation is freed within tombstoneTTL plus one
// ipamSweepInterval (#110).
func (p *Plugin) refuseDuplicateMAC(networkID string, mac net.HardwareAddr, held string) error {
	p.ipamReserveDuplicateMAC.Add(1)
	log.WithFields(log.Fields{
		"network": shortID(networkID),
		"mac":     mac.String(),
		"held":    held,
	}).Warn("A second address request arrived for a hardware address this network is already leasing for; refusing it, because answering it would give two endpoints one address")
	return fmt.Errorf("%w: this network is already leasing an address for the hardware address %v, so a second endpoint cannot be given one (%v). A DHCP server files its lease per hardware address, and both endpoints would end up holding the same address. Give each container its own --mac-address, or leave it unset and Docker generates one per endpoint. If this is one container being started again, remove its previous endpoint first: the address comes back to it only while that endpoint is the one recently-removed endpoint on this network. An address request that was answered and never became an endpoint is not freed for as long as %v",
		util.ErrIPAM, mac, held, tombstoneTTL+ipamSweepInterval)
}

func (p *Plugin) runIPAMReserve(ctx context.Context, networkID string, sn storedNetwork, mac net.HardwareAddr, requestedIP string) (ipamReservation, error) {
	var none ipamReservation
	opts := sn.Options
	mode := opts.effectiveMode()

	budget := p.ipamLeaseTimeout(opts, sn.Binding.PoolID)
	ctx, cancel := context.WithTimeout(ctx, ipamReserveBudget())
	defer cancel()

	name, peer, err := ipamReserveLinkNames()
	if err != nil {
		return none, fmt.Errorf("failed to name a reservation link: %w", err)
	}

	remove, err := ipamAddReserveLink(p, ctx, name, peer, mode, opts, mac)
	if err != nil {
		return none, err
	}
	defer remove()

	clientID := resolveClientID(opts, "", mac)
	identity := dhcp.ClientIdentity(clientID)

	// Exactly one live tombstone supplies the address and the client-id the server filed it under; with more than one,
	// RequestAddress carries no hostname or endpoint id to choose on, so the server decides and it is counted (#110).
	recordID, rebindAddr, rebindIdentity := p.ipamRebindCandidate(networkID, mac)
	rebound := recordID != ""
	requestedIP, demanded := ipamExchangeAddresses(requestedIP, rebindAddr)
	clientID = ipamExchangeClientID(clientID, rebindIdentity)
	if recordID == "" {
		recordID = p.recordReserved(networkID, mac, identity)
	}

	giveUp := func(keepTheWindow bool) { p.ipamGiveUpAttempt(recordID, keepTheWindow, time.Now()) }

	pol, err := resolveServerPolicy(opts)
	if err != nil {
		giveUp(rebound)
		return ipamReservation{record: recordID}, err
	}
	base := dhcp.DHCPClientOptions{
		FQDN:        opts.fqdnMode(),
		ClientID:    clientID,
		VendorClass: opts.VendorClass,
		MAC:         mac,
		Records:     p.records,
		RecordID:    recordID,
		RequestedIP: requestedIP,
	}
	if err := p.conflictWiring(&base, opts, roleAcquire, networkID, "", false); err != nil {
		giveUp(rebound)
		return ipamReservation{record: recordID}, err
	}

	info, _, err := p.acquireWithPolicy(ctx, name, pol, false, budget, "", base)
	if err != nil {
		giveUp(rebound)
		return none, fmt.Errorf("failed to reserve an address for %v via DHCP within %v: %w", mac, budget, err)
	}

	res, err := ipamAcceptedReservation(info, recordID, sn.Binding.Pool, demanded)
	if err != nil {
		// This exit does not keep the window: the record holds an ACKed address the plugin refused, and offering it
		// back would be refused the same way (#110).
		giveUp(false)
		return none, err
	}
	res.rebound = rebound
	return res, nil
}

// ipamAcceptedReservation is the only constructor of the returned reservation, so the acceptance rules cannot be
// skipped. The pool rule is checked first: when an ACK breaks both, a new --ip would not help (#110).
func ipamAcceptedReservation(info dhcp.Info, recordID, pool, demanded string) (ipamReservation, error) {
	var none ipamReservation
	got, err := netip.ParsePrefix(info.IP)
	if err != nil {
		return none, fmt.Errorf("the DHCP server answered %q, which is not an address with a prefix: %w", info.IP, util.ErrIPAM)
	}
	if err := ipamACKInPool(got.Addr(), pool); err != nil {
		return none, err
	}
	if err := ipamACKIsTheOneAsked(got.Addr(), demanded); err != nil {
		return none, err
	}
	return ipamReservation{addr: got, info: info, record: recordID}, nil
}

// ipamExchangeAddresses separates a demand from a preference: `--ip` must be what the ACK equals, while a
// tombstone's address is only asked for, and a different answer is the documented limit (#110).
func ipamExchangeAddresses(requestedIP, rebindAddr string) (ask, demand string) {
	if requestedIP != "" {
		return requestedIP, requestedIP
	}
	return rebindAddr, ""
}

// ipamExchangeClientID sends the record's option-61 identity on a re-bind, since Docker mints a fresh MAC per
// endpoint and a MAC-derived client-id is a stranger to the server, measured as a restart moving to a new address.
// An identity this chassis did not write keeps the fresh one; a changed client_id option waits for the next
// fresh reservation (#110).
func ipamExchangeClientID(fresh, rebindIdentity []byte) []byte {
	if payload, ok := dhcp.ClientIDPayload(rebindIdentity); ok {
		return payload
	}
	return fresh
}

// ipamGiveUpAttempt ends a record with no accepted reservation. ipamRebindCandidate clears the tombstone deadline
// before any packet goes out, so an attempt that re-bound retains it with a fresh deadline; otherwise an outage
// would spend the restarted container's address stability. The exit holding a refused ACK passes false (#1047).
func (p *Plugin) ipamGiveUpAttempt(recordID string, keepTheWindow bool, now time.Time) {
	if keepTheWindow {
		p.recordRetained(recordID, now.Add(tombstoneTTL))
		return
	}
	p.closeRecord(recordID)
}

// ipamGiveUpRecord is the one give-up for an accepted reservation. A record holding a lease is retained, since
// without a DHCPRELEASE (#962) a fresh DISCOVER would leave two leases; a record holding nothing is closed, since
// Tombstones never checks for an address and an empty candidate makes a real one ambiguous (#110).
func (p *Plugin) ipamGiveUpRecord(recordID string, now time.Time) {
	if p.ipamRecordHoldsLease(recordID, now) {
		p.recordRetained(recordID, now.Add(tombstoneTTL))
		return
	}
	p.closeRecord(recordID)
}

func (p *Plugin) ipamRecordHoldsLease(recordID string, now time.Time) bool {
	if p.records == nil || recordID == "" {
		return false
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("record", recordID).
			Warn("Could not read the lease records while giving up a reservation; closing it rather than laying a tombstone with no address on it")
		return false
	}
	rec, ok := rb.ByID(recordID)
	if !ok {
		return false
	}
	_, holds := rec.Resume(now)
	return holds
}

// ipamACKIsTheOneAsked refuses an ACK for another address than `--ip`: option 50 is a request, and libnetwork
// adopts whatever address the driver returns (moby libnetwork/endpoint.go), so docker inspect would show B for
// `--ip A`. A re-bind's address is a preference and is not checked here (#110).
func ipamACKIsTheOneAsked(got netip.Addr, demanded string) error {
	if demanded == "" {
		return nil
	}
	want, err := netip.ParseAddr(demanded)
	if err != nil {
		return fmt.Errorf("the requested address %q is not an address: %w", demanded, util.ErrIPAM)
	}
	if got == want {
		return nil
	}
	return fmt.Errorf("--ip asked for %v and the DHCP server answered %v instead; the address was not taken and that lease is left to expire. The server decides: %v may be reserved for another client, already leased, or outside the range it hands out. Ask for an address the server will grant this client, or create the container without --ip: %w",
		want, got, want, util.ErrIPAM)
}

// ipamACKInPool refuses an ACK outside the subnet the user typed, which libnetwork's pool check would fail at every
// daemon restart; pool 0.0.0.0/0 accepts all (#110).
func ipamACKInPool(addr netip.Addr, pool string) error {
	if pool == "" || pool == ipamAnyPool {
		return nil
	}
	p, err := netip.ParsePrefix(pool)
	if err != nil {
		return fmt.Errorf("this network's pool %q is not a prefix: %w", pool, util.ErrIPAM)
	}
	if p.Contains(addr) {
		return nil
	}
	return fmt.Errorf("the DHCP server offered %v, which is outside this network's subnet %v; the address was not taken and the lease is left to expire. Either widen --subnet to the range the server hands out, or create the network without --subnet: %w",
		addr, p, util.ErrIPAM)
}

// addIPAMReserveLink holds the macvlan parent gate until the link is deleted, since a parent registers one
// rx_handler and the link lives a whole DHCP round trip; remove() runs LinkDel, then Unlock (#110).
func (p *Plugin) addIPAMReserveLink(ctx context.Context, name, peer, mode string, opts DHCPNetworkOptions, mac net.HardwareAddr) (func(), error) {
	if mode == ModeMacvlan || mode == ModeIPvlan {
		guard := p.lockParent(ctx, opts.Parent, mode, "ipam_reserve")
		parent, err := validateParentForChild(opts.Parent)
		if err != nil {
			guard.Unlock()
			return nil, err
		}
		link := newProbeLink(mode, name, parent.Attrs().Index, mac)
		if err := addChildLink(guard, link); err != nil {
			guard.Unlock()
			return nil, explainChildLinkAdd(err, mode, opts.Parent, parent.Attrs().Index)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			_ = netlink.LinkDel(link)
			guard.Unlock()
			return nil, fmt.Errorf("failed to bring the reservation link up: %w", err)
		}
		return func() {
			if err := netlink.LinkDel(link); err != nil {
				log.WithError(err).WithField("link", name).Warn("Reservation link cleanup failed; remove it with `ip link del`")
			}
			guard.Unlock()
		}, nil
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge interface: %w", err)
	}
	veth := ipamReserveVeth(name, peer, mac)
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("failed to create the reservation veth pair: %w", err)
	}
	remove := func() {
		if err := netlink.LinkDel(veth); err != nil {
			log.WithError(err).WithField("link", name).Warn("Reservation link cleanup failed; remove it with `ip link del`")
		}
	}
	peerLink, err := netlink.LinkByName(peer)
	if err != nil {
		remove()
		return nil, fmt.Errorf("failed to find the reservation veth peer: %w", err)
	}
	for _, l := range []netlink.Link{veth, peerLink} {
		if err := netlink.LinkSetUp(l); err != nil {
			remove()
			return nil, fmt.Errorf("failed to bring the reservation link up: %w", err)
		}
	}
	// The peer is the bridge port, not the link the client runs on.
	if err := netlink.LinkSetMaster(peerLink, bridge); err != nil {
		remove()
		return nil, fmt.Errorf("failed to attach the reservation link to the bridge: %w", err)
	}
	return remove, nil
}

// ipamReserveVeth puts the MAC on the half the DHCP client runs on, since a frame sent on a bridge port leaves
// through the port into its partner. Reversed, integration run 34604958124 (2026-09-11) showed the bridge-mode
// DISCOVER never reaching the fixture. CreateEndpoint's hostLink/ctrLink have the same shape (#110).
func ipamReserveVeth(name, peer string, mac net.HardwareAddr) *netlink.Veth {
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.HardwareAddr = mac
	return &netlink.Veth{LinkAttrs: la, PeerName: peer}
}

func (p *Plugin) recordReserved(networkID string, mac net.HardwareAddr, identity []byte) string {
	if p.records == nil {
		return ""
	}
	id := newRecordID()
	if id == "" {
		return ""
	}
	if err := p.records.Reserved(id, networkID, mac, identity); err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not open the reservation's lease record; this address will not survive a plugin restart")
		return ""
	}
	return id
}

// ipamRebindCandidate re-binds exactly one live tombstone; with more than one it counts and logs, since
// RequestAddress carries no hostname or endpoint id to choose on (#110).
func (p *Plugin) ipamRebindCandidate(networkID string, mac net.HardwareAddr) (string, string, []byte) {
	if p.records == nil {
		return "", "", nil
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).Warn("Could not read the lease records; this reservation gets a fresh identity")
		return "", "", nil
	}
	candidates := p.ipamUnheldTombstones(networkID, rb.Tombstones(networkID, time.Now()))
	if len(candidates) == 0 {
		return "", "", nil
	}
	if len(candidates) > 1 {
		p.ipamRebindAmbiguous.Add(1)
		log.WithFields(log.Fields{
			"network":    shortID(networkID),
			"candidates": len(candidates),
		}).Info("More than one recently-removed endpoint on this network could claim this address request; the DHCP server decides and the address can change")
		return "", "", nil
	}
	rec := candidates[0]
	addr, ok := rec.Addr()
	if !ok {
		return "", "", nil
	}
	if err := p.records.Rebound(rec.ID, mac); err != nil {
		log.WithError(err).WithField("record", rec.ID).Warn("Could not re-bind the recently-removed endpoint's record; this reservation gets a fresh identity")
		return "", "", nil
	}
	return rec.ID, addr.String(), rec.Identity
}

// ipamUnheldTombstones drops a candidate whose MAC and address a live endpoint of this process still holds: the
// server keeps one binding per client-id, so re-binding it would take the running container's lease (#1047).
func (p *Plugin) ipamUnheldTombstones(networkID string, candidates []lease.Record) []lease.Record {
	kept := candidates[:0:0]
	for _, rec := range candidates {
		addr, ok := rec.Addr()
		if ok && p.ipamEndpointHolds(net.HardwareAddr(rec.CHAddr), addr.String()) {
			log.WithFields(log.Fields{
				"network": shortID(networkID),
				"record":  rec.ID,
			}).Info("A recently-removed endpoint's address is still held by a running endpoint on this network; it is not offered to this request")
			continue
		}
		kept = append(kept, rec)
	}
	return kept
}

const ipamSweepInterval = 15 * time.Second

// sweepIPAMReservations retains each reservation Docker never turned into an endpoint, since only a retained
// record carries a deadline; the tombstone then expires and the lease runs out at the server (#110, #962).
func (p *Plugin) sweepIPAMReservations(now time.Time) int {
	swept := 0
	for _, key := range p.ipamReserves.stale(now, tombstoneTTL) {
		r, ok := p.ipamReserves.take(key)
		if !ok {
			continue
		}
		if r.record != "" {
			p.recordRetained(r.record, now.Add(tombstoneTTL))
			swept++
			log.WithField("record", r.record).
				Info("An address was reserved for an endpoint Docker never created; retaining it so a retry can claim it back")
		}
	}
	return swept
}

// retainOrphanedReservations retains every RESERVED record at start-up, since no other process can bind it (#110).
func retainOrphanedReservations(records *dhcp.Records, now time.Time) int {
	if records == nil {
		return 0
	}
	rb, err := records.Rebuilt()
	if err != nil {
		log.WithError(err).Warn("Could not read the lease records at start-up; orphaned reservations stay as they are")
		return 0
	}
	retained := 0
	for _, rec := range rb.Records {
		if rec.Phase != lease.PhaseReserved {
			continue
		}
		if err := records.Retained(rec.ID, now.Add(tombstoneTTL)); err != nil {
			log.WithError(err).WithField("record", rec.ID).Warn("Could not retain an orphaned reservation")
			continue
		}
		retained++
	}
	if retained > 0 {
		log.WithField("count", retained).
			Info("Retained reservations left behind by a previous plugin process; a restarting container can claim its address back")
	}
	return retained
}

// ipamListedMACs refuses the whole set on one unparseable entry, since the stranded-record rule acts on absence.
// `ep-<id>` placeholders are kept: libnetwork stores an endpoint before its sandbox and still retries Join (#1047).
func ipamListedMACs(containers map[string]dNetwork.EndpointResource) ([]net.HardwareAddr, bool) {
	out := make([]net.HardwareAddr, 0, len(containers))
	for _, info := range containers {
		mac, err := net.ParseMAC(info.MacAddress)
		if err != nil {
			log.WithFields(log.Fields{
				"endpoint": shortID(info.EndpointID),
				"mac":      info.MacAddress,
			}).Warn("An endpoint in this network reports a hardware address that cannot be read, so records left behind by a previous plugin process are left alone on it")
			return nil, false
		}
		out = append(out, mac)
	}
	return out, true
}

// giveUpStrandedIPAMRecords gives up a CREATED record left by a process that ended between RequestAddress and
// CreateEndpoint: it has no tombstone, still answers lookups, and no sweep hands it back. Both keys are needed:
// an earlier process wrote it, since a running endpoint can sit in CREATED across a restart until setupClient
// binds, and the engine does not list its MAC, since a start in this process is listed only after
// CreateEndpoint returns. An engine returning a short list after a store read error is indistinguishable, and an
// IPAM handler may not ask Docker for a second source (#1047).
func (p *Plugin) giveUpStrandedIPAMRecords(networkID string, listed []net.HardwareAddr, now time.Time) int {
	if p.records == nil {
		return 0
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not read the lease records; addresses left behind by a previous plugin process stay where they are")
		return 0
	}
	instance := p.records.Instance()
	given := 0
	for _, rec := range rb.Records {
		if rec.Scope != networkID || rec.Phase != lease.PhaseCreated || rec.Instance == instance {
			continue
		}
		if ipamMACIsListed(listed, rec.CHAddr) {
			continue
		}
		_, held := rec.Resume(now)
		p.ipamGiveUpRecord(rec.ID, now)
		given++
		p.ipamStrandedRecords.Add(1)
		log.WithFields(log.Fields{
			"network":  shortID(networkID),
			"record":   rec.ID,
			"retained": held,
		}).Info("A previous plugin process left this endpoint's lease record behind and no endpoint on this network claims it; giving it up so a container restarting can claim the address back")
	}
	return given
}

func ipamMACIsListed(listed []net.HardwareAddr, chaddr []byte) bool {
	for _, mac := range listed {
		if bytes.Equal(mac, chaddr) {
			return true
		}
	}
	return false
}
