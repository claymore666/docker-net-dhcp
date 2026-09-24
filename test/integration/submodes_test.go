// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// leaseRowFor waits for the server's lease file to hold ip and returns the MAC it was leased to; dnsmasq rewrites the
// file after the ACK, not with it (#905).
func leaseRowFor(t *testing.T, ip string) string {
	t.Helper()
	row, ok, _ := harness.AwaitSettled(5*time.Second, 100*time.Millisecond,
		func() ([]string, error) {
			for _, f := range leaseLines(t) {
				if f[2] == ip {
					return f, nil
				}
			}
			return nil, nil
		},
		func(f []string) bool { return f != nil })
	if !ok {
		t.Fatalf("the server's lease file holds no row for %s:\n%v", ip, leaseLines(t))
	}
	return strings.ToLower(row[1])
}

// createPluginNetworkErr sends a null-IPAM create to Docker and returns its answer; an accepted network is removed at
// cleanup.
func createPluginNetworkErr(t *testing.T, ctx context.Context, cli *docker.Client, name string, opts map[string]string) error {
	t.Helper()
	res, err := cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: harness.DriverName, IPAM: &network.IPAM{Driver: "null"}, Options: opts})
	if err == nil {
		t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), res.ID) })
	}
	return err
}

// TestSubModes_EachLeasesInTheModeItWasCreatedWith checks, per accepted sub-mode, the kernel's mode of the container's
// link read in its namespace and the server's lease row for its address (#905).
func TestSubModes_EachLeasesInTheModeItWasCreatedWith(t *testing.T) {
	cases := []struct {
		name, mode, opt, value, want string
	}{
		{"macvlan with no macvlan_mode builds bridge", "macvlan", "", "", "bridge"},
		{"macvlan_mode=bridge", "macvlan", "macvlan_mode", "bridge", "bridge"},
		{"macvlan_mode=vepa", "macvlan", "macvlan_mode", "vepa", "vepa"},
		{"macvlan_mode=private", "macvlan", "macvlan_mode", "private", "private"},
		{"ipvlan with no ipvlan_mode builds l2", "ipvlan", "", "", "l2"},
		{"ipvlan_mode=l2", "ipvlan", "ipvlan_mode", "l2", "l2"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			ipamDumpOnFailure(t)
			netName := "dh-itest-submode-" + string(rune('a'+i))
			var opts map[string]string
			if c.opt != "" {
				opts = map[string]string{c.opt: c.value}
			}
			harness.CreateNetwork(t, ctx, netName, c.mode, opts)
			id, ipv4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
			harness.AssertIP(t, ipv4)

			kind, mode, mac := harness.ChildLinkMode(t, ctx, id, "eth0")
			if kind != c.mode || mode != c.want {
				t.Errorf("the container's eth0 is %s mode %q in its namespace, want %s mode %q", kind, mode, c.mode, c.want)
			}
			if leased := leaseRowFor(t, ipv4); leased != strings.ToLower(mac) {
				t.Errorf("the server leased %s to %s, not to the container's link MAC %s", ipv4, leased, mac)
			}
		})
	}
}

