// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
	dContainer "github.com/docker/docker/api/types/container"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// fastACD keeps RFC 5227's shape at a millisecond scale.
func fastACD() proto.ACDParams {
	acd := proto.DefaultACDParams()
	acd.ProbeWait = 2 * proto.Millisecond
	acd.ProbeMin = 2 * proto.Millisecond
	acd.ProbeMax = 4 * proto.Millisecond
	acd.AnnounceWait = 5 * proto.Millisecond
	acd.AnnounceInterval = 2 * proto.Millisecond
	return acd
}

type fakeARPLink struct {
	mac    net.HardwareAddr
	in     chan lease.ARPInbound
	onSend func(f *fakeARPLink, pkt *wire.ARPPacket)

	mu     sync.Mutex
	sent   []*wire.ARPPacket
	closed bool
}

func newFakeARPLink(mac string) *fakeARPLink {
	hw, _ := net.ParseMAC(mac)
	return &fakeARPLink{mac: hw, in: make(chan lease.ARPInbound, 64)}
}

func (f *fakeARPLink) Send(frame []byte) error {
	pkt, err := wire.DecodeARP(frame)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.sent = append(f.sent, pkt)
	f.mu.Unlock()
	if f.onSend != nil {
		f.onSend(f, pkt)
	}
	return nil
}

func (f *fakeARPLink) Received() <-chan lease.ARPInbound { return f.in }
func (f *fakeARPLink) HardwareAddr() net.HardwareAddr    { return f.mac }
func (f *fakeARPLink) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeARPLink) inject(t *testing.T, pkt *wire.ARPPacket) {
	t.Helper()
	b, err := wire.EncodeARP(pkt)
	if err != nil {
		t.Fatalf("EncodeARP: %v", err)
	}
	f.in <- lease.ARPInbound{Frame: b}
}

func (f *fakeARPLink) sentFrames() []*wire.ARPPacket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*wire.ARPPacket(nil), f.sent...)
}

var otherHW = net.HardwareAddr{0x02, 0, 0, 0, 0xbe, 0xef}

// defendsOn answers every probe for addr as the holder would, with an ARP reply from another host.
func defendsOn(t *testing.T, addr netip.Addr) func(f *fakeARPLink, pkt *wire.ARPPacket) {
	return func(f *fakeARPLink, pkt *wire.ARPPacket) {
		if pkt.IsProbe() && pkt.TargetIP == addr {
			f.inject(t, &wire.ARPPacket{Op: wire.ARPReply, SenderHW: otherHW, TargetHW: pkt.SenderHW,
				SenderIP: addr, TargetIP: netip.IPv4Unspecified()})
		}
	}
}

func sequence(addrs ...string) (func() netip.Addr, *int) {
	n := 0
	return func() netip.Addr {
		a := netip.MustParseAddr(addrs[n%len(addrs)])
		n++
		return a
	}, &n
}

func TestLinkLocalPicker_DrawsOnlyFromRFC3927sRangeAndRepeatsPerMAC(t *testing.T) {
	first, last := netip.MustParseAddr("169.254.1.0"), netip.MustParseAddr("169.254.254.255")
	macA, _ := net.ParseMAC("02:42:c0:a8:63:0a")
	macB, _ := net.ParseMAC("02:42:c0:a8:63:0b")
	a, again, b := newLinkLocalPicker(macA), newLinkLocalPicker(macA), newLinkLocalPicker(macB)
	sawFirstBlock, sawLastBlock := false, false
	same, differ := 0, 0
	for i := 0; i < 200000; i++ {
		x := a()
		if x.Less(first) || last.Less(x) {
			t.Fatalf("draw %d is %v, outside 169.254.1.0-169.254.254.255 (RFC 3927 section 2.1)", i, x)
		}
		b4 := x.As4()
		sawFirstBlock = sawFirstBlock || b4[2] == 1
		sawLastBlock = sawLastBlock || b4[2] == 254
		if y := again(); y == x {
			same++
		}
		if z := b(); z != x {
			differ++
		}
	}
	if !sawFirstBlock || !sawLastBlock {
		t.Errorf("200000 draws never reached 169.254.1.x (%v) or 169.254.254.x (%v)", sawFirstBlock, sawLastBlock)
	}
	if same != 200000 {
		t.Errorf("one MAC drew a different sequence on %d of 200000 draws; RFC 3927 section 2.1 wants it repeatable", 200000-same)
	}
	if differ < 199000 {
		t.Errorf("two MACs drew the same address on %d of 200000 draws", 200000-differ)
	}
}

