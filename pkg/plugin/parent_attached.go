package plugin

// This file implements the macvlan and ipvlan attachment modes. Both
// share the same lifecycle: a child sub-interface is created on a host
// parent NIC, an initial DHCP lease is acquired in the host netns,
// libnetwork moves the link into the container netns. The only
// per-mode difference is the netlink link type and whether the child
// can carry a distinct MAC.
//
// ipvlan support inspired by @LANCommander's fork
// (LANCommander/docker-net-dhcp), which independently added both
// modes side-by-side. Our implementation differs in keeping a separate
// `parent` driver option (instead of overloading `bridge`) and in
// using MAC-based link rediscovery instead of ifindex-based.

import (
	"bytes"
	"context"
	"fmt"
	"net"

	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// subLinkName returns the host-side child link name for an endpoint.
// Mirrors the prefix used for the bridge-mode veth so existing log/diag
// patterns still apply. Used for both macvlan and ipvlan children.
func subLinkName(endpointID string) string {
	return "dh-" + endpointID[:12]
}

// validateParentForChild ensures the parent NIC exists, is up, and is
// itself a suitable parent for a macvlan/ipvlan child (i.e. not already
// a bridge or another macvlan/ipvlan). We do not change the parent's
// state — the host's NIC config is off-limits.
func validateParentForChild(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
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

// newChildLink builds the right netlink.Link for the requested mode.
// macvlan submode is "bridge" so children on the same parent can talk
// to each other. ipvlan submode is L2 so it bridges (rather than
// L3-routes) packets — required for DHCP since DHCP needs L2 broadcast.
func newChildLink(mode string, la netlink.LinkAttrs) netlink.Link {
	if mode == ModeIPvlan {
		return &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
	}
	return &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
}

// createParentAttachedEndpoint creates the per-endpoint child link on
// the host's parent NIC (macvlan or ipvlan depending on mode), runs
// udhcpc on it (still in host netns) to acquire an initial lease, and
// stashes the result for Join. Docker will move the link into the
// container's netns when it acts on our Join response.
func (p *Plugin) createParentAttachedEndpoint(ctx context.Context, r CreateEndpointRequest, opts DHCPNetworkOptions) (CreateEndpointResponse, error) {
	res := CreateEndpointResponse{Interface: &EndpointInterface{}}
	mode := opts.effectiveMode()

	parent, err := validateParentForChild(opts.Parent)
	if err != nil {
		return res, err
	}

	// MAC selection: explicit > tombstone > kernel-picked. ipvlan
	// children share the parent's MAC and ignore HardwareAddr, so
	// the tombstone path doesn't apply there (and an explicit MAC is
	// rejected loudly to avoid silent misconfiguration).
	effectiveMAC := ""
	if r.Interface != nil {
		effectiveMAC = r.Interface.MacAddress
	}
	if mode == ModeMacvlan && effectiveMAC == "" {
		if tomb, ok := p.consumeTombstone(r.NetworkID); ok {
			effectiveMAC = tomb
			log.WithFields(log.Fields{
				"network":     r.NetworkID[:12],
				"endpoint":    r.EndpointID[:12],
				"mac_address": tomb,
			}).Info("Inherited MAC from recent endpoint on same network (likely container restart)")
		}
	}

	la := netlink.NewLinkAttrs()
	la.Name = subLinkName(r.EndpointID)
	la.ParentIndex = parent.Attrs().Index
	if effectiveMAC != "" {
		// ipvlan children share the parent's MAC by design; libnetwork
		// passing us a custom MAC would silently get ignored, so we
		// fail loudly instead. (Tombstones are filtered out above for
		// ipvlan, so reaching this branch in ipvlan mode means the
		// caller really did request a custom MAC.)
		if mode == ModeIPvlan {
			return res, fmt.Errorf("%w: ipvlan does not support a custom MAC address (children share the parent's MAC)", util.ErrMACAddress)
		}
		mac, err := net.ParseMAC(effectiveMAC)
		if err != nil {
			return res, util.ErrMACAddress
		}
		la.HardwareAddr = mac
	}
	link := newChildLink(mode, la)

	if err := netlink.LinkAdd(link); err != nil {
		return res, fmt.Errorf("failed to create %v link: %w", mode, err)
	}

	if err := func() error {
		// Reload to pick up the kernel-assigned MAC (macvlan) or the
		// inherited parent MAC (ipvlan) if we didn't set one.
		fresh, err := netlink.LinkByName(la.Name)
		if err != nil {
			return fmt.Errorf("failed to re-fetch %v link: %w", mode, err)
		}
		mac := fresh.Attrs().HardwareAddr

		if err := netlink.LinkSetUp(fresh); err != nil {
			return fmt.Errorf("failed to set %v link up: %w", mode, err)
		}

		if r.Interface == nil || r.Interface.MacAddress == "" {
			res.Interface.MacAddress = mac.String()
		}

		timeout := defaultLeaseTimeout
		if opts.LeaseTimeout != 0 {
			timeout = opts.LeaseTimeout
		}
		// Best-effort hostname for the initial DISCOVER (so the lease
		// shows up in the upstream DHCP server's UI tagged with the
		// container hostname from minute one) and a stable client-id
		// derived from the endpoint ID (so reservations keyed on
		// option 61 survive container recreation, and so ipvlan
		// children can be told apart even though they all share the
		// parent's MAC).
		hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)
		clientID := clientIDFromEndpoint(r.EndpointID)

		runDHCP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			tCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			info, err := udhcpc.GetIP(tCtx, la.Name, &udhcpc.DHCPClientOptions{
				V6:       v6,
				Hostname: hostname,
				ClientID: clientID,
			})
			if err != nil {
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
				} else {
					res.Interface.Address = info.IP
					hint.IPv4 = addr
					hint.Gateway = info.Gateway
					if opts.Gateway != "" {
						hint.Gateway = opts.Gateway
					}
				}
			})
			return nil
		}

		if err := runDHCP(false); err != nil {
			return err
		}
		if opts.IPv6 {
			if err := runDHCP(true); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		// Roll back the child link if anything after LinkAdd failed.
		// Best-effort: if LinkDel itself fails the kernel will reap the
		// link with the netns soon enough.
		_ = netlink.LinkDel(link)
		return res, err
	}

	var hintMAC, hintGW string
	p.updateJoinHint(r.EndpointID, func(h *joinHint) {
		hintMAC = h.MacAddress.String()
		hintGW = h.Gateway
	})

	// Remember the chosen MAC so DeleteEndpoint can stash it as a
	// tombstone. macvlan only — for ipvlan the MAC is the parent's
	// and there's nothing to stabilize.
	if mode == ModeMacvlan {
		p.rememberEndpointMAC(r.EndpointID, hintMAC)
	}

	log.WithFields(log.Fields{
		"network":     r.NetworkID[:12],
		"endpoint":    r.EndpointID[:12],
		"mode":        mode,
		"parent":      opts.Parent,
		"mac_address": hintMAC,
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     hintGW,
	}).Info("Endpoint created")

	return res, nil
}

