// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func (p *Plugin) renewalWiring(o *dhcp.DHCPClientOptions, networkID, endpointID string, v6 bool) {
	if p == nil {
		return
	}
	o.OnRenewalStats = p.renewalReporter(networkID, endpointID, v6)
}

// renewalReporter adds a manager's unanswered-renewal delta to the counter and logs a
// WARN naming the endpoint, which the plugin-wide counter cannot (#940).
func (p *Plugin) renewalReporter(networkID, endpointID string, v6 bool) func(dhcp.RenewalStats) {
	return func(s dhcp.RenewalStats) {
		if s.Unanswered == 0 {
			return
		}
		addUint64(familyCounter(&p.renewalsUnansweredV4, &p.renewalsUnansweredV6, v6), s.Unanswered)
		log.WithFields(log.Fields{
			"network":    shortID(networkID),
			"endpoint":   shortID(endpointID),
			"family":     familyLabel(v6),
			"unanswered": s.Unanswered,
		}).Warn("The DHCP server did not answer this endpoint's renewal request; the client is " +
			"retransmitting on RFC 2131 section 4.4.5's schedule. The container keeps its address " +
			"until the lease runs out, at which point it loses it.")
	}
}

func familyCounter(v4Counter, v6Counter intCounter, v6 bool) intCounter {
	if v6 {
		return v6Counter
	}
	return v4Counter
}
