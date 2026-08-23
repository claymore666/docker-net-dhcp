// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "testing"

// The renewal half of #731.
//
// dhcp_server_policy_exhausted is written by acquireWithPolicy, which
// walks the dhcp_servers ladder and can run off the end of it. The
// PERSISTENT client has no ladder: it holds one whitelist for the life
// of the endpoint and, when every named server has gone, simply gets no
// answers. The only thing that showed was a dhcp_timeouts tick, which
// is the same thing a real outage produces — so an allow-list naming a
// server that had been renumbered, retired or firewalled read as "DHCP
// is down" and sent the operator to look at the wrong machine.
//
// The counter's whole meaning is a RELATIONSHIP: it is a strict subset
// of dhcp_timeouts, and that is how it is read (both rising = the
// allow-list is the cause; dhcp_timeouts alone = it is not). A
// relationship between two counters cannot be held by a comment on
// either of them, so it is asserted here on both at once.

func policyTickPlugin() *dhcpManager {
	return &dhcpManager{plugin: &Plugin{}}
}

// dhcp_timeouts has had no single backing field since #766 split every
// DHCP counter by family: the exposition adds the two, and the subset
// property below is a claim about that sum, not about either half. Read
// it the same way the Health handler does, so a future split cannot
// leave this test asserting against a fraction of the counter it names.
func aggregateTimeouts(p *Plugin) int32 {
	return p.dhcpTimeoutsV4.Load() + p.dhcpTimeoutsV6.Load()
}

func TestCountOutageTick_PolicyTimeoutsAreASubsetOfTimeouts(t *testing.T) {
	tests := []struct {
		name             string
		v6               bool
		policyRestricted bool
		wantTimeouts     int32
		wantV6           int32
		wantPolicy       int32
		why              string
	}{
		{
			name:         "unrestricted v4 outage",
			wantTimeouts: 1,
			wantPolicy:   0,
			why:          "no allow-list is in force, so nothing here is about dhcp_servers",
		},
		{
			name:             "restricted v4 outage",
			policyRestricted: true,
			wantTimeouts:     1,
			wantPolicy:       1,
			why:              "the endpoint's renewal client can only accept from the named servers",
		},
		{
			name:         "v6 outage",
			v6:           true,
			wantTimeouts: 1,
			wantV6:       1,
			wantPolicy:   0,
			why:          "dhcp_servers is v4-only, so a v6 client is never restricted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := policyTickPlugin()
			m.countOutageTick(tc.v6, tc.policyRestricted)

			if got := aggregateTimeouts(m.plugin); got != tc.wantTimeouts {
				t.Errorf("dhcp_timeouts = %d, want %d", got, tc.wantTimeouts)
			}
			if got := m.plugin.dhcpTimeoutsV6.Load(); got != tc.wantV6 {
				t.Errorf("dhcp_timeouts{ipv6} = %d, want %d", got, tc.wantV6)
			}
			if got := m.plugin.dhcpServerPolicyTimeouts.Load(); got != tc.wantPolicy {
				t.Errorf("dhcp_server_policy_timeouts = %d, want %d — %s", got, tc.wantPolicy, tc.why)
			}
		})
	}
}

// The subset property itself, over a mixed sequence rather than one
// tick. Counting the policy tick in place of the generic one — or in
// addition to a second generic one — would leave every single-tick case
// above green.
func TestCountOutageTick_SubsetHoldsAcrossAMixedSequence(t *testing.T) {
	m := policyTickPlugin()

	ticks := []struct{ v6, restricted bool }{
		{false, true},
		{false, false},
		{true, false},
		{false, true},
		{false, false},
		{true, false},
	}
	for _, tk := range ticks {
		m.countOutageTick(tk.v6, tk.restricted)
	}

	timeouts := aggregateTimeouts(m.plugin)
	policy := m.plugin.dhcpServerPolicyTimeouts.Load()

	if timeouts != int32(len(ticks)) {
		t.Errorf("dhcp_timeouts = %d, want %d: every tick is counted there, restricted or not", timeouts, len(ticks))
	}
	if policy != 2 {
		t.Errorf("dhcp_server_policy_timeouts = %d, want 2", policy)
	}
	if policy > timeouts {
		t.Errorf("dhcp_server_policy_timeouts (%d) exceeds dhcp_timeouts (%d); it is documented as a strict subset and read as one", policy, timeouts)
	}
}

// The premise that lets dhcp_server_policy_timeouts skip the family
// split every other DHCP counter carries: a v6 client is never
// restricted, so a v6 sibling would be a permanent zero. Asserted
// rather than commented, because a permanent-zero counter is the kind
// of thing nobody notices is wrong.
func TestClientServerLists_V6IsNeverRestricted(t *testing.T) {
	pol, err := resolveServerPolicy(DHCPNetworkOptions{
		DHCPServers: "192.168.0.1,192.168.0.2",
	})
	if err != nil {
		t.Fatalf("resolveServerPolicy: %v", err)
	}

	if allow, _ := clientServerLists(pol, false); len(allow) != 2 {
		t.Fatalf("v4 allow-list = %v, want the two named servers; the v6 case below is only "+
			"meaningful if v4 does get them", allow)
	}

	allow, deny := clientServerLists(pol, true)
	if len(allow) != 0 || len(deny) != 0 {
		t.Errorf("v6 client got allow=%v deny=%v, want neither: dhcpcd stores both lists as "+
			"in_addr_t and dhcp6.c never reads them, so a restricted v6 client is not a thing "+
			"that can exist", allow, deny)
	}
}

// Same, for a deny-only policy: denyList() is non-empty when there is
// no preference list, and it too must stop at v4.
func TestClientServerLists_V6DropsTheDenyListToo(t *testing.T) {
	pol, err := resolveServerPolicy(DHCPNetworkOptions{DenyServers: "192.168.0.9"})
	if err != nil {
		t.Fatalf("resolveServerPolicy: %v", err)
	}

	if _, deny := clientServerLists(pol, false); len(deny) != 1 {
		t.Fatalf("v4 deny-list = %v, want the one named server", deny)
	}
	if allow, deny := clientServerLists(pol, true); len(allow) != 0 || len(deny) != 0 {
		t.Errorf("v6 client got allow=%v deny=%v, want neither", allow, deny)
	}
}
