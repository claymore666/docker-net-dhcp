//go:build integration

// interface_name support (#125). Characterization result (from moby
// source, daemon/libnetwork/drivers/remote/driver.go): the engine
// forwards the endpoint option com.docker.network.endpoint.ifname to
// remote plugins in CreateEndpoint and Join, and the remote-driver
// API's InterfaceName response carries DstName — but the proxy calls
// `iface.SetNames(SrcName, DstPrefix, "")`, DISCARDING the plugin's
// DstName. Built-in drivers got per-driver interface_name in engine
// 28; remote drivers were left out. So:
//   - the plugin's side (validate + return DstName) is fully
//     assertable today, via its own logs and the Join error path;
//   - whether the ENGINE applies the name is probed at runtime —
//     the dependent tests skip with a pointer to the upstream gap
//     until a fixed engine runs this suite, then activate on their
//     own. No version-number gate: the probe tests the actual
//     behaviour, which is the thing that matters.
package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

const ifnameOpt = "com.docker.network.endpoint.ifname"

// runContainerWithIfname creates and starts a container on netName
// with the ifname endpoint driver-opt, returning (id, ipv4) once the
// endpoint has an IP. Cleanup registered.
func runContainerWithIfname(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName, ifname string) (string, string) {
	t.Helper()
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		&container.HostConfig{},
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

// engineAppliesIfname reports whether the running engine actually
// renamed the interface — the capability probe the dependent tests
// gate on.
func engineAppliesIfname(t *testing.T, ctx context.Context, ctrID, ifname string) bool {
	t.Helper()
	out := harness.ExecOutput(t, ctx, ctrID, "ip", "-o", "link")
	return strings.Contains(out, ": "+ifname+"@") || strings.Contains(out, ": "+ifname+":")
}

// TestInterfaceName_PluginHonorsOption asserts the plugin's half of
// #125, which is fully testable on any engine: a container attached
// with the ifname driver-opt gets a working DHCP lease, and the
// plugin's Join honored the option (its log records the custom name).
// The engine half is probed and reported; until the upstream remote-
// driver pass-through lands, the interface still comes up as ethN and
// the lease must be unaffected either way.
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
	id, ip := runContainerWithIfname(t, ctx, cli, netName, "dh-itest-ifname-ctr", "lan0")

	// The lease itself must be unaffected by the option.
	if !strings.Contains(harness.ExecOutput(t, ctx, id, "ip", "-4", "addr"), ip+"/") {
		t.Errorf("leased address %s not present on the container link", ip)
	}

	// Plugin half: Join logged that it honored the name (the response
	// DstName). This is the assertable contract on every engine.
	logTxt := harness.ReadPluginLog(t, ctx)
	if !strings.Contains(logTxt, "Honoring custom interface name") || !strings.Contains(logTxt, "lan0") {
		t.Error("plugin log shows no 'Honoring custom interface name' for lan0 — Join did not consume the ifname option")
	}

	// Engine half: probe and report. Not a failure either way — the
	// engine-dependent assertions live in the gated tests below.
	if engineAppliesIfname(t, ctx, id, "lan0") {
		t.Log("engine APPLIES remote-driver DstName — upstream pass-through is live on this runner")
	} else {
		t.Log("engine ignores remote-driver DstName (expected: moby drivers/remote/driver.go drops it); interface remains ethN")
	}
}

// TestInterfaceName_InvalidRejected: a name the kernel could never
// accept must fail the attach loudly at Join with the plugin's
// validation error — not surface as a cryptic rename failure. Fully
// engine-independent.
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
		&container.HostConfig{},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {DriverOpts: map[string]string{ifnameOpt: "way-too-long-interface-name"}},
		}},
		nil, "dh-itest-ifbad-ctr")
	if err != nil {
		// Some engine versions validate at create — also acceptable,
		// as long as the attach can't succeed.
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

// TestInterfaceName_MultiNetworkDeterministic is the reporter's
// actual pain (#125): one container on two plugin networks with fixed
// names must map names to networks identically on every restart.
// Gated on the engine actually applying DstName — skips with the
// upstream pointer until then, activates by itself on a fixed engine.
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

	// Probe with a throwaway container first.
	probeNet := "dh-itest-ifprobe"
	harness.CreateNetwork(t, ctx, probeNet, "macvlan", nil)
	probeID, _ := runContainerWithIfname(t, ctx, cli, probeNet, "dh-itest-ifprobe-ctr", "probe0")
	if !engineAppliesIfname(t, ctx, probeID, "probe0") {
		t.Skip("engine does not apply remote-driver DstName yet (moby drivers/remote/driver.go drops it); test activates once the upstream pass-through ships")
	}

	netA := "dh-itest-ifnetA"
	netB := "dh-itest-ifnetB"
	harness.CreateNetwork(t, ctx, netA, "macvlan", nil)
	harness.CreateNetwork(t, ctx, netB, "macvlan", nil)

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		&container.HostConfig{},
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

	// macForName maps interface name -> MAC inside the container.
	macForName := func(name string) string {
		out := harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show", name)
		for _, f := range strings.Fields(out) {
			if strings.Count(f, ":") == 5 && len(f) == 17 {
				return strings.ToLower(f)
			}
		}
		return ""
	}

	var wanMAC, lanMAC string
	for restart := 0; restart < 3; restart++ {
		if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			t.Fatalf("ContainerStart (round %d): %v", restart, err)
		}
		// Both names must exist with stable MAC association.
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
