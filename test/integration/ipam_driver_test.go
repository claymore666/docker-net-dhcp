// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The bundled DHCP IPAM driver (#110), against a real daemon.
//
// EVERY ASSERTION HERE IS AGAINST OUTSIDE EVIDENCE. The address comes
// from Docker's own store and is confirmed against the DHCP server's
// log line that granted it; the refusals are the daemon's own error
// text; the "nothing leased" claims are counts of client frames on the
// segment, captured off the wire. The plugin's counters appear in
// exactly two tests, and in both the counter IS the subject: an
// ambiguous re-bind is a documented limit, and a limit nothing counts
// is a limit nobody can see.
//
// The existing suite is the `--ipam-driver null` shape and stays
// unchanged, which is D19. TestIPAM_BothShapesOnOneDaemon is the part
// of that promise a test can carry.

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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// ipamDumpOnFailure is the evidence every test in this file leaves
// behind when it goes red: the fixture's own log, which is what says
// what the server did, and the plugin's, which is what says what it was
// asked.
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

// ipamNetworkAddress reads the address Docker publishes for one
// container on one network, together with the MAC it currently carries.
//
// It reads the SAME field RunContainer polls -- the endpoint's
// IPAddress in the daemon's store -- because that field is what the
// feature is about. A restart test that read the address off the link
// inside the container would pass while Docker's store said something
// else, which is the exact divergence an IPAM driver exists to close.
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

// ipamRunContainerErr starts a container and returns the daemon's error
// for the cases that must FAIL.
//
// Separate from harness.RunContainer for the reason
// CreateNetworkIPAMErr is separate from CreateNetworkIPAM: a refusal
// asserted by catching a t.Fatalf is not asserted at all.
func ipamRunContainerErr(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName string, ep *network.EndpointSettings, mac string) error {
	t.Helper()
	if ep == nil {
		ep = &network.EndpointSettings{}
	}
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:      harness.TestImage,
			Cmd:        []string{"sleep", "infinity"},
			Hostname:   ctrName,
			MacAddress: mac,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: ep}},
		nil, ctrName)
	if err != nil {
		// The daemon can refuse at create time -- `--ip` without
		// `--subnet` is refused there -- and that is a refusal, not a
		// harness failure.
		return err
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, create.ID, container.StopOptions{})
		_ = cli.ContainerRemove(bg, create.ID, container.RemoveOptions{Force: true})
	})
	return cli.ContainerStart(ctx, create.ID, container.StartOptions{})
}

// ipamNetworkInspect is `docker network inspect`, which is where an
// operator reads what the IPAM driver answered.
func ipamNetworkInspect(t *testing.T, ctx context.Context, cli *docker.Client, name string) network.Inspect {
	t.Helper()
	insp, err := cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		t.Fatalf("NetworkInspect(%s): %v", name, err)
	}
	return insp
}

// TestIPAM_AddressIsTheServersACK is design row 1.
//
// The address Docker publishes must be the one the DHCP server granted.
// Docker already publishes an address in null mode -- the network
// driver returns it at CreateEndpoint -- so "an address appears" proves
// nothing about this feature. What proves it is that the same address
// appears in the network's own record, inside a pool this driver
// answered for, and is the address in the server's DHCPACK line for
// this container's MAC.
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

	// Docker's network record carries it, which is what a second
	// container's DNS lookup and `docker network inspect` read.
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
	// The typed --subnet is what the IPAM block shows. In null mode
	// this block does not exist at all, which is half of what #110 is
	// about: `--ip`, compose's ipv4_address and every tool that reads
	// the block need it to be there and to be true.
	if len(insp.IPAM.Config) != 1 || insp.IPAM.Config[0].Subnet != harness.SubnetCIDR {
		t.Errorf("the IPAM block is %v, want the one --subnet that was typed (%s)",
			insp.IPAM.Config, harness.SubnetCIDR)
	}

	// The container agrees with the store. Two records of one fact, and
	// the IPAM shape is where they can disagree: the address reported
	// at RequestAddress and the address the link carries are written by
	// two different calls.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, ipv4) {
		t.Errorf("eth0 inside the container does not carry the address Docker published (%s)\n%s", ipv4, out)
	}
}

