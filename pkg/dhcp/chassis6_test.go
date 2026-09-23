// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/vishvananda/netns"
)

// DHCPv6 has no router option (RFC 9915 section 21) and RFC 5942 section 4 rule 1 makes no prefix on-link (D30 Q3).

func TestCheckRouterAdvertGuardShape(t *testing.T) {
	ns := netns.NsHandle(-1)
	cases := []struct {
		name    string
		v6      bool
		honor   bool
		netNS   bool
		oneShot bool
		wantErr bool
	}{
		{name: "the persistent v6 client", v6: true, honor: true, netNS: true},
		{name: "a persistent v6 client without the guard", v6: true, netNS: true, wantErr: true},
		{name: "a persistent v6 client with no namespace", v6: true, honor: true, wantErr: true},
		{name: "the v6 one-shot", v6: true, oneShot: true},
		{name: "a v6 one-shot claiming the guard", v6: true, honor: true, netNS: true, oneShot: true, wantErr: true},
		{name: "the persistent v4 client", netNS: true},
		{name: "a v4 client claiming the guard", honor: true, netNS: true, wantErr: true},
		{name: "the v4 one-shot", oneShot: true},
		{name: "a v4 one-shot claiming the guard", honor: true, oneShot: true, wantErr: true},
	}

	// Coverage of the three read booleans, not the row count (#911).
	covered := map[[3]bool]bool{}
	for _, tc := range cases {
		covered[[3]bool{tc.v6, tc.honor, tc.oneShot}] = true
	}
	for _, v6 := range []bool{false, true} {
		for _, honor := range []bool{false, true} {
			for _, oneShot := range []bool{false, true} {
				if !covered[[3]bool{v6, honor, oneShot}] {
					t.Fatalf("no row for v6=%v honor=%v oneShot=%v", v6, honor, oneShot)
				}
			}
		}
	}
	var accepts, refuses int
	for _, tc := range cases {
		if tc.wantErr {
			refuses++
		} else {
			accepts++
		}
	}
	if accepts == 0 || refuses == 0 {
		t.Fatalf("the table has %d accepting and %d refusing rows; it needs both", accepts, refuses)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &DHCPClientOptions{V6: tc.v6, HonorRouterAdverts: tc.honor}
			if tc.netNS {
				opts.NetNS = &ns
			}
			err := checkRouterAdvertGuardShape(opts, tc.oneShot)
			if tc.wantErr && err == nil {
				t.Error("accepted, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("refused with %v, want acceptance", err)
			}
		})
	}
}

// RFC 4861 sections 6.3.7 and 10: MAX_RTR_SOLICITATION_DELAY, then MAX_RTR_SOLICITATIONS at RTR_SOLICITATION_INTERVAL.

func TestRouterDiscoveryWindow(t *testing.T) {
	def := RouterDiscoveryWindow(proto.DefaultParams6())
	if want := time.Second + 3*4*time.Second; def != want {
		t.Errorf("the default window is %v, want %v (1s delay + 3 solicitations 4s apart)", def, want)
	}

	// Zero is the library's default, not zero seconds (#911).
	if got := RouterDiscoveryWindow(proto.Params6{}); got != def {
		t.Errorf("the window for zero parameters is %v, want the default %v", got, def)
	}

	slow := proto.DefaultParams6()
	slow.RouterSolicitations = 6
	if got := RouterDiscoveryWindow(slow); got <= def {
		t.Errorf("doubling the solicitations gave %v, not more than the default %v", got, def)
	}
	patient := proto.DefaultParams6()
	patient.RouterSolicitInterval = proto.RtrSolicitationInterval * 2
	if got := RouterDiscoveryWindow(patient); got <= def {
		t.Errorf("doubling the interval gave %v, not more than the default %v", got, def)
	}
}

// A stateless Configured ends the acquisition with a sentinel pkg/plugin classifies (D30 Q7, #868).

