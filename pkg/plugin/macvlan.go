package plugin

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

// macvlanLinkName returns the host-side macvlan link name for an endpoint.
// Mirrors the prefix used for the bridge-mode veth so existing log/diag
// patterns still apply.
func macvlanLinkName(endpointID string) string {
	return "dh-" + endpointID[:12]
}

// validateMacvlanParent ensures the parent NIC exists, is up, and is itself a
// suitable parent for a macvlan child (i.e. not already a bridge or macvlan).
// We do not change the parent's state — the host's NIC config is off-limits.
func validateMacvlanParent(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup parent interface %v: %w", name, err)
	}
	switch link.Type() {
	case "bridge", "macvlan", "macvtap":
		return nil, fmt.Errorf("%w: %v is %v", util.ErrParentInvalid, name, link.Type())
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("%w: %v", util.ErrParentDown, name)
	}
	return link, nil
}

// createMacvlanEndpoint creates the per-endpoint macvlan child on the host's
// parent NIC, runs udhcpc on it (still in host netns) to acquire an initial
// lease, and stashes the result for Join. Docker will move the link into the
// container's netns when it acts on our Join response.
func (p *Plugin) createMacvlanEndpoint(ctx context.Context, r CreateEndpointRequest, opts DHCPNetworkOptions) (CreateEndpointResponse, error) {
	res := CreateEndpointResponse{Interface: &EndpointInterface{}}

	parent, err := validateMacvlanParent(opts.Parent)
	if err != nil {
		return res, err
	}

	la := netlink.NewLinkAttrs()
	la.Name = macvlanLinkName(r.EndpointID)
	la.ParentIndex = parent.Attrs().Index
	if r.Interface != nil && r.Interface.MacAddress != "" {
		mac, err := net.ParseMAC(r.Interface.MacAddress)
		if err != nil {
			return res, util.ErrMACAddress
		}
		la.HardwareAddr = mac
	}
	link := &netlink.Macvlan{
		LinkAttrs: la,
		Mode:      netlink.MACVLAN_MODE_BRIDGE,
	}

	if err := netlink.LinkAdd(link); err != nil {
		return res, fmt.Errorf("failed to create macvlan link: %w", err)
	}

	if err := func() error {
		// Reload to pick up the kernel-assigned MAC if we didn't set one.
		fresh, err := netlink.LinkByName(la.Name)
		if err != nil {
			return fmt.Errorf("failed to re-fetch macvlan link: %w", err)
		}
		mac := fresh.Attrs().HardwareAddr

		if err := netlink.LinkSetUp(fresh); err != nil {
			return fmt.Errorf("failed to set macvlan link up: %w", err)
		}

		if r.Interface == nil || r.Interface.MacAddress == "" {
			res.Interface.MacAddress = mac.String()
		}

		timeout := defaultLeaseTimeout
		if opts.LeaseTimeout != 0 {
			timeout = opts.LeaseTimeout
		}

		runDHCP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			tCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			info, err := udhcpc.GetIP(tCtx, la.Name, &udhcpc.DHCPClientOptions{V6: v6})
			if err != nil {
				return fmt.Errorf("failed to get initial IP%v address via DHCP%v: %w", v6str, v6str, err)
			}
			addr, err := netlink.ParseAddr(info.IP)
			if err != nil {
				return fmt.Errorf("failed to parse initial IP%v address: %w", v6str, err)
			}

			hint := p.joinHints[r.EndpointID]
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
			p.joinHints[r.EndpointID] = hint
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
		// Roll back the macvlan link if anything after LinkAdd failed.
		netlink.LinkDel(link)
		return res, err
	}

	log.WithFields(log.Fields{
		"network":     r.NetworkID[:12],
		"endpoint":    r.EndpointID[:12],
		"parent":      opts.Parent,
		"mac_address": p.joinHints[r.EndpointID].MacAddress.String(),
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     p.joinHints[r.EndpointID].Gateway,
	}).Info("Macvlan endpoint created")

	return res, nil
}

// deleteMacvlanEndpoint best-effort cleans up the host-side macvlan link.
// Once Docker has moved the link into the container netns the host can no
// longer see it, and the kernel removes it when the netns dies — so a
// "not found" here is the normal happy path. We only delete when the link
// is still in our netns (e.g. CreateEndpoint failed mid-way or Join was
// never called).
func (p *Plugin) deleteMacvlanEndpoint(r DeleteEndpointRequest) error {
	name := macvlanLinkName(r.EndpointID)
	link, err := netlink.LinkByName(name)
	if err != nil {
		// Expected: the link is gone with the container netns.
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
		}).Debug("Macvlan link already gone (expected)")
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete leftover macvlan link %v: %w", name, err)
	}
	log.WithFields(log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
	}).Info("Cleaned up leftover macvlan link in host netns")
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

// macvlanOperInfo is what we hand back to libnetwork in EndpointOperInfo.
type macvlanOperInfo struct {
	Mode      string `mapstructure:"mode"`
	Parent    string `mapstructure:"parent"`
	HostLink  string `mapstructure:"macvlan_host"`
	LinkMAC   string `mapstructure:"macvlan_mac"`
}

func (p *Plugin) macvlanEndpointOperInfo(opts DHCPNetworkOptions, r InfoRequest) (InfoResponse, error) {
	res := InfoResponse{}
	name := macvlanLinkName(r.EndpointID)

	info := macvlanOperInfo{
		Mode:     ModeMacvlan,
		Parent:   opts.Parent,
		HostLink: name,
	}
	// The link is in the container netns by the time anyone polls this, so
	// "not found" is expected and not an error.
	if link, err := netlink.LinkByName(name); err == nil {
		info.LinkMAC = link.Attrs().HardwareAddr.String()
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode macvlan oper info: %w", err)
	}
	return res, nil
}
