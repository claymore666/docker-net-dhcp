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
	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// newHealthPlugin is a Plugin with the maps a health snapshot reads and
// nothing else. It is the shape every counter test needs.
func newHealthPlugin() *Plugin {
	return &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
}

// The mode an operator types has to reach proto.Params, and it has to
// reach the RIGHT value. A map that resolved an unknown name to the
// zero value would silently give every mistyped network the default,
// which is the slowest mode and the one an operator asking for `async`
// was trying to avoid.
func TestConflictWiring_EveryModeReachesTheClientOptions(t *testing.T) {
	p := newHealthPlugin()

	for _, name := range dhcp.ConflictModes() {
		want, err := dhcp.ParseConflictCheck(name)
		if err != nil {
			t.Fatalf("ParseConflictCheck(%q): %v", name, err)
		}
		var o dhcp.DHCPClientOptions
		if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: name}, roleAcquire, "net", "ep"); err != nil {
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

// The chassis default and the library default are ONE fact. Spelled
// twice they agree until they do not, and the failure is silent: every
// network created without the option runs a mode nobody chose.
func TestConflictWiring_TheUnsetOptionIsTheLibrarySOwnDefault(t *testing.T) {
	p := newHealthPlugin()
	var o dhcp.DHCPClientOptions
	if err := p.conflictWiring(&o, DHCPNetworkOptions{}, roleAcquire, "net", "ep"); err != nil {
		t.Fatalf("conflictWiring: %v", err)
	}
	// The library's default is the zero value of the field it is read
	// off, taken here rather than named.
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

// A network stored before conflict_check existed decodes to the empty
// string, which must read as the default rather than as a refusal --
// otherwise the upgrade breaks every existing network.
func TestConflictWiring_AStoredNetworkWithNoOptionReadsAsTheDefault(t *testing.T) {
	mode, err := dhcp.ParseConflictCheck("")
	if err != nil {
		t.Fatalf("an absent conflict_check was refused: %v", err)
	}
	if mode.String() != dhcp.DefaultConflictCheck {
		t.Errorf("an absent conflict_check gave %v, want %v", mode, dhcp.DefaultConflictCheck)
	}
}

// A value that is not a mode is refused, and the error says which
// values there are. An operator who typed `waite` has to be told the
// three, not "invalid".
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
	if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: "waite"}, roleAcquire, "net", "ep"); err == nil {
		t.Error("a corrupt persisted conflict_check started a client anyway")
	}
}

// A dhcpManager built for a unit test carries no plugin. The MODE must
// still reach the wire; only the counters have nowhere to go.
func TestConflictWiring_ANilPluginStillSetsTheMode(t *testing.T) {
	var p *Plugin
	var o dhcp.DHCPClientOptions
	// The one mode that is not the zero value, so a no-op cannot pass.
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
	if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: name}, roleAcquire, "net", "ep"); err != nil {
		t.Fatalf("conflictWiring on a nil plugin: %v", err)
	}
	if o.ConflictMode.String() != name {
		t.Errorf("a nil plugin dropped the mode: got %v, want %q", o.ConflictMode, name)
	}
	if o.OnConflict != nil || o.OnACDStats != nil {
		t.Error("a nil plugin installed a callback that would dereference it")
	}
}

// One conflict is one bump on address_conflicts, from either of the two
// events the library emits, and the log line differs because the
// operator's situation does.
func TestConflictReporter_CountsOncePerConflict(t *testing.T) {
	p := newHealthPlugin()
	report := p.conflictReporter("net", "ep")

	report(dhcp.Conflict{Held: false})
	if got := p.addressConflicts.Load(); got != 1 {
		t.Fatalf("a probe-window conflict bumped address_conflicts to %d, want 1", got)
	}
	report(dhcp.Conflict{Held: true, Addr: "192.0.2.5"})
	if got := p.addressConflicts.Load(); got != 2 {
		t.Fatalf("a section 2.4 conflict bumped address_conflicts to %d, want 2", got)
	}
}

// The ACD counters accumulate DELTAS across every manager, including
// the ones that have exited. A snapshot-based implementation passes the
// first assertion and fails the second.
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

// A uint64 delta may not make an int32 counter go backwards. A plain
// cast does exactly that, and a counter that rewinds reads as a process
// restart to every scraper.
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

// THE BOUNDARY, table-driven: lease_timeout below the probe window is
// refused in wait and accepted in the other two.
//
// The `async` and `off` rows are the preservation control. A refusal
// keyed on the timeout alone would refuse them too, and the refusal
// would then be wrong for every network that chose speed deliberately.
func TestLeaseTimeout_TheProbeWindowBoundIsWaitOnly(t *testing.T) {
	window := dhcp.ConflictWindow(proto.DefaultACDParams())

	for _, name := range dhcp.ConflictModes() {
		mode, err := dhcp.ParseConflictCheck(name)
		if err != nil {
			t.Fatalf("ParseConflictCheck(%q): %v", name, err)
		}
		wantRefused := mode == proto.ConflictWait

		// Just under the window.
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

		// Exactly the window, and above it: accepted in every mode.
		if err := dhcp.CheckLeaseTimeout(window, mode); err != nil {
			t.Errorf("conflict_check=%s refused a lease_timeout exactly equal to the window: %v", name, err)
		}
		if err := dhcp.CheckLeaseTimeout(window+time.Second, mode); err != nil {
			t.Errorf("conflict_check=%s refused a lease_timeout above the window: %v", name, err)
		}
		// Unset: the derived default applies and covers the window.
		if err := dhcp.CheckLeaseTimeout(0, mode); err != nil {
			t.Errorf("conflict_check=%s refused an unset lease_timeout: %v", name, err)
		}
	}
}

