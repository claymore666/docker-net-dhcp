// Package manifest pins verify.manifest.sh from a second language.
//
// The manifest states what must be there. This file states it again, in Go, in
// a different directory, with no derivation shared with the shell — so
// shrinking the arbiter's population takes an edit here as well as there.
//
// Round 9's finding is why. Four rounds running, a guard derived its
// expectation from the thing it guarded, and the fix each time was another
// guard with the same property. The last one was measured by deleting the
// shellcheck gate from verify.sh: eleven rows became ten and the arbiter said
// PASS. A number written down in one file can always be lowered by editing
// that file; the only thing that changes is how many files.
//
// This test runs inside the unit suite, and the unit suite's own floor —
// MIN_DECLARED_TESTS — is in the manifest. Deleting this file therefore takes
// the declared-test count below its floor rather than removing the pin
// silently.
package manifest

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	manifestPath = "../../verify.manifest.sh"
	oraclePath   = "../../scripts/test-verify.sh"
)

// The pinned row set. Exact and ordered: this is the verdict table, and a row
// arriving or leaving is a change somebody should have to make twice.
var pinnedRows = []string{
	"self-check",
	"citations",
	"bounds",
	"build",
	"vet",
	"gofmt",
	"shellcheck",
	"doc-numbers",
	"readme-usage",
	"gate-roster",
	"t1",
	"t2",
	"unit-suite",
	"netns-suite",
	"self-drive",
	"verify-oracle",
}

// The three SUBSETS of that table, pinned by membership rather than by size.
// Each names the rows one flag changes, and each is the kind of list that goes
// wrong by growing: --inner is defined by what it does not run, --light by
// what it leaves out, and SKIPPED by who may say it.
var (
	pinnedOuterRows     = []string{"netns-suite", "self-drive", "verify-oracle"}
	pinnedScopedOutRows = []string{"unit-suite", "netns-suite"}
	pinnedSkippableRows = []string{"verify-oracle"}
)

var pinnedGates = []string{"t1", "t2"}

// Floors, not equalities: these populations are meant to grow without an edit
// here, and are meant to be unable to shrink without one.
//
// minDeclaredTests is a LOW-WATER MARK and is deliberately not maintained in
// step with the manifest. verify.sh holds MIN_DECLARED_TESTS to a BAND against
// the tree — below it, or more than MAX_DECLARED_MARGIN above it, is a
// failure; this number exists only so that the manifest cannot be lowered past
// a level somebody once measured.
//
// maxDeclaredMarginCap is the other direction and is a CEILING, not a floor: a
// band that may be widened at will is a floor with no upper edge, which is the
// state round 11 was sent to close. Checked below by its own comparison, not by
// the floors loop.
// MAINTENANCE, stated because the review asked how a low-water mark is kept
// from decaying: a round that ADDS to one of these populations raises the pin
// to the number it just measured, in the same change. That is the whole rule.
// It is cheap because adding a scenario is already a three-file edit, and it
// keeps the distance between the pin and the tree at zero, which is the only
// value at which the pin is at full strength.
const (
	// 75 -> 77 at M7c, per the maintenance rule two paragraphs above and for
	// the reason it exists: MEASURED 2026-09-06, with the pin left at 75
	// against a 77-scenario manifest, the manifest-scenario-removed scenario
	// stopped being caught by THIS test at all — deleting a scenario left 76,
	// which is still above 75, and the run only went red because the deleted
	// name happened to sit in docs/verifying.md's derived stale-anchor block.
	// A count backstop holds only AT the threshold.
	minScenarios     = 77
	minShellScripts  = 4
	minDeclaredTests = 382
	// The self-check row's probes and the refusals record() owes them. Pinned
	// here for the reason the manifest declares them at all: the count must
	// not live in the file that can delete the probe it counts.
	minSelfCheckProbes   = 7
	minSelfCheckRefusals = 5
	minOracleSeconds     = 8
	// The two operands of that floor. The measurement is a low-water mark on
	// how long a real oracle run takes; the percentage is what stops the floor
	// being derived down to nothing by editing the measurement instead.
	minOracleMeasured    = 120
	minOraclePercent     = 4
	maxDeclaredMarginCap = 4
	// A hard stop on the doc-numbers ceiling. It is deliberately not far above
	// today's value: this is the number that must not be nudged.
	docNumberCeilingCap = 95
)

