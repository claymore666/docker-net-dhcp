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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The drives for #417's and #961's delivered property, and its bounds.
//
// DELIVERED: where the sandbox key the Join request carries resolves,
// the container's network namespace is entered, its link is located AND
// the persistent client is started with no call to the daemon. The
// inspect that supplies DHCP option 12 runs afterwards, on a container
// that is already leasing, and the name is given to the running client
// (#961).
//
// THE BOUNDS, asserted here so the claim cannot quietly grow. A network
// with register_dns still waits for the name before the client starts:
// the name goes in RFC 4702's option 81 there, which the library takes
// at construction and has no setter for. And where the sandbox key is
// refused the PID fallback has already asked the daemon on the way in,
// so the name is in hand before the client starts and costs no second
// call.
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
// replaces is the setns.
//
// WHAT THE DESCRIPTOR ASSERTION CATCHES, exactly, because an earlier
// version of this comment claimed more than it can fail for: Start
// handing this call a descriptor other than the namespace handle it
// just opened. It cannot catch Start opening the WRONG namespace. The
// field it compares against was assigned from that same open, and the
// substitute handle talks to the caller's namespace whatever Start
// opened, so the link walk would look identical. That property is held
// by a different drive: an opener that ignored the key and took the
// current namespace dies in TestStart_AsksTheDaemonOnceForTheWholeAttach,
// where sandbox_key_entries must stay 0 for a key naming no entry.
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
	links, err := util.DumpResult(netlink.LinkList())
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
	// Start goes on to open a DHCP socket, which this lane cannot do,
	// so it fails for that reason and not for the daemon's. The phases
	// and the counters below are what this drive reads, and they are
	// recorded either way.
	_ = m.Start(ctx)

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

