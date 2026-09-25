// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

// ipvlan support was inspired by LANCommander/docker-net-dhcp, which added both modes side by side (#46).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func subLinkName(endpointID string) string {
	prefix := endpointID
	if len(endpointID) > 12 {
		prefix = endpointID[:12]
	}
	return "dh-" + prefix
}

func validateParentForChild(name string) (netlink.Link, error) {
	link, err := nlLinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup parent interface %v: %w", name, err)
	}
	switch link.Type() {
	case "bridge", "macvlan", "macvtap", "ipvlan":
		return nil, fmt.Errorf("%w: %v is %v", util.ErrParentInvalid, name, link.Type())
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("%w: %v", util.ErrParentDown, name)
	}
	return link, nil
}

// newChildLink builds the child in the network's macvlan_mode or ipvlan_mode, so the endpoint, the IPAM reservation,
// the validate_dhcp probe and a replay after a plugin restart all build the stored sub-mode (#905).
func newChildLink(opts DHCPNetworkOptions, la netlink.LinkAttrs) (netlink.Link, error) {
	if opts.effectiveMode() == ModeIPvlan {
		m, err := parseIPvlanMode(opts.IPvlanMode)
		if err != nil {
			return nil, err
		}
		return &netlink.IPVlan{LinkAttrs: la, Mode: m}, nil
	}
	m, err := parseMacvlanMode(opts.MacvlanMode)
	if err != nil {
		return nil, err
	}
	return &netlink.Macvlan{LinkAttrs: la, Mode: m}, nil
}

// explainChildLinkAdd names the kind in the way when the kernel refuses a child with EBUSY: macvlan and ipvlan both
// claim the parent's single rx_handler, so the second kind is refused while same-kind children coexist (#486).
// Not a retry, since two kinds on one NIC is permanent.
func explainChildLinkAdd(err error, mode, parent string, parentIndex int) error {
	// A passthru child takes the parent alone, and the kernel refuses the next child EINVAL, measured on Linux 6.12;
	// EINVAL has other causes, so the text names passthru as one (#905).
	if mode == ModeMacvlan && errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("failed to create macvlan link on %q: %w. The kernel answers this when a "+
			"macvlan_mode=passthru child holds the parent: a passthru network gives its parent to one container, so "+
			"a second child is refused beside it, and a passthru child is refused while another macvlan child is on "+
			"the parent. If so, stop the container that holds %q, or put this network on another parent",
			parent, err, parent)
	}
	if !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("failed to create %v link: %w", mode, err)
	}

	occupant, known := childLinkKind(parentIndex)
	if !known {
		// An unreadable link table is reported apart from a parent that carries neither kind (#802).
		return fmt.Errorf("failed to create %v link on %q: %w — the parent would not "+
			"accept another child, and its link table could not be read, so whether it "+
			"already carries %v children is unknown; check with `ip -d link show` and "+
			"retry", mode, parent, err, otherChildMode(mode))
	}
	if occupant == "" || occupant == mode {
		return fmt.Errorf("failed to create %v link on %q: %w — the parent would not "+
			"accept another child; if a %v network is being torn down on the same "+
			"parent, retry once it has finished", mode, parent, err, otherChildMode(mode))
	}

	return fmt.Errorf("failed to create %v link on %q: %w — %q already carries %v "+
		"children, and a parent interface can be a macvlan port or an ipvlan port "+
		"but not both. Put the %v network on a different parent interface, or move "+
		"both networks to the same mode",
		mode, parent, err, parent, occupant, mode)
}

// childLinkKind reports the child kind on this parent and whether the link table could be read (#802).
func childLinkKind(parentIndex int) (kind string, known bool) {
	links, err := util.DumpResult(nlLinkList())
	if err != nil {
		return "", false
	}
	for _, l := range links {
		if l.Attrs().ParentIndex != parentIndex {
			continue
		}
		switch l.Type() {
		case "macvlan":
			return ModeMacvlan, true
		case "ipvlan":
			return ModeIPvlan, true
		}
	}
	return "", true
}

func otherChildMode(mode string) string {
	if mode == ModeIPvlan {
		return ModeMacvlan
	}
	return ModeIPvlan
}

// childLinkUpBudget covers Docker finishing a DeleteEndpoint it has begun, within the engine's creation patience
// (#408).
const childLinkUpBudget = 3 * time.Second

const childLinkUpInterval = 150 * time.Millisecond

