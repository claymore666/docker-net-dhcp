// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

func TestParseIPv6Mode_IsTheLibrarysOwnEnumeration(t *testing.T) {
	all := proto.AllModes6()
	if len(all) == 0 {
		t.Fatal("proto.AllModes6() is empty, so every loop here judges nothing")
	}
	if got, want := len(IPv6Modes()), len(all); got != want {
		t.Errorf("IPv6Modes() has %d value(s), the library declares %d", got, want)
	}
	for _, m := range all {
		mode, set, err := ParseIPv6Mode(m.String())
		if err != nil {
			t.Errorf("ParseIPv6Mode(%q): %v — the library declares this mode and the "+
				"option cannot name it", m.String(), err)
			continue
		}
		if !set {
			t.Errorf("ParseIPv6Mode(%q) reported the option as unset", m.String())
		}
		if mode != m {
			t.Errorf("ParseIPv6Mode(%q) = %v, want %v", m.String(), mode, m)
		}
	}
}

func TestParseIPv6Mode_UnsetIsNotDHCP(t *testing.T) {
	mode, set, err := ParseIPv6Mode("")
	if err != nil {
		t.Fatalf("ParseIPv6Mode(\"\"): %v", err)
	}
	if set {
		t.Error("an empty ipv6_mode reported itself as set")
	}
	if mode != proto.Mode6Off {
		t.Errorf("an empty ipv6_mode parsed as %v; it must not carry a working mode, "+
			"because the caller decides what unset means from `ipv6`", mode)
	}
}

func TestParseIPv6Mode_RefusesAValueOutsideTheSet(t *testing.T) {
	_, set, err := ParseIPv6Mode("slack")
	if err == nil {
		t.Fatal("ParseIPv6Mode accepted a value the library does not declare")
	}
	if set {
		t.Error("a refused value reported itself as set")
	}
	for _, m := range IPv6Modes() {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("the refusal does not name %q, so it does not tell the operator "+
				"what to write instead: %v", m, err)
		}
	}
}

// proto.Params6 refuses a missing LinkAddr exactly in the forming modes (ErrNoLinkAddr), so the library is asked
// (#817).

func TestIPv6ModeFormsAddresses_AgreesWithTheLibrary(t *testing.T) {
	for _, m := range proto.AllModes6() {
		p := proto.DefaultParams6()
		p.DUID = []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0, 2}
		p.IAID = 0xac110002
		p.Mode = m
		p.LinkAddr = nil

		_, err := proto.New6(p)
		if m == proto.Mode6Off {
			if !errors.Is(err, proto.ErrMode6Off) {
				t.Errorf("proto.New6 with Mode6Off returned %v, want ErrMode6Off", err)
			}
			continue
		}
		needs := errors.Is(err, proto.ErrNoLinkAddr)
		if got := IPv6ModeFormsAddresses(m); got != needs {
			t.Errorf("IPv6ModeFormsAddresses(%v) = %v, but the library %s a link address "+
				"in that mode (New6 said: %v). The two have drifted; the library is right.",
				m, got, map[bool]string{true: "requires", false: "does not require"}[needs], err)
		}
	}
}

// Read off the built Params6, where an override would show; the library's DefaultParams6 sets AcceptReconfigure (#925).

