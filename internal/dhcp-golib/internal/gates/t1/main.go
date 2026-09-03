// Command t1 is the T1 gate: ring 1 imports nothing that does I/O.
//
// T1 is a load-bearing guarantee, not hygiene. Ring 1 being pure is what makes
// the test suite instant and offline replay bit-exact; the day ring 1 can read
// a clock or open a socket, both properties are gone and no test will say so.
//
// The gate applies four rules:
//
//	A  Transitive, build-aware. Every dependency of a pure-ring package that
//	   lives inside this module must itself be in a pure ring. Blind to a file
//	   excluded by a build constraint: MEASURED 2026-08-29, a proto/ file
//	   carrying //go:build never_built and importing "net" produced a
//	   go list -deps closure that did not contain net at all.
//
//	B  Direct, build-tag-blind. Every import in every .go file under a pure
//	   ring root — including files the go tool would not compile today — must
//	   be module-internal-and-pure, or standard-library-and-on the allowlist.
//	   B is what caught the file A could not see, in the same measurement.
//
//	A is NOT an independent second instrument today, and should not be
//	described as one. Because B scans every file under every pure root, and a
//	pure ring may currently depend on nothing but another pure root, A's
//	module-internal findings are a subset of B's. A becomes load-bearing the
//	day a pure ring is allowed a dependency B does not scan — a third-party
//	package, or a module-internal package outside the pure roots — because B
//	sees only the import line and A sees what that import drags in. It is kept
//	for that, and because a disagreement between the go tool's view and the
//	file walk is itself worth surfacing.
//
//	C  Identifier-level. Within pure-ring files, only allowlisted identifiers
//	   of a restricted package may be named. This exists because "fmt" is on
//	   the import allowlist and fmt can write to stdout.
//
//	D  Non-vacuity. Every declared ring root must exist and hold at least one
//	   non-test .go file, and the go tool must return at least one package for
//	   the pure roots. A universal gate is satisfied by emptying its domain,
//	   so an empty domain is a refusal and not a pass.
//
// Exit status: 0 PASS (the normal state), 1 VIOLATION, 2 REFUSED — the gate
// could not measure its own domain and is reporting that rather than passing.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	root := flag.String("root", ".", "module root to check")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		refuse("cannot resolve -root %q: %v", *root, err)
	}
	os.Exit(run(abs))
}

func refuse(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "T1 REFUSED: "+format+"\n", args...)
	os.Exit(exitRefuse)
}

type violation struct {
	rule string
	pos  string
	msg  string
}

