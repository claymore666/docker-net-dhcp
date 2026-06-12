//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// ledgerLine mirrors pkg/plugin.ledgerEntry — duplicated like
// harness.HealthResponse so the integration package doesn't pull on
// plugin internals.
type ledgerLine struct {
	TS        string `json:"ts"`
	Kind      string `json:"kind"`
	Network   string `json:"network"`
	Endpoint  string `json:"endpoint"`
	Container string `json:"container"`
	Hostname  string `json:"hostname"`
	IP        string `json:"ip"`
	MAC       string `json:"mac"`
}

// readLedger reads STATE_DIR/leases.jsonl out of the plugin's rootfs
// (same host-side access pattern as harness.ReadPluginLog). Returns
// nil when the file doesn't exist yet — callers poll. Any line that
// fails to parse is a test failure: the ledger's contract is that
// every line is valid JSON.
func readLedger(t *testing.T, ctx context.Context, cli *docker.Client) []ledgerLine {
	t.Helper()
	p, _, err := cli.PluginInspectWithRaw(ctx, harness.PluginRef)
	if err != nil {
		t.Fatalf("PluginInspect: %v", err)
	}
	path := filepath.Join("/var/lib/docker/plugins", p.ID, "rootfs/var/lib/net-dhcp/leases.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open ledger %s: %v", path, err)
	}
	defer f.Close()
	var lines []ledgerLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var l ledgerLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("ledger line %d is not valid JSON: %v\n%s", len(lines)+1, err, sc.Text())
		}
		lines = append(lines, l)
	}
	return lines
}

// ledgerKindsForMAC filters the ledger to entries carrying the given
// MAC and returns their kinds in file order.
func ledgerKindsForMAC(lines []ledgerLine, mac string) []string {
	var kinds []string
	for _, l := range lines {
		if strings.EqualFold(l.MAC, mac) {
			kinds = append(kinds, l.Kind)
		}
	}
	return kinds
}

// TestAuditLog_RecordsLifecycle is #109's stated test plan made
// concrete: with audit_log=true, a full container lifecycle leaves a
// bound and a release entry in STATE_DIR/leases.jsonl carrying the
// container's exact MAC and pool IP, every line valid JSON, and
// ledger_write_failures stays flat.
//
// Container lifecycle is inlined (not harness.RunContainer) for the
// same reason as the health-counters test: the release entry is
// written during teardown, which must happen inside the test body.
func TestAuditLog_RecordsLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-audit"
	ctrName := "dh-itest-audit-ctr"

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

	before, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (before): %v", err)
	}

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"audit_log": "true",
	})

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		&container.HostConfig{},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
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
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// Learn the container's MAC + IP from inspect once the endpoint
	// is up, then poll the ledger for the persistent client's bound
	// entry (written on the first DHCPACK after Join).
	var mac, ip string
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ep := ins.NetworkSettings.Networks[netName]; ep != nil && ep.IPAddress != "" {
			mac, ip = ep.MacAddress, ep.IPAddress
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if mac == "" {
		t.Fatalf("container got no IP within %v", harness.IPAcquisitionBudget)
	}

	var bound *ledgerLine
	deadline = time.Now().Add(harness.IPAcquisitionBudget + 5*time.Second)
	for time.Now().Before(deadline) {
		for _, l := range readLedger(t, ctx, cli) {
			if strings.EqualFold(l.MAC, mac) && l.Kind == "bound" {
				bound = &l
				break
			}
		}
		if bound != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if bound == nil {
		t.Fatalf("no bound ledger entry for MAC %s within budget; ledger: %+v", mac, readLedger(t, ctx, cli))
	}
	if bound.IP != ip {
		t.Errorf("bound entry IP = %q, want container's leased IP %q", bound.IP, ip)
	}
	if bound.Network != netID {
		t.Errorf("bound entry network = %q, want created network ID %q", bound.Network, netID)
	}
	if bound.Endpoint == "" {
		t.Error("bound entry has empty endpoint ID")
	}
	if bound.Hostname != ctrName {
		t.Errorf("bound entry hostname = %q, want %q", bound.Hostname, ctrName)
	}
	if _, err := time.Parse(time.RFC3339, bound.TS); err != nil {
		t.Errorf("bound entry ts %q is not RFC3339: %v", bound.TS, err)
	}

	// Stop drives Leave -> dhcpManager.Stop -> DHCPRELEASE -> the
	// release ledger entry.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	var kinds []string
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		kinds = ledgerKindsForMAC(readLedger(t, ctx, cli), mac)
		if len(kinds) > 0 && kinds[len(kinds)-1] == "release" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != "release" {
		t.Fatalf("ledger kinds for MAC %s = %v, want trailing \"release\"", mac, kinds)
	}
	if kinds[0] != "bound" {
		t.Errorf("first ledger kind for MAC %s = %q, want \"bound\"", mac, kinds[0])
	}

	after, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (after): %v", err)
	}
	if after.LedgerWriteFailures != before.LedgerWriteFailures {
		t.Errorf("ledger_write_failures moved %d -> %d; want flat",
			before.LedgerWriteFailures, after.LedgerWriteFailures)
	}
}

// TestAuditLog_DefaultOff pins the opt-in: without audit_log, a full
// lifecycle leaves no trace of this container in the ledger. Asserted
// by MAC absence rather than file absence — STATE_DIR is shared
// plugin state, so other audit-enabled tests may legitimately have
// written the file in the same suite run.
func TestAuditLog_DefaultOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-audit-off"
	ctrName := "dh-itest-audit-off-ctr"

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

	before, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (before): %v", err)
	}

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, mac := harness.RunContainer(t, ctx, netName, ctrName)

	// Wait for the persistent client's bound event (the moment an
	// audit-enabled network would have written its entry), then check
	// the ledger has nothing for this MAC.
	deadline := time.Now().Add(harness.IPAcquisitionBudget + 5*time.Second)
	for time.Now().Before(deadline) {
		h, err := harness.PluginHealth(ctx, cli)
		if err == nil && h.LeasesObtained > before.LeasesObtained {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if kinds := ledgerKindsForMAC(readLedger(t, ctx, cli), mac); len(kinds) != 0 {
		t.Errorf("ledger has entries %v for MAC %s of a non-audit network; want none", kinds, mac)
	}
	_ = id
}
