// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// fakeSender records the lease record and source address; a source equal to the released address violates RFC 9915
// section 18.2.7.
type fakeSender struct {
	mu     sync.Mutex
	recs   []lease.Record
	cfgs   []runtime.ReleaseConfig
	err    error
	errFor func(lease.Record) error
}

func (f *fakeSender) send(rec lease.Record, cfg runtime.ReleaseConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, rec)
	f.cfgs = append(f.cfgs, cfg)
	if f.errFor != nil {
		return f.errFor(rec)
	}
	return f.err
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}

func installSender(t *testing.T, f *fakeSender) *fakeSender {
	t.Helper()
	if f == nil {
		f = &fakeSender{}
	}
	prev := rtSendRelease
	rtSendRelease = f.send
	t.Cleanup(func() { rtSendRelease = prev })
	return f
}

func hostParent(t *testing.T, addrs ...string) {
	t.Helper()
	link := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 7}}
	prevByName, prevList := nlLinkByName, nlAddrList
	nlLinkByName = func(name string) (netlink.Link, error) {
		if name != "br0" {
			return nil, netlink.LinkNotFoundError{}
		}
		return link, nil
	}
	nlAddrList = func(_ netlink.Link, family int) ([]netlink.Addr, error) {
		var out []netlink.Addr
		for _, a := range addrs {
			pa, err := netlink.ParseAddr(a)
			if err != nil {
				t.Fatalf("ParseAddr %q: %v", a, err)
			}
			if (family == unix.AF_INET) != (pa.IP.To4() != nil) {
				continue
			}
			out = append(out, *pa)
		}
		return out, nil
	}
	t.Cleanup(func() { nlLinkByName, nlAddrList = prevByName, prevList })
}

func withRecords(t *testing.T, p *Plugin) *Plugin {
	t.Helper()
	if p.records != nil {
		return p
	}
	r, err := dhcp.OpenRecords(filepath.Join(t.TempDir(), recordFileName), "test-instance")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	p.records = r
	return p
}

var releaseTestMAC = net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

func releaseTestIdentity6() dhcp.Identity6 {
	return dhcp.Identity6{
		DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
		IAID: 0xac110002,
	}
}