// deleteParentAttachedEndpoint best-effort cleans up the host-side
// child link. Once Docker has moved the link into the container netns
// the host can no longer see it, and the kernel removes it when the
// netns dies — so a "not found" here is the normal happy path. We
// only delete when the link is still in our netns (e.g. CreateEndpoint
// failed mid-way or Join was never called). Same code handles macvlan
// and ipvlan since they live under the same name.
func (p *Plugin) deleteParentAttachedEndpoint(r DeleteEndpointRequest) error {
	name := subLinkName(r.EndpointID)
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Expected: the link is gone with the container netns.
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
		}).Debug("Child link already gone (expected)")
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete leftover child link %v: %w", name, err)
	}
	log.WithFields(log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
	}).Info("Cleaned up leftover child link in host netns")
	return nil
}

// findLinkByMAC walks the link table behind `handle` (typically the
// container's netns handle) and returns the link with the given hardware
// address. Used to re-discover a macvlan child after Docker has moved and
// renamed it inside the container.
func findLinkByMAC(handle *netlink.Handle, mac net.HardwareAddr) (netlink.Link, error) {
	links, err := handle.LinkList()
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

// parentAttachedOperInfo is what we hand back to libnetwork in
// EndpointOperInfo for both macvlan and ipvlan endpoints.
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
		Parent:   opts.Parent,
		HostLink: name,
	}
	// The link is in the container netns by the time anyone polls this, so
	// "not found" is expected and not an error.
	if link, err := netlink.LinkByName(name); err == nil {
		info.LinkMAC = link.Attrs().HardwareAddr.String()
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode oper info: %w", err)
	}
	return res, nil
}
