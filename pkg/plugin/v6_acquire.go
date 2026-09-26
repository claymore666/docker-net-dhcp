// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// v6Acquire is what the bridge and the parent-attached sites hand the DHCPv6 one-shot (#960).
type v6Acquire struct {
	iface       string
	networkID   string
	endpointID  string
	callStart   time.Time
	timeout     time.Duration
	pol         serverPolicy
	identity6   dhcp.Identity6
	recordID6   string
	preferredV6 string
}

// acquireInitialV6 runs an endpoint's DHCPv6 one-shot on the site's shared base and returns the address, or "" when
// the segment explains its absence (#868, #960).
func (p *Plugin) acquireInitialV6(ctx context.Context, opts DHCPNetworkOptions, base dhcp.DHCPClientOptions, a v6Acquire) (string, error) {
	if err := p.v6Wiring(&base, opts, a.identity6, a.recordID6, a.preferredV6, a.endpointID); err != nil {
		return "", err
	}
	if err := p.conflictWiring(&base, opts, roleAcquire, a.networkID, a.endpointID, true); err != nil {
		return "", err
	}

	// The v6 half runs second and gets what is left of the daemon's deadline; see v6AcquisitionDeadline.
	acqCtx, endV6 := withV6AcquisitionDeadline(ctx, a.callStart)
	defer endV6()
	info, ra, err := p.acquireWithPolicy(acqCtx, a.iface, a.pol, true, a.timeout, a.endpointID, base)
	if err != nil {
		// No DHCPv6 address is fatal only where the segment advertised managed DHCPv6 (#868).
		if p.noteV6Absence(ra, a.iface, a.endpointID, err, base.Mode6) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get initial IPv6 address via DHCPv6: %w", err)
	}
	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return "", fmt.Errorf("failed to parse initial IPv6 address: %w", err)
	}

	p.updateJoinHint(a.endpointID, func(hint *joinHint) {
		hint.IPv6 = ip
		// DHCPv6 has no gateway option, so the v6 gateway is the advertisement's link-local source (#821).
		fillV6Hint(hint, info)
	})
	return info.IP, nil
}
