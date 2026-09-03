// Command testroster prints every test function DECLARED in the tree, one per
// line, so that `verify.sh` can compare the declarations against what
// `go test -list` actually reports.
//
// DECISION 2026-08-30: it walks the filesystem and parses, rather than asking
// the go tool. Both halves matter.
//
// Walking the filesystem is what makes it an independent witness: `go list`
// and `go test` honour build constraints, so a test file carrying a false
// //go:build tag is invisible to them. That is precisely the shape this exists
// to catch — MEASURED at 86cb3c5, ten of twenty-two test files could be
// disabled that way, taking the suite from 161 declared tests to 61, with
// every row of the arbiter still green.
//
// Parsing rather than grepping is what makes it correct: internal/gates/t2
// embeds whole test bodies inside Go raw string literals on purpose, so a grep
// reports two declarations that do not exist. An AST walk does not mistake a
// string literal for a declaration.
package main

import (
	"fmt"
	"go/ast"
	"os"
	"sort"
	"strings"

	"github.com/claymore666/dhcp-golib/internal/gates/scan"
)

// testPrefixes are the four prefixes `go test` recognises.
var testPrefixes = []string{"Test", "Benchmark", "Fuzz", "Example"}

// isTestFuncName reports whether name is one `go test -list` would report.
//
// The rule is Go's own: a prefix, then either nothing or a rune that is not a
// lower-case letter. A prefix followed by a lower-case letter is an ordinary
// function; a prefix followed by an underscore, a capital, a digit or nothing
// at all is a test.
//
// TestMain is excluded because `go test -list` never reports it — it is the
// suite's entry point, not a case. Excluding it here rather than filtering the
// comparison later keeps the two sides of that comparison built by one rule.
func isTestFuncName(name string) bool {
	if name == "TestMain" {
		return false
	}
	for _, pre := range testPrefixes {
		if !strings.HasPrefix(name, pre) {
			continue
		}
		rest := name[len(pre):]
		return rest == "" || rest[0] < 'a' || rest[0] > 'z'
	}
	return false
}

// declaredTests returns the sorted, de-duplicated names of the test functions
// declared under root.
//
// Methods are skipped: a method named Test… on some type is not a test, and
// including it would put a name in the population that `go test -list` can
// never report, which fails the comparison for a reason that is not true.
func declaredTests(root string) ([]string, error) {
	files, err := scan.GoFiles(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, path := range files {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := scan.Parse(root, path)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Syntax.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if isTestFuncName(fd.Name.Name) {
				seen[fd.Name.Name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	names, err := declaredTests(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testroster: %v\n", err)
		os.Exit(2)
	}
	// An empty roster is reported as an EXIT STATUS, not as an empty list.
	// The caller's comparison is "every declared name is listed", which is
	// vacuously true over an empty population — the exact shape this tool
	// exists to close, one level down.
	if len(names) == 0 {
		fmt.Fprintf(os.Stderr, "testroster: no test function declared anywhere under %s; the domain is empty\n", root)
		os.Exit(2)
	}
	for _, n := range names {
		fmt.Println(n)
	}
}