func TestReleaseLease_ParseRefusesEveryValueItDoesNotImplement(t *testing.T) {
	for _, tc := range []struct {
		in       string
		want     string
		wantErr  bool
		mentions []string
	}{
		{in: "", want: ReleaseNever},
		{in: "never", want: ReleaseNever},
		{in: "on_stop", want: ReleaseOnStop},
		{in: "on_remove", want: ReleaseOnRemove},
		// `on_remove` proves the refusal names every value (#984).
		{in: "On_Stop", wantErr: true, mentions: []string{"is not one of", "on_remove"}},
		{in: "on_stpo", wantErr: true, mentions: []string{"is not one of", "on_remove"}},
		{in: "ON_REMOVE", wantErr: true, mentions: []string{"is not one of", "on_remove"}},
		{in: "always", wantErr: true, mentions: []string{"is not one of"}},
		{in: " on_stop", wantErr: true, mentions: []string{"is not one of"}},
		{in: "true", wantErr: true, mentions: []string{"is not one of"}},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseReleaseLease(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseReleaseLease(%q) = %q, nil; a value this plugin does not "+
						"implement must not resolve to a behaviour by accident", tc.in, got)
				}
				if !errors.Is(err, util.ErrIPAM) {
					t.Errorf("refusal is not a %v, so `docker network create` reports it as an "+
						"internal error: %v", util.ErrIPAM, err)
				}
				for _, want := range tc.mentions {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not say %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReleaseLease(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseReleaseLease(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReleaseLease_TheCreateAndStoredPathsRefuseTheSameSet(t *testing.T) {
	for _, v := range []string{"", "never", "on_stop", "on_remove", "On_Stop", "sometimes"} {
		t.Run(v, func(t *testing.T) {
			opts := DHCPNetworkOptions{Bridge: "br0", ReleaseLease: v}
			_, wantErr := parseReleaseLease(v)

			if err := validateModeOptions(opts); (err != nil) != (wantErr != nil) {
				t.Errorf("validateModeOptions(release_lease=%q) = %v, want an error: %v",
					v, err, wantErr != nil)
			}

			p := &Plugin{}
			err := p.checkStoredOptions("n1", opts)
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("checkStoredOptions(release_lease=%q) = %v, want an error: %v",
					v, err, wantErr != nil)
			}
			want := int32(0)
			if wantErr != nil {
				want = 1
			}
			if got := p.networkOptionsRejected.Load(); got != want {
				t.Errorf("network_options_rejected = %d, want %d: an operator's only "+
					"machine-readable sign that a stored option was refused", got, want)
			}
		})
	}
}

func releasingManager(t *testing.T, p *Plugin, value string, ipv6 bool) *dhcpManager {
	t.Helper()
	withRecords(t, p)
	hostParent(t, "192.168.99.2/24", "fe80::2/64")
	opts := DHCPNetworkOptions{AuditLog: true, ReleaseLease: value, IPv6: ipv6, Bridge: "br0"}
	m := stoppingManager(t, p, opts, nil, nil)
	m.recordID = p.recordCreated("net1", releaseTestMAC, dhcp.ClientIdentity([]byte{1, 2, 3}))
	if ipv6 {
		m.recordID6 = p.recordCreated6("net1", releaseTestMAC, releaseTestIdentity6())
	}
	if m.recordID == "" {
		t.Fatal("no record was created for the releasing manager")
	}
	return m
}

func TestReleaseLease_OnlyALeaveOnAReleasingNetworkReleases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		leaving bool
		want    int
	}{
		{name: "on_stop, leaving", value: ReleaseOnStop, leaving: true, want: 1},
		{name: "on_stop, not leaving", value: ReleaseOnStop, leaving: false, want: 0},
		{name: "never, leaving", value: ReleaseNever, leaving: true, want: 0},
		{name: "never, not leaving", value: ReleaseNever, leaving: false, want: 0},
		{name: "unset, leaving", value: "", leaving: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			sender := installSender(t, nil)
			m := releasingManager(t, p, tc.value, false)

			var err error
			if tc.leaving {
				err = m.StopForLeave()
			} else {
				err = m.Stop()
			}
			if err != nil {
				t.Fatalf("stop(leaving=%v): %v", tc.leaving, err)
			}

			if got := sender.callCount(); got != tc.want {
				t.Fatalf("a release was sent %d time(s), want %d", got, tc.want)
			}
			if got := m.releasedV4.Load(); got != (tc.want == 1) {
				t.Errorf("releasedV4 = %v, want %v; the record phase and the tombstone "+
					"skip are both read off this", got, tc.want == 1)
			}
			if got := p.releasesSentV4.Load(); got != int32(tc.want) {
				t.Errorf("releases_sent_v4 = %d, want %d", got, tc.want)
			}
			if got := p.releaseFailuresV4.Load(); got != 0 {
				t.Errorf("release_failures_v4 = %d, want 0", got)
			}
		})
	}
}

func TestReleaseLease_TheCounterFollowsTheSendNotTheIntent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sendErr      error
		noRecord     bool
		wantSent     int32
		wantFailures int32
		wantReleased bool
		wantCalls    int
	}{
		{
			name:         "the datagram went out",
			wantSent:     1,
			wantReleased: true,
			wantCalls:    1,
		},
		{
			name:         "the datagram did not leave the host",
			sendErr:      errors.New("sendto: network is unreachable"),
			wantFailures: 1,
			wantCalls:    1,
		},
		{
			name:         "there is no record for this family",
			noRecord:     true,
			wantFailures: 1,
			wantCalls:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			sender := installSender(t, &fakeSender{err: tc.sendErr})
			m := releasingManager(t, p, ReleaseOnStop, false)
			if tc.noRecord {
				m.recordID = ""
			}

			releasedV4, releasedV6 := m.releaseHeldLeases()
			if releasedV4 != tc.wantReleased {
				t.Errorf("releasedV4 = %v, want %v", releasedV4, tc.wantReleased)
			}
			if releasedV6 {
				t.Errorf("releasedV6 = true on a v4-only network")
			}
			if got := sender.callCount(); got != tc.wantCalls {
				t.Errorf("the wire saw %d datagram(s), want %d: a refusal that still sent one "+
					"is the one failure no counter can see", got, tc.wantCalls)
			}
			if got := p.releasesSentV4.Load(); got != tc.wantSent {
				t.Errorf("releases_sent_v4 = %d, want %d", got, tc.wantSent)
			}
			if got := p.releaseFailuresV4.Load(); got != tc.wantFailures {
				t.Errorf("release_failures_v4 = %d, want %d", got, tc.wantFailures)
			}
			if got := p.releasesSentV6.Load() + p.releaseFailuresV6.Load(); got != 0 {
				t.Errorf("the v6 pair moved by %d on a v4-only network; a family that was "+
					"never asked for has no outcome to count", got)
			}
		})
	}
}