func TestAcquireStep6(t *testing.T) {
	now := time.Now()
	acquired := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{
		Addr:   netip.MustParsePrefix("2001:db8::5/128"),
		Expire: now.Add(time.Hour),
	}}

	got := acquireStep6(acquired, false, netip.Prefix{})
	if !got.Done {
		t.Error("Acquired did not end the acquisition")
	}
	if got.Err != nil {
		t.Errorf("Acquired carried an error: %v", got.Err)
	}
	if got.Info.IP == "" {
		t.Error("Acquired produced no address")
	}

	got = acquireStep6(lease.Event{Kind: lease.Configured}, false, netip.Prefix{})
	if !got.Done {
		t.Error("Configured did not end the acquisition; the endpoint would wait out " +
			"the whole lease_timeout for an address the segment has already declined to offer")
	}
	if !errors.Is(got.Err, ErrNoV6Address) {
		t.Errorf("Configured carried %v, want ErrNoV6Address: pkg/plugin distinguishes "+
			"\"no addresses here\" from \"out of time\" on this sentinel", got.Err)
	}
	if got.Info.IP != "" {
		t.Errorf("Configured produced an address %q", got.Info.IP)
	}

	got = acquireStep6(lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer}, false, netip.Prefix{})
	if got.Done {
		t.Error("Failed ended the acquisition; the ladder's next attempt is the caller's " +
			"decision and the deadline is what ends it")
	}
	if got.Err == nil {
		t.Error("Failed carried no error, so the caller reports ErrNoLease with no reason")
	}

	if got := acquireStep6(lease.Event{Kind: lease.Renewed}, false, netip.Prefix{}); got.Done || got.Err != nil {
		t.Errorf("Renewed ended the acquisition or carried an error: %+v", got)
	}
}

func TestInfoFromConfig(t *testing.T) {
	cfg := lease.Configuration{
		DNS:    []netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2")},
		Search: []string{"example.test", "corp.example.test"},
	}
	info, dropped := infoFromConfig(cfg)
	if dropped != 0 {
		t.Errorf("dropped %d clean values", dropped)
	}
	if len(info.DNSServers) != 2 || info.DNSServers[0] != "2001:db8::1" {
		t.Errorf("DNSServers = %v, want both servers in order", info.DNSServers)
	}
	if len(info.SearchList) != 2 || info.SearchList[1] != "corp.example.test" {
		t.Errorf("SearchList = %v, want both domains in order", info.SearchList)
	}
	if info.IP != "" || info.Gateway != "" {
		t.Errorf("a Configured event produced IP %q gateway %q", info.IP, info.Gateway)
	}

	// An Information-request Reply carries no router information (RFC 9915 section 18.2.6), so MTU 0 is not a
	// withdrawal (#821).
	if info.RouterSeen || info.MTU != 0 {
		t.Errorf("a Configured event claimed to carry router information "+
			"(RouterSeen=%v MTU=%d)", info.RouterSeen, info.MTU)
	}

	poisoned := lease.Configuration{Search: []string{"ok.test", "bad\nnameserver 10.0.0.1"}}
	info, dropped = infoFromConfig(poisoned)
	if dropped == 0 {
		t.Error("a search domain carrying a newline was not dropped: the plugin writes " +
			"this list into the container's resolv.conf")
	}
	for _, s := range info.SearchList {
		if s != "ok.test" {
			t.Errorf("kept %q", s)
		}
	}

	// The search list is copied, so the sanitiser never rewrites the library's value (#911).
	cfg = lease.Configuration{Search: []string{"example.test"}}
	info, _ = infoFromConfig(cfg)
	cfg.Search[0] = "rewritten"
	if info.SearchList[0] != "example.test" {
		t.Errorf("infoFromConfig aliases the library's search list: it now reads %q",
			info.SearchList[0])
	}
}

// RFC 9915 section 7.1 and RFC 4862 section 5.5.4: preferred and valid lifetimes let the kernel deprecate first.

