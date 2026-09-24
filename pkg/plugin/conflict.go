// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"math"

	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

type clientRole int

const (
	roleAcquire clientRole = iota

	// roleJoin is the persistent client started at Join, resuming the address through INIT-REBOOT.
	roleJoin
)

// mode maps want onto the role: at Join the address is already in use, so ConflictWait runs as
// ConflictAsync, same probes and DHCPDECLINE (RFC 5227 section 2.1), where holding back cost ~6 s
// per start (measured 2026-09-04); ConflictOff stays off (#901).
func (r clientRole) mode(want proto.ConflictMode) proto.ConflictMode {
	if r == roleJoin && want == proto.ConflictWait {
		return proto.ConflictAsync
	}
	return want
}

// conflictWiring sets the RFC 5227 mode from the network's stored options, and the callbacks, for all three client
// sites (#901).
func (p *Plugin) conflictWiring(o *dhcp.DHCPClientOptions, opts DHCPNetworkOptions, role clientRole, networkID, endpointID string, v6 bool) error {
	mode, err := dhcp.ParseConflictCheck(opts.ConflictCheck)
	if err != nil {
		return fmt.Errorf("invalid persisted conflict_check: %w", err)
	}
	o.ConflictMode = role.mode(mode)
	if p == nil {
		return nil
	}
	o.OnConflict = p.conflictReporter(networkID, endpointID, v6)
	o.OnACDStats = p.addACDStats
	return nil
}

// conflictReporter logs endpoint, network and address for a conflict (#524); the library's event
// carries no foreign MAC, and a v6 conflict is DAD, not RFC 5227.
func (p *Plugin) conflictReporter(networkID, endpointID string, v6 bool) func(dhcp.Conflict) {
	return func(c dhcp.Conflict) {
		bumpFamily(&p.addressConflictsV4, &p.addressConflictsV6, v6)
		fields := log.Fields{
			"network":  shortID(networkID),
			"endpoint": shortID(endpointID),
			"held":     c.Held,
			"family":   familyLabel(v6),
		}
		if c.Addr != "" {
			fields["address"] = c.Addr
		}
		if c.Note != "" {
			fields["detail"] = c.Note
		}
		log.WithFields(fields).Error(conflictMessage(c.Held, v6))
	}
}

func familyLabel(v6 bool) string {
	if v6 {
		return "ipv6"
	}
	return "ipv4"
}

// conflictMessage spells out four literal lines because the harness counts them in the log across
// restarts, and TestConflictMsgsMatchTheSource keeps the copies equal (#901).
func conflictMessage(held, v6 bool) string {
	switch {
	case held && v6:
		return "The IPv6 address this endpoint HOLDS was found in use by another node on the link " +
			"(RFC 4862 section 5.4 Duplicate Address Detection). It has been declined to the DHCPv6 " +
			"server (RFC 9915 section 18.2.8) and the container's IPv6 address will CHANGE; " +
			"connections on the old address are already broken for both hosts."
	case v6:
		return "The IPv6 address this endpoint was offered is already in use on the link " +
			"(RFC 4862 section 5.4 Duplicate Address Detection). It was declined to the DHCPv6 " +
			"server (RFC 9915 section 18.2.8) and a different address will be requested."
	case held:
		return "The address this endpoint HOLDS was found in use by another device on the segment " +
			"(RFC 5227 section 2.4). It has been declined and the container's address will CHANGE; " +
			"connections on the old address are already broken for both hosts."
	default:
		return "The address this endpoint was offered is already in use on the segment (RFC 5227). " +
			"It was declined and a different address will be requested. The DHCP server cannot see " +
			"statically configured hosts, so an address inside the pool range will be handed out again."
	}
}

// addACDStats adds one manager's RFC 5227 counter delta to the process totals, since a sum over
// live managers would fall when a container stops (#901).
func (p *Plugin) addACDStats(d dhcp.ACDStats) {
	addUint64(&p.acdProbesSent, d.ProbesSent)
	addUint64(&p.acdAnnouncementsSent, d.AnnouncementsSent)
	addUint64(&p.acdConflictsDetected, d.ConflictsDetected)
	addUint64(&p.acdARPSendFailures, d.ARPSendFailures)
}

// addUint64 saturates at the int32 health field's maximum, since a wrapped value reads as a counter reset.
func addUint64(c intCounter, d uint64) {
	if d == 0 {
		return
	}
	if d > math.MaxInt32 {
		d = math.MaxInt32
	}
	cur := c.Load()
	if int64(cur)+int64(d) > math.MaxInt32 {
		c.Store(math.MaxInt32)
		return
	}
	c.Add(int32(d))
}
