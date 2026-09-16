// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// The whole v6 half of one lease, rendered.
//
// EVERY NUMBER A CONTAINER'S IPv6 ADDRESSES GET COMES OUT OF THIS
// FUNCTION, and the two ways it can be wrong are both silent. Reading
// the lease's aggregate deadlines instead of each address's own gives a
// short-lived prefix a long-lived one's expiry (proto.Lease6.Deadlines
// makes Lease.Expire the LONGEST valid lifetime in the set). Rendering
// only the first address leaves the rest of them off the link while the
// library keeps refreshing them, and the endpoint looks perfectly
// healthy from every side: the address Docker shows is there, the
// counters move, and the container simply cannot be reached on the
// other prefix.
func TestInfoFromLease_EveryV6AddressCarriesItsOwnLifetimes(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	gua := netip.MustParsePrefix("2001:db8:1::42/64")
	ula := netip.MustParsePrefix("fd00:9::42/64")

	l := lease.Lease{
		SLAAC: true,
		Addr:  gua,
		// The aggregate the library computes over the set. Both
		// numbers are deliberately wrong for BOTH addresses, so any
		// read of them shows up.
		Expire:    now.Add(2 * time.Hour),
		Preferred: now.Add(time.Minute),
		Addrs: []lease.Addr6{
			{Addr: gua, Valid: now.Add(time.Hour), Preferred: now.Add(30 * time.Minute)},
			{Addr: ula, Valid: now.Add(2 * time.Hour), Preferred: now.Add(90 * time.Minute)},
		},
	}

	info, _ := infoFromLease(l, proto.RouterObservation{}, now, netip.Prefix{})

	if !info.SLAAC {
		t.Error("Info.SLAAC is false for a formed lease; the ledger's source column, the " +
			"ipv6_slaac_addresses counter and the outage rules all read it")
	}
	if len(info.Addrs) != 2 {
		t.Fatalf("Info.Addrs = %+v, want both addresses", info.Addrs)
	}
	for i, want := range []V6Addr{
		{IP: "2001:db8:1::42/64", ValidSeconds: 3600, PreferredSeconds: 1800},
		{IP: "fd00:9::42/64", ValidSeconds: 7200, PreferredSeconds: 5400},
	} {
		if info.Addrs[i] != want {
			t.Errorf("Info.Addrs[%d] = %+v, want %+v. The lease's own pair is %d/%d and is an "+
				"aggregate over the set, not either address's lifetimes",
				i, info.Addrs[i], want, 7200, 60)
		}
	}
	// The reported address is the first the lease holds, and the
	// lease-wide numbers Docker and the plugin read follow IT.
	if info.IP != "2001:db8:1::42/64" {
		t.Errorf("Info.IP = %q, want the first address the lease holds", info.IP)
	}
	if info.LeaseSeconds != 3600 || info.PreferredSeconds != 1800 {
		t.Errorf("the reported address got LeaseSeconds=%d PreferredSeconds=%d, want 3600/1800",
			info.LeaseSeconds, info.PreferredSeconds)
	}
}

// ipv6_main_prefix decides which address Docker is told about, and says
// so when it decided nothing.
//
// Docker's endpoint carries exactly ONE AddressIPv6 and libnetwork has
// no in-place swap for it (#104), so on a link advertising two prefixes
// one of them is the address `docker inspect` shows and the other is
// only on the link. Which one is the operator's choice; without the
// option it is the router's advertisement order, which is not a choice
// anybody made.
//
// THE FALLBACK IS THE HALF WORTH TESTING. A named prefix that matches
// nothing has to produce a working endpoint -- the addresses are formed
// either way and refusing the endpoint would make a typo in an
// annotation take containers down -- and it has to be visible, or an
// operator reading `docker inspect` sees a prefix they did not ask for
// with nothing anywhere saying why.
func TestInfoFromLease_TheMainPrefixChoosesWhatDockerIsTold(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	gua := netip.MustParsePrefix("2001:db8:1::42/64")
	ula := netip.MustParsePrefix("fd00:9::42/64")
	l := lease.Lease{
		SLAAC: true,
		Addr:  ula,
		Addrs: []lease.Addr6{
			{Addr: ula, Valid: now.Add(2 * time.Hour), Preferred: now.Add(90 * time.Minute)},
			{Addr: gua, Valid: now.Add(time.Hour), Preferred: now.Add(30 * time.Minute)},
		},
	}

	for _, tc := range []struct {
		name         string
		main         string
		wantIP       string
		wantLease    int
		wantFallback bool
	}{
		{"unset takes the first advertised", "", "fd00:9::42/64", 7200, false},
		{"the global prefix", "2001:db8:1::/64", "2001:db8:1::42/64", 3600, false},
		{"the unique-local prefix", "fd00:9::/64", "fd00:9::42/64", 7200, false},
		// A prefix shorter than the advertised one still contains the
		// address, which is the reading netip.Prefix.Contains gives
		// and the one an operator writing fd00::/8 means.
		{"a shorter prefix that contains it", "2001:db8::/32", "2001:db8:1::42/64", 3600, false},
		{"a prefix nothing falls inside", "2001:db8:ffff::/48", "fd00:9::42/64", 7200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var main netip.Prefix
			if tc.main != "" {
				main = netip.MustParsePrefix(tc.main)
			}
			info, _ := infoFromLease(l, proto.RouterObservation{}, now, main)
			if info.IP != tc.wantIP {
				t.Errorf("Info.IP = %q, want %q", info.IP, tc.wantIP)
			}
			if info.LeaseSeconds != tc.wantLease {
				t.Errorf("LeaseSeconds = %d, want %d: the reported lifetime belongs to a "+
					"different address than the reported one", info.LeaseSeconds, tc.wantLease)
			}
			if info.MainAddrFallback != tc.wantFallback {
				t.Errorf("MainAddrFallback = %v, want %v: an operator who named a prefix and "+
					"got another one has a router to look at, and this flag is the only thing "+
					"that tells them so", info.MainAddrFallback, tc.wantFallback)
			}
			if len(info.Addrs) != 2 {
				t.Errorf("choosing which address to report changed what is installed: %+v", info.Addrs)
			}
		})
	}
}

