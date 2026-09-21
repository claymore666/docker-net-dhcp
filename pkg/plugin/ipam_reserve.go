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

// ipamReserveBudget is the whole time a RequestAddress may take.
//
// IT IS THE DAEMON'S BUDGET AND NOT THE NETWORK'S lease_timeout. The
// IPAM client the daemon builds carries `docker plugin enable --timeout`
// (default 30s, moby plugin/manager_linux.go SetTimeout); when it
// expires the daemon has already stopped listening and re-sends the
// call after a backoff. The re-send carries NO BODY -- moby hands the
// same, already-drained reader to every attempt (pkg/plugins/client.go,
// callWithRetry) -- so a reserve that overruns does not produce a late
// answer: it produces a refusal the operator reads as a parse error,
// and the container start fails. Same
// arithmetic and the same two constants as the DHCPv6 half of
// CreateEndpoint, which is the other call that shares a deadline with a
// caller it cannot see.
func ipamReserveBudget() time.Duration { return pluginCallBudget - pluginCallMargin }

// ipamLeaseTimeout is what the reserve gives one DHCP acquisition.
//
// A network's `lease_timeout` is capped to the daemon's budget here, and
// the cap is announced. Left uncapped, the default 34s -- which is the
// conflict-recovery window, a DECLINE plus a second full exchange --
// runs past the moment the daemon stopped listening, so the operator
// sees a timeout from Docker while the plugin is still working, and the
// daemon's bodiless re-send arrives underneath it and is refused.
//
// The cap may not cross the floor CheckLeaseTimeout enforces, because
// two guards that disagree about one number leave the tighter one
// unreachable and untested. ipamLeaseTimeoutFloorHolds is the check.
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

// ipamReserveLinkNames names BOTH halves of the temporary link one
// reservation runs on: the link itself, and the veth peer the bridge
// mode needs.
//
// Both come from here rather than the peer being spelled where the veth
// is built, because IFNAMSIZ is 16 including the terminator and 15
// printable characters is therefore the ceiling. The first edition
// named only the link -- "dh-ipam-" plus 3 random bytes, 14 characters,
// and the comment stopped there -- and glued a "-p" on at the
// LinkAdd, which is 16. Every bridge-mode reservation died with a bare
// ERANGE from netlink ("numerical result out of range"), and the
// function's own test could not see the length that failed because the
// function never produced it.
//
// The shape is vethPairNames': the prefix marks the host half, the same
// token suffixed marks the peer. 3 random bytes is the probe link's
// 16M space, and both names are 14 characters.
func ipamReserveLinkNames() (name, peer string, err error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	h := hex.EncodeToString(b[:])
	return "dh-ipam-" + h, h + "-ipam-dh", nil
}

// ipamReservation is one (PoolID, MAC) reservation: the exchange in
// flight, or its result.
type ipamReservation struct {
	done chan struct{}

	addr netip.Prefix
	info dhcp.Info
	// record is the reserve's durable product: the record
	// CreateEndpoint folds CREATE onto.
	record  string
	err     error
	started time.Time
}

// ipamReserveKey keys a reservation. The PAIR and not the MAC alone: two
// networks on one host can be handed the same generated MAC by two
// different daemons' bad luck, and the pool is what says which network
// an address belongs to.
func ipamReserveKey(poolID string, mac net.HardwareAddr) string {
	return poolID + "\x00" + mac.String()
}

// ipamReserves holds every reservation this process has answered and not
// yet seen a CreateEndpoint for.
//
// IT IS THE IN-FLIGHT HALF of keeping one hardware address to one
// exchange, and only that half. A key lives here from the moment
// RequestAddress owns an exchange until CreateEndpoint takes it, so what
// it sees is two creates racing and a create that has stalled. The
// SETTLED half -- a first container already up, a second started later
// under the same pinned MAC -- is a key this map no longer has, and
// ipamLiveRecordForMAC is what answers there.
//
// Why either half exists: a DHCP server files its lease per hardware
// address, so two exchanges under one MAC means two DISCOVERs and two
// leases at the server for what the host believes is one endpoint, and
// the second is never released -- nothing holds it, and no DHCPRELEASE
// goes on the wire for it on any value of `release_lease` (D-7, #962),
// because `on_stop` releases at Leave from the endpoint's own lease
// record and a spare exchange nothing holds has neither.
//
// The producer of a second call for one key is not the daemon re-sending
// a RequestAddress, which an earlier version of this comment claimed:
// that re-send carries no body and is refused before any handler runs
// (pkg/util's explainRequestBody carries the measurement). It is two
// ENDPOINTS. libnetwork generates a unique MAC per endpoint and copies
// an operator-set one through unchanged, so `docker run --mac-address X`
// twice on one network arrives here as one key. The second call is
// refused: parking it on the first one's result would give two endpoints
// one address.
type ipamReserves struct {
	mu sync.Mutex
	m  map[string]*ipamReservation
}

