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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	ipamLocalAddressSpace  = "dhcp-local"
	ipamGlobalAddressSpace = "dhcp-global"
)

// ipamAnyPool answers an untyped request: libnetwork's remote allocator loops until a pool overlaps no on-link route,
// and exits only for a typed pool, the global space, or "0.0.0.0/0" (moby libnetwork/ipams/remote/remote.go) (#110).
const ipamAnyPool = "0.0.0.0/0"

// ipamPoolOptKeys fixes the PoolID order: libnetwork persists the options map and re-sends it at every daemon start,
// and map order would give a different PoolID on most starts (#110).
var ipamPoolOptKeys = []string{"parent", "bridge"}

const ipamPoolIDPrefix = "dhcp/"

// ipamPoolID is the canonical request, not a hash: the daemon-start replay sends back the pool this driver returned
// (controller.go, reservePools), so masking the typed prefix makes both askings derive one identity (#110).
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

// ipamPoolIDSuffix refuses an unknown key, since an ignored key would let two requests share one identity (#110).
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
	// One key, because ipamPoolIDNames decodes one: with both, `bridge` reached no comparison and two requests derived
	// one name (#1010).
	var given []string
	for _, k := range ipamPoolOptKeys {
		if _, ok := opts[k]; ok {
			given = append(given, k)
		}
	}
	// Both spellings are named: at pool allocation the network's mode is not yet known (#1010).
	if len(given) > 1 {
		return "", fmt.Errorf("--ipam-opt %s were given together and a pool names one interface. Keep the one this network's mode owns: `-o bridge=` on a bridge network, `-o parent=` on a macvlan or ipvlan one. Both are named because the pool is requested before this network's own `-o` options reach this driver, which cannot tell from here which of the two your network is: %w",
			strings.Join(given, " and "), util.ErrIPAM)
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
	sort.Strings(parts)
	return strings.Join(parts, ""), nil
}

// ipamPoolIDNames returns the first marker, relying on the two-key refusal, which
// TestIpamPoolIDNames_ItsOneMarkerPremiseIsTheTwoKeyRefusal drives (#1010).
func ipamPoolIDNames(poolID string) (key, name string) {
	for _, k := range ipamPoolOptKeys {
		marker := "/" + k + "="
		if i := strings.LastIndex(poolID, marker); i >= 0 {
			return k, poolID[i+len(marker):]
		}
	}
	return "", ""
}

// ipamPoolNetworkAddress answers an address-less gateway request with the pool's network address: libnetwork's
// remote allocator turns an empty reply into ErrNoIPReturned (v26.1.5 and v28.0.0), and the network address cannot
// shadow a lease (RFC 1122 section 3.2.1.3). On a /31 or /32 every address is a host address (RFC 3021 section 2.1),
// so the request is refused and names --gateway; engine 28 and up does not ask (#110).
func ipamPoolNetworkAddress(poolID string) (RequestAddressResponse, error) {
	pool, err := ipamPoolOfID(poolID)
	if err != nil {
		return RequestAddressResponse{}, err
	}
	if pool.Bits() > 30 {
		return RequestAddressResponse{}, fmt.Errorf("%w: this Docker Engine asks an IPAM driver for a gateway address at `docker network create`, and the pool %v has no address that is not a host address: every address in a /31 and a /32 can be handed to a container by the DHCP server, so one taken for the gateway here would be one this plugin later refuses to lease. Create the network with --gateway <address>, which is passed straight through, or use a shorter prefix. Docker Engine 28 and later does not ask and needs neither", util.ErrIPAM, pool)
	}
	return RequestAddressResponse{Address: pool.String()}, nil
}

// ipamPoolOfID derives the pool from the id, since the issued entry is consumed at CreateNetwork (#110).
func ipamPoolOfID(poolID string) (netip.Prefix, error) {
	rest, ok := strings.CutPrefix(poolID, ipamPoolIDPrefix)
	if !ok {
		return netip.Prefix{}, fmt.Errorf("pool %q was not issued by this driver: %w", poolID, util.ErrIPAM)
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 {
		return netip.Prefix{}, fmt.Errorf("pool %q carries no address pool: %w", poolID, util.ErrIPAM)
	}
	if parts[0] != ipamLocalAddressSpace && parts[0] != ipamGlobalAddressSpace {
		return netip.Prefix{}, fmt.Errorf("pool %q names address space %q, which is not one of this driver's: %w",
			poolID, parts[0], util.ErrIPAM)
	}
	p, err := netip.ParsePrefix(parts[1] + "/" + parts[2])
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pool %q carries no CIDR prefix: %w", poolID, util.ErrIPAM)
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("pool %q is not IPv4: %w", poolID, util.ErrIPAM)
	}
	return p.Masked(), nil
}

// ipamBinding lives in the network's state file: libnetwork calls ReleasePool before DeleteNetwork
// (moby libnetwork/network.go), so a separate table would need a second lifetime (#110).
type ipamBinding struct {
	PoolID string `json:"pool_id"`
	Space  string `json:"space"`
	Pool   string `json:"pool"`
	// Gateway and Aux are kept because an aux request and an endpoint replay are wire-identical (moby libnetwork/network.go, endpoint.go).
	Gateway string   `json:"gateway,omitempty"`
	Aux     []string `json:"aux,omitempty"`
}

type issuedPool struct {
	space string
	pool  string
	name  string
	at    time.Time
}

// issuedPools is in memory only: RequestPool runs at every daemon start with no CreateNetwork behind it (#110).
type issuedPools struct {
	mu  sync.Mutex
	m   map[string]issuedPool
	ttl time.Duration
}

// issuedPoolTTL covers the validate_dhcp probe run between RequestPool and CreateNetwork;
// TestIssuedPoolTTLClearsTheProbeBudget holds it (#110).
const issuedPoolTTL = 6 * (preflightProbeBudget + 5*time.Second)

func newIssuedPools() *issuedPools {
	return &issuedPools{m: map[string]issuedPool{}, ttl: issuedPoolTTL}
}

func (s *issuedPools) add(poolID, space, pool, name string, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	s.m[poolID] = issuedPool{space: space, pool: pool, name: name, at: now}
}

// take prefers the entry whose suffix names this network's interface, and returns a mismatched name for the refusal
// (#110).
func (s *issuedPools) take(space, pool, iface string, now time.Time) (id string, ok bool, otherName string) {
	if s == nil {
		return "", false, ""
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
			return id, true, ""
		}
		if e.name == "" {
			fallback = id
		} else {
			otherName = e.name
		}
	}
	if fallback != "" {
		delete(s.m, fallback)
		return fallback, true, ""
	}
	return "", false, otherName
}

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
