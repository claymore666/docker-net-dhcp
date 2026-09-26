// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The kernel's link MTU bounds, measured on veth, macvlan and ipvlan (6.12, 2026-09-24): 67 and 65536 are refused;
// 68 is RFC 791 section 3.2's minimum datagram (#1037).
const (
	minOptionMTU = 68
	maxOptionMTU = 65535
)

var hostNetHandle = &netlink.Handle{}

// validateMTUOption refuses an mtu outside the kernel's bounds or beside propagate_mtu=true, a second source (#1037).
func validateMTUOption(opts DHCPNetworkOptions) error {
	if opts.MTU == 0 {
		return nil
	}
	if opts.MTU < minOptionMTU || opts.MTU > maxOptionMTU {
		return fmt.Errorf("%w: mtu=%d is outside %d..%d", util.ErrIPAM, opts.MTU, minOptionMTU, maxOptionMTU)
	}
	if opts.PropagateMTU {
		return fmt.Errorf("%w: mtu=%d cannot be combined with propagate_mtu=true: each sets the link MTU, so set one",
			util.ErrIPAM, opts.MTU)
	}
	return nil
}

// mtuUnderParent refuses an mtu above the parent's or bridge's MTU at network creation; of the three link kinds the
// kernel refuses it only on a macvlan child (measured 6.12, 2026-09-24, #1037).
func mtuUnderParent(mtu int, parent netlink.Link) error {
	if mtu == 0 || mtu <= parent.Attrs().MTU {
		return nil
	}
	return fmt.Errorf("%w: mtu=%d is above the MTU of %v, %d", util.ErrIPAM, mtu, parent.Attrs().Name, parent.Attrs().MTU)
}

// applyEndpointMTU sets the mtu option on each of a new endpoint's links in the plugin's namespace; 0 sets nothing.
func applyEndpointMTU(mtu int, links ...netlink.Link) error {
	if mtu == 0 {
		return nil
	}
	for _, l := range links {
		if err := nlHandleLinkSetMTU(hostNetHandle, l, mtu); err != nil {
			return fmt.Errorf("failed to set mtu=%d on %v: %w", mtu, l.Attrs().Name, err)
		}
	}
	return nil
}
