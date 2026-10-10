// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	goruntime "runtime"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// requestAddress6 answers RequestAddress on the network's IPv6 pool. The DHCPv6 server is the only allocator and
// `--ip6` is the IA Address hint; the v4 arms never run here, so a v4 lease cannot answer a v6 call (#1132).
func (p *Plugin) requestAddress6(ctx context.Context, networkID string, sn storedNetwork, req RequestAddressRequest) (RequestAddressResponse, error) {
	var none RequestAddressResponse
	mac, err := ipamRequestedMAC(req.Options)
	if err != nil {
		return none, err
	}
	var addr netip.Addr
	if req.Address != "" {
		if addr, err = netip.ParseAddr(req.Address); err != nil || !addr.Is6() || addr.Is4In6() {
			return none, fmt.Errorf("%w: %q is not an IPv6 address, and pool %v is this network's IPv6 pool", util.ErrIPAM, req.Address, req.PoolID)
		}
	}
	// libnetwork injects a MAC only when creating an endpoint, so a request without one is a replay (#110).
	if mac == nil {
		return p.replayAddress6(networkID, sn, addr)
	}
	// Design D10: the hint rides the IA_NA only, and new connections leave from the IA_TA address beside it (#1132).
	if sn.Options.IPv6Temporary && addr.IsValid() {
		return none, fmt.Errorf("%w: --ip6 %v cannot be used on a network created with -o ipv6_temporary=true. The container would also get a temporary IPv6 address, and new connections leave from that one, not from the address you asked for. Create the container without --ip6, or the network without ipv6_temporary", util.ErrIPAM, addr)
	}
	if err := ipamRefuseIPvlan(sn.Options.effectiveMode()); err != nil {
		return none, err
	}
	rebound := p.ipamReboundRecord6(sn.Binding.PoolID, mac)
	if err := p.refuseHeldMAC6(networkID, mac, addr, rebound); err != nil {
		return none, err
	}
	res, err := p.ipamReserveAddress6(ctx, networkID, sn, mac, addr, rebound)
	if err != nil {
		return none, err
	}
	return RequestAddressResponse{Address: res.addr.String()}, nil
}

// replayAddress6 confirms a stored v6 address from its record, or refuses and counts it, as the v4 replay does (#1132).
func (p *Plugin) replayAddress6(networkID string, sn storedNetwork, addr netip.Addr) (RequestAddressResponse, error) {
	if !addr.IsValid() {
		return RequestAddressResponse{}, fmt.Errorf("%w: an address request with neither a hardware address nor a known one to replay", util.ErrIPAM)
	}
	if rec, ok := p.ipamRecordFor(dhcp.Scope6(networkID), addr); ok {
		p.ipamReplayHits.Add(1)
		return ipamAddressOfRecord(rec.Lease.Addr, sn.Binding.Pool6)
	}
	p.ipamReplayMiss.Add(1)
	log.WithFields(log.Fields{
		"network": shortID(networkID),
		"address": addr.String(),
	}).Warn("Docker replayed an endpoint IPv6 address this plugin has no lease record for; refusing to confirm it")
	return RequestAddressResponse{}, fmt.Errorf("%w: no lease record in network %v holds %v", util.ErrIPAM, shortID(networkID), addr)
}

// ipamReboundRecord6 is the v6 tombstone the same endpoint's v4 reservation re-bound, which the v6 call runs on, so
// the DUID survives a restart (RFC 9915 section 11, #960, #1132).
func (p *Plugin) ipamReboundRecord6(poolID string, mac net.HardwareAddr) string {
	if r, ok := p.ipamReserves.peek(ipamReserveKey(poolID, mac)); ok && r.err == nil {
		return r.record6
	}
	return ""
}

// refuseHeldMAC6 is the duplicate-MAC rule in the v6 scope only: the same endpoint's v4 reservation holds the MAC in
// the v4 scope, and the record it re-bound is this call's own (#1132).
func (p *Plugin) refuseHeldMAC6(networkID string, mac net.HardwareAddr, addr netip.Addr, rebound string) error {
	if p.records == nil {
		return nil
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not read the lease records; a second endpoint under a hardware address this network already leases for cannot be detected here")
		return nil
	}
	scope := dhcp.Scope6(networkID)
	now := time.Now()
	for _, rec := range rb.ByScopeMAC(scope, mac) {
		if _, holds := rec.Resume(now); holds && rec.ID != rebound {
			return p.refuseDuplicateMAC(networkID, mac, "an endpoint of this network already holds an IPv6 lease under it, in phase "+rec.Phase.String())
		}
	}
	if addr.IsValid() {
		if rec, ok := ipamLiveRecord(rb, scope, addr); ok && rec.ID != rebound {
			return ipamRecordAnswersFor(rec, mac, addr)
		}
	}
	return nil
}