// The two zeros this seam has to keep apart, and one it must not
// produce.
//
// A DEPRECATED ADDRESS IS PreferredSeconds=0 WITH A VALID LIFETIME
// LEFT. That is RFC 4862 section 5.5.4's second phase and the kernel's
// spelling of it (`preferred_lft 0`, flag `deprecated`): the container
// keeps using it for connections it already has and opens no new ones
// on it.
//
// AN INFINITE PREFERRED LIFETIME IS ALSO THE ZERO TIME in the library,
// because proto.Lease6.PreferredUntil refuses a preferred deadline of
// zero seconds. Rendered as PreferredSeconds=0 it would deprecate an
// address that is perfectly current, on every advertisement carrying an
// infinite lifetime, which is what a great many routers send. It takes
// the valid lifetime instead.
//
// AN EXPIRED ADDRESS IS NOT RENDERED AT ALL. Info's zero lifetime is
// netlink's infinity, so an address one second past its deadline and an
// address advertised forever are the same two numbers; dropping it here
// is what stops the second reading from being installed.
func TestInfoFromLease_DeprecatedInfiniteAndExpiredAreThreeThings(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	a := netip.MustParsePrefix("2001:db8:1::42/64")
	b := netip.MustParsePrefix("fd00:9::42/64")

	t.Run("deprecated", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
			{Addr: a, Valid: now.Add(time.Hour), Preferred: now.Add(-time.Minute)},
		}}, proto.RouterObservation{}, now, netip.Prefix{})
		if len(info.Addrs) != 1 {
			t.Fatalf("a deprecated address left the set: %+v", info.Addrs)
		}
		if info.Addrs[0].PreferredSeconds != 0 {
			t.Errorf("PreferredSeconds = %d for an address past its preferred lifetime, want 0: "+
				"the kernel deprecates on preferred_lft 0 and on nothing else",
				info.Addrs[0].PreferredSeconds)
		}
		if info.Addrs[0].ValidSeconds != 3600 {
			t.Errorf("ValidSeconds = %d, want 3600: a deprecated address stays on the link",
				info.Addrs[0].ValidSeconds)
		}
	})

	t.Run("infinite", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
			{Addr: a},
		}}, proto.RouterObservation{}, now, netip.Prefix{})
		if len(info.Addrs) != 1 {
			t.Fatalf("an address advertised forever left the set: %+v", info.Addrs)
		}
		if info.Addrs[0].ValidSeconds != 0 || info.Addrs[0].PreferredSeconds != 0 {
			t.Errorf("an infinite advertisement rendered as %+v, want both lifetimes zero, "+
				"which is what makes the kernel send no IFA_CACHEINFO", info.Addrs[0])
		}
	})

	t.Run("expired", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
			{Addr: a, Valid: now.Add(time.Hour), Preferred: now.Add(time.Hour)},
			{Addr: b, Valid: now.Add(-time.Second), Preferred: now.Add(-time.Hour)},
		}}, proto.RouterObservation{}, now, netip.Prefix{})
		if len(info.Addrs) != 1 || info.Addrs[0].IP != a.String() {
			t.Fatalf("Info.Addrs = %+v, want the one address whose valid lifetime is left. "+
				"An expired address renders as two zeros, which is this seam's spelling of "+
				"an infinite lifetime, and it would go on the link forever", info.Addrs)
		}
	})
}