func TestInfoFromLease_PreferredSecondsIsV6Only(t *testing.T) {
	now := time.Now()

	v6 := lease.Lease{
		Addr:      netip.MustParsePrefix("2001:db8::5/128"),
		Preferred: now.Add(30 * time.Minute),
		Expire:    now.Add(time.Hour),
	}
	info, _ := infoFromLease(v6, proto.RouterObservation{}, now, netip.Prefix{})
	if info.LeaseSeconds != 3600 {
		t.Errorf("LeaseSeconds = %d, want 3600", info.LeaseSeconds)
	}
	if info.PreferredSeconds != 1800 {
		t.Errorf("PreferredSeconds = %d, want 1800", info.PreferredSeconds)
	}

	// The zero Time is an infinite preferred lifetime on the library's convention (#911).
	v6.Preferred = time.Time{}
	info, _ = infoFromLease(v6, proto.RouterObservation{}, now, netip.Prefix{})
	if info.PreferredSeconds != info.LeaseSeconds {
		t.Errorf("an infinite preferred lifetime gave PreferredSeconds = %d with "+
			"LeaseSeconds = %d; a zero here deprecates the address on arrival",
			info.PreferredSeconds, info.LeaseSeconds)
	}

	v4 := lease.Lease{
		Addr:      netip.MustParsePrefix("192.168.0.5/24"),
		Preferred: now.Add(30 * time.Minute),
		Expire:    now.Add(time.Hour),
	}
	if info, _ := infoFromLease(v4, proto.RouterObservation{}, now, netip.Prefix{}); info.PreferredSeconds != 0 {
		t.Errorf("a v4 lease produced PreferredSeconds = %d; DHCPv4 has one lifetime "+
			"and the field is omitempty", info.PreferredSeconds)
	}
}

