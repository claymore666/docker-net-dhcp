//go:build integration

package integration

import (
	"context"
	"os"
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
// dhcpcd's `request` directive (DHCP option 50) so dnsmasq hands out
// the caller-chosen lease rather than picking from the pool.
//
// Exercises pkg/plugin/network.go::parseDriverOptIP (whose only
// not-trivial branch was a 0%-coverage gap in v0.7.0) and
// resolveExplicitV4 (the agreed-value return path).
//
// The address is RESERVED in the fixture — harness.StaticTestIP, pinned
// by a --dhcp-host on harness.StaticTestMAC — not merely picked high in
// the pool. The previous comment here claimed dnsmasq allocates "from
// the low end upward" so a high address would stay free. That is not how
// dnsmasq allocates; it hashes the client identity across the whole
// range. The address was never reserved and this test was a coin flip:
// it passed three consecutive runs on one commit and then failed twice
// on that same commit, drawing .89 once and .12 once.
//
// The reservation keys on the MAC, which the test pins, rather than on
// the hostname: initialDHCPHostname is best-effort and returns "" when
// the endpoint is not yet bound to a container, so a hostname key would
// reintroduce a race. Hostname is still set, but only to keep the
// dnsmasq log readable — that log is how this was diagnosed.
func TestStaticIP_DriverOpt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const netName = "dh-itest-staticip"
	ctrName := harness.StaticTestHostname
	wantIP := harness.StaticTestIP

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
			// Not what the reservation keys on — that is the MAC below
			// — but it makes the dnsmasq log readable, which is how
			// this test's failure was diagnosed in the first place.
			Hostname: harness.StaticTestHostname,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {
					DriverOpts: map[string]string{"ip": wantIP},
					// Must match the fixture's --dhcp-host reservation.
					// Fixed rather than Docker-assigned so the address
					// cannot be handed to anyone else, and keyed on the
					// MAC rather than the hostname because the hostname
					// is best-effort at DISCOVER time.
					MacAddress: harness.StaticTestMAC,
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

	// The address matching is necessary but not sufficient: before the
	// reservation existed this test passed three runs in a row on an
	// address nothing was holding for it, then failed twice. A pass is
	// only evidence that the reservation works if the SERVER says it
	// leased this address to the reserved MAC. Docker's view cannot
	// distinguish "reserved" from "free by luck".
	assertServerLeasedTo(t, wantIP, harness.StaticTestMAC)

	// Inside-container view must agree (truthfulness invariant).
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, wantIP) {
		t.Errorf("eth0 inside container does not show requested IP %q\nactual:\n%s", wantIP, out)
	}
}

// assertServerLeasedTo reads dnsmasq's own log and requires a DHCPACK
// handing ip to mac.
//
// This is the outside evidence the suite is supposed to prefer:
// Docker's endpoint view proves the container ended up with an
// address, not that the server chose it for the reason we think. The
// distinction is not academic — TestStaticIP_DriverOpt spent its whole
// life passing on an unreserved address, and the container view looked
// identical on the runs where it was lucky and the run where it was
// not.
func assertServerLeasedTo(t *testing.T, ip, mac string) {
	t.Helper()

	data, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read dnsmasq log %s: %v — cannot confirm the server leased %s to %s, "+
			"and an unreadable log is not evidence of anything",
			fixture.DnsmasqLog(), err, ip, mac)
	}

	ok, acks := harness.ACKedTo(data, ip, mac)
	if ok {
		return
	}
	if len(acks) == 0 {
		t.Errorf("dnsmasq never ACKed %s at all, yet Docker reports the container holds it", ip)
		return
	}
	t.Errorf("dnsmasq ACKed %s but never to the reserved MAC %s — the --dhcp-host "+
		"reservation is not in effect and this test is back to competing with the "+
		"dynamic pool.\nACKs seen:\n\t%s", ip, mac, strings.Join(acks, "\n\t"))
}
