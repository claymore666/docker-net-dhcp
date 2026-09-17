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
	// ONE KEY, because the decoder reads one (#1010, found by
	// FuzzIPAMPoolIDRoundTrip). ipamPoolIDNames returns a single
	// key/name pair, so a suffix carrying both encoded an option that
	// nothing downstream can see: RequestPool stored `parent` as the
	// issued pool's name and `bridge` reached no comparison at all, and
	// two requests differing only in the invisible half derived one
	// name. That is the identity collapse the unknown-key refusal above
	// exists to prevent, one level in. Refusing is also the only answer
	// that is not a guess about which of the two the operator meant, and
	// a network is on one interface: the two keys are the macvlan/ipvlan
	// and the bridge spelling of the same thing.
	var given []string
	for _, k := range ipamPoolOptKeys {
		if _, ok := opts[k]; ok {
			given = append(given, k)
		}
	}
	// BOTH SPELLINGS ARE NAMED, because this driver cannot see which one
	// the network owns. libnetwork allocates the pool while the create
	// is still running and hands the network's own `-o` options to
	// CreateNetwork afterwards, so at this point there is no mode to
	// read: the only thing here is the --ipam-opt map the operator
	// typed. Naming one key is then a guess, and the first version of
	// this message guessed by position -- it printed the first of
	// ipamPoolOptKeys that was present, which is always `parent`, so a
	// bridge network was told to keep an option it does not have.
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

// ipamPoolNetworkAddress answers a gateway-type request that carries no
// address, with the pool's own network address wearing the pool's
// prefix.
//
// SOMETHING HAS TO BE ANSWERED. A remote IPAM driver has no spelling
// for "no address": moby libnetwork/ipams/remote/remote.go turns an
// empty Address in the reply into ErrNoIPReturned, at v26.1.5 and at
// v28.0.0 alike, and the gateway call site in network.go turns any
// error into a failed `docker network create`. The built-in null
// driver's nil answer is reachable only from inside the daemon.
//
// THE NETWORK ADDRESS, because it is not a host address and so cannot
// shadow a lease a container is later given. RFC 1122 Section 3.2.1.3:
// "IP addresses are not permitted to have the value 0 or -1 for any of
// the <Host-number>, <Network-number>, or <Subnet-number> fields
// (except in the special cases listed above)." That matters here
// because the answer is persisted as this network's gateway
// (CreateNetwork) and an address equal to it is echoed from then on
// instead of being leased. It also describes the pool the daemon asked
// about, which 0.0.0.0 would not on a network created with --subnet.
// Nothing routes through it: the gateway a container uses comes from
// this plugin's Join answer, which libnetwork installs from the
// endpoint's join info and never from the pool's.
//
// AND THE BOUNDARY OF THAT SENTENCE, which is two prefix lengths.
// RFC 3021 Section 2.1, on the two addresses a /31 leaves: "In a
// point-to-point link with a 31-bit subnet mask, the two addresses
// above MUST be interpreted as host addresses." A /32 pool has one
// address and it is a host address too. On those two there is no
// address to invent that a DHCP server could not hand to a container,
// so none is invented: the request is refused and the message names the
// option that supplies one. On an engine that asks, such a network then
// needs --gateway; on engine 28 and up nothing asks and the prefix
// makes no difference.
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

// ipamPoolOfID reads the address pool back out of a PoolID.
//
// The identity is the request made canonical (ipamPoolID) and an
// interface name may carry no `/`, so the two fields after the address
// space are the pool and nothing else. Derived from the id the daemon
// sent rather than looked up, because the issue it was minted from is
// consumed at CreateNetwork and gone by the daemon-start replay.
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
//
// THE THIRD RETURN IS FOR THE REFUSAL, and it exists because the
// caller's message was wrong in a case that is easy to hit. An entry
// for this space and pool whose suffix names a DIFFERENT interface is
// not a miss for the ordinary reason: the operator typed
// `--ipam-opt parent=eth0` beside `-o parent=eth1`, the pool identity
// was minted against the first and the network is being created on the
// second, and telling them the plugin "did not issue this pool, or
// restarted since it did" sends them to look at the wrong thing
// entirely. The name is returned so the refusal can name it.
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
