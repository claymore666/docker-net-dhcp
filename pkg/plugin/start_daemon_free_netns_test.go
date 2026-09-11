// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The two drives for #417's delivered property, and its boundary.
//
// DELIVERED: the container's network namespace is entered and its link
// is located with no call to the daemon, where the sandbox key the Join
// request carries resolves.
//
// NOT DELIVERED, and asserted here so the claim cannot quietly grow:
// the persistent client is NOT started without the daemon. It waits on
// one ContainerInspect for the hostname that becomes DHCP option 12,
// because the library takes the hostname when the client is
// constructed, so a client started before the answer would never send
// it. #417 stays open for a hostname source that is not the daemon.
//
// The fixture is the package's own network namespace, reached through a
// sandbox-key entry that names it, and a link inside it found by MAC in
// macvlan mode. Nothing below Start is mocked: a real namespace handle,
// a real netlink handle opened on it, a real link. The daemon is the
// only fake.
//
// The MAC is READ FROM THAT NAMESPACE rather than written down here. A
// hardcoded address would make the drive a property of this host, and
// the one address that is the same everywhere -- loopback's -- is empty
// rather than zero, which macvlan mode refuses before it looks at any
// link. A namespace with no addressed link at all fails the fixture: it
// is an instrument that cannot measure, and a skip there would leave
// the whole property unasserted on exactly the host that has it.
func daemonFreeManager(t *testing.T, docker dockerClient) (*dhcpManager, *Plugin) {
	t.Helper()

	dir := t.TempDir()
	key := filepath.Join(dir, "1a2b3c4d5e6f")
	if err := linkANetnsEntry(key); err != nil {
		t.Fatalf("link fixture entry: %v", err)
	}
	withSandboxNetnsDirs(t, []string{dir})

	p := &Plugin{}
	m := newDHCPManager(docker, JoinRequest{
		NetworkID:  "net-1",
		EndpointID: "ep-abcdef",
		SandboxKey: key,
	}, DHCPNetworkOptions{Mode: ModeMacvlan}).withPlugin(p)
	m.MacAddress = anAddressedLink(t)
	withNetlinkHandleInThisNamespace(t, m)
	t.Cleanup(func() {
		closeNetHandle(m.netHandle)
		closeNsHandle(m.nsHandle)
	})
	return m, p
}

// withNetlinkHandleInThisNamespace swaps the one call in Start that
// needs a privilege this lane does not have.
//
// netlink.NewHandleAt setns()es to build its socket, and the kernel
// gates that on CAP_SYS_ADMIN even for the caller's OWN namespace. So
// root-free, nothing past the namespace open in Start is reachable at
// all, and the phases this file asserts could never appear.
//
// The substitute is not a stub of the link lookup. It is a real netlink
// handle whose zero value talks to the caller's current namespace,
// which is the same namespace the fixture key names, so the link walk
// that follows is the production one over real links. What the swap
// replaces is the setns, and it asserts the handle Start opened is the
// one it was about to enter: a seam that accepted any descriptor would
// hide a Start that entered the wrong namespace, which is the failure
// the sandbox key route exists to avoid.
func withNetlinkHandleInThisNamespace(t *testing.T, m *dhcpManager) {
	t.Helper()
	prev := nlNewHandleAt
	nlNewHandleAt = func(ns netns.NsHandle, _ ...int) (*netlink.Handle, error) {
		if ns != m.nsHandle {
			t.Errorf("the netlink handle was opened on namespace %d, but Start opened %d: the link "+
				"would be looked for somewhere other than the sandbox it just entered", ns, m.nsHandle)
		}
		return &netlink.Handle{}, nil
	}
	t.Cleanup(func() { nlNewHandleAt = prev })
}

