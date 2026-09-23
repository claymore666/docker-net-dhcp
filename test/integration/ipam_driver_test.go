// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The bundled DHCP IPAM driver against a real daemon (#110): addresses come from Docker's store and are matched to the
// server's DHCPACK, refusals are the daemon's own text, and "nothing leased" is a count of client frames on the wire.

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// ipamDumpOnFailure dumps the fixture's and the plugin's logs when the test fails.
func ipamDumpOnFailure(t *testing.T) {
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

func ipamDockerClient(t *testing.T) *docker.Client {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// It reads the endpoint's IPAddress in the daemon's store, the field RunContainer polls, because that is what an IPAM
// driver makes true (#110).

// ipamNetworkAddress reads the address and MAC Docker publishes for one container on one network.
func ipamNetworkAddress(t *testing.T, ctx context.Context, cli *docker.Client, containerID, netName string) (addr, mac string) {
	t.Helper()
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, containerID)
		if err != nil {
			t.Fatalf("ContainerInspect(%s): %v", containerID, err)
		}
		if ep, ok := ins.NetworkSettings.Networks[netName]; ok && ep.IPAddress != "" {
			return ep.IPAddress, ep.MacAddress
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s published no address on %s within %v", containerID, netName, harness.IPAcquisitionBudget)
	return "", ""
}

// ipamRunContainerErr starts a container and returns the daemon's error, for the cases that must fail.
func ipamRunContainerErr(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName string, ep *network.EndpointSettings) error {
	t.Helper()
	if ep == nil {
		ep = &network.EndpointSettings{}
	}
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: ep}},
		nil, ctrName)
	if err != nil {
		// The daemon refuses at create time an address no pool on the network contains.
		return err
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, create.ID, container.StopOptions{})
		_ = cli.ContainerRemove(bg, create.ID, container.RemoveOptions{Force: true})
	})
	return cli.ContainerStart(ctx, create.ID, container.StartOptions{})
}

// ipamNetworkInspect returns `docker network inspect` for name.
func ipamNetworkInspect(t *testing.T, ctx context.Context, cli *docker.Client, name string) network.Inspect {
	t.Helper()
	insp, err := cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		t.Fatalf("NetworkInspect(%s): %v", name, err)
	}
	return insp
}

// Null mode also publishes an address at CreateEndpoint, so the evidence is the network's record, the driver's pool
// and the server's DHCPACK for this MAC (#110).

// TestIPAM_AddressIsTheServersACK checks that the address Docker publishes is the one the DHCP server granted.
func TestIPAM_AddressIsTheServersACK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-ack"
	const ctrName = "dh-itest-ipam-ack-ctr"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)

	id, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ipv4, mac)
	harness.AssertIP(t, ipv4)

	// Outside evidence: the server's own log line.
	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the fixture's log: %v", err)
	}
	acked, acks := harness.ACKedTo(logData, ipv4, mac)
	if !acked {
		t.Errorf("the server never ACKed %s to %s, so the address Docker published was not "+
			"leased to this container.\nACKs seen for that address: %v", ipv4, mac, acks)
	}

	insp := ipamNetworkInspect(t, ctx, cli, netName)
	if !strings.Contains(insp.IPAM.Driver, "docker-net-dhcp") {
		t.Errorf("the network's IPAM driver is %q, want this plugin", insp.IPAM.Driver)
	}
	found := false
	for _, c := range insp.Containers {
		if strings.HasPrefix(c.IPv4Address, ipv4+"/") {
			found = true
		}
	}
	if !found {
		t.Errorf("docker network inspect lists no container at %s; its Containers map is %v",
			ipv4, insp.Containers)
	}
	// In null mode the IPAM block does not exist; `--ip`, compose's ipv4_address and inspect tools read it (#110).
	if len(insp.IPAM.Config) != 1 || insp.IPAM.Config[0].Subnet != harness.SubnetCIDR {
		t.Errorf("the IPAM block is %v, want the one --subnet that was typed (%s)",
			insp.IPAM.Config, harness.SubnetCIDR)
	}

	// RequestAddress reports the address and a separate call configures the link, so the two can disagree.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, ipv4) {
		t.Errorf("eth0 inside the container does not carry the address Docker published (%s)\n%s", ipv4, out)
	}
}

// libnetwork's remote allocator asks again for any pool overlapping an on-link host route, so answering the parent's
// prefix would make `docker network create` never return. The exchange runs on a temporary veth whose MAC the
// container's veth reuses seconds later, so a stale bridge FDB entry would misdirect the server's unicast reply (#110).

