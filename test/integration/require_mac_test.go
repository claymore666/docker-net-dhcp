// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// requireMACRefusal is every fragment of the refusal an operator needs to act on it (#1036).
var requireMACRefusal = []string{"require_mac", "--mac-address", "mac_address", "docker network connect"}

// hostLinkNames is the kernel's link list in the host namespace.
func hostLinkNames(t *testing.T) map[string]bool {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list host links: %v", err)
	}
	out := map[string]bool{}
	for _, i := range ifs {
		out[i.Name] = true
	}
	return out
}

// liveRecords reads the plugin's lease journal from disk and returns the records on a network that are not closed.
func liveRecords(t *testing.T, networkID string) []lease.Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(harness.HostStateDir, "lease-records.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the lease journal: %v", err)
	}
	var evs []lease.RecordEvent
	for _, line := range bytes.Split(data, []byte("\n")) {
		var ev lease.RecordEvent
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &ev) != nil {
			continue
		}
		evs = append(evs, ev)
	}
	var live []lease.Record
	for _, rec := range lease.Rebuild(evs).Records {
		if rec.Scope == networkID && rec.Phase != lease.PhaseClosed && rec.Phase != lease.PhaseUnset {
			live = append(live, rec)
		}
	}
	return live
}

// ackedSince is every address dnsmasq ACKed in its log after offset.
func ackedSince(t *testing.T, logPath string, offset int64) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read the server log %s: %v", logPath, err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(harness.LogSince(data, offset)), "\n") {
		i := strings.Index(line, "DHCPACK(")
		if i < 0 {
			continue
		}
		rest := line[i:]
		if j := strings.Index(rest, ") "); j >= 0 {
			if f := strings.Fields(rest[j+2:]); len(f) > 0 {
				out[f[0]] = true
			}
		}
	}
	return out
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}

// leaseLines is dnsmasq's lease file as "expiry MAC IP hostname client-id" rows.
func leaseLines(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(fixture.LeaseFile())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the server lease file: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 5 {
			out = append(out, f)
		}
	}
	return out
}

