//go:build integration

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

// TestStaticIP_DriverOpt drives the `--driver-opt ip=<addr>` static-IP
// override path: the container is connected with an explicit
// per-endpoint driver opt, and the plugin must propagate that to
// udhcpc's DHCPDISCOVER as a `requested IP` so dnsmasq hands out the
// caller-chosen lease rather than picking from the pool.
//
// Exercises pkg/plugin/network.go::parseDriverOptIP (whose only
// not-trivial branch was a 0%-coverage gap in v0.7.0) and
// resolveExplicitV4 (the agreed-value return path).
//
// We pick a host high in the pool (.95) to keep the test reproducible
// across suite runs: dnsmasq allocates pool entries from the low end
// upward, and the existing tests rarely consume more than a handful
// of leases before this test runs.
func TestStaticIP_DriverOpt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		netName = "dh-itest-staticip"
		ctrName = "dh-itest-staticip-ctr"
		wantIP  = "192.168.99.95"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Inline a RunContainer-equivalent because the harness helper
	// doesn't take per-endpoint DriverOpts — the static-IP override
	// is the only test that needs them so far. Promote to a harness
	// helper if a second consumer appears.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: harness.TestImage,
			Cmd:   []string{"sleep", "infinity"},
		},
		&container.HostConfig{},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {
					DriverOpts: map[string]string{"ip": wantIP},
				},
			},
		},
		nil,
		ctrName,
	)
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

	// Poll docker inspect until the endpoint reports the IP we asked
	// for. If the plugin ignored the driver-opt and let dnsmasq pick,
	// we'd see a different address from the pool — that's the
	// regression this test guards against.
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var gotIP string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				gotIP = ep.IPAddress
			}
		}
		if gotIP != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if gotIP == "" {
		t.Fatalf("container never got an IP within %v", harness.IPAcquisitionBudget)
	}
	if gotIP != wantIP {
		t.Errorf("static-IP driver-opt was ignored: requested %s, got %s", wantIP, gotIP)
	}

	// Inside-container view must agree (truthfulness invariant).
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, wantIP) {
		t.Errorf("eth0 inside container does not show requested IP %q\nactual:\n%s", wantIP, out)
	}
}
