// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

func TestParseServerList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  []string
		errIs bool
	}{
		{name: "empty is not an error", in: "", want: nil},
		{name: "whitespace only is not an error", in: "   ", want: nil},
		{name: "single", in: "1.1.1.1", want: []string{"1.1.1.1"}},
		{name: "ordered pair", in: "1.1.1.1,2.2.2.2", want: []string{"1.1.1.1", "2.2.2.2"}},
		{name: "spaces are trimmed", in: " 1.1.1.1 , 2.2.2.2 ", want: []string{"1.1.1.1", "2.2.2.2"}},

		{name: "not an address", in: "1.1.1.1,nope", errIs: true},
		{name: "empty entry", in: "1.1.1.1,,2.2.2.2", errIs: true},
		{name: "trailing comma", in: "1.1.1.1,", errIs: true},
		{name: "duplicate", in: "1.1.1.1,1.1.1.1", errIs: true},
		// v6 is refused rather than ignored: dhcpcd stores both lists as
		// in_addr_t and dhcp6.c never reads them, so accepting a v6 entry
		// would apply to nothing while looking like it worked.
		{name: "ipv6 is rejected", in: "2001:db8::1", errIs: true},
		{name: "ipv6 mixed in is rejected", in: "1.1.1.1,2001:db8::1", errIs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseServerList("dhcp_servers", tc.in)
			if tc.errIs {
				if err == nil {
					t.Fatalf("parseServerList(%q) = %v, want an error", tc.in, got)
				}
				if !errors.Is(err, util.ErrInvalidServerList) {
					t.Fatalf("error %v does not wrap ErrInvalidServerList", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServerList(%q): %v", tc.in, err)
			}
			if strings.Join(addrsToStrings(got), ",") != strings.Join(tc.want, ",") {
				t.Fatalf("parseServerList(%q) = %v, want %v", tc.in, addrsToStrings(got), tc.want)
			}
		})
	}
}

// The option names must appear in the error: an operator who set both
// lists needs to know which one is malformed.
func TestParseServerList_ErrorNamesTheOption(t *testing.T) {
	_, err := parseServerList("dhcp_deny_servers", "nope")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "dhcp_deny_servers") {
		t.Fatalf("error %q does not name the option it came from", err)
	}
}

func TestResolveServerPolicy(t *testing.T) {
	t.Run("deny is subtracted from prefer", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{
			DHCPServers: "1.1.1.1,2.2.2.2,3.3.3.3",
			DenyServers: "2.2.2.2",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pol.allowList(), ","); got != "1.1.1.1,3.3.3.3" {
			t.Fatalf("allowList = %q, want the denied entry removed", got)
		}
	})

	// dhcpcd ignores a blacklist whenever a whitelist is configured
	// (src/dhcp.c:3181-3196). Emitting one anyway would advertise a
	// denial that is not enforced, so the renderer must be handed none.
	t.Run("no blacklist is emitted alongside a whitelist", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{
			DHCPServers: "1.1.1.1",
			DenyServers: "3.3.3.3",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := pol.denyList(); got != nil {
			t.Fatalf("denyList = %v, want nil so dhcpcd is not given a directive it will not read", got)
		}
		// ...and the denial still has to be real, via subtraction.
		for _, a := range pol.allowList() {
			if a == "3.3.3.3" {
				t.Fatal("denied server survived into the whitelist")
			}
		}
	})

	t.Run("deny alone is emitted as a blacklist", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{DenyServers: "3.3.3.3"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(pol.denyList(), ","); got != "3.3.3.3" {
			t.Fatalf("denyList = %q", got)
		}
		if pol.allowList() != nil {
			t.Fatalf("allowList = %v, want nil", pol.allowList())
		}
	})

	// Denying every preferred server would otherwise collapse to "no
	// preference", i.e. accept anything — the opposite of what both
	// options were set to do.
	t.Run("denying the whole preference list is refused", func(t *testing.T) {
		_, err := resolveServerPolicy(DHCPNetworkOptions{
			DHCPServers: "1.1.1.1,2.2.2.2",
			DenyServers: "2.2.2.2,1.1.1.1",
		})
		if err == nil {
			t.Fatal("want an error, got a policy that silently accepts any server")
		}
		if !errors.Is(err, util.ErrInvalidServerList) {
			t.Fatalf("error %v does not wrap ErrInvalidServerList", err)
		}
	})

	t.Run("neither option set is the zero policy", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !pol.IsZero() {
			t.Fatalf("policy = %+v, want zero", pol)
		}
	})
}