// TestIPAM_NoSubnetAnswersTheAnyPool is the other half of row 1, on the
// bridge fixture, and carries row 16 with it.
//
// A network with no --subnet must come back as 0.0.0.0/0, and that is
// not cosmetic: libnetwork's remote allocator asks again for any pool
// that overlaps an on-link route on the host, and the parent's own
// subnet is such a route -- so a driver that answered the real LAN
// prefix would be asked forever and `docker network create` would never
// return. This test returning at all is the assertion behind the one it
// makes.
//
// Row 16 is the second half: the exchange runs on a temporary veth that
// is then deleted and its MAC re-used on the container's own veth
// seconds later. If the bridge kept the old FDB entry, the server's
// unicast reply would go to a port that no longer exists. `bridge fdb`
// after Join is what says it did not.
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

	// Row 16: the container's MAC is learned on the bridge, on the
	// container's own port, after the temporary link that used it went
	// away.
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

// TestIPAM_SingleRestartKeepsTheAddress is design row 2, and it carries
// row 10's tombstone gate with it.
//
// Docker mints a fresh MAC at every container start, so the address
// survives only because the re-bind takes the client identity from the
// record of the endpoint that just went away. The second assertion is
// that the JSON tombstone store -- 1.x's hostname-keyed mechanism,
// still in the tree for null mode -- was NOT written for this network:
// two tombstone mechanisms both holding a re-bind candidate is one
// address promised to two endpoints.
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
			"exercise the identity carry-over the assertion above is about", beforeMAC)
	}
	logData, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read the fixture's log: %v", err)
	}
	if acked, acks := harness.ACKedTo(logData, after, afterMAC); !acked {
		t.Errorf("the server never ACKed %s to the restarted container's MAC %s; the address "+
			"in Docker's store is not the one that was leased.\nACKs for it: %v", after, afterMAC, acks)
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

// TestIPAM_RestartedTogetherIsTheDocumentedLimit is design row 3, and
// its name is the point: N>=2 containers restarted together have NO
// address-stability guarantee in IPAM mode, deliberately.
//
// A RequestAddress carries no hostname and no endpoint id, so with two
// retained records on one network there is nothing to match a request
// back to a previous lease on. Every container still gets an address;
// WHICH address is the server's decision. The driver counts the
// ambiguity rather than guessing, and the counter is the assertion here
// because the counter is what makes the limit visible in
// /Plugin.Health.
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

	w := harness.BeginCounterWindow(t, ctx, cli, "ipam_rebind_ambiguous")

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
}

// TestIPAM_ARemovedNeighbourMakesTheRebindAmbiguous is design row 17.
//
// "A container restarted on its own keeps its address" is narrower than
// it reads. Any OTHER endpoint on the same network removed inside the
// retention window leaves a second retained record, and the re-bind has
// nothing to narrow on again -- with one container restarting. The
// sentence in docs/reference.md names that window; this is the test
// that makes it a measured claim rather than a caveat.
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
	// Inside the retention window, which is what makes B's record a
	// candidate for A's request.
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

// TestIPAM_StaticIPIsHonouredWithASubnet is design row 4, the half that
// is the feature: `--ip` reaches the driver as a preferred address only
// when the network was created with `--subnet`.
//
// The fixture pins StaticTestIP to StaticTestMAC with a --dhcp-host
// reservation, so asking for that pair is asking for something the
// server will actually grant -- which is the only honest shape for this
// test. An unreserved address would be granted or not depending on what
// else the suite is holding, and would have passed either way.
func TestIPAM_StaticIPIsHonouredWithASubnet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-static"
	const ctrName = "dh-itest-ipam-static-ctr"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:      harness.TestImage,
			Cmd:        []string{"sleep", "infinity"},
			Hostname:   ctrName,
			MacAddress: harness.StaticTestMAC,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: harness.StaticTestIP}},
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