func TestRouterFlags(t *testing.T) {
	cases := []struct {
		r    proto.RouterObservation
		want string
	}{
		{proto.RouterObservation{}, ""},
		{proto.RouterObservation{Managed: true, Other: true}, ""},
		{proto.RouterObservation{Seen: true}, ""},
		{proto.RouterObservation{Seen: true, Managed: true}, "M"},
		{proto.RouterObservation{Seen: true, Other: true}, "O"},
		{proto.RouterObservation{Seen: true, Managed: true, Other: true}, "MO"},
	}
	for _, tc := range cases {
		if got := routerFlags(tc.r); got != tc.want {
			t.Errorf("routerFlags(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func TestRAObservation_MergeIsMonotonic(t *testing.T) {
	seen := RAObservation{Seen: true, Managed: true, Other: true}
	if got := seen.Merge(RAObservation{}); got != seen {
		t.Errorf("merging a quiet attempt into a seen one gave %+v, want %+v", got, seen)
	}
	if got := (RAObservation{}).Merge(seen); got != seen {
		t.Errorf("merging a seen attempt into a quiet one gave %+v, want %+v", got, seen)
	}
	a := RAObservation{Seen: true, Managed: true}
	b := RAObservation{Seen: true, Other: true}
	want := RAObservation{Seen: true, Managed: true, Other: true}
	if got := a.Merge(b); got != want {
		t.Errorf("%+v merged with %+v gave %+v, want %+v", a, b, got, want)
	}
	if (RAObservation{}).Merge(RAObservation{}) != (RAObservation{}) {
		t.Error("two quiet attempts merged into a seen one")
	}
}

func TestRAObservation_ConvertsEveryField(t *testing.T) {
	got := raObservation(proto.RouterObservation{Seen: true, Managed: true, Other: true})
	if !got.Seen || !got.Managed || !got.Other {
		t.Errorf("raObservation dropped a field: %+v", got)
	}
	if raObservation(proto.RouterObservation{}) != (RAObservation{}) {
		t.Error("raObservation invented a flag from a zero observation")
	}
	if got := raObservation(proto.RouterObservation{Managed: true}); got != (RAObservation{Managed: true}) {
		t.Errorf("raObservation(Managed) = %+v", got)
	}
	if got := raObservation(proto.RouterObservation{Other: true}); got != (RAObservation{Other: true}) {
		t.Errorf("raObservation(Other) = %+v", got)
	}
	if got := raObservation(proto.RouterObservation{Seen: true}); got != (RAObservation{Seen: true}) {
		t.Errorf("raObservation(Seen) = %+v", got)
	}
}

func TestV6AcquisitionWindow_FitsInsideTheDaemonsDeadline(t *testing.T) {
	p := proto.DefaultParams6()
	got := V6AcquisitionWindow(p)

	if got <= RouterDiscoveryWindow(p) {
		t.Errorf("V6AcquisitionWindow(%s) does not outlast router discovery (%s); "+
			"an absence verdict drawn inside it is about the deadline",
			got, RouterDiscoveryWindow(p))
	}
	// moby's plugin client gives a request 30 s (#868).
	const daemonDeadline = 30 * time.Second
	if got >= daemonDeadline {
		t.Errorf("V6AcquisitionWindow(%s) reaches the daemon's %s plugin deadline; "+
			"CreateEndpoint would be abandoned before it could report the segment",
			got, daemonDeadline)
	}
	if got >= ConflictRecoveryWindow(proto.DefaultParams(nil)) {
		t.Errorf("V6AcquisitionWindow(%s) is not shorter than the v4-derived default "+
			"lease_timeout (%s), so the v6 one-shot still runs on DHCPv4's budget",
			got, ConflictRecoveryWindow(proto.DefaultParams(nil)))
	}
}

// RFC 9915 section 15 doubles each timer and section 18.2.1 delays the first: 1 + 1.1 + 2.2 + 4.4 with the defaults.

func TestV6SolicitWindow_CoversTheRetransmissionsItClaims(t *testing.T) {
	p := proto.DefaultParams6()
	if v6SolicitTransmissions != 4 {
		t.Fatalf("this expectation is written for 4 transmissions, not %d; "+
			"re-derive it rather than adjusting the total", v6SolicitTransmissions)
	}
	want := time.Duration(p.SolMaxDelay) +
		1100*time.Millisecond + 2200*time.Millisecond + 4400*time.Millisecond
	if got := v6SolicitWindow(p); got != want {
		t.Errorf("v6SolicitWindow = %s, want %s", got, want)
	}

	if got := v6SolicitWindow(proto.Params6{}); got != want {
		t.Errorf("v6SolicitWindow(zero Params6) = %s, want the default's %s", got, want)
	}
}

func TestAdvertisedNoDHCPv6(t *testing.T) {
	for _, tc := range []struct {
		ra   RAObservation
		want bool
	}{
		{RAObservation{}, false},
		{RAObservation{Managed: true}, false},
		{RAObservation{Other: true}, false},
		{RAObservation{Managed: true, Other: true}, false},
		{RAObservation{Seen: true}, true},
		{RAObservation{Seen: true, Managed: true}, false},
		{RAObservation{Seen: true, Other: true}, false},
		{RAObservation{Seen: true, Managed: true, Other: true}, false},
	} {
		if got := advertisedNoDHCPv6(tc.ra); got != tc.want {
			t.Errorf("advertisedNoDHCPv6(%+v) = %v, want %v", tc.ra, got, tc.want)
		}
	}
}

func TestErrNoDHCPv6OnSegment_IsAnAdvertisedAbsence(t *testing.T) {
	ra := RAObservation{Seen: true}
	if !advertisedNoDHCPv6(ra) {
		t.Fatalf("the observation that produces %v is not an advertised absence", ErrNoDHCPv6OnSegment)
	}
	if errors.Is(ErrNoDHCPv6OnSegment, ErrNoV6Address) {
		t.Error("ErrNoDHCPv6OnSegment must not read as ErrNoV6Address: " +
			"the stateless Reply is an answer from a server, this is an answer from a router")
	}
}

// RFC 9915 section 18.2.13's Reply to a Confirm carries a status and no options.

func TestCarryResumedConfig6(t *testing.T) {
	dns := func(s ...string) []netip.Addr {
		out := make([]netip.Addr, 0, len(s))
		for _, one := range s {
			out = append(out, netip.MustParseAddr(one))
		}
		return out
	}
	resume := func() *lease.Lease {
		return &lease.Lease{DNS: dns("2001:db8::53"), DomainSearch: []string{"corp.example"}}
	}

	t.Run("a confirmed lease with nothing on it is filled", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		ev := lease.Event{Kind: lease.Acquired}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 1 || ev.Lease.DNS[0].String() != "2001:db8::53" {
			t.Errorf("DNS = %v, want the remembered server", ev.Lease.DNS)
		}
		if len(ev.Lease.DomainSearch) != 1 || ev.Lease.DomainSearch[0] != "corp.example" {
			t.Errorf("DomainSearch = %v, want the remembered list", ev.Lease.DomainSearch)
		}
	})

	t.Run("a lease the server described is left alone", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		ev := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{DNS: dns("2001:db8::9")}}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 1 || ev.Lease.DNS[0].String() != "2001:db8::9" {
			t.Errorf("DNS = %v, want the server's own answer untouched", ev.Lease.DNS)
		}
		// RFC 3646: a Reply with option 23 and no option 24 has said there is no search list (#911).
		if len(ev.Lease.DomainSearch) != 0 {
			t.Errorf("DomainSearch = %v, want none: the server sent DNS and no search list", ev.Lease.DomainSearch)
		}
	})

	t.Run("the memory is spent on the first lease-bearing event", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		first := lease.Event{Kind: lease.Acquired, Lease: lease.Lease{DNS: dns("2001:db8::9")}}
		o.carryResumedConfig6(&first)
		later := lease.Event{Kind: lease.Renewed}
		o.carryResumedConfig6(&later)
		if len(later.Lease.DNS) != 0 {
			t.Errorf("DNS = %v on a later renewal, want none: the server has spoken since, "+
				"and a memory that keeps applying is a second source of truth", later.Lease.DNS)
		}
	})

	t.Run("an event carrying no lease does not spend it", func(t *testing.T) {
		o := &DHCPClientOptions{V6: true, Resume: resume()}
		lost := lease.Event{Kind: lease.Lost}
		o.carryResumedConfig6(&lost)
		if len(lost.Lease.DNS) != 0 {
			t.Errorf("a Lost was filled in: %v", lost.Lease.DNS)
		}
		ev := lease.Event{Kind: lease.Acquired}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 1 {
			t.Errorf("DNS = %v after a Lost, want the memory still available", ev.Lease.DNS)
		}
	})

	t.Run("a v4 client never carries one", func(t *testing.T) {
		o := &DHCPClientOptions{Resume: resume()}
		ev := lease.Event{Kind: lease.Acquired}
		o.carryResumedConfig6(&ev)
		if len(ev.Lease.DNS) != 0 {
			t.Errorf("DNS = %v on a v4 client: option 6 arrives in every DHCPACK, "+
				"including an INIT-REBOOT's, so there is nothing to carry", ev.Lease.DNS)
		}
	})
}