// TestStart_AsksTheDaemonNothingBeforeThePersistentClientStarts is the
// whole of what #961 delivers, read at the instant it is about.
//
// The phases say the namespace, the link and the clients were reached.
// They do not say the daemon was not asked on the way: a Start that
// inspected first and then did all three would print the same phases,
// and so would every total taken when Start returns. So this samples
// the call count INSIDE the client start, through the one seam that
// runs there, and then reads the same counter afterwards: zero at the
// socket, non-zero at the end, and the container's name in hand.
//
// The three assertions are one property and none of them is redundant.
// Zero-at-the-socket alone is satisfied by an attach that never asks
// at all, which would leave every container nameless. Non-zero-at-the-
// end alone is the old order. The name is what says the answer was
// used.
// ITS WINDOW OPENS AT Start, NOT AT Join. A Join that carries no hint
// rebuilds the endpoint before any attach begins and asks the daemon
// while doing it; that route is
// TestReacquireEndpoint_AsksTheDaemonBeforeTheAttachBegins, and the
// reference page and release notes both name it beside the claim.
func TestStart_AsksTheDaemonNothingBeforeThePersistentClientStarts(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"ctr-1": {EndpointID: "ep-abcdef"},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"ctr-1": {
				ContainerJSONBase: &dContainer.ContainerJSONBase{State: &dContainer.State{Pid: os.Getpid()}},
				Config:            &dContainer.Config{Hostname: "ctr-1"},
			},
		},
	}
	m, plug := daemonFreeManager(t, docker)

	// The observer sits IN the client, so the order is read at the
	// moment of the call and not inferred from what is left at the end.
	// m.ctrLink is assigned by locateContainerLink and by nothing else,
	// so "the link was already located" is a fact about this attach and
	// not about the fixture.
	watch := &firstCallWatcher{dockerClient: docker, m: m}
	m.docker = watch

	starts, callsAtSocket := 0, -1
	withStartedClient(t, func() {
		if starts == 0 {
			callsAtSocket = watch.calls
		}
		starts++
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !strings.Contains(m.startPhases, "open_netns=") {
		t.Errorf("phase summary %q has no open_netns: the namespace was not entered", m.startPhases)
	}
	if starts != 1 {
		t.Fatalf("the attach opened %d persistent client sockets, want 1: the sample below is taken at the "+
			"first one, and an attach that opened none never reached the instant this drive is about", starts)
	}
	if callsAtSocket != 0 {
		t.Errorf("the daemon had been asked %d times when the persistent client's socket was opened, want 0 "+
			"(list %d, network inspect %d, container inspect %d). Every daemon call can block for the "+
			"length of a ContainerStart (#406), and a container that waits for one before it leases is a "+
			"container with no address for the length of its own start (#961)",
			callsAtSocket, docker.listCalls, docker.inspectCalls, docker.containerCalls)
	}
	if watch.calls == 0 {
		t.Fatal("the daemon was never asked anything for the whole attach, so the container's name was " +
			"never looked up and no DHCP server will ever have a name for it")
	}
	if !watch.linkAtFirstCall {
		t.Errorf("the daemon's first call was made while m.ctrLink was still nil, so the link had not been "+
			"located yet and the namespace route still depends on the daemon (#417). list %d, network "+
			"inspect %d, container inspect %d",
			docker.listCalls, docker.inspectCalls, docker.containerCalls)
	}
	// The name is READ here and cannot be DELIVERED here: this lane
	// substitutes the socket open, so the manager still holds the
	// library client that never started, and the handover ends in
	// ErrNoRunningClient. Which is the assertion: the lookup ran and
	// answered after the client started (no lookup failure), and the
	// only thing that stopped the name was the absent socket (one apply
	// failure). The delivery itself is
	// TestStart_TheDefaultNetworkTakesTheNameAfterTheClientStarts,
	// which publishes a client that can be asked what it was told.
	if got := plug.hostnameLookupFailures.Load(); got != 0 {
		t.Errorf("hostname_lookup_failures = %d, want 0: the daemon answered, and an attach that never "+
			"asked for the name is not the attach this drive is about", got)
	}
	if got := plug.hostnameApplyFailures.Load(); got != 1 {
		t.Errorf("hostname_apply_failures = %d, want 1: the name was read after the client started and "+
			"had nowhere to go in this lane, which is the only reason it did not reach the wire", got)
	}
	if got := m.hostnameOnTheWire(); got != "" {
		t.Errorf("m.hostname = %q, want empty: the field is the name the endpoint puts on the wire, and "+
			"no client here ever took one", got)
	}
}

// withStartedClient substitutes the socket-opening seam with one that
// succeeds and calls at, so that an attach can be driven past the point
// this lane's privileges stop at.
//
// The event channel is the attach's own: the consumer goroutine ranges
// over it and is closed out at cleanup, so the substitute leaves the
// manager in the shape a real client start leaves it in rather than in
// a shape only this file produces.
func withStartedClient(t *testing.T, at func()) {
	t.Helper()
	events := make(chan dhcp.Event)
	prev := startDHCPClient
	startDHCPClient = func(*dhcp.DHCPClient) (chan dhcp.Event, error) {
		at()
		return events, nil
	}
	t.Cleanup(func() {
		startDHCPClient = prev
		close(events)
	})
}

// firstCallWatcher records the manager's state at the moment the daemon
// is first asked anything.
//
// The property is an ORDER, and a count at the end of Start cannot see
// one: an attach that inspected first and then found the link leaves
// exactly the same totals as an attach that did it the other way round.
// The three wrappers below cover the three calls Start makes today.
// Embedding the interface is what lets the rest compile, and it is also
// the hole: a call Start starts making later reaches the embedded
// client without passing note(), so a new call needs a wrapper here.
//
// "The link is located" is read as m.ctrLink != nil, and that equality
// is this fixture's, not the general one. Both assignments to the field
// are inside locateContainerLink; on the macvlan branch the only one
// runs when the search has succeeded, so the two coincide. The bridge
// branch assigns on every poll round, before its rename condition
// passes, so there the field is non-nil while location is still going
// on. This drive is macvlan and does not reach that.
type firstCallWatcher struct {
	dockerClient
	m               *dhcpManager
	calls           int
	linkAtFirstCall bool
}

func (w *firstCallWatcher) note() {
	if w.calls == 0 {
		w.linkAtFirstCall = w.m.ctrLink != nil
	}
	w.calls++
}

func (w *firstCallWatcher) NetworkList(ctx context.Context, options dNetwork.ListOptions) ([]dNetwork.Summary, error) {
	w.note()
	return w.dockerClient.NetworkList(ctx, options)
}

func (w *firstCallWatcher) NetworkInspect(ctx context.Context, networkID string, options dNetwork.InspectOptions) (dNetwork.Inspect, error) {
	w.note()
	return w.dockerClient.NetworkInspect(ctx, networkID, options)
}

func (w *firstCallWatcher) ContainerInspect(ctx context.Context, containerID string) (dContainer.InspectResponse, error) {
	w.note()
	return w.dockerClient.ContainerInspect(ctx, containerID)
}

// TestStart_TheClientOpensOnTheNameTheLinkHasAtOpenTime drives the gap
// between finding the link and using it.
//
// The engine moves the link into the sandbox and then renames it, and
// the macvlan branch of locateContainerLink takes the link the moment
// its MAC appears, which can be before that rename. Nothing orders the
// two, so the snapshot the locate leaves can carry a name the kernel no
// longer has, and a client opened on it fails: hosted run 34624582681
// opened one on a name that had already been replaced; the endpoint got
// no renewal client, and the address it had just declined was never
// replaced either. The index survives a rename, so re-reading by it is
// what makes the name current.
//
// THE FIXTURE RENAMES AT THE RE-READ ITSELF, which is what makes this a
// drive for the re-read and not for the locate: the located link's name
// is real and is asserted to be the other one, so a client opening on
// the renamed name can only be opening on the re-read's value.
//
// THE DAEMON IS ASSERTED NOT TO HAVE BEEN ASKED YET, and that is #961's
// half of this. It used to be the opposite -- the re-read had to follow
// the inspect, because the inspect sat between the locate and the open
// and was the interval the name went stale in. The inspect has moved to
// the far side of the client start, so a re-read that waits for the
// daemon is a socket that waits for the daemon.
//
// WHAT THIS CANNOT CATCH: a Start that re-reads the link and then opens
// the client on some other copy of it. It asserts the field, and the
// field is the one expression the open reads.
func TestStart_TheClientOpensOnTheNameTheLinkHasAtOpenTime(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"ctr-1": {EndpointID: "ep-abcdef"},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"ctr-1": {
				ContainerJSONBase: &dContainer.ContainerJSONBase{State: &dContainer.State{Pid: os.Getpid()}},
				Config:            &dContainer.Config{Hostname: "ctr-1"},
			},
		},
	}
	gate := &renameOnInspect{dockerClient: docker}
	m, _ := daemonFreeManager(t, gate)

	// A name no link in this namespace carries, so the assertion below
	// can only pass if the re-read happened after the rename.
	const renamed = "ep-abcdef-renamed"
	var (
		refreshes    int
		askedIndex   int
		locatedName  string
		locatedIndex int
		sawInspect   bool
	)
	prev := nlLinkByIndex
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		refreshes++
		askedIndex = index
		sawInspect = gate.inspected
		if m.ctrLink != nil {
			locatedName = m.ctrLink.Attrs().Name
			locatedIndex = m.ctrLink.Attrs().Index
		}
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{
			Index:        index,
			Name:         renamed,
			HardwareAddr: m.MacAddress,
		}}, nil
	}
	t.Cleanup(func() { nlLinkByIndex = prev })
	withStartedClient(t, func() {})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if refreshes == 0 {
		t.Fatal("the link was never re-read before the client was opened, so the name handed to the " +
			"client is the one locateContainerLink saw, however long ago that was (#417)")
	}
	if locatedName == renamed {
		t.Fatal("the located link already carried the renamed name, so this drive would pass without " +
			"the re-read: the fixture is not measuring anything")
	}
	if sawInspect {
		t.Error("the link was re-read after the daemon had been asked for the container, so the socket " +
			"that re-read precedes was waiting on the daemon too. The inspect belongs on the far side " +
			"of the client start (#961)")
	}
	if m.ctrLink == nil {
		t.Fatal("no link on the manager after Start: the re-read cannot be judged")
	}
	if got := m.ctrLink.Attrs().Name; got != renamed {
		t.Errorf("the client was opened on %q, but the link had been renamed to %q by then: a name read "+
			"when the link was located is a name the kernel may no longer have (#417)", got, renamed)
	}
	if askedIndex != locatedIndex {
		t.Errorf("the re-read asked for index %d and the located link is index %d: an index that is not "+
			"the located link's re-reads some other link", askedIndex, locatedIndex)
	}
}