func (p *Plugin) ipamReserveAddress6(ctx context.Context, networkID string, sn storedNetwork, mac net.HardwareAddr, demanded netip.Addr, rebound string) (*ipamReservation, error) {
	key := ipamReserveKey(sn.Binding.PoolID6, mac)
	res, mine := p.ipamReserves.begin(key, time.Now())
	if !mine {
		return nil, p.refuseDuplicateMAC(networkID, mac, "an IPv6 address request under it is still running")
	}
	out, err := p.runIPAMReserve6(ctx, networkID, sn, mac, demanded, rebound)
	p.ipamReserves.finish(key, res, out, err)
	return res, err
}

// runIPAMReserve6 is runIPAMReserve for DHCPv6 on its own short-lived link (design D6). The link keeps IPv6 on, since
// the client binds to its link-local. No absence is tolerated: RequestAddress answers an address or fails (D5, #1132).
func (p *Plugin) runIPAMReserve6(ctx context.Context, networkID string, sn storedNetwork, mac net.HardwareAddr, demanded netip.Addr, rebound string) (ipamReservation, error) {
	var none ipamReservation
	opts := sn.Options

	budget := p.ipamLeaseTimeout(opts, sn.Binding.PoolID6)
	ctx, cancel := context.WithTimeout(ctx, ipamReserveBudget())
	defer cancel()

	name, peer, err := ipamReserveLinkNames()
	if err != nil {
		return none, fmt.Errorf("failed to name a reservation link: %w", err)
	}
	remove, err := ipamAddReserveLink6(p, ctx, name, peer, opts.effectiveMode(), opts, mac)
	if err != nil {
		return none, err
	}
	defer remove()

	recordID, id6, preferred, err := p.ipamReserveIdentity6(opts, networkID, mac, rebound)
	if err != nil {
		return none, err
	}
	if recordID == "" {
		return none, fmt.Errorf("%w: the IPv6 lease record for %v could not be written, so no address is requested for it; check the plugin's state directory", util.ErrIPAM, mac)
	}
	reused := recordID == rebound
	giveUp := func(keepTheWindow bool) { p.ipamGiveUpAttempt(recordID, keepTheWindow, time.Now()) }

	pol, err := resolveServerPolicy(opts)
	if err != nil {
		giveUp(reused)
		return none, err
	}
	hint := preferred
	if demanded.IsValid() {
		hint = demanded.String()
	}
	base := dhcp.DHCPClientOptions{
		FQDN:          opts.fqdnMode(),
		VendorClass:   opts.VendorClass,
		RapidCommit:   opts.RapidCommit,
		IPv6Temporary: opts.IPv6Temporary,
		MAC:           mac,
		Records:       p.records,
	}
	if err := p.v6Wiring(&base, opts, id6, recordID, hint, "", networkID); err != nil {
		giveUp(reused)
		return none, err
	}
	if err := p.conflictWiring(&base, opts, roleAcquire, networkID, "", true); err != nil {
		giveUp(reused)
		return none, err
	}

	info, _, err := p.acquireWithPolicy(ctx, name, pol, true, budget, "", base)
	if err != nil {
		giveUp(reused)
		return none, fmt.Errorf("failed to reserve an IPv6 address for %v via DHCPv6 within %v: %w", mac, budget, err)
	}
	got, err := netip.ParsePrefix(info.IP)
	if err == nil {
		err = ipamAccept6(got.Addr(), sn.Binding.Pool6, demanded)
	}
	if err != nil {
		note := p.ipamReleaseRefused6(opts, recordID)
		giveUp(false)
		return none, fmt.Errorf("%w: %v. %s", util.ErrIPAM, err, note)
	}
	return ipamReservation{addr: got, info: info, record: recordID, rebound: reused}, nil
}

