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

// Every mode the library declares has an option spelling, and every
// option spelling is a mode the machine implements.
//
// THE TWO SETS ARE ONE SET OR THEY DRIFT. An option value the plugin
// accepts and the machine does not implement is a `docker network
// create` that succeeds and an endpoint that fails; a mode the machine
// implements and the option cannot name is a feature nobody can reach.
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

// The unset option and `ipv6_mode=dhcp` are the same number and not the
// same instruction, which is the whole reason ParseIPv6Mode has a third
// result. proto.Mode6's zero is Mode6DHCP, so a parser that answered
// with the mode alone would make an unset option indistinguishable from
// the one value that switches DHCPv6 on.
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

// A typo is refused and never resolved to the zero value, which is
// `dhcp`: a network created with `ipv6_mode=slack` would otherwise
// quietly buy the mode the operator was moving away from.
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

// IPv6ModeFormsAddresses is a second spelling of an unexported library
// predicate, and this is the check that keeps the two in agreement.
//
// IT DRIVES THE LIBRARY RATHER THAN RESTATING IT. proto.Params6's own
// validation refuses a machine with no LinkAddr in exactly the modes
// that form addresses (ErrNoLinkAddr), so building one per declared
// mode with the link address left out asks the library which modes
// those are. A mode added later, or a mode that changes side, fails
// here rather than in the field.
func TestIPv6ModeFormsAddresses_AgreesWithTheLibrary(t *testing.T) {
	for _, m := range proto.AllModes6() {
		p := proto.DefaultParams6()
		p.DUID = []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0, 2}
		p.IAID = 0xac110002
		p.Mode = m
		p.LinkAddr = nil

		_, err := proto.New6(p)
		if m == proto.Mode6Off {
			// Off never reaches a machine at all; buildParams6 refuses
			// it one layer up, and the library refuses it here.
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

// The mode, the link address and the library's Reconfigure default, on
// the Params6 THIS PLUGIN BUILDS.
//
// READ OFF THE BUILT VALUE AND NOT OFF THE LIBRARY'S SOURCE (#925). The
// library's DefaultParams6 sets AcceptReconfigure true
// (proto/backoff6.go:288) and that is where the value comes from, but a
// proof that reads it there passes just as well for a chassis that
// overrode it afterwards. Asking the plugin for its own parameters is
// the only reading that can catch an override, which is the finding
// #925 asks this plugin to rule out.
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
			// A LinkAddr the library would refuse never reaches it.
			if _, err := proto.New6(p); err != nil {
				t.Errorf("the library refuses the Params6 this plugin builds in mode %v: %v", m, err)
			}
		})
	}
}

// The strict setting has ONE spelling the library reads: a negative
// AutoFallback. Zero is "the caller did not say", which resolves to
// half the router-discovery window -- so a strict setting that shipped
// as a zero would read as honoured at every layer above and fall back
// anyway.
//
// BOTH ARMS ARE ONE TABLE on purpose: the default arm is the
// preservation control for the strict one, and a change that made
// everything strict would pass a test that only drove `true`.
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

// A stored preferred address is not asked for in `slaac`, and it is
// still parsed and still refused when malformed.
//
// IGNORED AT THE ONE PLACE THAT BUILDS Params6, not by never reading
// it: a value that is read and dropped can be tested, and a value
// nothing reads cannot.
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

	// The preservation control for the refusal: dropping the hint in
	// slaac must not drop the VALIDATION of it, or a typo in
	// preferred_ipv6 becomes silent on exactly the networks where it is
	// hardest to notice.
	o := testOpts6(t)
	o.Mode6 = proto.Mode6SLAAC
	o.MAC = []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	o.PreferredV6 = "not-an-address"
	if _, err := buildParams6(o, true); err == nil {
		t.Error("buildParams6 accepted a malformed preferred_ipv6 in slaac; the value is " +
			"not asked for in that mode and it is still wrong")
	}
}

// Mode6Off never reaches the library from here.
func TestBuildParams6_RefusesModeOff(t *testing.T) {
	o := testOpts6(t)
	o.Mode6 = proto.Mode6Off
	if _, err := buildParams6(o, true); err == nil {
		t.Fatal("buildParams6 built parameters for a network whose ipv6_mode is off")
	}
}

