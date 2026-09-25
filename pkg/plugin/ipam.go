// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Wire strings from libnetwork's RequestAddress options, spelled out so a rename cannot compile silently (#110).
const (
	ipamOptRequestAddressType = "RequestAddressType"
	ipamOptGateway            = "com.docker.network.gateway"
	ipamOptMacAddress         = "com.docker.network.endpoint.macaddress"
)

// Payload shapes from moby libnetwork/ipams/remote/api; the error field is `Error` here and `Err` on the
// network-driver side, and the daemon turns any non-200 into an error before reading it (#110).

// IpamCapabilitiesResponse answers /IpamDriver.GetCapabilities.
type IpamCapabilitiesResponse struct {
	RequiresMACAddress    bool
	RequiresRequestReplay bool
}

// IpamAddressSpacesResponse answers /IpamDriver.GetDefaultAddressSpaces.
type IpamAddressSpacesResponse struct {
	LocalDefaultAddressSpace  string
	GlobalDefaultAddressSpace string
}

// RequestPoolRequest asks for an address pool.
type RequestPoolRequest struct {
	AddressSpace string
	Pool         string
	SubPool      string
	Options      map[string]string
	V6           bool
}

// RequestPoolResponse answers it.
type RequestPoolResponse struct {
	PoolID string
	Pool   string
	Data   map[string]string
}

// ReleasePoolRequest gives a pool back.
type ReleasePoolRequest struct {
	PoolID string
}

// RequestAddressRequest asks for one address out of a pool.
type RequestAddressRequest struct {
	PoolID  string
	Address string
	Options map[string]string
}

// RequestAddressResponse answers it, in CIDR form.
type RequestAddressResponse struct {
	Address string
	Data    map[string]string
}

// ReleaseAddressRequest gives one address back.
type ReleaseAddressRequest struct {
	PoolID  string
	Address string
}

// GwAllocCheckRequest asks the network driver whether a gateway address is needed; it carries only driver options.
type GwAllocCheckRequest struct {
	Options map[string]interface{}
}

// GwAllocCheckResponse answers it.
type GwAllocCheckResponse struct {
	SkipIPv4 bool
	SkipIPv6 bool
}

// apiGwAllocCheck always skips the gateway allocation: the gateway comes from the DHCP server at Join (#110).
// The request has no network id, so the answer cannot be per network; a user-typed --gateway still reaches
// RequestAddress, since libnetwork asks whenever cfg.Gateway is set.
func (p *Plugin) apiGwAllocCheck(w http.ResponseWriter, r *http.Request) {
	var req GwAllocCheckRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	util.JSONResponse(w, GwAllocCheckResponse{SkipIPv4: true, SkipIPv6: true}, http.StatusOK)
}

func (p *Plugin) apiIpamGetCapabilities(w http.ResponseWriter, r *http.Request) {
	// The daemon fills the MAC into RequestAddress only with RequiresMACAddress, and replays stored endpoints'
	// addresses at start-up only with RequiresRequestReplay (#110).
	util.JSONResponse(w, IpamCapabilitiesResponse{
		RequiresMACAddress:    true,
		RequiresRequestReplay: true,
	}, http.StatusOK)
}

func (p *Plugin) apiIpamGetDefaultAddressSpaces(w http.ResponseWriter, r *http.Request) {
	util.JSONResponse(w, IpamAddressSpacesResponse{
		LocalDefaultAddressSpace:  ipamLocalAddressSpace,
		GlobalDefaultAddressSpace: ipamGlobalAddressSpace,
	}, http.StatusOK)
}

func (p *Plugin) apiRequestPool(w http.ResponseWriter, r *http.Request) {
	var req RequestPoolRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	res, err := p.RequestPool(req)
	if err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}
	util.JSONResponse(w, res, http.StatusOK)
}