// TestIPAM_NoSubnetAnswersTheAnyPool checks that a network with no --subnet gets 0.0.0.0/0 and the bridge learns the container's MAC on its own port.
func TestIPAM_NoSubnetAnswersTheAnyPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})

	const netName = "dh-itest-ipam-anypool"
	const ctrName = "dh-itest-ipam-anypool-ctr"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "bridge", "", nil, nil)

	insp := ipamNetworkInspect(t, ctx, cli, netName)
	if len(insp.IPAM.Config) != 1 || insp.IPAM.Config[0].Subnet != "0.0.0.0/0" {
		t.Fatalf("the IPAM block for a network with no --subnet is %v, want the single pool "+
			"0.0.0.0/0. Any other answer is one libnetwork's allocator would reject and ask "+
			"again for, which is a `docker network create` that never returns.", insp.IPAM.Config)
	}

	_, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	harness.AssertBridgeIP(t, ipv4)
	if n := fixture.CountBridgeLogLines("DHCPACK", mac); n == 0 {
		t.Errorf("the bridge fixture's server logged no DHCPACK to %s, so %s was not leased to it",
			mac, ipv4)
	}

	fdb, err := exec.Command("bridge", "fdb", "show", "br", harness.BridgeName).CombinedOutput()
	if err != nil {
		t.Fatalf("bridge fdb show br %s: %v\n%s", harness.BridgeName, err, fdb)
	}
	if !strings.Contains(strings.ToLower(string(fdb)), strings.ToLower(mac)) {
		t.Errorf("%s does not appear in the fixture bridge's forwarding table after Join:\n%s\n"+
			"The exchange ran on a temporary veth carrying this MAC, which was then deleted. "+
			"A bridge that did not re-learn it on the container's port would black-hole every "+
			"unicast reply, starting with the first renewal.", mac, fdb)
	}
}

// Docker mints a fresh MAC at every start, so the address survives only through the retained record's client
// identity; the JSON tombstone store must stay empty for this network, or two mechanisms could promise one address
// (#110).

// TestIPAM_SingleRestartKeepsTheAddress checks that a single restarted container keeps its address and lays no JSON tombstone.
func TestIPAM_SingleRestartKeepsTheAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-restart"
	const ctrName = "dh-itest-ipam-restart-ctr"

	cli := ipamDockerClient(t)
	netID := harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)
	id, before, beforeMAC := harness.RunContainer(t, ctx, netName, ctrName)

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	after, afterMAC := ipamNetworkAddress(t, ctx, cli, id, netName)

	if after != before {
		t.Errorf("the container came back on %s; it held %s.\n"+
			"In IPAM mode the address is Docker's published value, so losing it across a "+
			"restart changes what every other container on this network resolves it to.",
			after, before)
	}
	if beforeMAC == afterMAC {
		t.Logf("NOTE: the MAC did not change across the restart (%s), so this run did not "+
			"exercise the identity carry-over the assertion above is about. The scenario "+
			"that exercises it whatever the engine does is "+
			"TestIPAMStranded_APluginThatEndsMidRestartGivesTheAddressBack, where the "+
			"container is replaced and its hardware address cannot be the old one",
			beforeMAC)
	}
	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the fixture's log: %v", err)
	}
	if acked, acks := harness.ACKedTo(logData, after, afterMAC); !acked {
		t.Errorf("the server never ACKed %s to the restarted container's MAC %s; the address "+
			"in Docker's store is not the one that was leased.\nACKs for it: %v", after, afterMAC, acks)
	}
	// The address is claimed twice across a restart, by the reservation and by the container's client, so the last ACK
	// for this MAC must be the published address; an earlier ACK is satisfied even when the second claim was NAKed (#1047).
	if last := harness.LastACKedAddress(logData, afterMAC); last != after {
		t.Errorf("the server last acknowledged %q for %s, and Docker publishes %s.\n"+
			"Every other container on this network resolves the published address, so a "+
			"container running on a different one is unreachable at the name it is "+
			"published under.", last, afterMAC, after)
	}

	// Row 10: the JSON store must carry nothing for this network.
	data, err := os.ReadFile(harness.HostStateDir + "/tombstones.json")
	switch {
	case os.IsNotExist(err):
		// Nothing on this host has ever laid one. Also a pass.
	case err != nil:
		t.Fatalf("read the JSON tombstone store: %v", err)
	case strings.Contains(string(data), netID):
		t.Errorf("the JSON tombstone store carries an entry for the IPAM-mode network %s.\n"+
			"In IPAM mode the re-bind candidate is the record store's retained record and "+
			"nothing else; a second mechanism holding the same address is one address "+
			"promised to two endpoints.\n%s", netID[:12], data)
	}
}

// A RequestAddress carries no hostname or endpoint id, so with two retained records the driver counts the ambiguity
// in ipam_rebind_ambiguous and the server picks the address (#110).

// TestIPAM_RestartedTogetherIsTheDocumentedLimit checks that containers restarted together all get an address and the ambiguity is counted.
func TestIPAM_RestartedTogetherIsTheDocumentedLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-together"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)
	idA, addrA, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-together-a")
	idB, addrB, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-together-b")
	t.Logf("before: a=%s b=%s", addrA, addrB)

	w := harness.BeginCounterWindow(t, ctx, cli, "ipam_rebind_ambiguous", "parent_link_wait_timeouts")

	// Both down, so two retained records are live at once, then both up.
	for _, id := range []string{idA, idB} {
		if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
			t.Fatalf("ContainerStop: %v", err)
		}
	}
	for _, id := range []string{idA, idB} {
		if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			t.Fatalf("ContainerStart: %v", err)
		}
	}

	// The guarantee that DOES hold: both come back with an address.
	for name, id := range map[string]string{"a": idA, "b": idB} {
		addr, _ := ipamNetworkAddress(t, ctx, cli, id, netName)
		if !harness.IsInPool(harness.AssertIP(t, addr)) {
			t.Errorf("container %s came back on %s, outside the fixture's pool", name, addr)
		}
		t.Logf("after: %s=%s", name, addr)
	}

	before, after := w.End()
	if after.IPAMRebindAmbiguous <= before.IPAMRebindAmbiguous {
		t.Errorf("ipam_rebind_ambiguous did not move (%d -> %d).\n"+
			"Two containers restarted together is exactly the case this counter documents: "+
			"two retained records, two address requests, and nothing in the request to tell "+
			"them apart. A limit nothing counts is a limit nobody can see.",
			before.IPAMRebindAmbiguous, after.IPAMRebindAmbiguous)
	}

	// A reservation holds the parent NIC for its whole exchange, so a second simultaneous macvlan start queues and then
	// succeeds; parent_link_wait_timeouts is a health warning and must not move for that (#110).
	if after.ParentLinkWaitTimeouts != before.ParentLinkWaitTimeouts {
		t.Errorf("parent_link_wait_timeouts moved (%d -> %d) on two containers starting "+
			"together on one macvlan network.\n"+
			"Nothing here is the cross-kind pair the kernel refuses: both endpoints attach "+
			"macvlan children to the same parent, which the kernel permits side by side, and "+
			"both containers came back with an address above. Same-kind contention belongs in "+
			"parent_link_waits.",
			before.ParentLinkWaitTimeouts, after.ParentLinkWaitTimeouts)
	}
}

