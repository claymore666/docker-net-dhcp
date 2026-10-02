// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// ipv6_temporary asks for an IA_TA beside the IA_NA (RFC 8415 section 21.5, #927). The observers are the server's lease
// file, where dnsmasq writes an IA_TA lease with a "T" IAID, and the container's own `ip -6 addr`. Both fixtures draw
// from a range 2^32 + 1 wide, so the stable and the temporary address collide about once in 4e9 runs, not 1 in 138.

// tmp6Leases reads the stable and temporary addresses the server holds for duid.
type tmp6Leases func(duid string) (stable, temporary []string)

// tmp6AwaitLeases polls the server's lease file until it holds a stable address for duid and, when wantTemp is set, a
// temporary one too.
func tmp6AwaitLeases(t *testing.T, read tmp6Leases, duid string, wantTemp bool) (stable, temporary []string) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		stable, temporary = read(duid)
		if len(stable) > 0 && (!wantTemp || len(temporary) > 0) {
			return stable, temporary
		}
	}
	t.Errorf("the server's lease file holds stable %v and temporary %v for %s, want a stable address and temporary=%v",
		stable, temporary, duid, wantTemp)
	return stable, temporary
}

func tmp6GlobalSet(t *testing.T, ctx context.Context, ctrID string) map[netip.Addr]bool {
	t.Helper()
	out := harness.ExecOutput(t, ctx, ctrID, "ip", "-6", "addr", "show", "scope", "global")
	set := map[netip.Addr]bool{}
	for _, f := range strings.Fields(out) {
		if !strings.Contains(f, "/") {
			continue
		}
		if a, err := netip.ParsePrefix(f); err == nil && a.Addr().Is6() {
			set[a.Addr()] = true
		}
	}
	return set
}

// tmp6AwaitHeld polls the container link until it holds every address in want.
func tmp6AwaitHeld(t *testing.T, ctx context.Context, ctrID string, want ...string) map[netip.Addr]bool {
	t.Helper()
	var held map[netip.Addr]bool
	for end := time.Now().Add(harness.IPAcquisitionBudget); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		held = tmp6GlobalSet(t, ctx, ctrID)
		all := true
		for _, w := range want {
			if a, err := netip.ParseAddr(w); err != nil || !held[a] {
				all = false
			}
		}
		if all {
			return held
		}
	}
	t.Errorf("the container's global addresses are %v, want all of %v", held, want)
	return held
}

func tmp6EndpointHealth(t *testing.T, ctx context.Context, netID string) *harness.EndpointHealth {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	want := shortDockerID(netID)
	h := harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second,
		"the endpoint on network "+want+" to report lease_state=bound",
		func(h *harness.HealthResponse) bool {
			for _, e := range h.Endpoints {
				if e.Network == want {
					return e.LeaseState == "bound"
				}
			}
			return false
		})
	for i := range h.Endpoints {
		if h.Endpoints[i].Network == want {
			return &h.Endpoints[i]
		}
	}
	t.Fatalf("no endpoint on network %s in /Plugin.Health", want)
	return nil
}

func tmp6Inspect(t *testing.T, ctx context.Context, ctrID, netName string) string {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	return inspectV6(t, ctx, cli, ctrID, netName)
}

// tmp6AssertHeld checks the four places the temporary address must show: the server granted it, the container
// holds it beside the stable one, Docker reports the stable one, and /Plugin.Health names it (#927).
func tmp6AssertHeld(t *testing.T, ctx context.Context, read tmp6Leases, ctrID, netName, netID, duid string) {
	t.Helper()
	stable, temporary := tmp6AwaitLeases(t, read, duid, true)
	if len(stable) == 0 || len(temporary) == 0 {
		return
	}
	held := tmp6AwaitHeld(t, ctx, ctrID, stable[0], temporary[0])
	if len(held) < 2 {
		t.Errorf("the container holds %d global IPv6 address(es), want the stable and the temporary one", len(held))
	}

	reported := tmp6Inspect(t, ctx, ctrID, netName)
	for _, tmp := range temporary {
		if ip := net.ParseIP(reported); ip != nil && ip.Equal(net.ParseIP(tmp)) {
			t.Errorf("docker reports %s as the container's IPv6 address, which is a temporary one", reported)
		}
	}
	isStable := false
	for _, s := range stable {
		if ip := net.ParseIP(reported); ip != nil && ip.Equal(net.ParseIP(s)) {
			isStable = true
		}
	}
	if !isStable {
		t.Errorf("docker reports %q, want one of the server's stable addresses %v", reported, stable)
	}

	e := tmp6EndpointHealth(t, ctx, netID)
	pfx, err := netip.ParsePrefix(e.IPv6TemporaryAddress)
	if err != nil {
		t.Fatalf("/Plugin.Health ipv6_temporary_address = %q, want a CIDR", e.IPv6TemporaryAddress)
	}
	if !held[pfx.Addr()] {
		t.Errorf("/Plugin.Health says the temporary address is %s, the container holds %v", pfx, held)
	}
	granted := false
	for _, tmp := range temporary {
		if a, perr := netip.ParseAddr(tmp); perr == nil && a == pfx.Addr() {
			granted = true
		}
	}
	if !granted {
		t.Errorf("/Plugin.Health says the temporary address is %s, the server granted %v", pfx, temporary)
	}
}

