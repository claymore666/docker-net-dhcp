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

// RFC 9915 section 18.2.10.1 has the client run DAD before using the address, which the library does; a second
// kernel run (RFC 4862 section 5.4) can fail where the first passed (RFC 7527 section 4.1), so NODAD is set (#911).
func TestV6AddrAttrs_TurnsOffDuplicateAddressDetection(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr, 3600, 1800, false)

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

func TestV6AddrAttrs_KeepsFlagsItDidNotSet(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	addr.Flags |= unix.IFA_F_NOPREFIXROUTE
	v6AddrAttrs(addr, 0, 0, false)
	if addr.Flags&unix.IFA_F_NOPREFIXROUTE == 0 {
		t.Error("v6AddrAttrs cleared a flag it did not set")
	}
	if addr.Flags&unix.IFA_F_NODAD == 0 {
		t.Error("IFA_F_NODAD is not set")
	}
}

// Both lifetimes zero sends no IFA_CACHEINFO, which is netlink's forever (#911).
func TestV6AddrAttrs_InfiniteLeaseSendsNoLifetimes(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr, 0, 0, false)
	if addr.ValidLft != 0 || addr.PreferedLft != 0 {
		t.Errorf("an infinite lease gave ValidLft=%d PreferedLft=%d, want both zero",
			addr.ValidLft, addr.PreferedLft)
	}
	v6AddrAttrs(addr, 10, 5, false)
	if addr.ValidLft == 0 || addr.PreferedLft == 0 {
		t.Error("a finite lease produced no lifetimes")
	}
}

// A router may advertise an infinite valid lifetime (0xFFFFFFFF, RFC 4861 section 4.6.2) beside a finite preferred
// one. netlink sends IFA_CACHEINFO when either lifetime is non-zero, so (0, 1800) would reach the kernel as valid 0
// and fail EINVAL (#819).
func TestV6AddrAttrs_AnInfiniteValidLifetimeIsTranslatedNotSentAsZero(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::5/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr, 0, 1800, false)
	if addr.ValidLft != infiniteLft {
		t.Errorf("ValidLft = %d for an infinite valid lifetime beside a finite preferred "+
			"one, want the kernel's own infinity %d. A zero here is sent as a valid "+
			"lifetime of zero seconds and the kernel refuses the address.",
			addr.ValidLft, infiniteLft)
	}
	if addr.PreferedLft != 1800 {
		t.Errorf("PreferedLft = %d, want the advertised 1800: the translation is of the "+
			"valid half alone", addr.PreferedLft)
	}

	addr2, err := netlink.ParseAddr("2001:db8::6/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr2, 0, 0, false)
	if addr2.ValidLft != 0 || addr2.PreferedLft != 0 {
		t.Errorf("an infinite lease gave ValidLft=%d PreferedLft=%d, want both zero: a "+
			"permanent address is the one shape that carries no lifetimes",
			addr2.ValidLft, addr2.PreferedLft)
	}
}

// Measured 2026-09-16 in unshare -Urn on a dummy link: valid_lft forever with preferred_lft 0 installs the address
// deprecated, the state RFC 4862 section 5.5.4 asks for, so a deprecated permanent address is translated (#819).
func TestV6AddrAttrs_ADeprecatedInfiniteAddressIsNotSentAsPermanent(t *testing.T) {
	addr, err := netlink.ParseAddr("2001:db8::7/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(addr, 0, 0, true)
	if addr.ValidLft != infiniteLft {
		t.Errorf("ValidLft = %d for a deprecated address on a prefix advertised forever, "+
			"want the kernel's own infinity %d. A zero here attaches no IFA_CACHEINFO at "+
			"all and the address goes on the link permanent and preferred",
			addr.ValidLft, infiniteLft)
	}
	if addr.PreferedLft != 0 {
		t.Errorf("PreferedLft = %d, want 0: preferred_lft 0 is what makes the kernel mark "+
			"the address deprecated, and it is the only thing that does",
			addr.PreferedLft)
	}

	keep, err := netlink.ParseAddr("2001:db8::8/128")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
	v6AddrAttrs(keep, 0, 0, false)
	if keep.ValidLft != 0 || keep.PreferedLft != 0 {
		t.Errorf("an address advertised with no deadlines gave ValidLft=%d PreferedLft=%d, "+
			"want both zero: widening the translation must not reach the permanent shape",
			keep.ValidLft, keep.PreferedLft)
	}
}

// RFC 4862 section 5.5.3 e) treats preferred > valid as malformed, and the kernel clamps it, leaving no deprecation
// window (#911).
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
		v6AddrAttrs(addr, info.LeaseSeconds, info.PreferredSeconds, info.IPDeprecated)
		if addr.ValidLft != 0 && addr.PreferedLft > addr.ValidLft {
			t.Errorf("%+v gave PreferedLft=%d above ValidLft=%d",
				info, addr.PreferedLft, addr.ValidLft)
		}
	}
}

// A v4 address with NODAD would skip the RFC 5227 check the library did not run for it, silently (#911).
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

// The rule follows the call graph: a call is allowed under `if v6` or in a function every caller of which is allowed.
// A function value or a closure is not followed (#818, #911).
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

// The `endpoints` array describes the v4 client: RFC 5227 fields are v4-only (#911).
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