// The classes a scenario contract may declare, and the verdicts it may name.
var (
	rcClasses = map[string]bool{"zero": true, "nonzero": true, "static": true}
	// SKIPPED joined the verdicts on 2026-09-05 with the oracle stamp. It is
	// the only verdict that is neither a measurement nor a refusal, which is
	// why MANIFEST_SKIPPABLE_ROWS exists and is pinned above: a verdict that
	// every row may give is not an exception, it is an exit.
	verdicts = map[string]bool{"PASS": true, "FAIL": true, "ABSENT": true, "SKIPPED": true}
)

// static means the scenario does not run the subject, so it cannot read the
// subject's table. It does NOT mean the scenario may observe nothing: the "-"
// token that once said exactly that is gone, and a static contract must still
// name something it saw. That rule, not this cap, is what closes the hole.
//
// The cap stays because a class spreads through its exemption. It was raised
// from 1 to 2 when the second member arrived, deliberately and with the count
// beside it — the pattern to refuse is a cap raised in the same edit as the
// member that broke it becoming routine.
//
// ROUND 2, 2026-09-05: raised to 3 with scenario-rc-follows-the-verdict, which
// runs one scenario in a copy and observes the exit status it answered with.
// The reason it is the sanctioned case and not the routine one: two of the
// three members are about the ORACLE'S OWN PROTOCOL — how a scenario reports,
// and what its exit status means — and such a scenario has no row of the
// subject's table to read by construction. A fourth member that is not of that
// kind is the one to refuse.
const maxStaticContracts = 3

func readManifest(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("the manifest is unreadable, so nothing here measured anything: %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("the manifest is empty; an empty expectation is satisfied by everything")
	}
	return string(b)
}

// list returns the names in a `NAME=( ... )` block. It fails the test rather
// than returning empty, because an unparsed list reads exactly like a list
// with nothing in it — which is the defect this whole file exists against.
func list(t *testing.T, src, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=\(\n((?:.*\n)*?)\)\n`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s is not declared as a multi-line array in %s", name, manifestPath)
	}
	var out []string
	for _, ln := range strings.Split(m[1], "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		out = append(out, ln)
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to no names", name)
	}
	return out
}

// number returns a `NAME=<int>` scalar.
func number(t *testing.T, src, name string) int {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=(\d+)\s*$`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s is not declared as an integer in %s", name, manifestPath)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%s = %q is not a number: %v", name, m[1], err)
	}
	return n
}

func TestManifestRowsAreTheRowsPinnedHere(t *testing.T) {
	got := list(t, readManifest(t), "MANIFEST_ROWS")
	if len(got) != len(pinnedRows) {
		t.Fatalf("MANIFEST_ROWS has %d row(s), this file pins %d: %v", len(got), len(pinnedRows), got)
	}
	for i := range got {
		if got[i] != pinnedRows[i] {
			t.Errorf("row %d: manifest says %q, this file pins %q", i, got[i], pinnedRows[i])
		}
	}
}

func TestManifestGatesAreTheGatesPinnedHere(t *testing.T) {
	got := list(t, readManifest(t), "MANIFEST_GATES")
	if len(got) != len(pinnedGates) {
		t.Fatalf("MANIFEST_GATES has %d gate(s), this file pins %d: %v", len(got), len(pinnedGates), got)
	}
	for i := range got {
		if got[i] != pinnedGates[i] {
			t.Errorf("gate %d: manifest says %q, this file pins %q", i, got[i], pinnedGates[i])
		}
	}
}

