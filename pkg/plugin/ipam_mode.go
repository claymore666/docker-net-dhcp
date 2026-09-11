// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// errIPAMBindingLost is an IPAM-mode network whose pool binding this
// process cannot read.
//
// IT IS A REFUSAL AND NOT A FALLBACK, and that is the whole point of the
// error existing. netOptionsRaw serves a network from the Docker API
// whenever the state file cannot be read -- a corrupt file, a transient
// EIO, or a schema written by a newer build -- and the API is
// authoritative for everything in DHCPNetworkOptions. It is not
// authoritative for the pool binding, because the binding is not
// Docker's: it is what CreateNetwork learned about which pool this
// network holds. An IPAM-mode network served without it is served on the
// null-mode path, which runs its own DHCP exchange, writes a JSON
// tombstone the IPAM shape does not use, and answers libnetwork with an
// address libnetwork already allocated and will refuse. Every one of
// those is silent at the plugin and arrives at the user as something
// else. So the fallback stops here.
var errIPAMBindingLost = errors.New("this network's IPAM pool binding could not be read; refusing rather than serving it as a null-IPAM network")

// ipamIndex maps a PoolID to the network bound to it.
//
// REBUILT FROM THE STATE DIRECTORY AT START-UP AND NOT FROM THE DAEMON.
// The daemon replays RequestPool and one RequestAddress per stored
// endpoint from inside libnetwork.New, which NewDaemon calls before the
// API serves; a lookup that asked Docker there would be asking a server
// that is not listening yet. One file per network is already the record,
// so the index is a fold of it.
type ipamIndex struct {
	mu sync.Mutex
	m  map[string]string
}

func newIPAMIndex() *ipamIndex { return &ipamIndex{m: map[string]string{}} }

// A NIL INDEX IS EMPTY AND NOT A PANIC, on every method below. A Plugin
// built literally -- which is how most of this package's tests build one
// -- has no index, and the null-IPAM path must behave the same with or
// without one. A nil receiver reading as "no network is bound to any
// pool" is exactly right for that: it is the truth on a plugin that
// never answered a RequestPool.
func (x *ipamIndex) bind(poolID, networkID string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.m[poolID] = networkID
}

func (x *ipamIndex) unbindNetwork(networkID string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for id, n := range x.m {
		if n == networkID {
			delete(x.m, id)
		}
	}
}

