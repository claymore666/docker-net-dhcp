// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

func preferPolicy(t *testing.T, addrs ...string) serverPolicy {
	t.Helper()
	pol := serverPolicy{}
	for _, a := range addrs {
		parsed, err := netip.ParseAddr(a)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", a, err)
		}
		pol.Prefer = append(pol.Prefer, parsed)
	}
	return pol
}

func servers(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i % 256)}).String())
	}
	return out
}

// TestAcquisitionAttempts_NoAttemptIsStarved is the #731 regression,
// and it pins the GUARANTEE rather than the constant.
//
// The ladder divided a fixed 10s budget by the number of preferred
// servers with no floor. Every attempt is a full dhcp.NewDHCPClient --
// an unshare, a dhcpcd spawn, FIFO setup, then a round trip -- so six
// servers bought 1.66s each and twenty bought 500ms. An operator who
// filled the option in carefully got an acquisition that failed where
// naming nothing would have succeeded.
//
// Deliberately asserted as ">= minAttemptBudget" and not "== 3s": the
// number is a policy choice and may move, while "no attempt is ever
// given less time than one exchange needs" is the property that must
// not.
func TestAcquisitionAttempts_NoAttemptIsStarved(t *testing.T) {
	const total = 10 * time.Second

	for _, n := range []int{1, 2, 3, 4, 6, 20, 200} {
		pol := preferPolicy(t, servers(n)...)
		attempts := acquisitionAttempts(pol, false, total)

		if len(attempts) == 0 {
			t.Fatalf("%d servers: no attempts at all", n)
		}
		var sum time.Duration
		for i, a := range attempts {
			if a.Budget < minAttemptBudget {
				t.Errorf("%d servers: attempt %d got %v, below the %v floor — an attempt that cannot "+
					"outlive its own dhcpcd spawn is not a fast attempt, it is a guaranteed failure",
					n, i, a.Budget, minAttemptBudget)
			}
			sum += a.Budget
		}

		// The ladder DIVIDES the budget; it must never extend it. A
		// preference list that makes `docker run` slower is the
		// regression this feature was explicitly built to avoid
		// (#403, #417), and a floor is the obvious fix that would
		// have caused it.
		if sum > total {
			t.Errorf("%d servers: attempts total %v, over the %v budget — the ladder must divide the budget, "+
				"never extend it", n, sum, total)
		}
	}
}

// TestAcquisitionAttempts_OrderingIsKeptWhereItFits pins what packing
// costs and, more importantly, what it does not.
//
// The tail shares one attempt only once the list outgrows the budget.
// Merging the TAIL rather than the head is the design: the operator
// wrote the list in preference order, so the entries that lose their
// own attempt must be the ones they ranked lowest.
func TestAcquisitionAttempts_OrderingIsKeptWhereItFits(t *testing.T) {
	const total = 10 * time.Second

	t.Run("a list that fits keeps one attempt per server", func(t *testing.T) {
		pol := preferPolicy(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
		attempts := acquisitionAttempts(pol, false, total)

		if len(attempts) != 3 {
			t.Fatalf("got %d attempts, want 3 — three servers fit in a 10s budget at a 3s floor", len(attempts))
		}
		for i, want := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
			if len(attempts[i].Allow) != 1 || attempts[i].Allow[0] != want {
				t.Errorf("attempt %d allows %v, want exactly [%s] — strict ordering must survive wherever it fits",
					i, attempts[i].Allow, want)
			}
		}
	})

	t.Run("an oversized list keeps its head and groups its tail", func(t *testing.T) {
		pol := preferPolicy(t, "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5")
		attempts := acquisitionAttempts(pol, false, total)

		if len(attempts) != 3 {
			t.Fatalf("got %d attempts, want 3", len(attempts))
		}
		for i, want := range []string{"10.0.0.1", "10.0.0.2"} {
			if len(attempts[i].Allow) != 1 || attempts[i].Allow[0] != want {
				t.Errorf("attempt %d allows %v, want exactly [%s] — the operator's top preferences are the ones "+
					"that must keep their own attempt", i, attempts[i].Allow, want)
			}
		}
		last := attempts[2].Allow
		if len(last) != 3 || last[0] != "10.0.0.3" || last[1] != "10.0.0.4" || last[2] != "10.0.0.5" {
			t.Errorf("last attempt allows %v, want [10.0.0.3 10.0.0.4 10.0.0.5] — the tail is asked as a group, "+
				"in order, and nothing may be dropped from the list the operator wrote", last)
		}
	})

	t.Run("nothing is dropped, however long the list", func(t *testing.T) {
		const n = 50
		pol := preferPolicy(t, servers(n)...)
		attempts := acquisitionAttempts(pol, false, total)

		seen := 0
		for _, a := range attempts {
			seen += len(a.Allow)
		}
		if seen != n {
			t.Errorf("attempts name %d servers in total, want %d — packing must regroup the list, never "+
				"truncate it; a silently dropped server is a server the operator believes is being tried", seen, n)
		}
	})
}

