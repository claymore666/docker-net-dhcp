// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func recordingPlugin(t *testing.T) *Plugin {
	t.Helper()
	r, err := dhcp.OpenRecords(filepath.Join(t.TempDir(), recordFileName), "test-instance")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &Plugin{records: r}
}

func acquired(addr string, until time.Duration) lease.Event {
	return lease.Event{
		Kind: lease.Acquired,
		Lease: lease.Lease{
			Addr:     netip.MustParsePrefix(addr),
			Gateway:  netip.MustParseAddr("192.168.99.1"),
			ServerID: netip.MustParseAddr("192.168.99.1"),
			Expire:   time.Now().Add(until),
		},
	}
}

func TestRecordLifecycle_TheJoinManagerResumesTheOneShotsLease(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0a")

	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{1, 2, 3}))
	if id == "" {
		t.Fatal("no record was created")
	}
	if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed(acquired): %v", err)
	}
	if err := p.records.Observed(id, lease.Event{Kind: lease.Lost, Reason: proto.ReasonStopped}, nil); err != nil {
		t.Fatalf("Observed(stopped): %v", err)
	}

	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: "net-1"}}
	m.MacAddress = mac
	gotID, res := m.resumeFromRecord()
	if gotID != id {
		t.Fatalf("Join resumed record %q, CreateEndpoint wrote %q", gotID, id)
	}
	if res.Lease == nil {
		t.Fatal("nothing to resume: the Join manager would DISCOVER and can be handed a different address")
	}
	if got := res.Lease.Addr.String(); got != "192.168.99.10/24" {
		t.Errorf("resuming %s, the one-shot won 192.168.99.10/24", got)
	}
	if res.Prefer != "" {
		t.Errorf("both a resume and a preference (%q); they are mutually exclusive by construction", res.Prefer)
	}

	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, _ := rb.ByID(id)
	if rec.Phase != lease.PhaseJoined {
		t.Errorf("phase after Join = %s, want joined", rec.Phase)
	}
	if rec.Counters.Rejects != 0 {
		t.Fatalf("the fold refused %d event(s); last %v", rec.Counters.Rejects, rec.LastReject)
	}
}

