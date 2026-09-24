// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The engine moves, renames and addresses the sandbox link while it is down and sets it up last (#1089). A raw
// socket opened in that window reads ENETDOWN once, and the client library then stops reading for good.
func TestStart_ThePersistentClientOpensOnlyOnceTheEngineSetTheLinkUp(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, tc := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan}},
		{"bridge", DHCPNetworkOptions{Mode: ModeBridge}},
		{"macvlan with DHCPv6", DHCPNetworkOptions{Mode: ModeMacvlan, IPv6: true}},
	} {
		t.Run(tc.name, func(t *testing.T) { persistentClientAfterLinkUp(t, tc.opts) })
	}
}

func persistentClientAfterLinkUp(t *testing.T, opts DHCPNetworkOptions) {
	m, server, ctr := managerOnADownLink(t, opts)

	// The client's socket is opened where Start hands over to the client, as the chassis opens it there.
	var (
		upAtOpen  []bool
		transport *runtime.PacketTransport
		openErr   error
	)
	withStartedClient(t, func() {
		upAtOpen = append(upAtOpen, mustLinkByName(t, ctr).Attrs().Flags&net.FlagUp != 0)
		if transport == nil && openErr == nil {
			transport, openErr = runtime.NewPacketTransport(ctr)
		}
	})
	t.Cleanup(func() {
		if transport != nil {
			_ = transport.Close()
		}
	})

	started := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { started <- m.Start(ctx) }()

	var startErr error
	returned := false
	select {
	case startErr = <-started:
		returned = true
		t.Errorf("Start returned (%v) while the engine still held the link down: the client was handed a link "+
			"its socket cannot read from (#1089)", startErr)
	case <-time.After(5 * pollTime):
	}
	if err := netlink.LinkSetUp(mustLinkByName(t, ctr)); err != nil {
		t.Fatalf("set the container end up as the engine does last: %v", err)
	}
	if !returned {
		select {
		case startErr = <-started:
		case <-ctx.Done():
			t.Fatal("Start did not return after the link came up")
		}
	}
	if startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	if openErr != nil || transport == nil {
		t.Fatalf("opening the client's socket at the hand-over: %v", openErr)
	}
	wantClients := 1
	if opts.IPv6 {
		wantClients = 2
	}
	if len(upAtOpen) != wantClients {
		t.Fatalf("%d persistent clients started, want %d", len(upAtOpen), wantClients)
	}
	for i, up := range upAtOpen {
		if !up {
			t.Errorf("persistent client %d was started on %s while it was down (#1089)", i+1, ctr)
		}
	}

	sendServerBroadcast(t, server)
	select {
	case in := <-transport.Received():
		if in.Err != nil {
			t.Fatalf("the client's socket read %v instead of the server's answer, and the library reads "+
				"nothing after a read error: every ACK would go unread (#1089)", in.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the server's broadcast never reached the client's socket (stats %+v)", transport.Stats())
	}
}

// A link that stays down past the bound must fail the attach, which the callers count as join_start_failures or
// recovery_failed, and must not leave a client that sends but cannot read (#1089).
func TestStart_ALinkThatStaysDownFailsTheAttachAndStartsNoClient(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	prev := linkAwaitTimeout
	linkAwaitTimeout = 3 * pollTime
	t.Cleanup(func() { linkAwaitTimeout = prev })

	m, _, ctr := managerOnADownLink(t, DHCPNetworkOptions{Mode: ModeMacvlan})
	starts := 0
	withStartedClient(t, func() { starts++ })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	began := time.Now()
	err := m.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), ctr) {
		t.Fatalf("Start on a link that never came up returned %v, want an error naming %s", err, ctr)
	}
	if took := time.Since(began); took > linkAwaitTimeout+2*time.Second {
		t.Errorf("Start gave up after %s, past its bound of %s", took, linkAwaitTimeout)
	}
	if err := netlink.LinkSetUp(mustLinkByName(t, ctr)); err != nil {
		t.Fatalf("set the link up after the bound: %v", err)
	}
	time.Sleep(3 * pollTime)
	if starts != 0 {
		t.Errorf("%d persistent clients started for an attach that failed", starts)
	}
}

// The kernel only fails a raw socket's read on a link that is not administratively up, so a link that is up with no
// carrier yet must not hold the attach until the bound (#1089).
func TestStart_ALinkThatIsUpWithoutCarrierStartsTheClientAtOnce(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	prev := linkAwaitTimeout
	linkAwaitTimeout = 3 * pollTime
	t.Cleanup(func() { linkAwaitTimeout = prev })

	m, server, ctr := managerOnADownLink(t, DHCPNetworkOptions{Mode: ModeMacvlan})
	peer, err := netlink.LinkByIndex(server)
	if err != nil {
		t.Fatalf("read the server end: %v", err)
	}
	if err := netlink.LinkSetDown(peer); err != nil {
		t.Fatalf("take the carrier away: %v", err)
	}
	if err := netlink.LinkSetUp(mustLinkByName(t, ctr)); err != nil {
		t.Fatalf("set the container end up: %v", err)
	}
	if attrs := mustLinkByName(t, ctr).Attrs(); attrs.Flags&net.FlagRunning != 0 {
		t.Fatalf("%s reports a carrier (%v), so the case cannot be driven here. This is the instrument failing",
			ctr, attrs.Flags)
	}
	starts := 0
	withStartedClient(t, func() { starts++ })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start on a link that is up without carrier: %v", err)
	}
	if starts != 1 {
		t.Errorf("%d persistent clients started, want 1", starts)
	}
}

