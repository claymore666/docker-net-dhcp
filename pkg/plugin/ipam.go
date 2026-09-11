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

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// The option keys libnetwork puts in a RequestAddress. Spelled out here
// rather than imported: this plugin talks to the daemon over HTTP and
// JSON, so the wire strings are the contract, and importing a constant
// would hide a rename behind a compile that still succeeded.
const (
	ipamOptRequestAddressType = "RequestAddressType"
	ipamOptGateway            = "com.docker.network.gateway"
	ipamOptMacAddress         = "com.docker.network.endpoint.macaddress"
)

// Payload shapes, from moby libnetwork/ipams/remote/api. The error field
// is `Error` here and `Err` on the network-driver side; they are two
// different structs in moby and this plugin serves both. A non-200 with
// the plugin's usual error body is what actually carries a refusal --
// the daemon's plugin client turns any non-200 into an error before it
// ever looks at this field -- so these responses carry only success.

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

// GwAllocCheckRequest asks the NETWORK driver whether this network needs
// a gateway address allocated for it. It carries the driver options and
// nothing else -- no network id and no address space.
type GwAllocCheckRequest struct {
	Options map[string]interface{}
}

// GwAllocCheckResponse answers it.
type GwAllocCheckResponse struct {
	SkipIPv4 bool
	SkipIPv6 bool
}

// apiGwAllocCheck says this driver never wants a gateway ADDRESS
// allocated out of the pool.
//
// The gateway a container uses is the one the DHCP server named, and it
// is delivered by the network driver's Join as a route into the sandbox.
// Asking the IPAM driver for a gateway address would mean running a DHCP
// exchange at `docker network create` for an address nothing uses.
//
// THE ANSWER CANNOT BE PER NETWORK, and that is a property of the call
// and not a shortcut: the request carries the driver options only, so
// there is no network id and no address space to branch on, and
// libnetwork persists whatever comes back into each network's own store.
// So a `--ipam-driver null` network gets this answer too. That is safe
// and it is measured rather than assumed: with the null IPAM driver the
// gateway request returned no address anyway, and a user-typed
// `--gateway` still reaches the IPAM driver whatever this says
// (libnetwork asks when `cfg.Gateway != ""` regardless of the skip).
func (p *Plugin) apiGwAllocCheck(w http.ResponseWriter, r *http.Request) {
	var req GwAllocCheckRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	util.JSONResponse(w, GwAllocCheckResponse{SkipIPv4: true, SkipIPv6: true}, http.StatusOK)
}