// ipamReserveIdentity6 keeps the re-bound record's DUID and last address; with none it mints on the MAC and opens a
// RESERVED v6 record (design D7, #1132).
func (p *Plugin) ipamReserveIdentity6(opts DHCPNetworkOptions, networkID string, mac net.HardwareAddr, rebound string) (string, dhcp.Identity6, string, error) {
	if rebound != "" {
		if id6, preferred, ok := p.reboundIdentity6(rebound); ok {
			return rebound, id6, preferred, nil
		}
	}
	id6, err := resolveIdentity6(opts, "", mac)
	if err != nil {
		return "", dhcp.Identity6{}, "", err
	}
	if p.records == nil {
		return "", id6, "", nil
	}
	id := newRecordID()
	if id == "" {
		return "", id6, "", nil
	}
	if err := p.records.Reserved6(id, networkID, mac, id6.Bytes()); err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not open the IPv6 reservation's lease record; no address is requested without one")
		return "", id6, "", nil
	}
	return id, id6, "", nil
}

// ipamAccept6 is the pair of acceptance rules on netip.Addr: the typed prefix first (design D4), since a new --ip6
// would not help there, then `--ip6` itself (D3), since libnetwork adopts whatever address the driver returns (#1132).
func ipamAccept6(got netip.Addr, pool string, demanded netip.Addr) error {
	prefix, err := netip.ParsePrefix(pool)
	if err != nil {
		return fmt.Errorf("this network's IPv6 pool %q is not a prefix", pool)
	}
	if !prefix.Contains(got) {
		return fmt.Errorf("the DHCPv6 server answered %v, which is outside this network's IPv6 subnet %v, so the address was not taken. Type the --subnet the server hands addresses out from, or create the network without an IPv6 --subnet and switch IPv6 on with -o ipv6_mode=dhcp", got, prefix)
	}
	if demanded.IsValid() && got != demanded {
		return fmt.Errorf("--ip6 asked for %v and the DHCPv6 server answered %v instead, so the address was not taken. The server decides: %v may be reserved for another client, already leased, outside the range it hands out, or in use on the segment. Ask for an address the server will grant this client, or create the container without --ip6", demanded, got, demanded)
	}
	return nil
}

// ipamReleaseRefused6 hands a refused Reply's lease back through the one release path, from the parent's link-local
// (RFC 9915 section 18.2.7); the plugin installed nothing, so nothing is withdrawn first (#1132).
func (p *Plugin) ipamReleaseRefused6(opts DHCPNetworkOptions, recordID string) string {
	const notSent = "No Release could be sent, so that lease runs out at the server"
	if p.records == nil {
		return notSent
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("record", recordID).Warn("Could not read the refused IPv6 lease back; it is not released")
		return notSent
	}
	rec, ok := rb.ByID(recordID)
	if !ok || !rec.Lease.Addr.IsValid() {
		return notSent
	}
	fields := log.Fields{"record": recordID, "ip": rec.Lease.Addr.String(), "is_ipv6": true}
	held, _ := rec.Addr()
	out := releaseFromRecord(rec, opts, true, held, nil, fields)
	p.countRelease(true, out == releaseSent)
	announceReleaseOutcome(log.WithFields(fields).WithField("outcome", string(out)), out)
	if out != releaseSent {
		return notSent
	}
	return "That lease was released"
}

var ipamAddReserveLink6 = func(p *Plugin, ctx context.Context, name, peer, mode string, opts DHCPNetworkOptions, mac net.HardwareAddr) (func(), error) {
	return p.addIPAMReserveLinkFor(ctx, name, peer, mode, opts, mac, true)
}

// ipamReserveLinkIPv6 leaves a v6 reservation link its link-local but stops the host acting on advertisements there,
// which is what #1247 turned IPv6 off for; a v4 one keeps IPv6 off (#1132).
func ipamReserveLinkIPv6(name string, v6 bool) {
	if !v6 {
		childIPv6Off(name)
		return
	}
	if err := ipamReserveLinkRAGuard(name); err != nil {
		log.WithError(err).WithField("link", name).Warn("Could not stop the host taking router advertisements on a reservation link; it may hold a SLAAC address and route there until the link goes (#1247)")
	}
}

var ipamReserveLinkRAGuard = func(name string) error { return hostRAGuardUnder(ipv6DisableSysctlDir, name) }

