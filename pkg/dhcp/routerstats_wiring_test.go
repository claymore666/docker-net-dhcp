// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestOneShotAcquisition_FoldsItsOwnRouterDiscovery closes the one fold
// site no behaviour test in this package can reach.
//
// WHY IT NEEDS A WIRING TEST AT ALL. The other three fold sites are in
// translate(), which a fake runner drives, and each has a test that
// goes red when its line is deleted. acquireOnce6 is not reachable that
// way: it opens a real packet socket on a real interface in a real
// namespace (see the comment on getIP6), so nothing in this package
// executes it. MEASURED: deleting opts.routerReport(stats) from
// acquireOnce6 leaves the whole of ./pkg/... green.
//
// WHY THAT LINE MATTERS MORE THAN THE OTHER THREE, not less. A
// CreateEndpoint one-shot never reaches translate() at all. It runs for
// the whole of RFC 4861 section 6.3.7's discovery window, counts every
// solicitation it sent and every advertisement that came back, and then
// ends. No later client inherits those counters: the persistent client
// Join starts has a manager, and a set of counters, of its own. The
// line's own comment says the numbers are counted "here or nowhere",
// and until this test that claim had no observer.
//
// THE BOUND, which is the same one the acquisition-window wiring test
// states about itself. This is keyed on SPELLING. A fold reached
// through a wrapper, or through a variable holding the stats, reads as
// a violation here though it is correct; a call to something merely
// NAMED routerReport reads as correct though it is not. It is a wiring
// check beside the behaviour tests, not instead of them.
func TestOneShotAcquisition_FoldsItsOwnRouterDiscovery(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "chassis6.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing chassis6.go: %v", err)
	}

	spell := func(n ast.Node) string {
		var b strings.Builder
		if err := printer.Fprint(&b, fset, n); err != nil {
			t.Fatalf("printing %T: %v", n, err)
		}
		return b.String()
	}

	calls := func(n ast.Node, fn string) [][]string {
		var found [][]string
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if name != fn {
				return true
			}
			var args []string
			for _, a := range call.Args {
				args = append(args, spell(a))
			}
			found = append(found, args)
			return true
		})
		return found
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "acquireOnce6" && f.Recv == nil {
			fn = f
		}
	}
	// NON-VACUITY. A renamed or moved acquireOnce6 empties every
	// assertion below, and an empty domain satisfies all of them.
	if fn == nil {
		t.Fatal("no acquireOnce6 in chassis6.go: this test's domain is empty, and an " +
			"empty domain satisfies every rule below without checking anything")
	}

	fold := calls(fn, "routerReport")
	if len(fold) != 1 {
		t.Fatalf("acquireOnce6 folds router discovery %d time(s), want exactly one. A "+
			"CreateEndpoint one-shot never reaches translate(), so its solicitations and "+
			"the advertisements they brought back are counted here or nowhere, and a host "+
			"that only ever creates endpoints then reads zero on a segment with a router "+
			"on it", len(fold))
	}
	if n := len(fold[0]); n != 1 {
		t.Fatalf("acquireOnce6 folds router discovery with %d argument(s), want 1", n)
	}

	// THE SAME SNAPSHOT THE RECORD IS WRITTEN FROM. A second
	// client.Stats() call here would be a second reading of a moving
	// counter, so the live number and the durable one would disagree
	// about the same acquisition by whatever arrived between them.
	count := calls(fn, "count")
	if len(count) != 1 || len(count[0]) != 2 {
		t.Fatalf("acquireOnce6 writes the durable record %d time(s); this test reads the "+
			"fold's argument against that call's", len(count))
	}
	if fold[0][0] != count[0][1] {
		t.Errorf("acquireOnce6 folds router discovery from %q and writes the record from "+
			"%q. Two readings of a counter that is still moving make the live number and "+
			"the durable one disagree about the same acquisition", fold[0][0], count[0][1])
	}

	// A NEW MANAGER'S SNAPSHOT STARTS AT ZERO. getIP6 runs this
	// function twice through one options value, so without this the
	// second pass's gains are subtracted away. See managerStarted.
	if got := calls(fn, "managerStarted"); len(got) != 1 {
		t.Errorf("acquireOnce6 calls managerStarted %d time(s), want exactly one: on "+
			"getIP6's errV6HintInUse retry this function runs again through the SAME "+
			"options value, and a snapshot left over from the first pass takes the whole "+
			"of the second acquisition away", len(got))
	}
}

// TestDocComments_AreAttachedToTheFunctionTheyName closes the defect
// this PR's own fix commit introduced and nothing it ran could see.
//
// WHAT HAPPENED. managerStarted was inserted directly above
// routerReport with no blank line between the new comment block and
// the old one, so the two became ONE block. Go gives a contiguous
// block to the declaration below it, which made routerReport's
// documentation into managerStarted's, opening by describing a
// function it is not about, and left routerReport with no doc comment
// at all. gofmt, go vet, go build and the whole local lane were green
// across it, and the diff could not show it either: the insertion adds
// lines and the damage is to the unchanged lines above them.
//
// THE RULE, and it is narrow on purpose. A doc block whose first word
// names a function that EXISTS IN THIS PACKAGE and is NOT the one the
// block is attached to is a block that has come adrift from its
// declaration. It is keyed on the first word because that is where Go's
// own convention puts the subject.
//
// THE CORPUS IS ZERO AND EARNED, not ratcheted. MEASURED over pkg/dhcp
// at 8aecc4a and at this head: no non-test declaration matches. The
// same measurement over pkg/plugin finds three that do, all of them
// older than this change, so this test is scoped to the package it can
// hold at zero and the others are reported rather than allowlisted
// here. A gate with an allowlist would have admitted the fourth.
func TestDocComments_AreAttachedToTheFunctionTheyName(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	pkg := pkgs["dhcp"]
	// NON-VACUITY. A package that did not parse, or parsed under
	// another name, leaves the loop below with nothing to walk and
	// every rule in it satisfied.
	if pkg == nil || len(pkg.Files) == 0 {
		t.Fatal("no non-test files parsed for package dhcp: this test's domain is empty, " +
			"and an empty domain satisfies its rule without checking anything")
	}

	declared := map[string]bool{}
	var funcs []*ast.FuncDecl
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			declared[fn.Name.Name] = true
			funcs = append(funcs, fn)
		}
	}
	if len(funcs) < 2 {
		t.Fatalf("package dhcp has %d function declarations; this test cannot tell a "+
			"detached block from an attached one below two", len(funcs))
	}

	firstWord := func(doc *ast.CommentGroup) string {
		if doc == nil {
			return ""
		}
		fields := strings.Fields(strings.TrimPrefix(doc.List[0].Text, "//"))
		if len(fields) == 0 {
			return ""
		}
		return strings.TrimRight(fields[0], ".,:;")
	}

	for _, fn := range funcs {
		w := firstWord(fn.Doc)
		if w == "" || w == fn.Name.Name || !declared[w] {
			continue
		}
		t.Errorf("%s: the doc comment on %s opens by naming %s, which is a different "+
			"function in this package. A comment block runs to the declaration below it, so a "+
			"block inserted under another function's documentation with no blank line between "+
			"them takes that documentation over and leaves the function it belonged to with "+
			"none. gofmt and go vet do not model this",
			fset.Position(fn.Pos()), fn.Name.Name, w)
	}
}