// TestIPAM_StaticIPWithoutASubnetIsRefused is the other half of row 4.
//
// `--ip` is only meaningful against a typed pool, and the daemon itself
// refuses it on a network with none. The assertion belongs here anyway:
// this is the error a user of the new shape will hit, and the remedy
// (`--subnet`) is a property of how this driver answers RequestPool.
func TestIPAM_StaticIPWithoutASubnetIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-nosubnet-ip"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "", nil, nil)

	err := ipamRunContainerErr(t, ctx, cli, netName, "dh-itest-ipam-nosubnet-ip-ctr",
		&network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: "192.168.99.71"}}, "")
	if err == nil {
		t.Fatal("a container took `--ip` on a network created without `--subnet`. Nothing " +
			"then constrains the address the server may hand back, and Docker's store would " +
			"be reporting an address that was asked for and not granted.")
	}
	t.Logf("refused, as it must be: %v", err)
}

// TestIPAM_AnAddressOutsideTheSubnetIsRefused is D50.
//
// A network created with a --subnet the server does not serve gets an
// ACK from outside it. Accepting it would put a container in Docker's
// store at an address outside its own network's pool -- libnetwork
// never checks -- and every daemon restart would then fail that
// endpoint's pool check and adopt it with a warning. The price of
// refusing is a failed `docker run` and one server lease burned, which
// is the price `--ip` already pays.
func TestIPAM_AnAddressOutsideTheSubnetIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-outside"
	// A subnet the fixture's server does not serve, so every ACK it
	// gives is outside it. Deliberately not the bridge fixture's
	// range either: this must not be an address any fixture could
	// legitimately hand out.
	const foreignSubnet = "10.66.0.0/24"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", foreignSubnet, nil, nil)

	err := ipamRunContainerErr(t, ctx, cli, netName, "dh-itest-ipam-outside-ctr", nil, "")
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

// TestIPAM_GatewayAndAuxNeverLease is design row 5.
//
// `--gateway` and `--aux-address` reach an IPAM driver as address
// requests that are WIRE-IDENTICAL to a stored endpoint's replay: an
// address, no options. Answering either with a DHCP exchange would burn
// a real lease at `docker network create`, for an address no container
// will ever use, and again at every daemon start.
//
// The evidence is the segment: a create that leased anything would put
// a DISCOVER on it, and this test starts no container.
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

	// Anything the create was going to put on the wire is out by now:
	// the create returned, and a DHCP exchange is synchronous inside it.
	time.Sleep(2 * time.Second)

	// DISCOVERs only. The segment is shared with every other test in
	// this process, and a container of theirs renewing its lease sends
	// a REQUEST -- which is not an acquisition and not this test's
	// subject. A DISCOVER here can only have come from an exchange
	// started during this window, and this window created no container.
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

// TestIPAM_BothShapesOnOneDaemon is design row 7 and the part of D19 a
// test can carry: the `--ipam-driver null` shape is the product and
// does not change when the IPAM driver exists beside it.
//
// The rest of this directory is that assertion at length -- every
// existing test runs on the same plugin process as this one, on the
// null shape, unchanged. What this test adds is the two shapes ALIVE AT
// THE SAME TIME on one plugin, which is the configuration in which a
// per-network decision made globally would show up.
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

	// The two networks' records are what an operator reads, and they
	// must say different things: null mode has no IPAM block to show.
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

