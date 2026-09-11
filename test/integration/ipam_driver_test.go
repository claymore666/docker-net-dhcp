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
		// The daemon can refuse at create time -- an address no pool on
		// the network contains is refused there -- and that is a
		// refusal, not a harness failure.
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

	// The OTHER counter this sequence could move, and must not. An
	// address reservation holds the parent NIC across its whole DHCP
	// exchange, so the second of two simultaneous starts on one macvlan
	// network queues behind the first and gives up after the 4s gate
	// budget. That is contention, not a collision: both are macvlan
	// children, a parent takes any number of those, and the second
	// start proceeds and succeeds. parent_link_wait_timeouts is a
	// health WARNING whose action says a start on that NIC may have
	// been refused, so it moving here would put a warning on the health
	// endpoint of every host running `docker compose up`, for something
	// nobody can act on -- and would teach an operator to ignore the
	// counter that names the collision the gate exists for.
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

	// The other side of the same pin, and the one nothing else can
	// reach: the address is asked for by a container the reservation is
	// NOT for, so dnsmasq -- which takes a --dhcp-host address out of
	// the dynamic pool entirely -- hands out something else. libnetwork
	// adopts whatever the driver returns without comparing it to the
	// address it preferred, so an unchecked ACK publishes an address the
	// operator never asked for and exits 0. The invariant is written as
	// the invariant: what Docker publishes is the address that was
	// demanded, or the run fails.
	//
	// IT RUNS BEFORE THE PINNED CONTAINER, and that ordering is the
	// whole test. With the pinned container already up, its live record
	// holds .95 and the dispatch refuses this request before any
	// exchange starts -- a correct refusal, and the "never substituted"
	// branch, but it reaches none of the reserve's post-ACK decisions.
	// Nothing holds the address here, so the reserve runs, the server
	// answers with an address that is not the one asked for, and
	// ipamACKIsTheOneAsked is what stands between that and a silent
	// override.
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
				// Must match the fixture's --dhcp-host reservation, so
				// the address asked for is one the server will grant.
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

