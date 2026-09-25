// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
	"github.com/vishvananda/netns"
)

func testMAC(t *testing.T) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC("02:42:c0:a8:63:07")
	if err != nil {
		t.Fatalf("parse MAC: %v", err)
	}
	return mac
}

// Lost{ReasonStopped} follows every CreateEndpoint and clean Leave; a SLAAC ending must not feed dhcp_timeouts (#818).

func TestTranslate_AStopIsNotALeaseLoss(t *testing.T) {
	now := time.Now()
	v4 := netip.MustParsePrefix("192.168.99.7/24")
	v6 := netip.MustParsePrefix("2001:db8:1::42/64")

	for _, tc := range []struct {
		name     string
		reason   proto.Reason
		slaac    bool
		wantEmit bool
		wantType string
	}{
		{"the chassis cancelling its own manager", proto.ReasonStopped, false, false, ""},
		{"a server NAK", proto.ReasonNak, false, true, "nak"},
		{"the lease expiring", proto.ReasonExpired, false, true, "leasefail"},
		{"the link going down", proto.ReasonLinkDown, false, true, "leasefail"},

		{"a formed address expiring", proto.ReasonExpired, true, true, "slaac_lost"},
		{"a formed address on a link that went down", proto.ReasonLinkDown, true, true, "slaac_lost"},
		{"a stop on a formed lease", proto.ReasonStopped, true, false, ""},
		// Unreachable and pinned: the SLAAC field decides before the reason (#818).
		{"a NAK naming a formed lease", proto.ReasonNak, true, true, "slaac_lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := lease.Lease{Addr: v4}
			if tc.slaac {
				l = lease.Lease{SLAAC: true, Addr: v6, Addrs: []lease.Addr6{{Addr: v6}}}
			}
			out, emit, _ := translateOne(
				lease.Event{Kind: lease.Lost, Reason: tc.reason, Lease: l}, now, time.Time{}, netip.Prefix{})
			if emit != tc.wantEmit {
				t.Fatalf("Lost{%v}: emit=%v, want %v. A stop reported as a loss makes every "+
					"successful container start look like a lease failure; a real loss "+
					"swallowed makes the plugin silent about an address the container no "+
					"longer holds.", tc.reason, emit, tc.wantEmit)
			}
			if emit && out.Type != tc.wantType {
				t.Errorf("Lost{%v}: emitted %q, want %q", tc.reason, out.Type, tc.wantType)
			}
			if tc.wantType == "slaac_lost" {
				if !out.Data.SLAAC || out.Data.IP != v6.String() {
					t.Errorf("the event carries %+v; the plugin takes the addresses off the "+
						"link from this data and has no other source for them",
						out.Data.Addrs)
				}
			}
		})
	}
}

func TestTranslate_ARenewalIsNotCountedTwice(t *testing.T) {
	now := time.Now()
	l := lease.Lease{Addr: netip.MustParsePrefix("192.168.99.7/24")}

	_, emit, renewedAt := translateOne(lease.Event{Kind: lease.Renewed, Lease: l}, now, time.Time{}, netip.Prefix{})
	if !emit {
		t.Fatal("a Renewed emitted nothing; the plugin would never see a renewal at all")
	}

	if _, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: l},
		now.Add(coalesceWindow/2), renewedAt, netip.Prefix{}); emit {
		t.Error("the Changed that accompanied the same DHCPACK was emitted as a second renewal")
	}

	if _, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: l},
		now.Add(10*coalesceWindow), renewedAt, netip.Prefix{}); !emit {
		t.Error("a Changed well outside the window was swallowed. That is a re-acquisition on a " +
			"different address — a NAK and a new lease — and the container is left configured " +
			"with the old one.")
	}
}

// RFC 2131 section 4.4.1's desync is for a fleet booting together; each manager is one container (D-1, #899).