func TestBuildParams6_CarriesTheModeTheLinkAddressAndReconfigure(t *testing.T) {
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	for _, m := range proto.AllModes6() {
		if m == proto.Mode6Off {
			continue
		}
		t.Run(m.String(), func(t *testing.T) {
			o := testOpts6(t)
			o.Mode6 = m
			o.MAC = mac

			p, err := buildParams6(o, true)
			if err != nil {
				t.Fatalf("buildParams6: %v", err)
			}
			if p.Mode != m {
				t.Errorf("Params6.Mode = %v, want %v — the network's ipv6_mode did not "+
					"reach the machine, which then runs in the enum's zero value", p.Mode, m)
			}
			if !bytes.Equal(p.LinkAddr, mac) {
				t.Errorf("Params6.LinkAddr = %x, want %x — RFC 4291 appendix A forms the "+
					"interface identifier from it, so a mode that forms addresses cannot start "+
					"without it", p.LinkAddr, mac)
			}
			if !p.AcceptReconfigure {
				t.Errorf("Params6.AcceptReconfigure is false in mode %v. The library's "+
					"DefaultParams6 sets it true (proto/backoff6.go:288) and RFC 9915 "+
					"section 21.20 makes the absence of the option mean the client is "+
					"unwilling, so something in this chassis turned it off. #925's server "+
					"side is answered by the library; this is the plugin side of it.", m)
			}
			if _, err := proto.New6(p); err != nil {
				t.Errorf("the library refuses the Params6 this plugin builds in mode %v: %v", m, err)
			}
		})
	}
}

// A zero AutoFallback resolves to half the router-discovery window; strict is only a negative value (#817).

func TestBuildParams6_StrictAutoIsTheValueTheLibraryReads(t *testing.T) {
	cases := []struct {
		name   string
		strict bool
		want   func(proto.Duration) bool
		why    string
	}{
		{"default", false, func(d proto.Duration) bool { return d == 0 },
			"zero is the library's own default, half the router-discovery window"},
		{"ipv6_auto_strict=true", true, func(d proto.Duration) bool { return d < 0 },
			"negative is the library's only spelling of \"never fall back\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := testOpts6(t)
			o.Mode6 = proto.Mode6Auto
			o.MAC = []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
			o.StrictAuto6 = c.strict

			p, err := buildParams6(o, true)
			if err != nil {
				t.Fatalf("buildParams6: %v", err)
			}
			if !c.want(p.AutoFallback) {
				t.Errorf("Params6.AutoFallback = %v with ipv6_auto_strict=%v; %s",
					p.AutoFallback, c.strict, c.why)
			}
		})
	}
}

func TestBuildParams6_PreferredAddressIsDroppedOnlyWhereItCannotBeAsked(t *testing.T) {
	const want = "fd00:98::10"
	cases := []struct {
		mode  proto.Mode6
		hints bool
	}{
		{proto.Mode6DHCP, true},
		{proto.Mode6Auto, true},
		{proto.Mode6SLAAC, false},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			o := testOpts6(t)
			o.Mode6 = c.mode
			o.MAC = []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
			o.PreferredV6 = want

			p, err := buildParams6(o, true)
			if err != nil {
				t.Fatalf("buildParams6: %v", err)
			}
			switch {
			case c.hints && p.Hint.String() != want:
				t.Errorf("Params6.Hint = %v in mode %v, want %v — the endpoint's stored "+
					"address is what makes a restart keep it (#213)", p.Hint, c.mode, want)
			case !c.hints && p.Hint.IsValid():
				t.Errorf("Params6.Hint = %v in mode %v; there is no server to ask, the "+
					"address comes from the router's prefix, and asking would be a "+
					"parameter the machine cannot act on", p.Hint, c.mode)
			}
		})
	}

	o := testOpts6(t)
	o.Mode6 = proto.Mode6SLAAC
	o.MAC = []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	o.PreferredV6 = "not-an-address"
	if _, err := buildParams6(o, true); err == nil {
		t.Error("buildParams6 accepted a malformed preferred_ipv6 in slaac; the value is " +
			"not asked for in that mode and it is still wrong")
	}
}

func TestBuildParams6_RefusesModeOff(t *testing.T) {
	o := testOpts6(t)
	o.Mode6 = proto.Mode6Off
	if _, err := buildParams6(o, true); err == nil {
		t.Fatal("buildParams6 built parameters for a network whose ipv6_mode is off")
	}
}