func TestReleaseLease_TheReleaseIsBuiltFromThisFamilysRecord(t *testing.T) {
	p := &Plugin{}
	sender := installSender(t, nil)
	m := releasingManager(t, p, ReleaseOnStop, true)

	releasedV4, releasedV6 := m.releaseHeldLeases()
	if !releasedV4 || !releasedV6 {
		t.Fatalf("released v4=%v v6=%v, want both", releasedV4, releasedV6)
	}
	if got := sender.callCount(); got != 2 {
		t.Fatalf("the wire saw %d datagram(s), want one per family", got)
	}

	sender.mu.Lock()
	recs, cfgs := sender.recs, sender.cfgs
	sender.mu.Unlock()

	for i, want := range []struct {
		family string
		id     string
		source string
	}{
		{"v4", m.recordID, "192.168.99.2"},
		{"v6", m.recordID6, "fe80::2"},
	} {
		if recs[i].ID != want.id {
			t.Errorf("the %s release was built from record %q, want %q",
				want.family, recs[i].ID, want.id)
		}
		if got := cfgs[i].Source.String(); got != want.source {
			t.Errorf("the %s release was sent from %s, want %s", want.family, got, want.source)
		}
		if cfgs[i].Interface != "br0" {
			t.Errorf("the %s release left by %q, want the parent br0", want.family, cfgs[i].Interface)
		}
	}
}

// TestReleaseLease_TheSourceIsNeverTheReleasedAddress drives hostSourceFor per family (RFC 9915 section 18.2.7).
func TestReleaseLease_TheSourceIsNeverTheReleasedAddress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		v6    bool
		addrs []string
		want  string
	}{
		{"v4 takes the one address there is", false,
			[]string{"192.168.99.2/24"}, "192.168.99.2"},
		{"v4 on a parent with several takes the smallest", false,
			[]string{"192.168.99.9/24", "192.168.99.2/24", "192.168.99.7/24"}, "192.168.99.2"},
		{"v4 never takes the address being released", false,
			[]string{"192.168.99.50/24", "192.168.99.60/24"}, "192.168.99.60"},
		{"v4 skips a link-local", false,
			[]string{"169.254.1.1/16", "192.168.99.2/24"}, "192.168.99.2"},
		{"v4 on a parent with no address refuses", false,
			[]string{"fe80::2/64"}, ""},
		{"v6 takes the link-local", true,
			[]string{"fe80::2/64", "fd00::9/64"}, "fe80::2"},
		{"v6 on a parent with several link-locals takes the smallest", true,
			[]string{"fe80::9/64", "fe80::2/64"}, "fe80::2"},
		{"v6 with only a global address refuses", true,
			[]string{"fd00::9/64"}, ""},
		{"v6 on a parent with IPv6 disabled refuses", true,
			[]string{"192.168.99.2/24"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			m := releasingManager(t, p, ReleaseOnStop, true)
			// stoppingManager's 192.168.99.50 and fd00::50 make the third row's first candidate the released address.
			hostParent(t, tc.addrs...)

			got, iface, err := m.hostSourceFor(tc.v6)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("hostSourceFor(v6=%v) = %s, want a refusal", tc.v6, got)
				}
				if !errors.Is(err, errNoHostSource) {
					t.Errorf("hostSourceFor refused with %v, want errNoHostSource", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("hostSourceFor(v6=%v): %v", tc.v6, err)
			}
			if got.String() != tc.want {
				t.Errorf("hostSourceFor(v6=%v) = %s, want %s", tc.v6, got, tc.want)
			}
			if iface != "br0" {
				t.Errorf("hostSourceFor named interface %q, want br0", iface)
			}
		})
	}
}

