// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"

// addRouterStats folds one manager's gain in the library's RFC 4861
// router-discovery counters into the process-wide ones (#814).
//
// A DELTA, from every DHCPv6 manager including the CreateEndpoint
// one-shots, which is why these are not summed from the live managers
// on demand: a sum over the live set falls when a container stops, and
// a counter that falls is not a counter.
func (p *Plugin) addRouterStats(d dhcp.RouterStats) {
	addUint64(&p.routerSolicitsSent, d.SolicitsSent)
	addUint64(&p.routerAdvertsSeen, d.AdvertsSeen)
	addUint64(&p.routerAdvertsRefused, d.AdvertsRefused)
	addUint64(&p.routerAdvertOptionsIgnored, d.OptionsIgnored)
	addUint64(&p.routerTableEntriesDropped, d.EntriesDropped)
	addUint64(&p.routerTableEntriesEvicted, d.EntriesEvicted)
}
