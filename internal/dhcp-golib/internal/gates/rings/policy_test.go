package rings

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The policy tables in rings.go are DATA that the gates read, and until these
// tests existed nothing checked them. MEASURED 2026-08-29: 21 one-line
// allowlist widenings driven through the whole suite, 12 SURVIVED — admitting
// syscall, context, os/exec and net/http into ring 1; admitting fmt.Printf,
// Fprintf, Fprintln and Fscan into ring 1; admitting time.Tick and
// time.NewTimer into tests; admitting context.WithDeadline and
// context.AfterFunc into tests.
//
// The tests below come in two kinds and the distinction is the point:
//
//   - DERIVED. The property is computed from the toolchain — the dependency
//     closure, or the package's real declared signatures — so it holds over
//     the WHOLE package surface, including identifiers nobody has heard of and
//     ones Go has not shipped yet.
//   - ENUMERATED. A hand-written refusal list. Bounded by construction, and
//     used only where no derivation exists.
//
// A count of things this cannot see is at the bottom of the file.

// impureRoots are the packages that actually perform I/O or read ambient
// state. Everything impure in the standard library reaches one of them, which
// is what makes reachability a usable derivation rather than a name check.
var impureRoots = []string{
	"syscall", "os", "net", "time", "context",
	"internal/poll", "runtime/cgo", "os/signal", "os/exec",
}

// TestPureStdlibClosureIsClean is the DERIVED check, and the one that covers
// packages nobody listed.
//
// Rule: a package on PureStdlib whose transitive dependency closure reaches an
// impure root must carry a PureIdents restriction. A clean closure needs none.
//
// This is what found encoding/hex. It was on the allowlist unrestricted, its
// closure reaches os and syscall, and hex.Dumper takes an io.Writer — so ring 1
// could have held a stream through a package that looks like a codec. No
// enumeration of "the five names the requirement lists" would ever have
// returned it.
func TestPureStdlibClosureIsClean(t *testing.T) {
	pkgs := sortedKeys(PureStdlib)
	if len(pkgs) == 0 {
		t.Fatal("PureStdlib is empty; this check would pass having judged nothing")
	}
	for _, pkg := range pkgs {
		deps, err := goListDeps(pkg)
		if err != nil {
			t.Fatalf("cannot resolve the closure of %q, so the policy is unmeasurable: %v", pkg, err)
		}
		if len(deps) == 0 {
			t.Fatalf("go list returned no dependencies for %q; refusing rather than passing", pkg)
		}
		var reached []string
		for _, root := range impureRoots {
			if deps[root] && root != pkg {
				reached = append(reached, root)
			}
		}
		_, restricted := PureIdents[pkg]
		switch {
		case len(reached) == 0 && restricted:
			t.Errorf("%q has a clean closure but carries a PureIdents restriction. "+
				"Either the restriction is unnecessary or the closure moved; decide which.", pkg)
		case len(reached) > 0 && !restricted:
			t.Errorf("%q is admitted to ring 1 unrestricted, but its closure reaches %v. "+
				"Give it a PureIdents entry naming the identifiers that do not touch a "+
				"stream, or take it off PureStdlib.", pkg, reached)
		}
	}
}

// TestPureRefusedPkgsAreAbsent is the ENUMERATED companion. It exists because
// a package can have a clean closure and still be wrong for ring 1 — math/rand
// carries a global source, reflect defeats the purity argument entirely — and
// reachability cannot see that.
func TestPureRefusedPkgsAreAbsent(t *testing.T) {
	if len(PureRefusedPkgs) == 0 {
		t.Fatal("PureRefusedPkgs is empty; this check would pass having judged nothing")
	}
	for _, pkg := range PureRefusedPkgs {
		if PureStdlib[pkg] {
			t.Errorf("%q is on PureStdlib and on PureRefusedPkgs. Ring 1 must not import it.", pkg)
		}
	}
}

