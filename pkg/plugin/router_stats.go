// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"

// addRouterStats adds one manager's RFC 4861 router-discovery counter delta to the
// process totals, since a sum over live managers would fall when a container stops (#814).
func (p *Plugin) addRouterStats(d dhcp.RouterStats) {
	addUint64(&p.routerSolicitsSent, d.SolicitsSent)
	addUint64(&p.routerAdvertsSeen, d.AdvertsSeen)
	addUint64(&p.routerAdvertsRefused, d.AdvertsRefused)
	addUint64(&p.routerAdvertOptionsIgnored, d.OptionsIgnored)
	addUint64(&p.routerTableEntriesDropped, d.EntriesDropped)
	addUint64(&p.routerTableEntriesEvicted, d.EntriesEvicted)
}