// anAddressedLink returns the hardware address of some link in this
// process's network namespace, which is the namespace the fixture key
// names and therefore the one Start will search.
func anAddressedLink(t *testing.T) net.HardwareAddr {
	t.Helper()
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatalf("listing links in this namespace: %v", err)
	}
	for _, l := range links {
		if len(l.Attrs().HardwareAddr) > 0 {
			return l.Attrs().HardwareAddr
		}
	}
	t.Fatal("no link in this network namespace has a hardware address, so link location cannot be " +
		"driven here at all. This is the instrument failing and not the property being absent")
	return nil
}

// TestStart_EntersTheNamespaceAndLocatesTheLinkWithoutTheDaemon is the
// drive for the property the reorder delivers.
//
// The daemon fails every call. If anything on the path into the
// namespace still asked it, there would be no open_netns phase and no
// locate_link phase to find: the failure would be the daemon's, before
// either. Both phases completing against a daemon that answers nothing
// is the property, and it is read from the record Start keeps for its
// caller rather than from a log line.
//
// The counters are asserted too, because "the namespace was opened"
// without "through the key" is satisfied by the PID route -- which
// needs the daemon, and would have had to fail here.
func TestStart_EntersTheNamespaceAndLocatesTheLinkWithoutTheDaemon(t *testing.T) {
	daemonDown := errors.New("daemon is not answering anything")
	m, p := daemonFreeManager(t, &fakeDocker{
		listErr:      daemonDown,
		inspectErr:   daemonDown,
		containerErr: daemonDown,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded against a daemon that fails every call; the hostname inspect cannot have happened")
	}

	if !strings.Contains(m.startPhases, "open_netns=") {
		t.Errorf("phase summary %q has no open_netns: the namespace was not entered against a daemon "+
			"that answers nothing, so something on the path into it still asks the daemon (#417)", m.startPhases)
	}
	if !strings.Contains(m.startPhases, "locate_link=") {
		t.Errorf("phase summary %q has no locate_link: the container's link was not found against a "+
			"daemon that answers nothing, so link location still depends on it (#417)", m.startPhases)
	}
	if got := p.sandboxKeyEntries.Load(); got != 1 {
		t.Errorf("sandbox_key_entries = %d, want 1: the namespace this test opened was not entered "+
			"through the sandbox key, and every other route needs the daemon", got)
	}
	if got := p.sandboxPIDFallbacks.Load(); got != 0 {
		t.Errorf("sandbox_pid_fallbacks = %d, want 0: the PID route reads the container's PID from the "+
			"daemon, which failed every call here", got)
	}
	if got := p.sandboxKeyEntryFailures.Load(); got != 0 {
		t.Errorf("sandbox_key_entry_failures = %d, want 0 on a key that names a real namespace", got)
	}
}

// TestStart_AsksTheDaemonNothingBeforeTheLinkIsLocated is the same
// property read from the other side, and it is the one a reordering
// mutant cannot survive.
//
// The phases above say the namespace and the link were reached. They do
// not say the daemon was not asked on the way: a Start that inspected
// first and then opened both would print the same two phases. So this
// counts the calls. The fake fails every one, and the failure this
// leaves is the link's, at a point where the daemon has been asked
// nothing at all.
func TestStart_AsksTheDaemonNothingBeforeTheLinkIsLocated(t *testing.T) {
	daemonDown := errors.New("daemon is not answering anything")
	docker := &fakeDocker{listErr: daemonDown, inspectErr: daemonDown, containerErr: daemonDown}
	// A MAC no link carries, so link location is what fails and the
	// inspect that follows it is never reached.
	m, _ := daemonFreeManager(t, docker)
	m.MacAddress = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx); err == nil {
		t.Fatal("Start found a link carrying a MAC nothing in this namespace has")
	}

	if !strings.Contains(m.startPhases, "open_netns=") {
		t.Errorf("phase summary %q has no open_netns: the namespace was not entered", m.startPhases)
	}
	if calls := docker.listCalls + docker.inspectCalls + docker.containerCalls; calls != 0 {
		t.Errorf("the daemon was called %d time(s) (list %d, network inspect %d, container inspect %d) "+
			"before the container's link was located. Every one of them can block for the length of a "+
			"ContainerStart (#406), which is the whole reason the namespace is entered through the "+
			"sandbox key first (#417)",
			calls, docker.listCalls, docker.inspectCalls, docker.containerCalls)
	}
}

