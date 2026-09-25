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
	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// errIPAMBindingLost refuses an IPAM-mode network whose binding cannot be read: the Docker API fallback does not
// hold the binding, and the null-mode path would answer with an address libnetwork will refuse (#110).
var errIPAMBindingLost = errors.New("this network's IPAM pool binding could not be read; refusing rather than serving it as a null-IPAM network")

// ipamIndex is rebuilt from the state directory: the daemon replays RequestPool before its API serves (#110).
type ipamIndex struct {
	mu         sync.Mutex
	m          map[string]string
	incomplete bool
}

func newIPAMIndex() *ipamIndex { return &ipamIndex{m: map[string]string{}} }

// A nil index is empty, not a panic: a Plugin built literally has none (#110).
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

// network matches the exact PoolID: two networks sharing a subnet on two parents differ only in the suffix (#110).
func (x *ipamIndex) network(poolID string) (string, bool) {
	if x == nil {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	n, ok := x.m[poolID]
	return n, ok
}

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

// markIncomplete separates an aux request during a create from a replay for a network the fold skipped; while
// anything is unread the unbound branch refuses, so on such a host --aux-address is refused too (#110).
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

// ipamNetwork reads disk only: IPAM RPCs arrive during the daemon's start-up replay, before its API serves (#110).
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

// lease Rebuilt.ByScopeAddr applies no phase filter (lease/rebuild.go), so CLOSED is filtered here; RETAINED is a
// re-bind candidate, and answering from it would give one address to two endpoints (#110).
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

// Newest wins: Rebuild returns records in creation order (#110).
func ipamLiveRecord(rb lease.Rebuilt, networkID string, addr netip.Addr) (lease.Record, bool) {
	matches := rb.ByScopeAddr(networkID, addr)
	for i := len(matches) - 1; i >= 0; i-- {
		if ipamPhaseAnswers(matches[i].Phase) {
			return matches[i], true
		}
	}
	return lease.Record{}, false
}

// ipamLiveRecordForMAC is the settled half of the one-exchange rule: the reserve set is emptied when CreateEndpoint
// takes a reservation, so a second `--mac-address X` would otherwise lease a second address under that MAC.
// RETAINED is excluded, since a restart consumes it. The bound is the library's Record.Resume (lease/record.go):
// Held, a live phase, a valid address, and an expiry unset or in the future. An orphaned JOINED record gets no
// DHCPRELEASE (#962), so it refuses its MAC until its lease runs out, and a zero expiry is RFC 2131's infinite
// lease (#110).
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

// ipamRefuseIPvlan refuses at create: with RequiresMACAddress libnetwork sets the generated MAC on the container
// link at join, which an ipvlan slave refuses with EOPNOTSUPP even for its own MAC (#949).
func ipamRefuseIPvlan(mode string) error {
	if mode != ModeIPvlan {
		return nil
	}
	return fmt.Errorf("%w: ipvlan networks cannot use this plugin as an IPAM driver. Docker generates a MAC for each endpoint when the IPAM driver asks for one and sets it on the container's interface at start, and an ipvlan interface cannot change its MAC, so every container would fail to start. Create the network with --ipam-driver null instead, which is unchanged and supported. Issue #949 tracks the engine change this needs", util.ErrIPAM)
}

// ipamRefuseIPv6 refuses IPv6 in IPAM mode: the IPAM CreateEndpoint is v4 only, and Join would mint a DUID-LL from
// a MAC libnetwork regenerates per endpoint, so the DUID would change at every restart. The message names the
// resolved mode, since two spellings switch IPv6 on (#110, #817).
func ipamRefuseIPv6(mode proto.Mode6) error {
	if mode == proto.Mode6Off {
		return nil
	}
	return fmt.Errorf("%w: this network switches IPv6 on with ipv6_mode=%s, and IPv6 cannot be combined with this plugin as the IPAM driver. Two spellings reach this refusal: `-o ipv6_mode=` with any mode but off, and `-o ipv6=true`, which is the short spelling of the dhcp mode. This plugin's IPAM driver serves IPv4 only, so the network would run no DHCPv6 exchange and the container would get no IPv6 address from it. Create the network with --ipam-driver null instead, where every ipv6_mode is unchanged and supported, or create it without IPv6. Progress on IPv6 in IPAM mode is tracked in issue #960", util.ErrIPAM, mode)
}
