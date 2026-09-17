// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryDocCommentIsAttachedToWhatItNames closes a defect class that
// every other instrument in this repo is blind to.
//
// WHAT IT IS. A Go comment block runs to the declaration under it. Put
// a new function directly beneath an existing doc block with no blank
// line between the two comments and they become ONE block, which Go
// then gives to the new declaration: the old function loses its
// documentation and the new one is documented by a description of
// something else. Nothing catches it. gofmt, go vet, go build,
// staticcheck and the whole local lane are green across it, and a diff
// cannot show it either, because the insertion adds lines and the
// damage is to the unchanged lines above them.
//
// HOW IT WAS FOUND. #814 did it: managerStarted was inserted above
// routerReport and took routerReport's godoc with it. It was found by
// a reviewer reading, which is not a check, so this is the check.
//
// THE POPULATION IS DERIVED FROM THE BUILD, on no_exec_test.go's
// precedent, and it is DELIBERATELY WIDER than that test's in two ways.
// That one judges what is linked into the shipped binary, because its
// property is about what the binary can do. This property is about
// source a reader trusts, so the population here is every non-test file
// of every package in THIS module, and it includes the files a build
// tag excludes. GoFiles ALONE WOULD NOT DO: MEASURED,
// test/integration/harness reports 14 GoFiles and 30 IgnoredGoFiles,
// the second set being everything behind `//go:build integration`, and
// one of the four detachments this check was written for was in that
// set. A rule that reads only what the default build sees is satisfied
// by a build tag. The pinned library is excluded: it is a separate
// repository and a finding in it cannot be acted on here.
//
// NOT go/parser.ParseDir, which is the obvious way to write this and is
// deprecated since Go 1.25 for not considering build tags. The
// replacement asks `go list` for the file names and parses each, which
// answers the deprecation and gets the tagged files back by naming them
// explicitly rather than by ignoring the question.
//
// THE RULE. A doc block whose first word names a function declared in
// the same package, and which is attached to a DIFFERENT declaration,
// is a block that has come adrift. It is keyed on the first word
// because that is where Go's own convention puts the subject.
//
// TWO BOUNDS, stated rather than discovered later. It sees only blocks
// whose first word is a FUNCTION name, so a block opening with a type
// or a constant name is outside it. And a block adrift onto a function
// whose name is not in the text at all is invisible to it. Both are the
// price of a rule with no allowlist; what it does see, it sees
// everywhere, and the corpus below is zero and earned rather than
// ratcheted. MEASURED at 8aecc4a: four instances, three in pkg/plugin
// and one in test/integration/harness, all older than #814 and all
// repaired in the same commit as this test.
func TestEveryDocCommentIsAttachedToWhatItNames(t *testing.T) {
	// The FULL import path pattern, not "./...": a test runs with its
	// own package directory as the working directory, so the relative
	// form would resolve under pkg/dhcp and judge one package.
	//
	// IgnoredGoFiles beside GoFiles is the whole reason this is a
	// `go list` call and not a directory walk. See the header.
	const pattern = "github.com/claymore666/docker-net-dhcp/v2/..."
	cmd := exec.Command("go", "list", "-f",
		"{{.Dir}}\t{{join .GoFiles \" \"}}\t{{join .IgnoredGoFiles \" \"}}", pattern)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pattern, err, stderr.String())
	}

	// dir -> the non-test source files in it, and separately how many
	// of them the default build does NOT see. The second number is a
	// population in its own right and is asserted below.
	files := map[string][]string{}
	tagged := 0
	for _, line := range strings.Split(string(out), "\n") {
		dir, rest, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(dir) == "" {
			continue
		}
		built, ignored, _ := strings.Cut(rest, "\t")
		for i, names := range []string{built, ignored} {
			for _, n := range strings.Fields(names) {
				if strings.HasSuffix(n, "_test.go") {
					continue
				}
				if i == 1 {
					tagged++
				}
				files[dir] = append(files[dir], filepath.Join(dir, n))
			}
		}
	}
	var dirs []string
	for d := range files {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	// BOTH POPULATIONS ARE ASSERTED, and the second is the one that
	// matters. A go list that returned nothing, a renamed module path
	// or a file set that came back empty each leave the loop below with
	// nothing to judge and the rule trivially satisfied.
	if len(dirs) < 5 {
		t.Fatalf("go list reported %d package director(ies) with source under %s; the "+
			"module is not being read and a pass here would mean nothing", len(dirs), pattern)
	}

	var judged, parsed int
	for _, dir := range dirs {
		fset := token.NewFileSet()
		// Files of one directory can declare more than one package
		// name, so the declared set is per directory, which is the
		// scope a doc comment's subject is resolved in anyway.
		declared := map[string]bool{}
		var funcs []*ast.FuncDecl
		var positions []*token.FileSet
		for _, path := range files[dir] {
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			parsed++
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				declared[fn.Name.Name] = true
				funcs = append(funcs, fn)
				positions = append(positions, fset)
			}
		}
		for i, fn := range funcs {
			if fn.Doc == nil {
				continue
			}
			fields := strings.Fields(strings.TrimPrefix(fn.Doc.List[0].Text, "//"))
			if len(fields) == 0 {
				continue
			}
			judged++
			w := strings.TrimRight(fields[0], ".,:;")
			if w == fn.Name.Name || !declared[w] {
				continue
			}
			t.Errorf("%s: the doc comment on %s opens by naming %s, which is a "+
				"different function declared in that package. A comment block runs "+
				"to the declaration below it, so a block written under another "+
				"function's documentation with no blank line between them takes that "+
				"documentation over and leaves the function it belonged to with none. "+
				"gofmt, go vet and staticcheck do not model this, and a diff cannot "+
				"show it because the damage is to the lines above the insertion",
				positions[i].Position(fn.Pos()), fn.Name.Name, w)
		}
	}

	// THE POPULATION THAT DISAPPEARS QUIETLY. MEASURED: dropping
	// IgnoredGoFiles from the query above removes 30 files of
	// test/integration/harness, including the one that carried one of
	// the four detachments this check was written for, and the file
	// count stays comfortably over any round floor -- so a floor alone
	// does NOT catch it and this test went on passing while its domain
	// had been cut. A universal rule is satisfied by emptying its
	// domain, so the thing asserted here is the property and not a
	// number: source the default build excludes is IN.
	if tagged == 0 {
		t.Fatalf("no build-tag-excluded file was parsed. `go list` reports them in "+
			"IgnoredGoFiles and they are source a reader trusts exactly as much as the "+
			"rest; one of the detachments this check exists for was behind "+
			"`//go:build integration`. %d file(s) were parsed in total, which is why a "+
			"floor on that number cannot see this", parsed)
	}

	if parsed < 50 {
		t.Fatalf("only %d non-test source file(s) were parsed across %d package "+
			"director(ies); the file set is not reaching the module and a pass would be "+
			"over an almost empty population", parsed, len(dirs))
	}
	if judged < 100 {
		t.Fatalf("only %d documented function(s) were judged across %d package "+
			"director(ies); the walk is not reaching the source and a pass would be over "+
			"an almost empty population", judged, len(dirs))
	}
}
