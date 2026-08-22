// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	"golang.org/x/sys/unix"
)

// TestNetnsIdentityProbe measures, for #785, whether the plugin can
// authenticate a container's network namespace by a KERNEL IDENTITY
// instead of by the substring match on a name the attacker chooses.
//
// # It decides nothing about the design, and that is deliberate
//
// Every assertion here is on a PREMISE — the plugin was found, the
// containers came up, a sandbox key was issued. The measurements
// themselves are logged, not asserted. A probe that asserted its own
// expected answer could only ever confirm it; the point is to find out
// which of two designs is possible before either is written.
//
// The thing it must not do is look like a result when it never ran,
// so every premise is a t.Fatalf with the reason.
//
// # Why it runs the SAME arrangement twice, in two orders
//
// The design this replaced -- stat(SandboxKey) inside the plugin and
// compare inodes against /proc/<pid>/ns/net -- does not fail
// uniformly. dockerd creates each sandbox key as its own nsfs mount,
// and config.json rbinds /var/run/docker rprivate, so a mount made
// AFTER the plugin started does not propagate into it: the plugin
// stats the placeholder file dockerd created before bind-mounting over
// it, gets a tmpfs inode, and refuses. A sandbox that existed BEFORE
// the plugin started was captured by the rbind and resolves correctly.
//
// So the setup order selects which answer you get, and nothing about a
// setup order looks like an assertion -- it reads as scaffolding.
// "Start a container, then install the plugin" is the order anyone
// would write to test anything else, and it is the order that passes.
// Measuring one arrangement proves the arrangement was reachable, not
// that the check works. Hence two, and their expected results DIFFER.
//
// # What it cannot answer
//
// The reads below run in the plugin's MOUNT namespace but in this test
// process, which does not carry the plugin's AppArmor profile. That
// half is reported separately, out of /proc/<plugin>/attr/current: if
// the plugin is unconfined the question does not arise, and if it is
// not, this probe has not answered it.
func TestNetnsIdentityProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	netName := "dh-itest-nsprobe-net"
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	// ---- arrangement 1: the sandbox exists BEFORE the plugin process
	// we measure from. Created now, then the plugin is recycled, so the
	// rbind in the NEW plugin's mount namespace captures it.
	beforeID, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-nsprobe-before")
	beforeKey := harness.LiveSandboxKey(t, ctx, cli, beforeID)
	beforePID := containerPID(t, ctx, cli, beforeID)

	// Not parallel-safe: recycling the plugin takes RPC service away
	// from every other test. Same constraint as
	// TestRecovery_PluginDisableEnable_PreservesEndpoint.
	t.Cleanup(func() {
		bg, c := context.WithTimeout(context.Background(), 60*time.Second)
		defer c()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})
	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 30*time.Second); err != nil {
		t.Fatalf("waiting for the plugin to go down: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("waiting for the plugin to come back: %v", err)
	}

	pluginPID := findPluginPID(t, ctx, cli)
	t.Logf("plugin pid=%d apparmor=%q", pluginPID, procAttrCurrent(pluginPID))

	// ---- arrangement 2: the sandbox is created AFTER that plugin
	// process started. This is every container in production.
	afterID, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-nsprobe-after")
	afterKey := harness.LiveSandboxKey(t, ctx, cli, afterID)
	afterPID := containerPID(t, ctx, cli, afterID)

	for _, tc := range []struct {
		arrangement string
		key         string
		pid         int
	}{
		{"BEFORE the plugin started", beforeKey, beforePID},
		{"AFTER the plugin started", afterKey, afterPID},
	} {
		if tc.key == "" {
			t.Fatalf("%s: no sandbox key — the measurement did not run", tc.arrangement)
		}
		truth := mustReadlink(t, fmt.Sprintf("/proc/%d/ns/net", tc.pid))
		t.Logf("=== %s ===", tc.arrangement)
		t.Logf("  SandboxKey                 %s", tc.key)
		t.Logf("  C. truth: readlink ns/net  %s", truth)

		inPluginMountNS(t, pluginPID, func() {
			// A. the dead design's route: stat the key by path.
			var st unix.Statfs_t
			if err := unix.Statfs(tc.key, &st); err != nil {
				t.Logf("  A. statfs(SandboxKey)      ERROR %v", err)
			} else {
				t.Logf("  A. statfs(SandboxKey) type 0x%x (%s)", uint64(st.Type), harness.FSName(uint64(st.Type)))
			}

			// D. can the host mount table be read from in here at all?
			raw, err := os.ReadFile("/proc/1/mountinfo")
			if err != nil {
				t.Logf("  D. /proc/1/mountinfo       UNREADABLE %v", err)
				return
			}
			t.Logf("  D. /proc/1/mountinfo       %d bytes, %d nsfs rows", len(raw), harness.CountFSType(raw, "nsfs"))

			// A': the equality that was specified first, reported
			// beside the basename match so the record shows which one
			// holds. On a distro where /var/run is a symlink to /run
			// these disagree.
			if root := harness.MountRootFor(raw, func(p string) bool { return p == tc.key }); root != "" {
				t.Logf("  A'. field5 == SandboxKey   %s", root)
			} else {
				t.Logf("  A'. field5 == SandboxKey   NO MATCH")
			}

			// B. the basename match, split with the same rule
			// splitSandboxKeyIn uses.
			root := harness.MountRootFor(raw, harness.SandboxKeyMatcher(tc.key, probeNetnsDirs))
			if root == "" {
				t.Logf("  B. basename match          NO MATCH")
				return
			}
			t.Logf("  B. basename match field4   %s", root)
			if root == truth {
				t.Logf("  C. field4 == readlink      MATCH")
			} else {
				t.Logf("  C. field4 == readlink      DIFFER (%s vs %s)", root, truth)
			}
		})
	}

	probeJoinWindow(t, ctx, cli, pluginPID, netName)
}