// The floors. A floor of zero is not a floor: that sentence is the round's
// whole finding, and these are the numbers that stop being zero.
func TestManifestFloorsAreNotBelowTheirPins(t *testing.T) {
	src := readManifest(t)
	for _, c := range []struct {
		name string
		got  int
		min  int
	}{
		{"MANIFEST_SCENARIOS_N", number(t, src, "MANIFEST_SCENARIOS_N"), minScenarios},
		{"MANIFEST_SCENARIO_CONTRACTS_N", number(t, src, "MANIFEST_SCENARIO_CONTRACTS_N"), minScenarios},
		{"MANIFEST_SHELL_SCRIPTS_N", number(t, src, "MANIFEST_SHELL_SCRIPTS_N"), minShellScripts},
		{"MIN_DECLARED_TESTS", number(t, src, "MIN_DECLARED_TESTS"), minDeclaredTests},
		{"SELF_CHECK_PROBES_N", number(t, src, "SELF_CHECK_PROBES_N"), minSelfCheckProbes},
		{"SELF_CHECK_REFUSALS_N", number(t, src, "SELF_CHECK_REFUSALS_N"), minSelfCheckRefusals},
		// ROUND 13, N12. ORACLE_MIN_SECONDS is no longer a literal, so the
		// pin reads its two OPERANDS and does the arithmetic itself. Reading
		// the derived name here would have to parse shell; reading the
		// operands cannot be satisfied by reinstating a literal, because
		// manifest_check refuses the two disagreeing.
		{"ORACLE_MEASURED_SECONDS", number(t, src, "ORACLE_MEASURED_SECONDS"), minOracleMeasured},
		{"ORACLE_MIN_PERCENT", number(t, src, "ORACLE_MIN_PERCENT"), minOraclePercent},
		{"the derived oracle floor", number(t, src, "ORACLE_MEASURED_SECONDS") * number(t, src, "ORACLE_MIN_PERCENT") / 100, minOracleSeconds},
	} {
		if c.got < c.min {
			t.Errorf("%s is %d, below the %d pinned here; a population shrank and the shell alone would not have said so", c.name, c.got, c.min)
		}
	}

	// The one operand here whose danger is upward. Widening the band is the
	// cheapest way to make the declared-test row stop saying anything, and it
	// looks like maintenance while doing it.
	if m := number(t, src, "MAX_DECLARED_MARGIN"); m < 0 || m > maxDeclaredMarginCap {
		t.Errorf("MAX_DECLARED_MARGIN is %d, outside 0..%d; the band was widened rather than the floor raised", m, maxDeclaredMarginCap)
	}

	// The self-check's two halves, held apart. Refusals equal to probes is a
	// row with no preservation control; zero refusals is not a guard at all.
	// The shell checks this too, in the file that declares them.
	if pr, rf := number(t, src, "SELF_CHECK_PROBES_N"), number(t, src, "SELF_CHECK_REFUSALS_N"); rf < 1 || rf >= pr {
		t.Errorf("SELF_CHECK_REFUSALS_N is %d against %d probe(s); a self-check that refuses none of its probes is not a guard and one that refuses all of them has no control", rf, pr)
	}

	// Same direction, same reason: raising the ceiling is the cheap way to
	// make the doc-numbers row stop saying anything, and it looks like
	// maintenance while doing it.
	if c := number(t, src, "DOC_NUMBER_CEILING"); c < 1 || c > docNumberCeilingCap {
		t.Errorf("DOC_NUMBER_CEILING is %d, outside 1..%d; the prose was allowed to grow rather than the number deleted", c, docNumberCeilingCap)
	}
}

// Layer 2, read from Go as well as from the shell. The shell's manifest_check
// runs this same comparison; if only the shell ran it, deleting manifest_check
// would delete the check with its subject, which is the shape of every finding
// in rounds 5 through 8.
func TestManifestListLengthsMatchTheirDeclaredCounts(t *testing.T) {
	src := readManifest(t)
	for _, c := range []struct{ list, count string }{
		{"MANIFEST_ROWS", "MANIFEST_ROWS_N"},
		{"MANIFEST_GATES", "MANIFEST_GATES_N"},
		{"MANIFEST_SHELL_SCRIPTS", "MANIFEST_SHELL_SCRIPTS_N"},
		{"MANIFEST_SCENARIOS", "MANIFEST_SCENARIOS_N"},
		{"MANIFEST_SCENARIO_CONTRACTS", "MANIFEST_SCENARIO_CONTRACTS_N"},
	} {
		names := list(t, src, c.list)
		if n := number(t, src, c.count); n != len(names) {
			t.Errorf("%s holds %d name(s) but %s says %d", c.list, len(names), c.count, n)
		}
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				t.Errorf("%s lists %q twice; a duplicate inflates a count without adding a subject", c.list, n)
			}
			seen[n] = true
		}
	}
}

// contract is one MANIFEST_SCENARIO_CONTRACTS entry: what a scenario must have
// OBSERVED, as opposed to what it is called.
type contract struct {
	scenario string
	rcClass  string
	token    string
	row      string
	verdict  string
	diag     string
}

