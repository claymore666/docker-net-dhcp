// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/claymore666/dhcp-golib/wire"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// probeSeen is one DHCP message the fake server received, with the source MAC of the frame that carried it.
type probeSeen struct {
	typ     wire.MessageType
	srcMAC  net.HardwareAddr
	chaddr  net.HardwareAddr
	options wire.Options
}

// fakeProbeServer answers DISCOVER with an OFFER and REQUEST with an ACK over a packet socket on one link, and
// records what reached it (#1117).
type fakeProbeServer struct {
	fd       int
	serverID netip.Addr
	offer    netip.Addr
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	seen     []probeSeen
}

func startFakeProbeServer(t *testing.T, ifindex int, serverID, offer netip.Addr) *fakeProbeServer {
	t.Helper()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_IP)))
	if err != nil {
		t.Fatalf("packet socket: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifindex}); err != nil {
		t.Fatalf("bind the fake server to the link: %v", err)
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 50000}); err != nil {
		t.Fatalf("receive timeout: %v", err)
	}
	s := &fakeProbeServer{fd: fd, serverID: serverID, offer: offer,
		stop: make(chan struct{}), done: make(chan struct{})}
	go s.serve(ifindex)
	t.Cleanup(func() {
		close(s.stop)
		<-s.done
		_ = unix.Close(fd)
	})
	return s
}

func (s *fakeProbeServer) serve(ifindex int) {
	defer close(s.done)
	buf := make([]byte, 2048)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		n, from, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			continue
		}
		ll, ok := from.(*unix.SockaddrLinklayer)
		if !ok || ll.Pkttype == unix.PACKET_OUTGOING || n < 28 || buf[9] != unix.IPPROTO_UDP {
			continue
		}
		if binary.BigEndian.Uint16(buf[22:24]) != 67 {
			continue
		}
		req, err := wire.Decode(append([]byte(nil), buf[28:n]...))
		if err != nil {
			continue
		}
		typ, _ := req.Type()
		s.mu.Lock()
		s.seen = append(s.seen, probeSeen{typ: typ, srcMAC: append(net.HardwareAddr(nil), ll.Addr[:6]...),
			chaddr: append(net.HardwareAddr(nil), req.CHAddr...), options: req.Options.Clone()})
		s.mu.Unlock()
		switch typ {
		case wire.MsgDiscover:
			s.reply(ifindex, req, wire.MsgOffer)
		case wire.MsgRequest:
			s.reply(ifindex, req, wire.MsgAck)
		}
	}
}

func (s *fakeProbeServer) reply(ifindex int, req *wire.Message, typ wire.MessageType) {
	m := &wire.Message{Op: wire.BootReply, HType: wire.HTypeEthernet, XID: req.XID, Flags: wire.FlagBroadcast,
		YIAddr: s.offer, SIAddr: s.serverID, CHAddr: req.CHAddr, Options: wire.Options{}}
	m.SetType(typ)
	m.Options[wire.OptServerID] = s.serverID.AsSlice()
	m.Options[wire.OptSubnetMask] = []byte{255, 255, 255, 0}
	m.Options[wire.OptRouter] = s.serverID.AsSlice()
	m.Options[wire.OptLeaseTime] = []byte{0, 0, 0x0e, 0x10}
	payload, err := wire.Encode(m)
	if err != nil {
		return
	}
	frame, err := runtime.BuildIPv4UDP(s.serverID, netip.MustParseAddr("255.255.255.255"), 67, 68, 1, 64, payload)
	if err != nil {
		return
	}
	to := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifindex, Halen: 6,
		Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	_ = unix.Sendto(s.fd, frame, 0, to)
}

func (s *fakeProbeServer) received(typ wire.MessageType) []probeSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []probeSeen
	for _, m := range s.seen {
		if m.typ == typ {
			out = append(out, m)
		}
	}
	return out
}

