// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// TestIpamPoolID_IsAFunctionOfItsInputs is the defect the PoolID's shape
// exists to prevent, driven rather than argued.
//
// libnetwork persists the --ipam-opt map with the network and re-sends
// it at every daemon start. A PoolID assembled by ranging over a Go map
// is a different string on most of those starts, and the daemon stores
// the id it was FIRST given: the second derivation stops resolving, and
// every endpoint on the network fails its address replay. Go randomises
// map iteration order per range, so one derivation proves nothing and a
// hundred is the cheapest thing that does.
func TestIpamPoolID_IsAFunctionOfItsInputs(t *testing.T) {
	opts := map[string]string{"parent": "eth0", "bridge": "br-lan"}
	first, err := ipamPoolID(ipamLocalAddressSpace, "192.168.0.0/24", opts)
	if err != nil {
		t.Fatalf("ipamPoolID: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := ipamPoolID(ipamLocalAddressSpace, "192.168.0.0/24", opts)
		if err != nil {
			t.Fatalf("ipamPoolID (run %d): %v", i, err)
		}
		if got != first {
			t.Fatalf("derivation %d gave %q, the first gave %q — a PoolID that is not a "+
				"function of its inputs unbinds every endpoint at the next daemon restart",
				i, got, first)
		}
	}
	if !strings.HasPrefix(first, ipamPoolIDPrefix) {
		t.Errorf("PoolID %q does not carry the %q prefix that marks it as ours in `docker network inspect`", first, ipamPoolIDPrefix)
	}
}

// TestIpamPoolID_CreateAndReplayDeriveOneIdentity is the OTHER half, and
// it is the one that made the identity a canonical string instead of a
// hash of the request.
//
// The daemon asks twice and the two askings differ. At `docker network
// create` libnetwork sends the pool the user typed -- unmasked, as
// typed. At the start-up replay it sends back the pool this driver
// RETURNED, together with the persisted options. If those two derive
// different identities the network is unbound the first time dockerd
// restarts.
func TestIpamPoolID_CreateAndReplayDeriveOneIdentity(t *testing.T) {
	// canonical is what the driver must ANSWER with, written out here
	// rather than taken from ipamCanonicalPool. A replay derived by
	// calling the subject would agree with the create for any
	// derivation at all, including one that never masks: the two sides
	// would be the same wrong function. This column is the independent
	// half, and it is what makes the masking observable.
	cases := []struct{ name, typed, canonical string }{
		{"host bits set", "192.168.0.7/24", "192.168.0.0/24"},
		{"already masked", "192.168.0.0/24", "192.168.0.0/24"},
		{"no subnet typed", "", ipamAnyPool},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := map[string]string{"parent": "eth0"}
			atCreate, err := ipamPoolID(ipamLocalAddressSpace, c.typed, opts)
			if err != nil {
				t.Fatalf("create derivation: %v", err)
			}
			// What the driver answered with, which is what libnetwork
			// stores and sends back.
			returned, err := ipamCanonicalPool(c.typed)
			if err != nil {
				t.Fatalf("ipamCanonicalPool: %v", err)
			}
			if returned != c.canonical {
				t.Fatalf("the driver answers %q for a typed %q; libnetwork stores that answer "+
					"and replays it, and the daemon's own pool check reads it, so an answer "+
					"carrying host bits is a pool nothing else agrees with. Want %q.",
					returned, c.typed, c.canonical)
			}
			atReplay, err := ipamPoolID(ipamLocalAddressSpace, c.canonical, opts)
			if err != nil {
				t.Fatalf("replay derivation: %v", err)
			}
			if atCreate != atReplay {
				t.Errorf("create derived %q and the daemon-start replay derives %q; the "+
					"daemon stores the first and replays it, so every endpoint on this "+
					"network would miss", atCreate, atReplay)
			}
		})
	}
}

// TestIpamPoolID_RefusesWhatItCannotTellApart. An --ipam-opt this driver
// ignored would make two different requests derive one identity, and the
// second network would take the first one's binding.
func TestIpamPoolID_RefusesWhatItCannotTellApart(t *testing.T) {
	cases := []struct {
		name  string
		space string
		pool  string
		opts  map[string]string
	}{
		{"an address space that is not ours", "LocalDefault", "192.168.0.0/24", nil},
		{"an unknown ipam-opt", ipamLocalAddressSpace, "192.168.0.0/24", map[string]string{"vlan": "7"}},
		{"an ipam-opt with no value", ipamLocalAddressSpace, "192.168.0.0/24", map[string]string{"parent": ""}},
		{"a value carrying the separator", ipamLocalAddressSpace, "192.168.0.0/24", map[string]string{"parent": "eth0/1"}},
		{"a value carrying the assignment", ipamLocalAddressSpace, "192.168.0.0/24", map[string]string{"parent": "a=b"}},
		{"a pool that is not a prefix", ipamLocalAddressSpace, "192.168.0.1", nil},
		{"an IPv6 pool", ipamLocalAddressSpace, "2001:db8::/64", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ipamPoolID(c.space, c.pool, c.opts)
			if err == nil {
				t.Fatalf("derived %q; this request had to be refused", got)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("error %v does not wrap util.ErrIPAM, so the daemon gets a 500 instead of a 400", err)
			}
		})
	}
}

