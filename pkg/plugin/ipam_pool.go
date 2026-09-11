// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// The two address spaces this driver answers with. They are public:
// `docker network inspect` prints the address space of every pool.
const (
	ipamLocalAddressSpace  = "dhcp-local"
	ipamGlobalAddressSpace = "dhcp-global"
)

// ipamAnyPool is the pool a network that typed no `--subnet` gets.
//
// It is not a placeholder. libnetwork's remote allocator loops over
// pools until it finds one that overlaps no on-link route on the host,
// and the parent NIC's own subnet is such a route -- so a driver that
// answered the real LAN prefix would be asked again, forever, and
// `docker network create` would never return. The allocator exits that
// loop for exactly three answers: the user typed the pool, the space is
// the global one, or the pool is this string (moby
// libnetwork/ipams/remote/remote.go, the `alloc.Pool.String() ==
// "0.0.0.0/0"` arm). We are a local-scope driver answering an untyped
// request, so this string is the only one of the three available.
const ipamAnyPool = "0.0.0.0/0"

// ipamPoolOptKeys are the only `--ipam-opt` keys this driver accepts,
// in the order they are appended to a PoolID.
//
// The order is FIXED HERE and not taken from the map, which is the
// whole reason this is a slice. libnetwork persists the options map
// with the network and re-sends it at every daemon start (moby
// libnetwork/network.go, ipamOptions), and a PoolID assembled by
// ranging over a Go map is a different string on most starts. The
// daemon stores the PoolID it was given and replays it verbatim, so a
// derivation that is not a function of its inputs alone unbinds every
// endpoint at the first restart -- silently, because each half looks
// right on its own.
var ipamPoolOptKeys = []string{"parent", "bridge"}

// ipamPoolIDPrefix marks a PoolID as ours in `docker network inspect`.
const ipamPoolIDPrefix = "dhcp/"

// ipamPoolID is the pool identity: the request itself, canonical.
//
// THE IDENTITY IS THE REQUEST AND NOT A HASH OF IT, because the daemon
// asks the same question twice and the two askings are not byte-equal.
// At create libnetwork sends the pool the user typed, or nothing; at
// the daemon-start replay it sends back the pool this driver RETURNED
// (moby libnetwork/controller.go, reservePools' PreferredPool rewrite)
// together with the persisted option map. Masking the typed prefix and
// answering with that same masked string is what makes the two requests
// derive one identity. A readable string also puts the answer in
// `docker network inspect` where an operator can compare it to the
// state file.
func ipamPoolID(space, pool string, opts map[string]string) (string, error) {
	if space != ipamLocalAddressSpace && space != ipamGlobalAddressSpace {
		return "", fmt.Errorf("address space %q is not one of this driver's (%s, %s): %w",
			space, ipamLocalAddressSpace, ipamGlobalAddressSpace, util.ErrIPAM)
	}
	canonical, err := ipamCanonicalPool(pool)
	if err != nil {
		return "", err
	}
	suffix, err := ipamPoolIDSuffix(opts)
	if err != nil {
		return "", err
	}
	return ipamPoolIDPrefix + space + "/" + canonical + suffix, nil
}

// ipamCanonicalPool masks a requested prefix to its network address, or
// answers ipamAnyPool when none was requested.
func ipamCanonicalPool(pool string) (string, error) {
	if pool == "" {
		return ipamAnyPool, nil
	}
	p, err := netip.ParsePrefix(pool)
	if err != nil {
		return "", fmt.Errorf("pool %q is not a CIDR prefix: %w", pool, util.ErrIPAM)
	}
	if !p.Addr().Is4() {
		return "", fmt.Errorf("pool %q is not IPv4; this plugin's IPAM driver serves IPv4 only, and IPv6 in this shape is issue #960: %w", pool, util.ErrIPAM)
	}
	return p.Masked().String(), nil
}

// ipamPoolIDSuffix renders the accepted `--ipam-opt` keys, refusing any
// other. An unknown key is refused rather than dropped: a key this
// driver ignored would make two different requests derive one identity,
// and the second network would take the first one's binding.
func ipamPoolIDSuffix(opts map[string]string) (string, error) {
	for k := range opts {
		known := false
		for _, ok := range ipamPoolOptKeys {
			if k == ok {
				known = true
				break
			}
		}
		if !known {
			return "", fmt.Errorf("--ipam-opt %q is not one this driver accepts (%s): %w",
				k, strings.Join(ipamPoolOptKeys, ", "), util.ErrIPAM)
		}
	}
	var parts []string
	for _, k := range ipamPoolOptKeys {
		v, ok := opts[k]
		if !ok {
			continue
		}
		if v == "" {
			return "", fmt.Errorf("--ipam-opt %s was given with no value: %w", k, util.ErrIPAM)
		}
		if strings.ContainsAny(v, "/=\x00") {
			return "", fmt.Errorf("--ipam-opt %s=%q contains a character an interface name cannot: %w", k, v, util.ErrIPAM)
		}
		parts = append(parts, "/"+k+"="+v)
	}
	// Belt for the loop above, which already ranges in a fixed order:
	// sorting a slice built from a fixed order is a no-op today and
	// stays correct if a key is ever added out of order.
	sort.Strings(parts)
	return strings.Join(parts, ""), nil
}