func TestV6FailureCause(t *testing.T) {
	cases := []struct {
		name  string
		ev    lease.Event
		check func(*testing.T, error)
	}{
		{"a server that refused", lease.Event{Reason: proto.ReasonNak, Status: wire.StatusNoAddrsAvail, Note: "pool empty"},
			func(t *testing.T, err error) {
				status, ok := V6RefusalStatus(err)
				if !ok {
					t.Fatalf("not reported as a refusal: %v", err)
				}
				if status != wire.StatusNoAddrsAvail.String() {
					t.Errorf("status name = %q, want %q", status, wire.StatusNoAddrsAvail.String())
				}
				if !strings.Contains(err.Error(), wire.StatusNoAddrsAvail.String()) {
					t.Errorf("the message does not name the code: %v", err)
				}
			}},
		// RFC 9915 section 21.13 makes an absent Status Code and Success one verdict (#816).
		{"a Nak with no status code", lease.Event{Reason: proto.ReasonNak},
			func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("a status-less Nak produced %v; it is not one of the three "+
						"causes the verdict table names", err)
				}
			}},
		{"a router with no usable prefix", lease.Event{Reason: proto.ReasonNoPrefix, Note: "2 option(s) refused"},
			func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoSLAACPrefix) {
					t.Errorf("got %v, want ErrNoSLAACPrefix", err)
				}
				if _, ok := V6RefusalStatus(err); ok {
					t.Error("reported as a server refusal; no server answered")
				}
			}},
		{"no router at all", lease.Event{Reason: proto.ReasonNoRouter, Note: "3 solicitation(s)"},
			func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoV6Router) {
					t.Errorf("got %v, want ErrNoV6Router", err)
				}
				if errors.Is(err, ErrNoV6Server) {
					t.Error("a link with no router was reported as a silent server; " +
						"they are different links and different things to go and fix")
				}
			}},
		{"a silent server", lease.Event{Reason: proto.ReasonNoServer},
			func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoV6Server) {
					t.Errorf("got %v, want ErrNoV6Server", err)
				}
			}},
		{"a conflict", lease.Event{Reason: proto.ReasonConflict},
			func(t *testing.T, err error) {
				if err != nil {
					t.Errorf("got %v; a reason outside the three keeps the caller's own "+
						"sentence rather than becoming one of them by default", err)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.ev.Kind = lease.Failed
			c.check(t, v6FailureCause(c.ev))
		})
	}
}

// Up to v2.1.x the M=0 O=0 conclusion ended slaac acquisitions with no address, a case #989 pinned; the dhcp row keeps
// #868's early verdict (#818).

func TestRunAcquisition6_OnlyAModeThatFormsAddressesOutlivesAnAdvertisedAbsence(t *testing.T) {
	cases := []struct {
		mode      proto.Mode6
		concludes bool
	}{
		{proto.Mode6DHCP, true},
		{proto.Mode6SLAAC, false},
		{proto.Mode6Auto, false},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			client := &fakeV6Client{
				events: make(chan lease.Event),
				router: proto.RouterObservation{Seen: true},
			}
			_, _, err := acquisition6Result(t, context.Background(), client,
				&DHCPClientOptions{V6: true, Mode6: c.mode}, netip.Addr{}, 3*time.Second, 10*time.Second)

			if got := errors.Is(err, ErrNoDHCPv6OnSegment); got != c.concludes {
				if c.concludes {
					t.Fatalf("runAcquisition6 in ipv6_mode=%s returned %v, want ErrNoDHCPv6OnSegment.\n"+
						"This is #868's early conclusion: an advertisement with neither the managed "+
						"nor the other-configuration flag ends the acquisition in about two seconds "+
						"instead of the whole budget, and a mode that does not form its own address "+
						"has nothing to wait for.", c.mode, err)
				}
				t.Fatalf("runAcquisition6 in ipv6_mode=%s returned %v, and an advertisement with "+
					"neither flag must NOT end the acquisition in a mode that forms its own "+
					"address (#818): that advertisement is where the address comes from. The "+
					"acquisition ends on what the library says instead -- Acquired once the "+
					"formed address passes duplicate address detection, or ErrNoSLAACPrefix.", c.mode, err)
			}
		})
	}
}