// TestIPAM_StaticIPWithoutASubnetIsStillTheAddressAsked is the other
// half of row 4, and it asserts the opposite of what that row predicted.
//
// The row said the daemon refuses `--ip` on a network created without
// `--subnet`, citing its own validation. It does not, for a driver that
// answers the any-pool: the check the daemon runs is whether some
// subnet on the network contains the address, and 0.0.0.0/0 contains
// every address. MEASURED on CI engine 29.8.0 in integration run
// 34600486961: the container started, the fixture logged
// `DHCPDISCOVER ... 192.168.99.71` and `DHCPACK ... 192.168.99.71`, and
// Docker published .71. So `--ip` reaches RequestAddress here exactly
// as it does with `--subnet`.
//
// That leaves the invariant, which is the one that matters and is the
// same on both shapes: the address the operator pinned is the address
// the container gets, or the run fails. A substitution is the failure
// -- libnetwork adopts whatever the driver returns without comparing it
// to the preferred address -- and it is what ipamACKIsTheOneAsked
// refuses. The subnet rule (D50) has nothing to say on the any-pool, so
// this is the only thing standing between `--ip` and a silent override
// on a network with no subnet.
func TestIPAM_StaticIPWithoutASubnetIsStillTheAddressAsked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-nosubnet-ip"
	const ctrName = "dh-itest-ipam-nosubnet-ip-ctr"
	// In the fixture's range and pinned to nobody, so the server is
	// free to grant it and the run is expected to succeed.
	const wantIP = "192.168.99.71"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "", nil, nil)

	err := ipamRunContainerErr(t, ctx, cli, netName, ctrName,
		&network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: wantIP}})
	if err != nil {
		// A refusal is a legitimate outcome -- the server may have that
		// address out to someone else -- and it is not a substitution.
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
// At every daemon start libnetwork replays RequestPool for each stored
// network, and RequestAddress for each stored endpoint, from inside
// libnetwork.New -- before the daemon's own API is listening. Two
// things have to hold there and nowhere else: the PoolID must be the
// same function of the replayed request as of the create's, or nothing
// resolves; and the handlers must not ask Docker anything, because
// there is nobody to ask.
//
// WHAT THIS TEST DEMANDED AND WHY IT NO LONGER DOES.
// The first edition demanded ipam_replay_hits >= 1, reasoning that this
// test's own endpoint would be among the replayed ones. That is not the
// daemon restart this lane performs. harness.RestartDockerDaemon takes
// its containerized branch on CI and shuts the daemon down GRACEFULLY,
// a graceful shutdown drives Leave, and -- as
// TestRecovery_DaemonRestart_PreservesContainer has recorded since #386
// -- the container then comes back through a fresh CreateEndpoint
// rather than through a restored endpoint. A deleted endpoint is not
// replayed, so hits stays 0 and the address is preserved by the
// RETAINED LEASE RECORD instead -- not by the JSON tombstone, which
// #386 was about and which is never written for an IPAM-mode network
// (see below). Demanding the hit demands the ungraceful restart, which
// is #480 and not this fixture.
//
// The demand here is the #386 shape instead: the address survives, and
// it survived by a path this test MODELS, because "preserved by a
// mechanism nobody named" reads exactly like success and is how a
// regression hides. A replay MISS is refused in every case: a miss is
// an endpoint libnetwork re-allocates an address for.
//
// THREE PATHS ARE MODELLED, and tombstones_consumed is NOT one of them.
// The JSON tombstone write is gated on !ipamMode (network.go:1597), so
// for an IPAM-mode network no tombstone is ever laid and that counter
// cannot move -- an earlier edition of this switch offered it as one of
// two alternatives, which made the disjunction half dead and the live
// half the only thing that could ever pass. What can happen is: the
// endpoint is replayed and its record found (ipam_replay_hits); or the
// container is rebuilt with a NEW endpoint MAC and the retained lease
// record re-binds the old address under its own client identity; or the
// plugin process outlived the daemon and the endpoint was never torn
// down at all. Each is identified by what it did, not by the absence of
// the others.
//
// The pool half of the replay gets its own evidence, and it is the half
// this driver owns: a container created AFTER the restart, on a network
// created BEFORE it, has to get an address. Its CreateEndpoint resolves
// the PoolID libnetwork replayed at startup, so an address means the
// replayed pool and the created pool are the same id. If they were not,
// the daemon would have no pool to allocate from and the run would fail
// -- and nothing else in this suite would say so, since every other
// test creates its network inside its own run.
//
// The counters are read as ABSOLUTES, not deltas: the plugin process is
// replaced with the daemon, so what it has counted since it started IS
// what the restart did.
func TestIPAM_ReplayAfterDaemonRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-replay"
	const ctrName = "dh-itest-ipam-replay-ctr"
	const afterName = "dh-itest-ipam-replay-after"

	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil, nil)

	// RestartPolicy=always, because the property under test is about an
	// endpoint that still exists after the restart. Without it the
	// daemon comes back with the container stopped, Docker publishes no
	// address for a stopped container, and the comparison below
	// measures the restart policy rather than the replay. MEASURED:
	// that is what the first edition did, and it failed on "no address
	// within the budget" having never reached a replay assertion.
	//
	// RunContainer does not take a HostConfig; the create is inlined
	// the way TestRecovery_DaemonRestart_PreservesContainer inlines it,
	// and for the same reason.
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
		// A fresh client: the one above may already have been closed by
		// the cleanup chain, and the restart policy has to come off
		// before the stop or the container comes straight back.
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

	// The address above appears at CreateEndpoint, before Join has
	// started the persistent client. Pulling the daemon down inside
	// that window is a different test; wait for the bind first, the way
	// TestRecovery_DaemonRestart_PreservesContainer does, and close the
	// window while the plugin it measured is still the running one.
	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")
	waitLeaseObtained(t, bindW, 30*time.Second)
	bindW.End()

	// Who the plugin process was before the restart, and from when the
	// clock runs. Both are needed to tell the third path below apart
	// from the two modelled ones; see the switch.
	healthBefore := harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)
	restartMark := time.Now()

	harness.RestartDockerDaemon(t, ctx)

	// Every connection the old daemon held is dead, this test's
	// included.
	_ = cli.Close()
	cli2, err := waitDaemonReady(ctx, 60*time.Second)
	if err != nil {
		t.Fatalf("daemon did not return: %v", err)
	}
	t.Cleanup(func() { _ = cli2.Close() })
	if err := waitContainerRunning(ctx, cli2, id, 60*time.Second); err != nil {
		t.Fatalf("container not running after daemon restart: %v", err)
	}

	// The daemon is coming back; so is the plugin it starts. Readiness
	// only -- this makes no claim about a delta, and there is none to
	// make across a process that was replaced.
	health := harness.WaitPluginHealth(t, ctx, cli2, 90*time.Second)
	windowSeconds := time.Since(restartMark).Seconds()
	// tombstones_consumed is logged, not judged: see the header -- it
	// cannot move for an IPAM-mode network. A non-zero here would mean
	// the !ipamMode gate stopped holding, which is a different test's
	// subject.
	t.Logf("replay: hits=%d miss=%d tombstones_consumed=%d; plugin instance %s -> %s, "+
		"uptime %.0fs -> %.0fs across a %.0fs window",
		health.IPAMReplayHits, health.IPAMReplayMiss, health.TombstonesConsumed,
		healthBefore.InstanceID, health.InstanceID,
		healthBefore.UptimeSeconds, health.UptimeSeconds, windowSeconds)

	// Did the plugin process survive the restart?
	//
	// Two independent answers, and both must say so. instance_id is the
	// plugin's own identity, minted once per process (#405); uptime_seconds
	// is measured from that process's start, so a process that started
	// inside this window cannot report an uptime longer than the window.
	// Requiring both means a stuck or defaulted instance_id cannot on its
	// own make the restart disappear.
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
		// The path this fixture MEASURABLY takes, and the one the first
		// two editions of this switch did not model. Run 34604958124
		// main-8: the plugin log reads "Shutting down..." 13:37:35 and
		// "Starting server..." 13:37:45 -- the plugin went down WITH the
		// daemon -- and the container came back as a different endpoint
		// with a different MAC (4a:2d:08:d3:a7:e9 -> fe:d9:ef:2c:e0:ce),
		// whose DISCOVER asked for the previous address and got it.
		//
		// That is the retained lease record re-binding under its own
		// client identity: the record survives on disk, the reserve
		// finds exactly one live candidate for the network, and it asks
		// under the identity the server already has the lease filed
		// under rather than under the new MAC. A NEW MAC holding the
		// OLD address is that mechanism's signature and nothing else's
		// -- a replay would have restored the endpoint MAC and all.
		t.Logf("the endpoint was rebuilt (%s -> %s) and the retained record re-bound the "+
			"same address under its own identity", beforeMAC, afterMAC)
	case samePlugin:
		// The third modelled path, on the restart shapes where the
		// plugin is NOT recycled with the daemon: the endpoint is never
		// torn down, libnetwork's startup replay resolves it against
		// state the same plugin still holds, and neither the driver nor
		// the server is asked anything. Nothing was reallocated because
		// nothing was released. Identified positively -- same
		// instance_id AND an uptime longer than the window, so a
		// process that started inside it cannot claim this arm.
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

	// The pool half: a NEW endpoint on the network that existed before
	// the restart. Its allocation goes through the PoolID libnetwork
	// replayed at startup, so an address here is that id resolving.
	if err := ipamRunContainerErr(t, ctx, cli2, netName, afterName, nil); err != nil {
		t.Fatalf("a container created after the daemon restart could not start on the "+
			"pre-existing network: %v\nThe pool libnetwork replayed at startup does not "+
			"answer to the id this driver derives, so the network survives the restart "+
			"unusable -- visible to an operator only the next time they start something.", err)
	}
	// ContainerInspect takes a name as readily as an id, and the id of
	// this one is inside the helper that created it.
	newAddr, _ := ipamNetworkAddress(t, ctx, cli2, afterName, netName)
	t.Logf("a container created after the restart came up at %s", newAddr)
}