func TestBuildParams_NeitherManagerDesyncs(t *testing.T) {
	mac := testMAC(t)

	for _, tc := range []struct {
		name string
		once bool
	}{
		{"the CreateEndpoint one-shot", true},
		{"the Join manager", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildParams(&DHCPClientOptions{MAC: mac}, tc.once)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			if p.DesyncMin != 0 || p.DesyncMax != 0 {
				t.Errorf("%s desyncs by %v..%v; the library documents both zero as the "+
					"disabling value", tc.name, p.DesyncMin, p.DesyncMax)
			}
		})
	}

	once, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams(once): %v", err)
	}
	persistent, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams(persistent): %v", err)
	}
	if once.DesyncMin != persistent.DesyncMin || once.DesyncMax != persistent.DesyncMax {
		t.Errorf("the two managers desync differently: one-shot %v..%v, Join %v..%v. "+
			"D-1 is that neither of them is the fleet section 4.4.1 is about, and the "+
			"argument for exempting only one of them has already been falsified once.",
			once.DesyncMin, once.DesyncMax, persistent.DesyncMin, persistent.DesyncMax)
	}

	// RFC 2131 section 4.1 waits 4 s before the first retransmission, which the rejected desync draw could leave no
	// room for.
	rejected := proto.DefaultParams(mac)
	if rejected.DesyncMin == 0 && rejected.DesyncMax == 0 {
		t.Fatal("proto.DefaultParams no longer desyncs, so the measurement below prices " +
			"nothing and D-1 has become a no-op the chassis is still paying a line for")
	}
	const (
		budget      = 10 * proto.Second
		oneRetrans  = 4 * proto.Second // RFC 2131 4.1's first retransmission
		drawsWanted = 1000
	)
	var tight, n int
	var worst proto.Duration
	for i := 0; i < drawsWanted; i++ {
		d := firstSendDelay(t, rejected, uint64(i)*0x9e3779b97f4a7c15+1)
		n++
		if d > worst {
			worst = d
		}
		if budget-d < oneRetrans {
			tight++
		}
	}
	t.Logf("MEASURED: with the desync left at the library default, the first DISCOVER waits "+
		"up to %.2fs of the %ds lease_timeout, and in %d of %d draws (%.1f%%) less than the "+
		"%ds of RFC 2131 4.1's first retransmission is left for the exchange -- one dropped "+
		"packet and the container start fails",
		float64(worst)/float64(proto.Second), budget/proto.Second, tight, n,
		100*float64(tight)/float64(n), oneRetrans/proto.Second)
	if tight == 0 {
		t.Error("no draw left the exchange short of a retransmission, so this measurement does " +
			"not show what the desync costs and the rule above is unmotivated by it")
	}
}

// Measured in run 33785125087: an expired resume starts from INIT (RFC 2131 section 4.3.2), and the Join manager spent
// the container's life inside the desync draw (#899).

func TestBuildParams_AColdJoinSendsItsFirstPacketAtOnce(t *testing.T) {
	mac := testMAC(t)

	p, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams(Join): %v", err)
	}
	p.Resume = &proto.Resume{
		Addr:      netip.MustParseAddr("192.168.99.7"),
		Expire:    0,
		HasExpire: true,
	}

	const draws = 256
	var worst proto.Duration
	for i := 0; i < draws; i++ {
		rnd := uint64(i)*0x9e3779b97f4a7c15 + 1
		d, mt := firstSendDelayAndType(t, p, rnd)
		if mt != wire.MsgDiscover {
			t.Fatalf("the first packet on draw %d was %s, not a DHCPDISCOVER: this test is "+
				"not on the path it claims to be on. An expired Resume must be refused by "+
				"takeResume and acquired from INIT (RFC 2131 4.3.2).", i, mt)
		}
		if d > worst {
			worst = d
		}
	}
	if worst != 0 {
		t.Errorf("a Join with an expired remembered lease waited up to %.2fs before its "+
			"first DHCPDISCOVER. That is RFC 2131 4.4.1's fleet desync applied to one "+
			"container, and for the length of it the container runs with Docker's own "+
			"resolv.conf and the link-default MTU on a network that asked for neither "+
			"(propagate_dns / propagate_mtu are applied from the bind event). D-1 says "+
			"both managers send at once.", float64(worst)/float64(proto.Second))
	}
	t.Logf("MEASURED: cold Join, first DHCPDISCOVER at %.2fs over %d entropy draws",
		float64(worst)/float64(proto.Second), draws)

	restored := p
	restored.DesyncMin, restored.DesyncMax = proto.DefaultParams(mac).DesyncMin, proto.DefaultParams(mac).DesyncMax
	var delayed int
	var ctlWorst proto.Duration
	for i := 0; i < draws; i++ {
		d, _ := firstSendDelayAndType(t, restored, uint64(i)*0x9e3779b97f4a7c15+1)
		if d > 0 {
			delayed++
		}
		if d > ctlWorst {
			ctlWorst = d
		}
	}
	if delayed != draws {
		t.Errorf("control: with the desync restored, %d of %d draws still sent at once. "+
			"The drive is not reaching the desync block, so the assertion above is not "+
			"measuring the thing it names.", draws-delayed, draws)
	}
	t.Logf("control: with the library default restored, the same cold Join waits up to "+
		"%.2fs before its first DHCPDISCOVER (%d of %d draws delayed)",
		float64(ctlWorst)/float64(proto.Second), delayed, draws)
}