// linkUpAwaitingAddress retries LinkSetUp on EADDRINUSE: the kernel refuses a macvlan child whose MAC is live on the
// parent, and a restart re-applies the previous endpoint's MAC before DeleteEndpoint removed the old child (#408).
// No fallback address: the MAC is what brings the lease back. It retries on the kernel's answer, since a child in a
// dying netns holds the address but is absent from the host link list. It reports whether it waited (#422).
func linkUpAwaitingAddress(ctx context.Context, link netlink.Link, budget time.Duration) (bool, error) {
	deadline := time.Now().Add(budget)
	waited := false
	for {
		err := nlLinkSetUp(link)
		if err == nil {
			return waited, nil
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			return waited, err
		}
		// From here the departing link held the address, so this call met the #408 window.
		waited = true
		if !time.Now().Before(deadline) {
			return waited, fmt.Errorf("%w (the address is still held by the link this one replaces, "+
				"after waiting %v for it to be removed)", err, budget)
		}
		select {
		case <-ctx.Done():
			return waited, fmt.Errorf("%w (last attempt: %w)", ctx.Err(), err)
		case <-time.After(childLinkUpInterval):
		}
	}
}

// noteRestartLinkUpWait records a #408 wait; neither counter affects healthy, since a timeout surfaces through
// CreateEndpoint (#422).
func (p *Plugin) noteRestartLinkUpWait(r CreateEndpointRequest, waited bool, err error) {
	if !waited {
		return
	}
	fields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"budget":   childLinkUpBudget.String(),
	}
	if err != nil {
		p.restartLinkUpTimeouts.Add(1)
		log.WithError(err).WithFields(fields).
			Warn("Child link never got the address back; the departing link still holds it")
		return
	}
	p.restartLinkUpWaited.Add(1)
	log.WithFields(fields).
		Info("Child link came up after waiting out the departing link's address (#408)")
}

