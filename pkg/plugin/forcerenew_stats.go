// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// addForcerenewStats adds one manager's FORCERENEW and Reconfigure counter delta to the process totals, since a sum
// over live managers would fall when a container stops (#1119).
func (p *Plugin) addForcerenewStats(d dhcp.ForcerenewStats) {
	addUint64(&p.forcerenewsRenewed, d.Renewed)
	addUint64(&p.forcerenewsAlreadyRenewing, d.AlreadyRenewing)
	addUint64(&p.forcerenewsAckRefused, d.AckRefused)
	addUint64(&p.forcerenewsRefused, d.Refused)
	addUint64(&p.reconfiguresAccepted, d.ReconfiguresAccepted)
	addUint64(&p.reconfiguresRefused, d.ReconfiguresRefused)
}

func (p *Plugin) forcerenewWiring(o *dhcp.DHCPClientOptions, networkID, endpointID string) {
	if p == nil {
		return
	}
	o.OnForcerenewStats = p.forcerenewReporter(networkID, endpointID)
}

// forcerenewReporter folds a manager's delta into the totals and logs one line naming the endpoint, which the
// plugin-wide counters cannot: info while only obeyed messages moved, a warning once any refusal did (#1119).
func (p *Plugin) forcerenewReporter(networkID, endpointID string) func(dhcp.ForcerenewStats) {
	return func(d dhcp.ForcerenewStats) {
		if d.IsZero() {
			return
		}
		p.addForcerenewStats(d)
		entry := log.WithFields(log.Fields{
			"network":               shortID(networkID),
			"endpoint":              shortID(endpointID),
			"renewed":               d.Renewed,
			"already_renewing":      d.AlreadyRenewing,
			"refused":               d.Refused,
			"ack_refused":           d.AckRefused,
			"reconfigures_accepted": d.ReconfiguresAccepted,
			"reconfigures_refused":  d.ReconfiguresRefused,
		})
		if d.AnyRefused() {
			entry.Warn("The client refused a DHCP server message under the FORCERENEW or Reconfigure rules; the fields " +
				"hold the counts. A refusal is not a fault by itself: a lease resumed after a restart holds no " +
				"reconfigure key, an unauthenticated FORCERENEW is discarded as RFC 3203 section 4 requires, and an " +
				"ACK without the authentication its OFFER promised is discarded and the acquisition starts over " +
				"(RFC 6704 section 3.1.4).")
			return
		}
		entry.Info("The DHCP server told this endpoint to renew now (FORCERENEW or Reconfigure); the client obeyed.")
	}
}
