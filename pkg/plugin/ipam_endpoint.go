// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func ipamBindingOf(networkID string) *ipamBinding {
	sn, err := loadNetwork(networkID)
	if err != nil {
		return nil
	}
	return sn.Binding
}

// createIPAMEndpoint runs no v4 exchange, since RequestAddress already leased that address, and answers only the v6
// address, since libnetwork refuses a v4 address or MAC it did not ask for (#110, #960).
func (p *Plugin) createIPAMEndpoint(ctx context.Context, callStart time.Time, r CreateEndpointRequest, opts DHCPNetworkOptions, binding *ipamBinding) (CreateEndpointResponse, error) {
	res := CreateEndpointResponse{Interface: &EndpointInterface{}}
	mode := opts.effectiveMode()
	// CreateNetwork refuses ipvlan in IPAM mode: the kernel rejects libnetwork's generated MAC on an ipvlan link
	// (#110).
	if err := ipamRefuseIPvlan(mode); err != nil {
		return res, err
	}
	if err := ipamRefusePassthru(opts); err != nil {
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

	// A missing reservation is refused: a second exchange would lease an address other than the one Docker published
	// (#110).
	rsv, ok := p.ipamReserves.take(ipamReserveKey(binding.PoolID, mac))
	if !ok {
		return res, fmt.Errorf("%w: no reservation is held for %v on this network. The plugin was restarted between Docker allocating the address and creating the container; run `docker start` again", util.ErrIPAM, mac)
	}
	// From here every exit returns the record through ipamGiveUpRecord: the sweeper can no longer
	// reach it, and its lease is real at the server (#110).
	giveUp := func() { p.ipamGiveUpRecord(rsv.record, time.Now()) }
	if rsv.err != nil {
		giveUp()
		return res, rsv.err
	}
	if rsv.addr.Addr() != want.Addr() {
		giveUp()
		return res, fmt.Errorf("%w: Docker created this endpoint with %v while the lease reserved for %v is %v", util.ErrIPAM, want.Addr(), mac, rsv.addr.Addr())
	}
	if rsv.record == "" {
		return res, fmt.Errorf("%w: the reservation for %v has no lease record, so its address could not survive to Join", util.ErrIPAM, mac)
	}

	hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)

	remove, err := p.addIPAMEndpointLink(ctx, r.EndpointID, mode, opts, mac)
	if err != nil {
		giveUp()
		return res, err
	}

	// Fold onto the reserved record: a second record would make Join send a DISCOVER instead of an INIT-REBOOT (#110).
	if err := p.recordCreatedOn(rsv.record, r.NetworkID, endpointRecordKey(mode, r.EndpointID, mac)); err != nil {
		remove()
		giveUp()
		return res, err
	}

	ip, err := netlink.ParseAddr(rsv.info.IP)
	if err != nil {
		remove()
		giveUp()
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
		// Routes come from memory because a lease.Lease carries no route list (#110).
		h.Routes = dhcpStaticRoutes(rsv.info.Routes)
	})

	// The v6 half is the last step that can fail, after the v4 record is folded, so a fatal verdict gives up both
	// records and no failure follows a v6 lease (#960).
	var v6IP string
	if opts.ipv6Enabled() {
		addr, err := p.createIPAMEndpointV6(ctx, callStart, r, opts, mac, rsv.record6, hostname)
		if err != nil {
			remove()
			giveUp()
			return res, err
		}
		res.Interface.AddressIPv6 = addr
		p.updateJoinHint(r.EndpointID, func(h *joinHint) {
			if h.IPv6 != nil {
				v6IP = h.IPv6.IP.String()
			}
		})
	}

	p.rememberEndpoint(r.EndpointID, endpointFingerprint{
		MAC:    mac.String(),
		IPv4:   want.Addr().String(),
		IPv6:   v6IP,
		Ifname: p.hintIfname(r.EndpointID),
	}, hostname)

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"mode":     mode,
		"ip":       rsv.info.IP,
		"ipv6":     res.Interface.AddressIPv6,
		"gateway":  gateway,
	}).Info("Endpoint created from the address this plugin's IPAM driver reserved for it")

	return res, nil
}

// createIPAMEndpointV6 runs null mode's DHCPv6 one-shot on the IPAM link, on the record the re-bind carried when
// there is one (#960).
func (p *Plugin) createIPAMEndpointV6(ctx context.Context, callStart time.Time, r CreateEndpointRequest, opts DHCPNetworkOptions, mac net.HardwareAddr, rebound string, hostname dhcpHostname) (string, error) {
	recordID6, identity6, preferredV6, err := p.ipamIdentity6(opts, r.NetworkID, r.EndpointID, mac, rebound)
	if err != nil {
		return "", err
	}
	pol, err := resolveServerPolicy(opts)
	if err != nil {
		return "", err
	}
	base := dhcp.DHCPClientOptions{
		Hostname:    hostname.name,
		FQDN:        opts.fqdnMode(),
		VendorClass: opts.VendorClass,
		MAC:         mac,
		Records:     p.records,
	}
	return p.acquireInitialV6(ctx, opts, base, v6Acquire{iface: ipamEndpointIface(opts.effectiveMode(), r.EndpointID),
		networkID: r.NetworkID, endpointID: r.EndpointID, callStart: callStart, timeout: leaseTimeoutFor(opts),
		pol: pol, identity6: identity6, recordID6: recordID6, preferredV6: preferredV6})
}