// probeVethPair adds two up veth ends in the test's own namespace: the parent the probe runs on, and the end the fake
// server listens on.
func probeVethPair(t *testing.T, parent, server string) (netlink.Link, netlink.Link) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: parent}, PeerName: server}); err != nil {
		t.Fatalf("add the veth pair: %v", err)
	}
	var links [2]netlink.Link
	for i, name := range []string{parent, server} {
		l := mustLinkByName(t, name)
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatalf("up %s: %v", name, err)
		}
		links[i] = mustLinkByName(t, name)
	}
	return links[0], links[1]
}

// probeChildren lists the links the kernel holds as children of parent.
func probeChildren(t *testing.T, parent netlink.Link) []string {
	t.Helper()
	all, err := netlink.LinkList()
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	var out []string
	for _, l := range all {
		if l.Attrs().ParentIndex == parent.Attrs().Index && l.Type() == "macvlan" {
			out = append(out, l.Attrs().Name)
		}
	}
	return out
}

func probeNetnsPlugin(t *testing.T) *Plugin {
	t.Helper()
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	return p
}

const (
	probeParentName = "dh1117a"
	probeServerName = "dh1117b"
)

// TestRunDHCPProbe_AnAnsweringServerSeesADiscoverFromTheProbeAddressAndTheChildIsGone reads what the fake server
// received on the wire and what the kernel holds afterwards, not what the plugin reports (#1117).
func TestRunDHCPProbe_AnAnsweringServerSeesADiscoverFromTheProbeAddressAndTheChildIsGone(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p := probeNetnsPlugin(t)
	parent, server := probeVethPair(t, probeParentName, probeServerName)
	srv := startFakeProbeServer(t, server.Attrs().Index, netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.77"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeParentName}
	began := time.Now()
	if err := p.runDHCPProbe(ctx, opts, serverPolicy{}); err != nil {
		t.Fatalf("a reachable server answered and the probe failed: %v", err)
	}
	// RFC 5227 is off for the throwaway address: with it on, the same run takes about 7 s of the 8 s budget (#901).
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("the probe took %v against an answering server; the address conflict check must stay off (#901, #1117)", took)
	}

	discovers := srv.received(wire.MsgDiscover)
	if len(discovers) == 0 {
		t.Fatal("the server saw no DISCOVER: the probe sent nothing on the parent")
	}
	d := discovers[0]
	if len(d.chaddr) != 6 || d.chaddr[0]&0x02 == 0 || d.chaddr[0]&0x01 != 0 {
		t.Errorf("DISCOVER chaddr %v is not a locally administered unicast MAC; a manufacturer-assigned address "+
			"could match a reservation on the real server (#307)", d.chaddr)
	}
	if d.srcMAC.String() != d.chaddr.String() {
		t.Errorf("the frame came from %v but carries chaddr %v: the probe must speak as its own child", d.srcMAC, d.chaddr)
	}
	if d.srcMAC.String() == parent.Attrs().HardwareAddr.String() {
		t.Errorf("the DISCOVER wore the parent's MAC %v: the probe is meant to be a throwaway identity", d.srcMAC)
	}
	// Measured: the probe sends the library's default vendor class (option 60), so its comment's "no vendor class" is
	// wrong; no hostname and no client id do go out (#1117).
	for _, c := range []wire.OptionCode{wire.OptHostName, wire.OptClientID} {
		if _, ok := d.options[c]; ok {
			t.Errorf("DISCOVER carries option %d: the probe is identity-neutral so class-based policy cannot deny it (#307)", c)
		}
	}
	if got := string(d.options[wire.OptVendorClassID]); got != dhcp.VendorID {
		t.Errorf("DISCOVER vendor class %q, want the default %q", got, dhcp.VendorID)
	}
	if len(srv.received(wire.MsgRequest)) == 0 {
		t.Error("the server saw no REQUEST: the probe stopped at the OFFER, and validate_dhcp reports success on a DORA")
	}
	if got := probeChildren(t, parent); len(got) != 0 {
		t.Errorf("the kernel still holds probe children %v after the probe returned (#577)", got)
	}
}