func TestRunAcquisition6_AFormingModeRunsToItsWindow(t *testing.T) {
	client := &fakeV6Client{
		events: make(chan lease.Event),
		router: proto.RouterObservation{Seen: true},
	}
	start := time.Now()
	_, _, err := acquisition6Result(t, context.Background(), client,
		&DHCPClientOptions{V6: true, Mode6: proto.Mode6SLAAC}, netip.Addr{}, 600*time.Millisecond, 10*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runAcquisition6 in ipv6_mode=slaac returned %v, want the acquisition window's "+
			"own deadline: nothing else ended it", err)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("runAcquisition6 in ipv6_mode=slaac returned after %v, want about the 600ms "+
			"window: it returned early, so the formed address had no time to pass duplicate "+
			"address detection", elapsed)
	}
}

func TestRunAcquisition6_AFormedAddressEndsTheAcquisition(t *testing.T) {
	events := make(chan lease.Event, 1)
	events <- lease.Event{Kind: lease.Acquired, Lease: lease.Lease{
		SLAAC: true,
		Addr:  netip.MustParsePrefix("2001:db8:1::42/64"),
		Addrs: []lease.Addr6{
			{Addr: netip.MustParsePrefix("2001:db8:1::42/64"), Valid: time.Now().Add(time.Hour), Preferred: time.Now().Add(30 * time.Minute)},
			{Addr: netip.MustParsePrefix("fd00:9::42/64"), Valid: time.Now().Add(2 * time.Hour), Preferred: time.Now().Add(time.Hour)},
		},
	}}
	client := &fakeV6Client{events: events, router: proto.RouterObservation{Seen: true}}

	info, _, err := acquisition6Result(t, context.Background(), client,
		&DHCPClientOptions{V6: true, Mode6: proto.Mode6SLAAC}, netip.Addr{}, 3*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("runAcquisition6 returned %v, want the formed address", err)
	}
	if info.IP != "2001:db8:1::42/64" {
		t.Errorf("Info.IP = %q, want the first advertised prefix's address", info.IP)
	}
	if !info.SLAAC {
		t.Error("Info.SLAAC is false for a lease the library marked as formed from an advertisement; " +
			"the ledger row and the outage rules both read it")
	}
	if len(info.Addrs) != 2 {
		t.Fatalf("Info.Addrs has %d entries, want both advertised prefixes: a chassis that installs "+
			"Info.IP alone leaves the second prefix unconfigured while the library keeps refreshing it",
			len(info.Addrs))
	}
}

func TestRunAcquisition6_TheNetworksMainPrefixChoosesTheReportedAddress(t *testing.T) {
	cases := []struct {
		name     string
		main     netip.Prefix
		wantIP   string
		wantBack bool
	}{
		{"unset selects the first advertised prefix", netip.Prefix{}, "2001:db8:1::42/64", false},
		{"names the second advertised prefix", netip.MustParsePrefix("fd00:9::/64"), "fd00:9::42/64", false},
		{"names the first advertised prefix", netip.MustParsePrefix("2001:db8:1::/64"), "2001:db8:1::42/64", false},
		{"names a prefix nothing advertised", netip.MustParsePrefix("2001:db8:ffff::/64"), "2001:db8:1::42/64", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := make(chan lease.Event, 1)
			events <- lease.Event{Kind: lease.Acquired, Lease: lease.Lease{
				SLAAC: true,
				Addr:  netip.MustParsePrefix("2001:db8:1::42/64"),
				Addrs: []lease.Addr6{
					{Addr: netip.MustParsePrefix("2001:db8:1::42/64"), Valid: time.Now().Add(time.Hour)},
					{Addr: netip.MustParsePrefix("fd00:9::42/64"), Valid: time.Now().Add(2 * time.Hour)},
				},
			}}
			client := &fakeV6Client{events: events, router: proto.RouterObservation{Seen: true}}

			info, _, err := acquisition6Result(t, context.Background(), client,
				&DHCPClientOptions{V6: true, Mode6: proto.Mode6SLAAC, MainPrefix6: c.main},
				netip.Addr{}, 3*time.Second, 10*time.Second)
			if err != nil {
				t.Fatalf("runAcquisition6 returned %v, want the formed addresses", err)
			}
			if info.IP != c.wantIP {
				t.Errorf("Info.IP = %q, want %q. That is the one address Docker is told about and "+
					"the one an operator reads out of `docker inspect`; the option that names it "+
					"reached no further than the option struct (#818).", info.IP, c.wantIP)
			}
			if info.MainAddrFallback != c.wantBack {
				t.Errorf("Info.MainAddrFallback = %v, want %v. It is the only evidence that the "+
					"prefix an operator named matched nothing the router advertised; the plugin "+
					"counts ipv6_main_prefix_unmatched and names both prefixes from it.",
					info.MainAddrFallback, c.wantBack)
			}
			if len(info.Addrs) != 2 {
				t.Errorf("Info.Addrs has %d entries, want 2: which address is REPORTED is a "+
					"separate question from which are installed, and choosing one must not drop "+
					"the other from the link", len(info.Addrs))
			}
		})
	}
}

