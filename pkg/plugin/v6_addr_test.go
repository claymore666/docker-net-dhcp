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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
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
	v6AddrAttrs(addr, 3600, 1800)

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
	v6AddrAttrs(addr, 0, 0)
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
	v6AddrAttrs(addr, 0, 0)
	if addr.ValidLft != 0 || addr.PreferedLft != 0 {
		t.Errorf("an infinite lease gave ValidLft=%d PreferedLft=%d, want both zero",
			addr.ValidLft, addr.PreferedLft)
	}
	// And a finite one does send them, or the assertion above is
	// satisfied by a function that never sets anything.
	v6AddrAttrs(addr, 10, 5)
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
		v6AddrAttrs(addr, info.LeaseSeconds, info.PreferredSeconds)
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
//
// THE RULE IS "REACHED ONLY FROM THE V6 ARM", NOT "WRITTEN INSIDE IT".
// The apply path installs a LIST of addresses (#818), each carrying its
// own lifetimes, so the call that stamps them sits one frame below the
// `if v6` that decides the family. A rule keyed on the neighbouring
// text would be satisfied by moving the call back up and would refuse a
// helper that cannot be reached from the v4 path at all, so it is keyed
// on the call graph instead: a call is allowed where it is lexically
// under `if v6`, or inside a function EVERY call site of which is
// itself allowed. A function nothing in the package calls is not
// allowed, so the rule cannot be satisfied by making its subject
// unreachable.
//
// WHAT IT CANNOT SEE, stated rather than hidden: a function value. A
// closure written under `if v6` and called from somewhere else reads as
// guarded, and a call made through a variable of function type has no
// callee name to follow. Both are refused by TestNODAD_IsSetOnlyOnTheV6Path
// only insofar as they name the flag themselves; a second helper that
// took v6AddrAttrs as a parameter would pass both. The package has no
// such call today and the analyser is driven against a synthetic one in
// TestV6AttrGuard_RefusesACallThatEscapesTheV6Arm.
func TestV6AddrAttrs_IsCalledUnderTheFamilySwitch(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no files: the search has an empty domain")
	}
	g := v6AttrGuardOf(fset, files)
	if len(g.sites) == 0 {
		t.Fatal("v6AddrAttrs is never called: the address goes to the kernel without " +
			"NODAD or lifetimes and this test's domain is empty")
	}
	for _, name := range g.ambiguous {
		t.Errorf("%q names more than one function in this package, so the call graph "+
			"this rule walks is not the one the compiler resolves", name)
	}
	for _, site := range g.unguarded {
		t.Errorf("v6AddrAttrs is called at %s, which the v4 path can reach; "+
			"an unguarded call puts NODAD and a countdown on every IPv4 address", site)
	}
}

// v6AttrGuardOf answers, for one parsed package, which calls to
// v6AddrAttrs are reachable only from a family switch that has already
// chosen v6.
//
// `if v6 { ... }` with no init statement is the only guard it reads,
// because it is the only one the apply path writes: the family is a
// bool parameter threaded through renew, applyAddressChange and the
// phases below them. An `else` branch is outside the body's braces and
// so is not guarded, which is the direction that matters.
type v6AttrGuard struct {
	sites     []string
	unguarded []string
	v6Only    map[string]bool
	ambiguous []string
}

type v6AttrCall struct {
	callee  string
	caller  string
	guarded bool
	pos     string
}

func v6AttrGuardOf(fset *token.FileSet, files []*ast.File) v6AttrGuard {
	type span struct {
		lo, hi token.Pos
	}
	var guards []span
	decls := map[string]int{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Init != nil {
				return true
			}
			if cond, ok := ifs.Cond.(*ast.Ident); ok && cond.Name == "v6" {
				guards = append(guards, span{ifs.Body.Lbrace, ifs.Body.Rbrace})
			}
			return true
		})
	}
	inGuard := func(p token.Pos) bool {
		for _, g := range guards {
			if p > g.lo && p < g.hi {
				return true
			}
		}
		return false
	}

	var calls []v6AttrCall
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			decls[fn.Name.Name]++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				if name == "" {
					return true
				}
				calls = append(calls, v6AttrCall{
					callee:  name,
					caller:  fn.Name.Name,
					guarded: inGuard(call.Pos()),
					pos:     fset.Position(call.Pos()).String(),
				})
				return true
			})
		}
	}

	byCallee := map[string][]v6AttrCall{}
	for _, c := range calls {
		byCallee[c.callee] = append(byCallee[c.callee], c)
	}
	// Ascending fixed point from the lexically guarded calls. A
	// function with no call site in this package never enters the set,
	// so "nothing calls it" is not an answer.
	v6Only := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for callee, sites := range byCallee {
			if v6Only[callee] || callee == "v6AddrAttrs" {
				continue
			}
			ok := true
			for _, s := range sites {
				if !s.guarded && !v6Only[s.caller] {
					ok = false
					break
				}
			}
			if ok {
				v6Only[callee] = true
				changed = true
			}
		}
	}

	out := v6AttrGuard{v6Only: v6Only}
	for _, c := range calls {
		if c.callee != "v6AddrAttrs" {
			continue
		}
		out.sites = append(out.sites, c.pos)
		if c.guarded || v6Only[c.caller] {
			if !c.guarded && decls[c.caller] > 1 {
				out.ambiguous = append(out.ambiguous, c.caller)
			}
			continue
		}
		out.unguarded = append(out.unguarded, c.pos)
	}
	return out
}

// The analyser is driven against sources that are wrong in each of the
// ways it exists to catch, and against the shape it must keep allowing.
//
// Without this, the rule above is a rule with one possible verdict:
// every widening of a guard rule has to show that the widened rule
// still goes red, and that the narrow case it used to cover is still
// covered.
func TestV6AttrGuard_RefusesACallThatEscapesTheV6Arm(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{
			name: "lexically under the switch",
			src: `package p
func renew(v6 bool) { if v6 { v6AddrAttrs() } }
func v6AddrAttrs() {}`,
			want: 0,
		},
		{
			name: "one frame below a guarded call",
			src: `package p
func renew(v6 bool) { if v6 { install() } }
func install() { v6AddrAttrs() }
func v6AddrAttrs() {}`,
			want: 0,
		},
		{
			name: "unguarded at the top",
			src: `package p
func renew(v6 bool) { v6AddrAttrs() }
func v6AddrAttrs() {}`,
			want: 1,
		},
		{
			name: "a helper the v4 path also calls",
			src: `package p
func renew(v6 bool) { if v6 { install() } }
func bind() { install() }
func install() { v6AddrAttrs() }
func v6AddrAttrs() {}`,
			want: 1,
		},
		{
			name: "the else branch is not the guard",
			src: `package p
func renew(v6 bool) { if v6 { } else { v6AddrAttrs() } }
func v6AddrAttrs() {}`,
			want: 1,
		},
		{
			name: "a helper nothing calls",
			src: `package p
func install() { v6AddrAttrs() }
func v6AddrAttrs() {}`,
			want: 1,
		},
		{
			name: "a guard on some other bool",
			src: `package p
func renew(v4 bool) { if v4 { v6AddrAttrs() } }
func v6AddrAttrs() {}`,
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "p.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			g := v6AttrGuardOf(fset, []*ast.File{f})
			if len(g.sites) != 1 {
				t.Fatalf("found %d call sites in a source with one", len(g.sites))
			}
			if len(g.unguarded) != tc.want {
				t.Errorf("refused %d calls, want %d (%v)", len(g.unguarded), tc.want, g.unguarded)
			}
		})
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