// The default lease_timeout has to fund one DISCOVER retransmission AND
// the worst probe window, or `docker run` fails against a working
// server whenever a packet is lost. The old literal 10s does not.
func TestLeaseTimeout_DefaultCoversTheWorstWaitAcquisition(t *testing.T) {
	params := proto.DefaultParams(nil)
	want := dhcp.AcquisitionWindow(params)

	if defaultLeaseTimeout < want {
		t.Errorf("defaultLeaseTimeout is %v; one DISCOVER retransmission plus the RFC 5227 probe window is %v",
			defaultLeaseTimeout, want)
	}

	// The two terms, each derived here a second time from the library's
	// own constants, so a change to either is caught rather than
	// absorbed into the sum.
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

// The worst-case handler arithmetic must move with the acquisition
// budget rather than with a number somebody wrote down.
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

// TestConflictWiring_TheJoinManagerNeverHoldsTheAddressBack drives the
// one place the two client roles differ.
//
// WHY IT IS NOT DEFEAT ROW Y-11. That row is about the mode reaching
// one manager and not the other, so that half the endpoint's life is
// unprotected. Nothing here is unprotected: proto.ConflictAsync runs
// the same RFC 5227 section 2.1 probes, the same section 2.4 listener
// and sends the same DHCPDECLINE. What it does not do is hold an
// address back that CreateEndpoint has already handed to dockerd —
// there is no "before use" left at Join, and waiting for one cost the
// whole probe window on every container start (MEASURED ~6s on the
// 2.x lane 2026-09-04; it is what broke the resolv.conf and MTU
// propagation cases).
//
// The `off` row is the one that would carry Y-11 if the rule were
// written loosely: a role-dependent mode that turned `off` into
// anything else would probe a network whose operator switched probing
// off. It is asserted for both roles.
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
		if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: c.option}, c.role, "net", "ep"); err != nil {
			t.Fatalf("conflictWiring(%q, role %d): %v", c.option, c.role, err)
		}
		if o.ConflictMode != c.want {
			t.Errorf("conflict_check=%q on role %d reached the client as %v, want %v",
				c.option, c.role, o.ConflictMode, c.want)
		}
	}

	// NO ROLE EVER HOLDS AN ADDRESS BACK AT JOIN, stated over the
	// library's own enumeration so a mode added later arrives with a
	// decision rather than a default.
	for _, m := range proto.AllConflictModes() {
		var o dhcp.DHCPClientOptions
		if err := p.conflictWiring(&o, DHCPNetworkOptions{ConflictCheck: m.String()}, roleJoin, "net", "ep"); err != nil {
			t.Fatalf("conflictWiring(%q, roleJoin): %v", m, err)
		}
		if o.ConflictMode == proto.ConflictWait {
			t.Errorf("conflict_check=%q gave the Join manager %v; the address is already in use at Join and holding it back costs the probe window on every container start",
				m, o.ConflictMode)
		}
	}
}

// TestLeaseTimeout_DefaultFundsOneConflictAndItsRestartDelay is the
// 2.x lane's 2026-09-04 finding, pinned.
//
// A deadline of AcquisitionWindow alone funds an acquisition that finds
// NO conflict. The run that matters is the other one: the squatter
// answered the probe, the library DECLINEd and owed RFC 2131 section
// 3.1(5) ten seconds before asking again, and the clean address arrived
// ~11s after the first DHCPACK — 0.8s after the chassis had given up.
// `docker run` failed with a DHCP timeout while the server's log showed
// a lease allocated.
func TestLeaseTimeout_DefaultFundsOneConflictAndItsRestartDelay(t *testing.T) {
	params := proto.DefaultParams(nil)
	one := dhcp.AcquisitionWindow(params)
	restart := time.Duration(params.RestartDelay)

	if defaultLeaseTimeout < one+restart+one {
		t.Errorf("defaultLeaseTimeout is %v; one conflict costs %v (first attempt) + %v (RFC 2131 3.1(5)) + %v (second attempt) = %v",
			defaultLeaseTimeout, one, restart, one, one+restart+one)
	}
	// Derived twice on purpose: the composition above and the function
	// the plugin actually calls have to agree, or the number in the
	// docs is a third fact.
	if got := dhcp.ConflictRecoveryWindow(params); got != defaultLeaseTimeout {
		t.Errorf("defaultLeaseTimeout is %v but ConflictRecoveryWindow is %v", defaultLeaseTimeout, got)
	}
	// The restart delay is REAL in the number, not absorbed: a
	// derivation that dropped it would still be larger than one
	// acquisition and would still fail the same way on the lane.
	if defaultLeaseTimeout-2*one != restart {
		t.Errorf("the default carries %v beyond two acquisitions; RFC 2131 3.1(5) is %v", defaultLeaseTimeout-2*one, restart)
	}
	if restart < 10*time.Second {
		t.Errorf("the library's restart delay is %v; RFC 2131 3.1(5) says a minimum of ten seconds", restart)
	}
}
