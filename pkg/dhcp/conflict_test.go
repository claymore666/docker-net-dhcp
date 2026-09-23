// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

const (
	wantWait  = "wait"
	wantAsync = "async"
	wantOff   = "off"
)

func TestConflictModes_AreTheLibrarySThree(t *testing.T) {
	got := ConflictModes()
	want := []string{wantWait, wantAsync, wantOff}
	if len(got) != len(want) {
		t.Fatalf("ConflictModes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ConflictModes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if DefaultConflictCheck != wantWait {
		t.Errorf("DefaultConflictCheck = %q, want %q", DefaultConflictCheck, wantWait)
	}
}

func TestParseConflictCheck_MapsEachNameToItsOwnMode(t *testing.T) {
	cases := map[string]proto.ConflictMode{
		"":        proto.ConflictWait,
		wantWait:  proto.ConflictWait,
		wantAsync: proto.ConflictAsync,
		wantOff:   proto.ConflictOff,
	}
	for in, want := range cases {
		got, err := ParseConflictCheck(in)
		if err != nil {
			t.Errorf("ParseConflictCheck(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseConflictCheck(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"WAIT", "Async", "none", "true", "0", " wait"} {
		if _, err := ParseConflictCheck(bad); err == nil {
			t.Errorf("ParseConflictCheck(%q) was accepted", bad)
		}
	}
}

// RFC 5227 sections 2.1.1 and 2.1, written out here a second time so the test does not re-call the function under test.

func TestConflictWindow_IsRFC5227sArithmetic(t *testing.T) {
	p := proto.DefaultACDParams()
	got := ConflictWindow(p)

	// RFC 5227 section 2.1.1: PROBE_WAIT 1s + (PROBE_NUM-1=2) * PROBE_MAX 2s + ANNOUNCE_WAIT 2s.
	want := 1*time.Second + 2*2*time.Second + 2*time.Second
	if got != want {
		t.Errorf("ConflictWindow(defaults) = %v, want %v", got, want)
	}
	if want != 7*time.Second {
		t.Fatalf("the RFC's own table gives %v, not 7s; the fixture is wrong", want)
	}

	// RFC 5227 section 2.1 completes ANNOUNCE_WAIT after the last probe; a 5s window would let a 6s lease_timeout past
	// the refusal (#882).
	noSettle := p
	noSettle.AnnounceWait = 0
	if ConflictWindow(noSettle) != want-2*time.Second {
		t.Error("ANNOUNCE_WAIT does not contribute to the window")
	}

	// RFC 5227 section 2.3 releases the address at the first announcement, so announcements are not in the window.
	more := p
	more.AnnounceNum, more.AnnounceInterval = 9, 9*proto.Second
	if ConflictWindow(more) != want {
		t.Error("the announcement schedule leaked into the probe window")
	}
}

func TestAcquisitionWindow_IsOneRetransmissionPlusTheProbeWindow(t *testing.T) {
	p := proto.DefaultParams(nil)
	got := AcquisitionWindow(p)

	// RFC 2131 section 4.1: "four seconds randomized by ... -1 to +1".
	retransmit := 4*time.Second + 1*time.Second
	want := retransmit + 7*time.Second
	if got != want {
		t.Errorf("AcquisitionWindow(defaults) = %v, want %v", got, want)
	}
	if want != 12*time.Second {
		t.Fatalf("the arithmetic gives %v, not the 12.0s the M6 review measured", want)
	}

	bare := p
	bare.ACD = proto.ACDParams{}
	if AcquisitionWindow(bare) != want {
		t.Errorf("an unset Params.ACD gave %v, want %v", AcquisitionWindow(bare), want)
	}
}

// proto.Params.RestartDelay zero means the library's 10s default, not an immediate restart (#882).

func TestConflictRecoveryWindow_AZeroRestartDelayIsTheDefaultNotZero(t *testing.T) {
	one := AcquisitionWindow(proto.DefaultParams(nil))

	filled := proto.DefaultParams(nil)
	if filled.RestartDelay == 0 {
		t.Fatalf("this test's control is gone: proto.DefaultParams no longer fills " +
			"RestartDelay, so the two cases below are the same case")
	}
	want := one + time.Duration(filled.RestartDelay) + one
	if got := ConflictRecoveryWindow(filled); got != want {
		t.Fatalf("ConflictRecoveryWindow(defaults) = %v, want %v", got, want)
	}

	bare := proto.DefaultParams(nil)
	bare.RestartDelay = 0
	got := ConflictRecoveryWindow(bare)
	if got != want {
		t.Errorf("a zero RestartDelay derived %v, want %v (the same as an explicit %v). "+
			"Zero means the library's default, not no wait: RFC 2131 section 3.1(5) makes "+
			"the client wait ten seconds after a DHCPDECLINE whatever this field says, so a "+
			"deadline derived without it gives up mid-recovery.",
			got, want, time.Duration(proto.DefaultRestartDelay))
	}
	if got-2*one != time.Duration(proto.DefaultRestartDelay) {
		t.Errorf("the restart term resolved to %v, want the library's default %v",
			got-2*one, time.Duration(proto.DefaultRestartDelay))
	}

	neg := proto.DefaultParams(nil)
	neg.RestartDelay = -1
	if got := ConflictRecoveryWindow(neg); got != want {
		t.Errorf("a negative RestartDelay derived %v, want %v", got, want)
	}
}

// The library emits Failed{ReasonConflict} or Lost{ReasonConflict} for one conflict, never both (#882).

const (
	testAddr   = "192.168.99.50"
	testServer = "192.168.99.1"
)

var (
	ourMAC   = net.HardwareAddr{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02}
	theirMAC = net.HardwareAddr{0x02, 0x42, 0xAC, 0x11, 0x00, 0x63}
)

// fastACD is RFC 5227's counts with nanosecond durations.
func fastACD() proto.ACDParams {
	p := proto.DefaultACDParams()
	p.ProbeWait = 3 * proto.Nanosecond
	p.ProbeMin = 4 * proto.Nanosecond
	p.ProbeMax = 5 * proto.Nanosecond
	p.AnnounceWait = 6 * proto.Nanosecond
	p.AnnounceInterval = 7 * proto.Nanosecond
	p.DefendInterval = 8 * proto.Nanosecond
	return p
}

func acdMachine(t *testing.T, mode proto.ConflictMode) (*proto.Machine, []proto.Action) {
	t.Helper()
	p := proto.DefaultParams(ourMAC)
	p.Conflict = mode
	p.ACD = fastACD()
	p.DesyncMin, p.DesyncMax = 0, 0
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	_, acts := m.Step(0, 1, proto.Simple(proto.EvStart))
	disc := mustSend(t, acts)
	_, acts = m.Step(instant(1), 2, received(t, reply(disc, wire.MsgOffer, 3600)))
	req := mustSend(t, acts)
	_, acts = m.Step(instant(2), 3, received(t, reply(req, wire.MsgAck, 3600)))
	return m, acts
}

func mustSend(t *testing.T, acts []proto.Action) *wire.Message {
	t.Helper()
	for _, a := range acts {
		if a.Kind == proto.ActSend {
			return a.Msg
		}
	}
	t.Fatalf("no ActSend in %v", proto.RenderActions(acts))
	return nil
}

func reply(req *wire.Message, kind wire.MessageType, leaseSecs uint32) *wire.Message {
	return &wire.Message{
		Op: wire.BootReply, HType: wire.HTypeEthernet, XID: req.XID,
		YIAddr: netip.MustParseAddr(testAddr),
		CHAddr: append([]byte(nil), req.CHAddr...),
		Options: wire.Options{
			wire.OptMessageType: {byte(kind)},
			wire.OptServerID:    addr4(testServer),
			wire.OptSubnetMask:  {255, 255, 255, 0},
			wire.OptRouter:      addr4(testServer),
			wire.OptLeaseTime: {
				byte(leaseSecs >> 24), byte(leaseSecs >> 16),
				byte(leaseSecs >> 8), byte(leaseSecs),
			},
		},
	}
}

func addr4(s string) []byte {
	a := netip.MustParseAddr(s).As4()
	return a[:]
}

func received(t *testing.T, m *wire.Message) proto.Event {
	t.Helper()
	raw, err := wire.Encode(m)
	if err != nil {
		t.Fatalf("wire.Encode: %v", err)
	}
	dec, err := wire.Decode(raw)
	if err != nil {
		t.Fatalf("wire.Decode: %v", err)
	}
	return proto.Received(dec, raw)
}

func instant(n int64) proto.Instant { return proto.Instant(n) }

// squatterReply is the frame RFC 5227 sections 2.1.1 and 2.4 call a conflict.
func squatterReply() *wire.ARPPacket {
	a := netip.MustParseAddr(testAddr)
	return &wire.ARPPacket{Op: wire.ARPReply, SenderHW: theirMAC, SenderIP: a, TargetIP: a}
}

func countReason(acts []proto.Action, k proto.ActionKind, r proto.Reason) int {
	n := 0
	for _, a := range acts {
		if a.Kind == k && a.Reason == r {
			n++
		}
	}
	return n
}

func has(acts []proto.Action, k proto.ActionKind) bool {
	for _, a := range acts {
		if a.Kind == k {
			return true
		}
	}
	return false
}

func TestConflict_TheLibraryEmitsExactlyOneEventPerConflict(t *testing.T) {
	t.Run("probe window, wait", func(t *testing.T) {
		m, ackActs := acdMachine(t, proto.ConflictWait)
		if has(ackActs, proto.ActLeaseAcquired) {
			t.Fatal("wait announced the lease at the DHCPACK; the probe window is not in front of it")
		}
		_, acts := m.Step(instant(3), 4, proto.ARPReceived(squatterReply()))

		if n := countReason(acts, proto.ActFailed, proto.ReasonConflict); n != 1 {
			t.Errorf("Failed{ReasonConflict} appeared %d times, want 1: %v", n, proto.RenderActions(acts))
		}
		if n := countReason(acts, proto.ActLeaseLost, proto.ReasonConflict); n != 0 {
			t.Errorf("Lost{ReasonConflict} also appeared, %d times: the two are not exclusive and "+
				"address_conflicts would double-count: %v", n, proto.RenderActions(acts))
		}
	})

	t.Run("probe window, async", func(t *testing.T) {
		m, ackActs := acdMachine(t, proto.ConflictAsync)
		if !has(ackActs, proto.ActLeaseAcquired) {
			t.Fatal("async did not announce the lease at the DHCPACK")
		}
		_, acts := m.Step(instant(3), 4, proto.ARPReceived(squatterReply()))

		if n := countReason(acts, proto.ActLeaseLost, proto.ReasonConflict); n != 1 {
			t.Errorf("Lost{ReasonConflict} appeared %d times, want 1: %v", n, proto.RenderActions(acts))
		}
		if n := countReason(acts, proto.ActFailed, proto.ReasonConflict); n != 0 {
			t.Errorf("Failed{ReasonConflict} also appeared, %d times: %v", n, proto.RenderActions(acts))
		}
	})

	// RFC 5227 section 2.4: a conflict after the address is in use.
	t.Run("after acquisition, wait", func(t *testing.T) {
		m, _ := acdMachine(t, proto.ConflictWait)
		acquired := false
		for i := 0; i < 24 && !acquired; i++ {
			_, acts := m.Step(instant(int64(10+i)), uint64(10+i), proto.TimerFired(proto.TimerACD))
			acquired = has(acts, proto.ActLeaseAcquired)
		}
		if !acquired {
			t.Fatal("the probe schedule never produced an acquisition")
		}

		_, acts := m.Step(instant(100), 100, proto.ARPReceived(squatterReply()))
		if n := countReason(acts, proto.ActLeaseLost, proto.ReasonConflict); n != 1 {
			t.Errorf("Lost{ReasonConflict} appeared %d times, want 1: %v", n, proto.RenderActions(acts))
		}
		if n := countReason(acts, proto.ActFailed, proto.ReasonConflict); n != 0 {
			t.Errorf("Failed{ReasonConflict} also appeared, %d times: %v", n, proto.RenderActions(acts))
		}
	})

	t.Run("off, the same frame is nothing", func(t *testing.T) {
		m, _ := acdMachine(t, proto.ConflictOff)
		_, acts := m.Step(instant(3), 4, proto.ARPReceived(squatterReply()))
		if countReason(acts, proto.ActFailed, proto.ReasonConflict) != 0 ||
			countReason(acts, proto.ActLeaseLost, proto.ReasonConflict) != 0 {
			t.Errorf("conflict_check=off reported a conflict: %v", proto.RenderActions(acts))
		}
	})
}

func TestConflict_TheChassisCountsEachEventOnce(t *testing.T) {
	cases := []struct {
		name string
		ev   lease.Event
		want bool
		held bool
	}{
		{"Failed{ReasonConflict}", lease.Event{Kind: lease.Failed, Reason: proto.ReasonConflict}, true, false},
		{"Lost{ReasonConflict}", lease.Event{Kind: lease.Lost, Reason: proto.ReasonConflict}, true, true},
		{"Failed{ReasonNoServer}", lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer}, false, false},
		{"Lost{ReasonStopped}", lease.Event{Kind: lease.Lost, Reason: proto.ReasonStopped}, false, false},
		{"Lost{ReasonNak}", lease.Event{Kind: lease.Lost, Reason: proto.ReasonNak}, false, false},
		{"Acquired", lease.Event{Kind: lease.Acquired}, false, false},
		{"Renewed{ReasonConflict}", lease.Event{Kind: lease.Renewed, Reason: proto.ReasonConflict}, false, false},
	}

	for _, c := range cases {
		n := 0
		var last Conflict
		o := &DHCPClientOptions{OnConflict: func(x Conflict) { n++; last = x }}
		got := o.conflict(c.ev)
		if got != c.want {
			t.Errorf("%s: conflict() = %v, want %v", c.name, got, c.want)
		}
		want := 0
		if c.want {
			want = 1
		}
		if n != want {
			t.Errorf("%s: OnConflict called %d times, want %d", c.name, n, want)
		}
		if c.want && last.Held != c.held {
			t.Errorf("%s: Held = %v, want %v", c.name, last.Held, c.held)
		}
	}
}

// The leasefail event feeds dhcp_timeouts, which means the DHCP server went quiet (#882).

func TestTranslateOne_AConflictIsNotALeaseFailure(t *testing.T) {
	now := time.Now()
	for _, ev := range []lease.Event{
		{Kind: lease.Failed, Reason: proto.ReasonConflict},
		{Kind: lease.Lost, Reason: proto.ReasonConflict},
	} {
		out, emit, _ := translateOne(ev, now, time.Time{}, netip.Prefix{})
		if emit {
			t.Errorf("%v was emitted as %q; it must not reach the outage counter", ev.Kind, out.Type)
		}
	}

	for _, c := range []struct {
		ev   lease.Event
		want string
	}{
		{lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer}, "leasefail"},
		{lease.Event{Kind: lease.Lost, Reason: proto.ReasonExpired}, "leasefail"},
		{lease.Event{Kind: lease.Failed, Reason: proto.ReasonNak}, "nak"},
		{lease.Event{Kind: lease.Lost, Reason: proto.ReasonNak}, "nak"},
	} {
		out, emit, _ := translateOne(c.ev, now, time.Time{}, netip.Prefix{})
		if !emit || out.Type != c.want {
			t.Errorf("%v/%v translated to %q (emit=%v), want %q", c.ev.Kind, c.ev.Reason, out.Type, emit, c.want)
		}
	}
}

func TestBuildParams_TheModeReachesBothManagers(t *testing.T) {
	for _, name := range ConflictModes() {
		mode, err := ParseConflictCheck(name)
		if err != nil {
			t.Fatalf("ParseConflictCheck(%q): %v", name, err)
		}
		for _, once := range []bool{true, false} {
			p, err := buildParams(&DHCPClientOptions{MAC: ourMAC, ConflictMode: mode}, once)
			if err != nil {
				t.Fatalf("buildParams(once=%v): %v", once, err)
			}
			if p.Conflict != mode {
				t.Errorf("conflict_check=%s, once=%v: Params.Conflict = %v, want %v",
					name, once, p.Conflict, mode)
			}
		}
	}
}

// The library's own-traffic exemption is keyed on Params.CHAddr; a wrong one declines its own address (#882).

func TestBuildParams_TheCHAddrIsOptsMACAndNotTheClientID(t *testing.T) {
	p, err := buildParams(&DHCPClientOptions{MAC: ourMAC, ClientID: []byte("something-else")}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if string(p.CHAddr) != string(ourMAC) {
		t.Errorf("Params.CHAddr = %x, want the endpoint's MAC %x", p.CHAddr, ourMAC)
	}
	if strings.Contains(string(p.CHAddr), "something-else") {
		t.Error("the client-id reached Params.CHAddr")
	}
}

func TestACDStats_SubIsSaturating(t *testing.T) {
	cur := ACDStats{ProbesSent: 5, AnnouncementsSent: 2}
	if got := cur.Sub(ACDStats{ProbesSent: 2}); got.ProbesSent != 3 || got.AnnouncementsSent != 2 {
		t.Errorf("Sub gave %+v", got)
	}
	if got := cur.Sub(ACDStats{ProbesSent: 9}); got.ProbesSent != 0 {
		t.Errorf("a backwards delta gave %d, want 0", got.ProbesSent)
	}
	if !(ACDStats{}).IsZero() {
		t.Error("the zero ACDStats is not IsZero")
	}
	if (ACDStats{ARPSendFailures: 1}).IsZero() {
		t.Error("a non-zero ACDStats reported IsZero")
	}
}

func TestACDReport_IsADeltaNotASnapshot(t *testing.T) {
	var got []ACDStats
	o := &DHCPClientOptions{OnACDStats: func(d ACDStats) { got = append(got, d) }}

	o.acdReport(lease.Stats{ProbesSent: 3})
	o.acdReport(lease.Stats{ProbesSent: 3})
	o.acdReport(lease.Stats{ProbesSent: 5, ConflictsDetected: 1})

	if len(got) != 2 {
		t.Fatalf("acdReport produced %d deltas, want 2: %+v", len(got), got)
	}
	if got[0].ProbesSent != 3 {
		t.Errorf("first delta ProbesSent = %d, want 3", got[0].ProbesSent)
	}
	if got[1].ProbesSent != 2 || got[1].ConflictsDetected != 1 {
		t.Errorf("second delta = %+v, want ProbesSent 2 and ConflictsDetected 1", got[1])
	}
}

// In proto.ConflictWait the library declines per RFC 2131 section 3.1(5) and offers another address (#882).

func TestAcquireStep_AnAcquisitionEndsOnAcquiredAndNothingElse(t *testing.T) {
	leased := lease.Lease{
		Addr:     netip.MustParsePrefix("192.168.99.30/24"),
		Gateway:  netip.MustParseAddr("192.168.99.1"),
		Acquired: time.Unix(1000, 0),
		Expire:   time.Unix(1600, 0),
	}
	conflicting := lease.Lease{Addr: netip.MustParsePrefix("192.168.99.30/24")}

	cases := []struct {
		name       string
		ev         lease.Event
		conflicted bool
		wantDone   bool
		wantIP     string
		wantErr    error
		wantErrHas string
	}{
		{
			name:     "Acquired is the only thing that ends it",
			ev:       lease.Event{Kind: lease.Acquired, Lease: leased},
			wantDone: true,
			wantIP:   "192.168.99.30/24",
		},
		{
			name:       "a conflict in the probe window is recorded, not returned",
			ev:         lease.Event{Kind: lease.Failed, Reason: proto.ReasonConflict, Lease: conflicting},
			conflicted: true,
			wantDone:   false,
			wantErr:    ErrAddressConflict,
			wantErrHas: "192.168.99.30/24",
		},
		{
			name:       "a plain failure is recorded, not returned",
			ev:         lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer},
			wantDone:   false,
			wantErrHas: "acquisition failed",
		},
		{
			name:     "a loss during the one-shot window is not an end",
			ev:       lease.Event{Kind: lease.Lost, Reason: proto.ReasonConflict, Lease: conflicting},
			wantDone: false,
		},
		{
			name:     "a renewal is not an end",
			ev:       lease.Event{Kind: lease.Renewed, Lease: leased},
			wantDone: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := acquireStep(tc.ev, tc.conflicted, time.Unix(1000, 0))
			if out.Done != tc.wantDone {
				t.Fatalf("Done = %v, want %v (outcome %+v)", out.Done, tc.wantDone, out)
			}
			if tc.wantIP != "" && out.Info.IP != tc.wantIP {
				t.Errorf("Info.IP = %q, want %q", out.Info.IP, tc.wantIP)
			}
			if tc.wantErr != nil && !errors.Is(out.Err, tc.wantErr) {
				t.Errorf("Err = %v, want one wrapping %v", out.Err, tc.wantErr)
			}
			if tc.wantErrHas != "" {
				if out.Err == nil {
					t.Fatalf("Err = nil, want one naming %q", tc.wantErrHas)
				}
				if !strings.Contains(out.Err.Error(), tc.wantErrHas) {
					t.Errorf("Err = %q, want it to name %q", out.Err, tc.wantErrHas)
				}
			}
			if tc.wantErr == nil && tc.wantErrHas == "" && out.Err != nil {
				t.Errorf("Err = %v, want none", out.Err)
			}
		})
	}
}