// firstSendDelayAndType is firstSendDelay plus the message type of the packet it stopped on.
func firstSendDelayAndType(t *testing.T, p proto.Params, rnd uint64) (proto.Duration, wire.MessageType) {
	t.Helper()
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	now := proto.Instant(0)
	ev := proto.Simple(proto.EvStart)
	for step := 0; step < 20; step++ {
		_, acts := m.Step(now, rnd+uint64(step), ev)
		var next proto.Duration
		var armed bool
		for _, a := range acts {
			switch a.Kind {
			case proto.ActSend:
				if a.Msg == nil {
					t.Fatalf("ActSend with no message at %v", now)
				}
				mt, ok := a.Msg.Type()
				if !ok {
					t.Fatalf("the message sent at %v carries no option 53", now)
				}
				return proto.Duration(now), mt
			case proto.ActSetTimer:
				next, armed = a.After, true
			}
		}
		if !armed {
			t.Fatalf("no send and no timer at %v", now)
		}
		now += proto.Instant(next)
		ev = proto.TimerFired(lastTimer(acts))
	}
	t.Fatal("no send within 20 steps")
	return 0, 0
}

// firstSendDelay drives one machine from EvStart to its first send and returns the simulated delay before it.
func firstSendDelay(t *testing.T, p proto.Params, rnd uint64) proto.Duration {
	t.Helper()
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	now := proto.Instant(0)
	ev := proto.Simple(proto.EvStart)
	for step := 0; step < 20; step++ {
		_, acts := m.Step(now, rnd+uint64(step), ev)
		var next proto.Duration
		var armed bool
		for _, a := range acts {
			switch a.Kind {
			case proto.ActSend:
				return proto.Duration(now)
			case proto.ActSetTimer:
				next, armed = a.After, true
			}
		}
		if !armed {
			t.Fatalf("no send and no timer at %v", now)
		}
		now += proto.Instant(next)
		ev = proto.TimerFired(lastTimer(acts))
	}
	t.Fatal("no send within 20 steps")
	return 0
}

// An empty vendor_class means option 60 = "docker-net-dhcp", an empty Params.VendorClass none at all (D-2, #899).

func TestBuildParams_TheVendorClassDefaultIsTheChassisS(t *testing.T) {
	mac := testMAC(t)

	p, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.VendorClass != VendorID {
		t.Errorf("with no vendor_class set, option 60 is %q, want %q. Empty means the option "+
			"is not sent at all, which no test asserting on an address can tell apart from "+
			"the default being sent.", p.VendorClass, VendorID)
	}

	p, err = buildParams(&DHCPClientOptions{MAC: mac, VendorClass: "acme"}, true)
	if err != nil {
		t.Fatalf("buildParams(override): %v", err)
	}
	if p.VendorClass != "acme" {
		t.Errorf("the operator's vendor_class was replaced by %q", p.VendorClass)
	}
}

// RFC 2132 section 9.14: option 61 is a type byte then the value, and the library sends ClientID verbatim (D10).

func TestClientIdentity_CarriesTheTypeByte(t *testing.T) {
	got := ClientIdentity([]byte{0xde, 0xad})
	want := []byte{0x00, 0xde, 0xad}
	if len(got) != len(want) {
		t.Fatalf("ClientIdentity gave %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ClientIdentity gave %v, want %v", got, want)
		}
	}

	if n := len(ClientIdentity(nil)); n != 0 {
		t.Errorf("ClientIdentity(nil) is %d byte(s); an empty client-id must send no option 61", n)
	}
}

func TestClientIDPayload_IsClientIdentitysInverse(t *testing.T) {
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	got, ok := ClientIDPayload(ClientIdentity(payload))
	if !ok {
		t.Fatal("the chassis's own identity was refused by the inverse of the function that built it")
	}
	if len(got) != len(payload) {
		t.Fatalf("round trip gave %v, want %v", got, payload)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("round trip gave %v, want %v", got, payload)
		}
	}

	for _, c := range []struct {
		name     string
		identity []byte
	}{
		{"nothing", nil},
		{"a type byte with no payload", []byte{0x00}},
		{"a shape this chassis does not write", []byte{0xff, 1, 2}},
	} {
		if _, ok := ClientIDPayload(c.identity); ok {
			t.Errorf("%s was accepted; trimming its first byte would put a value on the wire "+
				"that no record says, which is the failure the type byte exists to prevent", c.name)
		}
	}
}

// The BROADCAST flag of RFC 2131 section 2 is required on the raw socket; dnsmasq and Kea answer either way (#243,
// #899).