// A lease with no v6 list is left exactly as it was.
//
// Every DHCPv4 lease is this shape, and so is every Info a caller
// builds by hand. The v6 rendering has to be total over them: a
// function that wrote an empty Addrs slice, or that reached for
// Lease.Addr without checking its family, would put a v4 address into
// the field the v6 apply path walks.
func TestInfoFromLease_AV4LeaseGetsNoV6Rendering(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	info, _ := infoFromLease(lease.Lease{
		Addr:   netip.MustParsePrefix("192.168.99.50/24"),
		Expire: now.Add(time.Hour),
	}, proto.RouterObservation{}, now, netip.MustParsePrefix("2001:db8:1::/64"))

	if info.Addrs != nil {
		t.Errorf("a v4 lease rendered Addrs = %+v", info.Addrs)
	}
	if info.IP != "192.168.99.50/24" {
		t.Errorf("Info.IP = %q, want the v4 address", info.IP)
	}
	if info.LeaseSeconds != 3600 {
		t.Errorf("LeaseSeconds = %d, want the v4 lease's own 3600", info.LeaseSeconds)
	}
	if info.MainAddrFallback {
		t.Error("a v4 lease reported an ipv6_main_prefix fallback")
	}
}

// A deprecated address whose valid lifetime never ends.
//
// RFC 4861 section 4.6.2 lets a Prefix Information option carry a
// preferred lifetime of 0 beside a valid lifetime of 0xFFFFFFFF, and
// RFC 4862 section 5.5.3 e) accepts it: the prefix is autonomous, the
// address is formed, and it is deprecated from the moment it exists.
// Rendered as the pair of numbers alone that arrives as (0, 0), which
// is the SAME spelling this seam gives an address advertised forever
// and preferred forever. Two facts derived from one pair, and the
// permanent reading is the one the apply path takes: the container
// would get a preferred address on a prefix the router has already
// told it to stop using for new connections, and nothing on either
// side says so. The flag is the address's own answer, carried instead
// of derived.
func TestInfoFromLease_ADeprecatedAddressWithNoValidDeadlineIsNotAPermanentOne(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	a := netip.MustParsePrefix("2001:db8:1::42/64")

	t.Run("deprecated and infinite", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
			// Preferred in the past, Valid zero: the library's
			// spelling of "no deadline".
			{Addr: a, Preferred: now.Add(-time.Minute)},
		}}, proto.RouterObservation{}, now, netip.Prefix{})
		if len(info.Addrs) != 1 {
			t.Fatalf("Info.Addrs = %+v, want the one deprecated address: an infinite valid "+
				"lifetime does not expire", info.Addrs)
		}
		if got := info.Addrs[0]; got.ValidSeconds != 0 || got.PreferredSeconds != 0 {
			t.Fatalf("Info.Addrs[0] = %+v, want both lifetimes zero. The pair is the premise "+
				"of this test: it is why the flag has to exist", got)
		}
		if !info.Addrs[0].Deprecated {
			t.Errorf("Deprecated = false on an address whose preferred lifetime ran out and " +
				"whose valid lifetime never will. The pair of numbers cannot say it, so a " +
				"reader of the numbers alone installs it preferred and permanent")
		}
		if !info.IPDeprecated {
			t.Errorf("Info.IPDeprecated = false while Info.IP is that address: the apply path " +
				"reads the scalar for the address Docker was told about")
		}
	})

	// The preservation control for the widening above: the same two
	// zeros, reached the other way. An address with neither deadline is
	// preferred forever, and a flag that answered yes here would
	// deprecate every permanent address on the link.
	t.Run("infinite and preferred", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
			{Addr: a},
		}}, proto.RouterObservation{}, now, netip.Prefix{})
		if len(info.Addrs) != 1 {
			t.Fatalf("Info.Addrs = %+v, want one address", info.Addrs)
		}
		if info.Addrs[0].Deprecated || info.IPDeprecated {
			t.Errorf("an address advertised with no deadlines at all came back deprecated "+
				"(%+v, Info.IPDeprecated=%v): its preferred lifetime is infinite, not spent",
				info.Addrs[0], info.IPDeprecated)
		}
	})

	// And the third way to reach a zero preferred lifetime: a deadline
	// that has not arrived yet is not a spent one.
	t.Run("preferred in the future", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
			{Addr: a, Preferred: now.Add(time.Minute), Valid: now.Add(time.Hour)},
		}}, proto.RouterObservation{}, now, netip.Prefix{})
		if len(info.Addrs) != 1 {
			t.Fatalf("Info.Addrs = %+v, want one address", info.Addrs)
		}
		if info.Addrs[0].Deprecated {
			t.Errorf("an address preferred for another minute came back deprecated: %+v",
				info.Addrs[0])
		}
	})
}
