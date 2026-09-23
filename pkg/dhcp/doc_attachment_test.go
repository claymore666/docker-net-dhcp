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

// A Go comment block with no blank line above a new function becomes that function's doc, and gofmt, vet and build all
// pass it; #814 moved routerReport's doc onto managerStarted this way.
// go/parser.ParseDir is deprecated since Go 1.25 for ignoring build tags, so the files come from `go list`,
// IgnoredGoFiles included (#814).
// Bound: only blocks whose first word names a function are judged.

func TestEveryDocCommentIsAttachedToWhatItNames(t *testing.T) {
	// A test runs in its own package directory, so "./..." would judge pkg/dhcp alone (#814).
	const pattern = "github.com/claymore666/docker-net-dhcp/v2/..."
	cmd := exec.Command("go", "list", "-f",
		"{{.Dir}}\t{{join .GoFiles \" \"}}\t{{join .IgnoredGoFiles \" \"}}", pattern)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pattern, err, stderr.String())
	}

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

	if len(dirs) < 5 {
		t.Fatalf("go list reported %d package director(ies) with source under %s; the "+
			"module is not being read and a pass here would mean nothing", len(dirs), pattern)
	}

	var judged, parsed int
	for _, dir := range dirs {
		fset := token.NewFileSet()
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

	// Measured: dropping IgnoredGoFiles removed 30 files of test/integration/harness, one of them carrying a detachment
	// (#814).
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