func contracts(t *testing.T, src string) []contract {
	t.Helper()
	var out []contract
	for _, raw := range list(t, src, "MANIFEST_SCENARIO_CONTRACTS") {
		e := strings.Trim(raw, `"`)
		parts := strings.Split(e, "|")
		if len(parts) != 4 {
			t.Errorf("contract %q is not name|rc-class|token|diagnosis", e)
			continue
		}
		c := contract{scenario: parts[0], rcClass: parts[1], token: parts[2], diag: parts[3]}
		// There is no "-" escape any more. A contract that demanded nothing
		// was one entry away from being the thing round 11 exists to forbid,
		// and it had exactly one user, which could observe something real.
		tok := strings.SplitN(parts[2], ":", 2)
		if len(tok) != 2 {
			t.Errorf("contract %q: token %q is not <subject>:<value>", e, parts[2])
			continue
		}
		c.row, c.verdict = tok[0], tok[1]
		out = append(out, c)
	}
	return out
}

// Round 11. The manifest pinned names and counts, and B14 kept every name while
// deleting what the names stood for: four scenario bodies emptied, one comment
// left in place to satisfy a grep, and the arbiter printed PASS over a live
// defect. A name is not a behaviour.
//
// This is the structural half of the answer — verify.sh compares each contract
// against what the oracle reports it observed, and this test makes sure there
// is a contract to compare, that it demands something real, and that the one
// exemption cannot spread.
func TestEveryScenarioHasAWellFormedBehaviourContract(t *testing.T) {
	src := readManifest(t)
	scenarios := list(t, src, "MANIFEST_SCENARIOS")
	cs := contracts(t, src)

	if len(cs) != len(scenarios) {
		t.Fatalf("%d scenario(s) but %d contract(s); a scenario with no contract is a name with no behaviour", len(scenarios), len(cs))
	}

	seen := map[string]bool{}
	static := 0
	for _, c := range cs {
		if seen[c.scenario] {
			t.Errorf("scenario %q has more than one contract", c.scenario)
		}
		seen[c.scenario] = true
		if !rcClasses[c.rcClass] {
			t.Errorf("scenario %q: rc-class %q is not one of zero/nonzero/static", c.scenario, c.rcClass)
		}
		if c.rcClass == "static" {
			static++
			// A static scenario still has to observe SOMETHING — it just
			// cannot be a row verdict, because it never ran the subject. This
			// is the class B14 would have hidden in: "it does not run
			// anything" was, until this round, also "it need not report
			// anything", and the two are not the same claim.
			if verdicts[c.verdict] {
				t.Errorf("scenario %q is static and names a row verdict %q; a scenario that never runs the subject cannot read its table", c.scenario, c.token)
			}
			continue
		}
		if c.row == "" {
			t.Errorf("scenario %q demands no row observation, so it is satisfied by a body that runs the subject and reads nothing", c.scenario)
		}
		if !verdicts[c.verdict] {
			t.Errorf("scenario %q: verdict %q is not PASS/FAIL/ABSENT", c.scenario, c.verdict)
		}
	}
	for _, s := range scenarios {
		if !seen[s] {
			t.Errorf("scenario %q has no contract", s)
		}
	}
	checkDiagnoses(t, cs)
	if static > maxStaticContracts {
		t.Errorf("%d static contract(s), at most %d allowed; static means the scenario never runs the subject, and an uncapped exemption empties this check without moving a single count", static, maxStaticContracts)
	}

	// ROUND 13, N9. The cap is a number; the MEMBERSHIP is a list, and until
	// this round the list lived only in a comment that had been wrong since
	// the second member landed. Set equality, both directions: an undeclared
	// static contract and a declared name that is not static both fail.
	declared := map[string]bool{}
	for _, s := range list(t, src, "MANIFEST_STATIC_CONTRACTS") {
		declared[s] = true
	}
	for _, c := range cs {
		if c.rcClass == "static" && !declared[c.scenario] {
			t.Errorf("scenario %q is static and is not in MANIFEST_STATIC_CONTRACTS; the exemption must be enumerated where a reader can count it", c.scenario)
		}
		if c.rcClass != "static" && declared[c.scenario] {
			t.Errorf("scenario %q is declared static in MANIFEST_STATIC_CONTRACTS but its contract is %q; a declaration that overstates the exemption hides a member being added", c.scenario, c.rcClass)
		}
	}
	for s := range declared {
		if !seen[s] {
			t.Errorf("MANIFEST_STATIC_CONTRACTS names %q, which has no contract at all", s)
		}
	}
}