func TestAcquisitionAttempts(t *testing.T) {
	const total = 12 * time.Second

	t.Run("no policy is one unrestricted attempt with the whole budget", func(t *testing.T) {
		got := acquisitionAttempts(serverPolicy{}, false, total)
		if len(got) != 1 || got[0].Budget != total || got[0].Allow != nil || got[0].Deny != nil {
			t.Fatalf("attempts = %+v", got)
		}
	})

	t.Run("one tier per preferred server, in order", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{DHCPServers: "1.1.1.1,2.2.2.2,3.3.3.3"})
		if err != nil {
			t.Fatal(err)
		}
		got := acquisitionAttempts(pol, false, total)
		if len(got) != 3 {
			t.Fatalf("want 3 attempts, got %d", len(got))
		}
		for i, want := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
			if len(got[i].Allow) != 1 || got[i].Allow[0] != want {
				t.Fatalf("attempt %d restricted to %v, want %q", i, got[i].Allow, want)
			}
		}
	})

	// The ladder must divide the existing budget, never extend it: the
	// one-shot acquisition already runs against a tight ceiling (#403,
	// #417) and buying ordering with extra seconds there would trade a
	// rare misconfiguration for a common regression.
	t.Run("tier budgets never sum above the total", func(t *testing.T) {
		for _, list := range []string{
			"1.1.1.1",
			"1.1.1.1,2.2.2.2",
			"1.1.1.1,2.2.2.2,3.3.3.3",
			"1.1.1.1,2.2.2.2,3.3.3.3,4.4.4.4,5.5.5.5",
		} {
			pol, err := resolveServerPolicy(DHCPNetworkOptions{DHCPServers: list})
			if err != nil {
				t.Fatal(err)
			}
			var sum time.Duration
			for _, a := range acquisitionAttempts(pol, false, total) {
				sum += a.Budget
			}
			if sum > total {
				t.Fatalf("%q: attempts sum to %v, over the %v budget", list, sum, total)
			}
		}
	})

	// Both directives are DHCPv4-only. Applying them to a v6 exchange
	// would restrict nothing while implying it had.
	t.Run("v6 gets one unrestricted attempt whatever the policy", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{
			DHCPServers: "1.1.1.1,2.2.2.2",
			DenyServers: "3.3.3.3",
		})
		if err != nil {
			t.Fatal(err)
		}
		got := acquisitionAttempts(pol, true, total)
		if len(got) != 1 {
			t.Fatalf("want 1 v6 attempt, got %d", len(got))
		}
		if got[0].Allow != nil || got[0].Deny != nil {
			t.Fatalf("v6 attempt carries v4-only directives: %+v", got[0])
		}
		if got[0].Budget != total {
			t.Fatalf("v6 budget = %v, want the whole %v", got[0].Budget, total)
		}
	})

	t.Run("deny-only keeps one attempt but carries the blacklist", func(t *testing.T) {
		pol, err := resolveServerPolicy(DHCPNetworkOptions{DenyServers: "3.3.3.3"})
		if err != nil {
			t.Fatal(err)
		}
		got := acquisitionAttempts(pol, false, total)
		if len(got) != 1 || got[0].Budget != total {
			t.Fatalf("attempts = %+v", got)
		}
		if strings.Join(got[0].Deny, ",") != "3.3.3.3" {
			t.Fatalf("deny = %v", got[0].Deny)
		}
	})
}