// TestSubModes_PassthruTakesItsParentAlone checks passthru on a parent of its own: the refusals beside another macvlan
// network both ways, validate_dhcp, the lease under the parent's MAC, the second container's error, the --mac-address
// refusal with the parent's MAC unchanged, and a docker restart (#905).
func TestSubModes_PassthruTakesItsParentAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)

	parent := harness.AddSegmentParent(t, "dh-itest-pt", "dh-itest-ptp")
	parentMAC := harness.LinkMAC(t, parent).String()
	const netName, plainNet = "dh-itest-passthru", "dh-itest-passthru-plain"

	plainID := harness.CreateNetwork(t, ctx, plainNet, "macvlan", map[string]string{"parent": parent})
	err := createPluginNetworkErr(t, ctx, cli, netName,
		map[string]string{"mode": "macvlan", "parent": parent, "macvlan_mode": "passthru"})
	if err == nil || !strings.Contains(err.Error(), plainNet) || !strings.Contains(err.Error(), "takes its parent alone") {
		t.Fatalf("a passthru network beside %s on %s: %v", plainNet, parent, err)
	}
	if err := cli.NetworkRemove(ctx, plainID); err != nil {
		t.Fatalf("NetworkRemove(%s): %v", plainNet, err)
	}

	logOff := fileSize(t, fixture.DnsmasqLog())
	harness.CreateNetwork(t, ctx, netName, "macvlan",
		map[string]string{"parent": parent, "macvlan_mode": "passthru", "validate_dhcp": "true"})
	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the server log: %v", err)
	}
	if !offeredTo(harness.LogSince(logData, logOff), parentMAC) {
		t.Errorf("validate_dhcp on the passthru network: no OFFER to the parent's MAC %s in the server's log", parentMAC)
	}

	err = createPluginNetworkErr(t, ctx, cli, plainNet, map[string]string{"mode": "macvlan", "parent": parent})
	if err == nil || !strings.Contains(err.Error(), netName) || !strings.Contains(err.Error(), "macvlan_mode=passthru") {
		t.Errorf("a bridge-mode network beside the passthru network: %v", err)
	}

	id, ipv4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	harness.AssertIP(t, ipv4)
	kind, mode, mac := harness.ChildLinkMode(t, ctx, id, "eth0")
	if kind != "macvlan" || mode != "passthru" || mac != parentMAC {
		t.Errorf("the container's eth0 is %s mode %q with MAC %s, want macvlan passthru wearing the parent's %s",
			kind, mode, mac, parentMAC)
	}
	assertMACNotSet(t, ctx, id)
	if leased := leaseRowFor(t, ipv4); leased != parentMAC {
		t.Errorf("the server leased %s to %s, not to the parent's MAC %s", ipv4, leased, parentMAC)
	}

	err = ipamRunContainerErr(t, ctx, cli, netName, netName+"-second", nil)
	if err == nil || !strings.Contains(err.Error(), "passthru") || !strings.Contains(err.Error(), "one container") {
		t.Errorf("a second container on the passthru network: %v", err)
	}
	err = ipamRunContainerErr(t, ctx, cli, netName, netName+"-mac",
		&network.EndpointSettings{MacAddress: "02:00:00:09:05:03"})
	if err == nil || !strings.Contains(err.Error(), "does not support a custom MAC address") {
		t.Errorf("--mac-address on the passthru network: %v", err)
	}
	if now := harness.LinkMAC(t, parent).String(); now != parentMAC {
		t.Errorf("the parent's MAC is %s, was %s", now, parentMAC)
	}

	restartOff := fileSize(t, fixture.DnsmasqLog())
	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("docker restart of the passthru container: %v", err)
	}
	after, _ := ipamNetworkAddress(t, ctx, cli, id, netName)
	if _, mode, mac := harness.ChildLinkMode(t, ctx, id, "eth0"); mode != "passthru" || mac != parentMAC {
		t.Errorf("after docker restart the container's eth0 is mode %q with MAC %s", mode, mac)
	}
	assertMACNotSet(t, ctx, id)
	if !ackedSince(t, fixture.DnsmasqLog(), restartOff)[after] {
		t.Errorf("the server ACKed no %s after the docker restart", after)
	}
}

// offeredTo reports whether the server log holds a DHCPOFFER to mac: a passthru probe wears the parent's MAC, where a
// probe built in the default sub-mode would ask with a random one (#905).
func offeredTo(log []byte, mac string) bool {
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "DHCPOFFER(") && strings.Contains(line, mac) {
			return true
		}
	}
	return false
}

