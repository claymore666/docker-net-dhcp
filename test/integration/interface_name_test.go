// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The engine forwards com.docker.network.endpoint.ifname to remote plugins, but until moby/moby#52866 (merged
// 2026-08-26, shipped in 29.8.0) the remote proxy discarded the plugin's DstName; measured with a nested daemon per
// line, 28.5.2 and 29.7.2 name the interface by the driver prefix and 29.8.0 names it as asked (#125, #670). The
// plugin's half is asserted on any engine; the engine's half is probed at runtime, not gated on a version.
package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

const ifnameOpt = "com.docker.network.endpoint.ifname"

// runContainerWithIfname starts a container on netName with the ifname driver-opt and returns its id and IPv4 once it has one.
func runContainerWithIfname(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName, ifname string) (string, string) {
	t.Helper()
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {DriverOpts: map[string]string{ifnameOpt: ifname}},
		}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, id, container.StopOptions{})
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ep := ins.NetworkSettings.Networks[netName]; ep != nil && ep.IPAddress != "" {
			return id, ep.IPAddress
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s got no IP within %v", ctrName, harness.IPAcquisitionBudget)
	return "", ""
}

// engineAppliesIfname reports whether the running engine actually renamed the interface.
func engineAppliesIfname(t *testing.T, ctx context.Context, ctrID, ifname string) bool {
	t.Helper()
	out := harness.ExecOutput(t, ctx, ctrID, "ip", "-o", "link")
	return strings.Contains(out, ": "+ifname+"@") || strings.Contains(out, ": "+ifname+":")
}

// TestInterfaceName_PluginHonorsOption checks that the ifname option leaves the lease intact and that the plugin's statement about the name matches the engine (#125).
func TestInterfaceName_PluginHonorsOption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-ifname"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	// The engine's version decides which statement is owed, asked of the engine and not the plugin (#670).
	srv, err := cli.ServerVersion(ctx)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	engineApplies := engineVersionAppliesIfname(srv.Version)

	window := harness.BeginCounterWindow(t, ctx, cli, "ifname_unsupported")

	// Sibling tests log the same name, so the read is scoped to this attach.
	logMark := harness.MarkPluginLog(t, ctx)
	id, ip := runContainerWithIfname(t, ctx, cli, netName, "dh-itest-ifname-ctr", "lan0")

	if !strings.Contains(harness.ExecOutput(t, ctx, id, "ip", "-4", "addr"), ip+"/") {
		t.Errorf("leased address %s not present on the container link", ip)
	}

	wantStatement := "Honoring custom interface name"
	if !engineApplies {
		wantStatement = "older than the first that applies a remote driver's interface name"
	}
	logTxt := harness.AwaitPluginLogSince(t, ctx, logMark, 5*time.Second, func(window string) bool {
		return strings.Contains(window, wantStatement) && strings.Contains(window, "lan0")
	})
	if !strings.Contains(logTxt, wantStatement) || !strings.Contains(logTxt, "lan0") {
		t.Errorf("engine %s: the plugin log does not say %q for lan0.\n"+
			"On an engine that ignores the name, a log line claiming it was honoured is "+
			"the only thing standing between the operator and an interface named "+
			"something they did not ask for.", srv.Version, wantStatement)
	}

	applied := engineAppliesIfname(t, ctx, id, "lan0")
	if applied != engineApplies {
		t.Errorf("engine %s: the container interface is named lan0 = %v, and this suite expected %v "+
			"from the version alone. The measured boundary in this file is wrong, or the engine "+
			"changed behaviour inside a line.", srv.Version, applied, engineApplies)
	}

	// The counter must not move on an engine that applies the name (#670).
	before, after := window.End()
	delta := after.IfnameUnsupported - before.IfnameUnsupported
	switch {
	case !engineApplies && delta < 1:
		t.Errorf("engine %s ignores the requested interface name and ifname_unsupported moved by %d, want at least 1",
			srv.Version, delta)
	case engineApplies && delta != 0:
		t.Errorf("engine %s applies the requested interface name and ifname_unsupported moved by %d, want 0",
			srv.Version, delta)
	}
}

// This suite's own copy of the boundary, not imported from pkg/plugin, so the cell cannot agree with the code by
// construction; 29.8.0 is the first engine with moby/moby#52866 (#670).

