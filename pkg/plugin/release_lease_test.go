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
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// fakeReleaser is a DHCP client whose SEND is separate from its CALL.
//
// That separation is the whole point of the type. The library's
// Release() does not block and reports nothing; it bumps ReleasesSent
// only where the packet actually went out, and it bumps nothing at all
// when the machine held no binding to give back. A fake that moved the
// counter inside Release() would make every test below pass against a
// plugin counter folded from intent, which is the defect this shape
// exists to catch.
type fakeReleaser struct {
	mu sync.Mutex
	// sends is how many of the next calls put a packet on the wire.
	// Zero models the states that send nothing: never bound, socket
	// error, request dropped from a full queue.
	sends int
	calls int
	stats lease.Stats
}

func (f *fakeReleaser) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.sends > 0 {
		f.sends--
		f.stats.ReleasesSent++
	}
}

func (f *fakeReleaser) Stats() lease.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *fakeReleaser) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// shortReleaseBudget keeps the wait for a send that never comes from
// costing a second per case.
func shortReleaseBudget(t *testing.T) {
	t.Helper()
	prev := releaseSendBudget
	releaseSendBudget = 20 * time.Millisecond
	t.Cleanup(func() { releaseSendBudget = prev })
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
func releasingManager(t *testing.T, p *Plugin, value string, v4, v6 *fakeReleaser) *dhcpManager {
	t.Helper()
	opts := DHCPNetworkOptions{AuditLog: true, ReleaseLease: value, IPv6: v6 != nil}
	m := stoppingManager(t, p, opts, nil, nil)
	if v4 != nil {
		m.setReleaseClient(false, v4)
	}
	if v6 != nil {
		m.setReleaseClient(true, v6)
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
			shortReleaseBudget(t)
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			client := &fakeReleaser{sends: 1}
			m := releasingManager(t, p, tc.value, client, nil)

			var err error
			if tc.leaving {
				err = m.StopForLeave()
			} else {
				err = m.Stop()
			}
			if err != nil {
				t.Fatalf("stop(leaving=%v): %v", tc.leaving, err)
			}

			if got := client.callCount(); got != tc.want {
				t.Fatalf("the client's Release() was called %d time(s), want %d", got, tc.want)
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

// TestReleaseLease_TheCounterFollowsTheSendNotTheCall is the counter's
// own observer, driven over the three states the library distinguishes.
//
// A plugin counter folded from "we called Release()" would read the
// same in all three, and an operator alerting on release_failures would
// see a clean zero on a host that handed nothing back.
func TestReleaseLease_TheCounterFollowsTheSendNotTheCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		// client is nil for the family that never started one.
		client       *fakeReleaser
		wantSent     int32
		wantFailures int32
		wantReleased bool
	}{
		{
			name:         "the packet went out",
			client:       &fakeReleaser{sends: 1},
			wantSent:     1,
			wantReleased: true,
		},
		{
			name: "the client held no binding, or the send failed",
			// The library counts ReleasesSent on a successful send and
			// nowhere else; a machine in INIT or SELECTING sends
			// nothing at all for a release.
			client:       &fakeReleaser{sends: 0},
			wantFailures: 1,
		},
		{
			name:         "there is no client for this family",
			client:       nil,
			wantFailures: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortReleaseBudget(t)
			p := &Plugin{}
			m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
				DHCPNetworkOptions{ReleaseLease: ReleaseOnStop}).withPlugin(p)
			if tc.client != nil {
				m.setReleaseClient(false, tc.client)
			}

			releasedV4, releasedV6 := m.releaseHeldLeases()
			if releasedV4 != tc.wantReleased {
				t.Errorf("releasedV4 = %v, want %v", releasedV4, tc.wantReleased)
			}
			if releasedV6 {
				t.Errorf("releasedV6 = true on a v4-only network")
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
			shortReleaseBudget(t)
			prev := nlAddrDel
			nlAddrDel = func(*netlink.Handle, netlink.Link, *netlink.Addr) error { return tc.delErr }
			t.Cleanup(func() { nlAddrDel = prev })

			p := &Plugin{}
			m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
				DHCPNetworkOptions{ReleaseLease: ReleaseOnStop, IPv6: true}).withPlugin(p)
			m.netHandle = &netlink.Handle{}
			m.ctrLink = &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}
			v6, err := netlink.ParseAddr("fd00::50/64")
			if err != nil {
				t.Fatalf("ParseAddr: %v", err)
			}
			m.setLastIP(true, v6)

			client := &fakeReleaser{sends: 1}
			m.setReleaseClient(true, client)

			if got := m.releaseHeldLease(true); got != tc.wantOutcome {
				t.Errorf("releaseHeldLease(v6) = %q, want %q", got, tc.wantOutcome)
			}
			if got := client.callCount(); got != tc.wantCalls {
				t.Fatalf("Release() was called %d time(s), want %d: RFC 9915 section 18.2.7 "+
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
				MAC: mac.String(), IPv4: "192.168.0.50",
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
		v4Sends      int
		v6Sends      int
		wantReleased bool
		wantV4Phase  lease.Phase
		wantV6Phase  lease.Phase
	}{
		{
			name: "both families released", value: ReleaseOnStop, v4Sends: 1, v6Sends: 1,
			wantReleased: true, wantV4Phase: lease.PhaseClosed, wantV6Phase: lease.PhaseClosed,
		},
		{
			// The mixed outcome. One shared flag would settle both
			// records the same way, and the v6 lease this endpoint
			// still holds would stop being resumable.
			name: "only v4 got out", value: ReleaseOnStop, v4Sends: 1, v6Sends: 0,
			wantReleased: true, wantV4Phase: lease.PhaseClosed, wantV6Phase: lease.PhaseLeft,
		},
		{
			// The other half of the mixed outcome, and it is the one
			// an "any" that forgot the v6 half still passes: the
			// tombstone carries both addresses, so a v6 lease that
			// went back must suppress it even though the v4 lease
			// did not move.
			name: "only v6 got out", value: ReleaseOnStop, v4Sends: 0, v6Sends: 1,
			wantReleased: true, wantV4Phase: lease.PhaseLeft, wantV6Phase: lease.PhaseClosed,
		},
		{
			name: "nothing was asked for", value: ReleaseNever, v4Sends: 1, v6Sends: 1,
			wantReleased: false, wantV4Phase: lease.PhaseLeft, wantV6Phase: lease.PhaseLeft,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortReleaseBudget(t)
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

			m := releasingManager(t, p, tc.value,
				&fakeReleaser{sends: tc.v4Sends}, &fakeReleaser{sends: tc.v6Sends})
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

// TestReleaseLease_TheClientIsPublishedOnceAndOnlyFromSetupClient is the
// CALL SITE, and it is a source-level test for the reason
// TestRenewalWiring_IsCalledOnceAndOnlyFromSetupClient gives: there is
// no seam between setupClient and a raw socket in a real network
// namespace, so the tail of setupClient is not reachable from a unit
// test at all, and what is checkable here is the wiring.
//
// Deleting the call leaves every other test in this file green. The
// fakes below publish their own client, so the option still parses, the
// budget still expires, the counters still move and the records still
// settle -- and on a real host releaseClient would return nil for every
// endpoint, releaseHeldLease would return false without sending
// anything, and `release_lease=on_stop` would be a network option that
// counts a failure per stop and never hands an address back.
//
// The family argument is checked for the same reason the renewal test
// checks its own: a literal there publishes both families under one
// key, and the family that is overwritten is a lease nothing releases.
func TestReleaseLease_TheClientIsPublishedOnceAndOnlyFromSetupClient(t *testing.T) {
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
			// The definition is a FuncDecl and not a call, so the
			// method itself is not one of the sites this counts.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "setReleaseClient" {
					return true
				}
				sites = append(sites, fn.Name.Name)
				if len(call.Args) != 2 {
					t.Errorf("setReleaseClient in %s takes %d argument(s); this test reads the "+
						"first one as the family and can no longer do so", fn.Name.Name, len(call.Args))
					return true
				}
				id, ok := call.Args[0].(*ast.Ident)
				if !ok || id.Name != "v6" {
					t.Errorf("setReleaseClient in %s is passed %T as its family argument, not the "+
						"`v6` parameter; a constant there files both families under one key and "+
						"leaves the other family with no client to release", fn.Name.Name, call.Args[0])
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("no production sources parsed; this test would pass vacuously")
	}
	if len(sites) != 1 {
		t.Fatalf("setReleaseClient is called %d time(s), in %v; want exactly one, in setupClient. "+
			"None means release_lease=on_stop finds no client at Leave, sends nothing, and counts a "+
			"failure for every stop; more than one means a CreateEndpoint one-shot, which returns "+
			"before any Leave, is a second writer to the field the teardown path reads.", len(sites), sites)
	}
	if sites[0] != "setupClient" {
		t.Errorf("setReleaseClient is called from %s; the client that holds this endpoint's lease "+
			"at Leave is the persistent one, and it is built in setupClient", sites[0])
	}
}

// TestReleaseLease_AFailedStartStillAsksAndStillCounts drives the
// population the call-site test above cannot see: an endpoint whose
// Join failed.
//
// setupClient publishes the client BEFORE Start, on purpose, so a
// client whose Start failed is still there to be asked. It holds no
// binding, so nothing goes on the wire and the attempt lands in
// release_failures. The alternative, which this pins against, is the
// release sitting below stop's `startErr` return: then a network that
// asked for releases would be SILENT for exactly this population --
// neither sent nor failed -- while its one-shot's address stays leased
// upstream. Silence there reads like a network with no failures.
func TestReleaseLease_AFailedStartStillAsksAndStillCounts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		wantCalls  int
		wantSent   int32
		wantFailed int32
	}{
		{"on_stop: the attempt is made and counted as a failure", ReleaseOnStop, 1, 0, 1},
		{"never: nothing is asked and nothing is counted", ReleaseNever, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortReleaseBudget(t)
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			// sends: 0 is the state of a client whose Start failed. The
			// machine never bound, so the library has no binding to
			// relinquish and moves no counter.
			client := &fakeReleaser{sends: 0}
			m := releasingManager(t, p, tc.value, client, nil)
			m.startErr = errors.New("failed to start DHCP client")

			if err := m.StopForLeave(); err != nil {
				t.Fatalf("StopForLeave: %v", err)
			}

			if got := client.callCount(); got != tc.wantCalls {
				t.Errorf("the client's Release() was called %d time(s), want %d", got, tc.wantCalls)
			}
			if got := m.releasedV4.Load(); got {
				t.Error("releasedV4 is true after a release that put nothing on the wire; " +
					"the tombstone would be skipped for an address still leased upstream")
			}
			if got := p.releasesSentV4.Load(); got != tc.wantSent {
				t.Errorf("releases_sent_v4 = %d, want %d", got, tc.wantSent)
			}
			if got := p.releaseFailuresV4.Load(); got != tc.wantFailed {
				t.Errorf("release_failures_v4 = %d, want %d; an operator who asked for releases "+
					"reads this counter to find the endpoints that did not get one", got, tc.wantFailed)
			}
		})
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

// TestReleaseLease_EveryReasonForNotSendingIsNamed drives the four
// outcomes apart.
//
// WHY THE REASON IS A TESTED VALUE AND NOT JUST LOG TEXT. A stop on a
// `release_lease=on_stop` network that hands nothing back has three
// unrelated causes, and until this change all three returned a bare
// false: no client to ask, a client that was asked and never sent, and
// a v6 address that could not come off the link. They are three
// different operator problems -- a container that stopped before its
// persistent client attached, a wedged send, and a netlink permission
// failure -- and the counter pair deliberately does not tell them
// apart, so the outcome is the only place the difference survives.
//
// The no-client arm is the one measured against the product: it is the
// `docker run --rm` shape the option exists for, where the address in
// use came from the acquisition at endpoint creation and no persistent
// client ever attached to be asked.
func TestReleaseLease_EveryReasonForNotSendingIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hasClient false means nothing was ever published for this
		// family; sends is how many of the fake's calls reach the wire.
		hasClient bool
		sends     int
		want      releaseOutcome
	}{
		{"no client was ever published", false, 0, releaseNoClient},
		{"the client was asked and sent nothing", true, 0, releaseBudgetExpired},
		{"the client sent", true, 1, releaseSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortReleaseBudget(t)
			client := func() *fakeReleaser {
				if !tc.hasClient {
					return nil
				}
				return &fakeReleaser{sends: tc.sends}
			}

			// The reason.
			c := client()
			m := releasingManager(t, &Plugin{}, ReleaseOnStop, c, nil)
			if got := m.releaseHeldLease(false); got != tc.want {
				t.Errorf("releaseHeldLease(v4) = %q, want %q", got, tc.want)
			}
			// The client is asked in every arm that has one; a mutant
			// answering no_client without looking would pass above.
			if c != nil && c.callCount() != 1 {
				t.Errorf("Release() was called %d time(s), want 1", c.callCount())
			}

			// The counters, on a FRESH manager, because the fake's send
			// budget is spent by the probe above. They stay a two-way
			// split: every outcome but the send is one failure.
			p := &Plugin{}
			m2 := releasingManager(t, p, ReleaseOnStop, client(), nil)

			prevLevel := log.GetLevel()
			log.SetLevel(log.DebugLevel)
			t.Cleanup(func() { log.SetLevel(prevLevel) })
			hook := logtest.NewLocal(log.StandardLogger())
			defer hook.Reset()

			if got := m2.releaseFamily(false); got != (tc.want == releaseSent) {
				t.Errorf("releaseFamily(v4) = %v, want %v", got, tc.want == releaseSent)
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
				if tc.want == releaseSent {
					wantLevel = log.DebugLevel
				}
				if e.Level != wantLevel {
					t.Errorf("the %q line is at %s, want %s: a stop that hands nothing back "+
						"on a releasing network is a warning", v, e.Level, wantLevel)
				}
			}
			if len(said) != 1 || said[0] != string(tc.want) {
				t.Errorf("the log named outcomes %v, want exactly [%s]", said, tc.want)
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
		})
	}
}

// TestReleaseLease_TheShippedSendBudgetIsTheOneInForce closes the gap
// every other test in this file opens.
//
// shortReleaseBudget replaces releaseSendBudget in all of them, so the
// value that actually ships has never executed: a change to it, to
// `time.Second`'s units, or to the line that reads it would be caught
// by nothing here. This case runs the send path with the shipped budget
// untouched, which costs nothing because a client that sends returns on
// the first poll.
//
// WHAT THIS DOES NOT COVER, stated rather than implied: the EXPIRY arm
// under the shipped value is still driven only under the short budget.
// Waiting a real second for it would put a second on every run of this
// package to observe a `time.Now` comparison that the short-budget
// cases already drive. The constant below is pinned instead, so a
// change to the shipped number is a change to this test.
func TestReleaseLease_TheShippedSendBudgetIsTheOneInForce(t *testing.T) {
	if releaseSendBudget != time.Second {
		t.Fatalf("releaseSendBudget = %v, want 1s: the number is documented in docs/reference.md "+
			"as the per-family cost of a stop that cannot release", releaseSendBudget)
	}

	p := &Plugin{}
	client := &fakeReleaser{sends: 1}
	m := releasingManager(t, p, ReleaseOnStop, client, nil)

	start := time.Now()
	if got := m.releaseHeldLease(false); got != releaseSent {
		t.Fatalf("releaseHeldLease(v4) = %q, want %q", got, releaseSent)
	}
	if elapsed := time.Since(start); elapsed >= releaseSendBudget {
		t.Errorf("the send path took %v with a client that sends immediately; "+
			"it must not wait out the budget", elapsed)
	}
}