// fakeV6Client closes its event channel when Run returns, as *dhcpruntime.Client6 does.
type fakeV6Client struct {
	events chan lease.Event
	router proto.RouterObservation
}

func (f *fakeV6Client) Run(ctx context.Context) error {
	<-ctx.Done()
	close(f.events)
	return ctx.Err()
}

func (f *fakeV6Client) Events() <-chan lease.Event { return f.events }

func (f *fakeV6Client) Router() proto.RouterObservation { return f.router }

// acquisition6Result bounds the wait so a late verdict fails instead of hanging (#911).
func acquisition6Result(t *testing.T, ctx context.Context, client v6AcquisitionClient, opts *DHCPClientOptions, hint netip.Addr, window, patience time.Duration) (Info, time.Duration, error) {
	t.Helper()

	type result struct {
		info Info
		err  error
	}
	out := make(chan result, 1)
	start := time.Now()
	go func() {
		info, err := runAcquisition6(ctx, "test0", client, opts, hint, window)
		out <- result{info, err}
	}()

	select {
	case r := <-out:
		return r.info, time.Since(start), r.err
	case <-time.After(patience):
		t.Fatalf("runAcquisition6 did not return within %v; window was %v", patience, window)
		return Info{}, 0, nil
	}
}

func TestRunAcquisition6_EndsOnAnAdvertisementThatOffersNoDHCPv6(t *testing.T) {
	t.Run("M=0 O=0 ends it without waiting the window out", func(t *testing.T) {
		client := &fakeV6Client{
			events: make(chan lease.Event),
			router: proto.RouterObservation{Seen: true},
		}
		_, took, err := acquisition6Result(t, context.Background(), client,
			&DHCPClientOptions{V6: true}, netip.Addr{}, 3*time.Second, 10*time.Second)

		if !errors.Is(err, ErrNoDHCPv6OnSegment) {
			t.Fatalf("runAcquisition6 returned %v after %v, want ErrNoDHCPv6OnSegment; "+
				"the segment advertised that it has no DHCPv6 and the acquisition "+
				"waited for one anyway", err, took)
		}
		if took > 2*time.Second {
			t.Errorf("the verdict took %v on an advertisement that arrived at once; "+
				"an operator's container start is charged for the whole window", took)
		}
	})

	t.Run("M=1 leaves the acquisition running", func(t *testing.T) {
		client := &fakeV6Client{
			events: make(chan lease.Event, 1),
			router: proto.RouterObservation{Seen: true, Managed: true},
		}
		go func() {
			time.Sleep(400 * time.Millisecond)
			client.events <- lease.Event{
				Kind:  lease.Acquired,
				Lease: lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")},
			}
		}()
		info, _, err := acquisition6Result(t, context.Background(), client,
			&DHCPClientOptions{V6: true}, netip.Addr{}, 5*time.Second, 10*time.Second)

		if err != nil {
			t.Fatalf("runAcquisition6: %v; a managed segment answered and the acquisition "+
				"had already concluded there was nothing to wait for", err)
		}
		if info.IP != "fd00:6470:6865::61/128" {
			t.Errorf("got address %q, want the leased one", info.IP)
		}
	})
}