func TestBuildParams_TheBroadcastFlagReachesTheWire(t *testing.T) {
	mac := testMAC(t)

	for _, tc := range []struct {
		name string
		once bool
	}{
		{"the CreateEndpoint one-shot", true},
		{"the Join manager", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildParams(&DHCPClientOptions{MAC: mac}, tc.once)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			if !p.Broadcast {
				t.Fatal("the chassis cleared Params.Broadcast. The library sets it true in " +
					"DefaultParams for a raw-socket client and documents that clearing it " +
					"hangs against any server that honours the flag; the fixture cannot " +
					"see the difference, so nothing downstream of here will catch this.")
			}
		})
	}

	p, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	_, acts := m.Step(0, 0, proto.Simple(proto.EvStart))

	var sent int
	for _, a := range acts {
		if a.Kind != proto.ActSend || a.Msg == nil {
			continue
		}
		sent++
		mt, _ := a.Msg.Type()
		if a.Msg.Flags&wire.FlagBroadcast == 0 {
			t.Errorf("%s went out with flags %#04x; the BROADCAST bit (%#04x) is clear, so a "+
				"server honouring it will unicast the reply to an address this client does "+
				"not have yet", mt, a.Msg.Flags, wire.FlagBroadcast)
		}
	}
	if sent == 0 {
		t.Fatal("EvStart emitted no ActSend, so the flag assertion above judged nothing. " +
			"The one-shot's desync is zero and the DISCOVER is supposed to go out in this " +
			"same step; a test that passes here having sent nothing is the failure this " +
			"repository keeps meeting.")
	}
}

// Option 81 (RFC 4702) is built from the name at construction and has no setter (#961).

func TestBuildParams_Option81NeedsTheNameAtConstruction(t *testing.T) {
	mac := testMAC(t)

	p, err := buildParams(&DHCPClientOptions{MAC: mac, Hostname: "web1", FQDN: "both"}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.FQDN.Name != "web1" {
		t.Errorf("option 81 carries %q, want %q: register_dns is the opt-in to a DNS registration and "+
			"this is the only place the name reaches it", p.FQDN.Name, "web1")
	}

	p, err = buildParams(&DHCPClientOptions{MAC: mac, FQDN: "both"}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.FQDN.Name != "" {
		t.Errorf("option 81 carries %q for a client built with no name, want it absent. If this ever "+
			"became a name the attach could fill in afterwards, the pre-start wait on a register_dns "+
			"network in pkg/plugin could go away with it", p.FQDN.Name)
	}
}

func TestSetHostname_RefusesWhatItCannotSend(t *testing.T) {
	mac := testMAC(t)

	ns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer func() { _ = ns.Close() }()

	newV6 := func(fqdn string) *DHCPClient {
		t.Helper()
		c, err := NewDHCPClient("eth0", &DHCPClientOptions{
			MAC:                mac,
			V6:                 true,
			NetNS:              &ns,
			HonorRouterAdverts: true,
			FQDN:               fqdn,
			Identity6:          Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xc0, 0xa8, 0x63, 0x07}, IAID: 0xc0a86307},
		})
		if err != nil {
			t.Fatalf("NewDHCPClient v6: %v", err)
		}
		return c
	}

	// The refusal comes before the running-client check, so a started v6 client without register_dns is refused too.
	unset := newV6("")
	named := &recordingNamer{}
	unset.namer = named
	if err := unset.SetHostname("web1"); !errors.Is(err, ErrHostnameV6) {
		t.Errorf("SetHostname on a v6 client without register_dns = %v, want ErrHostnameV6: the library sends "+
			"a v6 name only as option 39 with S=1, an AAAA registration nobody opted into (#1029 (a))", err)
	}
	if len(named.names) != 0 {
		t.Errorf("the v6 library client was given %q on a network without register_dns", named.names)
	}

	registered := newV6("both")
	if err := registered.SetHostname("web1"); !errors.Is(err, ErrNoRunningClient) {
		t.Errorf("SetHostname on an unstarted register_dns v6 client = %v, want ErrNoRunningClient", err)
	}
	registered.namer = named
	if err := registered.SetHostname("web1"); err != nil || len(named.names) != 1 || named.names[0] != "web1" {
		t.Errorf("SetHostname on a register_dns v6 client = %v, names %q: want the name handed to the v6 "+
			"library client, which sends it in option 39 (#1029)", err, named.names)
	}

	v4, err := NewDHCPClient("eth0", &DHCPClientOptions{MAC: mac})
	if err != nil {
		t.Fatalf("NewDHCPClient v4: %v", err)
	}
	if err := v4.SetHostname("web1"); !errors.Is(err, ErrNoRunningClient) {
		t.Errorf("SetHostname before Start = %v, want ErrNoRunningClient: nothing is opened until "+
			"Start, so the name would be dropped by a client that does not exist yet", err)
	}
}

// recordingNamer stands in for the library client SetHostname hands the name to (#1029).
type recordingNamer struct{ names []string }

func (r *recordingNamer) SetHostname(name string) error {
	r.names = append(r.names, name)
	return nil
}