// The three causes #816 tells apart, read off the library's own event.
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
		// RFC 9915 section 21.13 makes an absent Status Code and
		// Success one verdict. A Nak carrying neither is v4-shaped and
		// must not produce a refusal message naming "Success".
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

// THE BOUNDARY OF THIS CHANGE, PINNED AS A CASE RATHER THAN DESCRIBED.
//
// `ipv6_mode=slaac` reaches the library, and the library forms an
// address from an autonomous prefix and stamps it as an acquired lease
// (proto/machine6_slaac.go:302), which acquireStep6 would turn into an
// installed /128 like any other. It does not get that far, because the
// chassis concludes the acquisition the moment an advertisement says
// M=0 O=0 (ErrNoDHCPv6OnSegment, chassis6.go:155) -- which is the
// ordinary SLAAC segment, and therefore exactly the segment `slaac`
// exists for. That early conclusion is #868's fix and is not
// mode-aware.
//
// SO THE ENDPOINT STARTS WITHOUT A SLAAC ADDRESS TODAY, and this test
// asserts that, deliberately, as the record of where this change stops.
// The reference row and the release note say the same thing in words;
// this is the copy that goes red when #818 makes the conclusion
// mode-aware, which is the point at which those words have to change.
// Without it "address formation lands in later work" is a sentence
// nothing checks, and the two halves of the documentation could drift
// apart again with nothing failing.
func TestRunAcquisition6_SLAACStillEndsOnAnAdvertisedAbsence(t *testing.T) {
	for _, m := range []proto.Mode6{proto.Mode6SLAAC, proto.Mode6Auto} {
		t.Run(m.String(), func(t *testing.T) {
			client := &fakeV6Client{
				events: make(chan lease.Event),
				router: proto.RouterObservation{Seen: true},
			}
			_, _, err := acquisition6Result(t, context.Background(), client,
				&DHCPClientOptions{V6: true, Mode6: m}, netip.Addr{}, 3*time.Second, 10*time.Second)

			if !errors.Is(err, ErrNoDHCPv6OnSegment) {
				t.Fatalf("runAcquisition6 in ipv6_mode=%s returned %v, want "+
					"ErrNoDHCPv6OnSegment.\nIf this is #818 landing, that is the intended "+
					"change: update docs/reference.md's ipv6_mode row, the DHCPv6 verdict "+
					"table and RELEASE_NOTES.md's \"Not in this release\" entry in the same "+
					"commit, because all three say the endpoint gets no address here.", m, err)
			}
		})
	}
}

// The chain that makes dhcpv6_auto_fallbacks mean anything: the
// library's running total, this chassis's delta, the plugin's counter.
//
// WITHOUT THIS THE COUNTER IS DRIVEN ONLY BY CALLING ITS OWN CALLBACK,
// which tests the plugin's arithmetic on a number nothing produced. The
// library's lease.Stats is a RUNNING TOTAL and this chassis is asked
// for it more than once per acquisition, so the whole of what could go
// wrong here is reporting the total every time: an endpoint that fell
// back once would be counted once per poll, and the counter an operator
// reads as "how many containers are on an advertised prefix" would be a
// count of how often the chassis looked.
func TestV6ModeReport_ReportsTheGainAndNeverTheTotal(t *testing.T) {
	var got []uint64
	o := &DHCPClientOptions{V6: true, Mode6: proto.Mode6Auto, OnV6Fallback: func(n uint64) {
		got = append(got, n)
	}}

	o.v6ModeReport(lease.Stats{SLAACFallbacks: 0})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1}) // the same reading again
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

	// The other direction, and it is the one that is easy to lose: a
	// reading that did not move must produce nothing, and a total that
	// went DOWN must not be reported as a gain. lease.Stats is
	// monotonic per manager, so the second is unreachable today and is
	// asserted because the guard is written as an inequality and an
	// inequality has a direction.
	got = nil
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 3})
	o.v6ModeReport(lease.Stats{SLAACFallbacks: 1})
	if len(got) != 0 {
		t.Errorf("a reading that did not rise produced %v", got)
	}

	// A chassis with no reporter behind it does not panic: the
	// one-shot acquisition builds these options without a plugin.
	(&DHCPClientOptions{V6: true}).v6ModeReport(lease.Stats{SLAACFallbacks: 5})
}
