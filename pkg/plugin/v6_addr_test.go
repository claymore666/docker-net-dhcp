// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// A DHCPv6 address is installed with duplicate-address detection turned
// OFF, because it has already passed one (D30 Q1).
//
// RFC 9915 section 18.2.10.1 makes the CLIENT run the check: "The client
// performs duplicate address detection on each of the received addresses
// in any IAs it accepts before using that address for traffic". The
// library does exactly that and only then emits Acquired. Handing the
// address to the kernel without this flag makes RFC 4862 section 5.4 run
// a second time on an address that just passed, and the second run can
// FAIL where the first did not: RFC 7527 section 4.1's loopback case, or
// any node that answers the probe, marks the address `dadfailed` and the
// kernel withdraws it. RFC 4429 section 3.3 is the same argument from
// the other side.
//
// This is asserted on the flag and not on `ip -6 addr` timing on
// purpose: without the flag the address is `tentative` only for the
// length of the check, so a proof that reads the link state passes on a
// fast box and fails on a loaded runner.
func TestV6AddrAttrs_TurnsOffDuplicateAddressDetection(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr, dhcp.Info{LeaseSeconds: 3600, PreferredSeconds: 1800})

	if addr.Flags&unix.IFA_F_NODAD == 0 {
		t.Error("IFA_F_NODAD is not set: the kernel re-runs a check the library already " +
			"passed, and a second run that fails takes the address out of service")
	}
	if addr.ValidLft != 3600 {
		t.Errorf("ValidLft = %d, want 3600", addr.ValidLft)
	}
	if addr.PreferedLft != 1800 {
		t.Errorf("PreferedLft = %d, want 1800", addr.PreferedLft)
	}
}

// The flag is ORed in, not assigned. netlink.ParseAddr and the callers
// above it can put flags on the address already, and an assignment here
// would drop them silently.
func TestV6AddrAttrs_KeepsFlagsItDidNotSet(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	addr.Flags |= unix.IFA_F_NOPREFIXROUTE
	v6AddrAttrs(addr, dhcp.Info{})
	if addr.Flags&unix.IFA_F_NOPREFIXROUTE == 0 {
		t.Error("v6AddrAttrs cleared a flag it did not set")
	}
	if addr.Flags&unix.IFA_F_NODAD == 0 {
		t.Error("IFA_F_NODAD is not set")
	}
}

// An infinite lease is both lifetimes zero, which is what makes the
// kernel send no IFA_CACHEINFO at all.
//
// The alternative encoding -- a very large number -- would be a
// countdown the kernel eventually reaches. Zero here is netlink's
// "forever", and it is the ONLY value that means it: a one-second
// lifetime and an infinite one differ by one integer.
func TestV6AddrAttrs_InfiniteLeaseSendsNoLifetimes(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr, dhcp.Info{})
	if addr.ValidLft != 0 || addr.PreferedLft != 0 {
		t.Errorf("an infinite lease gave ValidLft=%d PreferedLft=%d, want both zero",
			addr.ValidLft, addr.PreferedLft)
	}
	// And a finite one does send them, or the assertion above is
	// satisfied by a function that never sets anything.
	v6AddrAttrs(addr, dhcp.Info{LeaseSeconds: 10, PreferredSeconds: 5})
	if addr.ValidLft == 0 || addr.PreferedLft == 0 {
		t.Error("a finite lease produced no lifetimes")
	}
}

// The preferred lifetime never outlives the valid one.
//
// RFC 4862 section 5.5.3 e) treats a preferred lifetime longer than the
// valid one as a malformed advertisement; the kernel clamps rather than
// refuses, so the symptom of getting this backwards is an address that
// is preferred right up to the moment it disappears -- no deprecation
// window, and every connection established in it dies at once.
//
// The chassis is what guarantees the ordering (Preferred is derived from
// the lease's own preferred deadline and falls back to the valid one),
// so this reads the pair the chassis produces rather than an invented
// one.
func TestV6AddrAttrs_PreferredNeverExceedsValid(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	for _, info := range []dhcp.Info{
		{LeaseSeconds: 3600, PreferredSeconds: 1800},
		{LeaseSeconds: 3600, PreferredSeconds: 3600},
		{},
	} {
		v6AddrAttrs(addr, info)
		if addr.ValidLft != 0 && addr.PreferedLft > addr.ValidLft {
			t.Errorf("%+v gave PreferedLft=%d above ValidLft=%d",
				info, addr.PreferedLft, addr.ValidLft)
		}
	}
}