// Any other endpoint removed inside the retention window leaves a second retained record; docs/reference.md names the
// window (#110).

// TestIPAM_ARemovedNeighbourMakesTheRebindAmbiguous checks that a neighbour removed inside the retention window makes a single restart's re-bind ambiguous.
func TestIPAM_ARemovedNeighbourMakesTheRebindAmbiguous(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-neighbour"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)
	idA, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-neighbour-a")
	idB, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-neighbour-b")

	w := harness.BeginCounterWindow(t, ctx, cli, "ipam_rebind_ambiguous")

	if err := cli.ContainerStop(ctx, idA, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop(a): %v", err)
	}
	if err := cli.ContainerRemove(ctx, idB, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove(b): %v", err)
	}
	if err := cli.ContainerStart(ctx, idA, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(a): %v", err)
	}
	addr, _ := ipamNetworkAddress(t, ctx, cli, idA, netName)
	if !harness.IsInPool(harness.AssertIP(t, addr)) {
		t.Errorf("the restarted container came back on %s, outside the fixture's pool", addr)
	}

	before, after := w.End()
	if after.IPAMRebindAmbiguous <= before.IPAMRebindAmbiguous {
		t.Errorf("ipam_rebind_ambiguous did not move (%d -> %d) with one container restarting "+
			"beside a neighbour removed seconds earlier.\n"+
			"That is the case the documented window is about, and a user who reads "+
			"\"restarted on its own keeps its address\" and hits this case has no signal at all "+
			"unless this counter moves.", before.IPAMRebindAmbiguous, after.IPAMRebindAmbiguous)
	}
}

// The fixture pins StaticTestIP to StaticTestMAC with a --dhcp-host reservation, so the server will grant the pair.

// TestIPAM_StaticIPIsHonouredWithASubnet checks that `--ip` on a network with --subnet reaches the driver and is the address published.
func TestIPAM_StaticIPIsHonouredWithASubnet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-static"
	const ctrName = "dh-itest-ipam-static-ctr"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)

	// dnsmasq takes a --dhcp-host address out of the dynamic range, and libnetwork adopts whatever the driver returns, so
	// a container the reservation is not for gets another address unless ipamACKIsTheOneAsked refuses it. This runs before
	// the pinned container, whose live record would refuse the request before any exchange (#110).
	t.Run("an --ip the server will not grant is refused, never substituted", func(t *testing.T) {
		const otherName = "dh-itest-ipam-static-other"
		other, err := cli.ContainerCreate(ctx,
			&container.Config{
				Image:    harness.TestImage,
				Cmd:      []string{"sleep", "infinity"},
				Hostname: otherName,
			},
			harness.HostConfig(),
			&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: harness.StaticTestIP}},
			}},
			nil, otherName)
		if err != nil {
			t.Fatalf("ContainerCreate: %v", err)
		}
		t.Cleanup(func() {
			bg := context.Background()
			_ = cli.ContainerStop(bg, other.ID, container.StopOptions{})
			_ = cli.ContainerRemove(bg, other.ID, container.RemoveOptions{Force: true})
		})
		if err := cli.ContainerStart(ctx, other.ID, container.StartOptions{}); err != nil {
			t.Logf("refused, which is the expected branch: %v", err)
			return
		}
		got, _ := ipamNetworkAddress(t, ctx, cli, other.ID, netName)
		if got != harness.StaticTestIP {
			t.Fatalf("this container asked for %s and Docker published %s. The address the "+
				"operator pinned is not the one the container has, `docker run` exited 0, "+
				"and nothing anywhere reports the substitution.", harness.StaticTestIP, got)
		}
		t.Logf("the server granted %s to this container as well; the demand was met", got)
	})

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {
				IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: harness.StaticTestIP},
				MacAddress: harness.StaticTestMAC,
			},
		}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate with --ip %s: %v", harness.StaticTestIP, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, create.ID, container.StopOptions{})
		_ = cli.ContainerRemove(bg, create.ID, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart with --ip %s: %v", harness.StaticTestIP, err)
	}

	addr, mac := ipamNetworkAddress(t, ctx, cli, create.ID, netName)
	if addr != harness.StaticTestIP {
		t.Errorf("asked for %s, Docker published %s", harness.StaticTestIP, addr)
	}
	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the fixture's log: %v", err)
	}
	if acked, acks := harness.ACKedTo(logData, harness.StaticTestIP, mac); !acked {
		t.Errorf("the server never ACKed %s to %s. `--ip` that Docker reports and the server "+
			"never granted is the worst of both shapes: the store says one thing and the "+
			"segment another.\nACKs for it: %v", harness.StaticTestIP, mac, acks)
	}

}

