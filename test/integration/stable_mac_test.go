//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestStableMAC_BridgeRecreatePreservesMACAndIP is the core #218 guarantee:
// with `-o stable_mac=true`, a container that is fully destroyed and
// recreated under the same name comes back with the same MAC and the same
// DHCP lease — no manual IP pin, no server-side reservation.
//
// This is the `compose up -d` scenario, distinct from `docker restart`:
// the old endpoint is torn down and the replacement gets a fresh,
// randomized endpoint ID, so the per-endpoint tombstone (which covers the
// restart case) does NOT apply. To prove the stability is attributable to
// the deterministic MAC and not the tombstone, the replacement is created
// with a DIFFERENT hostname: the tombstone is keyed on (network, hostname),
// so a fresh hostname guarantees it cannot supply the MAC. The container
// NAME — which is what the stable MAC is derived from — is unchanged, so
// any stability we observe comes solely from the identity-derived MAC and
// its matching stable client-id.
func TestStableMAC_BridgeRecreatePreservesMACAndIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-stablemac-net"
	ctrName := "dh-itest-stablemac-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"stable_mac": "true"})

	id1, ip1, mac1 := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("first start:    ip=%s mac=%s", ip1, mac1)
	harness.AssertBridgeIP(t, ip1)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// Full destroy — the next create is a brand-new endpoint.
	if err := cli.ContainerStop(ctx, id1, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if err := cli.ContainerRemove(ctx, id1, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	id2, ip2, mac2 := runWithHostname(t, ctx, cli, netName, ctrName, "dh-itest-stablemac-fresh-host")
	t.Logf("after recreate: ip=%s mac=%s", ip2, mac2)
	_ = id2

	if mac2 != mac1 {
		t.Errorf("MAC changed across recreate: before=%s after=%s (stable_mac did not reproduce the identity-derived MAC)", mac1, mac2)
	}
	if ip2 != ip1 {
		t.Errorf("IP changed across recreate: before=%s after=%s (stable MAC/client-id did not recover the same lease)", ip1, ip2)
	}
}

// TestStableMAC_BridgeOffChangesMAC is the control: without stable_mac,
// the same destroy/recreate-with-fresh-hostname cycle yields a different
// MAC (kernel-random). It proves the recreate genuinely re-addresses the
// endpoint, so the stability asserted above is the feature at work and not
// an artifact of Docker or the DHCP fixture handing back the same values.
func TestStableMAC_BridgeOffChangesMAC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-stablemac-off-net"
	ctrName := "dh-itest-stablemac-off-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	// No stable_mac opt — default behaviour.
	harness.CreateNetwork(t, ctx, netName, "bridge", nil)

	id1, ip1, mac1 := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("first start:    ip=%s mac=%s", ip1, mac1)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	if err := cli.ContainerStop(ctx, id1, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if err := cli.ContainerRemove(ctx, id1, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	_, _, mac2 := runWithHostname(t, ctx, cli, netName, ctrName, "dh-itest-stablemac-off-fresh-host")
	t.Logf("after recreate: mac=%s", mac2)

	if mac2 == mac1 {
		t.Errorf("MAC unexpectedly stable without stable_mac: %s — control invalid, the positive test proves nothing", mac1)
	}
}

// TestStableMAC_IpvlanNoOp asserts that enabling stable_mac on an ipvlan
// network is a harmless no-op: ipvlan children share the parent's MAC on
// the wire, so a per-child MAC can't influence the upstream view (the
// stable-client-id arm that would cover ipvlan is the separate #219).
// The container must still come up with a valid lease and the create must
// not error.
func TestStableMAC_IpvlanNoOp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-stablemac-ipvlan-net"
	ctrName := "dh-itest-stablemac-ipvlan-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "ipvlan", map[string]string{"stable_mac": "true"})
	_, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("ipvlan+stable_mac: ip=%s mac=%s", ip, mac)
	harness.AssertIP(t, ip)
}

// runWithHostname creates and starts a container with an explicit
// hostname (distinct from its name) on the given network, polls until it
// has an IP, and returns its id, IPv4 and MAC. It registers a cleanup.
// Used by the recreate tests to take a fresh hostname so the
// hostname-keyed tombstone can't mask the stable-MAC behaviour under test.
func runWithHostname(t *testing.T, ctx context.Context, cli *docker.Client, networkName, containerName, hostname string) (id, ipv4, mac string) {
	t.Helper()

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: hostname,
		},
		&container.HostConfig{AutoRemove: false},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		},
		nil,
		containerName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", containerName, err)
	}
	id = create.ID
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, id, container.StopOptions{})
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(%s): %v", containerName, err)
	}

	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect(%s): %v", id, err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				return id, ep.IPAddress, ep.MacAddress
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("recreated container %s did not get an IP within %v", containerName, harness.IPAcquisitionBudget)
	return
}
