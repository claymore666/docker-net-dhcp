// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// execProgramArg names every call through which this package can hand a
// program name to the kernel, and which argument is that name.
//
// It is a small map, and being small is a limitation rather than a
// design: an unlisted spelling is a process this derivation cannot see.
// Read the "WHAT THIS STILL DOES NOT SEE" section on
// TestDockerfileGuaranteesEveryAbsoluteBinary before trusting it. Short
// version, measured: REWRITING an existing site into an unlisted
// spelling goes red, because the exec-site floor notices the count drop;
// ADDING one does not. If a new way to start a process arrives in this
// package, it belongs in this map in the same change.
var execProgramArg = map[string]int{
	"exec.Command":        0,
	"exec.CommandContext": 1, // arg 0 is the context
	"syscall.Exec":        0,
	"unix.Exec":           0,
	"os.StartProcess":     0,
}

// TestDockerfileGuaranteesEveryAbsoluteBinary keeps two copies of one
// set from drifting: the binaries this package RUNS, and the binaries
// the image GUARANTEES exist.
//
// Each absolute path here exists only because the Dockerfile installs
// it, and the Dockerfile asserts them with `test -x` so an Alpine or
// busybox relocation fails the BUILD instead of failing some
// container's first lease with "not found". A fix that reaches one copy
// and not the other is this repository's most repeated failure.
//
// # WHY THE WANTED SET IS DERIVED AND NOT WRITTEN DOWN
//
// It used to be written down, and this is the whole reason the test was
// rewritten:
//
//	want := map[string]bool{dhcpcdBin: true, mountBin: true, mkdirBin: true}
//
// Three entries, maintained by hand, inside the test whose entire
// purpose is to stop a set from being maintained by hand in two places.
// It was wrong on the day it was written — this package runs six
// binaries, not three — and it cost nothing to be wrong, because the
// three it omitted were precisely the three the Dockerfile omitted too.
// Both copies were missing the same entries, so comparing them agreed.
// That is a mirror: a test that cannot see a defect the two sides
// share. The previous version had already failed once this way (it
// filtered operands to the one containing "dhcpcd", so mountBin and
// mkdirBin were added under a green parity check), which is twice for
// one test.
//
// # WHY USE AND NOT SPELLING
//
// The obvious derivation is "every string constant in this package
// matching ^/". Measured, that returns nine:
//
//	/sbin/dhcpcd  /usr/bin/unshare  /bin/mount  /bin/mkdir
//	/usr/lib/net-dhcp/dhcp-handler
//	/var/lib/dhcpcd  /run/dhcpcd  /proc/sys  /proc
//
// The last four are DIRECTORIES. `test -x /var/lib/dhcpcd` is not a
// thing the Dockerfile should assert and /proc/sys does not exist at
// image build time at all, so a spelling derivation reddens on four
// entries for a reason that is not the defect — and the only remedy it
// leaves is a hand-written exclusion list, which is the hand-written map
// above wearing a different name. It leaks in the other direction too:
// /bin/sh is a binary this package runs and, until this change, matched
// no constant at all. A proxy that leaks in both directions is not a
// proxy for anything.
//
// So the derivation reads USE. A command word is an absolute path that
// this package hands to the kernel, or to a shell, in a position where
// something will try to EXECUTE it. Three sources, and all three are
// needed:
//
//  1. the program argument of an exec call (see execProgramArg);
//  2. any absolute element of a []string literal, because in this
//     package those literals are argv (renderArgs' dhcpcd argv, and the
//     `unshare -m /bin/sh -c …` wrapper);
//  3. the first word of each command in mountPrep()'s shell string,
//     read from the string the function actually returns.
//
// Source 3 is not redundant with 1 and 2, and leaving it out is exactly
// how #707 happened twice. The audit that produced dhcpcdBin looked at
// exec.Command call sites — source 1 alone — so the four commands inside
// the `sh -c` body were invisible to it, because a shell resolves every
// command word through PATH and not only the one that reaches execve.
//
// # THE THREE WAYS THIS GOES RED, AND WHAT EACH MEANS
//
//  1. a derived binary the Dockerfile does not assert — the image does
//     not guarantee something this package runs;
//  2. an asserted path no derivation reached — either a binary lost its
//     constant, or the assertion line is not the chained
//     `test -x a && test -x b` form this parses;
//  3. an exec whose program argument this test CANNOT resolve to a
//     string — a local, a parameter, a function call. This gets its own
//     message rather than being skipped, because a derivation that
//     silently drops what it cannot read is a check that goes quiet: the
//     set shrinks, the comparison still passes, and the number is now
//     trusted precisely because it was derived.
//
// A fourth red is really source 1's own bug: an exec program that
// resolves to a BARE name is #707 itself, and is reported as such.
//
// # WHAT THIS STILL DOES NOT SEE, STATED RATHER THAN IMPLIED
//
// execProgramArg is an enumeration, and an enumeration can be short.
// Measured, by adding each mutant and running the package:
//
//   - a NEW binary at a recognised exec site, in an argv literal, or in
//     mountPrep's shell body — RED, naming the binary. This is the
//     mutant that decides whether the derivation is real or decorative,
//     because a derived set that does not grow when the package grows is
//     a hand-written set with extra steps.
//   - an EXISTING exec site rewritten into a spelling not listed above —
//     RED, via the exec-site floor. The count drops and the floor says
//     so. This is why the floors are not decoration.
//   - a purely ADDITIVE process start, in a spelling not listed above,
//     whose program appears in no argv literal and no shell body —
//     SURVIVES. Nothing here sees it.
//
// That last one is a real hole and it is left open deliberately, because
// the alternatives are worse: enumerating every way Go can start a
// process is unbounded, and deriving from spelling instead of use is the
// approach this test replaced (it returns four directories and misses
// /bin/sh, so it leaks in both directions). The floor is what keeps the
// hole from widening on its own. If a new way to start a process is
// added to this package, add it to execProgramArg in the same change.
//
// # WHAT KEEPS THE DERIVATION FROM QUIETLY FINDING NOTHING
//
// Every rule above is a rule about sites that exist, so all of them are
// satisfied completely by there being none. The floors at the bottom are
// the answer: they fail closed, they can only ever be too low, and their
// job is to notice that this test has stopped finding the code it claims
// to check.
//
// # THE ONE CARVE-IN
//
// DefaultHandler is named below by hand. It is dhcpcd's hook script,
// passed as `-c <handler>` and executed by dhcpcd on every lease event,
// and it reaches the argv through a struct field (client.go assigns it
// to a local, the local into dhcpcdParams.Handler, the field into
// renderArgs' argv). Following that needs dataflow analysis, which is
// more machinery than a test should carry. So it is stated, with its
// reason, as one named inclusion — the opposite of an exclusion list,
// because it can only ever ADD an obligation to the image.
//
// The unresolvable elements of an argv literal are deliberately NOT a
// red, unlike an unresolvable exec program. Argv literals are full of
// them — flag values, config paths, the interface name — so redding
// there would be noise, and noise is how a real red gets discharged by
// habit.
//
// Boundary condition, stated so the next person does not have to
// rediscover it: source 2 assumes an absolute path in a []string literal
// is a program. True in this package today. If a literal of DATA paths
// ever appears, the remedy is a second named carve-out with its reason,
// not a filter.
func TestDockerfileGuaranteesEveryAbsoluteBinary(t *testing.T) {
	want := deriveCommandWords(t)

	// DefaultHandler: see "THE ONE CARVE-IN" above.
	want[DefaultHandler] = "client.go, dhcpcd's -c hook script (named here by hand)"

	const marker = "test -x "

	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}

	// Collect EVERY `test -x` on a line, not just the first, which is
	// what makes the chained `a && test -x b && test -x c` spelling
	// readable back. The one-line `test -x a b c` form is not an option
	// to re-introduce: busybox sh answers it "unknown operand" and exits
	// 2 whatever the files are, and this test would then see one operand
	// against six constants and fail — which is the outcome we want,
	// since that line cannot build. An `-a` chain fails here too, by
	// putting "-a" and "-x" into the operand set.
	got := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for i := strings.Index(trimmed, marker); i >= 0; i = strings.Index(trimmed, marker) {
			rest := strings.TrimSpace(trimmed[i+len(marker):])
			if f := strings.Fields(rest); len(f) > 0 {
				got[f[0]] = true
			}
			trimmed = trimmed[i+len(marker):]
		}
	}

	if len(got) == 0 {
		t.Fatalf("the Dockerfile no longer asserts `test -x <path>` for anything; "+
			"nothing now guarantees %v exist in the image, and this test would "+
			"otherwise pass having compared nothing", sortedKeys(want))
	}

	for p, where := range want {
		if !got[p] {
			t.Errorf("this package runs %q as a command word (%s), but the Dockerfile "+
				"does not `test -x` it. Either the image does not guarantee a binary "+
				"this package executes — add the assertion, so a relocation breaks the "+
				"build instead of breaking a container at run time — or %q is a data "+
				"path that reached an argv, in which case name it here with its reason. "+
				"This derivation has no exclusion list on purpose", p, where, p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("the Dockerfile asserts %q, which nothing in this package runs; "+
				"either a binary lost the constant that named it, or the assertion is "+
				"not the chained `test -x a && test -x b` form this parses (operands "+
				"got: %v; derived: %v)", p, sortedKeys(setOf(got)), sortedKeys(want))
		}
	}
}