// NODAD is set in ONE place, and that place is the v6 arm.
//
// WHY A SOURCE-LEVEL TEST. The flag reaches the kernel through a netlink
// socket against a real link in a real namespace; there is no seam
// between the manager and that socket that a unit test can sit in. What
// IS checkable is that no other site sets the flag -- because the
// failure that matters is not "it was set wrongly" but "a v4 address
// picked it up too", and a v4 address installed with NODAD skips RFC
// 5227's check that the library ran for v6 and did not run for v4 in
// this mode. That is silent: the address works until another host on the
// segment has it too.
func TestNODAD_IsSetOnlyOnTheV6Path(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var sites []string
	parsed := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "IFA_F_NODAD" {
				return true
			}
			sites = append(sites, fset.Position(sel.Pos()).String())
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("parsed no files: the search has an empty domain")
	}
	if len(sites) != 1 {
		t.Fatalf("IFA_F_NODAD is named at %d sites, want exactly 1 (v6AddrAttrs): %v",
			len(sites), sites)
	}
	if !strings.Contains(sites[0], "dhcp_manager.go") {
		t.Errorf("IFA_F_NODAD is set at %v, expected v6AddrAttrs in dhcp_manager.go", sites[0])
	}
}

// And the v6 attributes are applied only when the family is v6.
//
// The same argument in the other direction: v6AddrAttrs called
// unconditionally would put NODAD and a pair of lifetimes on every IPv4
// address the plugin installs. The lifetimes are the loud half -- a v4
// address would start expiring -- and NODAD is the silent one.
func TestV6AddrAttrs_IsCalledUnderTheFamilySwitch(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dhcp_manager.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	calls := 0
	guarded := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		cond, ok := ifs.Cond.(*ast.Ident)
		if !ok || cond.Name != "v6" || ifs.Init != nil {
			return true
		}
		ast.Inspect(ifs.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "v6AddrAttrs" {
				guarded++
			}
			return true
		})
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "v6AddrAttrs" {
			calls++
		}
		return true
	})
	if calls == 0 {
		t.Fatal("v6AddrAttrs is never called: the address goes to the kernel without " +
			"NODAD or lifetimes and this test's domain is empty")
	}
	if guarded != calls {
		t.Errorf("v6AddrAttrs is called %d times and only %d of those are under `if v6`; "+
			"an unguarded call puts NODAD and a countdown on every IPv4 address",
			calls, guarded)
	}
}

// TestHealthClient_IsPublishedOnlyForV4 pins which family the
// `endpoints` array of /Plugin.Health describes.
//
// A dual-stack endpoint runs two clients and the array has one entry
// per ENDPOINT, so one of the two has to be the one it reads. It is
// the v4 client: `address`, `lease_state`, the three lease times and
// the RFC 5227 pair all come from it, and RFC 5227 is a v4 protocol
// with no v6 counterpart at all. docs/reference.md states that bound
// on the `endpoints` row.
//
// The guard is `if !v6` around ONE call, and inverting it is silent in
// exactly the way this array cannot afford: the entry would carry the
// container's IPv6 address in a field every consumer reads as its
// IPv4 one, with an `acd_phase` belonging to a client that never ran
// ACD. Nothing else in the document would disagree.
func TestHealthClient_IsPublishedOnlyForV4(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dhcp_manager.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	calls := 0
	guarded := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "setHealthClient" {
			return true
		}
		calls++
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		un, ok := ifs.Cond.(*ast.UnaryExpr)
		if !ok || un.Op != token.NOT || ifs.Init != nil {
			return true
		}
		if id, ok := un.X.(*ast.Ident); !ok || id.Name != "v6" {
			return true
		}
		ast.Inspect(ifs.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "setHealthClient" {
				guarded++
			}
			return true
		})
		return true
	})

	if calls == 0 {
		t.Fatal("setHealthClient is never called in dhcp_manager.go: the endpoints array " +
			"reports no lease for any endpoint and this test's domain is empty")
	}
	if guarded != calls {
		t.Errorf("setHealthClient is called %d times and %d of those are under `if !v6`; "+
			"an unguarded or v6-guarded call publishes the DHCPv6 client as the one the "+
			"endpoints array reads, so `address` carries a v6 lease in a field consumers "+
			"read as the v4 one", calls, guarded)
	}
}