func newIPAMReserves() *ipamReserves { return &ipamReserves{m: map[string]*ipamReservation{}} }

// begin returns the reservation for key and whether THIS caller owns the
// exchange. A caller that does not own it waits on done.
func (s *ipamReserves) begin(key string, now time.Time) (*ipamReservation, bool) {
	if s == nil {
		// Nil-safe like its two siblings. An owned reservation with
		// nowhere to publish it still runs its exchange correctly; what
		// it loses is the idempotence, which a plugin with no reserve
		// set was never providing.
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

// finish publishes a reservation's result to everyone waiting on it.
func (s *ipamReserves) finish(key string, r *ipamReservation, out ipamReservation, err error) {
	r.addr, r.info, r.record, r.err = out.addr, out.info, out.record, err
	close(r.done)
	if s == nil {
		return
	}
	if err != nil {
		// A failed reservation is removed rather than remembered: the
		// next attempt must run a fresh exchange, not be handed this
		// one's error forever.
		s.mu.Lock()
		delete(s.m, key)
		s.mu.Unlock()
	}
}

// take removes and returns a completed reservation. CreateEndpoint's
// consumption.
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

// inFlight reports whether an exchange under this key is running: the
// reservation is in the map and its answer has not been written yet.
// The re-bind is folded into the journal between begin and finish, so
// a record in created with an exchange in flight belongs to that
// exchange and to nobody else.
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

// stale lists the keys of completed reservations older than age, which
// is the sweeper's population: Docker asked for an address, was given
// one, and never created the endpoint.
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

// ipamReserveAddress answers one RequestAddress that asks for a lease.
//
// It runs the same exchange CreateEndpoint runs, on a temporary link
// carrying the MAC libnetwork generated, and writes its events into a
// RESERVED record so that CreateEndpoint can bind a link to the lease
// this call acquired rather than acquiring a second one.
//
// WHY A TEMPORARY LINK AND NOT THE PARENT ITSELF. The library binds its
// packet socket by ifindex and sets no promiscuous mode, and it fills
// the link's own hardware address into the client's parameters; a server
// that unicasts its OFFER to the MAC we asked under would be sending to
// an address the parent does not carry, and the reply would never be
// seen. On macvlan that link is the same child the preflight probe
// builds; on a bridge it is the same veth pair CreateEndpoint builds.
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

// refuseDuplicateMAC is the ONE refusal both halves return, so that the
// operator reads the same sentence whichever half caught it and neither
// can drift from the other.
//
// The two boundaries are in the text on purpose. The address of a
// PREVIOUS endpoint comes back only while its tombstone is the single
// re-bind candidate on the network (ipamRebindCandidate), and a
// reservation Docker never turned into an endpoint is not freed until
// the sweeper reaps it, which is tombstoneTTL plus at most one
// ipamSweepInterval. A refusal that promised the address back without
// either boundary would send an operator into a retry loop that cannot
// succeed yet.
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

// runIPAMReserve is the exchange. Separated from the idempotence above
// so the two can be driven apart: one is concurrency, the other is
// netlink and DHCP.
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

	// The re-bind rule, before any identity is minted. Exactly one live
	// tombstone in this network supplies both its address and, because
	// the identity is write-once in the fold, the client-id the server
	// already has a lease filed under -- which is the only reason a
	// restarted container keeps its address when Docker gave it a new
	// MAC. More than one candidate and there is nothing to choose on:
	// RequestAddress carries no hostname and no endpoint id, so the
	// server decides and the ambiguity is counted rather than guessed.
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
		// THE WINDOW IS NOT KEPT HERE, and this is the one exit that
		// does not keep it. The ACK is folded onto the record before
		// either acceptance rule looks at it, so this record now holds
		// an address this plugin refused. A tombstone carrying it is
		// offered to the next container on the network, which asks for
		// it under this container's identity and is refused the same
		// way; on an on_remove network it is also a release for an
		// address that was never taken. What the window offers back is
		// an address that cannot be used here, so it is worth less
		// than either.
		giveUp(false)
		return none, err
	}
	return res, nil
}