// The daemon checks only that some subnet on the network contains the address, and 0.0.0.0/0 contains all: on engine
// 29.8.0, run 34600486961, a container with `--ip 192.168.99.71` and no --subnet got .71 from the fixture's DHCPACK, so
// ipamACKIsTheOneAsked is all that stops a substitution there (#110).

// TestIPAM_StaticIPWithoutASubnetIsStillTheAddressAsked checks that `--ip` on a network without --subnet is granted or refused, never substituted.
func TestIPAM_StaticIPWithoutASubnetIsStillTheAddressAsked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-nosubnet-ip"
	const ctrName = "dh-itest-ipam-nosubnet-ip-ctr"
	const wantIP = "192.168.99.71"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "", nil, nil)

	err := ipamRunContainerErr(t, ctx, cli, netName, ctrName,
		&network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: wantIP}})
	if err != nil {
		t.Logf("refused rather than substituted, which is the other legal branch: %v", err)
		return
	}
	got, _ := ipamNetworkAddress(t, ctx, cli, ctrName, netName)
	if got != wantIP {
		t.Fatalf("the container asked for %s and Docker published %s. `docker run` exited 0 "+
			"with an address nobody asked for, and nothing anywhere reports the "+
			"substitution.", wantIP, got)
	}
	t.Logf("the address asked for is the address published: %s", got)
}

// libnetwork never checks an ACK against the network's pool, and every daemon restart would then fail that endpoint's
// pool check, so the driver refuses it at the price of one failed `docker run` (#110).

// TestIPAM_AnAddressOutsideTheSubnetIsRefused checks that an ACK outside the network's --subnet is refused.
func TestIPAM_AnAddressOutsideTheSubnetIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-outside"
	// Served by no fixture, so every ACK is outside it.
	const foreignSubnet = "10.66.0.0/24"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", foreignSubnet, nil, nil)

	err := ipamRunContainerErr(t, ctx, cli, netName, "dh-itest-ipam-outside-ctr", nil)
	if err == nil {
		t.Fatal("a container was accepted on an address outside its own network's --subnet. " +
			"libnetwork does not check, so Docker's store would carry a container outside its " +
			"network's pool, and the endpoint's replay fails the pool check at every daemon " +
			"restart from then on (D50).")
	}
	if !strings.Contains(err.Error(), foreignSubnet) {
		t.Errorf("the refusal is %q and does not name the subnet that was typed (%s), which "+
			"is the one thing the operator has to change", err, foreignSubnet)
	}
}

// `--gateway` and `--aux-address` arrive as requests wire-identical to a stored endpoint's replay, and answering them
// would burn a lease at every create and daemon start (#110).

// TestIPAM_GatewayAndAuxNeverLease checks from the wire that --gateway and --aux-address put no DISCOVER on the segment.
func TestIPAM_GatewayAndAuxNeverLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-gw"
	const gw = "192.168.99.7"
	const aux = "192.168.99.8"

	cli := ipamDockerClient(t)
	wire := harness.StartDHCPCapture(t, harness.DHCPSegment)
	t.Cleanup(func() {
		if t.Failed() {
			wire.Dump(func(s string) { t.Log(s) })
		}
	})

	res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM: &network.IPAM{
			Driver: harness.DriverName,
			Config: []network.IPAMConfig{{
				Subnet:     harness.SubnetCIDR,
				Gateway:    gw,
				AuxAddress: map[string]string{"reserved": aux},
			}},
		},
		Options: map[string]string{"mode": "macvlan", "parent": harness.HostVeth},
	})
	if err != nil {
		t.Fatalf("NetworkCreate with --gateway and --aux-address: %v", err)
	}
	t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), res.ID) })

	// A DHCP exchange is synchronous inside the create, which has returned.
	time.Sleep(2 * time.Second)

	// Other tests' containers renew with REQUESTs on this shared segment; only a DISCOVER is an acquisition.
	var discovers []harness.DHCPClientMessage
	for _, m := range wire.Frames() {
		if m.Type == harness.DHCPDiscover {
			discovers = append(discovers, m)
		}
	}
	if len(discovers) != 0 {
		wire.Dump(func(s string) { t.Log(s) })
		t.Errorf("%d DISCOVER(s) on the segment for a create that started no container:\n%v\n"+
			"The gateway and the aux address are reserved in Docker's own record and must "+
			"never reach the DHCP server.", len(discovers), discovers)
	}

	insp := ipamNetworkInspect(t, ctx, cli, netName)
	if len(insp.IPAM.Config) != 1 {
		t.Fatalf("the IPAM block is %v, want one entry", insp.IPAM.Config)
	}
	if insp.IPAM.Config[0].Gateway != gw {
		t.Errorf("gateway = %q, want %q", insp.IPAM.Config[0].Gateway, gw)
	}
	if got := insp.IPAM.Config[0].AuxAddress["reserved"]; got != aux {
		t.Errorf("aux address = %q, want %q", got, aux)
	}
}

