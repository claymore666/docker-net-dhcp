// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func newHealthPlugin() *Plugin {
	return &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
}

func TestConflictWiring_EveryModeReachesTheClientOptions(t *testing.T) {
	p := newHealthPlugin()

	for _, name := range dhcp.ConflictModes() {
		want, err := dhcp.ParseConflictCheck(name)
		if err != nil {
			t.Fatalf("ParseConflictCheck(%q): %v", name, err)
		}
		var o dhcp.DHCPClientOptions
		if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: name}, roleAcquire, "net", "ep", false); err != nil {
			t.Fatalf("conflictWiring(%q): %v", name, err)
		}
		if o.ConflictMode != want {
			t.Errorf("conflict_check=%q reached the client as %v, want %v", name, o.ConflictMode, want)
		}
		if o.ConflictMode.String() != name {
			t.Errorf("conflict_check=%q round-tripped as %q", name, o.ConflictMode.String())
		}
		if o.OnConflict == nil || o.OnACDStats == nil {
			t.Errorf("conflict_check=%q left a callback nil; the counters would never move", name)
		}
	}
}

func TestConflictWiring_TheUnsetOptionIsTheLibrarySOwnDefault(t *testing.T) {
	p := newHealthPlugin()
	var o dhcp.DHCPClientOptions
	if err := p.conflictWiring(&o, DHCPNetworkOptions{}, roleAcquire, "net", "ep", false); err != nil {
		t.Fatalf("conflictWiring: %v", err)
	}
	var libraryDefault proto.Params
	if o.ConflictMode != libraryDefault.Conflict {
		t.Errorf("an unset conflict_check gave %v; the library's Params default is %v",
			o.ConflictMode, libraryDefault.Conflict)
	}
	if dhcp.DefaultConflictCheck != libraryDefault.Conflict.String() {
		t.Errorf("DefaultConflictCheck is %q; the library's default mode is %q",
			dhcp.DefaultConflictCheck, libraryDefault.Conflict.String())
	}
}

func TestConflictWiring_AStoredNetworkWithNoOptionReadsAsTheDefault(t *testing.T) {
	mode, err := dhcp.ParseConflictCheck("")
	if err != nil {
		t.Fatalf("an absent conflict_check was refused: %v", err)
	}
	if mode.String() != dhcp.DefaultConflictCheck {
		t.Errorf("an absent conflict_check gave %v, want %v", mode, dhcp.DefaultConflictCheck)
	}
}

func TestConflictWiring_AnUnknownModeIsRefusedAndNamesTheAlternatives(t *testing.T) {
	_, err := dhcp.ParseConflictCheck("waite")
	if err == nil {
		t.Fatal("conflict_check=waite was accepted")
	}
	for _, name := range dhcp.ConflictModes() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %q: %v", name, err)
		}
	}

	p := newHealthPlugin()
	var o dhcp.DHCPClientOptions
	if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: "waite"}, roleAcquire, "net", "ep", false); err == nil {
		t.Error("a corrupt persisted conflict_check started a client anyway")
	}
}

func TestConflictWiring_ANilPluginStillSetsTheMode(t *testing.T) {
	var p *Plugin
	var o dhcp.DHCPClientOptions
	name := ""
	for _, m := range dhcp.ConflictModes() {
		if m != dhcp.DefaultConflictCheck {
			name = m
			break
		}
	}
	if name == "" {
		t.Fatal("no non-default mode to drive with")
	}
	if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: name}, roleAcquire, "net", "ep", false); err != nil {
		t.Fatalf("conflictWiring on a nil plugin: %v", err)
	}
	if o.ConflictMode.String() != name {
		t.Errorf("a nil plugin dropped the mode: got %v, want %q", o.ConflictMode, name)
	}
	if o.OnConflict != nil || o.OnACDStats != nil {
		t.Error("a nil plugin installed a callback that would dereference it")
	}
}

func TestConflictReporter_CountsOncePerConflict(t *testing.T) {
	p := newHealthPlugin()
	report := p.conflictReporter("net", "ep", false)

	report(dhcp.Conflict{Held: false})
	if got := p.addressConflictsV4.Load(); got != 1 {
		t.Fatalf("a probe-window conflict bumped address_conflicts_v4 to %d, want 1", got)
	}
	report(dhcp.Conflict{Held: true, Addr: "192.0.2.5"})
	if got := p.addressConflictsV4.Load(); got != 2 {
		t.Fatalf("a section 2.4 conflict bumped address_conflicts_v4 to %d, want 2", got)
	}
	if got := p.addressConflictsV6.Load(); got != 0 {
		t.Fatalf("two v4 conflicts moved address_conflicts_v6 to %d, want 0", got)
	}
}

