// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestForceCreate_BridgeFromParentUnderDroppingFirewall checks that a bridge made from parent is refused while the
// host drops bridged frames, and created with -o force_create=true once a rule accepts them, with the verdict in the
// plugin log at warning level and a lease from the server behind the parent (#903). The parent is a veth whose peer is a
// port of the bridge fixture, so the fixture's server answers through the new bridge. The kernel's own view of the
// bridge, its mark and its port is the evidence for the create; the server's log is the evidence for the lease.
func TestForceCreate_BridgeFromParentUnderDroppingFirewall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	const (
		parentName = "dh-itest-fcpar"
		peerName   = "dh-itest-fcpeer"
		ownBridge  = "dh-itest-fcbr"
		refusedNet = "dh-itest-fc-refused"
		netName    = "dh-itest-fc-forced"
		ctrName    = "dh-itest-fc-forced-ctr"
		// vlanOwnerAlias in pkg/plugin, the mark of a link this plugin made.
		ownerAlias = "docker-net-dhcp"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// Registered first, so it is put back last and a failed test leaves the host as it found it.
	harness.WithForwardDrop(t)

	la := netlink.NewLinkAttrs()
	la.Name = parentName
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: la, PeerName: peerName}); err != nil {
		t.Fatalf("LinkAdd veth %s/%s: %v", parentName, peerName, err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(ownBridge); err == nil {
			_ = netlink.LinkDel(l)
		}
		if l, err := netlink.LinkByName(parentName); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	// The plugin refuses a NIC with an address or a route; the kernel forms no global address without an
	// advertisement, so the fixture server's router advertisements are switched off for this NIC before it comes up (#903).
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+parentName+"/accept_ra", []byte("0"), 0o644); err != nil {
		t.Fatalf("accept_ra=0 on %s: %v", parentName, err)
	}
	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", peerName, err)
	}
	serverBridge, err := netlink.LinkByName(harness.BridgeName)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", harness.BridgeName, err)
	}
	if err := netlink.LinkSetMaster(peer, serverBridge); err != nil {
		t.Fatalf("enslaving %s into %s: %v", peerName, harness.BridgeName, err)
	}
	for _, name := range []string{peerName, parentName} {
		l, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName(%s): %v", name, err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatalf("LinkSetUp(%s): %v", name, err)
		}
	}

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	opts := func(extra ...string) map[string]string {
		o := map[string]string{"mode": "bridge", "bridge": ownBridge, "parent": parentName}
		for i := 0; i+1 < len(extra); i += 2 {
			o[extra[i]] = extra[i+1]
		}
		return o
	}

	// Without the option the create is refused before anything is made.
	res, createErr := cli.NetworkCreate(ctx, refusedNet, docker.NetworkCreateOptions{
		Driver: harness.DriverName, IPAM: &network.IPAM{Driver: "null"}, Options: opts(),
	})
	if createErr == nil {
		_, _ = cli.NetworkRemove(context.Background(), res.ID, docker.NetworkRemoveOptions{})
		t.Fatalf("the create succeeded under FORWARD DROP without force_create (%s)", func() string {
			_, detail := harness.ForwardDropState()
			return detail
		}())
	}
	if !strings.Contains(createErr.Error(), "force_create=true") {
		t.Fatalf("the refusal does not name force_create=true:\n%v", createErr)
	}
	if _, err := netlink.LinkByName(ownBridge); err == nil {
		t.Fatalf("bridge %s exists after the refused create", ownBridge)
	}
	if l, err := netlink.LinkByName(parentName); err != nil || l.Attrs().MasterIndex != 0 {
		t.Fatalf("parent %s was enslaved by the refused create (err %v)", parentName, err)
	}
	t.Logf("✓ refused as documented: %v", createErr)

	// The documented rule goes in before the option does.
	for _, flag := range []string{"-i", "-o"} {
		args := []string{"-I", "FORWARD", flag, ownBridge, "-j", "ACCEPT"}
		if out, err := exec.Command("iptables", args...).CombinedOutput(); err != nil {
			t.Fatalf("iptables %v: %v (%s)", args, err, out)
		}
		t.Cleanup(func() {
			args[0] = "-D"
			_ = exec.Command("iptables", args...).Run()
		})
	}

	mark := harness.MarkPluginLog(t, ctx)
	harness.CreateNetwork(t, ctx, netName, "bridge", opts("force_create", "true"))
	logged := harness.AwaitPluginLogSince(t, ctx, mark, 10*time.Second, func(w string) bool {
		return strings.Contains(w, "level=warning") && strings.Contains(w, "force_create=true")
	})
	var warned bool
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "level=warning") && strings.Contains(line, "force_create=true") && strings.Contains(line, "bridge="+ownBridge) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning line names force_create=true and bridge=%s in the plugin log written by the create:\n%s", ownBridge, logged)
	}

	br, err := netlink.LinkByName(ownBridge)
	if err != nil {
		t.Fatalf("the forced create made no bridge %s: %v", ownBridge, err)
	}
	if br.Type() != "bridge" || br.Attrs().Alias != ownerAlias {
		t.Errorf("bridge %s is a %s with alias %q, want a bridge marked %q", ownBridge, br.Type(), br.Attrs().Alias, ownerAlias)
	}
	if l, err := netlink.LinkByName(parentName); err != nil || l.Attrs().MasterIndex != br.Attrs().Index {
		t.Errorf("parent %s is not a port of %s (err %v)", parentName, ownBridge, err)
	}

	_, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	harness.AssertBridgeIP(t, ipv4)
	if n := fixture.CountBridgeLogLines("DHCPACK", ipv4, mac); n < 1 {
		t.Errorf("the bridge fixture's dnsmasq logged no DHCPACK of %s to %s through the new bridge", ipv4, mac)
	}
}