// The replacement for the row-coverage check that used to live in the oracle as
// `grep -q "row $r"` over the oracle's own source. MEASURED 2026-08-30 by
// review: one comment satisfied it.
//
// A row is covered when some scenario must OBSERVE it in a named state. That
// cannot be satisfied by a comment, and it cannot be satisfied by a name.
func TestEveryRequiredRowIsTheSubjectOfAContract(t *testing.T) {
	src := readManifest(t)
	covered := map[string]bool{}
	for _, c := range contracts(t, src) {
		if c.row != "" {
			covered[c.row] = true
		}
	}
	for _, r := range list(t, src, "MANIFEST_ROWS") {
		if !covered[r] {
			t.Errorf("row %q is required by the manifest and no scenario contract requires it to be observed; nothing proves its count is derived rather than written", r)
		}
	}
}

// The diagnosis half of a contract, added round 13.
//
// A contract used to name the ROW a scenario must redden and nothing named the
// DEFECT it must plant, so a body cut down to the lines producing its
// contracted observation — planting anything at all that reached the same row —
// satisfied every operand in the tree. Two scenarios on one row were
// interchangeable at every instrument.
//
// The diagnosis is the arbiter's own account of what it found. It is written by
// the subject, not by the scenario, which is the whole reason it can name a
// defect that a verdict cannot.
const (
	minDiagLen = 8
	// A diagnosis genuinely shared by more than one contract. Two families
	// today and both are honest: an ABSENT row has no note to differ in (five
	// contracts), and the two false-positive citation controls assert the SAME
	// passing account on purpose. Static contracts are excluded above and
	// counted by their own cap. This one is tight rather than slack — a cap at
	// today's value refuses the third family loudly, which is the opposite
	// direction from a floor at today's value.
	maxSharedDiagnoses = 9
)

func checkDiagnoses(t *testing.T, cs []contract) {
	t.Helper()
	seenDiag := map[string]int{}
	byRow := map[string][]contract{}
	for _, c := range cs {
		if c.diag == "" {
			t.Errorf("scenario %q states no diagnosis; a contract that names only a row is satisfied by any defect that reddens it", c.scenario)
			continue
		}
		if c.rcClass == "static" {
			if c.diag != "no row" {
				t.Errorf("scenario %q is static and its diagnosis is %q; a scenario that reads no row has no note, and the field must say so in one spelling", c.scenario, c.diag)
			}
			continue
		}
		if len(c.diag) < minDiagLen {
			t.Errorf("scenario %q: diagnosis %q is shorter than %d characters; a short diagnosis is satisfied by notes it does not name", c.scenario, c.diag, minDiagLen)
		}
		for _, r := range c.diag {
			if !(r == ' ' || r == '#' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				t.Errorf("scenario %q: diagnosis %q carries %q; notes are recorded squashed to letters, spaces and # so a diagnosis must be written the same way", c.scenario, c.diag, r)
				break
			}
		}
		seenDiag[c.diag]++
		if c.verdict == "FAIL" {
			byRow[c.row] = append(byRow[c.row], c)
		}
	}

	// Two scenarios reddening one row must not be able to claim each other's
	// finding. Nesting counts as sharing: a diagnosis that is a substring of
	// another is satisfied by the other's note.
	for row, items := range byRow {
		for _, a := range items {
			for _, b := range items {
				if a.scenario != b.scenario && strings.Contains(b.diag, a.diag) {
					t.Errorf("row %q: %q's diagnosis %q is contained in %q's %q; the two scenarios are interchangeable on this row",
						row, a.scenario, a.diag, b.scenario, b.diag)
				}
			}
		}
	}

	shared := 0
	for _, n := range seenDiag {
		if n > 1 {
			shared += n
		}
	}
	if shared > maxSharedDiagnoses {
		t.Errorf("%d contract(s) share a diagnosis with another, at most %d allowed; a diagnosis that names two scenarios names neither", shared, maxSharedDiagnoses)
	}
}