func TestLinkLocalConflict_IsRFC5227s211Rules(t *testing.T) {
	cand := netip.MustParseAddr("169.254.7.7")
	own, _ := net.ParseMAC("02:00:00:00:00:01")
	zero := netip.IPv4Unspecified()
	for _, tc := range []struct {
		name string
		pkt  wire.ARPPacket
		want bool
	}{
		{"a reply from the holder", wire.ARPPacket{Op: wire.ARPReply, SenderHW: otherHW, SenderIP: cand, TargetIP: zero}, true},
		{"the holder's own announcement", wire.ARPPacket{Op: wire.ARPRequest, SenderHW: otherHW, SenderIP: cand, TargetIP: cand}, true},
		{"another host probing the same address", wire.ARPPacket{Op: wire.ARPRequest, SenderHW: otherHW, SenderIP: zero, TargetIP: cand}, true},
		{"our own probe echoed back", wire.ARPPacket{Op: wire.ARPRequest, SenderHW: own, SenderIP: zero, TargetIP: cand}, false},
		{"our own announcement echoed back", wire.ARPPacket{Op: wire.ARPRequest, SenderHW: own, SenderIP: cand, TargetIP: cand}, false},
		{"a request for the address from a host that holds another", wire.ARPPacket{Op: wire.ARPRequest, SenderHW: otherHW, SenderIP: netip.MustParseAddr("169.254.9.9"), TargetIP: cand}, false},
		{"a probe for another address", wire.ARPPacket{Op: wire.ARPRequest, SenderHW: otherHW, SenderIP: zero, TargetIP: netip.MustParseAddr("169.254.7.8")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkt := tc.pkt
			pkt.TargetHW = make([]byte, 6)
			if got := linkLocalConflict(&pkt, cand, own); got != tc.want {
				t.Errorf("conflict = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLinkLocalEligible_OnlyAnAttemptThatRanOutFallsBack(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"the lease_timeout ran out", live, fmt.Errorf("acquire: %w", context.DeadlineExceeded), true},
		{"the client ended with no lease", live, fmt.Errorf("acquire: %w", dhcp.ErrNoLease), true},
		{"the socket could not open", live, errors.New("open packet socket: permission denied"), false},
		{"the engine gave up on the request", cancelled, context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkLocalEligible(tc.ctx, tc.err); got != tc.want {
				t.Errorf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateLinkLocalFallback_RefusesWhatItCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     DHCPNetworkOptions
		sentinel error
		want     string
	}{
		{name: "bridge", opts: DHCPNetworkOptions{LinkLocalFallback: true, Bridge: "br0"}},
		{name: "macvlan", opts: DHCPNetworkOptions{LinkLocalFallback: true, Mode: ModeMacvlan, Parent: "eth0"}},
		{name: "lease_timeout at the ceiling", opts: DHCPNetworkOptions{LinkLocalFallback: true, LeaseTimeout: 16 * time.Second}},
		{name: "ipvlan without the option", opts: DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0", LeaseTimeout: time.Minute}},
		{name: "ipvlan", opts: DHCPNetworkOptions{LinkLocalFallback: true, Mode: ModeIPvlan, Parent: "eth0"},
			sentinel: util.ErrModeMismatch, want: "does not receive the ARP replies"},
		{name: "ipv6_mode=dhcp", opts: DHCPNetworkOptions{LinkLocalFallback: true, IPv6Mode: "dhcp"},
			sentinel: util.ErrModeMismatch, want: "ipv6_mode=dhcp"},
		{name: "ipv6=true", opts: DHCPNetworkOptions{LinkLocalFallback: true, IPv6: true},
			sentinel: util.ErrModeMismatch, want: "IPv4 only"},
		{name: "lease_timeout over the ceiling", opts: DHCPNetworkOptions{LinkLocalFallback: true, LeaseTimeout: 16*time.Second + time.Millisecond},
			sentinel: util.ErrIPAM, want: "Set lease_timeout to 16s or less"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLinkLocalFallback(tc.opts)
			if tc.sentinel == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.sentinel) || !strings.Contains(fmt.Sprint(err), tc.want) {
				t.Fatalf("err = %v, want %v naming %q", err, tc.sentinel, tc.want)
			}
		})
	}
}

func TestIPAMRefuseLinkLocal_NamesTheNullDriver(t *testing.T) {
	if err := ipamRefuseLinkLocal(DHCPNetworkOptions{}); err != nil {
		t.Fatalf("refused a network without the option: %v", err)
	}
	err := ipamRefuseLinkLocal(DHCPNetworkOptions{LinkLocalFallback: true})
	if !errors.Is(err, util.ErrIPAM) || !strings.Contains(fmt.Sprint(err), "--ipam-driver null") {
		t.Fatalf("err = %v, want ErrIPAM naming --ipam-driver null", err)
	}
}

func TestLinkLocalBudget_AClaimAfterTheDHCPDeadlineEndsBeforeTheEngines(t *testing.T) {
	acd := proto.DefaultACDParams()
	if got, want := linkLocalWindow(acd), 9*time.Second; got != want {
		t.Errorf("claim window = %v, want %v: RFC 5227's 7 s probe window plus one 2 s announce interval", got, want)
	}
	start := time.Now()
	if got := linkLocalClaimDeadline(start).Sub(start); got != pluginCallBudget-pluginCallMargin {
		t.Errorf("claim deadline %v after the call, want %v", got, pluginCallBudget-pluginCallMargin)
	}
	if got := linkLocalDHCPDeadline(start).Add(linkLocalDrain + linkLocalWindow(acd)); !got.Equal(linkLocalClaimDeadline(start)) {
		t.Errorf("DHCP deadline plus the drain and one window = %v, want the claim deadline %v", got.Sub(start), linkLocalClaimDeadline(start).Sub(start))
	}
	if linkLocalLeaseTimeout != 16*time.Second {
		t.Errorf("linkLocalLeaseTimeout = %v, want 30 - 4 - 9 - 1 = 16s", linkLocalLeaseTimeout)
	}
	for _, tc := range []struct {
		opts DHCPNetworkOptions
		want time.Duration
	}{
		{DHCPNetworkOptions{}, defaultLeaseTimeout},
		{DHCPNetworkOptions{LinkLocalFallback: true}, linkLocalLeaseTimeout},
		{DHCPNetworkOptions{LinkLocalFallback: true, LeaseTimeout: 8 * time.Second}, 8 * time.Second},
		{DHCPNetworkOptions{LeaseTimeout: 60 * time.Second}, 60 * time.Second},
	} {
		if got := leaseTimeoutFor(tc.opts); got != tc.want {
			t.Errorf("leaseTimeoutFor(fallback=%v, lease_timeout=%v) = %v, want %v",
				tc.opts.LinkLocalFallback, tc.opts.LeaseTimeout, got, tc.want)
		}
	}
}

func TestClaimLinkLocal_AFreeAddressIsProbedThenAnnounced(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	next, drawn := sequence("169.254.10.1")
	acd := fastACD()
	link.onSend = func(f *fakeARPLink, pkt *wire.ARPPacket) {
		// A frame of our own echoed back is not a conflict (RFC 5227 section 2.1.1).
		f.inject(t, pkt)
	}
	addr, tried, err := claimLinkLocal(context.Background(), link, acd, next)
	if err != nil || addr != netip.MustParseAddr("169.254.10.1") || tried != 1 || *drawn != 1 {
		t.Fatalf("claim = %v, %d tried, %d drawn, %v; want 169.254.10.1 on the first draw", addr, tried, *drawn, err)
	}
	sent := link.sentFrames()
	if len(sent) != acd.ProbeNum+acd.AnnounceNum {
		t.Fatalf("sent %d frames, want %d probes and %d announcements", len(sent), acd.ProbeNum, acd.AnnounceNum)
	}
	for i, p := range sent {
		wantSender := netip.IPv4Unspecified()
		if i >= acd.ProbeNum {
			wantSender = addr
		}
		if p.Op != wire.ARPRequest || p.SenderIP != wantSender || p.TargetIP != addr || net.HardwareAddr(p.SenderHW).String() != link.mac.String() {
			t.Errorf("frame %d = op %v sender %v target %v hw %v; want a request from %v for %v", i, p.Op, p.SenderIP, p.TargetIP, net.HardwareAddr(p.SenderHW), wantSender, addr)
		}
	}
}

func TestClaimLinkLocal_ADefendedAddressIsSkippedForTheNext(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	taken := netip.MustParseAddr("169.254.10.1")
	link.onSend = defendsOn(t, taken)
	next, _ := sequence("169.254.10.1", "169.254.10.2")
	addr, tried, err := claimLinkLocal(context.Background(), link, fastACD(), next)
	if err != nil || addr != netip.MustParseAddr("169.254.10.2") || tried != 2 {
		t.Fatalf("claim = %v, %d tried, %v; want 169.254.10.2 on the second candidate", addr, tried, err)
	}
	for _, p := range link.sentFrames() {
		if p.SenderIP == taken {
			t.Errorf("announced the defended %v", taken)
		}
	}
}

func TestClaimLinkLocal_AProbeFromAnotherHostIsAConflictBeforeOurFirstProbe(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	link.inject(t, &wire.ARPPacket{Op: wire.ARPRequest, SenderHW: otherHW, TargetHW: make([]byte, 6),
		SenderIP: netip.IPv4Unspecified(), TargetIP: netip.MustParseAddr("169.254.10.1")})
	next, _ := sequence("169.254.10.1", "169.254.10.2")
	acd := fastACD()
	acd.ProbeWait = 50 * proto.Millisecond
	addr, tried, err := claimLinkLocal(context.Background(), link, acd, next)
	if err != nil || addr != netip.MustParseAddr("169.254.10.2") || tried != 2 {
		t.Fatalf("claim = %v, %d tried, %v; want the second candidate", addr, tried, err)
	}
}

func TestClaimLinkLocal_StopsAtMaxConflicts(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	link.onSend = func(f *fakeARPLink, pkt *wire.ARPPacket) { defendsOn(t, pkt.TargetIP)(f, pkt) }
	next, drawn := sequence("169.254.10.1", "169.254.10.2", "169.254.10.3")
	acd := fastACD()
	// Bounded like every real claim, so one that ignores MAX_CONFLICTS fails here instead of hanging (#904).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, tried, err := claimLinkLocal(ctx, link, acd, next)
	if !errors.Is(err, ErrLinkLocalTooManyConflicts) || tried != acd.MaxConflicts || *drawn != acd.MaxConflicts {
		t.Fatalf("err = %v after %d tried and %d drawn, want ErrLinkLocalTooManyConflicts after %d",
			err, tried, *drawn, acd.MaxConflicts)
	}
}

func TestClaimLinkLocal_NoWholeWindowLeftProbesNothing(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	acd := fastACD()
	ctx, cancel := context.WithTimeout(context.Background(), linkLocalWindow(acd)/2)
	defer cancel()
	next, drawn := sequence("169.254.10.1")
	_, tried, err := claimLinkLocal(ctx, link, acd, next)
	if !errors.Is(err, ErrLinkLocalNoTime) || tried != 0 || *drawn != 0 || len(link.sentFrames()) != 0 {
		t.Fatalf("err = %v, %d tried, %d drawn, %d sent; want ErrLinkLocalNoTime before any probe",
			err, tried, *drawn, len(link.sentFrames()))
	}
}

func TestClaimLinkLocal_TheSecondCandidateAlsoNeedsAWholeWindow(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	taken := netip.MustParseAddr("169.254.10.1")
	acd := fastACD()
	acd.AnnounceWait = 40 * proto.Millisecond
	probes := 0
	link.onSend = func(f *fakeARPLink, pkt *wire.ARPPacket) {
		if pkt.IsProbe() && pkt.TargetIP == taken {
			if probes++; probes == acd.ProbeNum {
				// The holder answers late in ANNOUNCE_WAIT, so the first candidate spends most of its window.
				time.AfterFunc(30*time.Millisecond, func() { defendsOn(t, taken)(f, pkt) })
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), linkLocalWindow(acd)+linkLocalWindow(acd)/2)
	defer cancel()
	next, drawn := sequence("169.254.10.1", "169.254.10.2")
	_, tried, err := claimLinkLocal(ctx, link, acd, next)
	if !errors.Is(err, ErrLinkLocalNoTime) || tried != 1 || *drawn != 1 {
		t.Fatalf("err = %v after %d tried, %d drawn; want ErrLinkLocalNoTime after the defended first", err, tried, *drawn)
	}
}

func TestAnnounceLinkLocal_SendsANNOUNCE_NUMAnnouncementsAnIntervalApart(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	acd := fastACD()
	acd.AnnounceInterval = 200 * proto.Millisecond
	addr := netip.MustParseAddr("169.254.9.9")
	start := time.Now()
	if err := announceLinkLocal(context.Background(), link, acd, addr); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if took, gap := time.Since(start), time.Duration(acd.AnnounceInterval); took < gap*time.Duration(acd.AnnounceNum-1) {
		t.Errorf("%d announcements took %v, want at least %v between each (RFC 5227 section 2.3)", acd.AnnounceNum, took, gap)
	}
	if len(link.sent) != acd.AnnounceNum || link.sent[0].SenderIP != addr || link.sent[0].TargetIP != addr {
		t.Errorf("sent %d frames (%v), want %d announcements of %v", len(link.sent), link.sent, acd.AnnounceNum, addr)
	}
}

func TestWatchARP_ASocketThatClosesMidProbeIsAnErrorNotSilence(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	close(link.in)
	conflict, err := watchARP(context.Background(), link, 5*time.Second, netip.MustParseAddr("169.254.9.9"), link.mac)
	if err == nil || conflict {
		t.Fatalf("conflict = %v, err = %v; want an error, since nothing was heard for the address", conflict, err)
	}
}

func withFakeARP(t *testing.T, link *fakeARPLink) *int {
	t.Helper()
	opened := 0
	oldOpen, oldACD := openARPLink, linkLocalACD
	openARPLink = func(string) (arpLink, error) { opened++; return link, nil }
	linkLocalACD = fastACD
	t.Cleanup(func() { openARPLink, linkLocalACD = oldOpen, oldACD })
	return &opened
}

func TestLinkLocalFallback_ClaimsA16WithNoGatewayOnlyWhenEligible(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	opened := withFakeARP(t, link)
	p := &Plugin{}
	noLease := fmt.Errorf("acquire: %w", dhcp.ErrNoLease)
	start := time.Now()

	info, err := p.linkLocalFallback(context.Background(), DHCPNetworkOptions{}, start, "veth0", "ep", noLease)
	if !errors.Is(err, dhcp.ErrNoLease) || info.IP != "" || *opened != 0 {
		t.Fatalf("option off: info %+v, err %v, %d links opened; want the DHCP error unchanged", info, err, *opened)
	}
	other := errors.New("setup failed")
	if _, err := p.linkLocalFallback(context.Background(), DHCPNetworkOptions{LinkLocalFallback: true}, start, "veth0", "ep", other); err != other || *opened != 0 {
		t.Fatalf("ineligible error: err %v, %d links opened; want it unchanged", err, *opened)
	}

	info, err = p.linkLocalFallback(context.Background(), DHCPNetworkOptions{LinkLocalFallback: true}, start, "veth0", "ep", noLease)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	pfx, perr := netip.ParsePrefix(info.IP)
	if perr != nil || pfx.Bits() != 16 || !isLinkLocalV4String(info.IP) || info.Gateway != "" || len(info.Routes) != 0 {
		t.Fatalf("info = %+v; want a 169.254 /16 with no gateway and no routes", info)
	}
	if !link.closed {
		t.Error("the ARP link was left open")
	}
}

func TestLinkLocalFallback_AnExhaustedClaimFailsNamingBoth(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	withFakeARP(t, link)
	link.onSend = func(f *fakeARPLink, pkt *wire.ARPPacket) { defendsOn(t, pkt.TargetIP)(f, pkt) }
	_, err := (&Plugin{}).linkLocalFallback(context.Background(), DHCPNetworkOptions{LinkLocalFallback: true},
		time.Now(), "veth0", "ep", dhcp.ErrNoLease)
	if !errors.Is(err, ErrLinkLocalTooManyConflicts) || !strings.Contains(fmt.Sprint(err), dhcp.ErrNoLease.Error()) {
		t.Fatalf("err = %v; want MAX_CONFLICTS naming the DHCP failure too", err)
	}
}

func TestLinkLocalFallback_TheClaimEndsAtTheCallsDeadlineNotTheRequests(t *testing.T) {
	link := newFakeARPLink("02:00:00:00:00:01")
	opened := withFakeARP(t, link)
	linkLocalACD = proto.DefaultACDParams
	// 20 s into the call leaves 6 s before the 26 s mark, less than the 9 s window, on a request with no deadline (#904).
	_, err := (&Plugin{}).linkLocalFallback(context.Background(), DHCPNetworkOptions{LinkLocalFallback: true},
		time.Now().Add(-20*time.Second), "veth0", "ep", dhcp.ErrNoLease)
	if !errors.Is(err, ErrLinkLocalNoTime) || *opened != 1 || len(link.sent) != 0 {
		t.Fatalf("err = %v, link opened %d times, %d frames sent; want no time left and nothing probed", err, *opened, len(link.sent))
	}
}

func llManager(t *testing.T, p *Plugin, last string) *dhcpManager {
	t.Helper()
	m := newDHCPManager(nil, JoinRequest{EndpointID: "e1", NetworkID: "n1"}, DHCPNetworkOptions{LinkLocalFallback: true}).withPlugin(p)
	a, err := netlink.ParseAddr(last)
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	m.setLastIP(false, a)
	return m
}

func TestHealth_ALinkLocalEndpointIsCountedAndShown(t *testing.T) {
	p := newHealthPlugin()
	ll := llManager(t, p, "169.254.10.1/16")
	ll.setHealthClient(&fakeJoinClient{mode: proto.ConflictWait})
	p.persistentDHCP["a"] = ll
	p.persistentDHCP["b"] = newDHCPManager(nil, JoinRequest{EndpointID: "e2", NetworkID: "n1"}, DHCPNetworkOptions{})
	bound := newDHCPManager(nil, JoinRequest{EndpointID: "e3", NetworkID: "n1"}, DHCPNetworkOptions{}).withPlugin(p)
	bound.setHealthClient(&fakeJoinClient{bound: true, l: lease.Lease{Addr: netip.MustParsePrefix("192.0.2.17/24")}})
	bound.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{IP: "192.0.2.17/24"}}, false)
	p.persistentDHCP["c"] = bound

	h := p.healthSnapshot()
	if h.LinkLocalEndpoints != 1 {
		t.Errorf("link_local_endpoints = %d, want 1", h.LinkLocalEndpoints)
	}
	got := map[string]string{}
	for _, e := range h.Endpoints {
		got[e.Endpoint] = e.LeaseState + " " + e.Address
	}
	want := map[string]string{"e1": "link_local 169.254.10.1/16", "e2": "acquiring ", "e3": "bound 192.0.2.17/24"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("endpoint %s = %q, want %q", k, got[k], w)
		}
	}
}

func TestRelease_ALinkLocalEndpointHasNothingToHandBackAndCountsNothing(t *testing.T) {
	p := recordingPlugin(t)
	m := llManager(t, p, "169.254.10.1/16")
	if out := m.releaseHeldLease(false); out != releaseLinkLocal {
		t.Fatalf("outcome = %q, want %q", out, releaseLinkLocal)
	}
	if m.releaseFamily(false) {
		t.Fatal("a link-local endpoint reported a release sent")
	}
	if s, f := p.releasesSentV4.Load(), p.releaseFailuresV4.Load(); s != 0 || f != 0 {
		t.Fatalf("releases_sent_v4=%d release_failures_v4=%d, want 0 and 0", s, f)
	}

	// Control: the same endpoint on a leased address with no record is a counted failure.
	leased := llManager(t, p, "192.0.2.17/24")
	if leased.releaseFamily(false) {
		t.Fatal("control: an endpoint with no record reported a release sent")
	}
	if f := p.releaseFailuresV4.Load(); f != 1 {
		t.Fatalf("control: release_failures_v4=%d, want 1", f)
	}
}

func TestTombstone_ALinkLocalAddressIsNeverRequestedAgain(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "h1", "02:42:ac:11:00:01", "169.254.10.1/16", "")
	mac, ip, _, ok := p.consumeTombstone("net-A", dhcpHostname{name: "h1"})
	if !ok || mac != "02:42:ac:11:00:01" || ip != "" {
		t.Fatalf("tombstone = (%q, %q, %v); want the MAC kept and no address", mac, ip, ok)
	}
	p.addTombstone("net-A", "h2", "02:42:ac:11:00:02", "192.0.2.17/24", "")
	if _, ip, _, _ := p.consumeTombstone("net-A", dhcpHostname{name: "h2"}); ip != "192.0.2.17/24" {
		t.Fatalf("control: a leased address came back as %q", ip)
	}
}

func TestCloseLinkLocalRecord_ClosesOnlyAnAddresslessRecord(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0c")
	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{1}))
	p.closeLinkLocalRecord("net-1", mac)
	p.retainRecordFor("net-1", mac)
	rb, _ := p.records.Rebuilt()
	if rec, _ := rb.ByID(id); rec.Phase != lease.PhaseClosed {
		t.Fatalf("addressless record phase = %s, want closed", rec.Phase)
	}

	mac2, _ := net.ParseMAC("02:42:c0:a8:63:0d")
	id2 := p.recordCreated("net-1", mac2, dhcp.ClientIdentity([]byte{2}))
	if err := p.records.Observed(id2, acquired("192.168.99.12/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	p.closeLinkLocalRecord("net-1", mac2)
	p.retainRecordFor("net-1", mac2)
	rb, _ = p.records.Rebuilt()
	if rec, _ := rb.ByID(id2); rec.Phase != lease.PhaseRetained {
		t.Fatalf("a record holding a lease is %s, want retained", rec.Phase)
	}
}

func TestUnboundState_OnlyAV4LinkLocalAddressReads(t *testing.T) {
	for _, tc := range []struct{ last, want string }{
		{"169.254.10.1/16", "link_local"},
		{"192.0.2.17/24", "acquiring"},
		{"169.253.255.255/16", "acquiring"},
		{"169.255.0.1/16", "acquiring"},
	} {
		if got, _ := llManager(t, nil, tc.last).unboundState(); got != tc.want {
			t.Errorf("%s: state %q, want %q", tc.last, got, tc.want)
		}
	}
	if got, _ := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{}).unboundState(); got != "acquiring" {
		t.Errorf("no address yet: state %q, want acquiring", got)
	}
}

// abortJoinAttach aborts Join's attach as Leave does and waits until its goroutine drops the manager, its last act (#904).
func abortJoinAttach(t *testing.T, p *Plugin, endpointID string, m *dhcpManager) {
	t.Helper()
	m.attachAborted.Store(true)
	m.attachCancel()
	<-m.startedCh
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		p.mu.Lock()
		cur := p.persistentDHCP[endpointID]
		p.mu.Unlock()
		if cur != m {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Join's attach goroutine for %s still holds its manager after 10s", endpointID)
		}
	}
}

