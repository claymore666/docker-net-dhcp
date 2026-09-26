// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// ipLinkDetail returns `ip -d link show name`, and false when the link does not exist.
func ipLinkDetail(t *testing.T, name string) (string, bool) {
	t.Helper()
	cmd := exec.Command("ip", "-d", "link", "show", name)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "does not exist") {
			return string(out), false
		}
		t.Fatalf("ip -d link show %s: %v\n%s", name, err, out)
	}
	return string(out), true
}

// ipRun runs ip with args, failing the test on an error.
func ipRun(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("ip", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

var ipLinkFlagUp = regexp.MustCompile(`<([^>]*,)?UP[,>]`)

// assertPluginVlan checks, in iproute2's own words, that name is an up 802.1Q sub-interface with id and the plugin's
// mark (#902).
func assertPluginVlan(t *testing.T, name, id string) {
	t.Helper()
	out, ok := ipLinkDetail(t, name)
	if !ok {
		t.Fatalf("%s does not exist; the network's children have no link to attach to", name)
	}
	if !strings.Contains(out, "vlan protocol 802.1Q id "+id+" ") {
		t.Errorf("%s is not an 802.1Q sub-interface with id %s:\n%s", name, id, out)
	}
	if !strings.Contains(out, "alias docker-net-dhcp") {
		t.Errorf("%s does not carry the plugin's mark, so a restarted plugin would never remove it:\n%s", name, out)
	}
	if !ipLinkFlagUp.MatchString(out) {
		t.Errorf("%s is not up:\n%s", name, out)
	}
	// With IPv6 on, the host takes a link-local, a SLAAC address and the vlan router's default route (#902).
	if b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6"); err == nil && strings.TrimSpace(string(b)) != "1" {
		t.Errorf("%s has host IPv6 on (disable_ipv6=%s), so the host joins the vlan", name, strings.TrimSpace(string(b)))
	} else if err != nil && !os.IsNotExist(err) {
		t.Errorf("read %s's disable_ipv6: %v", name, err)
	}
}

// taggedRowMAC waits for the tagged server's lease file to hold ip and returns the MAC of that row, since dnsmasq
// rewrites the file after the ACK (#905).
func taggedRowMAC(v *harness.VlanFixture, ip string) string {
	mac, _, _ := harness.AwaitSettled(5*time.Second, 100*time.Millisecond,
		func() (string, error) {
			for _, line := range strings.Split(v.TaggedLeases(), "\n") {
				if f := strings.Fields(line); len(f) >= 3 && f[2] == ip {
					return strings.ToLower(f[1]), nil
				}
			}
			return "", nil
		},
		func(m string) bool { return m != "" })
	return mac
}

// assertTaggedLease checks that the tagged server leased the container's address to the MAC its eth0 carries, that
// the untagged server never saw that MAC, and that eth0 sits on the sub-interface: the kernel reports a macvlan or
// ipvlan child's lower link as its iflink (#902).
func assertTaggedLease(t *testing.T, ctx context.Context, v *harness.VlanFixture, id, ipv4 string) {
	t.Helper()
	if !harness.IsInVlanTaggedPool(net.ParseIP(ipv4)) {
		t.Errorf("the container holds %s, outside the tagged pool %s-%s", ipv4, harness.VlanTaggedPoolStart, harness.VlanTaggedPoolEnd)
	}
	mac := strings.ToLower(strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/eth0/address")))
	if _, err := net.ParseMAC(mac); err != nil {
		t.Fatalf("the container's eth0 address %q is not a MAC: %v", mac, err)
	}
	if got := taggedRowMAC(v, ipv4); got != mac {
		t.Errorf("the tagged server's row for %s names MAC %q, want the container's %s:\n%s", ipv4, got, mac, v.TaggedLeases())
	}
	if strings.Contains(strings.ToLower(v.UntaggedLeases()), mac) {
		t.Errorf("the untagged server leased to %s too, so frames left the parent untagged:\n%s", mac, v.UntaggedLeases())
	}
	sub, err := netlink.LinkByName(harness.VlanSubIf)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", harness.VlanSubIf, err)
	}
	got := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/eth0/iflink"))
	if got != strconv.Itoa(sub.Attrs().Index) {
		t.Errorf("the container's eth0 has iflink %s; %s is ifindex %d", got, harness.VlanSubIf, sub.Attrs().Index)
	}
}

// removeContainer removes id now, ahead of its cleanup, so the network it is on can be deleted.
func removeContainer(t *testing.T, ctx context.Context, cli *docker.Client, id string) {
	t.Helper()
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove(%s): %v", id[:12], err)
	}
}