func (p *Plugin) createParentAttachedEndpoint(ctx context.Context, callStart time.Time, r CreateEndpointRequest, opts DHCPNetworkOptions) (CreateEndpointResponse, error) {
	res := CreateEndpointResponse{Interface: &EndpointInterface{}}
	mode := opts.effectiveMode()

	if _, err := p.ensureVlanLink(ctx, opts, "create_endpoint"); err != nil {
		return res, err
	}
	parent, err := validateParentForChild(opts.linkParent())
	if err != nil {
		return res, err
	}

	// ipvlan children share the parent's MAC, so no tombstone MAC applies and an explicit MAC is refused; --ip is option 50 in both modes.
	effectiveMAC := ""
	if r.Interface != nil {
		effectiveMAC = r.Interface.MacAddress
	}
	// A MAC set on a passthru child changes the parent's own, so a user MAC is refused (#905).
	if opts.macvlanPassthru() && effectiveMAC != "" {
		return res, fmt.Errorf("%w: macvlan_mode=passthru does not support a custom MAC address: the child wears the parent's MAC, and a MAC set on it would change the parent's", util.ErrMACAddress)
	}
	explicitV4, err := resolveExplicitV4(r)
	if err != nil {
		return res, err
	}
	explicitV6, err := resolveExplicitV6(r)
	if err != nil {
		return res, err
	}
	hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)

	requestedIP := explicitV4
	requestedV6 := explicitV6
	if mode == ModeMacvlan && effectiveMAC == "" {
		if tombMAC, tombIP, tombIPv6, ok := p.consumeTombstone(r.NetworkID, hostname); ok {
			// The kernel ignores a passthru child's create address, and the pin below sets the parent's (#905).
			if !opts.macvlanPassthru() {
				effectiveMAC = tombMAC
			}
			if requestedIP == "" {
				requestedIP = tombIP
			}
			// The prior v6 address is the DHCPv6 preferred address, so a restart keeps its v6 lease like v4 (#213).
			if requestedV6 == "" {
				requestedV6 = tombIPv6
			}
			log.WithFields(log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
				"hostname": hostname.name,
			}).Info("Inherited MAC/IP from recent endpoint on same network (likely container restart)")
			log.WithFields(log.Fields{
				"network":      shortID(r.NetworkID),
				"endpoint":     shortID(r.EndpointID),
				"mac_address":  tombMAC,
				"requested_ip": requestedIP,
				"prior_ipv6":   tombIPv6,
			}).Debug("Tombstone inheritance details")
		}
	}

	la := netlink.NewLinkAttrs()
	la.Name = subLinkName(r.EndpointID)
	la.ParentIndex = parent.Attrs().Index
	if effectiveMAC != "" {
		// ipvlan children share the parent's MAC, so a custom MAC would be ignored silently and is refused.
		if mode == ModeIPvlan {
			return res, fmt.Errorf("%w: ipvlan does not support a custom MAC address (children share the parent's MAC)", util.ErrMACAddress)
		}
		mac, err := net.ParseMAC(effectiveMAC)
		if err != nil {
			return res, util.ErrMACAddress
		}
		la.HardwareAddr = mac
	}
	link, err := newChildLink(opts, la)
	if err != nil {
		return res, err
	}

	// Queues behind the validate_dhcp probe, which holds the parent across a DHCP round trip, for the LinkAdd only
	// (#549).
	guard := p.lockParent(ctx, opts.linkParent(), mode, "create_endpoint")
	if opts.macvlanPassthru() {
		var waited bool
		waited, err = retryPassthruAdd(ctx, childLinkUpBudget, childLinkUpInterval, func() error { return addChildLink(guard, link) })
		if waited {
			log.WithError(err).WithFields(log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
			}).Info("Passthru child waited for the parent's previous child to go (#905)")
		}
	} else {
		err = addChildLink(guard, link)
	}
	guard.Unlock()
	if err != nil {
		return res, explainChildLinkAdd(err, mode, opts.linkParent(), parent.Attrs().Index)
	}

	var (
		recordID  string
		recordID6 string
		identity6 dhcp.Identity6
	)

	if err := func() error {
		fresh, err := netlink.LinkByName(la.Name)
		if err != nil {
			return fmt.Errorf("failed to re-fetch %v link: %w", mode, err)
		}
		if err := applyEndpointMTU(opts.MTU, fresh); err != nil {
			return err
		}
		mac := fresh.Attrs().HardwareAddr

		// Pin the kernel-assigned macvlan MAC (#103): udev's MACAddressPolicy=persistent, the Debian default,
		// replaces a randomly assigned MAC just after creation, and a set addr_assign_type stops it. Without the pin
		// the one-shot DHCPv6 poisoned the server's neighbour cache for about 45 s on the capture. ipvlan refuses any
		// MAC set with EOPNOTSUPP. A passthru child is pinned to the parent's own MAC, which leaves the parent as it is
		// (measured on Linux 6.12); unpinned, a rewrite of the child would change the parent's (#905).
		if opts.effectiveMode() != ModeIPvlan && effectiveMAC == "" {
			if err := netlink.LinkSetHardwareAddr(fresh, mac); err != nil {
				return fmt.Errorf("failed to pin %v link MAC: %w", mode, err)
			}
		}

		waited, err := linkUpAwaitingAddress(ctx, fresh, childLinkUpBudget)
		p.noteRestartLinkUpWait(r, waited, err)
		if err != nil {
			return fmt.Errorf("failed to set %v link up: %w", mode, err)
		}

		// libnetwork sets MacAddress at Join, which an ipvlan slave refuses with EOPNOTSUPP even for its own MAC; a
		// passthru child already wears the parent's, pinned above (#905).
		if !opts.childWearsParentMAC() && (r.Interface == nil || r.Interface.MacAddress == "") {
			res.Interface.MacAddress = mac.String()
		}

		timeout := leaseTimeoutFor(opts)
		// Client-id from the MAC for macvlan and from the endpoint ID for ipvlan, whose slaves share the parent MAC
		// (#371).
		clientID := resolveClientID(opts, r.EndpointID, mac)

		recordID = p.recordCreated(r.NetworkID,
			endpointRecordKey(mode, r.EndpointID, mac), dhcp.ClientIdentity(clientID))
		p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
			hint.RecordID = recordID
		})

		// An ipvlan slave inherits the parent's MAC, so its DUID is endpoint-derived (#895).
		if opts.ipv6Enabled() {
			id6, err := resolveIdentity6(opts, r.EndpointID, mac)
			if err != nil {
				return err
			}
			identity6 = id6
			recordID6 = p.recordCreated6(r.NetworkID,
				endpointRecordKey(mode, r.EndpointID, mac), id6)
		}

		runDHCP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			// Server preference ladder (#111) and deny-list (#669), shared with the bridge path.
			pol, err := resolveServerPolicy(opts)
			if err != nil {
				return err
			}

			base := dhcp.DHCPClientOptions{
				Hostname:    hostname.name,
				FQDN:        opts.fqdnMode(),
				ClientID:    clientID,
				VendorClass: opts.VendorClass,
				// The MAC keys the v4 lease and the v6 DUID-LL, except on ipvlan, where both come from the endpoint
				// (#152, #895).
				MAC:      mac,
				Records:  p.records,
				RecordID: recordID,
			}
			if v6 {
				if err := p.v6Wiring(&base, opts, identity6, recordID6, requestedV6, r.EndpointID); err != nil {
					return err
				}
			}
			if err := p.conflictWiring(&base, opts, roleAcquire, r.NetworkID, r.EndpointID, v6); err != nil {
				return err
			}
			if !v6 {
				base.RequestedIP = requestedIP
			}

			var (
				info dhcp.Info
				ra   dhcp.RAObservation
			)
			if v6 {
				acqCtx, endV6 := withV6AcquisitionDeadline(ctx, callStart)
				defer endV6()
				info, ra, err = p.acquireWithPolicy(acqCtx, la.Name, pol, true, timeout, r.EndpointID, base)
			} else {
				info, err = p.acquireV4(ctx, opts, callStart, la.Name, pol, timeout, r.EndpointID, base)
			}
			if err != nil {
				// No DHCPv6 address is fatal only where the segment advertised managed DHCPv6 (#868).
				if v6 && p.noteV6Absence(ra, la.Name, r.EndpointID, err, base.Mode6) {
					return nil
				}
				return fmt.Errorf("failed to get initial IP%v address via DHCP%v: %w", v6str, v6str, err)
			}
			addr, err := netlink.ParseAddr(info.IP)
			if err != nil {
				return fmt.Errorf("failed to parse initial IP%v address: %w", v6str, err)
			}

			p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
				hint.MacAddress = mac
				if v6 {
					res.Interface.AddressIPv6 = info.IP
					hint.IPv6 = addr
					// DHCPv6 has no gateway option, so the v6 gateway is the advertisement's link-local source (#821).
					fillV6Hint(hint, info)
				} else {
					res.Interface.Address = info.IP
					hint.IPv4 = addr
					hint.Gateway = info.Gateway
					// No gateway on link-local, as in bridge mode (#904).
					if opts.Gateway != "" && !isLinkLocalAddr(addr) {
						hint.Gateway = opts.Gateway
					}
					// DHCP option-121 classless static routes (RFC 3442);
					// any default route was already folded into
					// info.Gateway by the parser.
					hint.Routes = dhcpStaticRoutes(info.Routes)
				}
			})
			return nil
		}

		if err := runDHCP(false); err != nil {
			return err
		}
		if opts.ipv6Enabled() {
			if err := runDHCP(true); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		// Best-effort rollback: a link LinkDel misses goes with its netns.
		p.closeRecord(recordID)
		p.closeRecord(recordID6)
		_ = netlink.LinkDel(link)
		return res, err
	}

	var hintMAC, hintGW, hintIPv4, hintIPv6 string
	p.updateJoinHint(r.EndpointID, func(h *joinHint) {
		hintMAC = h.MacAddress.String()
		hintGW = h.Gateway
		if h.IPv4 != nil {
			hintIPv4 = h.IPv4.IP.String()
		}
		if h.IPv6 != nil {
			hintIPv6 = h.IPv6.IP.String()
		}
	})

	if mode == ModeMacvlan {
		p.rememberEndpoint(r.EndpointID, endpointFingerprint{MAC: hintMAC, IPv4: hintIPv4, IPv6: hintIPv6, Ifname: p.hintIfname(r.EndpointID)}, hostname)
	}

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"mode":     mode,
		"parent":   opts.linkParent(),
	}).Info("Endpoint created")
	log.WithFields(log.Fields{
		"network":     shortID(r.NetworkID),
		"endpoint":    shortID(r.EndpointID),
		"mac_address": hintMAC,
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     hintGW,
	}).Debug("Endpoint details")

	return res, nil
}