func TestConflictReporter_SplitsTheFamilies(t *testing.T) {
	p := newHealthPlugin()

	p.conflictReporter("net", "ep", true)(dhcp.Conflict{Held: true, Addr: "2001:db8::5"})
	if got := p.addressConflictsV6.Load(); got != 1 {
		t.Errorf("a DHCPv6 conflict bumped address_conflicts_v6 to %d, want 1", got)
	}
	if got := p.addressConflictsV4.Load(); got != 0 {
		t.Errorf("a DHCPv6 conflict bumped address_conflicts_v4 to %d; that half is what "+
			"acd_conflicts_detected is compared against, and ARP never saw this conflict", got)
	}

	p.conflictReporter("net", "ep", false)(dhcp.Conflict{Held: true, Addr: "192.0.2.5"})
	h := p.healthSnapshot()
	if h.AddressConflictsV4 != 1 || h.AddressConflictsV6 != 1 {
		t.Errorf("health document reports v4=%d v6=%d, want 1 and 1",
			h.AddressConflictsV4, h.AddressConflictsV6)
	}
	if h.AddressConflicts != 2 {
		t.Errorf("address_conflicts=%d, want 2: the aggregate is the sum of the two halves",
			h.AddressConflicts)
	}
	if h.Healthy {
		t.Error("healthy is still true with two conflicts recorded")
	}
}

func TestConflictReporter_AV6OnlyConflictIsUnhealthyAndStamped(t *testing.T) {
	p := newHealthPlugin()
	p.conflictReporter("net", "ep", true)(dhcp.Conflict{Held: true, Addr: "2001:db8::5"})

	h := p.healthSnapshot()
	if h.AddressConflicts != 1 {
		t.Errorf("address_conflicts=%d after one v6 conflict, want 1", h.AddressConflicts)
	}
	if h.Healthy {
		t.Error("healthy is true although a container holds an address another node on the link owns")
	}
	if stamp := p.checkStamps()["address_conflicts"]; stamp.IsZero() {
		t.Error("address_conflicts has no stamp after a v6 conflict; laterOf reads one half only")
	}
}

func TestConflictReporter_LogsTheMessageOfItsOwnFamily(t *testing.T) {
	p := newHealthPlugin()
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p.conflictReporter("net", "ep", true)(dhcp.Conflict{Held: true, Addr: "2001:db8::5"})
	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("a DHCPv6 conflict logged nothing at all")
	}
	if !strings.Contains(entry.Message, "RFC 4862") || strings.Contains(entry.Message, "RFC 5227") {
		t.Errorf("a DHCPv6 conflict logged %q; the reporter lost its family on the way "+
			"to the message and named the ARP standard", entry.Message)
	}
	if got := entry.Data["family"]; got != "ipv6" {
		t.Errorf("family field is %v after a DHCPv6 conflict, want ipv6", got)
	}

	p.conflictReporter("net", "ep", false)(dhcp.Conflict{Held: true, Addr: "192.0.2.5"})
	entry = hook.LastEntry()
	if !strings.Contains(entry.Message, "RFC 5227") || strings.Contains(entry.Message, "RFC 4862") {
		t.Errorf("a DHCPv4 conflict logged %q, which is not the ARP line the operator "+
			"documentation points at", entry.Message)
	}
	if got := entry.Data["family"]; got != "ipv4" {
		t.Errorf("family field is %v after a DHCPv4 conflict, want ipv4", got)
	}
}