// removeNetwork deletes a network now, failing the test on an error.
func removeNetwork(t *testing.T, ctx context.Context, cli *docker.Client, id string) {
	t.Helper()
	if err := cli.NetworkRemove(ctx, id); err != nil {
		t.Fatalf("NetworkRemove(%s): %v", id[:12], err)
	}
}

// TestVlan_TheSubInterfaceCarriesTheLeaseSurvivesARestartAndGoesWithItsLastNetwork checks that a vlan network
// creates a marked 802.1Q sub-interface, re-creates it for an endpoint when it went missing, leases through it from
// the tagged server only, shares it with a second network, and that a restarted plugin removes it with the last
// network (#902).
func TestVlan_TheSubInterfaceCarriesTheLeaseSurvivesARestartAndGoesWithItsLastNetwork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)
	v := harness.StartVlan(t)
	opts := map[string]string{"parent": harness.VlanParent, "vlan": harness.VlanID}

	netA := harness.CreateNetwork(t, ctx, "dh-itest-vlan-a", "macvlan", opts)
	assertPluginVlan(t, harness.VlanSubIf, harness.VlanID)

	// A sub-interface removed behind the plugin's back is made again by the next endpoint.
	ipRun(t, "link", "del", harness.VlanSubIf)
	id, ipv4, _ := harness.RunContainer(t, ctx, "dh-itest-vlan-a", "dh-itest-vlan-a-ctr")
	assertPluginVlan(t, harness.VlanSubIf, harness.VlanID)
	assertTaggedLease(t, ctx, v, id, ipv4)

	before, err := netlink.LinkByName(harness.VlanSubIf)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", harness.VlanSubIf, err)
	}
	netB := harness.CreateNetwork(t, ctx, "dh-itest-vlan-b", "macvlan", opts)
	removeContainer(t, ctx, cli, id)
	removeNetwork(t, ctx, cli, netA)
	after, err := netlink.LinkByName(harness.VlanSubIf)
	if err != nil {
		t.Fatalf("deleting one of two networks on %s removed it: %v", harness.VlanSubIf, err)
	}
	if after.Attrs().Index != before.Attrs().Index {
		t.Errorf("%s changed ifindex %d to %d while a second network used it", harness.VlanSubIf,
			before.Attrs().Index, after.Attrs().Index)
	}

	// A restarted plugin knows the link only by its mark.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil &&
			!strings.Contains(err.Error(), "already enabled") {
			t.Logf("WARN: cleanup PluginEnable: %v", err)
		}
	})
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
	if _, ok := ipLinkDetail(t, harness.VlanSubIf); !ok {
		t.Fatalf("the plugin restart removed %s while network B still uses it", harness.VlanSubIf)
	}
	removeNetwork(t, ctx, cli, netB)
	if out, ok := ipLinkDetail(t, harness.VlanSubIf); ok {
		t.Errorf("%s outlived the last network on it:\n%s", harness.VlanSubIf, out)
	}
}