// renameOnInspect records that the daemon has been asked for the
// hostname. The engine's rename of the link and this call are not
// ordered by anything in production; what the drive needs is a rename
// that lands inside the interval the reorder created, and the inspect
// is that interval.
type renameOnInspect struct {
	dockerClient
	inspected bool
}

func (r *renameOnInspect) ContainerInspect(ctx context.Context, id string) (dContainer.InspectResponse, error) {
	r.inspected = true
	return r.dockerClient.ContainerInspect(ctx, id)
}

// TestStart_LeasesWhileTheDaemonIsStillInsideContainerStart is the case
// #961 exists for, and it is the exact inversion of what this file
// asserted until #961: the same daemon, the same wait, and the opposite
// verdict on the client.
//
// The daemon here is the #406 daemon: it accepts the connection and
// never answers, because it is inside ContainerStart for this very
// container. The namespace opens, the link is found, AND THE PERSISTENT
// CLIENT STARTS -- all of it while that call is outstanding. Then the
// attach waits for the name and is abandoned at its budget, and that
// abandonment is not a failure: the container is leasing and what it
// lacks is its name in the server's table, which hostname_lookup_failures
// is the record of.
//
// attachDaemonBusyGrace stays load-bearing and the wait is still here;
// what changed is what the container holds while it waits.
func TestStart_LeasesWhileTheDaemonIsStillInsideContainerStart(t *testing.T) {
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
	withStartedClient(t, func() {})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start failed with %v while the daemon never answered ContainerInspect. The name is "+
			"the only thing that inspect supplies, and an endpoint without one is a working endpoint "+
			"(#961)", err)
	}

	if !strings.Contains(m.startPhases, "open_netns=") || !strings.Contains(m.startPhases, "locate_link=") {
		t.Errorf("phase summary %q: the namespace and the link must be reached before the wait, "+
			"or this test is measuring a failure that happened earlier", m.startPhases)
	}
	if !strings.Contains(m.startPhases, "start_clients=") {
		t.Errorf("phase summary %q has no start_clients, so the client did not start while the daemon "+
			"was unanswering. That wait is the whole of #406: the daemon does not answer until the "+
			"container start it is inside is finished, and a client that waits for it is a container "+
			"with no address for that whole time (#961)", m.startPhases)
	}
	if m.errChan == nil {
		t.Error("no persistent client on the manager: nothing is renewing this endpoint's lease")
	}
	if strings.Contains(m.startPhases, "hostname=") {
		t.Errorf("phase summary %q records a completed hostname phase, but the daemon never answered: "+
			"the phase is marked on the answer, so this one is being marked on the wait", m.startPhases)
	}
	// The assertions above say the client started. They do not say the
	// attach reached the name at all, and an attach that skipped the
	// lookup would satisfy every one of them while leaving every
	// container on this host nameless. These three say it was reached,
	// waited out, and recorded.
	if docker.containerCalls != 1 {
		t.Errorf("the daemon was asked to inspect the container %d times, want exactly 1: the attach "+
			"must reach the name lookup and wait there", docker.containerCalls)
	}
	if got := p.hostnameLookupFailures.Load(); got != 1 {
		t.Errorf("hostname_lookup_failures = %d, want 1: an endpoint leasing with no name in the DHCP "+
			"server's table is the one thing this outcome leaves behind, and without the counter it is "+
			"indistinguishable from an endpoint that was named", got)
	}
	if got := m.hostnameOnTheWire(); got != "" {
		t.Errorf("m.hostname = %q after a lookup that never answered: the name can only have come from "+
			"somewhere that did not decide it", got)
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

	inspectsAtSocket := -1
	withStartedClient(t, func() {
		if inspectsAtSocket < 0 {
			inspectsAtSocket = docker.containerCalls
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if inspectsAtSocket != 1 {
		t.Errorf("the container had been inspected %d times when the client's socket was opened, want 1. "+
			"This is #961's bound and not a violation of it: the PID route needed that answer to open "+
			"the namespace at all, so the name is already in hand and deferring it would put it on the "+
			"wire later for nothing", inspectsAtSocket)
	}

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
	if got := m.hostnameOnTheWire(); got != "ctr-1" {
		t.Errorf("m.hostname = %q, want %q: the inspect the fallback made must also be the one the "+
			"DHCP hostname option comes from", got, "ctr-1")
	}
	if got := p.hostnamesAppliedLate.Load(); got != 0 {
		t.Errorf("hostnames_applied_late = %d, want 0: the name was in the client's parameters from "+
			"construction on this route, and telling a client a name it is already sending is an "+
			"exchange nobody asked for", got)
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

// TestStart_AnEndpointNoContainerClaimsIsNotAStartFailure is the
// attribution the reorder took away and this drive puts back.
//
// A Join can name a real sandbox for an endpoint no container holds:
// #566's shape, and the shape a container disconnected from the network
// mid-attach leaves behind. The old order found that out first, because
// the container-ID poll ran before anything else and util.ErrNoContainer
// was the only way out. Reordered, the link lookup runs first and ends
// on its own deadline, which is not that error, so an endpoint nobody
// claimed was charged to join_start_failures: Healthy-affecting, and an
// operator paged about a container that does not exist.
//
// The drive is the error identity rather than the counter, because
// joinFailureLeavesAddressUnused keys on exactly that
// (network.go: errors.Is(err, util.ErrNoContainer)) and the counter is
// Join's to move. The integration cell TestJoinNoContainer_Address
// IsHeldUntilItExpires asserts the counters on a live daemon, and it is
// what caught this: it is green on a host that refuses the sandbox key
// and red on a host that takes it, because only the second one reaches
// the link lookup before the daemon is asked anything.
func TestStart_AnEndpointNoContainerClaimsIsNotAStartFailure(t *testing.T) {
	// No container holds this endpoint: the network answers, and its
	// container map has nothing with this endpoint id in it.
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"some-other-container": {EndpointID: "ep-somebody-else"},
			}},
		},
	}
	m, _ := daemonFreeManager(t, docker)

	// A MAC no link in this namespace carries, so the macvlan wait can
	// only end on its own deadline. Locally administered and unicast,
	// so it cannot collide with a real adapter.
	m.MacAddress = net.HardwareAddr{0x02, 0x00, 0x5e, 0x00, 0x53, 0x01}

	// The production cap is 30s inside a 70s attach window, which is
	// what leaves budget for the question below. Shrunk here so the
	// same two steps fit in a unit drive.
	prev := linkAwaitTimeout
	linkAwaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { linkAwaitTimeout = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded for an endpoint no container holds and a link that never appeared")
	}
	if !errors.Is(err, util.ErrNoContainer) {
		t.Errorf("Start returned %v, which is not util.ErrNoContainer. network.go decides "+
			"join_aborted_no_container against join_start_failures with errors.Is on exactly "+
			"that sentinel, so an endpoint nobody claimed is charged to the Healthy-affecting "+
			"counter and pages an operator about a container that does not exist (#566)", err)
	}
	if !strings.Contains(err.Error(), "no link for this endpoint in the sandbox") {
		t.Errorf("Start returned %v: it names the missing container but not the link lookup that "+
			"asked the question, so a reader cannot tell this from an endpoint whose container "+
			"was never created at all", err)
	}
	if strings.Contains(m.startPhases, "locate_link=") {
		t.Errorf("phase summary %q records locate_link for an attach whose link never appeared, "+
			"so the phase that consumed the budget is not the one the summary names", m.startPhases)
	}
	// The question is asked ONCE. It is asked at all only because the
	// link never appeared, and a link lookup that polls the daemon each
	// time round would turn the one call this attach can afford into as
	// many as the budget allows (#406).
	if docker.inspectCalls != 1 || docker.containerCalls != 0 {
		t.Errorf("the daemon was asked %d network inspect(s) and %d container inspect(s) for one "+
			"failed attach, want exactly one network inspect: the endpoint has no container, so "+
			"the question is answered by the first call and nothing after it can change the answer",
			docker.inspectCalls, docker.containerCalls)
	}
}