// assertMACNotSet fails on addr_assign_type 3, the kernel's mark of a set address, on the passthru link: a MAC set on
// it changes the parent's, measured on Linux 6.12, so neither the plugin nor Docker may set one (#905).
func assertMACNotSet(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	if got := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/eth0/addr_assign_type")); got != "0" && got != "1" && got != "2" {
		t.Errorf("the passthru link's addr_assign_type reads %q, want 0, 1 or 2: a MAC set on it changes the parent's", got)
	}
}

// TestSubModes_DockersOwnIPvlanL3NetworkRefusesAnIPvlanNetworkOnItsParent checks the one-mode-per-parent refusal
// against Docker's own ipvlan driver, and that the parent is accepted once that network is gone (#905).
func TestSubModes_DockersOwnIPvlanL3NetworkRefusesAnIPvlanNetworkOnItsParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli := ipamDockerClient(t)

	const dockerNet, netName = "dh-itest-docker-ipvlan-l3", "dh-itest-ipvlan-beside-l3"
	res, err := cli.NetworkCreate(ctx, dockerNet, network.CreateOptions{Driver: "ipvlan",
		Options: map[string]string{"parent": harness.IpvlanParent, "ipvlan_mode": "l3"}})
	if err != nil {
		t.Fatalf("Docker's ipvlan driver refused an l3 network on %s: %v", harness.IpvlanParent, err)
	}
	removed := false
	t.Cleanup(func() {
		if !removed {
			_ = cli.NetworkRemove(context.Background(), res.ID)
		}
	})

	err = createPluginNetworkErr(t, ctx, cli, netName, map[string]string{"mode": "ipvlan", "parent": harness.IpvlanParent})
	if err == nil || !strings.Contains(err.Error(), dockerNet) || !strings.Contains(err.Error(), "one ipvlan mode per parent") {
		t.Errorf("an ipvlan network beside Docker's l3 network on %s: %v", harness.IpvlanParent, err)
	}
	if err := cli.NetworkRemove(ctx, res.ID); err != nil {
		t.Fatalf("NetworkRemove(%s): %v", dockerNet, err)
	}
	removed = true
	if err := createPluginNetworkErr(t, ctx, cli, netName, map[string]string{"mode": "ipvlan", "parent": harness.IpvlanParent}); err != nil {
		t.Errorf("the ipvlan network was refused with Docker's l3 network gone: %v", err)
	}
}

// TestSubModes_APluginRecycleKeepsTheSubMode checks that a plugin restart and a later docker restart rebuild the child
// in the stored sub-mode and lease again (#905).
func TestSubModes_APluginRecycleKeepsTheSubMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)

	const netName = "dh-itest-submode-recycle"
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"macvlan_mode": "private"})
	id, ipv4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	harness.AssertIP(t, ipv4)
	if _, mode, _ := harness.ChildLinkMode(t, ctx, id, "eth0"); mode != "private" {
		t.Fatalf("the container's eth0 is mode %q before the recycle, want private", mode)
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
	const rebuilt = "recovery to rebuild the private endpoint (recovered_ok >= 1)"
	if waited, ok := harness.AwaitRecoveryRebuildWindow(w, rebuilt,
		func(h *harness.HealthResponse) bool { return h.RecoveredOK >= 1 }); !ok {
		t.Errorf("%s", harness.RecoveryRebuildFailure(rebuilt, waited))
	}

	restartOff := fileSize(t, fixture.DnsmasqLog())
	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("docker restart after the plugin recycle: %v", err)
	}
	after, _ := ipamNetworkAddress(t, ctx, cli, id, netName)
	if _, mode, _ := harness.ChildLinkMode(t, ctx, id, "eth0"); mode != "private" {
		t.Errorf("after the plugin recycle and docker restart the container's eth0 is mode %q, want private", mode)
	}
	if !ackedSince(t, fixture.DnsmasqLog(), restartOff)[after] {
		t.Errorf("the server ACKed no %s after the docker restart", after)
	}
}