// ROUND 13, N11. MAX_DECLARED_MARGIN was a literal sitting exactly at today's
// measured maximum, capped at 4 — so the band could be quadrupled one line at
// a time, and every one of those edits would look like the maintenance the
// manifest says it removes. §0.2: a literal at today's value erodes.
//
// The number has a derivation, and this is it. The margin exists because an
// oracle scenario plants Go test functions into its copy of the tree, so the
// declared-test count seen by verify.sh INSIDE that copy is the tree's count
// plus whatever that one scenario planted. The margin must therefore equal the
// largest number of test functions any single scenario plants, counting the
// helpers it calls — not "1 because that is what we measured once".
//
// BOUNDS, because this is a static call-graph read of a shell script:
//   - Call edges are name occurrences outside comments. A name mentioned in a
//     string is a false edge, which can only ever RAISE the derived maximum,
//     so it fails closed here rather than silently permitting a wider band.
//   - Indirect dispatch — a helper invoked through a variable — is invisible
//     to it. The oracle calls scenarios as `sc_$name`, which this resolves
//     specially; anything else added later would be missed.
//   - It counts `func Test...` in the whole body including comments, so a
//     commented-out planted test still counts. Same direction: closed.
func TestDeclaredTestMarginIsDerivedFromWhatScenariosPlant(t *testing.T) {
	src, err := os.ReadFile(oraclePath)
	if err != nil {
		t.Fatalf("reading %s: %v", oraclePath, err)
	}
	bodies := shellFunctions(string(src))
	if len(bodies) < 20 {
		t.Fatalf("parsed %d shell function(s) out of %s; the parse failed, and a failed parse derives a maximum of zero", len(bodies), oraclePath)
	}

	worst, worstName := 0, ""
	for name := range bodies {
		if !strings.HasPrefix(name, "sc_") {
			continue
		}
		if n := plantedBy(name, bodies, map[string]bool{}); n > worst {
			worst, worstName = n, name
		}
	}
	if worstName == "" {
		t.Fatalf("no sc_* function found in %s", oraclePath)
	}

	declared := number(t, readManifest(t), "MAX_DECLARED_MARGIN")
	if declared != worst {
		t.Errorf("MAX_DECLARED_MARGIN is %d; the widest single scenario plants %d test function(s) (%s). The margin is not a preference: set it to %d, and if that number is growing, the plants are what to shrink", declared, worst, worstName, worst)
	}
}

var (
	shellFuncStart = regexp.MustCompile(`(?m)^([a-z_][a-z0-9_]*)\(\) \{$`)
	goTestFunc     = regexp.MustCompile(`(?m)^func Test[A-Za-z0-9_]*\(`)
	shellComment   = regexp.MustCompile(`(?m)^[\t ]*#.*$`)
)

// shellFunctions splits a shell script into top-level function bodies. A
// top-level body ends at the first line that is exactly "}".
func shellFunctions(src string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		m := shellFuncStart.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				out[m[1]] = strings.Join(lines[i+1:j], "\n")
				i = j
				break
			}
		}
	}
	return out
}

// plantedBy counts the Go test functions a scenario writes into its copy,
// following the helpers it calls.
func plantedBy(name string, bodies map[string]string, seen map[string]bool) int {
	if seen[name] {
		return 0
	}
	seen[name] = true
	body, ok := bodies[name]
	if !ok {
		return 0
	}
	n := len(goTestFunc.FindAllString(body, -1))
	code := shellComment.ReplaceAllString(body, "")
	for callee := range bodies {
		if callee == name {
			continue
		}
		if regexp.MustCompile(`(^|[^A-Za-z0-9_$])` + regexp.QuoteMeta(callee) + `([^A-Za-z0-9_(]|$)`).MatchString(code) {
			n += plantedBy(callee, bodies, seen)
		}
	}
	return n
}

// TestManifestRowSubsetsAreThePinnedSubsets holds the three lists that say
// which rows a FLAG changes: the rows an inner run does not have, the rows a
// scoped run leaves out, and the rows that may record SKIPPED.
//
// Each is pinned by membership rather than by size, in a second language and a
// second directory, for the reason the row list itself is: these are the lists
// that go wrong by growing. A row added to MANIFEST_SCOPED_OUT_ROWS stops
// being run by every light scenario at once, and nothing in the shell would
// notice — the manifest would still agree with itself.
func TestManifestRowSubsetsAreThePinnedSubsets(t *testing.T) {
	src := readManifest(t)
	rows := map[string]bool{}
	for _, r := range list(t, src, "MANIFEST_ROWS") {
		rows[r] = true
	}
	for _, c := range []struct {
		name   string
		pinned []string
	}{
		{"MANIFEST_OUTER_ROWS", pinnedOuterRows},
		{"MANIFEST_SCOPED_OUT_ROWS", pinnedScopedOutRows},
		{"MANIFEST_SKIPPABLE_ROWS", pinnedSkippableRows},
	} {
		got := list(t, src, c.name)
		if n := number(t, src, c.name+"_N"); n != len(got) {
			t.Errorf("%s has %d name(s), %s_N says %d", c.name, len(got), c.name, n)
		}
		if len(got) != len(c.pinned) {
			t.Errorf("%s has %d name(s), this file pins %d: %v", c.name, len(got), len(c.pinned), got)
			continue
		}
		for i := range got {
			if got[i] != c.pinned[i] {
				t.Errorf("%s[%d]: manifest says %q, this file pins %q", c.name, i, got[i], c.pinned[i])
			}
			if !rows[got[i]] {
				t.Errorf("%s names %q, which is not a row in MANIFEST_ROWS", c.name, got[i])
			}
		}
		// Proper, in both directions. Empty and the flag means nothing;
		// equal to the whole table and the flag is a run of nothing.
		if len(got) == 0 || len(got) >= len(rows) {
			t.Errorf("%s holds %d of %d row(s); it must be a non-empty PROPER subset", c.name, len(got), len(rows))
		}
	}
}