// TestReleaseLease_AFailedV6WithdrawalSendsNothing: the address MUST be off the interface before the Release
// (RFC 9915 section 18.2.7).
func TestReleaseLease_AFailedV6WithdrawalSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		delErr       error
		wantCalls    int
		wantSent     int32
		wantFailures int32
		wantOutcome  releaseOutcome
	}{
		{name: "the address came off", delErr: nil, wantCalls: 1, wantSent: 1,
			wantOutcome: releaseSent},
		{
			name:         "the address could not be taken off",
			delErr:       errors.New("netlink: operation not permitted"),
			wantCalls:    0,
			wantFailures: 1,
			wantOutcome:  releaseWithdrawFailed,
		},
		{
			name: "the address was already gone", delErr: syscall.EADDRNOTAVAIL,
			wantCalls: 1, wantSent: 1, wantOutcome: releaseSent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := nlAddrDel
			nlAddrDel = func(*netlink.Handle, netlink.Link, *netlink.Addr) error { return tc.delErr }
			t.Cleanup(func() { nlAddrDel = prev })

			p := &Plugin{}
			sender := installSender(t, nil)
			m := releasingManager(t, p, ReleaseOnStop, true)
			m.netHandle = &netlink.Handle{}
			m.ctrLink = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}

			if got := m.releaseHeldLease(true); got != tc.wantOutcome {
				t.Errorf("releaseHeldLease(v6) = %q, want %q", got, tc.wantOutcome)
			}
			if got := sender.callCount(); got != tc.wantCalls {
				t.Fatalf("the wire saw %d datagram(s), want %d: RFC 9915 section 18.2.7 "+
					"forbids beginning the exchange while the address is on the link",
					got, tc.wantCalls)
			}
			m.countRelease(true, tc.wantSent == 1)
			if got := p.releasesSentV6.Load(); got != tc.wantSent {
				t.Errorf("releases_sent_v6 = %d, want %d", got, tc.wantSent)
			}
			if got := p.releaseFailuresV6.Load(); got != tc.wantFailures {
				t.Errorf("release_failures_v6 = %d, want %d", got, tc.wantFailures)
			}
		})
	}
}

func TestReleaseLease_TheRecordIsClosedPerFamily(t *testing.T) {
	for _, tc := range []struct {
		name        string
		releasedV4  bool
		releasedV6  bool
		wantV4Phase lease.Phase
		wantV6Phase lease.Phase
	}{
		{"neither family released", false, false, lease.PhaseLeft, lease.PhaseLeft},
		{"both families released", true, true, lease.PhaseClosed, lease.PhaseClosed},
		{"only v4 released", true, false, lease.PhaseClosed, lease.PhaseLeft},
		{"only v6 released", false, true, lease.PhaseLeft, lease.PhaseClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := recordingPlugin(t)
			mac, _ := net.ParseMAC("02:42:c0:a8:63:0a")
			id4 := p.recordCreated("net-1", mac, dhcp.ClientIdentity([]byte{1, 2, 3}))
			id6 := p.recordCreated6("net-1", mac, dhcp.Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xc0, 0xa8, 0x63, 0x0a}, IAID: 0xc0a8630a})
			if id4 == "" || id6 == "" {
				t.Fatal("no record was created")
			}
			p.recordBound(id4, "created")
			p.recordBound(id6, "created")

			p.settleReleasedRecord(id4, tc.releasedV4)
			p.settleReleasedRecord(id6, tc.releasedV6)

			rb, err := p.records.Rebuilt()
			if err != nil {
				t.Fatalf("Rebuilt: %v", err)
			}
			for _, f := range []struct {
				family string
				id     string
				want   lease.Phase
			}{
				{"v4", id4, tc.wantV4Phase},
				{"v6", id6, tc.wantV6Phase},
			} {
				rec, ok := rb.ByID(f.id)
				if !ok {
					t.Fatalf("%s record is not in the fold", f.family)
				}
				if rec.Phase != f.want {
					t.Errorf("%s record phase = %s, want %s", f.family, rec.Phase, f.want)
				}
				if rec.Counters.Rejects != 0 {
					t.Fatalf("%s: the fold refused %d event(s); last %v",
						f.family, rec.Counters.Rejects, rec.LastReject)
				}
			}
		})
	}
}