func TestV6ModeReport_ReportsTheGainAndNeverTheTotal(t *testing.T) {
	var got []uint64
	o := &DHCPClientOptions{V6: true, Mode6: proto.Mode6Auto, OnV6Fallback: func(n uint64) {
		got = append(got, n)
	}}

	o.v6ModeReport(lease.Stats{SLAACFallbacks: 0})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 3})

	want := []uint64{1, 2}
	if len(got) != len(want) {
		t.Fatalf("the callback was called %d time(s) with %v, want %v. A call per reading "+
			"counts how often the chassis looked; a call per gain counts endpoints.", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d carried %d, want %d", i, got[i], want[i])
		}
	}

	got = nil
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 3})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1})
	if len(got) != 0 {
		t.Errorf("a reading that did not rise produced %v", got)
	}

	(&DHCPClientOptions{V6: true}).v6ModeReport(lease.Stats{SLAACFallbacks: 5})
}

// A router readvertises every few seconds (RFC 4861 section 6.2.1), so this count is reported as a gain (#818).

func TestV6PrefixReport_ReportsTheGainAndNeverTheTotal(t *testing.T) {
	var prefixes, fallbacks []uint64
	o := &DHCPClientOptions{
		V6:                  true,
		Mode6:               proto.Mode6Auto,
		OnV6Fallback:        func(n uint64) { fallbacks = append(fallbacks, n) },
		OnV6PrefixesIgnored: func(n uint64) { prefixes = append(prefixes, n) },
	}

	o.v6PrefixReport(lease.Stats{SLAACPrefixesIgnored: 0})
	o.v6PrefixReport(lease.Stats{SLAACPrefixesIgnored: 2})
	o.v6PrefixReport(lease.Stats{SLAACPrefixesIgnored: 2})
	o.v6PrefixReport(lease.Stats{SLAACPrefixesIgnored: 5})

	want := []uint64{2, 3}
	if len(prefixes) != len(want) {
		t.Fatalf("the callback was called %d time(s) with %v, want %v", len(prefixes), prefixes, want)
	}
	for i := range want {
		if prefixes[i] != want[i] {
			t.Errorf("call %d carried %d, want %d", i, prefixes[i], want[i])
		}
	}
	if len(fallbacks) != 0 {
		t.Errorf("reporting refused prefixes also reported %v fallbacks; the two reporters "+
			"share a struct and must not share a seen-value", fallbacks)
	}

	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1})
	if len(fallbacks) != 1 || fallbacks[0] != 1 {
		t.Errorf("the fallback reporter carried %v after the prefix reporter ran, want [1]", fallbacks)
	}

	(&DHCPClientOptions{V6: true}).v6PrefixReport(lease.Stats{SLAACPrefixesIgnored: 5})
}