// TestIPAM_BothShapesOnOneDaemon checks that a null-shape and an IPAM-shape network work side by side on one plugin (#110).
func TestIPAM_BothShapesOnOneDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const nullNet = "dh-itest-ipam-nullside"
	const ipamNet = "dh-itest-ipam-ipamside"

	cli := ipamDockerClient(t)
	harness.CreateNetwork(t, ctx, nullNet, "macvlan", nil)
	harness.CreateNetworkIPAM(t, ctx, ipamNet, "macvlan", harness.SubnetCIDR, nil, nil)

	nullID, nullAddr, nullMAC := harness.RunContainer(t, ctx, nullNet, "dh-itest-ipam-nullside-ctr")
	_, ipamAddr, _ := harness.RunContainer(t, ctx, ipamNet, "dh-itest-ipam-ipamside-ctr")

	harness.AssertIP(t, nullAddr)
	harness.AssertIP(t, ipamAddr)
	if nullAddr == ipamAddr {
		t.Errorf("both containers hold %s; two live endpoints on one segment cannot share an "+
			"address", nullAddr)
	}
	out := harness.ExecOutput(t, ctx, nullID, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, nullAddr) {
		t.Errorf("the null-IPAM container's eth0 does not carry %s:\n%s", nullAddr, out)
	}
	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the fixture's log: %v", err)
	}
	if acked, _ := harness.ACKedTo(logData, nullAddr, nullMAC); !acked {
		t.Errorf("the null-mode container's address %s was not ACKed to %s while an IPAM-mode "+
			"network was live on the same plugin", nullAddr, nullMAC)
	}

	nullInsp := ipamNetworkInspect(t, ctx, cli, nullNet)
	if nullInsp.IPAM.Driver != "null" {
		t.Errorf("the null network's IPAM driver is %q; a network's IPAM driver is fixed at "+
			"create and an upgrade must not move it", nullInsp.IPAM.Driver)
	}
	ipamInsp := ipamNetworkInspect(t, ctx, cli, ipamNet)
	if !strings.Contains(ipamInsp.IPAM.Driver, "docker-net-dhcp") {
		t.Errorf("the IPAM network's IPAM driver is %q, want this plugin", ipamInsp.IPAM.Driver)
	}
}

// libnetwork generates a MAC for every endpoint of a RequiresMACAddress driver, and ipvlan children share the
// parent's MAC and refuse a supplied one, so the refusal moves to `docker network create` (#110).

// TestIPAM_IpvlanIsRefusedAtCreate checks that an IPAM-mode ipvlan network is refused at create and a null-shape one still works.
func TestIPAM_IpvlanIsRefusedAtCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	err := harness.CreateNetworkIPAMErr(ctx, "dh-itest-ipam-ipvlan", "ipvlan", harness.SubnetCIDR, nil, nil)
	if err == nil {
		t.Fatal("an ipvlan network was created with this plugin as its IPAM driver. Every " +
			"container on it would fail to start with a MAC error that says nothing about " +
			"IPAM (D49).")
	}
	for _, want := range []string{"ipvlan", "--ipam-driver null", "#949"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not mention %q, which is either the cause, "+
				"the way out, or where the work is tracked", err, want)
		}
	}

	harness.CreateNetwork(t, ctx, "dh-itest-ipam-ipvlan-null", "ipvlan", nil)
	_, addr, _ := harness.RunContainer(t, ctx, "dh-itest-ipam-ipvlan-null", "dh-itest-ipam-ipvlan-null-ctr")
	harness.AssertIP(t, addr)
}

// libnetwork sends ReleasePool for a create it rolled back, on the PoolID the first network still holds (#110).

// TestIPAM_TwoNetworksCannotShareOnePool checks that a second network on the same pool is refused and the first keeps its binding.
func TestIPAM_TwoNetworksCannotShareOnePool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})

	const netA = "dh-itest-ipam-pool-a"
	const netB = "dh-itest-ipam-pool-b"

	harness.CreateNetworkIPAM(t, ctx, netA, "macvlan", "", nil, nil)
	_, addrA, _ := harness.RunContainer(t, ctx, netA, "dh-itest-ipam-pool-a-ctr")
	harness.AssertIP(t, addrA)

	err := harness.CreateNetworkIPAMErr(ctx, netB, "bridge", "", nil, nil)
	if err == nil {
		t.Fatal("a second network took the pool identity the first one holds. Both would then " +
			"answer each other's address requests, and neither operator would see why.")
	}
	if !strings.Contains(err.Error(), "--ipam-opt") {
		t.Errorf("the refusal is %q and does not name the way out (`--ipam-opt parent=` or "+
			"`--ipam-opt bridge=`, or a `--subnet` of its own)", err)
	}

	_, addrA2, _ := harness.RunContainer(t, ctx, netA, "dh-itest-ipam-pool-a-ctr2")
	harness.AssertIP(t, addrA2)
	if addrA2 == addrA {
		t.Errorf("two live containers on %s both hold %s", netA, addrA)
	}

	// Named, the two identities differ and both networks exist.
	harness.CreateNetworkIPAM(t, ctx, netB, "bridge", "", map[string]string{"bridge": harness.BridgeName}, nil)
	_, addrB, _ := harness.RunContainer(t, ctx, netB, "dh-itest-ipam-pool-b-ctr")
	harness.AssertBridgeIP(t, addrB)
}