// TestReleaseLease_AReleasedEndpointLeavesNothingBehind: a tombstone after a release would reassign returned
// addresses (#524).
func TestReleaseLease_AReleasedEndpointLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name          string
		releasedV4    bool
		releasedV6    bool
		wantTombstone bool
		wantV4Phase   lease.Phase
		wantV6Phase   lease.Phase
	}{
		{"both families released: nothing is left behind", true, true,
			false, lease.PhaseClosed, lease.PhaseClosed},
		{"not released: the endpoint keeps its MAC across a restart", false, false,
			true, lease.PhaseRetained, lease.PhaseRetained},
		{
			"only the v6 lease went back: the v4 record still gets its tombstone phase", false, true,
			false, lease.PhaseRetained, lease.PhaseClosed,
		},
		{
			"only the v4 lease went back: the v6 record still gets its tombstone phase", true, false,
			false, lease.PhaseClosed, lease.PhaseRetained,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			if err := saveOptions("n1", DHCPNetworkOptions{Bridge: "br0", ReleaseLease: ReleaseOnStop}); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			restore := nlLinkByName
			nlLinkByName = func(string) (netlink.Link, error) { return nil, netlink.LinkNotFoundError{} }
			t.Cleanup(func() { nlLinkByName = restore })

			p := recordingPlugin(t)
			p.docker = &fakeDocker{inspectErr: errors.New("docker must not be called")}
			p.endpointFingerprints = make(map[string]endpointFingerprint)

			mac, _ := net.ParseMAC("02:42:ac:11:00:02")
			id4 := p.recordCreated("n1", mac, dhcp.ClientIdentity([]byte{1, 2, 3}))
			id6 := p.recordCreated6("n1", mac, dhcp.Identity6{
				DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02}, IAID: 0xac110002,
			})
			if id4 == "" || id6 == "" {
				t.Fatal("no record was created")
			}
			p.recordBound(id4, "created")
			p.recordBound(id6, "created")

			p.rememberEndpoint("ep-1", endpointFingerprint{
				MAC: mac.String(), IPv4: "192.168.99.50",
			}, dhcpHostname{name: "web"})
			p.settleReleasedRecord(id4, tc.releasedV4)
			p.settleReleasedRecord(id6, tc.releasedV6)
			if tc.releasedV4 || tc.releasedV6 {
				p.markEndpointReleased("ep-1")
			}

			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
				NetworkID: "n1", EndpointID: "ep-1",
			}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}

			_, _, _, ok := p.tombstones.consume("n1", "web")
			if ok != tc.wantTombstone {
				t.Errorf("a tombstone was consumable = %v, want %v", ok, tc.wantTombstone)
			}

			rb, err := p.records.Rebuilt()
			if err != nil {
				t.Fatalf("Rebuilt: %v", err)
			}
			for _, f := range []struct {
				family string
				id     string
				want   lease.Phase
			}{
				{"v4", id4, tc.wantV4Phase},
				{"v6", id6, tc.wantV6Phase},
			} {
				rec, found := rb.ByID(f.id)
				if !found {
					t.Fatalf("the %s record is not in the fold", f.family)
				}
				if rec.Phase != f.want {
					t.Errorf("%s record phase = %s, want %s", f.family, rec.Phase, f.want)
				}
				if rec.Counters.Rejects != 0 {
					t.Fatalf("the fold refused %d %s event(s); last %v",
						rec.Counters.Rejects, f.family, rec.LastReject)
				}
			}
		})
	}
}

