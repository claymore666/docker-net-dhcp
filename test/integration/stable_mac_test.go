//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
)

// tombstoneExpiryWait is how long the recreate tests idle between
// destroying a container and recreating it. It must exceed the plugin's
// 60s tombstoneTTL so the per-endpoint tombstone — which would otherwise
// inherit the old MAC/IP across a fast recreate regardless of stable_mac
// — has been pruned by the time we recreate. Only then is any observed
// MAC/IP stability attributable to the deterministic stable MAC, not to
// tombstone inheritance. Kept comfortably under the fixture's 2m DHCP
// lease so the upstream server still maps the MAC to its address.
const tombstoneExpiryWait = 70 * time.Second

// TestStableMAC_BridgeRecreatePastTombstone is the core #218 guarantee:
// with `-o stable_mac=true`, a container destroyed and recreated under
// the same name — after the tombstone window has closed — comes back with
// the same MAC and the same DHCP lease, with no static pin and no
// server-side reservation. A control container on a network without
// stable_mac runs the identical cycle and must get a *different* MAC,
// proving the stability is the feature at work and not tombstone
// inheritance or a fixture artifact. Both containers share one tombstone-
// expiry wait to keep the test's wall-clock down.
func TestStableMAC_BridgeRecreatePastTombstone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	stableNet := "dh-itest-stablemac-net"
	plainNet := "dh-itest-stablemac-plain-net"
	stableCtr := "dh-itest-stablemac-ctr"
	plainCtr := "dh-itest-stablemac-plain-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, stableNet, "bridge", map[string]string{"stable_mac": "true"})
	harness.CreateNetwork(t, ctx, plainNet, "bridge", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// First start of both containers.
	id1s, ip1s, mac1s := harness.RunContainer(t, ctx, stableNet, stableCtr)
	id1p, _, mac1p := harness.RunContainer(t, ctx, plainNet, plainCtr)
	t.Logf("stable first:  ip=%s mac=%s", ip1s, mac1s)
	t.Logf("control first: mac=%s", mac1p)
	harness.AssertBridgeIP(t, ip1s)

	// Destroy both, then wait out the tombstone window once.
	destroyContainer(t, ctx, cli, id1s)
	destroyContainer(t, ctx, cli, id1p)
	t.Logf("waiting %s for tombstones to expire so the recreate exercises the stable-MAC path, not tombstone inheritance", tombstoneExpiryWait)
	select {
	case <-time.After(tombstoneExpiryWait):
	case <-ctx.Done():
		t.Fatalf("context expired during tombstone wait: %v", ctx.Err())
	}

	// Recreate both under the same names.
	_, ip2s, mac2s := harness.RunContainer(t, ctx, stableNet, stableCtr)
	_, _, mac2p := harness.RunContainer(t, ctx, plainNet, plainCtr)
	t.Logf("stable recreate:  ip=%s mac=%s", ip2s, mac2s)
	t.Logf("control recreate: mac=%s", mac2p)

	// stable_mac: identity-derived MAC is reproduced past the tombstone
	// window, and the upstream server returns the same lease for it.
	if mac2s != mac1s {
		t.Errorf("stable_mac: MAC changed across recreate: before=%s after=%s (the identity-derived MAC was not reproduced past the tombstone window)", mac1s, mac2s)
	}
	if ip2s != ip1s {
		t.Errorf("stable_mac: IP changed across recreate: before=%s after=%s (stable MAC/client-id did not recover the same lease)", ip1s, ip2s)
	}

	// control: without stable_mac and past the tombstone window, the veth
	// MAC is kernel-random, so it must differ — otherwise the positive
	// assertion above proves nothing.
	if mac2p == mac1p {
		t.Errorf("control: MAC unexpectedly stable without stable_mac past the tombstone window: %s — the stable_mac assertion is not isolating the feature", mac1p)
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

// destroyContainer stops and removes a container, failing the test if the
// removal errors for any reason other than the container already being
// gone. Used to force a full endpoint teardown before a recreate.
func destroyContainer(t *testing.T, ctx context.Context, cli *docker.Client, id string) {
	t.Helper()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop(%s): %v", id, err)
	}
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove(%s): %v", id, err)
	}
}
