// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

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
	// incomplete records that the start-up fold could not read every
	// network, which is what makes an UNBOUND pool ambiguous. See
	// markIncomplete.
	incomplete bool
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

// markIncomplete says the start-up fold skipped at least one network.
//
// IT IS THE DIFFERENCE BETWEEN A CREATE IN FLIGHT AND A LOST BINDING,
// which is otherwise unanswerable. A RequestAddress for a pool no
// network holds has two causes and they arrive on the same wire: the
// aux addresses libnetwork asks for while a create is still running,
// before CreateNetwork has bound anything, and the daemon's replay of a
// stored endpoint whose network was skipped by rebuildIPAMIndex because
// its file would not read. Echoing the first is correct. Echoing the
// second confirms Docker's stored address from a process that holds no
// record of it, which is the one shape of row A neither replay counter
// can see, because the dispatch never reaches them.
//
// So the fold reports what it could not read, and the unbound branch
// refuses while anything is missing. The cost is borne by the host that
// already has an unreadable state file: on it, a create carrying
// `--aux-address` is refused too, loudly, naming the pool.
func (x *ipamIndex) markIncomplete() {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.incomplete = true
}

func (x *ipamIndex) isIncomplete() bool {
	if x == nil {
		return false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.incomplete
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
		x.markIncomplete()
		log.WithError(err).Warn("Could not list the state directory; IPAM-mode networks will refuse until their state is readable")
		return
	}
	for _, id := range ids {
		sn, err := loadNetwork(id)
		if err != nil {
			x.markIncomplete()
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

// ipamLiveRecordForMAC is ipamLiveRecord keyed on the hardware address:
// the record of an endpoint this network already holds under mac, or
// none.
//
// IT IS THE SETTLED HALF OF THE ONE-EXCHANGE RULE, and the in-memory
// reserve set cannot be it. That set holds a reservation only until
// CreateEndpoint takes it (ipam_endpoint.go, take), so by the time a
// first container is up its key is gone and a second `docker run
// --mac-address X` on the same network would reach an empty map, own
// the exchange, and lease a second address under a hardware address the
// server already has a lease filed against. The phases are the same
// ones an address replay answers from, which is what makes RETAINED the
// deliberate exclusion: a tombstone is the re-bind candidate a restart
// consumes, and refusing on one would cost every restarted container
// its address.
//
// THE LEASE'S OWN EXPIRY IS THE BOUND, and without it this lookup never
// lets go. A record can be left in an answering phase with nothing
// running behind it: retainRecordFor lays the tombstone only when an
// in-memory endpoint fingerprint exists (network.go), so a
// DeleteEndpoint that arrives without one -- the plugin restarted and
// recovery did not re-adopt that endpoint, or the container was removed
// while the plugin was down and DeleteEndpoint never ran at all --
// leaves the record JOINED, and nothing afterwards closes it: there is
// no compaction, and recovery closes no record for an endpoint Docker
// no longer lists. Keyed on the phase alone, such an orphan would
// refuse its hardware address on its network for the life of the
// journal, telling the operator to remove an endpoint that is already
// gone.
//
// The expiry is the honest boundary rather than a timeout picked to
// feel safe. No DHCPRELEASE is ever sent (D-7), so the server keeps the
// lease filed against that hardware address until it runs out, and
// while it is filed a second endpoint under the same address really
// would be handed the same lease. When it runs out, so does the reason
// to refuse. A renewal writes every lease event back to the record
// (pkg/dhcp/chassis.go, the persistent client's event loop), so a
// running endpoint's expiry keeps moving and only an abandoned record
// ages out.
//
// THE PREDICATE IS THE LIBRARY'S OWN, Record.Resume, and it is not
// re-derived here. The question this guard asks -- does this record
// still hold a lease the server would honour -- is the question
// INIT-REBOOT asks, and the library answers it in one place
// (lease/record.go): the record must be Held, in one of the five live
// phases, carry a valid address, and its expiry must be unset or in the
// future. Spelling that out again as a phase test plus an expiry test
// dropped two of the four clauses, and both are reachable. A lost lease
// folds to Lease{}, Held=false while the phase stays JOINED, so an
// endpoint whose lease EXPIRED under it read as an infinite lease and
// was refused for ever -- the same permanence the bound exists to
// remove, reached from the other side. A reservation whose process died
// before its ACK never had a lease either, and the in-flight half that
// owns that window died with it.
//
// A zero expiry still means an INFINITE lease where the record holds
// one: RFC 2131's 0xffffffff reaches lease.Lease as a zero Expire, and
// a lease that is never given back is the last one two endpoints should
// share. Held is what tells that apart from a record with no lease at
// all, which is why the answer has to come from the predicate that
// reads both.
func ipamLiveRecordForMAC(rb lease.Rebuilt, networkID string, mac net.HardwareAddr, now time.Time) (lease.Record, bool) {
	matches := rb.ByScopeMAC(networkID, mac)
	for i := len(matches) - 1; i >= 0; i-- {
		rec := matches[i]
		if _, holds := rec.Resume(now); !holds {
			continue
		}
		return rec, true
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

// ipamRefuseIPv6 closes `-o ipv6=true` on a network this plugin is the
// IPAM driver for, and it is a refusal of a combination that the tree
// already did not serve.
//
// What the option promises in null mode is a second address: the null
// CreateEndpoint runs a DHCPv6 exchange, opens a v6 record through
// recordCreated6 and hands libnetwork an AddressIPv6. The IPAM
// CreateEndpoint (ipam_endpoint.go) does none of those three -- the
// whole path is v4 -- so the option set on an IPAM network buys a
// container no v6 address from the plugin at all.
//
// What it does instead is worse than nothing. At Join the v6 manager
// finds no record for the endpoint and mints a DUID-LL out of the
// endpoint's hardware address, which in IPAM mode libnetwork generates
// anew for every endpoint. So the DUID changes at every restart, the
// server sees a stranger each time, and the one property this driver
// exists to give -- the same address back across a restart -- is the
// one v6 cannot have here. Leaving the option accepted would ship that
// as a silent half-feature on a network whose operator asked for v6 in
// writing.
//
// Refused at `docker network create` for the reason ipvlan is: the cost
// is paid once, by the operator who can still act on it, rather than at
// every container start by a message about something else. It takes
// nothing away from a null-mode network, where ipv6=true is unchanged.
func ipamRefuseIPv6(ipv6 bool) error {
	if !ipv6 {
		return nil
	}
	return fmt.Errorf("%w: `-o ipv6=true` cannot be combined with this plugin as the IPAM driver in v2.1.0. This plugin's IPAM driver serves IPv4 only, so the network would run no DHCPv6 exchange and the container would get no IPv6 address from it. Create the network with --ipam-driver null instead, where `-o ipv6=true` is unchanged and supported, or create it without ipv6. Progress on IPv6 in IPAM mode is tracked in issue #960", util.ErrIPAM)
}