// ipamIdentity6 keeps the DUID and IAID of the record the re-bind moved to this MAC, since RFC 9915 section 11 says a
// DUID "SHOULD NOT change over time"; with none it mints on the MAC the endpoint has now, as null mode does (#960).
func (p *Plugin) ipamIdentity6(opts DHCPNetworkOptions, networkID, endpointID string, mac net.HardwareAddr, rebound string) (string, dhcp.Identity6, string, error) {
	if rebound != "" {
		if id6, preferred, ok := p.reboundIdentity6(rebound); ok {
			return rebound, id6, preferred, nil
		}
	}
	id6, err := resolveIdentity6(opts, endpointID, mac)
	if err != nil {
		return "", dhcp.Identity6{}, "", err
	}
	return p.recordCreated6(networkID, endpointRecordKey(opts.effectiveMode(), endpointID, mac), id6), id6, "", nil
}

// reboundIdentity6 reads the re-bound record's identity and last address, which the one-shot asks for again.
func (p *Plugin) reboundIdentity6(id string) (dhcp.Identity6, string, bool) {
	if p.records == nil {
		return dhcp.Identity6{}, "", false
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithField("record", id).
			Warn("Could not read the lease records back; this endpoint gets a fresh DHCPv6 identity and the server sees a new client")
		return dhcp.Identity6{}, "", false
	}
	rec, ok := rb.ByID(id)
	if !ok || rec.Phase != lease.PhaseCreated {
		return dhcp.Identity6{}, "", false
	}
	id6, err := dhcp.ParseIdentity6(rec.Identity)
	if err != nil {
		log.WithError(err).WithField("record", id).
			Warn("The re-bound DHCPv6 identity could not be read back; this endpoint gets a fresh one and the server sees a new client")
		return dhcp.Identity6{}, "", false
	}
	preferred := ""
	if a, ok := rec.Addr(); ok {
		preferred = a.String()
	}
	return id6, preferred, true
}

// ipamEndpointIface is the container-side link addIPAMEndpointLink makes, which the DHCPv6 one-shot runs on.
func ipamEndpointIface(mode, endpointID string) string {
	if mode == ModeMacvlan || mode == ModeIPvlan {
		return subLinkName(endpointID)
	}
	_, ctrName := vethPairNames(endpointID)
	return ctrName
}

func (p *Plugin) addIPAMEndpointLink(ctx context.Context, endpointID, mode string, opts DHCPNetworkOptions, mac net.HardwareAddr) (func(), error) {
	if mode == ModeMacvlan || mode == ModeIPvlan {
		// The sub-interface a host reboot lost is made again before the child (#902).
		if _, err := p.ensureVlanLink(ctx, opts, "create_endpoint"); err != nil {
			return nil, err
		}
		parent, err := validateParentForChild(opts.linkParent())
		if err != nil {
			return nil, err
		}
		la := netlink.NewLinkAttrs()
		la.Name = subLinkName(endpointID)
		la.ParentIndex = parent.Attrs().Index
		la.HardwareAddr = mac
		link, err := newChildLink(opts, la)
		if err != nil {
			return nil, err
		}
		guard := p.lockParent(ctx, opts.linkParent(), mode, "create_endpoint")
		err = addChildLink(guard, link)
		guard.Unlock()
		if err != nil {
			return nil, explainChildLinkAdd(err, mode, opts.linkParent(), parent.Attrs().Index)
		}
		remove := func() {
			if err := netlink.LinkDel(link); err != nil {
				log.WithError(err).WithField("link", la.Name).Warn("Endpoint link cleanup failed; remove it with `ip link del`")
			}
		}
		if err := applyEndpointMTU(opts.MTU, link); err != nil {
			remove()
			return nil, err
		}
		if _, err := linkUpAwaitingAddress(ctx, link, childLinkUpBudget); err != nil {
			remove()
			return nil, fmt.Errorf("failed to set %v link up: %w", mode, err)
		}
		return remove, nil
	}

	// A host reboot loses the bridge made from parent while Docker keeps the network (#903).
	if _, err := p.ensureBridge(ctx, opts, "create_endpoint"); err != nil {
		return nil, err
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
	if err := applyEndpointMTU(opts.MTU, hostLink, &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: ctrName}}); err != nil {
		remove()
		return nil, err
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

// recordCreatedOn sends no identity, since the record's write-once identity may predate this MAC (#110).
func (p *Plugin) recordCreatedOn(recordID, networkID string, mac net.HardwareAddr) error {
	if p.records == nil || recordID == "" {
		return nil
	}
	if err := p.records.Created(recordID, networkID, mac, nil); err != nil {
		return fmt.Errorf("failed to bind this endpoint to the address reserved for it: %w", err)
	}
	return nil
}
