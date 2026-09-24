// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// refuseWithoutUserMAC is require_mac's check; a replayed endpoint already exists and carries no options (#1036).
func refuseWithoutUserMAC(opts DHCPNetworkOptions, r CreateEndpointRequest) error {
	if !opts.RequireMAC || r.replay || userSetMAC(r) {
		return nil
	}
	return fmt.Errorf("%w: require_mac is set on this network, so a container on it must set its own MAC address: `docker run --mac-address`, or `mac_address` in Compose. The `docker network connect` command line cannot set a MAC address, so a connect from it is refused here",
		util.ErrIPAM)
}

// userSetMAC matches libnetwork's user-MAC option, base64 of the MAC, to the interface MAC; `--driver-opt` can set the
// key as text (measured 2026-09-24, engines 26.1.4 and 29.8.1, #1036).
func userSetMAC(r CreateEndpointRequest) bool {
	s, _ := r.Options[ipamOptMacAddress].(string)
	if s == "" || r.Interface == nil {
		return false
	}
	marked, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	mac, err := net.ParseMAC(r.Interface.MacAddress)
	return err == nil && bytes.Equal(marked, mac)
}

// ipamDropRefusedReservation closes a refused endpoint's fresh record, so no start re-binds it, and returns a re-bound
// one to its window; the lease expires at the server, with no DHCPRELEASE (#962, #1036).
func (p *Plugin) ipamDropRefusedReservation(r CreateEndpointRequest, binding *ipamBinding) {
	if r.Interface == nil {
		return
	}
	mac, err := net.ParseMAC(r.Interface.MacAddress)
	if err != nil {
		return
	}
	rsv, ok := p.ipamReserves.take(ipamReserveKey(binding.PoolID, mac))
	if !ok || rsv.err != nil || rsv.record == "" {
		return
	}
	p.ipamGiveUpAttempt(rsv.record, rsv.rebound, time.Now())
	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"record":   rsv.record,
		"rebound":  rsv.rebound,
	}).Info("require_mac refused this endpoint; the address reserved for it is forgotten and not reused")
}