// deriveCommandWords returns every absolute path this package hands to
// the kernel or to a shell as a command word, mapped to where it was
// found so a failure names a line rather than only a value. See
// TestDockerfileGuaranteesEveryAbsoluteBinary for what counts and why.
//
// It reports its own failures — an unresolvable exec program, a bare
// exec program, and the floors — because those are defects in the
// derivation's ability to see, and a caller cannot tell an empty result
// from a clean one.
func deriveCommandWords(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}

	// Package-level string constants, so an identifier in an argv can be
	// read back as the path it names.
	consts := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if s, ok := stringValue(vs.Values[i]); ok {
						consts[name.Name] = s
					}
				}
			}
		}
	}

	resolve := func(e ast.Expr) (string, bool) {
		if s, ok := stringValue(e); ok {
			return s, true
		}
		if id, ok := e.(*ast.Ident); ok {
			if s, ok := consts[id.Name]; ok {
				return s, true
			}
		}
		return "", false
	}

	words := map[string]string{}
	execSites, argvLits := 0, 0

	// Sources 1 and 2.
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				idx, ok := execProgramArg[pkg.Name+"."+sel.Sel.Name]
				if !ok || idx >= len(v.Args) {
					return true
				}
				execSites++
				at := fset.Position(v.Args[idx].Pos())
				where := fmt.Sprintf("%s:%d", at.Filename, at.Line)

				s, ok := resolve(v.Args[idx])
				if !ok {
					t.Errorf("%s: this exec's program argument is not a string literal or a "+
						"package constant, so this test cannot tell which binary runs here "+
						"and cannot hold the image to providing it. Name the program at the "+
						"exec site. Silently skipping it would shrink the derived set while "+
						"leaving the comparison green, which is the failure this derivation "+
						"replaced", where)
					return true
				}
				if !strings.HasPrefix(s, "/") {
					t.Errorf("%s: this exec runs %q, a bare name that the kernel resolves "+
						"through PATH — which is #707 exactly. Name it absolutely, as "+
						"dhcpcdBin and unsharePath are", where, s)
					return true
				}
				words[s] = where

			case *ast.CompositeLit:
				// exec.Cmd{Path: "..."} starts a process without ever
				// calling exec.Command, so the call arm above never sees
				// it. Same treatment: it must resolve, and it must be
				// absolute.
				if sel, ok := v.Type.(*ast.SelectorExpr); ok {
					pkg, ok := sel.X.(*ast.Ident)
					if !ok || pkg.Name != "exec" || sel.Sel.Name != "Cmd" {
						return true
					}
					for _, e := range v.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Path" {
							continue
						}
						execSites++
						at := fset.Position(kv.Value.Pos())
						where := fmt.Sprintf("%s:%d", at.Filename, at.Line)
						str, ok := resolve(kv.Value)
						if !ok {
							t.Errorf("%s: this exec.Cmd's Path is not a string literal or a "+
								"package constant, so this test cannot tell which binary runs "+
								"here and cannot hold the image to providing it", where)
							continue
						}
						if !strings.HasPrefix(str, "/") {
							t.Errorf("%s: this exec.Cmd runs %q, a bare name the kernel "+
								"resolves through PATH — #707 exactly. Name it absolutely",
								where, str)
							continue
						}
						words[str] = where
					}
					return true
				}
				at, ok := v.Type.(*ast.ArrayType)
				if !ok || at.Len != nil {
					return true
				}
				el, ok := at.Elt.(*ast.Ident)
				if !ok || el.Name != "string" {
					return true
				}
				pos := fset.Position(v.Lbrace)
				found := false
				for _, e := range v.Elts {
					s, ok := resolve(e)
					if !ok || !strings.HasPrefix(s, "/") {
						continue
					}
					words[s] = fmt.Sprintf("%s:%d, in an argv", pos.Filename, pos.Line)
					found = true
				}
				if found {
					argvLits++
				}
			}
			return true
		})
	}

	// Source 3: the shell body, read from the string mountPrep actually
	// returns rather than from its source.
	//
	// Split on ;, & and | — every character that ENDS a command in sh, so
	// the word after one is a fresh command word. Splitting on ";" alone
	// reads `a && b` as one command and never looks at b, which is the
	// same shape of blindness as looking only at $0: the word that gets
	// checked is the word the splitter happened to produce.
	shellWords := 0
	for _, seg := range strings.FieldsFunc(mountPrep(), func(r rune) bool {
		return r == ';' || r == '&' || r == '|'
	}) {
		fields := strings.Fields(seg)
		// `exec` is a shell builtin: no PATH lookup to pin, and its
		// argument is $0, which is dhcpcdBin and already derived above.
		if len(fields) == 0 || fields[0] == "exec" {
			continue
		}
		shellWords++
		if !strings.HasPrefix(fields[0], "/") {
			// TestMountPrep_NamesEveryBinaryAbsolutely owns this red and
			// says it better, naming the whole string. Do not duplicate
			// it here; just do not derive a PATH-resolved name as though
			// the image could guarantee it.
			continue
		}
		words[fields[0]] = "mountPrep(), a command word in the sh -c body"
	}

	// The floors. Everything above is a rule about sites that exist, and
	// is satisfied completely by there being none.
	if len(files) == 0 {
		t.Fatal("scanned no non-test .go files; every derivation above produced its " +
			"result from an empty package and the comparison would pass vacuously")
	}
	for _, f := range []struct {
		got, min int
		what     string
	}{
		{execSites, 1, "exec call sites"},
		{argvLits, 2, "[]string literals carrying an absolute path"},
		{shellWords, 4, "command words in mountPrep()'s shell body"},
	} {
		if f.got < f.min {
			t.Errorf("found %d %s, want at least %d. Either this package stopped running "+
				"something it used to run — in which case a constant and a Dockerfile "+
				"assertion are now dead — or the derivation has stopped seeing a shape it "+
				"used to see, and is now guaranteeing less than it reports",
				f.got, f.what, f.min)
		}
	}

	return words
}

// stringValue reads an untyped string literal, and only that: a
// concatenation or a conversion is deliberately unresolvable, so it
// reaches the "cannot tell which binary runs here" red rather than being
// half-read.
func stringValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// setOf adapts a set to the map shape sortedKeys reads.
func setOf(m map[string]bool) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = ""
	}
	return out
}

// sortedKeys renders a set in a stable order so a failure message reads
// the same on every run; map iteration order would otherwise make two
// identical failures look like two different ones.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