// deleteParentAttachedEndpoint removes a child still in the host netns; once moved, the kernel removes it with the netns.
func (p *Plugin) deleteParentAttachedEndpoint(r DeleteEndpointRequest) error {
	name := subLinkName(r.EndpointID)
	// Through the rename guard like the bridge teardown, though nothing renames a child link today (#1051).
	link, err := hostLinkByGeneratedName(name)
	if err != nil {
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
		}).Debug("Child link already gone (expected)")
		return nil
	}
	if err := nlLinkDel(link); err != nil {
		return fmt.Errorf("failed to delete leftover child link %v: %w", name, err)
	}
	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Cleaned up leftover child link in host netns")
	return nil
}

func findLinkByMAC(handle linkLister, mac net.HardwareAddr) (netlink.Link, error) {
	links, err := util.DumpResult(handle.LinkList())
	if err != nil {
		return nil, fmt.Errorf("failed to list links: %w", err)
	}
	for _, l := range links {
		if bytes.Equal(l.Attrs().HardwareAddr, mac) {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no link with MAC %v", mac)
}

// parentAttachedOperInfo is the EndpointOperInfo answer for macvlan and ipvlan endpoints.
type parentAttachedOperInfo struct {
	Mode     string `mapstructure:"mode"`
	Parent   string `mapstructure:"parent"`
	HostLink string `mapstructure:"sub_link_host"`
	LinkMAC  string `mapstructure:"sub_link_mac"`
}

func (p *Plugin) parentAttachedEndpointOperInfo(opts DHCPNetworkOptions, r InfoRequest) (InfoResponse, error) {
	res := InfoResponse{}
	name := subLinkName(r.EndpointID)

	info := parentAttachedOperInfo{
		Mode:     opts.effectiveMode(),
		Parent:   opts.linkParent(),
		HostLink: name,
	}
	if link, err := hostLinkByGeneratedName(name); err == nil {
		info.LinkMAC = link.Attrs().HardwareAddr.String()
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode oper info: %w", err)
	}
	return res, nil
}