func TestJoin_ALinkLocalEndpointGetsNoGatewayAndNoHostRoutes(t *testing.T) {
	withStateDir(t, t.TempDir())
	_, dst, _ := net.ParseCIDR("10.88.0.0/16")
	stubKernelRouteTable(t, []netlink.Route{
		{Gw: net.ParseIP("192.168.99.1")},
		{Dst: dst, Gw: net.ParseIP("192.168.99.253")},
	}, nil, nil)
	if err := saveOptions("n904", DHCPNetworkOptions{Bridge: "lo", LinkLocalFallback: true}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	mac, _ := net.ParseMAC("02:42:0a:00:00:09")
	for _, tc := range []struct {
		addr, wantGW string
		wantRoutes   int
	}{
		{"169.254.60.199/16", "", 0},
		// The control: a leased endpoint on the same network takes the host's default and its route.
		{"192.168.99.61/24", "192.168.99.1", 1},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			a, _ := netlink.ParseAddr(tc.addr)
			p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
				joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
			p.storeJoinHint("e904", joinHint{IPv4: a, MacAddress: mac})
			res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n904", EndpointID: "e904"})
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			p.mu.Lock()
			m := p.persistentDHCP["e904"]
			p.mu.Unlock()
			if m != nil {
				defer abortJoinAttach(t, p, "e904", m)
			}
			if res.Gateway != tc.wantGW || len(res.StaticRoutes) != tc.wantRoutes {
				t.Errorf("Join returned gateway %q and %d routes, want %q and %d", res.Gateway, len(res.StaticRoutes), tc.wantGW, tc.wantRoutes)
			}
		})
	}
}

