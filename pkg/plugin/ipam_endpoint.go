// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// ipamBindingOf reads a network's pool binding, or nil.
//
// NIL MEANS NULL MODE AND NOT "UNKNOWN", and that is only true because
// netOptionsRaw refuses an IPAM-mode network whose state file it could
// not read. Every caller below has already been through netOptions or
// netMode, so a network that reaches here with an unreadable file is one
// the Docker API said is not ours to allocate for -- a null-IPAM
// network, which is exactly what nil says.
func ipamBindingOf(networkID string) *ipamBinding {
	sn, err := loadNetwork(networkID)
	if err != nil {
		return nil
	}
	return sn.Binding
}

// createIPAMEndpoint is CreateEndpoint for a network this plugin
// allocates addresses for.
//
// IT RUNS NO DHCP EXCHANGE, which is the whole difference. The address
// this endpoint will use was leased by RequestAddress, minutes or
// milliseconds ago, on a temporary link carrying the same MAC; running a
// second exchange here would ask the server for a second lease and then
// answer libnetwork with an address libnetwork did not allocate. So this
// builds the link, binds it to the RESERVED record the reservation
// opened, and hands Join the lease that is already in hand.
//
// It answers with an EMPTY Interface. libnetwork refuses a driver that
// returns an address or a MAC it did not ask for when its own IPAM
// already allocated one, and here it always has.
func (p *Plugin) createIPAMEndpoint(ctx context.Context, r CreateEndpointRequest, opts DHCPNetworkOptions, binding *ipamBinding) (CreateEndpointResponse, error) {
	res := CreateEndpointResponse{Interface: &EndpointInterface{}}
	mode := opts.effectiveMode()
	// Belt for D49. CreateNetwork refuses ipvlan in IPAM mode, so no
	// network that reaches here can be one -- unless a future edit makes
	// one, and the failure it would cause (libnetwork's generated MAC
	// refused by the ipvlan driver at the link) says nothing about IPAM.
	if err := ipamRefuseIPvlan(mode); err != nil {
		return res, err
	}

	if r.Interface == nil || r.Interface.MacAddress == "" {
		return res, fmt.Errorf("%w: Docker created this endpoint without a MAC address, which this plugin's IPAM driver asks for (RequiresMACAddress). This is a Docker-side inconsistency; re-create the container", util.ErrIPAM)
	}
	mac, err := net.ParseMAC(r.Interface.MacAddress)
	if err != nil {
		return res, util.ErrMACAddress
	}
	if r.Interface.Address == "" {
		return res, fmt.Errorf("%w: Docker created this endpoint with no IPv4 address after allocating one from this plugin", util.ErrIPAM)
	}
	want, err := netip.ParsePrefix(r.Interface.Address)
	if err != nil {
		return res, fmt.Errorf("%w: Docker gave this endpoint the address %q, which is not an address with a prefix", util.ErrIPAM, r.Interface.Address)
	}

	// The reservation, consumed. A miss is REFUSED and not recovered
	// from: the lease belongs to this process's memory, and the two ways
	// to be here without it -- the plugin restarted since RequestAddress,
	// or the sweeper already retained the record -- both mean the address
	// Docker is holding is one nothing has claimed at the server. A
	// second exchange would hand the container a different address than
	// the one Docker published for it.
	rsv, ok := p.ipamReserves.take(ipamReserveKey(binding.PoolID, mac))
	if !ok {
		return res, fmt.Errorf("%w: no reservation is held for %v on this network. The plugin was restarted between Docker allocating the address and creating the container; run `docker start` again", util.ErrIPAM, mac)
	}
	if rsv.err != nil {
		return res, rsv.err
	}
	if rsv.addr.Addr() != want.Addr() {
		return res, fmt.Errorf("%w: Docker created this endpoint with %v while the lease reserved for %v is %v", util.ErrIPAM, want.Addr(), mac, rsv.addr.Addr())
	}
	if rsv.record == "" {
		return res, fmt.Errorf("%w: the reservation for %v has no lease record, so its address could not survive to Join", util.ErrIPAM, mac)
	}

	hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)

	remove, err := p.addIPAMEndpointLink(ctx, r.EndpointID, mode, opts, mac)
	if err != nil {
		return res, err
	}

	// The fold onto the RESERVED record, not a new one. recordCreated
	// mints a fresh id every time it is called, and a second record for
	// one endpoint would leave the reserved one for the sweeper while
	// Join resumed the newer, lease-less one -- a DISCOVER instead of an
	// INIT-REBOOT, under an address Docker has already published.
	if err := p.recordCreatedOn(rsv.record, r.NetworkID, endpointRecordKey(mode, r.EndpointID, mac)); err != nil {
		remove()
		return res, err
	}

	ip, err := netlink.ParseAddr(rsv.info.IP)
	if err != nil {
		remove()
		return res, fmt.Errorf("failed to parse the reserved address %q: %w", rsv.info.IP, err)
	}
	gateway := rsv.info.Gateway
	if opts.Gateway != "" {
		gateway = opts.Gateway
	}
	p.updateJoinHint(r.EndpointID, func(h *joinHint) {
		h.RecordID = rsv.record
		h.MacAddress = mac
		h.IPv4 = ip
		h.Gateway = gateway
		// Option-121 routes as the reservation's exchange saw them. They
		// are carried in memory rather than re-read from the record
		// because a lease.Lease has no route list: the record would give
		// back the address and the gateway and silently drop the rest.
		h.Routes = dhcpStaticRoutes(rsv.info.Routes)
	})

	p.rememberEndpoint(r.EndpointID, endpointFingerprint{
		MAC:    mac.String(),
		IPv4:   want.Addr().String(),
		Ifname: p.hintIfname(r.EndpointID),
	}, hostname)

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"mode":     mode,
		"ip":       rsv.info.IP,
		"gateway":  gateway,
	}).Info("Endpoint created from the address this plugin's IPAM driver reserved for it")

	return res, nil
}