// RequestPool answers with the canonical pool identity and writes nothing durable, so the start-up replay derives the
// stored PoolID (#110).
func (p *Plugin) RequestPool(req RequestPoolRequest) (RequestPoolResponse, error) {
	if req.V6 {
		return RequestPoolResponse{}, fmt.Errorf("%w: --ipv6 is refused on a network that uses this plugin as its IPAM driver, because the plugin allocates no IPv6 pool. IPv6 on such a network needs no Docker pool: drop --ipv6 and switch it on with `-o ipv6=true` or `-o ipv6_mode=<mode>`, and each container gets its IPv6 address from the DHCPv6 server or the router advertisement on its link", util.ErrIPAM)
	}
	if req.SubPool != "" {
		return RequestPoolResponse{}, fmt.Errorf("%w: --ip-range is not supported: addresses come from the LAN's DHCP server, which this plugin does not narrow", util.ErrIPAM)
	}
	poolID, err := ipamPoolID(req.AddressSpace, req.Pool, req.Options)
	if err != nil {
		return RequestPoolResponse{}, err
	}
	pool, err := ipamCanonicalPool(req.Pool)
	if err != nil {
		return RequestPoolResponse{}, err
	}
	_, name := ipamPoolIDNames(poolID)
	p.ipamPools.add(poolID, req.AddressSpace, pool, name, time.Now())

	log.WithFields(log.Fields{"pool_id": poolID, "pool": pool}).Debug("Address pool requested")
	// No Data[gateway]: a Meta gateway would become the network's gateway before any endpoint exists (#110).
	return RequestPoolResponse{PoolID: poolID, Pool: pool}, nil
}

func (p *Plugin) apiReleasePool(w http.ResponseWriter, r *http.Request) {
	var req ReleasePoolRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	// Drops only the unconsumed issue: this arrives for a failed create and at every delete before DeleteNetwork
	// (#110).
	p.ipamPools.drop(req.PoolID)
	util.JSONResponse(w, struct{}{}, http.StatusOK)
}

func (p *Plugin) apiRequestAddress(w http.ResponseWriter, r *http.Request) {
	var req RequestAddressRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	res, err := p.RequestAddress(r.Context(), req)
	if err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}
	util.JSONResponse(w, res, http.StatusOK)
}