func TestReleaseLease_LeaveSettlesTheWholeEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		value        string
		v4OK         bool
		v6OK         bool
		wantReleased bool
		wantV4Phase  lease.Phase
		wantV6Phase  lease.Phase
	}{
		{
			name: "both families released", value: ReleaseOnStop, v4OK: true, v6OK: true,
			wantReleased: true, wantV4Phase: lease.PhaseClosed, wantV6Phase: lease.PhaseClosed,
		},
		{
			name: "only v4 got out", value: ReleaseOnStop, v4OK: true, v6OK: false,
			wantReleased: true, wantV4Phase: lease.PhaseClosed, wantV6Phase: lease.PhaseLeft,
		},
		{
			name: "only v6 got out", value: ReleaseOnStop, v4OK: false, v6OK: true,
			wantReleased: true, wantV4Phase: lease.PhaseLeft, wantV6Phase: lease.PhaseClosed,
		},
		{
			name: "nothing was asked for", value: ReleaseNever, v4OK: true, v6OK: true,
			wantReleased: false, wantV4Phase: lease.PhaseLeft, wantV6Phase: lease.PhaseLeft,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := recordingPlugin(t)
			var ledgerFailures atomic.Int32
			p.ledger = testLedger(t, &ledgerFailures)
			p.endpointFingerprints = make(map[string]endpointFingerprint)
			p.persistentDHCP = make(map[string]*dhcpManager)

			mac, _ := net.ParseMAC("02:42:c0:a8:63:0a")
			id4 := p.recordCreated("net1", mac, dhcp.ClientIdentity([]byte{1, 2, 3}))
			id6 := p.recordCreated6("net1", mac, dhcp.Identity6{
				DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xc0, 0xa8, 0x63, 0x0a}, IAID: 0xc0a8630a,
			})
			if id4 == "" || id6 == "" {
				t.Fatal("no record was created")
			}
			p.recordBound(id4, "created")
			p.recordBound(id6, "created")

			installSender(t, &fakeSender{errFor: func(rec lease.Record) error {
				ok := tc.v4OK
				if rec.Family == lease.FamilyV6 {
					ok = tc.v6OK
				}
				if ok {
					return nil
				}
				return errors.New("sendto: network is unreachable")
			}})
			m := releasingManager(t, p, tc.value, true)
			m.recordID, m.recordID6 = id4, id6
			p.persistentDHCP["ep1"] = m
			p.rememberEndpoint("ep1", endpointFingerprint{
				MAC: mac.String(), IPv4: "192.168.99.50",
			}, dhcpHostname{name: "web"})

			if err := p.Leave(context.Background(), LeaveRequest{
				NetworkID: "net1", EndpointID: "ep1",
			}); err != nil {
				t.Fatalf("Leave: %v", err)
			}

			p.mu.Lock()
			fp := p.endpointFingerprints["ep1"]
			p.mu.Unlock()
			if fp.Released != tc.wantReleased {
				t.Errorf("fingerprint Released = %v, want %v; DeleteEndpoint reads this to "+
					"decide whether to lay a tombstone", fp.Released, tc.wantReleased)
			}

			rb, err := p.records.Rebuilt()
			if err != nil {
				t.Fatalf("Rebuilt: %v", err)
			}
			for _, f := range []struct {
				family string
				id     string
				want   lease.Phase
			}{
				{"v4", id4, tc.wantV4Phase},
				{"v6", id6, tc.wantV6Phase},
			} {
				rec, ok := rb.ByID(f.id)
				if !ok {
					t.Fatalf("%s record is not in the fold", f.family)
				}
				if rec.Phase != f.want {
					t.Errorf("%s record phase = %s, want %s; a record left re-bindable "+
						"after its address went back asks for it again on the next start",
						f.family, rec.Phase, f.want)
				}
			}
		})
	}
}

func TestReleaseLease_AFailedStartStillReleases(t *testing.T) {
	for _, tc := range []struct {
		name         string
		value        string
		wantCalls    int
		wantSent     int32
		wantFailed   int32
		wantReleased bool
	}{
		{"on_stop: the record still has the lease, so it goes back", ReleaseOnStop, 1, 1, 0, true},
		{"never: nothing is attempted and nothing is counted", ReleaseNever, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			sender := installSender(t, nil)
			m := releasingManager(t, p, tc.value, false)
			m.startErr = errors.New("failed to start DHCP client")

			if err := m.StopForLeave(); err != nil {
				t.Fatalf("StopForLeave: %v", err)
			}

			if got := sender.callCount(); got != tc.wantCalls {
				t.Errorf("the wire saw %d datagram(s), want %d", got, tc.wantCalls)
			}
			if got := m.releasedV4.Load(); got != tc.wantReleased {
				t.Errorf("releasedV4 = %v, want %v", got, tc.wantReleased)
			}
			if got := p.releasesSentV4.Load(); got != tc.wantSent {
				t.Errorf("releases_sent_v4 = %d, want %d", got, tc.wantSent)
			}
			if got := p.releaseFailuresV4.Load(); got != tc.wantFailed {
				t.Errorf("release_failures_v4 = %d, want %d", got, tc.wantFailed)
			}
		})
	}
}