func TestConflictMessage_NamesTheProtocolThatFoundIt(t *testing.T) {
	cases := []struct {
		held, v6 bool
		must     []string
		mustNot  []string
	}{
		{false, false, []string{"RFC 5227", "was offered"}, []string{"RFC 4862", "IPv6"}},
		{true, false, []string{"RFC 5227 section 2.4", "HOLDS"}, []string{"RFC 4862", "IPv6"}},
		{false, true, []string{"RFC 4862 section 5.4", "RFC 9915 section 18.2.8", "IPv6"}, []string{"RFC 5227"}},
		{true, true, []string{"RFC 4862 section 5.4", "RFC 9915 section 18.2.8", "HOLDS", "CHANGE"}, []string{"RFC 5227"}},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		msg := conflictMessage(c.held, c.v6)
		if seen[msg] {
			t.Errorf("held=%v v6=%v repeats a message already used by another case", c.held, c.v6)
		}
		seen[msg] = true
		for _, want := range c.must {
			if !strings.Contains(msg, want) {
				t.Errorf("held=%v v6=%v: message does not carry %q:\n  %s", c.held, c.v6, want, msg)
			}
		}
		for _, no := range c.mustNot {
			if strings.Contains(msg, no) {
				t.Errorf("held=%v v6=%v: message carries %q, which is the wrong protocol for it:\n  %s",
					c.held, c.v6, no, msg)
			}
		}
	}
}

func TestACDStats_AccumulateAcrossManagers(t *testing.T) {
	p := newHealthPlugin()

	p.addACDStats(dhcp.ACDStats{ProbesSent: 3, AnnouncementsSent: 2, ConflictsDetected: 1, ARPSendFailures: 0})
	p.addACDStats(dhcp.ACDStats{ProbesSent: 3, AnnouncementsSent: 2, ConflictsDetected: 0, ARPSendFailures: 1})

	h := p.healthSnapshot()
	if h.ACDProbesSent != 6 {
		t.Errorf("acd_probes_sent = %d, want 6", h.ACDProbesSent)
	}
	if h.ACDAnnouncementsSent != 4 {
		t.Errorf("acd_announcements_sent = %d, want 4", h.ACDAnnouncementsSent)
	}
	if h.ACDConflictsDetected != 1 {
		t.Errorf("acd_conflicts_detected = %d, want 1", h.ACDConflictsDetected)
	}
	if h.ACDARPSendFailures != 1 {
		t.Errorf("acd_arp_send_failures = %d, want 1", h.ACDARPSendFailures)
	}
}

func TestACDStats_SaturateRatherThanWrap(t *testing.T) {
	p := newHealthPlugin()
	p.addACDStats(dhcp.ACDStats{ProbesSent: math.MaxUint64})
	if got := p.acdProbesSent.Load(); got != math.MaxInt32 {
		t.Errorf("a huge delta gave %d, want %d", got, int32(math.MaxInt32))
	}
	p.addACDStats(dhcp.ACDStats{ProbesSent: 5})
	if got := p.acdProbesSent.Load(); got != math.MaxInt32 {
		t.Errorf("adding past the ceiling gave %d, want it pinned at %d", got, int32(math.MaxInt32))
	}
}

func TestLeaseTimeout_TheProbeWindowBoundIsWaitOnly(t *testing.T) {
	window := dhcp.ConflictWindow(proto.DefaultACDParams())

	for _, name := range dhcp.ConflictModes() {
		mode, err := dhcp.ParseConflictCheck(name)
		if err != nil {
			t.Fatalf("ParseConflictCheck(%q): %v", name, err)
		}
		wantRefused := mode == proto.ConflictWait

		err = dhcp.CheckLeaseTimeout(window-time.Millisecond, mode)
		if (err != nil) != wantRefused {
			t.Errorf("conflict_check=%s, lease_timeout just under the %v window: err=%v, refused wanted=%v",
				name, window, err, wantRefused)
		}
		if wantRefused {
			var e dhcp.ErrLeaseTimeoutTooShort
			if !errors.As(err, &e) {
				t.Errorf("the refusal is not typed: %v", err)
			} else if !strings.Contains(err.Error(), window.String()) {
				t.Errorf("the refusal does not carry the arithmetic: %v", err)
			}
		}

		if err := dhcp.CheckLeaseTimeout(window, mode); err != nil {
			t.Errorf("conflict_check=%s refused a lease_timeout exactly equal to the window: %v", name, err)
		}
		if err := dhcp.CheckLeaseTimeout(window+time.Second, mode); err != nil {
			t.Errorf("conflict_check=%s refused a lease_timeout above the window: %v", name, err)
		}
		if err := dhcp.CheckLeaseTimeout(0, mode); err != nil {
			t.Errorf("conflict_check=%s refused an unset lease_timeout: %v", name, err)
		}
	}
}

