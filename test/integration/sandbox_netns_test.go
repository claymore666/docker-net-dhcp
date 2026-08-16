// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestSandboxNetnsIsVisibleToThePlugin asserts that the evidence
// sandboxGone depends on is actually reachable from inside the running
// plugin (#567).
//
// WHY THE UNIT TESTS DO NOT SUFFICE. sandboxGoneIn takes its
// directories as a parameter so it can be tested without root, and it
// is tested thoroughly — present, absent, empty key, wrong directory,
// unreadable directory. Every one of those cases passes a t.TempDir().
// So they answer "does this logic work, given a readable directory?"
// and never ask what production passes. Production passed a directory
// that was not mounted into the plugin at all, so os.ReadDir failed on
// every call, sandboxGone returned "no usable evidence" forever, and
// the branch was dead for every release up to #567 — with a green unit
// suite the whole time.
//
// The seam that made the function testable is the seam that let the
// tests never touch the failing case. This closes it from the only
// place that can: outside the process, against a real plugin.
//
// IT DELIBERATELY DOES NOT ASSERT sandboxGone's ANSWER. That would need
// a container caught mid-vanish, which is the race #566 and #558 spent
// the day removing from this suite. It asserts the INPUT instead —
// the part that was broken, and the part a mount regression breaks
// again.
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

	// Read through the counter window rather than calling PluginHealth
	// directly. This test wants two ABSOLUTE readings, not a delta, so
	// the hazard the window exists for (#405) does not apply to it —
	// but the guard that enforces the window is textual, deliberately,
	// because a reviewer cannot be expected to catch the thirtieth
	// hand-rolled pair. Taking the sanctioned path costs nothing here
	// and keeps the rule free of the exemption that would erode it.
	w := harness.BeginCounterWindow(t, ctx, cli)

	// Before anything is attached. The directory must be READABLE even
	// with nothing in it: -1 means the mount is missing, which is the
	// state every release before #567 shipped in.
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

	// THE ASSERTION THAT MATTERS. A container is attached, so its
	// sandbox netns exists on the host and the plugin must see it.
	//
	// A zero here is the dangerous reading, not a benign one: the
	// directory is readable but mounted from somewhere with no
	// sandboxes in it. sandboxGoneIn would then match no key and
	// conclude every container had vanished — confidently wrong, which
	// is worse than the "no evidence" it used to return. That is what
	// an unpropagated or misdirected bind mount looks like, and it
	// would satisfy a test that only checked the mount existed.
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