// addIPAMEndpointLink builds the endpoint's host-side link under the MAC
// libnetwork generated, and returns its removal for the failure paths.
//
// The names come from vethPairNames and subLinkName, the same two the
// null path uses, because Join and DeleteEndpoint both resolve the link
// by name and neither one knows which IPAM driver the network has.
func (p *Plugin) addIPAMEndpointLink(ctx context.Context, endpointID, mode string, opts DHCPNetworkOptions, mac net.HardwareAddr) (func(), error) {
	if mode == ModeMacvlan || mode == ModeIPvlan {
		parent, err := validateParentForChild(opts.Parent)
		if err != nil {
			return nil, err
		}
		la := netlink.NewLinkAttrs()
		la.Name = subLinkName(endpointID)
		la.ParentIndex = parent.Attrs().Index
		la.HardwareAddr = mac
		link := newChildLink(mode, la)
		// The gate across the LinkAdd alone, exactly as the null path
		// takes it: what it waits out is the preflight probe's round
		// trip, and nothing here holds the parent past the add.
		guard := p.lockParent(ctx, opts.Parent, "create_endpoint")
		err = addChildLink(guard, link)
		guard.Unlock()
		if err != nil {
			return nil, explainChildLinkAdd(err, mode, opts.Parent, parent.Attrs().Index)
		}
		remove := func() {
			if err := netlink.LinkDel(link); err != nil {
				log.WithError(err).WithField("link", la.Name).Warn("Endpoint link cleanup failed; remove it with `ip link del`")
			}
		}
		if _, err := linkUpAwaitingAddress(ctx, link, childLinkUpBudget); err != nil {
			remove()
			return nil, fmt.Errorf("failed to set %v link up: %w", mode, err)
		}
		return remove, nil
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return nil, fmt.Errorf("failed to get bridge interface: %w", err)
	}
	hostName, ctrName := vethPairNames(endpointID)
	la := netlink.NewLinkAttrs()
	la.Name = hostName
	hostLink := &netlink.Veth{LinkAttrs: la, PeerName: ctrName, PeerHardwareAddr: mac}
	if err := netlink.LinkAdd(hostLink); err != nil {
		return nil, fmt.Errorf("failed to create veth pair: %w", err)
	}
	remove := func() {
		if err := netlink.LinkDel(hostLink); err != nil {
			log.WithError(err).WithField("link", hostName).Warn("Endpoint link cleanup failed; remove it with `ip link del`")
		}
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		remove()
		return nil, fmt.Errorf("failed to set host side link of veth pair up: %w", err)
	}
	ctrLink, err := netlink.LinkByName(ctrName)
	if err != nil {
		remove()
		return nil, fmt.Errorf("failed to find container side of veth pair: %w", err)
	}
	if err := netlink.LinkSetUp(ctrLink); err != nil {
		remove()
		return nil, fmt.Errorf("failed to set container side link of veth pair up: %w", err)
	}
	if err := netlink.LinkSetMaster(hostLink, bridge); err != nil {
		remove()
		return nil, fmt.Errorf("failed to attach host side link of veth peer to bridge: %w", err)
	}
	return remove, nil
}

// recordCreatedOn folds CREATE onto a record that already exists.
//
// NO IDENTITY IS SENT, and that is what lets this one call serve both
// reserve paths. The record already carries one -- written by the
// reserve, or, when the reserve re-bound a tombstone, written by the
// endpoint that first held the address. The second case is the one that
// makes re-sending wrong: a re-bind keeps the old identity precisely so
// the server still recognises the client, while this call's MAC would
// derive a new one, and the fold refuses a second identity that differs
// from the first (lease/record.go, the write-once check). The CHAddr is
// sent, because it is the key Join resumes under and it is the one
// identifying field the fold lets change.
func (p *Plugin) recordCreatedOn(recordID, networkID string, mac net.HardwareAddr) error {
	if p.records == nil || recordID == "" {
		return nil
	}
	if err := p.records.Created(recordID, networkID, mac, nil); err != nil {
		return fmt.Errorf("failed to bind this endpoint to the address reserved for it: %w", err)
	}
	return nil
}

// ipamSweeper runs the orphan sweep until the plugin shuts down.
func (p *Plugin) ipamSweeper(stop <-chan struct{}) {
	t := time.NewTicker(ipamSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			p.sweepIPAMReservations(now)
		}
	}
}