// TestIPAM_IpvlanIsRefusedAtCreate is design row 9 and D49.
//
// libnetwork generates a MAC for every endpoint once an IPAM driver
// declares RequiresMACAddress, and ipvlan children share the parent's
// MAC and refuse a supplied one. Every ipvlan endpoint in IPAM mode
// would therefore fail at container start, with a MAC error that names
// nothing about IPAM. The refusal is moved to `docker network create`,
// where the operator can act on it, and it costs an ipvlan user nothing
// they have.
//
// The second half is the preservation control, and it is not optional:
// a widening whose only test is what it now refuses has no boundary.
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

	// The control: the same mode on the null shape still works, end to
	// end, on the same plugin.
	harness.CreateNetwork(t, ctx, "dh-itest-ipam-ipvlan-null", "ipvlan", nil)
	_, addr, _ := harness.RunContainer(t, ctx, "dh-itest-ipam-ipvlan-null", "dh-itest-ipam-ipvlan-null-ctr")
	harness.AssertIP(t, addr)
}

// TestIPAM_TwoNetworksCannotShareOnePool is design row 10 / defeat row
// 6.
//
// Two networks with the same pool and no `--ipam-opt` derive one
// PoolID, and a PoolID is what binds address requests to a network. The
// second create is refused with the remedy in its text.
//
// The assertion that matters more is the one after it: libnetwork sends
// ReleasePool for a create it rolled back, on a PoolID the FIRST
// network still holds. A refusal that cost the network already there
// its binding would be worse than the collision it prevented.
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

	// No --subnet on either, so both derive the any-pool identity in
	// the same address space: the collision this refusal is for.
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

	// The first network is untouched: its running container still holds
	// its address, and it can still lease a new one.
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

// TestIPAM_ReplayAfterDaemonRestart is design row 6.
//
// At every daemon start libnetwork replays RequestPool and one
// RequestAddress per STORED endpoint, from inside libnetwork.New --
// before the daemon's own API is listening. Two things have to hold
// there and nowhere else: the PoolID must be the same function of the
// replayed request as of the create's, or nothing resolves; and the
// handlers must not ask Docker anything, because there is nobody to
// ask.
//
// The evidence is the address in Docker's store after the restart and
// the replay count on the plugin that came up with the daemon. The
// counter is read as an ABSOLUTE here, not as a delta: the plugin
// process is new, so what it has counted since it started IS what the
// replay did. That is also why this test takes the one reading
// WaitPluginHealth exists for rather than a window.
func TestIPAM_ReplayAfterDaemonRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-replay"
	const ctrName = "dh-itest-ipam-replay-ctr"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)
	id, before, _ := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("before the restart: %s", before)

	harness.RestartDockerDaemon(t, ctx)

	// The daemon is coming back; so is the plugin it starts. Readiness
	// only -- this makes no claim about a delta, and there is none to
	// make across a process that was replaced.
	health := harness.WaitPluginHealth(t, ctx, cli, 90*time.Second)

	after, _ := ipamNetworkAddress(t, ctx, cli, id, netName)
	if after != before {
		t.Errorf("the container is at %s after the daemon restart; it was at %s.\n"+
			"The endpoint was replayed from the daemon's store, so an address that moved "+
			"means the replay did not resolve to the record that holds it.", after, before)
	}
	if health.IPAMReplayHits < 1 {
		t.Errorf("ipam_replay_hits is %d on the plugin that came up with the daemon.\n"+
			"At least this test's own endpoint was replayed, so a zero means the replayed "+
			"RequestAddress did not find the record that holds its address -- which is the "+
			"failure that looks like nothing until the address changes.", health.IPAMReplayHits)
	}
	if health.IPAMReplayMiss > 0 {
		t.Errorf("ipam_replay_miss is %d after a restart in which every endpoint's record was "+
			"on disk. A miss here is an endpoint libnetwork will re-allocate an address for.",
			health.IPAMReplayMiss)
	}
	t.Logf("replay: hits=%d miss=%d", health.IPAMReplayHits, health.IPAMReplayMiss)

	// The container is not merely recorded, it works.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, after) {
		t.Errorf("eth0 inside the container does not carry %s after the restart\n%s", after, out)
	}
}
