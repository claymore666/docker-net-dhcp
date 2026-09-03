// Command t2 is the T2 gate: no test in this library waits on wall-clock time.
//
// T2 is load-bearing rather than cosmetic, and §5.1 of the build plan is why:
// this library has no CI and never will. The only thing that runs the suite is
// a person running it, constantly. A suite you avoid running because it takes
// a minute is a suite that is not run, and then nothing observes anything.
//
// The other half of the reason is older. A test that waits gets "fixed" by
// waiting longer, and a longer timeout is how a real user-facing failure hid
// in the v1.x plugin for months behind an honest comment.
//
// The gate applies two rules:
//
//	A  Identifier-level, allowlist. In every _test.go file, a reference to a
//	   restricted package (see rings.TestIdents) must name an identifier on
//	   that package's allowlist. time.Sleep, time.After, time.Tick,
//	   time.NewTimer, time.NewTicker and time.AfterFunc are absent from it,
//	   and so is anything the standard library adds later — the allowlist
//	   refuses by default rather than admitting what nobody thought to ban.
//	   Import aliases are resolved from the file's own import declarations, so
//	   `import t "time"` then `t.Sleep` is caught; a text search for
//	   "time.Sleep" is not.
//
//	B  Non-vacuity. The tree must contain at least one _test.go file, or the
//	   gate refuses. A universal gate is satisfied by emptying its domain, so
//	   an empty domain is a refusal and never a pass. There is no opt-out
//	   flag: one existed at M0, for the case where the library had no tests at
//	   all, and it was removed once the gates' own self-tests made the domain
//	   genuinely non-empty. An unused escape hatch is what a later session
//	   reaches for when the gate is inconvenient.
//
// The wall-clock ceiling that T2 also calls for is enforced by verify.sh
// around the suite, not here: it is a different instrument measuring a
// different thing, and this gate cannot see time spent.
//
// Exit status: 0 PASS (the normal state), 1 VIOLATION, 2 REFUSED.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/claymore666/dhcp-golib/internal/gates/rings"
	"github.com/claymore666/dhcp-golib/internal/gates/scan"
)

const (
	exitPass    = 0
	exitViolate = 1
	exitRefuse  = 2
)

func main() {
	root := flag.String("root", ".", "tree to check")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		refuse("cannot resolve -root %q: %v", *root, err)
	}
	os.Exit(run(abs))
}

func refuse(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "T2 REFUSED: "+format+"\n", args...)
	os.Exit(exitRefuse)
}

type violation struct {
	pos string
	msg string
}

func run(root string) int {
	files, err := scan.GoFiles(root)
	if err != nil {
		refuse("the tree is not readable: %s", scan.RelErr(root, err))
	}

	var tests []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			tests = append(tests, f)
		}
	}
	if len(tests) == 0 {
		refuse("no _test.go file found; the domain is empty")
	}

	var vs []violation
	for _, path := range tests {
		f, err := scan.Parse(root, path)
		if err != nil {
			refuse("%v", err)
		}
		// local identifier -> allowlist it is bound to
		byLocal := map[string]map[string]bool{}
		locals := map[string]bool{}
		for _, imp := range f.Imports() {
			allowed, restricted := rings.TestIdents[imp.Path]
			if !restricted {
				continue
			}
			if imp.Dot {
				vs = append(vs, violation{
					pos: imp.Pos.String(),
					msg: fmt.Sprintf("dot import of restricted package %q: identifiers cannot be attributed to it, so the gate cannot judge them", imp.Path),
				})
				continue
			}
			if imp.Blank {
				continue // binds no identifier
			}
			byLocal[imp.Local] = allowed
			locals[imp.Local] = true
		}
		if len(locals) == 0 {
			continue
		}
		for _, sel := range f.Selectors(locals) {
			if !byLocal[sel.Local][sel.Name] {
				vs = append(vs, violation{
					pos: sel.Pos.String(),
					msg: fmt.Sprintf("test names %s.%s, which is not on the allowlist for that package in a test", sel.Local, sel.Name),
				})
			}
		}
	}

	if len(vs) > 0 {
		sort.Slice(vs, func(i, j int) bool { return vs[i].pos < vs[j].pos })
		fmt.Fprintf(os.Stderr, "T2 VIOLATION: no test may wait on wall-clock time (%d finding(s))\n", len(vs))
		for _, v := range vs {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", v.pos, v.msg)
		}
		return exitViolate
	}
	fmt.Printf("T2 PASS: %d test file(s) checked, none waits on the clock\n", len(tests))
	return exitPass
}
