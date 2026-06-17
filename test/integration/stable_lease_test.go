//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestStableLease_IPvlanSameIPAcrossRecreate is the #219 headline.
//
// ipvlan children share the parent's MAC, so a DHCP server keying on MAC
// can't tell two of them apart — and the default per-endpoint client-id is
// minted from the random Docker endpoint ID, which changes on every
// recreate. With stable_lease=true the client-id is derived from the
// container's stable identity (here, its --name), so the same logical
// container presents the same option 61 across stop/rm/recreate and
// dnsmasq (which keys leases on the client-identifier by default) hands
// back the same address.
//
// The test runs a named container, records its IP, removes it, recreates
// it under the same name (a fresh random endpoint ID), and asserts the IP
// is identical. As a control on the mechanism — not just the outcome — it
// also confirms the derived client-id surfaced in the lease file is stable
// across the two incarnations.
func TestStableLease_IPvlanSameIPAcrossRecreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-stablelease-recreate"
	ctrName := "dh-itest-stablelease-recreate-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "ipvlan", map[string]string{
		"stable_lease": "true",
	})

	id1, ip1, mac1 := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("incarnation 1: id=%s ip=%s mac=%s", id1[:12], ip1, mac1)
	cid1 := leaseClientIDForIP(t, ip1)

	// Tear the container down fully (Leave + DeleteEndpoint → DHCPRELEASE),
	// then recreate under the same name. The endpoint ID is fresh and
	// random; only the stable identity carries over.
	removeContainer(t, ctx, id1)

	id2, ip2, mac2 := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("incarnation 2: id=%s ip=%s mac=%s", id2[:12], ip2, mac2)

	if ip2 != ip1 {
		t.Errorf("stable_lease: recreated %q got IP %s, want the prior lease %s", ctrName, ip2, ip1)
	}
	cid2 := leaseClientIDForIP(t, ip2)
	if cid2 != cid1 {
		t.Errorf("derived client-id changed across recreate: %q -> %q (the lease only stays put because the id does)", cid1, cid2)
	} else {
		t.Logf("stable client-id %q held across recreate; lease stayed at %s", cid1, ip1)
	}
}

// TestStableLease_IPvlanDistinctIdentitiesDistinctIPs is the negative
// control: stable_lease must not collapse different containers onto one
// lease. Two differently-named containers on the same ipvlan network
// derive different client-ids and so must get different IPs — exactly the
// per-container distinction the shared parent MAC otherwise erases (a
// MAC-keyed server would see one client for both).
func TestStableLease_IPvlanDistinctIdentitiesDistinctIPs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-stablelease-distinct"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "ipvlan", map[string]string{
		"stable_lease": "true",
	})

	idA, ipA, _ := harness.RunContainer(t, ctx, netName, "dh-itest-stablelease-distinct-a")
	idB, ipB, _ := harness.RunContainer(t, ctx, netName, "dh-itest-stablelease-distinct-b")
	t.Logf("A: id=%s ip=%s | B: id=%s ip=%s", idA[:12], ipA, idB[:12], ipB)

	if ipA == ipB {
		t.Errorf("distinct containers under stable_lease must get distinct IPs; both got %s", ipA)
	}
}

// TestStableLease_RejectedOnBridgeAndMacvlan pins the ipvlan-only
// contract: stable_lease only stabilizes ipvlan (where the client-id is
// the sole per-container handle). bridge/macvlan lease stability is the
// deterministic-MAC work, not yet available, so the option is rejected at
// CreateNetwork rather than silently ignored.
func TestStableLease_RejectedOnBridgeAndMacvlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	cases := []struct {
		mode    string
		options map[string]string
	}{
		{"bridge", map[string]string{"mode": "bridge", "bridge": harness.BridgeName, "stable_lease": "true"}},
		{"macvlan", map[string]string{"mode": "macvlan", "parent": harness.HostVeth, "stable_lease": "true"}},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			netName := "dh-itest-stablelease-reject-" + c.mode
			res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
				Driver:  harness.DriverName,
				IPAM:    &network.IPAM{Driver: "null"},
				Options: c.options,
			})
			if err == nil {
				_ = cli.NetworkRemove(context.Background(), res.ID)
				t.Fatalf("NetworkCreate accepted stable_lease=true in mode=%s; should have rejected", c.mode)
			}
			if !strings.Contains(err.Error(), "stable_lease") {
				t.Errorf("rejection message should mention stable_lease; got: %q", err.Error())
			}
		})
	}
}

// removeContainer stops and removes a container synchronously within a
// test (RunContainer's own cleanup is best-effort and deferred to teardown;
// the recreate scenario needs the name freed *now*).
func removeContainer(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop(%s): %v", id, err)
	}
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove(%s): %v", id, err)
	}
}

// leaseClientIDForIP reads dnsmasq's lease file and returns the client-id
// column (last whitespace field) of the lease line for the given IP. The
// lease format is `<expiry> <mac> <ip> <hostname> <client-id>`. Fails the
// test if no line for the IP appears within a short FS-sync window.
func leaseClientIDForIP(t *testing.T, ip string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fixture.LeaseFile())
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 5 && fields[2] == ip {
					return fields[len(fields)-1]
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no lease line found for IP %s within 5s (lease file %s)", ip, fixture.LeaseFile())
	return "" // unreachable
}
