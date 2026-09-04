// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// clientRole is which of this plugin's two kinds of DHCP client is
// being built, and it exists because the two do not want the same
// conflict mode from the same network option.
type clientRole int

const (
	// roleAcquire is a CreateEndpoint one-shot: no address is
	// configured yet, so "before use" is a real moment and D22's
	// hold-back applies.
	roleAcquire clientRole = iota

	// roleJoin is the persistent client started at Join, resuming
	// through INIT-REBOOT an address CreateEndpoint has already handed
	// to dockerd.
	roleJoin
)

// mode is the conflict mode this role runs under on a network
// configured for want.
//
// THE ONE RULE, AND WHY IT IS NOT A MISMATCH. proto.ConflictWait means
// "hold the lease back until RFC 5227 section 2.1 clears it, because
// the address is not in use yet". At Join it IS in use: CreateEndpoint
// reported it to dockerd seconds ago and the interface is configured
// from that answer. There is no "before use" left to wait for, so
// holding the lease back a second time buys nothing and costs the
// whole probe window on every container start — MEASURED at ~6s on
// the beta lane 2026-09-04, which is what broke the resolv.conf and
// MTU propagation tests: the Join manager reports its lease, and
// everything the plugin writes into the container hangs off that.
//
// This is the library's own argument for the renewal case, applied to
// the resumption case. proto/machine.go, on a renewal that moves the
// address: "The probing runs BESIDE the address in every mode here,
// not before it, including ConflictWait. That is not the mode being
// ignored: the lease was announced seconds or hours ago and cannot be
// un-announced, so there is no 'before use' left to wait for. What
// ConflictWait still buys is the DHCPDECLINE when the check fails,
// which is section 2.4's path and is the same in both."
//
// NOTHING IS DROPPED. proto.ConflictAsync runs the same section 2.1
// probes and the same section 2.4 listener and sends the same
// DHCPDECLINE; the only difference is that the lease is announced
// while they run. proto.ConflictOff stays off in both roles, which is
// the half of defeat row Y-11 that a role-dependent mode could break,
// and TestConflictWiring_TheJoinManagerNeverHoldsTheAddressBack drives
// all three values against both roles.
func (r clientRole) mode(want proto.ConflictMode) proto.ConflictMode {
	if r == roleJoin && want == proto.ConflictWait {
		return proto.ConflictAsync
	}
	return want
}

// conflictWiring fills the RFC 5227 fields on one endpoint's client
// options: the mode the network asked for, and the two callbacks that
// bring the library's findings back to the plugin's counters.
//
// ONE FUNCTION FOR EVERY CLIENT THIS PLUGIN BUILDS. There are three
// call sites — the bridge CreateEndpoint one-shot, the parent-attached
// one-shot and the Join manager — and a mode that reached two of them
// would probe an address before use and then stop listening for the
// life of the container, or the reverse. The wrong version is made
// hard to write rather than documented: the mode is not a parameter
// here, it is read from the network's stored options in the one place,
// and the only thing a caller chooses is which ROLE it is building —
// which clientRole turns into a mode by a rule stated once.
//
// The options were validated at CreateNetwork, so an error means the
// persisted state is corrupt. It is returned rather than defaulted,
// for the same reason resolveServerPolicy refuses: silently starting a
// client in a mode the operator did not ask for is how `off` becomes
// `wait` on a network that chose speed, and nothing would say so.
func (p *Plugin) conflictWiring(o *dhcp.DHCPClientOptions, opts DHCPNetworkOptions, role clientRole, networkID, endpointID string) error {
	mode, err := dhcp.ParseConflictCheck(opts.ConflictCheck)
	if err != nil {
		return fmt.Errorf("invalid persisted conflict_check: %w", err)
	}
	o.ConflictMode = role.mode(mode)
	// THE MODE IS SET EVEN WITH NO PLUGIN BEHIND IT, and the callbacks
	// are not. A dhcpManager built for a unit test carries a nil
	// plugin (see its `plugin` field), and the counters have nowhere
	// to go; the MODE still has to reach the wire, because a test that
	// silently ran every client in the default mode would be testing a
	// configuration this call site cannot produce.
	if p == nil {
		return nil
	}
	o.OnConflict = p.conflictReporter(networkID, endpointID)
	o.OnACDStats = p.addACDStats
	return nil
}

// conflictReporter builds the callback that turns one library conflict
// into the plugin's counter and the operator's log line.
//
// The log carries every fact needed to find the other device, in one
// line: the production incident #524 came from was diagnosed from
// exactly this set, gathered by hand. What it can no longer carry is
// the foreign MAC — the old datagram probe read it out of the kernel's
// neighbour table, and RFC 5227's check does not surface it through the
// library's event. The DHCP server's log has the DHCPDECLINE and the
// segment's ARP tables have the holder; this says which endpoint, which
// network, which address, and whether the container is changing address
// or never had one.
func (p *Plugin) conflictReporter(networkID, endpointID string) func(dhcp.Conflict) {
	return func(c dhcp.Conflict) {
		p.addressConflicts.Add(1)
		fields := log.Fields{
			"network":  shortID(networkID),
			"endpoint": shortID(endpointID),
			"held":     c.Held,
		}
		if c.Addr != "" {
			fields["address"] = c.Addr
		}
		if c.Note != "" {
			fields["detail"] = c.Note
		}
		msg := "The address this endpoint was offered is already in use on the segment (RFC 5227). " +
			"It was declined and a different address will be requested. The DHCP server cannot see " +
			"statically configured hosts, so an address inside the pool range will be handed out again."
		if c.Held {
			msg = "The address this endpoint HOLDS was found in use by another device on the segment " +
				"(RFC 5227 section 2.4). It has been declined and the container's address will CHANGE; " +
				"connections on the old address are already broken for both hosts."
		}
		log.WithFields(fields).Error(msg)
	}
}

// addACDStats folds one manager's gain in the library's RFC 5227
// counters into the process-wide ones.
//
// A DELTA, from every manager including the CreateEndpoint one-shots,
// which is why these are not summed from the live managers on demand: a
// sum over the live set falls when a container stops, and a counter
// that falls is not a counter.
func (p *Plugin) addACDStats(d dhcp.ACDStats) {
	addUint64(&p.acdProbesSent, d.ProbesSent)
	addUint64(&p.acdAnnouncementsSent, d.AnnouncementsSent)
	addUint64(&p.acdConflictsDetected, d.ConflictsDetected)
	addUint64(&p.acdARPSendFailures, d.ARPSendFailures)
}

// addUint64 adds a library counter's delta to a health counter,
// saturating rather than wrapping.
//
// The health surface is int32 throughout and the library counts in
// uint64. A cast would turn a large value negative, and a counter that
// goes negative reads as a reset — which is the one thing a Prometheus
// counter may not do without the series identity changing with it. The
// saturation is unreachable in practice (it needs two billion probes in
// one plugin's life) and is written because the alternative failure is
// silent and this file has no way to know the future.
func addUint64(c *atomic.Int32, d uint64) {
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