// TestAcquisitionAttempts_UnrestrictedPathIsUntouched: the packing only
// exists for a preference ladder. A network with no dhcp_servers, and
// every v6 exchange (dhcpcd's whitelist is DHCPv4-only), must still get
// one unrestricted attempt with the WHOLE budget.
func TestAcquisitionAttempts_UnrestrictedPathIsUntouched(t *testing.T) {
	const total = 10 * time.Second

	cases := []struct {
		name string
		pol  serverPolicy
		v6   bool
	}{
		{name: "no preference list", pol: serverPolicy{}},
		{name: "v6 ignores the ladder entirely", pol: preferPolicy(t, servers(20)...), v6: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts := acquisitionAttempts(tc.pol, tc.v6, total)
			if len(attempts) != 1 {
				t.Fatalf("got %d attempts, want 1", len(attempts))
			}
			if attempts[0].Budget != total {
				t.Errorf("budget %v, want the full %v", attempts[0].Budget, total)
			}
			if len(attempts[0].Allow) != 0 {
				t.Errorf("allow list %v, want empty", attempts[0].Allow)
			}
		})
	}
}

// TestPackTiers_ATinyBudgetStillRunsOnce guards the degenerate end. If
// the total budget is ever configured below one floor, the ladder must
// collapse to a single attempt rather than to zero — an acquisition
// that makes no attempt at all fails without ever asking anybody.
func TestPackTiers_ATinyBudgetStillRunsOnce(t *testing.T) {
	pol := preferPolicy(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	attempts := acquisitionAttempts(pol, false, minAttemptBudget/2)

	if len(attempts) == 0 {
		t.Fatal("no attempts for a sub-floor budget; the acquisition would fail without asking any server")
	}
	seen := 0
	for _, a := range attempts {
		seen += len(a.Allow)
	}
	if seen != 3 {
		t.Errorf("attempts name %d servers, want 3 — a short budget may collapse the ladder, not shorten the list", seen)
	}
}

// TestAcquireWithPolicy_FallbacksCountStepsNotAcquisitions pins the
// semantics #731 found described wrongly in three of the four places
// that describe them.
//
// The counter has always bumped once per STEP down the ladder, which is
// the more useful number — it says how far acquisition had to walk, not
// merely that it walked. Three copies said "acquisitions". The code was
// right and the prose was wrong, so the prose moved; this is what stops
// the next reader from "fixing" the code to match a sentence.
func TestAcquireWithPolicy_FallbacksCountStepsNotAcquisitions(t *testing.T) {
	cases := []struct {
		name          string
		answerOn      int // 1-based attempt that succeeds; 0 = none ever does
		wantFallbacks int32
		wantExhausted int32
		reason        string
	}{
		{
			name: "three silent preferred servers add two, not one", answerOn: 0,
			wantFallbacks: 2, wantExhausted: 1,
			reason: "two steps were taken down a three-entry ladder; a per-acquisition reading would say 1 and " +
				"lose how far down the list the failure reached",
		},
		{
			name: "answered by the second entry", answerOn: 2,
			wantFallbacks: 1, wantExhausted: 0,
			reason: "one step down, and the acquisition succeeded — the policy was not exhausted",
		},
		{
			name: "answered by the first entry", answerOn: 1,
			wantFallbacks: 0, wantExhausted: 0,
			reason: "the preferred server answered; nothing fell back",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			restore := dhcpGetIP
			dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, error) {
				calls++
				if tc.answerOn != 0 && calls == tc.answerOn {
					return dhcp.Info{IP: "192.168.0.50/24"}, nil
				}
				return dhcp.Info{}, errors.New("no response")
			}
			t.Cleanup(func() { dhcpGetIP = restore })

			p := &Plugin{}
			pol := preferPolicy(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
			_, err := p.acquireWithPolicy(t.Context(), "eth0", pol, false, 10*time.Second, "ep-1", dhcp.DHCPClientOptions{})

			if (err == nil) != (tc.answerOn != 0) {
				t.Fatalf("err = %v for answerOn=%d", err, tc.answerOn)
			}
			if got := p.dhcpServerTierFallbacks.Load(); got != tc.wantFallbacks {
				t.Errorf("dhcp_server_tier_fallbacks: got %d, want %d — %s", got, tc.wantFallbacks, tc.reason)
			}
			if got := p.dhcpServerPolicyExhausted.Load(); got != tc.wantExhausted {
				t.Errorf("dhcp_server_policy_exhausted: got %d, want %d — %s", got, tc.wantExhausted, tc.reason)
			}
		})
	}
}
