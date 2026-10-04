// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"

	cerrdefs "github.com/containerd/errdefs"
	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// dropStaleNetworks removes the file, pool binding and retained records of a network persisted before this start that
// Docker no longer has. Only the start-up snapshot is a candidate, so a network created since is never dropped before
// Docker's store lists it; only not-found counts, as in networkGone, so an unreachable daemon keeps everything. The
// snapshot is consumed by the first pass that holds a list (#1174).
func (p *Plugin) dropStaleNetworks(ctx context.Context, live []dNetwork.Summary) (dropped int) {
	p.staleMu.Lock()
	candidates := p.persistedAtStart
	p.persistedAtStart = nil
	p.staleMu.Unlock()
	if len(candidates) == 0 || p.docker == nil {
		return 0
	}

	listed := make(map[string]bool, len(live))
	for _, n := range live {
		listed[n.ID] = true
	}
	var gone []string
	for _, id := range candidates {
		if listed[id] {
			continue
		}
		netCtx, cancel := context.WithTimeout(ctx, recoveryPerNetworkTimeout)
		_, err := p.docker.NetworkInspect(netCtx, id, dNetwork.InspectOptions{})
		cancel()
		if !cerrdefs.IsNotFound(err) {
			continue
		}
		if p.dropStaleNetwork(id) {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		p.closeRetainedOn(gone)
	}
	return len(gone)
}

// A deleteOptions failure keeps the binding: the file rebinds at the next start and this pass runs again (#1174).
func (p *Plugin) dropStaleNetwork(id string) bool {
	held := ipamBindingOf(id) != nil
	if err := deleteOptions(id); err != nil {
		log.WithError(err).WithField("network", shortID(id)).
			Warn("A network Docker no longer has could not have its persisted file removed; it is retried at the next start")
		return false
	}
	p.ipamIndex.unbindNetwork(id)
	p.staleNetworksDropped.Add(1)
	log.WithFields(log.Fields{
		"network":   shortID(id),
		"ipam_pool": held,
	}).Info("Dropped the persisted state of a network Docker no longer has, removed while this plugin was not running")
	return true
}

func (p *Plugin) closeRetainedOn(networks []string) {
	if p.records == nil {
		return
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		log.WithError(err).Warn("Could not read the lease records back; a removed network's held records stay for the sweep")
		return
	}
	want := make(map[string]bool, len(networks))
	for _, id := range networks {
		want[id] = true
	}
	for _, rec := range rb.Records {
		if rec.Phase != lease.PhaseRetained {
			continue
		}
		if id, _ := dhcp.NetworkOfScope(rec.Scope); want[id] {
			p.closeRecord(rec.ID)
		}
	}
}