// libnetwork replays RequestPool and RequestAddress from libnetwork.New before the daemon's API listens, so the
// handlers must not call Docker. The lane's graceful restart drives Leave and a fresh CreateEndpoint (#386), so the
// address survives through the retained lease record, a replay hit, or a plugin that outlived the daemon, each
// identified by what it did; a miss is refused, and tombstones_consumed cannot move because the JSON tombstone write is
// gated on !ipamMode in network.go. The counters are absolutes because the plugin process is replaced (#110).

// TestIPAM_ReplayAfterDaemonRestart checks that a daemon restart preserves an IPAM endpoint's address by a named path and that the replayed pool still allocates.
func TestIPAM_ReplayAfterDaemonRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-replay"
	const ctrName = "dh-itest-ipam-replay-ctr"
	const afterName = "dh-itest-ipam-replay-after"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)

	// Without RestartPolicy=always the container comes back stopped and publishes no address (#110).
	hostCfg := harness.HostConfig()
	hostCfg.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyAlways}
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		hostCfg,
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		// The restart policy comes off before the stop, or the container comes straight back.
		bgCli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
		if err != nil {
			return
		}
		defer bgCli.Close()
		_, _ = bgCli.ContainerUpdate(bg, id, container.UpdateConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		})
		_ = bgCli.ContainerStop(bg, id, container.StopOptions{})
		_ = bgCli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	before, beforeMAC := ipamNetworkAddress(t, ctx, cli, id, netName)
	t.Logf("before the restart: %s on %s", before, beforeMAC)

	// The address appears at CreateEndpoint before Join starts the persistent client, so wait for the bind first (#386).
	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")
	waitLeaseObtained(t, bindW, 30*time.Second)
	bindW.End()

	healthBefore := harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)
	restartMark := time.Now()

	harness.RestartDockerDaemon(t, ctx)

	_ = cli.Close()
	cli2, err := waitDaemonReady(ctx, 60*time.Second)
	if err != nil {
		t.Fatalf("daemon did not return: %v", err)
	}
	t.Cleanup(func() { _ = cli2.Close() })
	if err := waitContainerRunning(ctx, cli2, id, 60*time.Second); err != nil {
		t.Fatalf("container not running after daemon restart: %v", err)
	}

	// Readiness only: the plugin process was replaced, so there is no delta.
	health := harness.WaitPluginHealth(t, ctx, cli2, 90*time.Second)
	windowSeconds := time.Since(restartMark).Seconds()
	// Logged only; it cannot move for an IPAM-mode network.
	t.Logf("replay: hits=%d miss=%d tombstones_consumed=%d; plugin instance %s -> %s, "+
		"uptime %.0fs -> %.0fs across a %.0fs window",
		health.IPAMReplayHits, health.IPAMReplayMiss, health.TombstonesConsumed,
		healthBefore.InstanceID, health.InstanceID,
		healthBefore.UptimeSeconds, health.UptimeSeconds, windowSeconds)

	// instance_id is minted once per process (#405) and uptime_seconds counts from that start, and both must agree.
	samePlugin := healthBefore.InstanceID != "" &&
		healthBefore.InstanceID == health.InstanceID &&
		health.UptimeSeconds > windowSeconds

	after, afterMAC := ipamNetworkAddress(t, ctx, cli2, id, netName)
	if after != before {
		t.Errorf("the container is at %s after the daemon restart; it was at %s.\n"+
			"Whether the endpoint was replayed or rebuilt, the address is this plugin's to "+
			"keep: one that moved means neither the replayed record nor the tombstone "+
			"resolved to the lease that holds it.", after, before)
	}
	if health.IPAMReplayMiss > 0 {
		t.Errorf("ipam_replay_miss is %d after a restart in which every endpoint's record was "+
			"on disk. A miss here is an endpoint libnetwork will re-allocate an address for.",
			health.IPAMReplayMiss)
	}
	switch {
	case health.IPAMReplayHits >= 1:
		t.Log("the address was confirmed by the replayed RequestAddress finding its record")
	case afterMAC != beforeMAC && beforeMAC != "" && afterMAC != "":
		// Run 34604958124: the plugin went down with the daemon and the container came back as a new endpoint with a new MAC
		// whose DISCOVER got the previous address, which only the retained record re-binding can produce (#110).
		t.Logf("the endpoint was rebuilt (%s -> %s) and the retained record re-bound the "+
			"same address under its own identity", beforeMAC, afterMAC)
	case samePlugin:
		// Same instance_id and an uptime longer than the window: the endpoint was never torn down.
		t.Logf("the plugin process outlived the daemon (instance %s, uptime %.0fs > the "+
			"%.0fs window), so the endpoint was never torn down and nothing was replayed "+
			"through this driver", health.InstanceID, health.UptimeSeconds, windowSeconds)
	default:
		t.Errorf("the address above survived the restart by none of the three modelled "+
			"paths: ipam_replay_hits=0, the endpoint MAC is unchanged (%s) so nothing was "+
			"rebuilt and re-bound, and the plugin did not outlive the daemon. Either it did "+
			"not really survive (the comparison above says), or it survived by a mechanism "+
			"this test does not model -- and an unmodelled mechanism is not something to "+
			"pass on (#386).", beforeMAC)
	}

	// The container is not merely recorded, it works.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, after) {
		t.Errorf("eth0 inside the container does not carry %s after the restart\n%s", after, out)
	}

	// A new endpoint allocates through the PoolID libnetwork replayed at startup.
	if err := ipamRunContainerErr(t, ctx, cli2, netName, afterName, nil); err != nil {
		t.Fatalf("a container created after the daemon restart could not start on the "+
			"pre-existing network: %v\nThe pool libnetwork replayed at startup does not "+
			"answer to the id this driver derives, so the network survives the restart "+
			"unusable -- visible to an operator only the next time they start something.", err)
	}
	newAddr, _ := ipamNetworkAddress(t, ctx, cli2, afterName, netName)
	t.Logf("a container created after the restart came up at %s", newAddr)
}