// TestIPAM_TwoEndpointsCannotShareOneHardwareAddress.
//
// The engine honours an operator-set endpoint MAC and copies it into the
// IPAM options for a RequiresMACAddress driver (moby 28.5.2,
// libnetwork/network.go:1222 and :1240; only a nil MAC is generated), so
// `docker run --mac-address X` twice on one network arrives at this
// plugin as two address requests carrying one hardware address on one
// pool. A DHCP server files its lease per hardware address and would
// hand both the same address.
//
// The outside evidence is Docker's own: the second `docker run` fails,
// its message names the hardware address, and the FIRST container keeps
// the address it was given. The failure this is written against is the
// quiet one -- both endpoints published on one address and the loser
// refused later with a message about a plugin restart that did not
// happen -- so "the second container did not start" is not enough on its
// own, and the first container's address is read again afterwards.
func TestIPAM_TwoEndpointsCannotShareOneHardwareAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam-dupmac"
	const firstName = "dh-itest-ipam-dupmac-first"
	const secondName = "dh-itest-ipam-dupmac-second"
	// Locally administered and unicast, and deliberately NOT the
	// fixture's --dhcp-host reservation: this pair must take dynamic
	// leases, so that what refuses the second is the collision and not a
	// static reservation the server would decline to hand out twice.
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