// ipamAcceptedReservation turns an ACK into the reservation the reserve
// returns, and refuses it if either acceptance rule says no.
//
// It exists as a constructor rather than as two checks above their own
// `return ipamReservation{...}` so that skipping the checks cannot
// compile: there is no other way to build the value the caller returns.
// The two rules had a call site each, both on the success path of a
// function that needs netlink and a DHCP server to enter, so the unit
// suite could reach the rules but never their application -- and a
// deleted call is exactly the change that reads as a cleanup.
//
// The order is the operator's, not the compiler's. The pool rule (D50)
// is a property of the network they created; the --ip rule is a
// property of the container they just started. When an ACK breaks
// both, the network-level cause is the one to print, because acting on
// the other one -- picking a different --ip -- would not help.
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

// ipamExchangeAddresses splits what the exchange ASKS for from what the
// ACK must EQUAL.
//
// They are not the same question and only one of them is a demand.
// `--ip` is the operator pinning an address, and an ACK for another one
// is refused (ipamACKIsTheOneAsked). A tombstone's address is a
// PREFERENCE: it is how a restarted container keeps what it had, and
// the server answering otherwise is the documented limit -- refusing
// there would turn "your address moved" into "your container will not
// start", on the path that exists to make restarts survivable. The two
// are one line apart in the reserve, so they are decided here where a
// test can ask.
func ipamExchangeAddresses(requestedIP, rebindAddr string) (ask, demand string) {
	if requestedIP != "" {
		return requestedIP, requestedIP
	}
	return rebindAddr, ""
}

// ipamExchangeClientID is the option-61 payload ONE exchange sends, and
// on a re-bind it is not the one the MAC derives.
//
// THE RECORD'S IDENTITY IS THE ONLY THING THE SERVER RECOGNISES. In
// IPAM mode Docker mints a fresh MAC for every endpoint, including the
// one a restarted container comes back on, so the client-id derived
// from that MAC is a client the server has never seen: it asks for the
// tombstone's address as a stranger, the server declines to hand over
// another client's lease, and the container comes back on a different
// address with nothing logged. That is exactly what the lane measured
// -- came back on .54, held .82 -- while the re-bind comment claimed
// the write-once identity in the fold delivered this. The fold protects
// what the RECORD says; it puts nothing on the wire.
//
// An identity this chassis did not write (ClientIDPayload says so)
// leaves the fresh one in place rather than sending a shape no record
// describes.
//
// The network's own client_id option, if the operator changed it since
// the record was written, loses here. The record's identity is where
// the address it is offering actually lives; the new setting takes
// effect on the next fresh reservation, which is at most a tombstone
// TTL away.
func ipamExchangeClientID(fresh, rebindIdentity []byte) []byte {
	if payload, ok := dhcp.ClientIDPayload(rebindIdentity); ok {
		return payload
	}
	return fresh
}

