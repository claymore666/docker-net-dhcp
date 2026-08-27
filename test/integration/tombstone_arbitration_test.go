// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestRestart_TombstonedAddressIsNeverAlsoReleased covers #800 on the
// wire.
//
// A departing endpoint's address can be reserved for the restart that
// is coming, or handed back to the DHCP server. The plugin used to do
// both for the same endpoint: DeleteEndpoint laid a tombstone saying
// "the next CreateEndpoint re-requests exactly this address", while a
// Join-abort path spawned a reclaim handing that same lease back. The
// symptom that surfaced was a restart 500 — both links are macvlan
// children of the same parent wearing the same MAC, and the kernel
// refuses the second with EADDRINUSE — but the collision is downstream
// of the contradiction, and the contradiction is the thing with a
// consequence for a user: a window in which the server believes the
// lease is free while the container is claiming it.
//
// # Why the assertion is on dnsmasq's log
//
// The plugin's counters can only say what it MEANT to do. Whether the
// server was told the lease is free is a fact about the wire, and
// dnsmasq's log is the only place it exists. The counters below are
// read as diagnostics and printed on failure; nothing here passes or
// fails on them.
//
// # Why this cannot be red on timing
//
// It does not assert which side won. The verdict space is closed and
// both arms assert on the server's log:
//
//   - the container came back on its old address -> the tombstone was
//     honoured, so the server must NOT have seen a release for it.
//   - the container came back on a different address -> the reclaim
//     won, so the server MUST have seen the release, and the plugin
//     must have counted a suppressed tombstone.
//
// Neither arm is vacuous and neither depends on which side of the race
// the run landed on. Only doing BOTH — the shape #800 describes — fails,
// and on unfixed code that is what a fast reconnect produces.
//
// # Why connect/disconnect/reconnect rather than `docker restart`
//
// The reclaim fires only when the departing endpoint's persistent
// client never bound, which is the window between the attach starting
// that client and its DORA completing. Timing a container's exit does
// not reach it reliably; disconnecting immediately after connecting
// does, and it is the technique
// TestOrphanedLease_ReleasedWhenClientNeverBound already relies on.
// Reconnecting the same container under the same hostname is what makes
// the next CreateEndpoint inherit the tombstone.
//
// Deliberately macvlan: the tombstone is skipped entirely for ipvlan
// (its children share the parent's MAC), so ipvlan cannot reach this.
func TestRestart_TombstonedAddressIsNeverAlsoReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-tombarb"
		ctrName = "dh-itest-tombarb-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Started on no plugin network, so every DHCP event in this window
	// belongs to the connect/reconnect below. The hostname is what the
	// tombstone matcher narrows on, so it has to be stable across both.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
		nil, nil, ctrName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg := context.Background()
		if err := cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true}); err != nil {
			t.Logf("WARN: ContainerRemove(%s): %v", id, err)
		}
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	w := harness.BeginCounterWindow(t, ctx, cli,
		"orphan_releases_suppressed", "tombstones_suppressed",
		"orphaned_leases_released", "tombstones_consumed")

	if err := cli.NetworkConnect(ctx, netName, id, &network.EndpointSettings{}); err != nil {
		t.Fatalf("NetworkConnect: %v", err)
	}

	// Read the address before disconnecting: it is the key every
	// assertion hangs on and Docker forgets it the moment the endpoint
	// goes. One inspect against a DORA measured in hundreds of
	// milliseconds keeps the never-bound window open.
	first := endpointIPv4(t, ctx, cli, id, netName)

	if got := fixture.CountLogLines("DHCPACK", first); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the endpoint never took a lease, "+
			"so this run proves nothing about what happened to one", first)
	}
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", first)

	if err := cli.NetworkDisconnect(ctx, netName, id, false); err != nil {
		t.Fatalf("NetworkDisconnect: %v", err)
	}

	// The restart. On unfixed code this is where the two links collide:
	// this CreateEndpoint inherits the tombstoned MAC while the reclaim
	// spawned by the disconnect is wearing it on a release link.
	if err := cli.NetworkConnect(ctx, netName, id, &network.EndpointSettings{}); err != nil {
		t.Fatalf("reconnect after disconnect failed: %v\n"+
			"This is the user-visible shape of #800 — `docker restart` reports "+
			"`address already in use` because the plugin's own orphan-release link "+
			"is still wearing the MAC the restarting child just inherited.", err)
	}

	second := endpointIPv4(t, ctx, cli, id, netName)

	// The reclaim runs detached from any Docker request, so give it the
	// budget it would have had before deciding the server never saw a
	// release. Waiting only in the arm where a release is NOT expected
	// is what keeps the negative honest.
	sameAddress := second == first
	if sameAddress {
		deadline := time.Now().Add(orphanReleaseBudget)
		for time.Now().Before(deadline) {
			if fixture.CountLogLines("DHCPRELEASE", first) > releasesBefore {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	releases := fixture.CountLogLines("DHCPRELEASE", first) - releasesBefore
	before, after := w.End()

	t.Logf("first=%s second=%s releases=%d "+
		"orphan_releases_suppressed=+%d tombstones_suppressed=+%d "+
		"orphaned_leases_released=+%d tombstones_consumed=+%d",
		first, second, releases,
		after.OrphanReleasesSuppressed-before.OrphanReleasesSuppressed,
		after.TombstonesSuppressed-before.TombstonesSuppressed,
		after.OrphanedLeasesReleased-before.OrphanedLeasesReleased,
		after.TombstonesConsumed-before.TombstonesConsumed)

	if sameAddress {
		// The tombstone was honoured. The lease is ours and the server
		// must never have been told otherwise.
		if releases > 0 {
			t.Fatalf("the container came back on %s, but dnsmasq logged %d DHCPRELEASE(s) "+
				"for that address in the same window — the plugin reserved the lease for "+
				"the restart and handed it back to the server at the same time (#800). "+
				"Between the release and the container's own request the server was free "+
				"to give %s to somebody else.", first, releases, first)
		}
		return
	}

	// The reclaim won: the lease really was handed back, so the
	// container correctly came back on a different address. The server
	// must show the release that justifies losing it, and the plugin
	// must have recorded that it declined to write the tombstone.
	if releases < 1 {
		t.Errorf("the container came back on %s instead of %s, but dnsmasq logged no "+
			"DHCPRELEASE for %s — the address was abandoned rather than released, "+
			"and it stays held upstream until it expires",
			second, first, first)
	}
	if got := after.TombstonesSuppressed - before.TombstonesSuppressed; got < 1 {
		t.Errorf("tombstones_suppressed advanced by %d, want at least 1: the address "+
			"changed, so a tombstone must have been declined — and if it was not, "+
			"the address changed for some other reason and this run is not "+
			"measuring #800 at all", got)
	}
}