// RequestAddress dispatches on the record store, since libnetwork does not persist endpoint IPAM options (#110).
func (p *Plugin) RequestAddress(ctx context.Context, req RequestAddressRequest) (RequestAddressResponse, error) {
	var none RequestAddressResponse

	networkID, bound := p.ipamIndex.network(req.PoolID)
	if !bound {
		// A gateway is answered before any binding exists, including an empty address: engines below 28 have no
		// GwAllocCheck and request the gateway at create with no address (moby libnetwork/drivers/remote/driver.go at
		// v26.1.5 and v27.0.0); refusing it failed every such create (#1012).
		if req.Options[ipamOptRequestAddressType] == ipamOptGateway {
			if req.Address == "" {
				return ipamPoolNetworkAddress(req.PoolID)
			}
			return ipamEchoAddress(req.Address, ipamAnyPool)
		}
		if req.Address == "" {
			return none, fmt.Errorf("%w: no network is bound to pool %v, so there is nothing to lease from", util.ErrIPAM, req.PoolID)
		}
		// A network rebuildIPAMIndex skipped also looks unbound, so the echo is refused while the fold is incomplete
		// (#110).
		if p.ipamIndex.isIncomplete() {
			p.ipamReplayMiss.Add(1)
			log.WithFields(log.Fields{
				"pool":    req.PoolID,
				"address": req.Address,
			}).Warn("An address was requested for a pool no network holds, on a host where at least one network's state could not be read at start-up; refusing rather than confirming it")
			return none, fmt.Errorf("%w: no network is bound to pool %v, and at least one network's state file could not be read when this plugin started, so this address cannot be confirmed. Repair or remove the unreadable file in the plugin's state directory and restart the plugin", util.ErrIPAM, req.PoolID)
		}
		return ipamEchoAddress(req.Address, ipamAnyPool)
	}

	sn, err := ipamNetwork(networkID)
	if err != nil {
		return none, err
	}

	if req.Options[ipamOptRequestAddressType] == ipamOptGateway {
		if req.Address == "" {
			return none, fmt.Errorf("%w: this plugin does not allocate a gateway address. The gateway comes from the DHCP server and reaches the container at Join. Pass --gateway if you need Docker's own network record to name one", util.ErrIPAM)
		}
		return ipamEchoAddress(req.Address, sn.Binding.Pool)
	}

	if req.Address != "" && ipamIsAuxOfNetwork(sn.Binding, req.Address) {
		return ipamEchoAddress(req.Address, sn.Binding.Pool)
	}

	mac, err := ipamRequestedMAC(req.Options)
	if err != nil {
		return none, err
	}

	// libnetwork injects a MAC only when creating an endpoint, so a create whose MAC a live record here holds is a
	// second endpoint, even with a matching --ip (#110).
	if mac != nil {
		if rec, held := p.ipamEndpointHoldingMAC(networkID, mac); held {
			return none, p.refuseDuplicateMAC(networkID, mac, "an endpoint of this network already holds it, in phase "+rec.Phase.String())
		}
	}

	if req.Address != "" {
		addr, err := netip.ParseAddr(req.Address)
		if err != nil {
			return none, fmt.Errorf("%w: %q is not an address", util.ErrIPAM, req.Address)
		}
		if rec, ok := p.ipamRecordFor(networkID, addr); ok {
			if err := ipamRecordAnswersFor(rec, mac, addr); err != nil {
				return none, err
			}
			p.ipamReplayHits.Add(1)
			return ipamAddressOfRecord(rec.Lease.Addr, sn.Binding.Pool)
		}
		if mac == nil {
			// A replay with no record is refused; the network driver's recovery adopts the address from Docker's view
			// (#110).
			p.ipamReplayMiss.Add(1)
			log.WithFields(log.Fields{
				"network": shortID(networkID),
				"address": req.Address,
			}).Warn("Docker replayed an endpoint address this plugin has no lease record for; refusing to confirm it")
			return none, fmt.Errorf("%w: no lease record in network %v holds %v", util.ErrIPAM, shortID(networkID), req.Address)
		}
	}

	if mac == nil {
		return none, fmt.Errorf("%w: an address request with neither a hardware address nor a known one to replay", util.ErrIPAM)
	}
	if err := ipamRefuseIPvlan(sn.Options.effectiveMode()); err != nil {
		return none, err
	}

	res, err := p.ipamReserveAddress(ctx, networkID, sn, mac, req.Address)
	if err != nil {
		return none, err
	}
	return RequestAddressResponse{Address: res.addr.String()}, nil
}

// ipamRecordAnswersFor refuses a record whose CHAddr is not the creating endpoint's MAC, or is empty (#110).
func ipamRecordAnswersFor(rec lease.Record, mac net.HardwareAddr, addr netip.Addr) error {
	if mac == nil {
		return nil
	}
	if len(rec.CHAddr) > 0 && bytes.Equal(rec.CHAddr, mac) {
		return nil
	}
	return fmt.Errorf("%w: %v is held by another endpoint on this network (record %v), so it cannot be given to %v as well. Pick a free address, or stop the container holding this one",
		util.ErrIPAM, addr, rec.ID, mac)
}

// ipamEndpointHoldingMAC fails open on a read error, as ipamRecordFor does; failing closed would refuse every
// start on every IPAM network of a host whose journal does not read (#110).
func (p *Plugin) ipamEndpointHoldingMAC(networkID string, mac net.HardwareAddr) (lease.Record, bool) {
	if p.records == nil {
		return lease.Record{}, false
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not read the lease records; a second endpoint under a hardware address this network already leases for cannot be detected here")
		return lease.Record{}, false
	}
	return ipamLiveRecordForMAC(rb, networkID, mac, time.Now())
}

func (p *Plugin) ipamRecordFor(networkID string, addr netip.Addr) (lease.Record, bool) {
	if p.records == nil {
		return lease.Record{}, false
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).Warn("Could not read the lease records; an address replay cannot be confirmed")
		return lease.Record{}, false
	}
	return ipamLiveRecord(rb, networkID, addr)
}

