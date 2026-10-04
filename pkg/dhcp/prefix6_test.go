// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// A delegated prefix is routed, never assigned (RFC 3633 section 12.1): it is never Docker's address and it never
// joins Addrs, which go on the link (#214).
func TestInfoFromLease_DelegatedPrefixesRenderApartFromTheAddresses(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	stable := netip.MustParsePrefix("fd00:6470::1000/128")
	l := lease.Lease{
		Addr:   stable,
		Expire: now.Add(time.Hour),
		Addrs:  []lease.Addr6{{Addr: stable, Valid: now.Add(time.Hour), Preferred: now.Add(30 * time.Minute)}},
		Prefixes: []lease.Addr6{
			{Addr: netip.MustParsePrefix("fd00:98:0:1::/64"), Valid: now.Add(2 * time.Hour), Preferred: now.Add(time.Hour)},
			{Addr: netip.MustParsePrefix("fd00:98:0:2::/64"), Valid: now.Add(-time.Second), Preferred: now.Add(-time.Minute)},
			{Addr: netip.MustParsePrefix("fd00:98:0:3::/64"), Valid: now.Add(15 * time.Minute), Preferred: now.Add(10 * time.Minute)},
		},
	}
	for name, main := range map[string]netip.Prefix{
		"no main prefix":                   {},
		"a main prefix a delegated one is": netip.MustParsePrefix("fd00:98::/48"),
	} {
		info, _ := infoFromLease(l, proto.RouterObservation{}, now, main)
		if info.IP != "fd00:6470::1000/128" || len(info.Addrs) != 1 {
			t.Errorf("%s: Info.IP = %q, Addrs = %+v, want the IA_NA address alone", name, info.IP, info.Addrs)
		}
		want := []V6Addr{
			{IP: "fd00:98:0:1::/64", ValidSeconds: 7200, PreferredSeconds: 3600},
			{IP: "fd00:98:0:3::/64", ValidSeconds: 900, PreferredSeconds: 600},
		}
		if len(info.DelegatedPrefixes) != len(want) {
			t.Fatalf("%s: Info.DelegatedPrefixes = %+v, want %+v (both live ones, the expired one dropped)",
				name, info.DelegatedPrefixes, want)
		}
		for i := range want {
			if info.DelegatedPrefixes[i] != want[i] {
				t.Errorf("%s: prefix %d = %+v, want %+v", name, i, info.DelegatedPrefixes[i], want[i])
			}
		}
	}
	info, _ := infoFromLease(lease.Lease{Addr: stable, Addrs: l.Addrs}, proto.RouterObservation{}, now, netip.Prefix{})
	if info.DelegatedPrefixes != nil {
		t.Errorf("Info.DelegatedPrefixes = %+v on a lease with no IA_PD, want nil", info.DelegatedPrefixes)
	}
}

func TestBuildParams6_IPv6PDPutsOneIAPDInTheSolicit(t *testing.T) {
	p, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.PrefixHint != 0 || firstSolicit6(t, p).Options.Count(wire.OptV6IAPD) != 0 {
		t.Fatal("a network without ipv6_pd asks for a prefix")
	}
	opts := testOpts6(t)
	opts.IPv6PD = 56
	if p, err = buildParams6(opts, false); err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.PrefixHint != 56 {
		t.Errorf("Params6.PrefixHint = %d, want 56", p.PrefixHint)
	}
	if n := firstSolicit6(t, p).Options.Count(wire.OptV6IAPD); n != 1 {
		t.Errorf("the Solicit carries %d IA_PD option(s), want exactly one (RFC 8415 section 21.21)", n)
	}
}

// The address-refusal retry drops the address hint only; NoPrefixAvail never refuses the exchange (#214).
func TestRetryWithoutHint6_KeepsThePrefixHint(t *testing.T) {
	opts := &DHCPClientOptions{V6: true, IPv6PD: 64, Resume: &lease.Lease{}}
	opts.params6 = proto.Params6{Hint: netip.MustParseAddr("fd00:6470:6863::90"), PrefixHint: 64}
	if _, again := opts.retryWithoutHint6(fmt.Errorf("dhcp: %w: in use", errV6HintInUse)); !again {
		t.Fatal("the hinted attempt was not retried")
	}
	if opts.params6.PrefixHint != 64 {
		t.Errorf("the retry asks for a /%d, want the /64 the network asked for", opts.params6.PrefixHint)
	}
}

// A record that held prefixes, resumed on a network that no longer asks for one, would Rebind and route them (#214).
func TestResumeFor_ClearsThePrefixesOnlyWhenNoneIsAskedFor(t *testing.T) {
	held := []lease.Addr6{{Addr: netip.MustParsePrefix("fd00:98:0:1::/64")}}
	rec := &lease.Lease{Addr: netip.MustParsePrefix("fd00:6470::1000/128"), Prefixes: held}

	got := resumeFor(rec, 0)
	if len(got.Prefixes) != 0 {
		t.Errorf("resume with ipv6_pd removed still carries %v", got.Prefixes)
	}
	if got.Addr != rec.Addr {
		t.Errorf("the address %v was lost with the prefixes; it must still be confirmed", got.Addr)
	}
	if len(rec.Prefixes) != 1 {
		t.Error("the record itself was changed; the clearing must be on a copy")
	}
	if got := resumeFor(rec, 64); got != rec {
		t.Error("a network that still asks for a prefix lost the resumed one")
	}
	if got := resumeFor(nil, 0); got != nil {
		t.Errorf("resumeFor(nil) = %v", got)
	}
}

// Every v6 client is opened by newLibClient6, so its Resume going through resumeFor covers both clients (#214).
func TestNewLibClient6_TheResumeGoesThroughResumeFor(t *testing.T) {
	tree, err := parser.ParseFile(token.NewFileSet(), "chassis6.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(tree, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "newLibClient6" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Resume" {
				call, ok := kv.Value.(*ast.CallExpr)
				if ok {
					if f, ok := call.Fun.(*ast.Ident); ok && f.Name == "resumeFor" {
						found = true
					}
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Error("newLibClient6 passes Resume without resumeFor: a record's prefixes are resumed on a network " +
			"that no longer asks for one")
	}
}