// The engine copies an operator-set endpoint MAC into a RequiresMACAddress driver's IPAM options (moby 28.5.2
// libnetwork/network.go), so two `docker run --mac-address X` on one network share one hardware address, which a DHCP
// server leases once (#110).

// TestIPAM_TwoEndpointsCannotShareOneHardwareAddress checks that a second endpoint with the same MAC is refused by name and the first keeps its address.
func TestIPAM_TwoEndpointsCannotShareOneHardwareAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-dupmac"
	const firstName = "dh-itest-ipam-dupmac-first"
	const secondName = "dh-itest-ipam-dupmac-second"
	// Not the --dhcp-host reservation, so the pair takes dynamic leases and only the collision refuses the second.
	const sharedMAC = "02:00:00:00:99:70"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)

	if err := ipamRunContainerErr(t, ctx, cli, netName, firstName,
		&network.EndpointSettings{MacAddress: sharedMAC}); err != nil {
		t.Fatalf("the first container pinned to %s did not start: %v", sharedMAC, err)
	}
	firstAddr, firstMAC := ipamNetworkAddress(t, ctx, cli, firstName, netName)
	if firstMAC != sharedMAC {
		t.Fatalf("the first container carries %s, not the pinned %s; the engine did not "+
			"honour --mac-address and this test drives nothing", firstMAC, sharedMAC)
	}
	t.Logf("first container: %s on %s", firstAddr, firstMAC)

	err := ipamRunContainerErr(t, ctx, cli, netName, secondName,
		&network.EndpointSettings{MacAddress: sharedMAC})
	if err == nil {
		second, _ := ipamNetworkAddress(t, ctx, cli, secondName, netName)
		t.Fatalf("a second container pinned to %s started on %s while the first holds %s. "+
			"Two endpoints on one hardware address cannot both hold a DHCP lease, and "+
			"whichever address Docker published for the second is one nothing granted it.",
			sharedMAC, second, firstAddr)
	}
	t.Logf("refused: %v", err)
	for _, want := range []string{sharedMAC, "--mac-address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q; it does not contain %q, which is what tells the "+
				"operator which container to change", err, want)
		}
	}
	if strings.Contains(err.Error(), "restarted") {
		t.Errorf("the refusal is %q. It blames a plugin restart, which did not happen; "+
			"the cause is two endpoints on one hardware address", err)
	}

	// The winner is untouched by the loser's arrival and rollback.
	againAddr, againMAC := ipamNetworkAddress(t, ctx, cli, firstName, netName)
	if againAddr != firstAddr || againMAC != firstMAC {
		t.Errorf("the first container was on %s/%s before the refused second start and is "+
			"on %s/%s after it; the refusal took the running container's address with it",
			firstAddr, firstMAC, againAddr, againMAC)
	}
}

// The rule is "exactly one live tombstone, consumed by the next address request on this network"
// (ipamRebindCandidate), and a RequestAddress carries no hostname or endpoint id (moby 28.5.2), so a container first
// started inside the window takes the stopped one's identity and address while ipam_rebind_ambiguous stays still (#110).