// engineVersionAppliesIfname reports whether an engine version applies a remote driver's DstName.
func engineVersionAppliesIfname(version string) bool {
	fields := strings.SplitN(version, ".", 3)
	if len(fields) < 2 {
		return false
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil {
		return false
	}
	minorField := fields[1]
	for i, r := range minorField {
		if r < '0' || r > '9' {
			minorField = minorField[:i]
			break
		}
	}
	minor, err := strconv.Atoi(minorField)
	if err != nil {
		return false
	}
	if major != 29 {
		return major > 29
	}
	return minor >= 8
}

// TestInterfaceName_InvalidRejected checks that a name the kernel cannot accept fails the attach with the plugin's validation error (#125).
func TestInterfaceName_InvalidRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-ifbad"

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {DriverOpts: map[string]string{ifnameOpt: "way-too-long-interface-name"}},
		}},
		nil, "dh-itest-ifbad-ctr")
	if err != nil {
		// Some engine versions validate at create, which is acceptable as long as the attach cannot succeed.
		t.Logf("rejected at create: %v", err)
		return
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})

	err = cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	if err == nil {
		t.Fatal("ContainerStart succeeded with a 27-byte interface_name; Join validation did not fire")
	}
	if !strings.Contains(err.Error(), "IFNAMSIZ") && !strings.Contains(err.Error(), "interface_name") {
		t.Errorf("start failed but not with the plugin's validation error: %v", err)
	}
}

// The two networks are on different parents in different subnets: libnetwork refuses a second interface in a subnet
// the container already routes ("cannot program address ... conflicts with existing route"), on both sides of
// moby/moby#52866, and the reporter's mDNS bridge relays between subnets (#125).

// TestInterfaceName_MultiNetworkDeterministic checks that one container on two plugin networks maps each fixed name to its own network on every restart (#125).
func TestInterfaceName_MultiNetworkDeterministic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// The engine probe runs before the second fixture exists: EphemeralFixture asserts on teardown that it granted a
	// lease (#472), so a fixture created ahead of a skip reported FAIL on every engine below 29.8.0.
	probeNet := "dh-itest-ifprobe"
	harness.CreateNetwork(t, ctx, probeNet, "macvlan", nil)
	probeID, _ := runContainerWithIfname(t, ctx, cli, probeNet, "dh-itest-ifprobe-ctr", "probe0")
	if !engineAppliesIfname(t, ctx, probeID, "probe0") {
		t.Skip("engine does not apply remote-driver DstName yet (moby drivers/remote/driver.go drops it); test activates once the upstream pass-through ships")
	}

	// The suite-static fixture serves 192.168.99.0/24 on HostVeth; this one serves 192.168.101.0/24 on its own parent.
	ef := harness.NewEphemeralFixture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	netA := "dh-itest-ifnetA"
	netB := "dh-itest-ifnetB"
	harness.CreateNetwork(t, ctx, netA, "macvlan", nil)
	harness.CreateNetwork(t, ctx, netB, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netA: {DriverOpts: map[string]string{ifnameOpt: "wan0"}},
			netB: {DriverOpts: map[string]string{ifnameOpt: "lan0"}},
		}},
		nil, "dh-itest-ifmulti-ctr")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, id, container.StopOptions{})
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})

	macForName := func(name string) string {
		out := harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show", name)
		for _, f := range strings.Fields(out) {
			if strings.Count(f, ":") == 5 && len(f) == 17 {
				return strings.ToLower(f)
			}
		}
		return ""
	}

	// The subnet each name carries says it is the network the compose file named, not just a stable MAC.
	v4ForName := func(name string) string {
		out := harness.ExecOutput(t, ctx, id, "ip", "-o", "-4", "addr", "show", name)
		for _, f := range strings.Fields(out) {
			if strings.Contains(f, ".") && strings.Contains(f, "/") {
				return f
			}
		}
		return ""
	}

	var wanMAC, lanMAC string
	for restart := 0; restart < 3; restart++ {
		if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			t.Fatalf("ContainerStart (round %d): %v", restart, err)
		}
		deadline := time.Now().Add(harness.IPAcquisitionBudget)
		var w, l string
		for time.Now().Before(deadline) {
			w, l = macForName("wan0"), macForName("lan0")
			if w != "" && l != "" {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if w == "" || l == "" {
			t.Fatalf("round %d: wan0/lan0 not both present (wan0=%q lan0=%q)", restart, w, l)
		}

		wantWan, wantLan := "192.168.99.", "192.168.101."
		gotWan, gotLan := v4ForName("wan0"), v4ForName("lan0")
		if !strings.HasPrefix(gotWan, wantWan) {
			t.Errorf("round %d: wan0 carries %q, want an address in %s0/24 (netA)", restart, gotWan, wantWan)
		}
		if !strings.HasPrefix(gotLan, wantLan) {
			t.Errorf("round %d: lan0 carries %q, want an address in %s0/24 (netB)", restart, gotLan, wantLan)
		}

		if restart == 0 {
			wanMAC, lanMAC = w, l
		} else {
			if w != wanMAC || l != lanMAC {
				t.Errorf("round %d: name<->MAC mapping changed: wan0 %s->%s, lan0 %s->%s", restart, wanMAC, w, lanMAC, l)
			}
		}
		if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
			t.Fatalf("ContainerStop (round %d): %v", restart, err)
		}
	}
}