// TestLightScenariosCannotScopeAwayTheRowTheyDrive is the Go half of the
// refusal item 2 turns on.
//
// A scenario that runs at the light scope does not run the rows in
// MANIFEST_SCOPED_OUT_ROWS. If such a scenario's contract names one of those
// rows, it has scoped away the row it exists to drive: the row records
// nothing, the contract reads it ABSENT, and the scenario fails. That failure
// is the safety net. This is the refusal — the combination cannot be written
// down — and it is here rather than only in the shell because the shell's
// version of it lives in the same file as the lists it compares.
func TestLightScenariosCannotScopeAwayTheRowTheyDrive(t *testing.T) {
	src := readManifest(t)
	scoped := map[string]bool{}
	for _, r := range list(t, src, "MANIFEST_SCOPED_OUT_ROWS") {
		scoped[r] = true
	}
	declared := map[string]bool{}
	for _, s := range list(t, src, "MANIFEST_SCENARIOS") {
		declared[s] = true
	}
	byName := map[string]contract{}
	for _, c := range contracts(t, src) {
		byName[c.scenario] = c
	}
	light := list(t, src, "MANIFEST_LIGHT_SCENARIOS")
	if n := number(t, src, "MANIFEST_LIGHT_SCENARIOS_N"); n != len(light) {
		t.Errorf("MANIFEST_LIGHT_SCENARIOS has %d name(s), MANIFEST_LIGHT_SCENARIOS_N says %d", len(light), n)
	}
	seen := map[string]bool{}
	for _, s := range light {
		if seen[s] {
			t.Errorf("MANIFEST_LIGHT_SCENARIOS names %q twice", s)
		}
		seen[s] = true
		if !declared[s] {
			t.Errorf("MANIFEST_LIGHT_SCENARIOS names %q, which is not a declared scenario", s)
			continue
		}
		c, ok := byName[s]
		if !ok {
			t.Errorf("scenario %q is declared light and has no contract", s)
			continue
		}
		if scoped[c.row] {
			t.Errorf("scenario %q is declared light and its contract wants row %q, which a light run does not run; it would pass or fail on a row it scoped away", s, c.row)
		}
		if c.rcClass == "static" {
			t.Errorf("scenario %q is static — it never runs the subject — so declaring a scope for it describes a run that does not happen", s)
		}
	}
	// The control is the one scenario that must run everything: a control at a
	// reduced scope controls a reduced thing.
	if seen["control"] {
		t.Error("the control is declared light; a control that does not run every row cannot say the arbiter is green on an untouched tree")
	}
	// Non-empty and proper, for the same reason as the row subsets: every
	// scenario light is an oracle that never runs the suite, and none light is
	// a list that costs a maintenance rule and buys nothing.
	if len(light) == 0 || len(light) >= len(declared) {
		t.Errorf("%d of %d scenario(s) are declared light; the set must be a non-empty PROPER subset", len(light), len(declared))
	}
}