func TestSetupClient_ALinkLocalAddressIsNeverTheRequestedIP(t *testing.T) {
	for _, tc := range []struct{ last, want string }{
		{"169.254.60.199/16", ""},
		// The control: a leased address is still asked for again.
		{"192.168.99.61/24", "192.168.99.61"},
	} {
		t.Run(tc.last, func(t *testing.T) {
			m, _ := daemonFreeManager(t, &fakeDocker{})
			a, _ := netlink.ParseAddr(tc.last)
			m.setLastIP(false, a)
			m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "lo", HardwareAddr: m.MacAddress}}
			requested, opens := "unset", 0
			prevNew := newDHCPClient
			newDHCPClient = func(_ string, opts *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
				opens++
				requested = opts.RequestedIP
				return nil, errors.New("no client in this test")
			}
			t.Cleanup(func() { newDHCPClient = prevNew })
			_, _ = m.setupClient(false)
			if opens != 1 || requested != tc.want {
				t.Errorf("%d clients built, requested IP %q; want one, %q", opens, requested, tc.want)
			}
		})
	}
}

func TestCreateNetwork_LinkLocalFallbackRefusalsAndTheStoredValue(t *testing.T) {
	cases := []struct {
		name  string
		opts  map[string]interface{}
		names []string // nil means accepted
	}{
		{"ipvlan", map[string]interface{}{"mode": "ipvlan"}, []string{"link_local_fallback", "does not receive the ARP replies"}},
		{"ipv6", map[string]interface{}{"mode": "macvlan", "ipv6": "true"}, []string{"link_local_fallback", "IPv4 only"}},
		{"lease_timeout over the ceiling", map[string]interface{}{"mode": "macvlan", "lease_timeout": "20s"},
			[]string{"lease_timeout", "Set lease_timeout to 16s or less"}},
		{"macvlan", map[string]interface{}{"mode": "macvlan"}, nil},
		{"bridge", map[string]interface{}{"mode": "bridge"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			opts := map[string]interface{}{"parent": mtuTestParent, "link_local_fallback": "true"}
			if c.opts["mode"] == "bridge" {
				opts = map[string]interface{}{"bridge": mtuTestBridge, "link_local_fallback": "true"}
			}
			for k, v := range c.opts {
				opts[k] = v
			}
			err := mtuCreateNetwork(p, opts)
			if c.names == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				stored, err := loadOptions(mtuTestNetwork)
				if err != nil || !stored.LinkLocalFallback {
					t.Errorf("the state file holds link_local_fallback=%v (%v); a plugin restart would fail the endpoint instead", stored.LinkLocalFallback, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v with link_local_fallback=true was accepted", c.opts)
			}
			if got := util.ErrToStatus(err); got != http.StatusBadRequest {
				t.Errorf("the refusal %v maps to HTTP %d, want 400", err, got)
			}
			for _, n := range c.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("the refusal %q does not name %q", err, n)
				}
			}
		})
	}
}