func TestRunAcquisition6_HasItsOwnWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := &fakeV6Client{events: make(chan lease.Event)}
	_, took, err := acquisition6Result(t, ctx, client,
		&DHCPClientOptions{V6: true}, netip.Addr{}, 300*time.Millisecond, 10*time.Second)

	if err == nil {
		t.Fatal("runAcquisition6 produced an address from a client that never said anything")
	}
	if took > 5*time.Second {
		t.Errorf("the acquisition ran for %v under a 300ms window; it is on the caller's "+
			"clock, and the caller's clock outlives the deadline the daemon keeps on "+
			"the plugin call", took)
	}
}

// Measured on the lane 2026-09-06, run 34058213252: a held hinted address looped Solicit to Decline about once a second
// for sixteen seconds until the daemon gave up (#911).

func TestAcquireStep6_AConflictOnAHintedAddressEndsTheAttempt(t *testing.T) {
	conflict := lease.Event{Kind: lease.Failed, Reason: proto.ReasonConflict, Note: "in use"}

	got := acquireStep6(conflict, true, netip.Prefix{})
	if !got.Done {
		t.Error("a conflict on the address this attempt ASKED for did not end it; the " +
			"library restarts discovery with the same hint, the server hands back the " +
			"same address, and the loop runs until the daemon's deadline")
	}
	if !errors.Is(got.Err, errV6HintInUse) {
		t.Errorf("the conflict carried %v, want errV6HintInUse: getIP6 decides on this "+
			"sentinel whether a second attempt is worth running", got.Err)
	}

	if got := acquireStep6(conflict, false, netip.Prefix{}); got.Done {
		t.Error("a conflict on a SERVER-CHOSEN address ended the attempt; there is no " +
			"loop to break there -- the library asks again and is given a different " +
			"address -- and ending it costs the endpoint a whole second acquisition")
	}
	if got := acquireStep6(lease.Event{Kind: lease.Failed, Reason: proto.ReasonNoServer}, true, netip.Prefix{}); got.Done {
		t.Error("a Failed that is not a conflict ended the attempt on a hinted " +
			"acquisition; the ladder's next attempt is the caller's decision")
	}
}