// probeJoinWindow answers the third reading, and it is the one neither
// arrangement above reaches: at what point is the mapping observable?
//
// Both arrangements measure a container that is already up. A check
// that lives in Join runs earlier than that, and if libnetwork sends
// Join BEFORE dockerd has bind-mounted the sandbox netns, then the
// mapping is absent for a reason that has nothing to do with
// authorization -- and a fail-closed check refuses every legitimate
// call while looking exactly like a working one. That is the same
// failure as the two designs already discarded, arriving a third way,
// so it is measured rather than reasoned about.
//
// The evidence is two timestamps that can be compared by eye and by
// nothing else: the wall clock at which a NEW sandbox row first became
// visible inside the plugin's mount namespace, and the plugin's own log
// lines for the Join it served. Nothing is asserted about their order
// -- the probe's job is to produce the pair.
func probeJoinWindow(t *testing.T, ctx context.Context, cli *docker.Client, pluginPID int, netName string) {
	t.Log("=== when does the mapping become observable? ===")

	before := make(chan map[string]string, 1)
	firstSeen := make(chan string, 1)
	stop := make(chan struct{})

	go func() {
		runtime.LockOSThread()
		f, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", pluginPID))
		if err != nil {
			t.Errorf("join-window: opening the plugin mount namespace: %v", err)
			before <- nil
			return
		}
		defer f.Close()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			t.Errorf("join-window: unshare CLONE_FS: %v", err)
			before <- nil
			return
		}
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNS); err != nil {
			t.Errorf("join-window: setns: %v", err)
			before <- nil
			return
		}
		base := sandboxRows()
		before <- base
		for {
			select {
			case <-stop:
				firstSeen <- ""
				return
			default:
			}
			for mp, root := range sandboxRows() {
				if _, had := base[mp]; !had {
					firstSeen <- fmt.Sprintf("%s -> %s first visible at %s",
						mp, root, time.Now().Format(time.RFC3339Nano))
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	if base := <-before; base == nil {
		t.Fatal("join-window: the watcher never entered the plugin namespace — the measurement did not run")
	}

	t.Logf("  docker run starting at      %s", time.Now().Format(time.RFC3339Nano))
	id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-nsprobe-window")
	t.Logf("  docker run returned at      %s", time.Now().Format(time.RFC3339Nano))
	close(stop)

	if seen := <-firstSeen; seen != "" {
		t.Logf("  %s", seen)
	} else {
		t.Log("  NO new sandbox row became visible inside the plugin while the container started")
	}

	_, logData, err := harness.PluginLog(ctx)
	if err != nil {
		t.Logf("  plugin log unavailable: %v — the Join timestamps are missing from this record", err)
		return
	}
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	for _, line := range strings.Split(string(logData), "\n") {
		if strings.Contains(line, "Join") || strings.Contains(line, short) {
			t.Logf("  plugin| %s", line)
		}
	}
}

// sandboxRows returns the sandbox mount points currently visible under
// the known netns directories, as seen by the caller. Called from
// inside the plugin's mount namespace, so an unreadable table is
// reported as an empty set by the caller's own base comparison rather
// than mistaken for "no sandboxes".
func sandboxRows() map[string]string {
	raw, err := os.ReadFile("/proc/1/mountinfo")
	if err != nil {
		return nil
	}
	return harness.MountPointsUnder(raw, probeNetnsDirs)
}

// probeNetnsDirs mirrors pkg/plugin's sandboxNetnsDirs, which is
// unexported. The fix will use the real variable; the probe only has to
// name the same two spellings to take the measurement.
var probeNetnsDirs = []string{"/var/run/docker/netns", "/run/docker/netns"}

// inPluginMountNS runs fn on a thread that has entered the plugin's
// mount namespace, and leaves that thread behind.
//
// The Unshare(CLONE_FS) is not optional: Linux refuses setns
// CLONE_NEWNS while the caller still shares filesystem state with
// another task, and every Go runtime thread does. pkg/plugin's
// writeContainerResolvConf carries the same three lines and the same
// reason.
//
// The thread is never returned to the host namespace and never
// unlocked, so the Go runtime retires it when this goroutine ends.
// That is cheaper than the alternative and it removes the failure mode
// where a half-restored thread poisons an unrelated test.
func inPluginMountNS(t *testing.T, pid int, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		f, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", pid))
		if err != nil {
			t.Errorf("opening the plugin's mount namespace: %v — the measurement did not run", err)
			return
		}
		defer f.Close()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			t.Errorf("unshare CLONE_FS: %v — the measurement did not run", err)
			return
		}
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNS); err != nil {
			t.Errorf("setns into the plugin's mount namespace: %v — the measurement did not run", err)
			return
		}
		fn()
	}()
	<-done
}

