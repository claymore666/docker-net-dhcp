// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// renewalWiring points one persistent client's unanswered-renewal
// reports at the process-wide counter for its family.
//
// The persistent client only. A CreateEndpoint one-shot holds no lease
// to renew, so there is nothing for it to report and no second writer
// of this counter; see DHCPClientOptions.OnRenewalStats.
//
// The nil check is on the plugin, as in conflictWiring: a dhcpManager
// built for a unit test carries none, and a client with nowhere to
// report to still has to run.
func (p *Plugin) renewalWiring(o *dhcp.DHCPClientOptions, networkID, endpointID string, v6 bool) {
	if p == nil {
		return
	}
	o.OnRenewalStats = p.renewalReporter(networkID, endpointID, v6)
}

// renewalReporter builds the callback that turns one manager's gain in
// unanswered renewal requests into the plugin's counter and the
// operator's log line.
//
// THE LOG LINE IS HALF THE POINT. #940 is a production host on which
// the DHCP server stopped answering renewals for 7h52m while
// /Plugin.Health read healthy, dhcp_timeouts read 0, and the plugin log
// carried nothing at any level. A counter answers the operator who is
// already looking at a dashboard; the line is what the operator who is
// reading logs at 3am has to find. It names the endpoint, because the
// counter is plugin-wide and cannot.
//
// It is WARN and not ERROR: the container still holds a working
// address, and the failure this reports is recoverable by the server
// coming back. It becomes an outage, and dhcp_timeouts, only if the
// retransmissions run out.
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

// familyCounter picks the half of a family pair this event belongs to.
//
// It returns the counter rather than bumping it, which bumpFamily does,
// because a library delta is added with addUint64's saturation and not
// with a bare Add: the health surface is int32 and the library counts
// in uint64.
func familyCounter(v4Counter, v6Counter intCounter, v6 bool) intCounter {
	if v6 {
		return v6Counter
	}
	return v4Counter
}
