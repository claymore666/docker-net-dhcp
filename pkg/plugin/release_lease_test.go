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

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// fakeSender stands in for the one call that puts a datagram on the
// wire, which is the only part of the release path a unit test cannot
// run: runtime.SendRelease opens a socket on the parent.
//
// It records what it was handed, so the tests below can assert the
// RECORD and the SOURCE rather than only the fact of a call. Those two
// are where a release goes silently wrong: the wrong family's record
// releases the wrong address, and a source equal to the released
// address is the violation RFC 9915 section 18.2.7 names.
type fakeSender struct {
	mu   sync.Mutex
	recs []lease.Record
	cfgs []runtime.ReleaseConfig
	// err is what the library returns. The typed refusals go here.
	err error
	// errFor, when set, is consulted per record instead of err, so a
	// dual-stack case can fail one family and not the other. That mixed
	// outcome is the one a single shared flag gets wrong.
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

// installSender puts a fake on the wire seam for the test's lifetime.
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

// hostParent installs a parent link carrying the given addresses, so
// hostSourceFor has something to choose from without CAP_NET_ADMIN.
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

// withRecords gives a plugin a durable record store if it has none.
// Every release is built from one now, so a *Plugin{} with no store
// releases nothing at all.
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

// releaseTestMAC is the endpoint MAC every record below is keyed on.
var releaseTestMAC = net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

// releaseTestIdentity6 is a DUID-LL plus the IAID as the record stores
// it: "the DUID and IAID as sent".
func releaseTestIdentity6() dhcp.Identity6 {
	return dhcp.Identity6{
		DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
		IAID: 0xac110002,
	}
}

// TestReleaseLease_ParseRefusesEveryValueItDoesNotImplement pins the
// option's domain at the only place it is decided.
//
// A typo that fell through to the default would be the worst available
// outcome: an operator who wrote `release_lease=on_stpo` gets a network
// that looks configured and never releases, and nothing anywhere says
// so. `on_remove` is refused BY NAME and with its own sentence, because
// it is #962's own third value and "not a value this plugin implements"
// would read as a typo rather than as a decision.
func TestReleaseLease_ParseRefusesEveryValueItDoesNotImplement(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
		// mentions are substrings the refusal must carry, so a refusal
		// that says nothing useful fails here rather than in a support
		// thread.
		mentions []string
	}{
		{in: "", want: ReleaseNever},
		{in: "never", want: ReleaseNever},
		{in: "on_stop", want: ReleaseOnStop},
		{in: "on_remove", wantErr: true, mentions: []string{
			// The measured reason, and the two things the wording is
			// required to do beside it: say the value is not there yet
			// and say when it arrives. A message that said the value
			// does not exist would be false about a mechanism already
			// scoped on this milestone.
			"STOPS, not when the container is removed",
			"not available yet",
			"it arrives in the next change on this milestone",
		}},
		{in: "On_Stop", wantErr: true, mentions: []string{"is not one of"}},
		{in: "on_stpo", wantErr: true, mentions: []string{"is not one of"}},
		{in: "ON_REMOVE", wantErr: true, mentions: []string{"is not one of"}},
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

// TestReleaseLease_TheCreateAndStoredPathsRefuseTheSameSet is the
// second half of that refusal, and the reason it is a separate test is
// that the two paths are reached from different directions.
//
// validateModeOptions runs at `docker network create`. checkStoredOptions
// runs on every endpoint call against the record on disk, which is what
// a network created before this option existed replays -- and what an
// operator editing the stored options by hand produces. A value refused
// at create and accepted on replay would be a network that releases
// because nobody looked at it twice.
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

// releasingManager is a manager that holds one live client per family
// and is ready to be stopped.
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

// TestReleaseLease_OnlyALeaveOnAReleasingNetworkReleases drives the two
// guards on the send together, because each one alone is satisfied by
// the other's mutant.
//
// The `leaving` half is the one with teeth. Stop() and StopForLeave()
// differ by one bool, and everything that is NOT a container leaving
// its sandbox arrives through Stop with its container still running:
// `docker plugin disable` routes Plugin.Close there, so does a manager
// displaced by a newer one for the same endpoint, and so does the
// cleanup after `docker network rm`. A release on that path tells the
// server an address is free while a live container is using it.
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

// TestReleaseLease_TheCounterFollowsTheSendNotTheIntent is the
// counter's own observer, driven over the states the sender
// distinguishes.
//
// A plugin counter folded from "we tried" would read the same in all
// three, and an operator alerting on release_failures would see a clean
// zero on a host that handed nothing back. The sender returns every
// failure precisely so this counter can be honest; a swallowed write
// error would close the record on an address that stays leased.
func TestReleaseLease_TheCounterFollowsTheSendNotTheIntent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// sendErr is what the library returns; noRecord drops the
		// record instead, which is refused before any send.
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

// TestReleaseLease_TheReleaseIsBuiltFromThisFamilysRecord is the
// wiring nothing else reads: WHICH record each family's release is
// built from, and WHICH address it is sent from.
//
// Both are silent when wrong. A v6 release built from the v4 record
// releases an address the container never had, from a source in the
// wrong family, and every counter and phase in this file still reads
// correct. The library refuses the mismatched pair, which is the
// backstop; this is the observer that says the plugin never hands it
// one.
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

// TestReleaseLease_TheSourceIsNeverTheReleasedAddress drives
// hostSourceFor's rule, per family, including the cases that refuse.
//
// The v6 half is RFC 9915 section 18.2.7's second MUST NOT: "The client
// MUST NOT use any of the addresses it is releasing as the source
// address in the Release message." The library refuses the violation
// too, and that is deliberate belt and braces; what it cannot do is
// pick a legal source, because it does not know which link is the
// parent or which address is being given back.
func TestReleaseLease_TheSourceIsNeverTheReleasedAddress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		v6    bool
		addrs []string
		// want is the source, or "" for a refusal.
		want string
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
			// stoppingManager puts 192.168.99.50 and fd00::50 on the
			// endpoint, so the third row's first candidate is exactly
			// the address being given back.
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

// TestReleaseLease_AFailedV6WithdrawalSendsNothing is RFC 9915 section
// 18.2.7 as an executable check: "The client MUST stop using all of the
// leases being released before the client begins the Release message
// exchange process. For an address, this means the address MUST have
// been removed from the interface."
//
// The failure arm is the one that matters. A release sent while the
// address is still on the link is a client telling the server it has
// stopped using an address it is at that moment still configured with,
// and the plugin cannot honour the MUST after the fact. Not sending
// leaves the address to expire on the server's clock, which is what a
// `never` network does on every teardown.
func TestReleaseLease_AFailedV6WithdrawalSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		delErr       error
		wantCalls    int
		wantSent     int32
		wantFailures int32
		// wantOutcome is the REASON, read beside the count. Without it
		// the three arms are one bool and a wrong reason in the log is
		// invisible here.
		wantOutcome releaseOutcome
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
			// The link is being torn down around this call. An address
			// that is already gone satisfies the MUST by being absent.
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

// TestReleaseLease_TheRecordIsClosedPerFamily pins the phase each
// family's record ends in, and it drives the MIXED outcome because that
// is the one a single shared flag gets wrong.
//
// A record left LEFT after its address went back is re-bindable: the
// next start resumes it, sends an INIT-REBOOT naming an address the
// server has already returned to its pool, and the container comes up
// on an address that may by then belong to someone else. A record
// CLOSED after a release that never happened is the mirror mistake --
// this endpoint still holds that lease and may still resume it.
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
			// Both records are JOINED, which is where a teardown finds
			// them: the fold accepts LEFT only from there, so a test
			// that skipped the bind would be asserting on a rejected
			// event rather than on a phase.
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

// TestReleaseLease_AReleasedEndpointLeavesNothingBehind is the
// tombstone half, in both directions.
//
// A tombstone hands this endpoint's MAC AND its addresses to whichever
// container starts next on the network inside the TTL. After a release
// those addresses are back in the server's pool, so the inheritance
// would have the next container ask for an address that may already
// belong to someone else -- #524's duplicate assignment, manufactured
// by the plugin. The not-released direction is what stops the fix from
// being "stop writing tombstones".
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
			// THE MIXED OUTCOME, and it is the one that says why the
			// record's tombstone phase is NOT guarded on the
			// fingerprint flag. The tombstone is one object carrying
			// both addresses, so the v6 release suppresses it; the v4
			// lease is still outstanding and its record must be
			// retained exactly as under `never`, or a restart inside
			// the TTL stops finding the address this endpoint still
			// holds.
			"only the v6 lease went back: the v4 record still gets its tombstone phase", false, true,
			false, lease.PhaseRetained, lease.PhaseClosed,
		},
		{
			// THE SAME MIXED OUTCOME THE OTHER WAY ROUND, and it is
			// here because the two are not symmetric in the code that
			// produces them. retainRecordFor walks the two scopes in
			// one loop, v4 first; a CLOSED record is skipped and the
			// walk goes on to the next family. Skip written as a stop
			// -- `break` for `continue` -- is invisible from the row
			// above, where the CLOSED record is the last one visited
			// and stopping and skipping do the same thing. With the
			// families the other way round it is the difference
			// between the live v6 record getting its tombstone phase
			// and never being looked at.
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
			// The fact, not the option: Leave settles each family's
			// record from what actually left the host, and marks the
			// endpoint when either of them did.
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

// TestReleaseLease_LeaveSettlesTheWholeEndpoint drives Plugin.Leave
// itself, and it exists because everything else in this file drives one
// consequence at a time.
//
// The release, the two record phases and the fingerprint flag are four
// effects of one event, wired together in Leave and nowhere else. A
// test per effect leaves the WIRING unobserved: dropping
// markEndpointReleased, or settling the v6 record from the v4 outcome,
// changes nothing any of the tests above reads. This is the one that
// goes red for those.
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
			// The mixed outcome. One shared flag would settle both
			// records the same way, and the v6 lease this endpoint
			// still holds would stop being resumable.
			name: "only v4 got out", value: ReleaseOnStop, v4OK: true, v6OK: false,
			wantReleased: true, wantV4Phase: lease.PhaseClosed, wantV6Phase: lease.PhaseLeft,
		},
		{
			// The other half of the mixed outcome, and it is the one
			// an "any" that forgot the v6 half still passes: the
			// tombstone carries both addresses, so a v6 lease that
			// went back must suppress it even though the v4 lease
			// did not move.
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

// TestReleaseLease_AFailedStartStillReleases is the population finding
// 2 was about, and its expectation is the INVERSE of the one this test
// carried while the release was asked of a running client.
//
// The shape: a container stopped before its persistent client attached
// or bound. That is `docker run --rm` and it is what the option exists
// for. While the release was asked of a client, this endpoint had none
// to ask, so nothing was sent, the address stayed leased upstream for
// its whole lease time, and the plugin charged itself a release
// failure for a release it could never have made.
//
// Built from the record, the client's state stops mattering. The
// address the container used came from CreateEndpoint's one-shot, the
// one-shot wrote it into this same record, and the record is still
// there after the client that never started is gone. So the release
// goes out and the counter says sent.
//
// The `never` row is the control: the option still decides whether
// anything is attempted at all.
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

// TestReleaseLease_ACancelledAttachStillReleases drives the one state
// where the record exists and the manager holds no id for it.
//
// MEASURED on CI (main-7-suite, PR #966 round 2): an endpoint whose
// attach was cancelled because it was already leaving reaches Stop with
// both record ids empty, because they are assigned in setupClient and
// the cancelled attach returns before it runs. The release then found
// no record, sent no datagram, bumped release_failures_v4 and left the
// address leased on the server, on a network that had asked for it
// back. The record was in the store the whole time: CreateEndpoint's
// one-shot wrote the lease into it.
//
// The second case is the direction this must NOT fail in. The index
// answers with the newest record under the key, and a TOMBSTONE is a
// record under that key belonging to an endpoint that has already gone
// — the thing a restart inherits its address from. Releasing that would
// hand back an address the next CreateEndpoint is about to promise.
func TestReleaseLease_ACancelledAttachStillReleases(t *testing.T) {
	for _, tc := range []struct {
		name string
		// settle, when set, is the phase the record is moved to before
		// the stop: a tombstone or a record already ended.
		settle string
		// older adds a CLOSED record under the same key AHEAD of the
		// live one, which is the shape a restart leaves behind.
		older      bool
		wantCalls  int
		wantSent   int32
		wantFailed int32
		// wantID is whether the manager ends up naming the record it
		// released. Leave settles the record through that field
		// (Plugin.Leave, settleReleasedRecord), so a release built off
		// a record the manager does not name is a release whose record
		// is never closed.
		wantID bool
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
			// The state the cancelled attach leaves: the MAC arrived on
			// the join hint (Plugin.Join sets it in every mode) and
			// setupClient never ran, so there is no id.
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

// TestReleaseLease_TheFallbackReadsEachFamilysOwnScope is the same
// cancelled-attach state on a dual-stack endpoint.
//
// The v6 record is a SECOND record under a SECOND scope (dhcp.Scope6),
// so a fallback that looked both families up under the network id would
// build the v6 Release from the v4 record. The library refuses that as
// a family mismatch, which makes the mistake visible; a fallback that
// refused nothing would hand the wrong address back.
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

// TestReleaseLease_TheSenderIsCalledOnceAndOnlyFromTheReleasePath is the
// CALL SITE, in the shape the retired setReleaseClient test had.
//
// rtSendRelease is the one line that puts a datagram on a socket. A
// second caller anywhere in this package would be a release this file's
// counters, phases and tombstone logic never see, and the first thing
// an operator would know about it is an address disappearing from the
// server while a container is still using it.
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
	if len(sites) != 1 || sites[0] != "releaseHeldLease" {
		t.Fatalf("rtSendRelease is called from %v; want exactly one call, in releaseHeldLease. "+
			"A second caller sends a release that no counter, no record phase and no tombstone "+
			"skip in this package knows about.", sites)
	}
}

// TestReleaseLease_TheRetainedRecordIsNeverAnOlderOne pins which record
// DeleteEndpoint stamps its tombstone deadline on.
//
// Two records can share one scope and MAC: a teardown whose release
// FAILED leaves its record LEFT and lays a tombstone, so the next
// container inherits the MAC, and that container's own teardown can
// then release successfully and close its record. A lookup that walks
// PAST the closed record answers with the older one, which carries the
// address that has just been handed back, and stamping a fresh deadline
// on it keeps it answering lookups for an address in the server's pool.
// The `never` row is the control: with nothing closed, the newest
// record is retained exactly as before.
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

// TestReleaseLease_EveryReasonForNotSendingIsNamed drives the outcomes
// apart.
//
// WHY THE REASON IS A TESTED VALUE AND NOT JUST LOG TEXT. A stop on a
// `release_lease=on_stop` network that hands nothing back has several
// unrelated causes, and they used to share one bare false. The counter
// pair deliberately does not tell them apart, so the outcome is the
// only place the difference survives, and each is a different thing for
// an operator to do: a record that names no server is a server that
// never sent option 54, no source is a parent with no address in that
// family, a bad record is a plugin defect, and a send failure is the
// socket.
func TestReleaseLease_EveryReasonForNotSendingIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// one of these three shapes the case.
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

			// The counters stay a two-way split: every outcome but the
			// send is one failure, whatever its reason.
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

			// THE LINE AN OPERATOR READS. Without this the whole
			// outcome type is a value nothing outside the package can
			// see: deleting the announcement leaves every count and
			// every return value correct.
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