func TestRecordBound_ARecoveredRecordIsNotBoundTwice(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0b")

	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{4, 5, 6}))
	if err := p.records.Observed(id, acquired("192.168.99.11/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: "net-1"}}
	m.MacAddress = mac
	if _, res := m.resumeFromRecord(); res.Lease == nil {
		t.Fatal("first Join found nothing to resume")
	}
	if _, res := m.resumeFromRecord(); res.Lease == nil {
		t.Fatal("the restart found nothing to resume")
	}

	rb, _ := p.records.Rebuilt()
	rec, _ := rb.ByID(id)
	if rec.Counters.Rejects != 0 {
		t.Errorf("the second Join's bind was refused (%d rejects, last %v); "+
			"recovery after a plugin restart would leave the record with a hole in its history",
			rec.Counters.Rejects, rec.LastReject)
	}
	if rec.Phase != lease.PhaseJoined {
		t.Errorf("phase = %s, want joined", rec.Phase)
	}
}

func TestRetainRecordFor_TombstonesTheIdentity(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0c")

	id := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{7, 8, 9}))
	if err := p.records.Observed(id, acquired("192.168.99.12/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: "net-1"}}
	m.MacAddress = mac
	if _, res := m.resumeFromRecord(); res.Lease == nil {
		t.Fatal("nothing to resume")
	}
	p.recordLeft(id)
	p.retainRecordFor("net-1", mac)

	rb, _ := p.records.Rebuilt()
	rec, _ := rb.ByID(id)
	if rec.Phase != lease.PhaseRetained {
		t.Fatalf("phase = %s, want retained", rec.Phase)
	}
	if rec.Counters.Rejects != 0 {
		t.Fatalf("the fold refused %d event(s); last %v", rec.Counters.Rejects, rec.LastReject)
	}

	// A tombstone's address may be asked for in a DISCOVER but not claimed with an INIT-REBOOT,
	// which asserts a held lease (RFC 2131 section 3.2).
	_, res, ok := p.records.Resume("net-1", mac, time.Now())
	if !ok {
		t.Fatal("the tombstone answered nothing at all")
	}
	if res.Lease != nil {
		t.Error("a tombstoned record offered an INIT-REBOOT: the address was given up and claiming it back asserts a lease it does not hold")
	}
	if res.Prefer != "192.168.99.12" {
		t.Errorf("preference = %q, want 192.168.99.12 — a restarted container would not get its address back", res.Prefer)
	}
}

func TestRecordCreated_ASecondEndpointDoesNotShareTheFirstsRecord(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0d")

	a := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{1}))
	b := p.recordCreated("net-2", mac, dhcp.ClientIdentity([]byte{1}))
	if a == b || a == "" || b == "" {
		t.Fatalf("record ids %q and %q", a, b)
	}
	if err := p.records.Observed(a, acquired("192.168.99.13/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	id, res, ok := p.records.Resume("net-2", mac, time.Now())
	if !ok {
		t.Fatal("net-2 has no record")
	}
	if id != b {
		t.Errorf("net-2 resumed net-1's record")
	}
	if res.Lease != nil {
		t.Errorf("net-2 offered net-1's lease %s", res.Lease.Addr)
	}
}

func TestEndpointRecordKey_SeparatesIpvlanEndpointsThatShareAMAC(t *testing.T) {
	shared, err := net.ParseMAC("02:42:ac:11:00:02")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	epA := "aaaaaaaabbbbbbbbccccccccdddddddd11112222"
	epB := "eeeeeeeeffffffff00000000111111112222aaaa"

	a := endpointRecordKey(ModeIPvlan, epA, shared)
	b := endpointRecordKey(ModeIPvlan, epB, shared)
	if a.String() == b.String() {
		t.Fatalf("two ipvlan endpoints sharing MAC %s got one record key (%s); "+
			"the newer record answers both endpoints' resumes", shared, a)
	}
	if a.String() == shared.String() || b.String() == shared.String() {
		t.Errorf("an ipvlan key is still the parent MAC: a=%s b=%s parent=%s", a, b, shared)
	}

	for _, k := range []net.HardwareAddr{a, b} {
		if len(k) != 6 {
			t.Errorf("key %v is %d bytes, want 6 so it reads as a hardware address", k, len(k))
		}
		if k[0]&0x02 == 0 {
			t.Errorf("key %s is not locally administered, so it could collide with a real MAC", k)
		}
		if k[0]&0x01 != 0 {
			t.Errorf("key %s is a group address", k)
		}
	}

	if again := endpointRecordKey(ModeIPvlan, epA, shared); again.String() != a.String() {
		t.Errorf("the key is not stable: %s then %s", a, again)
	}
}

func TestEndpointRecordKey_LeavesEveryOtherModeOnItsMAC(t *testing.T) {
	mac, err := net.ParseMAC("02:42:ac:11:00:03")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	for _, mode := range []string{ModeBridge, ModeMacvlan, ""} {
		got := endpointRecordKey(mode, "aaaaaaaabbbbbbbbccccccccdddddddd11112222", mac)
		if got.String() != mac.String() {
			t.Errorf("endpointRecordKey(%q) = %s, want the endpoint's own MAC %s", mode, got, mac)
		}
	}
}

func TestRecoveredMAC_TreatsAnEmptyMACAsIpvlanOnly(t *testing.T) {
	t.Run("a reported MAC is used verbatim", func(t *testing.T) {
		got, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeMacvlan}, "02:42:ac:11:00:04")
		if err != nil {
			t.Fatalf("recoveredMAC: %v", err)
		}
		if got.String() != "02:42:ac:11:00:04" {
			t.Errorf("got %s", got)
		}
	})
	t.Run("an unparseable MAC is still a failure", func(t *testing.T) {
		if _, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeIPvlan}, "not-a-mac"); err == nil {
			t.Error("recoveredMAC accepted a malformed MAC on the one mode that tolerates an absent one")
		}
	})
	t.Run("an empty MAC on a mode that has one is a failure", func(t *testing.T) {
		_, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "dh-no-such-parent"}, "")
		if err == nil {
			t.Fatal("recoveredMAC accepted an empty MAC on macvlan, where an endpoint always has one")
		}
		if !errors.Is(err, errNoRecoveryMAC) {
			t.Errorf("error %q is not the refusal; macvlan reached the ipvlan branch and "+
				"went looking for a parent MAC to inherit, which is only ipvlan's rule", err)
		}
		if strings.Contains(err.Error(), "dh-no-such-parent") {
			t.Errorf("error %q names the parent link; macvlan must be refused before "+
				"any parent is consulted", err)
		}
	})
	t.Run("an empty MAC on ipvlan reads the parent", func(t *testing.T) {
		_, err := recoveredMAC(DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "dh-no-such-parent"}, "")
		if err == nil {
			t.Fatal("recoveredMAC found a parent that does not exist")
		}
		if !strings.Contains(err.Error(), "dh-no-such-parent") {
			t.Errorf("error %q does not name the parent it went looking for; "+
				"the empty MAC was refused rather than inherited", err)
		}
	})
}

func TestRecordKey_IsTheEndpointKeyAndNotTheBareMAC(t *testing.T) {
	mac, err := net.ParseMAC("02:42:ac:11:00:07")
	if err != nil {
		t.Fatalf("parse MAC: %v", err)
	}

	t.Run("ipvlan keys on the endpoint", func(t *testing.T) {
		a := &dhcpManager{
			opts:    DHCPNetworkOptions{Mode: ModeIPvlan},
			joinReq: JoinRequest{NetworkID: "net-1", EndpointID: strings.Repeat("a", 64)},
		}
		a.MacAddress = mac
		b := &dhcpManager{
			opts:    DHCPNetworkOptions{Mode: ModeIPvlan},
			joinReq: JoinRequest{NetworkID: "net-1", EndpointID: strings.Repeat("b", 64)},
		}
		b.MacAddress = mac

		if a.recordKey().String() == b.recordKey().String() {
			t.Fatalf("two ipvlan endpoints sharing the parent MAC %s got the same record "+
				"key %s; one of them resumes the other's lease and installs a duplicate "+
				"address", mac, a.recordKey())
		}
		if a.recordKey().String() == mac.String() {
			t.Errorf("the ipvlan record key is the bare MAC %s; every endpoint on the "+
				"network presents it, so the key identifies the network, not the endpoint",
				mac)
		}
	})

	t.Run("macvlan keeps its MAC", func(t *testing.T) {
		m := &dhcpManager{
			opts:    DHCPNetworkOptions{Mode: ModeMacvlan},
			joinReq: JoinRequest{NetworkID: "net-1", EndpointID: strings.Repeat("a", 64)},
		}
		m.MacAddress = mac
		if m.recordKey().String() != mac.String() {
			t.Errorf("macvlan record key is %s, want the endpoint MAC %s; an endpoint "+
				"whose stored records are under its MAC would stop finding them",
				m.recordKey(), mac)
		}
	})
}