// hostRAGuardUnder writes the guard on a thread never unlocked, as disableHostIPv6Under does (#1132).
func hostRAGuardUnder(dir, name string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		goruntime.LockOSThread()
		if err := makeProcSysWritable(); err != nil {
			log.WithError(err).Debug("Could not make /proc/sys writable; attempting the router advertisement guard anyway")
		}
		done <- dhcp.ApplyRouterAdvertGuard(dir, name).Err
	}()
	return <-done
}

// peek reads a finished reservation without taking it; the v6 call reads the v4 one the engine asked for first (#1132).
func (s *ipamReserves) peek(key string) (*ipamReservation, bool) {
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
		return r, true
	default:
		return nil, false
	}
}

// takeIPAMReservation6 takes the v6 reservation beside the v4 one, so every exit of createIPAMEndpoint gives it up
// with its v4 sibling (#1132).
func (p *Plugin) takeIPAMReservation6(binding *ipamBinding, mac net.HardwareAddr) (*ipamReservation, bool) {
	if binding.PoolID6 == "" {
		return nil, false
	}
	return p.ipamReserves.take(ipamReserveKey(binding.PoolID6, mac))
}

// checkIPAMReservation6 refuses a v6-pool endpoint whose reservation is missing or names another address than the
// engine's, which installs the address it holds (#1132).
func checkIPAMReservation6(rsv *ipamReservation, ok bool, mac net.HardwareAddr, engineAddr string) error {
	if !ok {
		return fmt.Errorf("%w: no IPv6 reservation is held for %v on this network. The plugin was restarted between Docker allocating the address and creating the container; run `docker start` again", util.ErrIPAM, mac)
	}
	if rsv.err != nil {
		return rsv.err
	}
	want, err := netip.ParsePrefix(engineAddr)
	if err != nil {
		return fmt.Errorf("%w: Docker gave this endpoint the IPv6 address %q, which is not an address with a prefix", util.ErrIPAM, engineAddr)
	}
	if rsv.addr.Addr() != want.Addr() {
		return fmt.Errorf("%w: Docker created this endpoint with %v while the IPv6 lease reserved for %v is %v", util.ErrIPAM, want.Addr(), mac, rsv.addr.Addr())
	}
	if rsv.record == "" {
		return fmt.Errorf("%w: the IPv6 reservation for %v has no lease record, so its address could not survive to Join", util.ErrIPAM, mac)
	}
	return nil
}

// consumeIPAMReservation6 folds the RESERVED v6 record into CREATED and hands Join the address; it answers nothing,
// since the engine already holds the address from RequestAddress (design D7, #1132).
func (p *Plugin) consumeIPAMReservation6(networkID, endpointID string, mac net.HardwareAddr, rsv *ipamReservation) (string, error) {
	// A re-bound record is CREATED already, and a second create is a fold reject.
	if !rsv.rebound && p.records != nil {
		if err := p.records.CreatedOn6(rsv.record, networkID, mac); err != nil {
			return "", fmt.Errorf("failed to bind this endpoint to the IPv6 address reserved for it: %w", err)
		}
	}
	ip, err := netlink.ParseAddr(rsv.info.IP)
	if err != nil {
		return "", fmt.Errorf("failed to parse the reserved IPv6 address %q: %w", rsv.info.IP, err)
	}
	p.updateJoinHint(endpointID, func(h *joinHint) {
		h.IPv6 = ip
		fillV6Hint(h, rsv.info)
	})
	return ip.IP.String(), nil
}

// ipamReservationEnded reports a v6 reservation whose record its v4 sibling's give-up already retained, which the
// sweep only drops: a second retain is a fold reject (#1132).
func (p *Plugin) ipamReservationEnded(recordID string) bool {
	if p.records == nil {
		return false
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		return false
	}
	rec, ok := rb.ByID(recordID)
	return ok && rec.Phase != lease.PhaseReserved && rec.Phase != lease.PhaseCreated
}

// ipamDropReservation6 drops the v6 reservation whose record the engine's v4 release already gave up, so a retry
// under the same MAC is not refused as a duplicate (#1132).
func (p *Plugin) ipamDropReservation6(rb lease.Rebuilt, scope, poolID string, addr netip.Addr) {
	if !addr.Is6() {
		return
	}
	for _, rec := range rb.ByScopeAddr(scope, addr) {
		key := ipamReserveKey(poolID, net.HardwareAddr(rec.CHAddr))
		if r, ok := p.ipamReserves.peek(key); ok && r.addr.Addr() == addr {
			p.ipamReserves.take(key)
			return
		}
	}
}
