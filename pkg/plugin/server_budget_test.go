// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
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

		// The ladder divides the budget and never extends it, so a preference list cannot slow `docker run` (#403,
		// #417).
		if sum > total {
			t.Errorf("%d servers: attempts total %v, over the %v budget — the ladder must divide the budget, "+
				"never extend it", n, sum, total)
		}
	}
}

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

func TestPackTiers_ATinyBudgetStillRunsOnce(t *testing.T) {
	pol := preferPolicy(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	attempts := acquisitionAttempts(pol, false, minAttemptBudget/2)

	if len(attempts) != 1 {
		t.Fatalf("a sub-floor budget produced %d attempts, want exactly 1.\n"+
			"  Zero would fail without asking anybody; more than one shreds a budget that could not fund\n"+
			"  even a single attempt into slices that certainly cannot -- which is #731's defect reached by\n"+
			"  way of its own fix. The whole of a too-small budget spent on ONE question can still be\n"+
			"  answered by a fast server.",
			len(attempts))
	}
	seen := 0
	for _, a := range attempts {
		seen += len(a.Allow)
	}
	if seen != 3 {
		t.Errorf("attempts name %d servers, want 3 — a short budget may collapse the ladder, not shorten the list", seen)
	}
	if attempts[0].Budget != minAttemptBudget/2 {
		t.Errorf("the single attempt got %v of a %v budget; a collapsed ladder must hand its one attempt "+
			"the whole of what there is", attempts[0].Budget, minAttemptBudget/2)
	}
}

func TestAcquisitionAttempts_NoLadderIsStarvedBelowTheFloor(t *testing.T) {
	servers := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		servers = append(servers, fmt.Sprintf("10.0.0.%d", i))
	}

	cases := []struct {
		name         string
		servers      int
		total        time.Duration
		wantAttempts int
	}{
		{"20 servers, three floors and change -- packed to what the budget funds", 20, 3*minAttemptBudget + minAttemptBudget/3, 3},
		{"20 servers, exactly one floor", 20, minAttemptBudget, 1},
		{"20 servers, half a floor -- below it", 20, minAttemptBudget / 2, 1},
		{"20 servers, a third of a floor -- far below", 20, minAttemptBudget / 3, 1},
		{"6 servers, half a floor -- below the floor with a short list", 6, minAttemptBudget / 2, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pol := preferPolicy(t, servers[:tc.servers]...)
			attempts := acquisitionAttempts(pol, false, tc.total)
			if len(attempts) != tc.wantAttempts {
				t.Fatalf("got %d attempts, want %d", len(attempts), tc.wantAttempts)
			}
			for i, a := range attempts {
				want := minAttemptBudget
				if tc.total < minAttemptBudget {
					want = tc.total
				}
				if a.Budget < want {
					t.Errorf("attempt %d got %v, want at least %v: the ladder starved an attempt it could have funded",
						i, a.Budget, want)
				}
			}
		})
	}
}

func TestAcquireWithPolicy_FallbacksCountStepsNotAcquisitions(t *testing.T) {
	cases := []struct {
		name          string
		answerOn      int
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
			dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
				calls++
				if tc.answerOn != 0 && calls == tc.answerOn {
					return dhcp.Info{IP: "192.168.0.50/24"}, dhcp.RAObservation{}, nil
				}
				return dhcp.Info{}, dhcp.RAObservation{}, errors.New("no response")
			}
			t.Cleanup(func() { dhcpGetIP = restore })

			p := &Plugin{}
			pol := preferPolicy(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
			_, _, err := p.acquireWithPolicy(t.Context(), "eth0", pol, false, 10*time.Second, "ep-1", dhcp.DHCPClientOptions{})

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

func TestAcquisitionAttempts_TheGuaranteeHoldsAtEveryFloor(t *testing.T) {
	servers := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		servers = append(servers, fmt.Sprintf("10.0.0.%d", i))
	}

	floors := []time.Duration{
		250 * time.Millisecond,
		2 * time.Second,
		minAttemptBudget,
		5 * time.Second,
		7 * time.Second,
	}
	multiples := []struct {
		name string
		of   func(time.Duration) time.Duration
	}{
		{"a third of a floor", func(f time.Duration) time.Duration { return f / 3 }},
		{"half a floor", func(f time.Duration) time.Duration { return f / 2 }},
		{"exactly one floor", func(f time.Duration) time.Duration { return f }},
		{"three floors and change", func(f time.Duration) time.Duration { return 3*f + f/3 }},
		{"ten floors", func(f time.Duration) time.Duration { return 10 * f }},
	}

	for _, nServers := range []int{3, 6, 20} {
		pol := preferPolicy(t, servers[:nServers]...)
		for _, floor := range floors {
			for _, m := range multiples {
				total := m.of(floor)
				name := fmt.Sprintf("%d servers/floor %v/%s", nServers, floor, m.name)
				t.Run(name, func(t *testing.T) {
					attempts := acquisitionAttemptsWithFloor(pol, false, total, floor)

					if len(attempts) == 0 {
						t.Fatalf("no attempts at all: a ladder that asks nobody cannot be answered")
					}

					var spent time.Duration
					seen := 0
					for i, a := range attempts {
						spent += a.Budget
						seen += len(a.Allow)
						want := floor
						if total < floor {
							want = total
						}
						if a.Budget < want {
							t.Errorf("attempt %d got %v, want at least %v (floor %v, total %v): "+
								"the ladder starved an attempt it could have funded",
								i, a.Budget, want, floor, total)
						}
					}
					if total < floor && len(attempts) != 1 {
						t.Errorf("a sub-floor budget produced %d attempts, want exactly 1: "+
							"a budget too small for one attempt can still be spent once, "+
							"but it cannot be shredded into slices that certainly fail",
							len(attempts))
					}
					if spent > total {
						t.Errorf("attempts spend %v of a %v budget: the ladder may not make "+
							"`docker run` slower than it is today (#403, #417)", spent, total)
					}
					if seen != nServers {
						t.Errorf("attempts name %d of %d servers; packing merges the tail, "+
							"it never drops it", seen, nServers)
					}
				})
			}
		}
	}
}