func TestCreateNetwork_IPAMModeRefusesLinkLocalFallback(t *testing.T) {
	err := createIPAMBridgeNetworkOpts(t, map[string]interface{}{"link_local_fallback": "true"}, ipamLocalAddressSpace)
	if err == nil {
		t.Fatal("link_local_fallback was accepted with this plugin as the IPAM driver, where Docker keeps the " +
			"169.254 address after the container moves to a lease")
	}
	if !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "--ipam-driver null") {
		t.Errorf("the refusal %v is not a util.ErrIPAM naming --ipam-driver null", err)
	}
	// The control: the same option on a null-IPAM network is accepted.
	if err := createIPAMBridgeNetworkOpts(t, map[string]interface{}{"link_local_fallback": "true"}, "null"); err != nil {
		t.Errorf("link_local_fallback on a null-IPAM network was refused: %v", err)
	}
}

func TestDeleteEndpoint_ClosesTheRecordOfALinkLocalEndpointOnly(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want lease.Phase
	}{
		{"169.254.60.199/16", lease.PhaseClosed},
		// The control: an addressless record under a leased fingerprint is retained, as before #904.
		{"192.168.99.12/24", lease.PhaseRetained},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			const netID, epID = "net-ll", "c1a1c0ffee00deadbeef0000000000000000000000000000000000000000abcd"
			p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test", LinkLocalFallback: true})
			p.records = recordingPlugin(t).records
			mac, _ := net.ParseMAC("02:42:a9:fe:3c:c7")
			id := p.recordCreated(netID, mac, dhcp.ClientIdentity([]byte{3}))
			p.rememberEndpoint(epID, endpointFingerprint{MAC: mac.String(), IPv4: tc.ip}, dhcpHostname{name: "ll-1"})
			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}
			if got := recordPhase(t, p, id); got != tc.want {
				t.Errorf("record phase after DeleteEndpoint = %s, want %s", got, tc.want)
			}
		})
	}
}