// TestStart_DoesNotStartTheClientBeforeTheInspectAnswers is the other
// half, and it is the BOUNDARY rather than the feature.
//
// The daemon here is the #406 daemon: it accepts the connection and
// never answers, because it is inside ContainerStart for this very
// container. The namespace opens and the link is found regardless --
// that is the property above -- and then the attach waits, and is
// abandoned at its budget with no persistent client started.
//
// Without this, "the attach no longer needs the daemon" would be a
// sentence nothing in the tree contradicts. attachDaemonBusyGrace is
// load-bearing for exactly this wait, and the test that would go red on
// its removal is the one that says the wait is still here.
func TestStart_DoesNotStartTheClientBeforeTheInspectAnswers(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"container-1": {EndpointID: "ep-abcdef"},
			}},
		},
		// Accepted, never answered: the daemon is inside ContainerStart
		// for this container (#406).
		containerDelay: time.Hour,
	}
	m, p := daemonFreeManager(t, docker)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded while the daemon never answered ContainerInspect")
	}

	if !strings.Contains(m.startPhases, "open_netns=") || !strings.Contains(m.startPhases, "locate_link=") {
		t.Errorf("phase summary %q: the namespace and the link must be reached before the wait, "+
			"or this test is measuring a failure that happened earlier", m.startPhases)
	}
	if strings.Contains(m.startPhases, "start_clients=") {
		t.Errorf("phase summary %q says the clients started while the hostname inspect never "+
			"answered. The library takes the hostname when the client is constructed, so a client "+
			"started here would send no hostname for its whole life (#417)", m.startPhases)
	}
	if m.errChan != nil {
		t.Error("a persistent client was started before the inspect answered; its lease would appear " +
			"in the DHCP server's table with no hostname until the plugin restarts")
	}
	// The two above say the client did not start. They do not say WHY,
	// and "it tried and failed" wears the same face in this lane as "it
	// waited": an attach that skipped the inspect entirely would satisfy
	// both, because building a DHCP client needs privileges no unit test
	// has. These two say the attach spent its budget inside the inspect.
	if docker.containerCalls != 1 {
		t.Errorf("the daemon was asked to inspect the container %d times, want exactly 1: the attach "+
			"must reach the hostname inspect and wait there, and an attach that never asked would "+
			"have started a client with no hostname for its whole life", docker.containerCalls)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Start failed with %v, want the deadline: the budget was spent somewhere other than "+
			"waiting on the daemon, so this case is not measuring the wait it names", err)
	}
	if got := p.sandboxKeyEntries.Load(); got != 1 {
		t.Errorf("sandbox_key_entries = %d, want 1: the wait must be after the namespace was entered, "+
			"not instead of entering it", got)
	}
	if m.startTotal == "" {
		t.Error("Start recorded no total: a reader cannot tell this wait from an immediate refusal")
	}
}