// TestRequireMAC_WithoutAMACIsRefusedAndLeavesNothing checks, per mode and address shape, that a container with no MAC of its own is refused leaving no link, record or endpoint, and that one with --mac-address then leases under that MAC (#1036).
func TestRequireMAC_WithoutAMACIsRefusedAndLeavesNothing(t *testing.T) {
	cases := []struct {
		name, mode string
		ipam       bool
		mac        string
	}{
		{"bridge, --ipam-driver null", "bridge", false, "02:00:00:10:36:01"},
		{"bridge, IPAM mode", "bridge", true, "02:00:00:10:36:02"},
		{"macvlan, --ipam-driver null", "macvlan", false, "02:00:00:10:36:03"},
		{"macvlan, IPAM mode", "macvlan", true, "02:00:00:10:36:04"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(cases))*90*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			netName := "dh-itest-reqmac-" + string(rune('a'+i))
			opts := map[string]string{"require_mac": "true"}
			var netID string
			switch {
			case c.ipam && c.mode == "bridge":
				netID = harness.CreateNetworkIPAM(t, ctx, netName, c.mode, "", nil, opts)
			case c.ipam:
				netID = harness.CreateNetworkIPAM(t, ctx, netName, c.mode, harness.SubnetCIDR, nil, opts)
			default:
				netID = harness.CreateNetwork(t, ctx, netName, c.mode, opts)
			}
			serverLog := fixture.DnsmasqLog()
			if c.mode == "bridge" {
				serverLog = fixture.BridgeDnsmasqLogPath()
			}

			linksBefore := hostLinkNames(t)
			leasesBefore := leaseLines(t)
			logMark := fileSize(t, serverLog)

			err := ipamRunContainerErr(t, ctx, cli, netName, netName+"-nomac", nil)
			if err == nil {
				t.Fatal("a container with no MAC of its own started on a require_mac=true network")
			}
			for _, w := range requireMACRefusal {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal Docker relayed does not carry %q:\n%v", w, err)
				}
			}
			for name := range hostLinkNames(t) {
				if !linksBefore[name] {
					t.Errorf("the refused start left host link %s behind", name)
				}
			}
			if live := liveRecords(t, netID); len(live) != 0 {
				t.Errorf("the refused start left %d live lease records on the network: %+v", len(live), live)
			}
			if eps := ipamNetworkInspect(t, ctx, cli, netName).Containers; len(eps) != 0 {
				t.Errorf("docker network inspect lists endpoints after the refusal: %v", eps)
			}
			refusedACKs := ackedSince(t, serverLog, logMark)
			if !c.ipam && len(refusedACKs) != 0 {
				t.Errorf("the server ACKed %v for a start the null shape refuses before any exchange", refusedACKs)
			}
			var refusedIDs []string
			if c.mode == "macvlan" {
				seen := map[string]bool{}
				for _, l := range leasesBefore {
					seen[strings.Join(l, " ")] = true
				}
				for _, l := range leaseLines(t) {
					if !seen[strings.Join(l, " ")] {
						refusedIDs = append(refusedIDs, l[4])
					}
				}
			}

			// Started seconds after the refusal, so a record the refusal left would be this start's re-bind candidate.
			if err := ipamRunContainerErr(t, ctx, cli, netName, netName+"-mac",
				&network.EndpointSettings{MacAddress: c.mac}); err != nil {
				t.Fatalf("a container with --mac-address %s was refused: %v", c.mac, err)
			}
			addr, mac := ipamNetworkAddress(t, ctx, cli, netName+"-mac", netName)
			if mac != c.mac {
				t.Errorf("the container carries %s, not its --mac-address %s", mac, c.mac)
			}
			logData, err := os.ReadFile(serverLog)
			if err != nil {
				t.Fatalf("read the server log: %v", err)
			}
			if ok, acks := harness.ACKedTo(logData, addr, c.mac); !ok {
				t.Errorf("the server never ACKed %s to %s; ACKs for that address: %v", addr, c.mac, acks)
			}
			if refusedACKs[addr] {
				t.Errorf("the container took %s, the address leased for the refused start", addr)
			}
			if c.mode == "macvlan" {
				for _, l := range leaseLines(t) {
					if !strings.EqualFold(l[1], c.mac) {
						continue
					}
					for _, id := range refusedIDs {
						if id != "*" && strings.EqualFold(l[4], id) {
							t.Errorf("the server filed %s under %s, the refused start's client id", c.mac, id)
						}
					}
				}
			}
		})
	}
}