// TestIPAM_AContainerStartedInsideTheWindowTakesTheTombstone pins what
// the re-bind rule actually does, which is not what the reference said
// it does.
//
// The docs conditioned single-restart stability on no OTHER container
// being "stopped or removed in the previous minute". That names the
// wrong side of the window. The rule in the code is "exactly one live
// tombstone, consumed by the next address request on this network"
// (ipamRebindCandidate), and a request is what a container START makes.
// So a container started for the first time inside the window takes the
// stopped container's record, its DHCP identity and its address, and
// the container that was stopped comes back on a fresh one -- with
// nothing stopped or removed besides itself.
//
// It is pinned rather than fixed because RequestAddress carries no
// hostname and no endpoint id (moby 28.5.2: the only option libnetwork
// injects is the endpoint MAC, and it generates that per endpoint). The
// null shape narrows by hostname because CreateEndpoint has one; here
// there is nothing to narrow on, and a rule that guessed would hand one
// address to whichever container asked first while claiming otherwise.
// The docs now state the rule the code has.
//
// ipam_rebind_ambiguous staying still is part of the finding: this is
// not the ambiguous case, there is exactly one candidate, and the
// counter that makes the documented limit visible is silent here. An
// operator who reads only the counter sees nothing at all.
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

	stopped := time.Now()
	if err := cli.ContainerStop(ctx, idA, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop(a): %v", err)
	}

	// A brand-new container, inside A's retention window. Nothing about
	// it has ever been on this network.
	idB, addrB, _ := harness.RunContainer(t, ctx, netName, "dh-itest-ipam-window-b")
	claimed := time.Since(stopped)
	t.Logf("b started on %s, %s after a stopped", addrB, claimed.Round(time.Second))

	// retentionWindow mirrors the plugin's tombstoneTTL, and it decides
	// which MESSAGE a failure carries, never whether this test passes.
	// Past it there is no tombstone left to claim, so the rule below
	// has nothing to act on and the run says nothing about it. If the
	// plugin's value ever changes, the cost here is a misleading
	// sentence on an already-red row and never a green one.
	const retentionWindow = 60 * time.Second
	if claimed >= retentionWindow {
		t.Fatalf("b's address request landed %s after a stopped, past the %s the plugin keeps a "+
			"stopped endpoint's identity for. There was no tombstone left to claim, so this run "+
			"cannot decide the rule in either direction.",
			claimed.Round(time.Second), retentionWindow)
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

// TestIPAM_SingleRestartNeedsAServerThatKeepsClientIDBindings is the
// dependence behind TestIPAM_SingleRestartKeepsTheAddress, driven.
//
// In IPAM mode a restarted container comes back under a NEW hardware
// address, because libnetwork generates one per endpoint. It keeps its
// address only because the plugin re-sends the previous endpoint's
// client identifier and the server matches the binding on THAT. RFC
// 2131 section 4.2: where a client sends option 61 a server "MUST use
// that identifier to identify the client".
//
// A server keyed on the hardware address alone -- dnsmasq's
// --dhcp-ignore-clid, a supported setting -- has nothing to match on,
// sees a stranger asking for an address it has leased to someone else,
// and hands out a different one. Nothing is broken and no counter
// moves; the property is simply not available there. Null mode does not
// depend on this, because it restores the MAC itself.
//
// Both arms run on the same ephemeral fixture one flag apart, so the
// red arm measures the flag and not the shape of the test.
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
		// No --subnet, so `--ipam-opt parent=` is what separates this
		// network's pool identity from any other subnet-less one.
		harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "",
			map[string]string{"parent": harness.EphemeralHostVeth},
			map[string]string{"parent": harness.EphemeralHostVeth})
		id, before, _ := harness.RunContainer(t, ctx, netName, ctrName)

		// The restart has to finish inside the fixture's lease. Past it
		// the old binding is gone at the server, the address changes
		// whatever the server keys on, and neither arm below means what
		// its name says. Measured rather than assumed: it is the one
		// non-product cause either arm can have.
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

	// The control first. Without it the arm below measures "a restart
	// on the ephemeral fixture loses the address", which would be a
	// different and much worse finding.
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
		// Scoped to THIS fixture. harness.AssertIP carries its own
		// fatal on the MAIN fixture's pool, so composing the two kills
		// the subtest on every run, naming a range no address on this
		// fixture was ever supposed to be in.
		harness.AssertEphemeralIP(t, after)
	})
}