func run(root string) int {
	if err := checkModulePath(root); err != nil {
		refuse("%v", err)
	}

	pure := rings.Pure()
	pureSet := map[string]bool{}
	for _, r := range pure {
		pureSet[r] = true
	}

	// Rule D — non-vacuity, before anything else. Every declared root, not
	// only the pure ones: a missing ring 3 means the layout this gate encodes
	// is no longer the layout of the tree, and a gate that cannot locate its
	// subject must refuse rather than report on what is left.
	all := append(append([]string{}, pure...), rings.Impure()...)
	for _, r := range all {
		dir := filepath.Join(root, r)
		if _, err := os.Stat(dir); err != nil {
			refuse("ring root %q does not exist; the domain is empty: %s", r, scan.RelErr(root, err))
		}
		files, err := scan.GoFiles(dir)
		if err != nil {
			refuse("ring root %q is not readable, so the domain is empty: %s", r, scan.RelErr(root, err))
		}
		n := 0
		for _, f := range files {
			if !strings.HasSuffix(f, "_test.go") {
				n++
			}
		}
		if n == 0 {
			refuse("ring root %q holds no non-test .go file; the domain is empty", r)
		}
	}

	var vs []violation

	// Rules B and C — per-file, build-tag-blind.
	for _, r := range pure {
		files, err := scan.GoFiles(filepath.Join(root, r))
		if err != nil {
			refuse("ring root %q is not readable: %s", r, scan.RelErr(root, err))
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				// T1 is a claim about the package, not about its tests. A
				// ring-1 test that reads a golden file does not make the
				// state machine impure. T2 governs test files.
				continue
			}
			f, err := scan.Parse(root, path)
			if err != nil {
				refuse("%v", err)
			}
			restricted := map[string]bool{}
			for _, imp := range f.Imports() {
				if imp.Dot {
					vs = append(vs, violation{
						rule: "B", pos: imp.Pos.String(),
						msg: fmt.Sprintf("dot import of %q: identifiers cannot be attributed, so purity cannot be judged", imp.Path),
					})
					continue
				}
				if rel, ok := insideModule(imp.Path); ok {
					top := rel
					if i := strings.Index(top, "/"); i >= 0 {
						top = top[:i]
					}
					if !pureSet[top] {
						vs = append(vs, violation{
							rule: "B", pos: imp.Pos.String(),
							msg: fmt.Sprintf("imports %q, which is outside the pure rings %v", imp.Path, pure),
						})
					}
					continue
				}
				if !scan.IsStdlib(imp.Path) {
					vs = append(vs, violation{
						rule: "B", pos: imp.Pos.String(),
						msg: fmt.Sprintf("imports third-party package %q; a pure ring has no third-party dependency allowlist", imp.Path),
					})
					continue
				}
				if !rings.PureStdlib[imp.Path] {
					vs = append(vs, violation{
						rule: "B", pos: imp.Pos.String(),
						msg: fmt.Sprintf("imports %q, which is not on the pure-stdlib allowlist", imp.Path),
					})
					continue
				}
				if imp.Blank {
					continue // binds no identifier, so rule C has nothing to judge
				}
				if _, ok := rings.PureIdents[imp.Path]; ok {
					restricted[imp.Local] = true
				}
			}
			if len(restricted) == 0 {
				continue
			}
			// Map each local name back to the allowlist it is bound to.
			byLocal := map[string]map[string]bool{}
			for _, imp := range f.Imports() {
				if allowed, ok := rings.PureIdents[imp.Path]; ok && restricted[imp.Local] {
					byLocal[imp.Local] = allowed
				}
			}
			for _, sel := range f.Selectors(restricted) {
				allowed := byLocal[sel.Local]
				if !allowed[sel.Name] {
					vs = append(vs, violation{
						rule: "C", pos: sel.Pos.String(),
						msg: fmt.Sprintf("names %s.%s, which is not on the allowlist for that package in a pure ring", sel.Local, sel.Name),
					})
				}
			}
		}
	}

	// Rule A — transitive, build-aware.
	deps, err := goListDeps(root, pure)
	if err != nil {
		// A cannot resolve the package graph. When rules B and C already
		// found something, that is conclusive and gets reported rather than
		// swallowed by a refusal — with the unrun rule named, so nobody reads
		// the finding list as complete. When they found nothing, an
		// unmeasurable rule A is a refusal: this gate does not pass on the
		// strength of a check that did not run.
		if len(vs) == 0 {
			refuse("%v", err)
		}
		fmt.Fprintf(os.Stderr, "T1 NOTE: rule A did not run, so the finding list below is not complete: %v\n", err)
		deps = nil
	}
	if err == nil && len(deps) == 0 {
		refuse("go list returned no packages for the pure rings; the domain is empty")
	}
	for _, dep := range deps {
		rel, ok := insideModule(dep)
		if !ok {
			continue // stdlib and third-party are rule B's job
		}
		top := rel
		if i := strings.Index(top, "/"); i >= 0 {
			top = top[:i]
		}
		if !pureSet[top] {
			vs = append(vs, violation{
				rule: "A",
				pos:  dep,
				msg: fmt.Sprintf("pure ring depends on %q, which is outside the pure rings %v",
					dep, pure),
			})
		}
	}

	if len(vs) > 0 {
		fmt.Fprintf(os.Stderr, "T1 VIOLATION: ring 1 must import nothing that does I/O (%d finding(s))\n", len(vs))
		for _, v := range vs {
			fmt.Fprintf(os.Stderr, "  [rule %s] %s: %s\n", v.rule, v.pos, v.msg)
		}
		return exitViolate
	}
	fmt.Printf("T1 PASS: pure rings %v import nothing outside the allowlist (%d transitive dep(s) checked)\n", pure, len(deps))
	return exitPass
}

// checkModulePath refuses when go.mod does not declare the module path this
// gate hardcodes. Without it, a rename would silently reclassify every
// internal import as third-party, and the gate would keep reporting.
func checkModulePath(root string) error {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return fmt.Errorf("cannot read go.mod: %s", scan.RelErr(root, err))
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		got := strings.TrimSpace(strings.TrimPrefix(line, "module "))
		if got != rings.Module {
			return fmt.Errorf("go.mod declares module %q but the gate is written for %q; "+
				"update internal/gates/rings.Module deliberately", got, rings.Module)
		}
		return nil
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading go.mod: %w", err)
	}
	return fmt.Errorf("go.mod declares no module path")
}

func insideModule(path string) (string, bool) {
	if path == rings.Module {
		return "", true
	}
	prefix := rings.Module + "/"
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix), true
	}
	return "", false
}

func goListDeps(root string, roots []string) ([]string, error) {
	args := []string{"list", "-deps"}
	for _, r := range roots {
		args = append(args, "./"+r+"/...")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("go %s failed: %v: %s", strings.Join(args, " "), err, stderr)
	}
	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			deps = append(deps, line)
		}
	}
	return deps, nil
}

func asExitError(err error, out **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*out = ee
		return true
	}
	return false
}