// TestIssuedPoolTTLClearsTheProbeBudget is defeat row G: the TTL is
// derived from what runs inside the gap it has to survive, not chosen.
//
// RequestPool answers, then `docker network create` runs its
// -o validate_dhcp=true preflight probe -- a full DHCP round trip --
// and only then does CreateNetwork consume the issue. A TTL that
// expired underneath the probe would refuse every validate_dhcp network
// in IPAM mode, and the refusal would name a missing pool rather than a
// timer. The relation is asserted, not the numbers: raising either
// constant must keep the order.
func TestIssuedPoolTTLClearsTheProbeBudget(t *testing.T) {
	gap := preflightProbeBudget + 5*time.Second
	if issuedPoolTTL <= gap {
		t.Fatalf("issuedPoolTTL is %v and the preflight probe can hold the create for %v; "+
			"an issue that expires inside the probe refuses every validate_dhcp network in "+
			"IPAM mode, naming a missing pool rather than a timer", issuedPoolTTL, gap)
	}
	if issuedPoolTTL < 3*gap {
		t.Errorf("issuedPoolTTL is %v, only %.1f times the %v the probe can take. The margin "+
			"is what absorbs a slow server retrying inside that budget; derive it from the "+
			"budget rather than trimming it", issuedPoolTTL, float64(issuedPoolTTL)/float64(gap), gap)
	}
}

// TestIssuedPools_TakePrefersTheSuffixedIssue is what lets two networks
// share a subnet on two parents: the second types --ipam-opt parent=,
// its PoolID carries the suffix, and CreateNetwork can tell them apart.
func TestIssuedPools_TakePrefersTheSuffixedIssue(t *testing.T) {
	now := time.Now()
	s := newIssuedPools()
	s.add("dhcp/dhcp-local/192.168.0.0/24", ipamLocalAddressSpace, "192.168.0.0/24", "", now)
	s.add("dhcp/dhcp-local/192.168.0.0/24/parent=eth1", ipamLocalAddressSpace, "192.168.0.0/24", "eth1", now)

	got, ok := s.take(ipamLocalAddressSpace, "192.168.0.0/24", "eth1", now)
	if !ok {
		t.Fatal("no issue taken for a network on eth1")
	}
	if got != "dhcp/dhcp-local/192.168.0.0/24/parent=eth1" {
		t.Errorf("took %q; the eth1 network must take the issue whose suffix names eth1, or "+
			"the two networks swap bindings", got)
	}
	// The unsuffixed one is still there for the network that typed no
	// interface, and it is taken by a create on a different parent.
	got, ok = s.take(ipamLocalAddressSpace, "192.168.0.0/24", "eth0", now)
	if !ok || got != "dhcp/dhcp-local/192.168.0.0/24" {
		t.Errorf("second take = (%q, %v), want the unsuffixed issue", got, ok)
	}
	if n := s.len(); n != 0 {
		t.Errorf("%d issue(s) left; both were consumed", n)
	}
}

// TestIssuedPools_ExpireDropsTheUnconsumed. A create that failed leaves
// its issue behind, and nothing else would ever remove it.
func TestIssuedPools_ExpireDropsTheUnconsumed(t *testing.T) {
	now := time.Now()
	s := newIssuedPools()
	s.add("dhcp/dhcp-local/0.0.0.0/0", ipamLocalAddressSpace, ipamAnyPool, "", now)
	if _, ok := s.take(ipamLocalAddressSpace, ipamAnyPool, "", now.Add(issuedPoolTTL+time.Second)); ok {
		t.Error("an issue older than the TTL was still taken; it has outlived the create it belonged to")
	}
}

// TestIpamLeaseTimeoutFloorHolds is defeat row J. Capping lease_timeout
// to the daemon's plugin-call budget must not push it under the floor
// CheckLeaseTimeout enforces: two guards disagreeing about one number
// leave the tighter one unreachable and untested.
func TestIpamLeaseTimeoutFloorHolds(t *testing.T) {
	p := &Plugin{}
	budget := ipamReserveBudget()
	if budget <= 0 {
		t.Fatalf("the reserve budget is %v; the cap below would be meaningless", budget)
	}
	// The cap itself: a network asking for longer than the daemon will
	// wait gets the daemon's budget.
	if got := p.ipamLeaseTimeout(DHCPNetworkOptions{LeaseTimeout: budget + time.Minute}, "pool"); got != budget {
		t.Errorf("lease_timeout %v capped to %v, want %v", budget+time.Minute, got, budget)
	}
	// The preservation control: a network under the budget is untouched.
	short := budget / 2
	if got := p.ipamLeaseTimeout(DHCPNetworkOptions{LeaseTimeout: short}, "pool"); got != short {
		t.Errorf("lease_timeout %v became %v; a value inside the budget must not be touched", short, got)
	}
	// The floor. CheckLeaseTimeout refuses a lease_timeout below the
	// conflict-detection window, and the cap must land above it or the
	// capped value is one the other guard would refuse.
	if err := dhcp.CheckLeaseTimeout(budget, proto.ConflictWait); err != nil {
		t.Errorf("the cap produces %v, which CheckLeaseTimeout refuses: %v.\n"+
			"Two guards disagreeing about one number leave the tighter one unreachable: "+
			"raise the daemon budget, lower the floor, or refuse the crossing loudly.", budget, err)
	}
}
