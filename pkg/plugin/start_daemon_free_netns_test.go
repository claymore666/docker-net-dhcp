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

// daemonFreeManager drives #417 and #961: via the sandbox key, Start opens the netns, finds the link and starts the
// client with no daemon call. With register_dns the client waits for the name, since option 81 (RFC 4702) is fixed
// at construction.
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

// withNetlinkHandleInThisNamespace replaces netlink.NewHandleAt, whose setns needs CAP_SYS_ADMIN even for the
// caller's own namespace (#961).
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

func TestStart_EntersTheNamespaceAndLocatesTheLinkWithoutTheDaemon(t *testing.T) {
	daemonDown := errors.New("daemon is not answering anything")
	m, p := daemonFreeManager(t, &fakeDocker{
		listErr:      daemonDown,
		inspectErr:   daemonDown,
		containerErr: daemonDown,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
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

// TestStart_TheClientOpensOnTheNameTheLinkHasAtOpenTime: the engine renames the link after moving it, so the client
// re-reads it by index (#417).
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

type renameOnInspect struct {
	dockerClient
	inspected bool
}

func (r *renameOnInspect) ContainerInspect(ctx context.Context, id string) (dContainer.InspectResponse, error) {
	r.inspected = true
	return r.dockerClient.ContainerInspect(ctx, id)
}

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

func TestStart_AnEndpointNoContainerClaimsIsNotAStartFailure(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"some-other-container": {EndpointID: "ep-somebody-else"},
			}},
		},
	}
	m, _ := daemonFreeManager(t, docker)

	m.MacAddress = net.HardwareAddr{0x02, 0x00, 0x5e, 0x00, 0x53, 0x01}

	// Production caps this wait at 30 s inside a 70 s attach window.
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
	// Asked once, only after the link never appeared (#406).
	if docker.inspectCalls != 1 || docker.containerCalls != 0 {
		t.Errorf("the daemon was asked %d network inspect(s) and %d container inspect(s) for one "+
			"failed attach, want exactly one network inspect: the endpoint has no container, so "+
			"the question is answered by the first call and nothing after it can change the answer",
			docker.inspectCalls, docker.containerCalls)
	}
}