// After the move to a lease Docker still reports the 169.254 address (#104); recovery must hold the lease (#904).
func TestRecoverOneEndpoint_AnEndpointThatLeftLinkLocalRecoversAsItsLease(t *testing.T) {
	const epID = "c1a1c0ffee00deadbeef0000000000000000000000000000000000000000ab04"
	for _, tc := range []struct {
		name, docker, lease, want, wantTombstone string
		wantLinkLocal                            bool
	}{
		{"moved to its lease", "169.254.33.7/16", "192.168.99.10/24", "192.168.99.10/24", "192.168.99.10", false},
		{"still on link-local", "169.254.33.7/16", "", "169.254.33.7/16", "", true},
		{"leased in Docker's view", "192.168.99.20/24", "192.168.99.10/24", "192.168.99.20/24", "192.168.99.20", false},
		{"a record holding a 169.254 lease", "169.254.33.7/16", "169.254.20.5/16", "169.254.33.7/16", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const netID, ctrID = "net-ll-rec", "ctr-ll"
			opts := DHCPNetworkOptions{Bridge: "br-test", LinkLocalFallback: true}
			p := recoveryPlugin(t, netID, &lockedDocker{
				containers: map[string]dContainer.InspectResponse{ctrID: withHostname("ll-rec")},
			})
			p.records = recordingPlugin(t).records
			mac, _ := net.ParseMAC("02:42:a9:fe:21:07")
			id := p.recordCreated(netID, mac, dhcp.ClientIdentity([]byte{4}))
			if tc.lease != "" {
				if err := p.records.Observed(id, acquired(tc.lease, time.Hour), nil); err != nil {
					t.Fatalf("Observed(acquired): %v", err)
				}
			}
			docker, _ := netlink.ParseAddr(tc.docker)
			got := p.recoveredV4(netID, mac, docker)
			if got.String() != tc.want {
				t.Fatalf("recovered v4 = %s, want %s", got, tc.want)
			}
			m := newDHCPManager(p.docker, JoinRequest{NetworkID: netID, EndpointID: epID}, opts).withPlugin(p)
			m.setLastIP(false, got)
			if state, _ := m.unboundState(); (state == linkLocalStateName) != tc.wantLinkLocal || m.onLinkLocal() != tc.wantLinkLocal {
				t.Errorf("health lease_state %q and onLinkLocal %v, want link-local %v", state, m.onLinkLocal(), tc.wantLinkLocal)
			}

			if _, err := p.recoverOneEndpoint(context.Background(), ctrID, netID, epID, mac.String(), tc.docker, "", opts); err != nil {
				t.Fatalf("recoverOneEndpoint: %v", err)
			}
			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{NetworkID: netID, EndpointID: epID}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}
			if _, ip4, _, _ := p.consumeTombstone(netID, dhcpHostname{name: "ll-rec"}); ip4 != tc.wantTombstone {
				t.Errorf("tombstone IPv4 %q, want %q: recovery took %s", ip4, tc.wantTombstone, tc.docker)
			}
		})
	}
}
