// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const clientIDOption = "client_id"

type clientIDClaim struct {
	networkID   string
	id          string
	perEndpoint bool
}

// parseClientIDOption validates the per-endpoint client_id under the network option's bound (#219).
func parseClientIDOption(options map[string]interface{}) (string, error) {
	raw, ok := options["client_id"]
	if !ok {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("invalid %s %v: expected non-empty string: %w", clientIDOption, raw, util.ErrIPAM)
	}
	if n := len(dhcp.ClientIdentity([]byte(s))); n > maxOptionOctets {
		return "", fmt.Errorf("%w: client_id is %d octets, the most option 61 carries is %d beside its type byte",
			util.ErrIPAM, n-1, maxOptionOctets-1)
	}
	return s, nil
}

// endpointClientID is the replay's carried id, else the request's option.
func endpointClientID(r CreateEndpointRequest) (string, error) {
	if r.clientID != "" {
		return r.clientID, nil
	}
	return parseClientIDOption(r.Options)
}

// applyEndpointClientID puts the endpoint's client_id over the network's in opts and claims the effective id before
// any DHCP packet. A shared id is refused when either side set it per endpoint, and on ipvlan, where the second
// container got the first one's address and failed with "address already in use" (measured 2026-10-28, #219); on
// bridge and macvlan a shared network client_id stays allowed, since a server keyed on chaddr serves both.
func (p *Plugin) applyEndpointClientID(ctx context.Context, r CreateEndpointRequest, opts *DHCPNetworkOptions, ipam bool) error {
	own, err := endpointClientID(r)
	if err != nil {
		return err
	}
	if ipam {
		if own != "" {
			return fmt.Errorf("%w: the %s driver option needs a network created with --ipam-driver null; in IPAM mode "+
				"the address is leased before the endpoint exists", util.ErrIPAM, clientIDOption)
		}
		return nil
	}
	if own != "" {
		opts.ClientID = own
		p.updateJoinHint(r.EndpointID, func(h *joinHint) { h.ClientID = own })
	}
	if opts.ClientID == "" {
		return nil
	}
	claim := clientIDClaim{networkID: r.NetworkID, id: opts.ClientID, perEndpoint: own != ""}
	holder, refused := p.claimClientID(r.EndpointID, claim, opts.effectiveMode() == ModeIPvlan)
	if holder == "" {
		return nil
	}
	name := p.endpointContainerName(ctx, r.NetworkID, holder)
	if !refused {
		log.WithFields(log.Fields{
			"network":   shortID(r.NetworkID),
			"endpoint":  shortID(r.EndpointID),
			"holder":    shortID(holder),
			"container": name,
		}).Warn("Another container on this network sends the same client_id; a DHCP server that matches clients by " +
			"client_id gives both one address. Set a per-container client_id driver option to tell them apart")
		return nil
	}
	if name == "" {
		name = "endpoint " + shortID(holder)
	} else {
		name = fmt.Sprintf("container %s (endpoint %s)", name, shortID(holder))
	}
	return fmt.Errorf("%w: client_id %q is already in use on this network by %s; a DHCP server gives two "+
		"containers with one client_id one address. Give each container its own client_id driver option",
		util.ErrIPAM, opts.ClientID, name)
}

// claimClientID inserts the claim unless another endpoint's claim on the network forbids sharing it, in one step under
// p.mu so two concurrent creates cannot both pass; holder is the other endpoint, refused whether c was not inserted.
func (p *Plugin) claimClientID(endpointID string, c clientIDClaim, refuseShared bool) (holder string, refused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for other, oc := range p.clientIDClaims {
		if other == endpointID || oc.networkID != c.networkID || oc.id != c.id {
			continue
		}
		holder = other
		if refuseShared || c.perEndpoint || oc.perEndpoint {
			return holder, true
		}
	}
	if p.clientIDClaims == nil {
		p.clientIDClaims = map[string]clientIDClaim{}
	}
	p.clientIDClaims[endpointID] = c
	return holder, false
}

func (p *Plugin) releaseClientIDClaim(endpointID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clientIDClaims, endpointID)
}

// reclaimClientID restores a recovered endpoint's claim from its record, since claims live in memory only.
func (p *Plugin) reclaimClientID(networkID, endpointID string, mac net.HardwareAddr, opts DHCPNetworkOptions) {
	id, own := p.recordedClientID(networkID, endpointID, mac, opts)
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clientIDClaims == nil {
		p.clientIDClaims = map[string]clientIDClaim{}
	}
	p.clientIDClaims[endpointID] = clientIDClaim{networkID: networkID, id: id, perEndpoint: own}
}

// recordedClientID is the endpoint's recorded option-61 payload when it is not the derived default, and whether the
// network's client_id cannot have produced it, so the per-endpoint option did.
func (p *Plugin) recordedClientID(networkID, endpointID string, mac net.HardwareAddr, opts DHCPNetworkOptions) (string, bool) {
	_, res := p.recordResume(networkID, endpointRecordKey(opts.effectiveMode(), endpointID, mac))
	payload, ok := dhcp.ClientIDPayload(res.Identity)
	if !ok {
		return "", false
	}
	derived := opts
	derived.ClientID = ""
	if bytes.Equal(payload, resolveClientID(derived, endpointID, mac)) {
		return "", false
	}
	return string(payload), string(payload) != opts.ClientID
}

// endpointContainerName is the container Docker lists for the endpoint, "" when the lookup fails.
func (p *Plugin) endpointContainerName(ctx context.Context, networkID, endpointID string) string {
	if p.docker == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	n, err := inspectNetwork(ctx, p.docker, networkID)
	if err != nil {
		return ""
	}
	for _, c := range n.Containers {
		if c.EndpointID == endpointID {
			return c.Name
		}
	}
	return ""
}
