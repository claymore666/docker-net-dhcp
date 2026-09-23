// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// ledgerLine mirrors pkg/plugin.ledgerEntry, duplicated so this package does not import plugin internals.
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

// STATE_DIR has been bind-mounted from the host's /var/lib/net-dhcp since #440, so the ledger is read there and not
// from the plugin rootfs, whose path is now the empty mount point; the plugin ID is not part of the path.

// readLedger reads STATE_DIR/leases.jsonl, returning nil before the file exists and failing on any invalid JSON line.
func readLedger(t *testing.T, ctx context.Context, cli *docker.Client) []ledgerLine {
	t.Helper()
	path := filepath.Join(harness.HostStateDir, "leases.jsonl")
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

// boundAndNamedRows returns the first bound row for mac and the first bound or renew row for mac that carries name.
func boundAndNamedRows(lines []ledgerLine, mac, name string) (bound, named *ledgerLine) {
	for _, l := range ledgerForMAC(lines, mac) {
		if bound == nil && l.Kind == "bound" {
			bound = &l
		}
		if named == nil && (l.Kind == "bound" || l.Kind == "renew") && l.Hostname == name {
			named = &l
		}
	}
	return bound, named
}

func ledgerForMAC(lines []ledgerLine, mac string) []ledgerLine {
	var out []ledgerLine
	for _, l := range lines {
		if strings.EqualFold(l.MAC, mac) {
			out = append(out, l)
		}
	}
	return out
}

// ledgerKindsForMAC returns the kinds of the entries for mac in file order.
func ledgerKindsForMAC(lines []ledgerLine, mac string) []string {
	var kinds []string
	for _, l := range lines {
		if strings.EqualFold(l.MAC, mac) {
			kinds = append(kinds, l.Kind)
		}
	}
	return kinds
}

// TestAuditLog_RecordsLifecycle checks that audit_log=true writes bound and stopped rows with the container's MAC and IP (#109).
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

	w := harness.BeginCounterWindow(t, ctx, cli, "ledger_write_failures")

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"audit_log": "true",
	})

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
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

	// The name can land on the renew after the bind: the client starts before the daemon answers with the name and renews
	// early to carry it (#961, RFC 2131 section 4.4.5).
	var bound, named *ledgerLine
	deadline = time.Now().Add(harness.IPAcquisitionBudget + 5*time.Second)
	for time.Now().Before(deadline) {
		bound, named = boundAndNamedRows(readLedger(t, ctx, cli), mac, ctrName)
		if bound != nil && named != nil {
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
	if _, err := time.Parse(time.RFC3339, bound.TS); err != nil {
		t.Errorf("bound entry ts %q is not RFC3339: %v", bound.TS, err)
	}
	if bound.Hostname != "" && bound.Hostname != ctrName {
		t.Errorf("bound entry hostname = %q, want %q or empty", bound.Hostname, ctrName)
	}
	if named == nil {
		t.Errorf("no bound or renew ledger entry for MAC %s carries hostname %q within budget; ledger: %+v",
			mac, ctrName, ledgerForMAC(readLedger(t, ctx, cli), mac))
	} else {
		if named.IP != ip {
			t.Errorf("%s entry carrying the name has IP %q, want container's leased IP %q", named.Kind, named.IP, ip)
		}
		if named.Network != netID || named.Endpoint != bound.Endpoint {
			t.Errorf("%s entry carrying the name is for network %q endpoint %q, want %q endpoint %q",
				named.Kind, named.Network, named.Endpoint, netID, bound.Endpoint)
		}
	}
	if got, line := waitLeaseHostname(t, fixture.LeaseFile(), ip, ctrName, harness.IPAcquisitionBudget); got != ctrName {
		t.Errorf("the DHCP server's table has %q as the name for %s, want %q; lease line:\n%s", got, ip, ctrName, line)
	}

	// Since #800 the row is "stopped", not "release": nothing is released, and the ledger may not claim what the server saw.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	var kinds []string
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		kinds = ledgerKindsForMAC(readLedger(t, ctx, cli), mac)
		if len(kinds) > 0 && kinds[len(kinds)-1] == "stopped" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != "stopped" {
		t.Fatalf("ledger kinds for MAC %s = %v, want trailing \"stopped\"", mac, kinds)
	}
	if kinds[0] != "bound" {
		t.Errorf("first ledger kind for MAC %s = %q, want \"bound\"", mac, kinds[0])
	}

	before, after := w.End()
	if after.LedgerWriteFailures != before.LedgerWriteFailures {
		t.Errorf("ledger_write_failures moved %d -> %d; want flat",
			before.LedgerWriteFailures, after.LedgerWriteFailures)
	}
}

// TestAuditLog_DefaultOff checks that without audit_log the ledger holds nothing for the container's MAC.
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

	w := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, mac := harness.RunContainer(t, ctx, netName, ctrName)

	// The bind is required: a container that never bound would satisfy the absence below trivially.
	if _, ok := w.Await(harness.IPAcquisitionBudget+5*time.Second,
		func(now, before *harness.HealthResponse) bool {
			return now.LeasesObtained > before.LeasesObtained
		}); !ok {
		t.Fatalf("no lease was obtained within %s, so an empty ledger proves nothing here",
			harness.IPAcquisitionBudget+5*time.Second)
	}
	w.End()

	if kinds := ledgerKindsForMAC(readLedger(t, ctx, cli), mac); len(kinds) != 0 {
		t.Errorf("ledger has entries %v for MAC %s of a non-audit network; want none", kinds, mac)
	}
	_ = id
}