func containerPID(t *testing.T, ctx context.Context, cli *docker.Client, id string) int {
	t.Helper()
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", id, err)
	}
	if info.State == nil || info.State.Pid == 0 {
		t.Fatalf("container %s has no pid — the measurement did not run", id)
	}
	return info.State.Pid
}

// findPluginPID locates the running plugin by its rootfs: a managed
// plugin's task has /var/lib/docker/plugins/<id>/rootfs as its root,
// and the id comes from PluginInspect rather than from a name match,
// so this cannot land on some other process that merely looks like the
// plugin.
func findPluginPID(t *testing.T, ctx context.Context, cli *docker.Client) int {
	t.Helper()
	p, _, err := cli.PluginInspectWithRaw(ctx, harness.PluginRef)
	if err != nil {
		t.Fatalf("PluginInspect: %v", err)
	}
	want := filepath.Join("/var/lib/docker/plugins", p.ID, "rootfs")

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("reading /proc: %v", err)
	}
	for _, e := range entries {
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid == 0 {
			continue
		}
		root, err := os.Readlink(fmt.Sprintf("/proc/%d/root", pid))
		if err != nil {
			continue
		}
		if root == want {
			return pid
		}
	}
	t.Fatalf("no process has %s as its root — the plugin was not found, so the measurement did not run", want)
	return 0
}

func procAttrCurrent(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/attr/current", pid))
	if err != nil {
		return fmt.Sprintf("unreadable (%v)", err)
	}
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	s, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v — the measurement did not run", path, err)
	}
	return s
}