// A link that goes away while the plugin waits for it must fail the attach, not count as up (#1089).
func TestStart_ALinkThatVanishesWhileDownFailsTheAttachAndStartsNoClient(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	m, _, ctr := managerOnADownLink(t, DHCPNetworkOptions{Mode: ModeMacvlan})
	starts := 0
	withStartedClient(t, func() { starts++ })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- m.Start(ctx) }()
	time.Sleep(5 * pollTime)
	if err := netlink.LinkDel(mustLinkByName(t, ctr)); err != nil {
		t.Fatalf("remove the container end: %v", err)
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("Start succeeded on a link that was removed before it came up")
		}
	case <-ctx.Done():
		t.Fatal("Start did not return after the link was removed")
	}
	if starts != 0 {
		t.Errorf("%d persistent clients started on a link that was removed", starts)
	}
}

// The engine renames the link after its move and before its LinkSetUp, and a macvlan link is found by MAC, so the
// client must be handed the name the link has once it is up (#1089).
func TestStart_TheClientIsHandedTheNameTheLinkHasOnceUp(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	m, _, created := managerOnALink(t, DHCPNetworkOptions{Mode: ModeMacvlan}, false)
	var handed []string
	withStartedClient(t, func() { handed = append(handed, m.ctrLink.Attrs().Name) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- m.Start(ctx) }()
	time.Sleep(5 * pollTime)
	const final = "ctr1089"
	link := mustLinkByName(t, created)
	if err := netlink.LinkSetName(link, final); err != nil {
		t.Fatalf("rename the container end as the engine does: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("set the container end up: %v", err)
	}
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Start did not return after the link came up")
	}
	if len(handed) != 1 || handed[0] != final {
		t.Errorf("the client was handed %q, want the link's name once up, %q", handed, final)
	}
}

// managerOnADownLink builds the sandbox the engine leaves before its LinkSetUp: a veth whose container end is renamed
// and down (#1089). It returns the server end's index and the container end's name.
func managerOnADownLink(t *testing.T, opts DHCPNetworkOptions) (*dhcpManager, int, string) {
	t.Helper()
	return managerOnALink(t, opts, true)
}

// managerOnALink leaves the container end under its creation name when renamed is false, as the engine does between
// its move and its rename (#1089).
func managerOnALink(t *testing.T, opts DHCPNetworkOptions, renamed bool) (*dhcpManager, int, string) {
	t.Helper()
	endpointID := "ep1089" + opts.Mode[:3]
	hostName, oldCtrName := vethPairNames(endpointID)
	const ctrName = "ctr1089"
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName}, PeerName: oldCtrName}); err != nil {
		t.Fatalf("create the veth pair in the test's own namespace: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostName}}) })
	server := mustLinkByName(t, hostName)
	if err := netlink.LinkSetUp(server); err != nil {
		t.Fatalf("set the server end up: %v", err)
	}
	name := oldCtrName
	if renamed {
		if err := netlink.LinkSetName(mustLinkByName(t, oldCtrName), ctrName); err != nil {
			t.Fatalf("rename the container end as the engine does: %v", err)
		}
		name = ctrName
	}
	ctr := mustLinkByName(t, name)
	if ctr.Attrs().Flags&net.FlagUp != 0 {
		t.Fatal("the container end is already up, so the engine's window cannot be driven here. This is the " +
			"instrument failing and not the property being absent")
	}

	dir := t.TempDir()
	key := filepath.Join(dir, "1a2b3c4d5e6f")
	if err := linkANetnsEntry(key); err != nil {
		t.Fatalf("link fixture entry: %v", err)
	}
	withSandboxNetnsDirs(t, []string{dir})
	daemonDown := errors.New("daemon is not answering anything")
	m := newDHCPManager(&fakeDocker{listErr: daemonDown, inspectErr: daemonDown, containerErr: daemonDown},
		JoinRequest{NetworkID: "net-1089", EndpointID: endpointID, SandboxKey: key}, opts).withPlugin(&Plugin{})
	m.MacAddress = ctr.Attrs().HardwareAddr
	withNetlinkHandleInThisNamespace(t, m)
	t.Cleanup(func() {
		closeNetHandle(m.netHandle)
		closeNsHandle(m.nsHandle)
	})
	return m, server.Attrs().Index, name
}

func mustLinkByName(t *testing.T, name string) netlink.Link {
	t.Helper()
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("link %s: %v", name, err)
	}
	return link
}

// sendServerBroadcast sends one UDP 67 -> 68 broadcast, the shape of a dnsmasq ACK to a client without an address.
func sendServerBroadcast(t *testing.T, ifindex int) {
	t.Helper()
	frame, err := runtime.BuildIPv4UDP(netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("255.255.255.255"), 67, 68,
		1, 64, []byte("ack"))
	if err != nil {
		t.Fatalf("build frame: %v", err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_IP)))
	if err != nil {
		t.Fatalf("packet socket: %v", err)
	}
	defer unix.Close(fd)
	to := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifindex, Halen: 6,
		Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	if err := unix.Sendto(fd, frame, 0, to); err != nil {
		t.Fatalf("send the broadcast: %v", err)
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
