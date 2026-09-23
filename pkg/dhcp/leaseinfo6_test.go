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

// proto.Lease6.Deadlines makes Lease.Expire the longest valid lifetime in the set, so each address needs its own
// (#819).

func TestInfoFromLease_EveryV6AddressCarriesItsOwnLifetimes(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	gua := netip.MustParsePrefix("2001:db8:1::42/64")
	ula := netip.MustParsePrefix("fd00:9::42/64")

	l := lease.Lease{
		SLAAC:     true,
		Addr:      gua,
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
	if info.IP != "2001:db8:1::42/64" {
		t.Errorf("Info.IP = %q, want the first address the lease holds", info.IP)
	}
	if info.LeaseSeconds != 3600 || info.PreferredSeconds != 1800 {
		t.Errorf("the reported address got LeaseSeconds=%d PreferredSeconds=%d, want 3600/1800",
			info.LeaseSeconds, info.PreferredSeconds)
	}
}

// Docker's endpoint carries exactly one AddressIPv6 and libnetwork has no in-place swap for it (#104).

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

// RFC 4862 section 5.5.4: a deprecated address is preferred_lft 0 with valid lifetime left; proto.Lease6.PreferredUntil
// gives an infinite preferred lifetime the zero time too (#819).

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

// RFC 4861 section 4.6.2 allows preferred 0 beside valid 0xFFFFFFFF, and RFC 4862 section 5.5.3 e) forms that address
// deprecated (#819).

func TestInfoFromLease_ADeprecatedAddressWithNoValidDeadlineIsNotAPermanentOne(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	a := netip.MustParsePrefix("2001:db8:1::42/64")

	t.Run("deprecated and infinite", func(t *testing.T) {
		info, _ := infoFromLease(lease.Lease{SLAAC: true, Addr: a, Addrs: []lease.Addr6{
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