// TestReleaseLease_ACancelledAttachStillReleases: a cancelled attach reaches Stop with no record ids, and must not
// release a tombstone (#966).
func TestReleaseLease_ACancelledAttachStillReleases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		settle     string
		older      bool
		wantCalls  int
		wantSent   int32
		wantFailed int32
		wantID     bool
	}{
		{name: "no id on the manager, the record is in the store", wantCalls: 1, wantSent: 1, wantID: true},
		{name: "the newest record under the key is a tombstone", settle: "retained", wantFailed: 1},
		{name: "the newest record under the key is already closed", settle: "closed", wantFailed: 1},
		{name: "an earlier closed record sits under the same key", older: true, wantCalls: 1, wantSent: 1, wantID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			sender := installSender(t, nil)
			if tc.older {
				withRecords(t, p)
				p.closeRecord(p.recordCreated("net1", releaseTestMAC,
					dhcp.ClientIdentity([]byte{9, 9, 9})))
			}
			m := releasingManager(t, p, ReleaseOnStop, false)

			id := m.recordID
			switch tc.settle {
			case "retained":
				p.recordRetained(id, time.Now().Add(tombstoneTTL))
			case "closed":
				p.closeRecord(id)
			}
			m.MacAddress = releaseTestMAC
			m.recordID = ""

			if err := m.StopForLeave(); err != nil {
				t.Fatalf("StopForLeave: %v", err)
			}

			if got := sender.callCount(); got != tc.wantCalls {
				t.Errorf("the wire saw %d datagram(s), want %d", got, tc.wantCalls)
			}
			if got := p.releasesSentV4.Load(); got != tc.wantSent {
				t.Errorf("releases_sent_v4 = %d, want %d", got, tc.wantSent)
			}
			if got := p.releaseFailuresV4.Load(); got != tc.wantFailed {
				t.Errorf("release_failures_v4 = %d, want %d", got, tc.wantFailed)
			}
			if got := m.recordID == id; got != tc.wantID {
				t.Errorf("the manager names the record it released = %v, want %v "+
					"(recordID %q, the record in the store %q)", got, tc.wantID, m.recordID, id)
			}
			if tc.wantCalls > 0 && sender.callCount() > 0 {
				if got := sender.recs[0].ID; got != id {
					t.Errorf("the datagram was built from record %q, want %q", got, id)
				}
			}
		})
	}
}

func TestReleaseLease_TheFallbackReadsEachFamilysOwnScope(t *testing.T) {
	var ledgerFailures atomic.Int32
	p := &Plugin{}
	p.ledger = testLedger(t, &ledgerFailures)

	sender := installSender(t, nil)
	m := releasingManager(t, p, ReleaseOnStop, true)
	id4, id6 := m.recordID, m.recordID6
	if id4 == id6 {
		t.Fatal("the two families share a record id; this test cannot tell the scopes apart")
	}
	m.MacAddress = releaseTestMAC
	m.recordID, m.recordID6 = "", ""

	if err := m.StopForLeave(); err != nil {
		t.Fatalf("StopForLeave: %v", err)
	}

	if got := sender.callCount(); got != 2 {
		t.Fatalf("the wire saw %d datagram(s), want 2 (one per family)", got)
	}
	if got := sender.recs[0].ID; got != id4 {
		t.Errorf("the v4 datagram was built from record %q, want the v4 record %q", got, id4)
	}
	if got := sender.recs[1].ID; got != id6 {
		t.Errorf("the v6 datagram was built from record %q, want the v6 record %q", got, id6)
	}
	if m.recordID != id4 || m.recordID6 != id6 {
		t.Errorf("the manager names records %q/%q after the release, want %q/%q: "+
			"Leave settles each family through its own field",
			m.recordID, m.recordID6, id4, id6)
	}
}

func TestReleaseLease_TheSenderIsCalledOnceAndOnlyFromTheReleasePath(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sites []string
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "rtSendRelease" {
					return true
				}
				sites = append(sites, fn.Name.Name)
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("no production sources parsed; this test would pass vacuously")
	}
	if len(sites) != 1 || sites[0] != "releaseFromRecord" {
		t.Fatalf("rtSendRelease is called from %v; want exactly one call, in releaseFromRecord. "+
			"A second caller sends a release that no counter, no record phase and no tombstone "+
			"skip in this package knows about.", sites)
	}
}