// network resolves a PoolID to the network bound to it, by the EXACT
// PoolID string. Two networks that share a subnet on two parents differ
// only in the suffix one of them typed, so a prefix match would hand the
// second network's requests to the first.
func (x *ipamIndex) network(poolID string) (string, bool) {
	if x == nil {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	n, ok := x.m[poolID]
	return n, ok
}

// boundTo reports the network holding this PoolID when it is not the one
// asking. The row-6 refusal at CreateNetwork.
func (x *ipamIndex) boundTo(poolID, exceptNetworkID string) (string, bool) {
	if x == nil {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	n, ok := x.m[poolID]
	if !ok || n == exceptNetworkID {
		return "", false
	}
	return n, true
}

func (x *ipamIndex) len() int {
	if x == nil {
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.m)
}

// rebuildIPAMIndex folds the state directory into the PoolID index.
//
// A network whose file cannot be read is LOGGED AND SKIPPED rather than
// failing start-up: one unreadable file must not stop a host's other
// networks from coming back. The cost of the skip is that the network's
// endpoint calls then meet errIPAMBindingLost, which is the refusal this
// file exists for and is exactly the right outcome.
func rebuildIPAMIndex(x *ipamIndex) {
	if x == nil {
		return
	}
	ids, err := listStateNetworks()
	if err != nil {
		log.WithError(err).Warn("Could not list the state directory; IPAM-mode networks will refuse until their state is readable")
		return
	}
	for _, id := range ids {
		sn, err := loadNetwork(id)
		if err != nil {
			log.WithError(err).WithField("network", shortID(id)).
				Warn("Could not read a persisted network; if it is in IPAM mode its endpoint calls will be refused")
			continue
		}
		if sn.Binding == nil {
			continue
		}
		x.bind(sn.Binding.PoolID, id)
	}
}

// ipamNetwork reads one network's stored record for an IPAM handler.
//
// DISK ONLY, NO DOCKER CLIENT. Every IPAM RPC can arrive during the
// daemon's start-up replay, before the daemon's own API serves, so a
// handler that asked Docker anything would deadlock exactly where the
// replay needs an answer. The refusal on an unreadable file is the same
// refusal errIPAMBindingLost names.
func ipamNetwork(networkID string) (storedNetwork, error) {
	sn, err := loadNetwork(networkID)
	if err != nil {
		return sn, fmt.Errorf("%w: %v: %w", errIPAMBindingLost, networkID, err)
	}
	if sn.Binding == nil {
		return sn, fmt.Errorf("%w: %v has no binding on disk", errIPAMBindingLost, networkID)
	}
	return sn, nil
}

// ipamRecordPhases are the record phases whose address a RequestAddress
// replay may be answered from.
//
// THE FILTER IS HERE BECAUSE THE LOOKUP DOES NOT APPLY ONE. lease
// Rebuilt.ByScopeAddr matches on scope and address, and Record.Addr
// reads the lease with no phase test at all, so a CLOSED record -- the
// phase closeRecord writes when CreateEndpoint fails after opening one
// -- still answers by its address. Answering a replay from one would
// report a hit for an address nothing holds, and ipam_replay_miss, whose
// whole job is to make that visible, would never move. The library says
// the narrowing is the caller's (lease/rebuild.go, ByScopeAddr's
// comment); this is the caller applying it.
//
// RETAINED is excluded for the opposite reason: a tombstone's address is
// a re-bind CANDIDATE, which the reserve branch consumes through
// Tombstones and its deadline. Treating it as a live endpoint here would
// hand one address to two endpoints.
func ipamRecordPhases() []lease.Phase {
	return []lease.Phase{
		lease.PhaseReserved,
		lease.PhaseCreated,
		lease.PhaseJoined,
		lease.PhaseLeft,
		lease.PhaseAdopted,
	}
}

func ipamPhaseAnswers(p lease.Phase) bool {
	for _, want := range ipamRecordPhases() {
		if p == want {
			return true
		}
	}
	return false
}

// ipamLiveRecord is the record that answers for one address in one
// network, or none.
//
// NEWEST WINS among the survivors of the phase filter. More than one
// record can carry an address over time -- a tombstone and the record
// that succeeded it share one -- and Rebuild returns records in creation
// order, so the last match is the current one.
func ipamLiveRecord(rb lease.Rebuilt, networkID string, addr netip.Addr) (lease.Record, bool) {
	matches := rb.ByScopeAddr(networkID, addr)
	for i := len(matches) - 1; i >= 0; i-- {
		if ipamPhaseAnswers(matches[i].Phase) {
			return matches[i], true
		}
	}
	return lease.Record{}, false
}

// ipamRefuseIPvlan is D49: ipvlan in IPAM mode is refused where the
// network is created, not where an endpoint fails.
//
// libnetwork generates a MAC for every endpoint once an IPAM driver
// declares RequiresMACAddress, and the ipvlan branch of CreateEndpoint
// refuses any supplied MAC because ipvlan children share the parent's.
// So every ipvlan endpoint in IPAM mode would fail at container start
// with a MAC error that says nothing about IPAM. Refusing at
// `docker network create` costs an ipvlan operator the IPAM shape and
// nothing they have: `--ipam-driver null` is unchanged and keeps working.
func ipamRefuseIPvlan(mode string) error {
	if mode != ModeIPvlan {
		return nil
	}
	return fmt.Errorf("%w: ipvlan networks cannot use this plugin as an IPAM driver in v2.1.0, because ipvlan children share the parent's MAC and Docker's IPAM contract requires a per-endpoint one. Create the network with --ipam-driver null instead, which is unchanged and supported. Progress on ipvlan in IPAM mode is tracked in issue #949", util.ErrIPAM)
}