// ipamPoolIDNames reports the interface name a PoolID's suffix carries,
// and which option named it. Both empty when the PoolID has no suffix.
func ipamPoolIDNames(poolID string) (key, name string) {
	for _, k := range ipamPoolOptKeys {
		marker := "/" + k + "="
		if i := strings.LastIndex(poolID, marker); i >= 0 {
			return k, poolID[i+len(marker):]
		}
	}
	return "", ""
}

// ipamBinding is what CreateNetwork learned and RequestAddress needs: it
// ties one PoolID to one network, and it is the ONLY thing that says a
// network is in IPAM mode.
//
// It lives in the network's own state file rather than in a table of its
// own, because a table is a second lifetime to get right: libnetwork
// calls ReleasePool for a create that failed on a PoolID another network
// may hold, and again at every network delete BEFORE the driver's
// DeleteNetwork (moby libnetwork/network.go, ipamRelease then
// deleteNetwork). A per-network file is dropped by DeleteNetwork with
// the options, which is the one lifetime that is already correct.
type ipamBinding struct {
	PoolID string `json:"pool_id"`
	Space  string `json:"space"`
	Pool   string `json:"pool"`
	// Gateway and Aux are the addresses CreateNetwork was told about.
	// They are kept because RequestAddress cannot otherwise tell an aux
	// address from a replayed endpoint address: the two calls are
	// wire-identical (moby libnetwork/network.go, the aux request and
	// endpoint.go's replay both send an address and nil options).
	Gateway string   `json:"gateway,omitempty"`
	Aux     []string `json:"aux,omitempty"`
}

// issuedPool is a PoolID answered by RequestPool and not yet consumed.
type issuedPool struct {
	space string
	pool  string
	// name is the interface the PoolID's suffix named, "" for none.
	name string
	at   time.Time
}

// issuedPools is the set RequestPool adds to and CreateNetwork consumes.
//
// IT IS IN MEMORY AND WRITES NOTHING DURABLE, deliberately. RequestPool
// runs again at every daemon start for every network that already
// exists, with no CreateNetwork behind it; a RequestPool that wrote
// state would rewrite -- or unbind -- every network on every restart.
// What makes the restart work instead is that the PoolID is a function
// of the request, so the replayed call derives the identity the daemon
// already stored.
type issuedPools struct {
	mu sync.Mutex
	m  map[string]issuedPool
	// ttl bounds how long an unconsumed entry is kept. Derived from the
	// longest thing that can run between RequestPool and CreateNetwork
	// rather than chosen: `-o validate_dhcp=true` puts a full DHCP
	// round trip in that gap.
	ttl time.Duration
}

// issuedPoolTTL is the gap between RequestPool and CreateNetwork, with
// room.
//
// DERIVED, NOT PICKED. The preflight DHCP probe runs inside that gap and
// takes its own budget plus the slack CreateNetwork gives it, and a TTL
// that expired underneath the probe would refuse every
// `-o validate_dhcp=true` network in IPAM mode -- silently, because the
// refusal would name a missing pool rather than a timer.
// TestIssuedPoolTTLClearsTheProbeBudget is what holds the two together.
const issuedPoolTTL = 6 * (preflightProbeBudget + 5*time.Second)

func newIssuedPools() *issuedPools {
	return &issuedPools{m: map[string]issuedPool{}, ttl: issuedPoolTTL}
}

// Nil-safe for the same reason ipamIndex is: a Plugin built literally
// has no issued set, and "nothing was issued" is the truth about one.
func (s *issuedPools) add(poolID, space, pool, name string, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	s.m[poolID] = issuedPool{space: space, pool: pool, name: name, at: now}
}

// take consumes the PoolID a CreateNetwork in this space and pool should
// bind, preferring one whose suffix names this network's own interface.
//
// The preference is what lets two networks share a subnet on two
// parents: the second one types `--ipam-opt parent=`, its PoolID carries
// the suffix, and this lookup can tell the two apart. Without a suffix
// there is at most one such entry, because a second unsuffixed create in
// the same space and pool derives the same PoolID.
func (s *issuedPools) take(space, pool, iface string, now time.Time) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	var fallback string
	for id, e := range s.m {
		if e.space != space || e.pool != pool {
			continue
		}
		if e.name != "" && e.name == iface {
			delete(s.m, id)
			return id, true
		}
		if e.name == "" {
			fallback = id
		}
	}
	if fallback != "" {
		delete(s.m, fallback)
		return fallback, true
	}
	return "", false
}

// drop removes one entry. ReleasePool's whole effect.
func (s *issuedPools) drop(poolID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, poolID)
}

func (s *issuedPools) expire(now time.Time) {
	for id, e := range s.m {
		if now.Sub(e.at) > s.ttl {
			delete(s.m, id)
		}
	}
}

func (s *issuedPools) len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