// ipamGiveUpAttempt ends the record of an exchange that produced no
// reservation this plugin accepted, and it does not ask whether that
// record holds a lease. The attempt either never got an address or was
// handed one the acceptance rules refused, and a refused address must
// not become the candidate the next container on this network takes:
// the next MAC would ask for it under the first container's identity
// and be refused in the same way, for as long as the window is renewed.
//
// A FAILED EXCHANGE MUST NOT CONSUME THE CANDIDATE, which is what
// keepTheWindow is for, and every caller passes whether THIS attempt
// re-bound the tombstone -- except the one exit holding an ACK the
// acceptance rules refused, which passes false because the address on
// the record is no longer the one the window promised. ipamRebindCandidate writes OpRebind before any packet
// goes out, because the exchange has to run under the identity the
// server already has a lease filed under, and that fold clears the
// tombstone deadline: the record stops being a candidate the moment it
// is taken. Closing it on failure would spend the documented address
// stability on an attempt that never reached the server -- a container
// restarted on its own during a brief outage would find nothing to
// re-bind seconds later, take a fresh address, and nothing would say
// so, because ipam_rebind_ambiguous counts a different case entirely.
// Retaining it puts the tombstone back with a fresh deadline, so a
// retry inside the window finds exactly the one candidate it had, and
// an attempt that never comes expires as it would have.
//
// A record this attempt did not re-bind is closed, for the reason the
// last paragraph of ipamGiveUpRecord gives.
func (p *Plugin) ipamGiveUpAttempt(recordID string, keepTheWindow bool, now time.Time) {
	if keepTheWindow {
		p.recordRetained(recordID, now.Add(tombstoneTTL))
		return
	}
	p.closeRecord(recordID)
}

// ipamGiveUpRecord ends a record no endpoint is going to own, and the
// choice between the two ways of ending it is whether the record still
// has an address to give back.
//
// IT IS THE ONE GIVE-UP FOR AN ACCEPTED RESERVATION, and every exit
// that abandons one comes through it: every exit of createIPAMEndpoint
// after it has taken the reservation, a ReleaseAddress for a
// reservation nobody consumed, and the stranded-record rule a plugin
// restart runs. One clock read and one rule, so the window a container
// is promised cannot be 60 seconds down one path and nothing down
// another. An exchange that produced no accepted reservation ends at
// ipamGiveUpAttempt instead, and that is the whole list.
//
// A RECORD THAT HOLDS A LEASE IS RETAINED even when nothing is owed to
// it, because the lease is real at the server: the plugin asked for an
// address, was given one, and is now walking away from it. Retained, a
// retry inside the window claims it back under the same identity and
// the server hands over the same address; closed, the next attempt runs
// a fresh DISCOVER and the host holds two leases where it needs one,
// and nothing ever gives the first one back (D-7: no DHCPRELEASE).
//
// A record holding NOTHING is closed, and it has to be, because
// Tombstones filters on the phase and the deadline and never asks for
// an address (lease/rebuild.go). An address-less tombstone is therefore
// a full candidate: laid beside a real one it makes the pair ambiguous,
// and the container the real one belongs to loses its address to
// ipam_rebind_ambiguous.
func (p *Plugin) ipamGiveUpRecord(recordID string, now time.Time) {
	if p.ipamRecordHoldsLease(recordID, now) {
		p.recordRetained(recordID, now.Add(tombstoneTTL))
		return
	}
	p.closeRecord(recordID)
}

// ipamRecordHoldsLease reports whether the record still has a lease the
// server would recognise. A record it cannot read holds nothing.
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

// ipamACKIsTheOneAsked refuses an ACK for an address other than the one
// `--ip` demanded.
//
// A DHCP request carries the wanted address as option 50, which is a
// REQUEST and not a command: a server may answer with another address
// because the one asked for is reserved for a different client, or
// already leased, or outside the range it serves. libnetwork does not
// compare the driver's answer to the address it preferred -- it adopts
// whatever comes back (moby libnetwork/endpoint.go, `*address = addr`)
// -- so without this, `docker run --ip A` publishes B in `docker
// inspect` and exits 0, and the operator's pinned address is silently
// not the one the container has.
//
// Refusing costs a failed `docker run` and one lease left to expire at
// the server, which is the price ipamACKInPool already pays for D50 and
// the price `--ip` pays everywhere else. A re-bind's address is NOT
// demanded and is not checked here: that one is a preference, and the
// server choosing otherwise is the documented limit the ambiguity
// counter is about.
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

// ipamACKInPool is D50: an ACK outside the subnet the user typed is
// refused.
//
// The alternative was to take the address and record it, which costs a
// container that Docker's own store says is outside its network's
// subnet, and a replay that fails libnetwork's pool check at every
// daemon restart. A refusal costs a failed `docker run` and one lease
// left to expire at the server, which is the price `--ip` already pays.
// A network that typed no subnet has pool 0.0.0.0/0 and this check has
// nothing to say.
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

