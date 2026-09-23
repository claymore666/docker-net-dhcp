// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The unit tests pass sandboxGoneIn a t.TempDir(), so none saw that production's directory was not mounted into the
// plugin; it asserts the input, not sandboxGone's answer, which would need the race #566 and #558 removed (#567).

// TestSandboxNetnsIsVisibleToThePlugin checks that the sandbox netns directory is readable inside the running plugin (#567).
func TestSandboxNetnsIsVisibleToThePlugin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		netName = "dh-itest-netns-vis"
		ctrName = "dh-itest-netns-vis-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// Two absolute readings, not a delta, but the #405 guard is textual, so this reads through the counter window too.
	w := harness.BeginCounterWindow(t, ctx, cli)

	// -1 means the mount is missing, as in every release before #567.
	before := w.Before()
	if before.SandboxNetnsVisible == nil {
		t.Fatal("sandbox_netns_visible is not published by this plugin — it cannot be " +
			"judged, and reading its absence as a value is how #567 stayed invisible")
	}
	if *before.SandboxNetnsVisible < 0 {
		t.Fatalf("sandbox_netns_visible = %d before any container: the plugin cannot read the "+
			"sandbox netns directory at all, so sandboxGone can never answer anything but "+
			"\"no usable evidence\" (#567). config.json must bind-mount one of the paths in "+
			"sandboxNetnsDirs into the plugin", *before.SandboxNetnsVisible)
	}

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	_, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container ip=%s", ipv4)

	// Zero with a container attached is a readable mount from the wrong place, where sandboxGoneIn would call every
	// container vanished (#567).
	_, during := w.End()
	if during.SandboxNetnsVisible == nil {
		t.Fatal("sandbox_netns_visible vanished from the health payload mid-test")
	}
	if *during.SandboxNetnsVisible < 1 {
		t.Fatalf("sandbox_netns_visible = %d while a container is attached "+
			"(active_endpoints=%d): the plugin can read the sandbox netns directory but sees "+
			"nothing in it, so it is mounted from the wrong place. sandboxGone would conclude "+
			"every container had vanished (#567)",
			*during.SandboxNetnsVisible, during.ActiveEndpoints)
	}
	t.Logf("plugin sees %d sandbox netns entr(ies) with %d endpoint(s) attached",
		*during.SandboxNetnsVisible, during.ActiveEndpoints)
}