// TestIPAM_AContainerStartedInsideTheWindowTakesTheTombstone checks that a new container started inside the retention window takes the stopped container's address.
func TestIPAM_AContainerStartedInsideTheWindowTakesTheTombstone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-window"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)
	idA, addrA, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-window-a")
	t.Logf("a started on %s", addrA)

	w := harness.BeginCounterWindow(t, ctx, cli, "ipam_rebind_ambiguous")

	if err := cli.ContainerStop(ctx, idA, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop(a): %v", err)
	}
	// The tombstone is laid inside the stop, so this instant is at or after it.
	stopped := time.Now()

	idB, addrB, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-window-b")
	claimed := time.Since(stopped)
	t.Logf("b started on %s, %s after a stopped", addrB, claimed.Round(time.Second))

	// retentionWindow mirrors the plugin's tombstoneTTL and only chooses a failure's message, after the comparison has
	// already failed, so it can never turn a green row red (#110).
	const retentionWindow = 60 * time.Second
	if addrB != addrA && claimed >= retentionWindow {
		t.Fatalf("b came up on %s and a held %s, but b's address request landed %s after a "+
			"stopped, past the %s the plugin keeps a stopped endpoint's identity for. There "+
			"was no tombstone left to claim, so this run cannot decide the rule in either "+
			"direction -- it is not evidence that the rule changed.",
			addrB, addrA, claimed.Round(time.Second), retentionWindow)
	}

	if err := cli.ContainerStart(ctx, idA, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(a): %v", err)
	}
	addrA2, _ := ipamNetworkAddress(t, ctx, cli, idA, netName)
	t.Logf("a came back on %s", addrA2)

	if addrB != addrA {
		t.Errorf("the new container came up on %s and the stopped one held %s, %s apart.\n"+
			"This test pins the rule the code has: one live tombstone, consumed by the next "+
			"address request on the network, whoever makes it. The window is ruled out above, "+
			"measured, so what is left is the rule itself or the tombstone never being laid: "+
			"read the plugin log for the retain before reading this as a change of rule. If it "+
			"did change on purpose, the reference's Restart stability section is the other half "+
			"of the change.", addrB, addrA, claimed.Round(time.Second))
	}
	if addrA2 == addrA {
		t.Errorf("the stopped container came back on its own address %s even though a new "+
			"container had already claimed the only tombstone. Two endpoints cannot hold one "+
			"address on this network, so one of the two readings above is wrong.", addrA)
	}
	if !harness.IsInPool(harness.AssertIP(t, addrA2)) {
		t.Errorf("the restarted container came back on %s, outside the fixture's pool", addrA2)
	}
	_ = idB
	if addrB == addrA2 {
		t.Errorf("both containers report %s; the fixture handed one address to two endpoints", addrB)
	}

	before, after := w.End()
	if after.IPAMRebindAmbiguous != before.IPAMRebindAmbiguous {
		t.Errorf("ipam_rebind_ambiguous moved (%d -> %d) on a sequence with exactly one "+
			"candidate at every request. If the driver now sees this as ambiguous, it is no "+
			"longer the case this test documents.",
			before.IPAMRebindAmbiguous, after.IPAMRebindAmbiguous)
	}
}

// A restarted IPAM container has a new MAC and keeps its address only because the server matches the re-sent client
// identifier (RFC 2131 section 4.2); with dnsmasq's --dhcp-ignore-clid it gets a different address and no counter
// moves. Both arms run on one fixture one flag apart (#110).

// TestIPAM_SingleRestartNeedsAServerThatKeepsClientIDBindings checks that a restarted IPAM container keeps its address only on a server that honours option 61.
func TestIPAM_SingleRestartNeedsAServerThatKeepsClientIDBindings(t *testing.T) {
	restartOnce := func(t *testing.T, ef *harness.EphemeralFixture, netName, ctrName string) (before, after string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()
		t.Cleanup(func() {
			if t.Failed() {
				ef.DumpLogs(func(s string) { t.Log(s) })
				harness.DumpPluginLog(t)
			}
		})

		cli := ipamDockerClient(t)
		harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "",
			map[string]string{"parent": harness.EphemeralHostVeth},
			map[string]string{"parent": harness.EphemeralHostVeth})
		id, before, _ := harness.RunContainer(t, ctx, netName, ctrName)

		// Past the fixture's lease the address changes whatever the server keys on, so the restart's duration is checked.
		started := time.Now()
		if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
			t.Fatalf("ContainerRestart: %v", err)
		}
		after, _ = ipamNetworkAddress(t, ctx, cli, id, netName)
		gap := time.Since(started)
		t.Logf("%s: before=%s after=%s, the restart took %s of the fixture's %ds lease",
			t.Name(), before, after, gap.Round(time.Second), harness.EphemeralDefaultLeaseSeconds)
		if gap >= time.Duration(harness.EphemeralDefaultLeaseSeconds)*time.Second {
			t.Fatalf("the restart took %s, longer than the fixture's %ds lease, so the old "+
				"binding had expired at the server before the new request arrived. This run "+
				"cannot decide what the server keys its bindings on, in either direction.",
				gap.Round(time.Second), harness.EphemeralDefaultLeaseSeconds)
		}
		return before, after
	}

	t.Run("a server that honours option 61 keeps the address", func(t *testing.T) {
		ef := harness.NewEphemeralFixture(t, harness.WithDnsmasqBackend())
		before, after := restartOnce(t, ef, "dh-itest-ipam-clid-on", "dh-itest-ipam-clid-on-ctr")
		if after != before {
			t.Errorf("the container came back on %s; it held %s. Against a server that keys "+
				"its bindings on the client identifier the plugin re-sends, a single restart "+
				"keeps the address, and that is the property IPAM mode claims.", after, before)
		}
	})

	t.Run("a server keyed on the MAC alone does not", func(t *testing.T) {
		ef := harness.NewEphemeralFixture(t, harness.WithIgnoreClientID())
		before, after := restartOnce(t, ef, "dh-itest-ipam-clid-off", "dh-itest-ipam-clid-off-ctr")
		if after == before {
			t.Errorf("the container kept %s against a server started with --dhcp-ignore-clid.\n"+
				"That server cannot match the re-sent client identifier, and the endpoint's "+
				"hardware address is new, so keeping the address means the stability this "+
				"driver advertises comes from somewhere this test does not know about. Read the "+
				"dnsmasq arguments in the fixture dump first, since the flag not reaching the "+
				"server produces exactly this, and the mechanism second; the reference's Restart "+
				"stability section rests on the answer. The lease window is ruled out above, "+
				"measured.", after)
		}
		// harness.AssertIP checks the main fixture's range.
		harness.AssertEphemeralIP(t, after)
	})
}