func (p *Plugin) apiIpamGetCapabilities(w http.ResponseWriter, r *http.Request) {
	// RequiresMACAddress is what puts the endpoint's hardware address
	// into RequestAddress at all: without it there is nothing to run a
	// DHCP exchange as. RequiresRequestReplay is what makes the daemon
	// re-ask for every stored endpoint's address at start-up, which is
	// how an IPAM-mode network survives a daemon restart.
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

// RequestPool answers with the pool identity and writes nothing durable.
//
// The identity is the canonical request, so the SAME call at the daemon's
// start-up replay -- where libnetwork sends back the pool this driver
// returned, together with the option map it persisted -- derives the
// PoolID the daemon already stored. A driver that minted a fresh id, or
// that wrote state here, would unbind every network at every restart.
func (p *Plugin) RequestPool(req RequestPoolRequest) (RequestPoolResponse, error) {
	if req.V6 {
		return RequestPoolResponse{}, fmt.Errorf("%w: this plugin does not allocate IPv6 pools yet. Run the network without Docker's --ipv6 and use -o ipv6=true, which is unchanged, or wait for v2.2.0", util.ErrIPAM)
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
	// No Data[gateway]: a Meta gateway would become the network's
	// gateway before any container exists, and the real one is whatever
	// the DHCP server tells each endpoint at Join.
	return RequestPoolResponse{PoolID: poolID, Pool: pool}, nil
}

func (p *Plugin) apiReleasePool(w http.ResponseWriter, r *http.Request) {
	var req ReleasePoolRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}
	// DROPS THE UNCONSUMED ISSUE AND NOTHING ELSE. This call arrives for
	// a create that failed on a PoolID another network may still hold,
	// and again at every network delete BEFORE the driver's
	// DeleteNetwork. Closing records or dropping a binding here would
	// destroy a live network's state because an unrelated create failed.
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

// RequestAddress is the whole IPAM dispatch, and the dispatch is a
// question about the RECORD STORE rather than about the request.
//
// It has to be, because three of the shapes are wire-identical. An aux
// address at create, an aux address at the daemon-start replay, and a
// stored endpoint's address replayed at that same restart all arrive as
// an address with no options: libnetwork does not persist the endpoint's
// IPAM options, and it injects the MAC only when it is creating an
// endpoint. So what tells them apart is what this plugin knows about the
// address: the network's own saved gateway and aux set, and whether a
// record in an answering phase holds it.
func (p *Plugin) RequestAddress(ctx context.Context, req RequestAddressRequest) (RequestAddressResponse, error) {
	var none RequestAddressResponse

	networkID, bound := p.ipamIndex.network(req.PoolID)
	if !bound {
		// No network holds this pool. The calls that legally arrive are
		// the gateway and aux ones libnetwork makes while a create is
		// still in flight, before CreateNetwork has bound anything;
		// both carry an address and want it back unchanged.
		if req.Address == "" {
			return none, fmt.Errorf("%w: no network is bound to pool %v, so there is nothing to lease from", util.ErrIPAM, req.PoolID)
		}
		// A gateway says so on the wire and is never an endpoint's
		// address, so it is answered whatever the index knows.
		if req.Options[ipamOptRequestAddressType] == ipamOptGateway {
			return ipamEchoAddress(req.Address, ipamAnyPool)
		}
		// The aux shape and a stored endpoint's replay are otherwise
		// wire-identical, and one more thing can make a pool unbound:
		// rebuildIPAMIndex skipping a network whose file would not
		// read. Echoing there would confirm Docker's stored address
		// from a process holding no record of it -- row A's
		// degradation, arriving before either replay counter is
		// reached. While anything is missing from the fold, the echo is
		// refused and counted as the miss it is.
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
		// Never a lease. A typed --gateway is handed straight back; an
		// untyped one is refused, which is unreachable while
		// GwAllocCheck answers skip and is the belt beside it.
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

	// THE SETTLED HALF OF THE ONE-EXCHANGE RULE, and it stands here
	// rather than inside the reserve because the reserve is not the only
	// way past. A MAC in the options means libnetwork is CREATING an
	// endpoint -- it injects one at no other time, which is the fact the
	// replay branch below is built on -- so a create whose hardware
	// address a live record on this network already holds is a second
	// endpoint, whatever else the request carries. Left to the reserve,
	// a second container that pins `--ip` as well as `--mac-address`
	// walked past: its address AND its MAC match the running endpoint's
	// record, ipamRecordAnswersFor reads that as the endpoint's own
	// replay, and the call is ANSWERED. Docker then published one
	// address for two endpoints and CreateEndpoint refused the loser
	// with a message about a plugin restart that never happened, with
	// this counter never moving.
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
			// The replay of a stored endpoint: the address is one this
			// plugin's own record holds, under the MAC that holds it.
			if err := ipamRecordAnswersFor(rec, mac, addr); err != nil {
				return none, err
			}
			p.ipamReplayHits.Add(1)
			return ipamAddressOfRecord(rec.Lease.Addr, sn.Binding.Pool)
		}
		if mac == nil {
			// A replay with no record behind it. Refused rather than
			// echoed: an echo would have Docker keep serving an address
			// nothing holds and nothing would ever say so. The daemon
			// logs the refusal, keeps the stored address, and the
			// network driver's own recovery adopts it from Docker's
			// view -- which is the path that has evidence behind it.
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

// ipamRecordAnswersFor refuses a record that belongs to some other
// endpoint.
//
// THE MATCH ON THE ADDRESS ALONE IS NOT ENOUGH WHEN THE REQUEST CARRIES
// A MAC. libnetwork injects the hardware address only when it is
// CREATING an endpoint, so `docker run --ip X` for an address a running
// container already holds looks exactly like that container's own
// replay: the call would be answered, libnetwork would allocate one
// address to two endpoints, ipam_replay_hits would move for something
// that is not a replay, and the contradiction would surface later at
// CreateEndpoint wearing a message about a plugin restart that never
// happened. A record answers a creating endpoint only when it is that
// endpoint's, which the CHAddr says.
//
// An empty CHAddr is refused with the rest. A record that cannot say
// whose it is cannot be handed to a new endpoint, and the refusal is
// visible at `docker run` rather than silent.
func ipamRecordAnswersFor(rec lease.Record, mac net.HardwareAddr, addr netip.Addr) error {
	if mac == nil {
		// The replay shape: no MAC to compare, and the address is what
		// Docker stored for this endpoint.
		return nil
	}
	if len(rec.CHAddr) > 0 && bytes.Equal(rec.CHAddr, mac) {
		return nil
	}
	return fmt.Errorf("%w: %v is held by another endpoint on this network (record %v), so it cannot be given to %v as well. Pick a free address, or stop the container holding this one",
		util.ErrIPAM, addr, rec.ID, mac)
}

// ipamEndpointHoldingMAC asks the RECORD STORE whether this network
// already has a live endpoint under this hardware address.
//
// It fails OPEN on a read error, and the opposite failure is why. The
// other disk lookup on this path, ipamRecordFor, already fails open on
// the same error, so a fold that will not read leaves the two agreeing
// rather than one refusing what the other confirms; and a fail-CLOSED
// guard here would refuse every container start on every IPAM network
// on a host whose journal is unreadable, which is a far larger outage
// than the one this guard exists to prevent. What is lost on such a
// host is the settled shape, and nothing downstream recovers it:
// createIPAMEndpoint reads no record at all, only its own in-memory
// reservation, and every check it makes passes for the second endpoint
// because they are all about that endpoint's own reservation. The
// in-flight half still closes two creates racing, with no disk.
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

// ipamRecordFor is the phase-filtered lookup, lifted so the dispatch
// reads as one decision and the filter can be driven on its own.
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

// ipamRequestedMAC reads the hardware address libnetwork generated for
// this endpoint, refusing a malformed one rather than leasing under a
// different identity than Docker will pin on the link.
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

// ipamIsAuxOfNetwork reports whether this address is one CreateNetwork
// was told about: the gateway or an auxiliary address. Those are
// reserved in Docker's own record and never leased.
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

// ipamEchoAddress returns an address the caller supplied, wearing this
// network's prefix length so libnetwork can parse it as a CIDR.
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

// ipamAddressOfRecord renders a record's leased address. The record
// carries the server's own option-1 mask, which is the honest one; the
// pool is the fallback for a record whose mask never arrived.
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

// ReleaseAddress gives one address back.
//
// For a live endpoint this arrives AFTER DeleteEndpoint, whose record is
// already RETAINED, so there is nothing left to do. The case that needs
// work is the other one: CreateEndpoint failed, so an address was
// reserved and no endpoint was ever created, and libnetwork releases it.
// Retaining the reservation with the tombstone deadline is what lets a
// restart policy's next attempt claim the same address back instead of
// burning a second lease on the server. No DHCPRELEASE goes on the wire
// (D-7): the address is left to expire exactly as any other host on the
// segment leaves one.
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
	if rec.Phase != lease.PhaseReserved {
		return nil
	}
	p.recordRetained(rec.ID, time.Now().Add(tombstoneTTL))
	p.ipamReserves.take(ipamReserveKey(req.PoolID, net.HardwareAddr(rec.CHAddr)))
	log.WithFields(log.Fields{
		"network": shortID(networkID),
		"address": req.Address,
	}).Info("An address was released before its endpoint existed; retaining it so a retry can claim it back")
	return nil
}