// TestRunDHCPProbe_ADeniedServerIsNeverRequestedAndTheErrorNamesTheParent drives the policy the probe hands the
// client with a server that answers: the OFFER arrives and the client must not take it (#111, #669).
func TestRunDHCPProbe_ADeniedServerIsNeverRequestedAndTheErrorNamesTheParent(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p := probeNetnsPlugin(t)
	parent, server := probeVethPair(t, probeParentName, probeServerName)
	denied := netip.MustParseAddr("192.0.2.1")
	srv := startFakeProbeServer(t, server.Attrs().Index, denied, netip.MustParseAddr("192.0.2.77"))

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeParentName}
	err := p.runDHCPProbe(ctx, opts, serverPolicy{Deny: []netip.Addr{denied}})
	if err == nil {
		t.Fatal("the only server on the wire is denied and the probe reported success")
	}
	if !strings.Contains(err.Error(), "no DHCP OFFER on \""+probeParentName+"\"") {
		t.Errorf("error %q does not name the parent NIC and the missing OFFER: an operator reads this to find the cause", err)
	}
	if len(srv.received(wire.MsgDiscover)) == 0 {
		t.Error("the server saw no DISCOVER, so the deny list was never put to the test")
	}
	if got := srv.received(wire.MsgRequest); len(got) != 0 {
		t.Errorf("the probe REQUESTed from a denied server %d time(s)", len(got))
	}
	if got := probeChildren(t, parent); len(got) != 0 {
		t.Errorf("the kernel still holds probe children %v after a failed probe (#577)", got)
	}
}

// TestRunDHCPProbe_AServerOutsideThePreferListIsNeverRequested is the allow side of the policy: the only server on
// the wire is not the one the operator named, so its OFFER must not be taken (#111, #669, #1117).
func TestRunDHCPProbe_AServerOutsideThePreferListIsNeverRequested(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p := probeNetnsPlugin(t)
	parent, server := probeVethPair(t, probeParentName, probeServerName)
	srv := startFakeProbeServer(t, server.Attrs().Index, netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.77"))

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeParentName}
	err := p.runDHCPProbe(ctx, opts, serverPolicy{Prefer: []netip.Addr{netip.MustParseAddr("192.0.2.9")}})
	if err == nil {
		t.Fatal("the only server on the wire is not on the prefer list and the probe reported success")
	}
	if len(srv.received(wire.MsgDiscover)) == 0 {
		t.Error("the server saw no DISCOVER, so the prefer list was never put to the test")
	}
	if got := srv.received(wire.MsgRequest); len(got) != 0 {
		t.Errorf("the probe REQUESTed from a server outside the prefer list %d time(s)", len(got))
	}
	if got := probeChildren(t, parent); len(got) != 0 {
		t.Errorf("the kernel still holds probe children %v after a failed probe (#577)", got)
	}
}

// TestRunDHCPProbe_ASilentSegmentFailsWithTheIsolationHintAndLeavesNoChild covers the arm a firewalled or VLAN-mistagged
// parent takes (#307).
func TestRunDHCPProbe_ASilentSegmentFailsWithTheIsolationHintAndLeavesNoChild(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p := probeNetnsPlugin(t)
	parent, _ := probeVethPair(t, probeParentName, probeServerName)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := p.runDHCPProbe(ctx, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeParentName}, serverPolicy{})
	if err == nil {
		t.Fatal("no server exists on the segment and the probe reported success")
	}
	for _, want := range []string{"no DHCP OFFER on \"" + probeParentName + "\"", "isolated, firewalled", "VLAN-tagged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if got := probeChildren(t, parent); len(got) != 0 {
		t.Errorf("the kernel still holds probe children %v after a timed-out probe (#577)", got)
	}
}