// addIPAMReserveLink builds the temporary link the exchange runs on and
// returns its removal.
//
// The macvlan half takes the parent gate and HOLDS IT UNTIL THE LINK IS
// GONE, not merely across the LinkAdd: a parent NIC registers one
// rx_handler, and this link lives across a whole DHCP round trip exactly
// as the preflight probe's does. The ordering of the two closures below
// is what gives that -- the caller's deferred remove() runs the LinkDel
// and then the Unlock, so the gate opens after the child is detached.
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
	// THE PEER IS THE BRIDGE PORT, NOT THE LINK THE CLIENT RUNS ON.
	if err := netlink.LinkSetMaster(peerLink, bridge); err != nil {
		remove()
		return nil, fmt.Errorf("failed to attach the reservation link to the bridge: %w", err)
	}
	return remove, nil
}

// ipamReserveVeth builds the reservation's veth pair with the endpoint's
// MAC on the half the DHCP CLIENT runs on -- `name`, the half every
// caller passes to the acquisition -- and nothing on the half that
// becomes the bridge port.
//
// WHICH HALF IS WHICH IS THE WHOLE FUNCTION, and the first edition had
// it backwards: the MAC went on the peer (`PeerHardwareAddr`) and the
// named half was enslaved to the bridge. Both halves of that are wrong
// and the second is fatal. A frame TRANSMITTED on a bridge port does not
// enter the bridge; it goes out of the port, which for a veth means into
// its partner. The DISCOVER therefore went to the dangling end and was
// never seen by anything on the segment, while the kernel's own IPv6
// router solicitation from the dangling end -- entering the bridge, the
// direction that does work -- reached the server and made the link look
// present. MEASURED, integration run 34604958124: main-7
// TestIPAM_NoSubnetAnswersTheAnyPool and main-8
// TestIPAM_TwoNetworksCannotShareOnePool, the fixture's log holding
// `RTR-SOLICIT(dh-itest-br2) 6e:d9:fe:bc:eb:c7` from the reservation's
// own MAC and not one DHCPDISCOVER, the reserve ending at its 26s budget
// with `context deadline exceeded`. Bridge is this plugin's default
// mode, and the two bridge networks in the suite were the only two that
// failed.
//
// The shape is CreateEndpoint's, which has always been right: the
// container half carries the MAC and runs the client, the host half is
// the bridge port (network.go, `hostLink`/`ctrLink`).
func ipamReserveVeth(name, peer string, mac net.HardwareAddr) *netlink.Veth {
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.HardwareAddr = mac
	return &netlink.Veth{LinkAttrs: la, PeerName: peer}
}

// recordReserved opens the RESERVED record for one reservation.
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

// ipamRebindCandidate is the re-bind rule: exactly one live tombstone in
// this network, consumed under the new hardware address.
//
// Returns the record id it re-bound and the address to ask for, or
// ("", "") when there is nothing to re-bind or more than one candidate.
// Ambiguity is COUNTED AND LOGGED rather than resolved: RequestAddress
// carries no hostname and no endpoint id, so there is nothing to narrow
// two candidates on, and picking one would hand an address to whichever
// container asked first. The documented limit is exactly this.
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

// ipamUnheldTombstones drops a candidate whose address a live endpoint
// of this process still holds.
//
// IT IS THE RE-BIND'S HALF OF THE RULE THAT THE IDENTITY MADE SHARP. A
// tombstone is laid by a teardown, and every teardown in this process
// takes the endpoint's fingerprint first, so a candidate that still has
// one is a record that was retained while its container kept running --
// the restart rule's own defeat row, reached from a truthful-looking
// but short endpoint list. Re-binding it used to cost the NEW container
// its address, which was self-limiting because its client then spoke as
// itself; now that the client speaks as the record, it would cost the
// OLD container its lease instead, because the server keeps one binding
// per client-id and the last exchange wins. A healthy container losing
// its address to a second one's arrival is worse than the defect this
// whole change repairs, so the candidate is skipped and the next
// container gets a fresh identity and a fresh address, which is exactly
// what it got before.
//
// The key is the pair, hardware address AND address, for the reason the
// release handler keys on the pair: two IPAM networks on one segment
// can hold the same address.
//
// It cannot see another process's endpoints. That half is the restart
// rule's two keys, and the short-list limit it documents.
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