// ipamRequestedMAC refuses a malformed MAC, which would lease under an identity Docker does not pin on the link.
func ipamRequestedMAC(opts map[string]string) (net.HardwareAddr, error) {
	s := opts[ipamOptMacAddress]
	if s == "" {
		return nil, nil
	}
	mac, err := net.ParseMAC(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a hardware address", util.ErrMACAddress, s)
	}
	return mac, nil
}

func ipamIsAuxOfNetwork(b *ipamBinding, address string) bool {
	if b.Gateway == address {
		return true
	}
	for _, a := range b.Aux {
		if a == address {
			return true
		}
	}
	return false
}

// ipamEchoAddress returns a caller-supplied address with this network's prefix length, as libnetwork parses a CIDR.
func ipamEchoAddress(address, pool string) (RequestAddressResponse, error) {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return RequestAddressResponse{}, fmt.Errorf("%w: %q is not an address", util.ErrIPAM, address)
	}
	bits := 32
	if pool != "" && pool != ipamAnyPool {
		p, err := netip.ParsePrefix(pool)
		if err == nil {
			bits = p.Bits()
		}
	}
	return RequestAddressResponse{Address: netip.PrefixFrom(addr, bits).String()}, nil
}

// ipamAddressOfRecord prefers the server's option-1 mask and falls back to the pool's.
func ipamAddressOfRecord(addr netip.Prefix, pool string) (RequestAddressResponse, error) {
	if addr.IsValid() && addr.Bits() > 0 {
		return RequestAddressResponse{Address: addr.String()}, nil
	}
	return ipamEchoAddress(addr.Addr().String(), pool)
}

func (p *Plugin) apiReleaseAddress(w http.ResponseWriter, r *http.Request) {
	var req ReleaseAddressRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	if err := p.ReleaseAddress(req); err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}
	util.JSONResponse(w, struct{}{}, http.StatusOK)
}

// ReleaseAddress after a failed CreateEndpoint retains the reservation to the tombstone deadline, so a restart's
// retry claims the same address; no DHCPRELEASE goes on the wire here on any release_on value (#110, #962). On
// `on_remove` the record sweeper releases it when the deadline passes (#984).

func (p *Plugin) ReleaseAddress(req ReleaseAddressRequest) error {
	networkID, bound := p.ipamIndex.network(req.PoolID)
	if !bound {
		return nil
	}
	addr, err := netip.ParseAddr(req.Address)
	if err != nil {
		return nil
	}
	if p.records == nil {
		return nil
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		return nil
	}
	rec, ok := ipamLiveRecord(rb, networkID, addr)
	if !ok {
		p.ipamReleaseUnknown.Add(1)
		return nil
	}
	// A re-bound record is created before any packet, and a rolled-back start reaches it only here (#110).
	if rec.Phase != lease.PhaseReserved && rec.Phase != lease.PhaseCreated {
		return nil
	}
	key := ipamReserveKey(req.PoolID, net.HardwareAddr(rec.CHAddr))
	if p.ipamReserves.inFlight(key) {
		return nil
	}
	if p.ipamEndpointHolds(net.HardwareAddr(rec.CHAddr), req.Address) {
		return nil
	}
	p.ipamReserves.take(key)
	p.ipamGiveUpRecord(rec.ID, time.Now())
	log.WithFields(log.Fields{
		"network": shortID(networkID),
		"address": req.Address,
		"phase":   rec.Phase.String(),
	}).Info("An address was released with no endpoint holding it; giving it back so a retry can claim it")
	return nil
}

// ipamEndpointHolds keys on network and address, since two IPAM networks on one segment can issue one address; either
// family counts, so a v6 tombstone a running endpoint holds is not re-bound (#110, #960).
func (p *Plugin) ipamEndpointHolds(mac net.HardwareAddr, addr string) bool {
	if len(mac) == 0 || addr == "" {
		return false
	}
	want := mac.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, fp := range p.endpointFingerprints {
		if fp.MAC == want && (fp.IPv4 == addr || fp.IPv6 == addr) {
			return true
		}
	}
	return false
}