// tmp6AssertNone is the control: the same server, the option off, no IA_TA lease and no health field.
func tmp6AssertNone(t *testing.T, ctx context.Context, read tmp6Leases, ctrID, netID, duid string) {
	t.Helper()
	stable, temporary := tmp6AwaitLeases(t, read, duid, false)
	if len(temporary) != 0 {
		t.Errorf("the server holds temporary addresses %v for %s although the network did not ask for an IA_TA", temporary, duid)
	}
	if len(stable) > 0 {
		tmp6AwaitHeld(t, ctx, ctrID, stable[0])
	}
	if e := tmp6EndpointHealth(t, ctx, netID); e.IPv6TemporaryAddress != "" {
		t.Errorf("/Plugin.Health shows ipv6_temporary_address=%q on a network without the option", e.IPv6TemporaryAddress)
	}
}

// tmp6Ephemeral is the dnsmasq backend serving DHCPv6 from a range wide enough that two draws never collide (#927).
func tmp6Ephemeral(t *testing.T) *harness.EphemeralFixture {
	t.Helper()
	ef := harness.NewEphemeralFixture(t, harness.WithV6Range(harness.EphemeralServerAddrV6,
		harness.EphemeralWideV6Start, harness.EphemeralWideV6End))
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
	return ef
}

// TestIPv6Temporary_MacvlanHoldsAStableAndATemporaryAddress checks that ipv6_temporary reaches the macvlan endpoint's Solicit and installs the IA_TA address (#927).
func TestIPv6Temporary_MacvlanHoldsAStableAndATemporaryAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ef := tmp6Ephemeral(t)

	netName := "dh-itest-tmp6"
	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth, "ipv6_mode": "dhcp", "ipv6_temporary": "true"})
	id, _, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	tmp6AssertHeld(t, ctx, ef.V6LeaseAddrs, id, netName, netID, "00:03:00:01:"+strings.ToLower(mac))
}

// TestIPv6Temporary_MacvlanWithoutTheOptionAsksForNoIATA checks that a network without the key sends no IA_TA, on a server that would grant one (#927).
func TestIPv6Temporary_MacvlanWithoutTheOptionAsksForNoIATA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ef := tmp6Ephemeral(t)

	netName := "dh-itest-tmp6-off"
	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth, "ipv6_mode": "dhcp"})
	id, _, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	tmp6AssertNone(t, ctx, ef.V6LeaseAddrs, id, netID, "00:03:00:01:"+strings.ToLower(mac))
}

// TestIPv6Temporary_IPAMMacvlanHoldsAStableAndATemporaryAddress checks that ipv6_temporary reaches the IPAM-mode lease, the fifth client-options site (#927).
func TestIPv6Temporary_IPAMMacvlanHoldsAStableAndATemporaryAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	ef := tmp6Ephemeral(t)

	netName := "dh-itest-tmp6-ipam"
	netID := harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "", nil, map[string]string{
		"parent": harness.EphemeralHostVeth, "ipv6": "true", "ipv6_mode": "dhcp", "ipv6_temporary": "true"})
	id, _, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	tmp6AssertHeld(t, ctx, ef.V6LeaseAddrs, id, netName, netID, "00:03:00:01:"+strings.ToLower(mac))
}

// TestIPv6Temporary_BridgeHoldsAStableAndATemporaryAddress checks that ipv6_temporary reaches the bridge-mode Solicit (#927).
func TestIPv6Temporary_BridgeHoldsAStableAndATemporaryAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	netName := "dh-itest-tmp6-br"
	netID := harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		"ipv6_mode": "dhcp", "ipv6_temporary": "true"})
	id, _, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	read := func(duid string) ([]string, []string) {
		b, err := os.ReadFile(fixture.BridgeLeaseFile())
		if err != nil {
			return nil, nil
		}
		return harness.DnsmasqLease6Addrs(string(b), duid)
	}
	tmp6AssertHeld(t, ctx, read, id, netName, netID, "00:03:00:01:"+strings.ToLower(mac))
}