// ipamSweepInterval is how often orphaned reservations are looked for.
const ipamSweepInterval = 15 * time.Second

// sweepIPAMReservations retains every reservation Docker asked for and
// never created an endpoint for.
//
// WITHOUT IT THE RECORD ANSWERS FOREVER. Only a RETAINED record carries a
// deadline, so a RESERVED one with a live lease and no endpoint -- the
// daemon spent every retry, or fell over between RequestAddress and
// CreateEndpoint -- would keep answering address lookups until the
// network is deleted. Retaining it with the tombstone deadline does two
// right things at once: a restart policy's next attempt re-binds the
// address the server just gave, and when nothing comes the tombstone
// expires and the lease runs out at the server on its own (no
// DHCPRELEASE, D-7).
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

// retainOrphanedReservations retains every RESERVED record found at
// start-up.
//
// AT START-UP THE AGE DOES NOT NEED MEASURING, and that is what makes
// this arm different from the sweeper above. A RESERVED record is one an
// address was answered for and no CreateEndpoint ever bound a link to;
// the process that could still have bound it is gone, and the fold
// admits no Create from another. So every one of them is orphaned by
// construction, and none of them can be waited for.
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

// ipamListedMACs is the engine's own answer about a network turned into
// the set the stranded-record rule reads, and a false second return
// means the answer cannot be used.
//
// AN ENTRY IT CANNOT PARSE POISONS THE WHOLE SET, because the rule below
// acts on ABSENCE: one hardware address that does not make it into the
// set makes every record on the network look unowned, and the rule would
// then give up the records of containers that are running. A set that is
// short by one is indistinguishable from a network with one fewer
// endpoint, so the only safe answer is to write nothing for that network
// this time round.
//
// The `ep-<id>` placeholders are deliberately IN. libnetwork stores an
// endpoint before its container has a sandbox, and it is still retrying
// Join for it; its record is owned and must be left alone.
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

// giveUpStrandedIPAMRecords hands back the addresses of IPAM records
// that the previous plugin process left with no endpoint behind them.
//
// THE HOLE IT CLOSES. An address request re-binds the single tombstone
// on its network and the record moves to CREATED before any link exists
// (ipamRebindCandidate). If the process ends there -- between
// RequestAddress and CreateEndpoint -- the reservation dies with its
// memory and the record survives in CREATED: no tombstone, so a retry
// re-binds nothing and takes a second lease; still answering address
// lookups, so `--ip` on that address is refused as held by another
// endpoint and a container pinned to the hardware address the re-bind
// wrote is refused outright; and outside the retained set, so neither
// the `on_remove` sweep nor `docker network rm` ever hands it back.
//
// TWO KEYS, AND BOTH ARE NECESSARY. A record is given up only when its
// last writer was an EARLIER process and its hardware address is one the
// engine does not list for this network.
//
// The writer alone is not enough: a running endpoint can sit in CREATED
// across a restart, because the phase only moves at the bind inside
// setupClient, which runs in a goroutine neither Join nor recovery waits
// for, and a failed attach leaves the record where it is. Giving those
// up would let a second container re-bind a running container's address,
// and on `release_lease=on_remove` it would put a DHCPRELEASE for a live
// address on the wire a minute later.
//
// The engine's list alone is not enough either: a container starting in
// THIS process is not listed until its CreateEndpoint has returned, so
// the rule would give up the record of the start it is racing.
//
// It does not wait for recovery's own adoptions, and does not need to:
// every endpoint recovery adopts is one the engine listed, so the list
// already protects it whether its bind has landed, failed, or not yet
// run.
//
// WHAT IT CANNOT SEE is an engine that answers with a SHORT list. A
// network's endpoint enumeration logs a store read error and returns
// what it has, and an endpoint whose own read fails is dropped from the
// answer; both look exactly like a network with fewer endpoints. There
// is no second source to check against: an IPAM handler may not ask
// Docker anything (ipam_mode.go), and recovery has the one answer.
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