// ROUND 2, 2026-09-05, finding 4. docs/verifying.md carried a hand-typed list
// of "the scenarios a skipped run is not re-checking" — the ones whose plants
// depend on the PRODUCT's shape, which the arbiter hash does not cover. It
// omitted eleven of them, MEASURED by review, and nothing could see that,
// because it was prose beside machinery.
//
// The list is now DERIVED here and the doc quotes it. What the derivation says,
// exactly: a scenario names a path inside its copy of the tree that is not
// verify.sh, not verify.manifest.sh and not under scripts/ — directly, or
// through a helper it calls. Those are the paths the stamp's hash does not
// cover, so an edit to one of them can leave a scenario anchored on text that
// is no longer there while every covered byte stands still.
//
// BOUNDS, because a completeness claim here would be the same defect one level
// up:
//   - It is textual. A scenario that reaches the product through a glob or a
//     `find` with no path written down is invisible to it, and
//     suite-tests-disabled is exactly that today.
//   - A path in a comment is excluded, a path in a string literal is not: the
//     read strips comment lines and nothing else. That direction ADDS names,
//     which is the safe one for a list of things to re-check.
//   - The copy root is spelled `$d` in a scenario and `$1`/`$2` in the helpers
//     it calls. Another spelling would be missed, and would show up as a name
//     dropping out of this list rather than as silence.
const staleAnchorFence = "```stale-anchor-scenarios"

// copyRootPath matches a path taken relative to the copied tree's root.
var copyRootPath = regexp.MustCompile(`\$(?:d|1|2)/([A-Za-z0-9_./-]+)`)

// hashedByTheStamp reports whether p is one of the arbiter files
// .verify-oracle-stamp's hash covers. It is verify.sh's own definition of the
// covered set, restated: verify.sh, verify.manifest.sh and everything under
// scripts/. The stamp itself is neither — it is the cache, not a subject.
func hashedByTheStamp(p string) bool {
	return p == "verify.sh" || p == "verify.manifest.sh" ||
		strings.HasPrefix(p, "scripts/") || p == ".verify-oracle-stamp"
}

// productPaths returns the paths outside the hashed set that fn names, following
// the helpers it calls.
func productPaths(fn string, bodies map[string]string, seen map[string]bool, out map[string]bool) {
	if seen[fn] {
		return
	}
	seen[fn] = true
	body, ok := bodies[fn]
	if !ok {
		return
	}
	code := shellComment.ReplaceAllString(body, "")
	for _, m := range copyRootPath.FindAllStringSubmatch(code, -1) {
		if !hashedByTheStamp(m[1]) {
			out[m[1]] = true
		}
	}
	for callee := range bodies {
		if callee == fn {
			continue
		}
		if regexp.MustCompile(`(^|[^A-Za-z0-9_$])` + regexp.QuoteMeta(callee) + `([^A-Za-z0-9_(]|$)`).MatchString(code) {
			productPaths(callee, bodies, seen, out)
		}
	}
}

func TestStaleAnchorBoundNamesWhatTheOracleDerives(t *testing.T) {
	src, err := os.ReadFile(oraclePath)
	if err != nil {
		t.Fatalf("reading %s: %v", oraclePath, err)
	}
	bodies := shellFunctions(string(src))
	if len(bodies) < 20 {
		t.Fatalf("parsed %d shell function(s) out of %s; a failed parse derives an empty list, which agrees with an empty block", len(bodies), oraclePath)
	}
	derived := map[string]bool{}
	for fn := range bodies {
		if !strings.HasPrefix(fn, "sc_") {
			continue
		}
		paths := map[string]bool{}
		productPaths(fn, bodies, map[string]bool{}, paths)
		if len(paths) > 0 {
			derived[strings.ReplaceAll(strings.TrimPrefix(fn, "sc_"), "_", "-")] = true
		}
	}
	if len(derived) == 0 {
		t.Fatalf("no scenario was found to name a path outside the hashed set; an empty derivation is satisfied by an empty block")
	}

	const docPath = "../../docs/verifying.md"
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}
	lines := strings.Split(string(doc), "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == staleAnchorFence {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s has no %s block; the bound is stated over nothing", docPath, staleAnchorFence)
	}
	stated := map[string]bool{}
	for _, ln := range lines[start:] {
		ln = strings.TrimSpace(ln)
		if ln == "```" {
			break
		}
		if ln != "" {
			stated[ln] = true
		}
	}

	declared := map[string]bool{}
	for _, s := range list(t, readManifest(t), "MANIFEST_SCENARIOS") {
		declared[s] = true
	}
	for name := range derived {
		if !stated[name] {
			t.Errorf("scenario %q anchors on the product and %s does not list it; the bound understates what a skipped run is not re-checking", name, docPath)
		}
		if !declared[name] {
			t.Errorf("the derivation produced %q, which is not a declared scenario; the parse is reading something other than the scenario set", name)
		}
	}
	for name := range stated {
		if !derived[name] {
			t.Errorf("%s lists %q as anchoring on the product and the oracle no longer does; a list that overstates the bound is a list nobody will believe", docPath, name)
		}
	}
}