func TestLeaseTimeout_DefaultCoversTheWorstWaitAcquisition(t *testing.T) {
	params := proto.DefaultParams(nil)
	want := dhcp.AcquisitionWindow(params)

	if defaultLeaseTimeout < want {
		t.Errorf("defaultLeaseTimeout is %v; one DISCOVER retransmission plus the RFC 5227 probe window is %v",
			defaultLeaseTimeout, want)
	}

	retransmit := time.Duration(params.Discover.Initial + params.Discover.Jitter)
	window := dhcp.ConflictWindow(proto.DefaultACDParams())
	if got := retransmit + window; got != want {
		t.Errorf("AcquisitionWindow is %v; %v (one retransmission) + %v (the probe window) is %v",
			want, retransmit, window, got)
	}
	if defaultLeaseTimeout < window {
		t.Errorf("defaultLeaseTimeout %v cannot even fund the probe window %v", defaultLeaseTimeout, window)
	}
}

func TestHTTPLimits_TheWorstCaseHandlerCarriesTheProbeWindow(t *testing.T) {
	got := socketWorstCaseHandler()
	want := linkAwaitTimeout + defaultLeaseTimeout + preflightProbeBudget
	if got != want {
		t.Errorf("socketWorstCaseHandler is %v, want %v", got, want)
	}
	if got < dhcp.ConflictWindow(proto.DefaultACDParams()) {
		t.Errorf("the worst-case handler %v does not even cover the probe window", got)
	}
}

func TestConflictWiring_TheJoinManagerNeverHoldsTheAddressBack(t *testing.T) {
	p := newHealthPlugin()

	cases := []struct {
		option string
		role   clientRole
		want   proto.ConflictMode
	}{
		{"", roleAcquire, proto.ConflictWait},
		{"", roleJoin, proto.ConflictAsync},
		{"wait", roleAcquire, proto.ConflictWait},
		{"wait", roleJoin, proto.ConflictAsync},
		{"async", roleAcquire, proto.ConflictAsync},
		{"async", roleJoin, proto.ConflictAsync},
		{"off", roleAcquire, proto.ConflictOff},
		{"off", roleJoin, proto.ConflictOff},
	}

	for _, c := range cases {
		var o dhcp.DHCPClientOptions
		if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: c.option}, c.role, "net", "ep", false); err != nil {
			t.Fatalf("conflictWiring(%q, role %d): %v", c.option, c.role, err)
		}
		if o.ConflictMode != c.want {
			t.Errorf("conflict_check=%q on role %d reached the client as %v, want %v",
				c.option, c.role, o.ConflictMode, c.want)
		}
	}

	for _, m := range proto.AllConflictModes() {
		var o dhcp.DHCPClientOptions
		if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: m.String()}, roleJoin, "net", "ep", false); err != nil {
			t.Fatalf("conflictWiring(%q, roleJoin): %v", m, err)
		}
		if o.ConflictMode == proto.ConflictWait {
			t.Errorf("conflict_check=%q gave the Join manager %v; the address is already in use at Join and holding it back costs the probe window on every container start",
				m, o.ConflictMode)
		}
	}
}

func TestLeaseTimeout_DefaultFundsOneConflictAndItsRestartDelay(t *testing.T) {
	params := proto.DefaultParams(nil)
	one := dhcp.AcquisitionWindow(params)
	restart := time.Duration(params.RestartDelay)

	if defaultLeaseTimeout < one+restart+one {
		t.Errorf("defaultLeaseTimeout is %v; one conflict costs %v (first attempt) + %v (RFC 2131 3.1(5)) + %v (second attempt) = %v",
			defaultLeaseTimeout, one, restart, one, one+restart+one)
	}
	if got := dhcp.ConflictRecoveryWindow(params); got != defaultLeaseTimeout {
		t.Errorf("defaultLeaseTimeout is %v but ConflictRecoveryWindow is %v", defaultLeaseTimeout, got)
	}
	if defaultLeaseTimeout-2*one != restart {
		t.Errorf("the default carries %v beyond two acquisitions; RFC 2131 3.1(5) is %v", defaultLeaseTimeout-2*one, restart)
	}
	if restart < 10*time.Second {
		t.Errorf("the library's restart delay is %v; RFC 2131 3.1(5) says a minimum of ten seconds", restart)
	}
}