func TestReleaseLease_TheRetainedRecordIsNeverAnOlderOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		closeLast bool
		wantOlder lease.Phase
		wantNewer lease.Phase
	}{
		{"the newest record was released", true, lease.PhaseLeft, lease.PhaseClosed},
		{"nothing was released", false, lease.PhaseLeft, lease.PhaseRetained},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := recordingPlugin(t)
			mac, _ := net.ParseMAC("02:42:ac:11:00:07")
			identity := dhcp.ClientIdentity([]byte{9, 8, 7})

			older := p.recordCreated("net1", mac, identity)
			newer := p.recordCreated("net1", mac, identity)
			if older == "" || newer == "" {
				t.Fatal("no record was created")
			}
			for _, id := range []string{older, newer} {
				p.recordBound(id, "created")
				p.recordLeft(id)
			}
			if tc.closeLast {
				p.closeRecord(newer)
			}

			p.retainRecordFor("net1", mac)

			rb, err := p.records.Rebuilt()
			if err != nil {
				t.Fatalf("Rebuilt: %v", err)
			}
			for _, f := range []struct {
				which string
				id    string
				want  lease.Phase
			}{
				{"older", older, tc.wantOlder},
				{"newer", newer, tc.wantNewer},
			} {
				rec, ok := rb.ByID(f.id)
				if !ok {
					t.Fatalf("the %s record is not in the fold", f.which)
				}
				if rec.Phase != f.want {
					t.Errorf("the %s record is %s, want %s", f.which, rec.Phase, f.want)
				}
			}
		})
	}
}

func TestReleaseLease_EveryReasonForNotSendingIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sendErr  error
		noRecord bool
		noSource bool
		want     releaseOutcome
	}{
		{name: "the datagram went out", want: releaseSent},
		{name: "no record for this family", noRecord: true, want: releaseNoRecord},
		{name: "no address on the parent", noSource: true, want: releaseNoSource},
		{name: "the record holds no address", sendErr: lease.ErrReleaseNoAddr, want: releaseNoAddress},
		{name: "the record names no server", sendErr: lease.ErrReleaseNoServer, want: releaseNoServer},
		{name: "the record is in no known family", sendErr: lease.ErrReleaseFamily, want: releaseBadRecord},
		{name: "the v6 record carries no DUID and IAID", sendErr: lease.ErrReleaseNoIdentity, want: releaseBadRecord},
		{name: "the record's two IAIDs disagree", sendErr: lease.ErrReleaseIAIDMismatch, want: releaseBadRecord},
		{name: "the source is in the other family", sendErr: runtime.ErrReleaseSourceFamily, want: releaseNoSource},
		{name: "the source is the released address", sendErr: runtime.ErrReleaseSourceIsReleased, want: releaseNoSource},
		{name: "a v6 release with no interface", sendErr: runtime.ErrReleaseNoInterface, want: releaseNoSource},
		{name: "the socket refused it", sendErr: errors.New("sendto: network is unreachable"), want: releaseSendFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}

			prevLevel := log.GetLevel()
			log.SetLevel(log.DebugLevel)
			t.Cleanup(func() { log.SetLevel(prevLevel) })
			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()

			installSender(t, &fakeSender{err: tc.sendErr})
			m := releasingManager(t, p, ReleaseOnStop, false)
			if tc.noRecord {
				m.recordID = ""
			}
			if tc.noSource {
				hostParent(t, "fe80::2/64")
			}

			if got := m.releaseFamily(false); got != (tc.want == releaseSent) {
				t.Errorf("releaseFamily(v4) = %v, want %v", got, tc.want == releaseSent)
			}

			wantSent, wantFailed := int32(0), int32(1)
			if tc.want == releaseSent {
				wantSent, wantFailed = 1, 0
			}
			if got := p.releasesSentV4.Load(); got != wantSent {
				t.Errorf("releases_sent_v4 = %d, want %d", got, wantSent)
			}
			if got := p.releaseFailuresV4.Load(); got != wantFailed {
				t.Errorf("release_failures_v4 = %d, want %d", got, wantFailed)
			}

			var said []string
			for _, e := range hook.AllEntries() {
				v, ok := e.Data["outcome"]
				if !ok {
					continue
				}
				said = append(said, v.(string))
				wantLevel := log.WarnLevel
				if tc.want == releaseSent || tc.want == releaseNoAddress {
					wantLevel = log.DebugLevel
				}
				if e.Level != wantLevel {
					t.Errorf("the %q line is at %s, want %s: a stop that hands nothing back "+
						"on a releasing network is a warning, and a stop that had nothing to "+
						"hand back is not", v, e.Level, wantLevel)
				}
			}
			if len(said) != 1 || said[0] != string(tc.want) {
				t.Errorf("the log named outcomes %v, want exactly [%s]", said, tc.want)
			}
		})
	}
}