// TestRequireMAC_NetworkConnectIsRefused checks that the docker network connect command, which cannot set a MAC, is refused onto a require_mac=true network and leaves no endpoint (#1036).
func TestRequireMAC_NetworkConnectIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)

	const firstNet, strictNet = "dh-itest-reqmac-conn-a", "dh-itest-reqmac-conn-b"
	const ctrName = "dh-itest-reqmac-conn-ctr"
	harness.CreateNetwork(t, ctx, firstNet, "bridge", nil)
	harness.CreateNetwork(t, ctx, strictNet, "macvlan", map[string]string{"require_mac": "true"})
	id, _, _ := harness.RunContainer(t, ctx, firstNet, ctrName)
	linksBefore := harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show")

	out, err := exec.CommandContext(ctx, "docker", "network", "connect", strictNet, id).CombinedOutput()
	if err == nil {
		t.Fatalf("docker network connect onto a require_mac=true network succeeded:\n%s", out)
	}
	for _, w := range requireMACRefusal {
		if !strings.Contains(string(out), w) {
			t.Errorf("the refusal the CLI printed does not carry %q:\n%s", w, out)
		}
	}
	if eps := ipamNetworkInspect(t, ctx, cli, strictNet).Containers; len(eps) != 0 {
		t.Errorf("the refused connect left an endpoint on %s: %v", strictNet, eps)
	}
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	// A failed connect leaves the engine's empty entry in the container view on 26.1.5 and 29.8.1 (measured 2026-09-24, #1036).
	if es, ok := ins.NetworkSettings.Networks[strictNet]; ok &&
		(es.EndpointID != "" || es.IPAddress != "" || es.GlobalIPv6Address != "" || es.MacAddress != "") {
		t.Errorf("the container holds an endpoint on %s after the refused connect: %+v", strictNet, es)
	}
	if linksAfter := harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show"); linksAfter != linksBefore {
		t.Errorf("the refused connect changed the container's interfaces:\nbefore:\n%s\nafter:\n%s", linksBefore, linksAfter)
	}
	if _, ok := ins.NetworkSettings.Networks[firstNet]; !ok || !ins.State.Running {
		t.Errorf("the refused connect disturbed the container: running=%v networks=%v",
			ins.State.Running, ins.NetworkSettings.Networks)
	}

	// With the leftover entry the restart is refused and stops the container, and only a stopped container can be disconnected (#1036).
	if _, ok := ins.NetworkSettings.Networks[strictNet]; ok {
		err := cli.ContainerRestart(ctx, id, container.StopOptions{})
		if err == nil || !strings.Contains(err.Error(), "require_mac is set on this network") {
			t.Fatalf("docker restart with the leftover entry: want the require_mac refusal, got %v", err)
		}
		if st, ierr := cli.ContainerInspect(ctx, id); ierr != nil || st.State.Running {
			t.Fatalf("the refused restart left the container running or unreadable: %v", ierr)
		}
		if out, err := exec.CommandContext(ctx, "docker", "network", "disconnect", strictNet, id).CombinedOutput(); err != nil {
			t.Fatalf("docker network disconnect of the stopped container: %v\n%s", err, out)
		}
		if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			t.Fatalf("docker start after the disconnect: %v", err)
		}
	} else if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("docker restart after the refused connect: %v", err)
	}
	if ins, err = cli.ContainerInspect(ctx, id); err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if _, ok := ins.NetworkSettings.Networks[strictNet]; ok || !ins.State.Running || ins.NetworkSettings.Networks[firstNet] == nil {
		t.Errorf("after the restart: running=%v networks=%v", ins.State.Running, ins.NetworkSettings.Networks)
	}
}

// TestRequireMAC_APluginRecycleKeepsAnAcceptedContainer checks that a container accepted at creation keeps its address across a plugin restart and a later docker restart (#1036).
func TestRequireMAC_APluginRecycleKeepsAnAcceptedContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)

	const netName, ctrName = "dh-itest-reqmac-recycle", "dh-itest-reqmac-recycle-ctr"
	const pinned = "02:00:00:10:36:05"
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"require_mac": "true"})
	if err := ipamRunContainerErr(t, ctx, cli, netName, ctrName,
		&network.EndpointSettings{MacAddress: pinned}); err != nil {
		t.Fatalf("a container with --mac-address was refused: %v", err)
	}
	before, mac := ipamNetworkAddress(t, ctx, cli, ctrName, netName)
	if mac != pinned {
		t.Fatalf("the container carries %s, not its --mac-address %s", mac, pinned)
	}

	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil &&
			!strings.Contains(err.Error(), "already enabled") {
			t.Logf("WARN: cleanup PluginEnable: %v", err)
		}
	})
	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok", "recovery_failed").ExpectRecycle()
	// As in TestRecovery_PluginDisableEnable_PreservesEndpoint: the recovered Join client probes asynchronously (#725).
	harness.AllowUnprobedLeases(1)

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)
	const rebuilt = "recovery to rebuild the accepted endpoint (recovered_ok >= 1)"
	if waited, ok := harness.AwaitRecoveryRebuildWindow(w, rebuilt,
		func(h *harness.HealthResponse) bool { return h.RecoveredOK >= 1 }); !ok {
		t.Errorf("%s", harness.RecoveryRebuildFailure(rebuilt, waited))
	}
	if _, after := w.End(); after.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d: the accepted endpoint was not rebuilt", after.RecoveryFailed)
	}

	if err := cli.ContainerRestart(ctx, ctrName, container.StopOptions{}); err != nil {
		t.Fatalf("docker restart of the accepted container failed: %v", err)
	}
	after, mac := ipamNetworkAddress(t, ctx, cli, ctrName, netName)
	if after != before || mac != pinned {
		t.Errorf("the accepted container was on %s/%s and is on %s/%s after the recycle and restart",
			before, pinned, after, mac)
	}
}