// TestRefusedIdentsAreNotAllowed keeps the two kinds of table disjoint. It is
// cheap and it is not the real guard: membership in a map proves nothing about
// what the gate does, so TestPureRefusedIdentsAreRefusedByTheGate and
// TestTestRefusedIdentsAreRefusedByTheGate drive them through the built gates.
func TestRefusedIdentsAreNotAllowed(t *testing.T) {
	check := func(kind string, refused map[string][]string, allowed map[string]map[string]bool) {
		if len(refused) == 0 {
			t.Fatalf("%s refusal table is empty; this check would pass having judged nothing", kind)
		}
		for pkg, idents := range refused {
			for _, id := range idents {
				if allowed[pkg][id] {
					t.Errorf("%s: %s.%s is on both the allowlist and the refusal list", kind, pkg, id)
				}
			}
		}
	}
	check("pure", PureRefusedIdents, PureIdents)
	check("test", TestRefusedIdents, TestIdents)
}

// TestAllowlistedIdentifiersExist catches the drift an allowlist rots by: a
// typo, or a name the standard library removed. An allowlist entry naming
// nothing is dead weight that reads as a considered decision.
func TestAllowlistedIdentifiersExist(t *testing.T) {
	n := 0
	for _, tbl := range []map[string]map[string]bool{PureIdents, TestIdents} {
		for _, pkg := range sortedKeys(tbl) {
			for _, id := range sortedKeys(tbl[pkg]) {
				n++
				if ok, why := declared(pkg, id); !ok {
					t.Errorf("%s.%s is allowlisted but %q declares no such exported identifier (%s)", pkg, id, pkg, why)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no allowlisted identifiers were probed; this check would pass having judged nothing")
	}
	t.Logf("probed %d allowlisted identifiers", n)
}

// TestAllowlistsExcludeStreamAPIs is DERIVED from the real declared
// signatures: an identifier taking or returning an io.Writer or io.Reader
// hands a pure ring a stream, whatever it is called. It covers the whole
// package surface, so a function added to fmt in a future Go release is
// covered without anybody editing this file.
//
// MEASURED 2026-08-29 by review, and this is why the hits guard below exists:
// with only the len(sigs) guard, neutering this regexp turned the whole lane
// green over a LIVE ring-1 purity violation. fmt.Fprintf admitted, its
// enumerated refusal removed, the pattern neutered, and a ring-1 file calling
// fmt.Fprintf into a bytes.Buffer: `./verify.sh` printed VERDICT: PASS (9
// steps) and exited 0. Ring 1 held a writer and every instrument in the
// repository reported the tree clean.
//
// The trigger needs no edit to this file at all. `go doc` changing how it
// renders a signature on a toolchain upgrade does it, which is exactly what
// the two sibling derivations below already guarded against and this one did
// not. A derivation that cannot say when it has stopped working reports
// success and silence identically.
func TestAllowlistsExcludeStreamAPIs(t *testing.T) {
	stream := regexp.MustCompile(`\bio\.(Writer|Reader|WriteCloser|ReadCloser|ReadWriter)\b`)
	if len(PureIdents) == 0 {
		t.Fatal("PureIdents is empty; this check would pass having judged nothing")
	}
	for _, pkg := range sortedKeys(PureIdents) {
		allowed := PureIdents[pkg]
		sigs, err := goDocSignatures(pkg)
		if err != nil {
			t.Fatalf("cannot read the signatures of %q: %v", pkg, err)
		}
		if len(sigs) == 0 {
			t.Fatalf("no signatures parsed for %q; refusing rather than passing", pkg)
		}
		matched := map[string]bool{}
		for name, sig := range sigs {
			if !stream.MatchString(sig) {
				continue
			}
			matched[name] = true
			if allowed[name] {
				t.Errorf("%s.%s is allowlisted for ring 1 but its signature carries a stream: %s", pkg, name, sig)
			}
		}

		// Per PACKAGE, not once for the loop: a pattern that still matches in
		// fmt while having stopped matching in encoding/hex would satisfy any
		// single counter, and hex is the package this whole derivation found.
		if len(matched) == 0 {
			t.Fatalf("the stream pattern matched nothing in package %q; the derivation has stopped "+
				"working and would now pass over any identifier handing ring 1 a writer. "+
				"%d signatures were read, so this is the pattern, not the toolchain.", pkg, len(sigs))
		}

		// The witnesses are the positive control, and they are why this is not
		// merely a counter.
		//
		// MEASURED 2026-08-29: a bare `hits == 0` guard SURVIVED a mutant that
		// moved the increment out of the match branch — every signature then
		// counted as a hit, the guard could never fire, and the derivation was
		// silently disabled again. A count cannot distinguish "matched the
		// right things" from "matched everything"; naming what MUST match can.
		//
		// These are not policy. They are known stream APIs of the package,
		// chosen so the assertion fails loudly if Go ever removes one, and
		// they are checked against the pattern rather than against the
		// allowlist.
		for _, w := range streamWitnesses[pkg] {
			sig, ok := sigs[w]
			if !ok {
				t.Fatalf("%s.%s is a stream witness but `go doc -all %s` no longer declares it. "+
					"Either the standard library removed it or the signature reader broke; "+
					"either way the derivation is unverified.", pkg, w, pkg)
			}
			// Against the PATTERN, not against the matched set built above.
			// MEASURED 2026-08-29: a mutant that recorded every signature as a
			// match defeated both the emptiness guard AND a witness check
			// written against that set — everything looked matched, including
			// the witnesses. A positive control has to test the instrument,
			// not the instrument's bookkeeping.
			if !stream.MatchString(sig) {
				t.Errorf("the stream pattern does not match %s.%s (%q), which takes or returns a "+
					"stream. The derivation is no longer detecting what it was written to detect, "+
					"so its silence about the rest of %q means nothing.", pkg, w, sig, pkg)
			}
		}
	}
}

// streamWitnesses names, per restricted package, identifiers whose signatures
// MUST match the stream pattern. Emptying this map, or leaving a package out
// of it, is a refusal rather than a pass — a positive control with no subject
// is the shape it exists to prevent.
var streamWitnesses = map[string][]string{
	"fmt":          {"Fprintf", "Fprintln", "Fscanf"},
	"encoding/hex": {"Dumper", "NewEncoder", "NewDecoder"},
}

// TestStreamWitnessesCoverEveryRestrictedPackage keeps the positive control
// from quietly narrowing. A package given a PureIdents restriction but no
// witness would have its stream derivation running unobserved.
func TestStreamWitnessesCoverEveryRestrictedPackage(t *testing.T) {
	if len(PureIdents) == 0 {
		t.Fatal("PureIdents is empty; this check would pass having judged nothing")
	}
	for _, pkg := range sortedKeys(PureIdents) {
		if len(streamWitnesses[pkg]) == 0 {
			t.Errorf("%q carries a PureIdents restriction but no stream witness, so nothing "+
				"proves the stream derivation still fires for it.", pkg)
		}
	}
	for pkg := range streamWitnesses {
		if _, ok := PureIdents[pkg]; !ok {
			t.Errorf("%q has stream witnesses but no PureIdents entry; the witness is checked "+
				"against a package the derivation never visits.", pkg)
		}
	}
}

// TestTimeAllowlistExcludesWaiters is DERIVED. Every waiting primitive in
// package time hands back a channel of Times, a *Timer or a *Ticker; the
// value-returning half hands back Durations, Times and strings. So the result
// type is the discriminator, and it holds for a primitive Go has not added yet.
//
// time.Sleep is the one waiter this derivation cannot see — it returns
// nothing — which is why TestRefusedIdents names it explicitly.
func TestTimeAllowlistExcludesWaiters(t *testing.T) {
	waiter := regexp.MustCompile(`\)\s*(<-chan Time|\*Timer|\*Ticker)\b`)
	sigs, err := goDocSignatures("time")
	if err != nil {
		t.Fatalf("cannot read the signatures of time: %v", err)
	}
	if len(sigs) == 0 {
		t.Fatal("no signatures parsed for time; refusing rather than passing")
	}
	hits := 0
	for name, sig := range sigs {
		if !waiter.MatchString(sig) {
			continue
		}
		hits++
		if TestIdents["time"][name] {
			t.Errorf("time.%s is allowlisted in tests but it is a waiting primitive: %s", name, sig)
		}
	}
	if hits == 0 {
		t.Fatal("the waiter pattern matched nothing in package time; the derivation has stopped working " +
			"and would now pass over any waiter")
	}
}

// TestContextAllowlistExcludesDeadlines is DERIVED. A context constructor that
// takes a time.Duration or a time.Time installs a deadline, and whatever later
// blocks on ctx.Done() is waiting for a timer to fire.
func TestContextAllowlistExcludesDeadlines(t *testing.T) {
	deadline := regexp.MustCompile(`\btime\.(Duration|Time)\b`)
	sigs, err := goDocSignatures("context")
	if err != nil {
		t.Fatalf("cannot read the signatures of context: %v", err)
	}
	hits := 0
	for name, sig := range sigs {
		if !deadline.MatchString(sig) {
			continue
		}
		hits++
		if TestIdents["context"][name] {
			t.Errorf("context.%s is allowlisted in tests but it installs a deadline: %s", name, sig)
		}
	}
	if hits == 0 {
		t.Fatal("the deadline pattern matched nothing in package context; the derivation has stopped " +
			"working and would now pass over any deadline constructor")
	}
}

// --- helpers ---------------------------------------------------------------

func goListDeps(pkg string) (map[string]bool, error) {
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set, nil
}

// funcRe matches a PACKAGE-LEVEL func in `go doc -all` output. A method reads
// `func (d Duration) Abs() ...`, where the character after "func " is "(", so
// requiring a capital letter there excludes methods without a second pattern.
// That matters: the gates resolve pkg.Ident, and a method is not one.
var funcRe = regexp.MustCompile(`^func ([A-Z]\w*)\(`)
var typeRe = regexp.MustCompile(`^type ([A-Z]\w*)\b`)

// goDocSignatures returns every package-level func and type signature in a
// package, keyed by identifier.
//
// It uses `go doc -all`, and the -all is load-bearing rather than tidy. Plain
// `go doc` groups a constructor UNDER the type it returns and indents it, so
// NewTimer and NewTicker are invisible to a top-level scan. The first version
// of this file used plain `go doc`, and the waiter derivation below therefore
// saw 3 of the 6 waiting primitives while its own non-empty guard stayed
// satisfied by the 3 it did see. Under-coverage hiding behind a passing check
// is the defect this whole file exists to remove.
// goDocAll runs `go doc -all` once per package. Cached because the exactness
// half of declared() asks for it per identifier and there are ~100 of them.
var goDocAllCache = map[string][]byte{}

func goDocAll(pkg string) ([]byte, error) {
	if out, ok := goDocAllCache[pkg]; ok {
		return out, nil
	}
	out, err := exec.Command("go", "doc", "-all", pkg).Output()
	if err != nil {
		return nil, err
	}
	goDocAllCache[pkg] = out
	return out, nil
}

func goDocSignatures(pkg string) (map[string]string, error) {
	out, err := goDocAll(pkg)
	if err != nil {
		return nil, err
	}
	sigs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if m := funcRe.FindStringSubmatch(line); m != nil {
			sigs[m[1]] = strings.TrimSpace(line)
			continue
		}
		if m := typeRe.FindStringSubmatch(line); m != nil {
			sigs[m[1]] = strings.TrimSpace(line)
		}
	}
	return sigs, nil
}

// declared asks whether pkg declares exactly this exported identifier.
//
// Two signals, because each covers the other's blind spot:
//
//  1. `go doc pkg.Ident` exits non-zero when it does not resolve. It reaches
//     constants inside a grouped const block, methods, types and functions
//     alike, which no single listing pattern does.
//  2. The spelling must appear verbatim in `go doc -all pkg`.
//
// (1) alone is what this file used, and it was documented as "an exact oracle
// with no format to drift". MEASURED 2026-08-29 by review: it is
// CASE-INSENSITIVE. `go doc time.now`, `go doc fmt.errorf` and
// `go doc encoding/hex.dump` all resolve. So `"Kitchen"` mistyped as
// `"kitchen"` survived the whole suite, while `"Kitchenz"` died — a
// case-only typo was inert rather than caught, which is dead weight of
// exactly the kind this check exists to remove.
//
// (2) alone would accept an identifier the standard library has REMOVED but
// still mentions in a doc comment, which is the drift (1) is good at.
//
// MEASURED 2026-08-29: (2) returns a hit for all 102 currently allowlisted
// identifiers, 0 misses, so it is implementable today with no false positive.
func declared(pkg, ident string) (bool, string) {
	if exec.Command("go", "doc", pkg+"."+ident).Run() != nil {
		return false, "does not resolve"
	}
	out, err := goDocAll(pkg)
	if err != nil {
		return false, "go doc -all failed: " + err.Error()
	}
	if !regexp.MustCompile(`\b` + regexp.QuoteMeta(ident) + `\b`).Match(out) {
		return false, "resolves only because `go doc` is case-insensitive; " +
			"the spelling appears nowhere in the package"
	}
	return true, ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestNarrowingCoverageIsMeasured reports how much of the allowlist any test
// file names at all. It asserts no floor; it exists so the bound in
// docs/gates.md is a number a run prints rather than a sentence that rots.
//
// Why the bound needs one. The preservation controls come in two kinds
// (t1/policy_driven_test.go, t2/policy_driven_test.go): generated ones, which
// build their fixture FROM the tables and therefore cannot see a table being
// narrowed, and hand-written ones, which can. So an identifier no hand-written
// fixture names can be deleted from the allowlist with nothing going red.
//
// MEASURED 2026-08-29, and confirmed independently by review: 16 of 102
// allowlisted identifiers are named in any _test.go file — encoding/hex 1/11,
// fmt 1/14, context 2/12, time 12/65. Review measured 4 of 8 identifier
// narrowings on TODAY's tables surviving the whole suite (fmt.Sprintf,
// hex.Dump, time.Kitchen, context.WithValue), against 4 of 4 PACKAGE
// narrowings dying.
//
// That escape is open NOW — it is a description of today's tables, not a
// hypothesis about a package admitted later.
//
// Why it is tolerated at M0 rather than closed: an over-narrow allowlist makes
// the gate REFUSE honest code, loudly, at the point of use, naming the
// identifier. It is a self-announcing failure, unlike a widening, which is
// silent — and the widening direction is fully covered. Closing it by naming
// all 102 identifiers in a hand-written fixture would rebuild the generated
// control by hand and lie about what M1 needs.
//
// The corpus is the test files with COMMENTS STRIPPED, and that is not a
// detail. MEASURED 2026-08-29: scanning the raw bytes reported 20/102 rather
// than 16/102, because this very docstring names fmt.Sprintf, hex.Dump,
// time.Kitchen and context.WithValue while explaining that they are NOT
// covered. A measurement that reads its own prose as evidence is the defect
// this file exists to remove, committed by the check that reports it.
//
// What this CANNOT see: it is a literal scan for "pkg.Ident", so a fixture
// naming an identifier through any other construction is invisible to it, and
// it counts appearances in VIOLATE-expecting fixtures too — so the number it
// prints is an UPPER bound on real preservation coverage, not the coverage.
func TestNarrowingCoverageIsMeasured(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate the module root from this package, so coverage is unmeasurable: %v", err)
	}

	var corpus []byte
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "testdata" || name == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := codeWithoutComments(path)
		if err != nil {
			return err
		}
		files++
		corpus = append(corpus, b...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree for test files failed, so coverage is unmeasurable: %v", err)
	}
	// A walk that finds nothing would report 0/102 as though it were a
	// measurement of the fixtures rather than of the walk.
	if files == 0 {
		t.Fatal("no _test.go file was found; this check would report zero coverage having measured nothing")
	}

	total, named := 0, 0
	for _, tbl := range []map[string]map[string]bool{PureIdents, TestIdents} {
		for _, pkg := range sortedKeys(tbl) {
			local := pkg[strings.LastIndex(pkg, "/")+1:]
			n, seen := 0, 0
			for _, id := range sortedKeys(tbl[pkg]) {
				n++
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(local+"."+id) + `\b`).Match(corpus) {
					seen++
				}
			}
			total += n
			named += seen
			t.Logf("%-14s %2d/%2d allowlisted identifiers named in a test file", pkg, seen, n)
		}
	}
	if total == 0 {
		t.Fatal("the allowlists are empty; this check would report full coverage having measured nothing")
	}
	t.Logf("NARROWING COVERAGE (upper bound): %d/%d named across %d test files; %d are named nowhere "+
		"and could be removed from the allowlist with nothing going red", named, total, files, total-named)
}

// codeWithoutComments renders a Go file with its comments removed, so a
// measurement over test sources cannot count prose about an identifier as a
// use of it. Fixtures held in raw string literals are preserved, because those
// are code as far as the gates are concerned.
func codeWithoutComments(path string) ([]byte, error) {
	fset := token.NewFileSet()
	// No parser.ParseComments: comments are then not attached to the AST at
	// all, so the printer cannot reintroduce them.
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