// TestStart_AsksTheDaemonOnceForTheWholeAttach drives the refusal path,
// which is the path #417 must leave exactly as it was.
//
// The key here names nothing, so it is refused, and the container PID
// route carries the attach the way it does on every host whose sandbox
// netns mount is private. Two things are asserted about that path:
//
//   - it still works, counters and all. A reorder that only ever ran
//     where the key resolves would pass every other case in this file
//     while breaking the host the CI lane actually runs on.
//   - the daemon is asked for the container EXACTLY ONCE. The PID
//     fallback and the hostname want the same inspect, and the daemon
//     is inside ContainerStart while both are wanted (#406), so a
//     second call is a second wait of the same length. Nothing else in
//     the tree would notice it: the attach would still succeed, just
//     twice as slowly, on the host that can least afford it.
func TestStart_AsksTheDaemonOnceForTheWholeAttach(t *testing.T) {
	pid := os.Getpid()
	ctrID := selfCgroupLeaf(t, pid)

	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				ctrID: {EndpointID: "ep-abcdef"},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			ctrID: {
				ContainerJSONBase: &dContainer.ContainerJSONBase{State: &dContainer.State{Pid: pid}},
				Config:            &dContainer.Config{Hostname: "ctr-1"},
			},
		},
	}
	m, p := daemonFreeManager(t, docker)
	// A key that names no entry of a permitted directory: refused on
	// sight, once, and the PID route carries it from there.
	m.joinReq.SandboxKey = "/tmp/not-a-sandbox-key"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Start goes on to build a DHCP client, which this lane cannot do;
	// the phases and the counters below are what this drive reads, and
	// they are recorded either way.
	_ = m.Start(ctx)

	if !strings.Contains(m.startPhases, "locate_link=") {
		t.Errorf("phase summary %q: the PID route did not carry this attach as far as the link, so "+
			"the assertions below are about a path that did not run", m.startPhases)
	}
	if got := p.sandboxPIDFallbacks.Load(); got != 1 {
		t.Errorf("sandbox_pid_fallbacks = %d, want 1: the refused key must fall through to the "+
			"container PID route, which is the only route on a host whose sandbox netns mount is "+
			"private", got)
	}
	if got := p.sandboxKeyEntryFailures.Load(); got != 1 {
		t.Errorf("sandbox_key_entry_failures = %d, want exactly 1: the key route is attempted once "+
			"and not retried, because a permanent refusal polled to the deadline is attach budget "+
			"the PID route then does not have (#401)", got)
	}
	if got := p.sandboxKeyEntries.Load(); got != 0 {
		t.Errorf("sandbox_key_entries = %d, want 0: nothing was entered through a key naming no entry", got)
	}
	if docker.containerCalls != 1 {
		t.Errorf("the daemon was asked to inspect the container %d times for one attach, want 1. The "+
			"PID fallback and the hostname want the same answer, and the daemon is inside "+
			"ContainerStart for this container while both are wanted (#406), so the second call is a "+
			"second wait of the same length", docker.containerCalls)
	}
	if m.hostname != "ctr-1" {
		t.Errorf("m.hostname = %q, want %q: the inspect the fallback made must also be the one the "+
			"DHCP hostname option comes from", m.hostname, "ctr-1")
	}
}

// TestStart_ARefusedKeyAndNoDaemonNamesBothCauses is the error-path
// half of the split opener.
//
// Where the key is refused the PID comes from the daemon, so a daemon
// that will not answer means there is no second route to try. What the
// attach must not do is carry on with the PID it did not get: polling
// /proc/0/ns/net to the deadline turns a known cause into an unknown
// one, spends the budget that is left, and reports a failure about a
// process that does not exist rather than about the daemon.
//
// Both causes are named because either alone is misleading. The key
// refusal is why the PID was needed at all; the daemon failure is why
// there was none.
func TestStart_ARefusedKeyAndNoDaemonNamesBothCauses(t *testing.T) {
	daemonDown := errors.New("daemon is not answering anything")
	m, _ := daemonFreeManager(t, &fakeDocker{
		listErr: daemonDown, inspectErr: daemonDown, containerErr: daemonDown,
	})
	m.joinReq.SandboxKey = "/tmp/not-a-sandbox-key"

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded with a refused key and a daemon that answers nothing")
	}
	if !strings.Contains(err.Error(), daemonDown.Error()) {
		t.Errorf("err = %v: it does not name the daemon failure that left the PID route without a "+
			"PID, so a reader is told about a namespace rather than about the daemon", err)
	}
	if !strings.Contains(err.Error(), "sandbox key route") {
		t.Errorf("err = %v: it does not name the key refusal that made the PID necessary, and the "+
			"refusal is what says which route this host takes", err)
	}
}