func TestRetryWithoutHint6(t *testing.T) {
	hint := netip.MustParseAddr("fd00:6470:6863::90")
	inUse := fmt.Errorf("dhcp: %w: in use", errV6HintInUse)

	opts := &DHCPClientOptions{V6: true, Resume: &lease.Lease{}}
	opts.params6 = proto.Params6{Hint: hint}

	declined, again := opts.retryWithoutHint6(inUse)
	if !again {
		t.Fatal("a hinted attempt refused for a duplicate was not retried; the endpoint " +
			"fails with a deadline it could have avoided by asking for any other address")
	}
	if declined != hint {
		t.Errorf("the retry named %v as the declined address, want %v", declined, hint)
	}
	if opts.params6.Hint.IsValid() {
		t.Errorf("the second attempt still carries the hint %v, which is the address "+
			"another node holds: the retry would fetch it again", opts.params6.Hint)
	}
	if opts.Resume != nil {
		t.Error("the second attempt still carries the resumed binding, which names the " +
			"declined address: its Confirm asks the server to bless exactly what the " +
			"node just refused (RFC 9915 section 18.2.12)")
	}

	if _, again := opts.retryWithoutHint6(inUse); again {
		t.Error("a THIRD pass was offered. The retry is bounded by the hint it clears; " +
			"if it is not, a segment with a squatter turns CreateEndpoint into a loop")
	}

	// Any other failure keeps the hint and resumption, #213's preferred address.
	keep := &DHCPClientOptions{V6: true, Resume: &lease.Lease{}}
	keep.params6 = proto.Params6{Hint: hint}
	if _, again := keep.retryWithoutHint6(ErrNoLease); again {
		t.Error("an attempt that failed for a reason other than a duplicate was retried")
	}
	if keep.params6.Hint != hint || keep.Resume == nil {
		t.Errorf("the hint or the resumption was dropped by a refusal that was not a "+
			"duplicate (hint %v, resume %v)", keep.params6.Hint, keep.Resume)
	}

	none := &DHCPClientOptions{V6: true}
	if _, again := none.retryWithoutHint6(inUse); again {
		t.Error("an attempt that asked for no particular address was retried without one")
	}
}

func TestRunAcquisition6_AHintedConflictEndsTheLoop(t *testing.T) {
	hint := netip.MustParseAddr("fd00:6470:6863::90")
	conflict := lease.Event{Kind: lease.Failed, Reason: proto.ReasonConflict, Note: "in use"}

	client := &fakeV6Client{
		events: make(chan lease.Event, 1),
		router: proto.RouterObservation{Seen: true, Managed: true},
	}
	client.events <- conflict
	_, took, err := acquisition6Result(t, context.Background(), client,
		&DHCPClientOptions{V6: true}, hint, 5*time.Second, 10*time.Second)
	if !errors.Is(err, errV6HintInUse) {
		t.Fatalf("runAcquisition6 returned %v after %v, want errV6HintInUse", err, took)
	}
	if took > 2*time.Second {
		t.Errorf("the conflict took %v to reach the caller under a 5s window; the "+
			"remaining budget is what the second attempt has to run in", took)
	}

	unhinted := &fakeV6Client{
		events: make(chan lease.Event, 1),
		router: proto.RouterObservation{Seen: true, Managed: true},
	}
	unhinted.events <- conflict
	_, took, err = acquisition6Result(t, context.Background(), unhinted,
		&DHCPClientOptions{V6: true}, netip.Addr{}, time.Second, 10*time.Second)
	if errors.Is(err, errV6HintInUse) {
		t.Fatalf("an unhinted conflict produced errV6HintInUse after %v", took)
	}
	if took < time.Second {
		t.Errorf("the unhinted acquisition ended after %v, before its window was out; "+
			"the library's own recovery never got to run", took)
	}
}