// TestVlan_ALinkThePluginDidNotMakeOrThatIsInUseIsLeftAlone checks an ipvlan network's lease and cleanup, then the
// cases where the plugin must not touch the link: one made by hand, a link of another type under the name, and one
// with a child in a namespace no host link list shows (#902).
func TestVlan_ALinkThePluginDidNotMakeOrThatIsInUseIsLeftAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)
	v := harness.StartVlan(t)
	opts := func(id string) map[string]string {
		return map[string]string{"parent": harness.VlanParent, "vlan": id}
	}

	ipvNet := harness.CreateNetwork(t, ctx, "dh-itest-vlan-ipv", "ipvlan", opts(harness.VlanID))
	id, ipv4, _ := harness.RunContainer(t, ctx, "dh-itest-vlan-ipv", "dh-itest-vlan-ipv-ctr")
	assertTaggedLease(t, ctx, v, id, ipv4)
	removeContainer(t, ctx, cli, id)
	removeNetwork(t, ctx, cli, ipvNet)
	if out, ok := ipLinkDetail(t, harness.VlanSubIf); ok {
		t.Errorf("%s outlived its only (ipvlan) network:\n%s", harness.VlanSubIf, out)
	}

	const handMade = harness.VlanParent + ".200"
	ipRun(t, "link", "add", "link", harness.VlanParent, "name", handMade, "type", "vlan", "id", "200")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", handMade).Run() })
	// A link the plugin did not make is used as it is, so a down one is refused like any down parent (#902).
	err := createPluginNetworkErr(t, ctx, cli, "dh-itest-vlan-hand",
		map[string]string{"mode": "macvlan", "parent": harness.VlanParent, "vlan": "200"})
	if err == nil || !strings.Contains(err.Error(), "parent interface is down: "+handMade) {
		t.Errorf("a create over the down %s was not refused naming it: %v", handMade, err)
	}
	if out, ok := ipLinkDetail(t, handMade); !ok || ipLinkFlagUp.MatchString(out) || strings.Contains(out, "alias docker-net-dhcp") {
		t.Errorf("the refused create changed %s, which the plugin did not create:\n%s", handMade, out)
	}
	ipRun(t, "link", "set", handMade, "up")
	handNet := harness.CreateNetwork(t, ctx, "dh-itest-vlan-hand", "macvlan", opts("200"))
	removeNetwork(t, ctx, cli, handNet)
	if out, ok := ipLinkDetail(t, handMade); !ok {
		t.Errorf("deleting the network removed %s, which the plugin did not create:\n%s", handMade, out)
	} else if strings.Contains(out, "alias docker-net-dhcp") {
		t.Errorf("the plugin marked %s, which it did not create:\n%s", handMade, out)
	}

	const foreign = harness.VlanParent + ".300"
	ipRun(t, "link", "add", foreign, "type", "dummy")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", foreign).Run() })
	err = createPluginNetworkErr(t, ctx, cli, "dh-itest-vlan-dummy",
		map[string]string{"mode": "macvlan", "parent": harness.VlanParent, "vlan": "300"})
	if err == nil || !strings.Contains(err.Error(), foreign+" exists and is a dummy link, not a vlan") {
		t.Errorf("a create over a dummy named %s was not refused with the reason: %v", foreign, err)
	}
	if ids := networksNamed(t, ctx, cli, "dh-itest-vlan-dummy"); len(ids) != 0 {
		t.Errorf("the refused create left network dh-itest-vlan-dummy behind: %v", ids)
	}
	if _, ok := ipLinkDetail(t, foreign); !ok {
		t.Errorf("the refused create removed the dummy %s", foreign)
	}

	// A child moved to another namespace vanishes from the host's link list and would die with the sub-interface.
	const hiddenNS, hidden = "dh-itest-vlh", "dh-itest-vlhc"
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", hiddenNS).Run() })
	hidNet := harness.CreateNetwork(t, ctx, "dh-itest-vlan-hid", "macvlan", opts(harness.VlanID))
	ipRun(t, "netns", "add", hiddenNS)
	ipRun(t, "link", "add", "link", harness.VlanSubIf, "name", hidden, "type", "macvlan", "mode", "bridge")
	ipRun(t, "link", "set", hidden, "netns", hiddenNS)
	removeNetwork(t, ctx, cli, hidNet)
	if _, ok := ipLinkDetail(t, harness.VlanSubIf); !ok {
		t.Errorf("deleting the network removed %s and with it the macvlan child in namespace %s", harness.VlanSubIf, hiddenNS)
	}
	if out, err := exec.Command("ip", "-n", hiddenNS, "link", "show", hidden).CombinedOutput(); err != nil {
		t.Errorf("the child %s in namespace %s is gone: %v\n%s", hidden, hiddenNS, err, out)
	}
}

// TestVlan_IPAMModeBindsThePoolToTheSubInterface checks that an IPAM network names `<parent>.<id>` in --ipam-opt
// parent= and leases tagged, and that naming the bare parent is refused as the mismatch it is (#902).
func TestVlan_IPAMModeBindsThePoolToTheSubInterface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	v := harness.StartVlan(t)
	opts := map[string]string{"parent": harness.VlanParent, "vlan": harness.VlanID}

	harness.CreateNetworkIPAM(t, ctx, "dh-itest-vlan-ipam", "macvlan", harness.VlanTaggedCIDR,
		map[string]string{"parent": harness.VlanSubIf}, opts)
	assertPluginVlan(t, harness.VlanSubIf, harness.VlanID)
	// The reservation and the endpoint each make a vanished sub-interface again, as a reboot leaves it (#902).
	ipRun(t, "link", "del", harness.VlanSubIf)
	id, ipv4, _ := harness.RunContainer(t, ctx, "dh-itest-vlan-ipam", "dh-itest-vlan-ipam-ctr")
	assertPluginVlan(t, harness.VlanSubIf, harness.VlanID)
	assertTaggedLease(t, ctx, v, id, ipv4)

	err := harness.CreateNetworkIPAMErr(ctx, "dh-itest-vlan-ipam-bare", "macvlan", "192.168.113.0/24",
		map[string]string{"parent": harness.VlanParent}, opts)
	if err == nil {
		t.Fatal("an IPAM pool named for the bare parent was accepted for a network on its sub-interface")
	}
	for _, w := range []string{`built for interface "` + harness.VlanParent + `"`, `being created on "` + harness.VlanSubIf + `"`} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal does not carry %q:\n%v", w, err)
		}
	}
}
