# shellcheck shell=bash
#
# verify.manifest.sh — what MUST be there. Declarations only; no logic that
# measures anything, and nothing here is derived from the files it describes.
#
# WHY THIS FILE EXISTS (round 9). Four consecutive review rounds found the same
# defect, one level up each time, and the cause was never carelessness: every
# guard derived its expectation from the thing it was guarding.
#
#   round 5  the unit suite's population came from the filesystem it checked
#   round 6  the oracle's expected count came from a grep over the oracle
#   round 7  the row roster lived in verify.sh, beside the rows
#   round 8  the row-coverage check read that roster back out of verify.sh
#
# Each had a non-vacuity floor and EVERY FLOOR WAS ZERO. Zero is the one value
# a domain cannot reach by deletion, so shrinking a population by one was
# invisible in all four. MEASURED 2026-08-30 by review: deleting the shellcheck
# gate — its step, its roster entry and its two scenarios — produced
# "VERDICT: PASS (10 steps)" with four live SC2034 findings in the tree.
#
# So the expectation is stated HERE, where it is not the subject of any check
# it parameterises, in three layers:
#
#   1. names, listed literally, sourced by verify.sh AND by the oracle;
#   2. a literal count beside each list, so removing a name without editing the
#      count beside it is a refusal — one edit is not enough even in this file;
#   3. a Go pin, internal/manifest, in another language and another directory,
#      which asserts these names and these numbers. It runs inside the unit
#      suite, whose own population is floored below.
#
# THE CLAIM, AND ITS BOUND. No edit confined to a SINGLE FILE can shrink the
# arbiter's population. That is not "cannot be shrunk": editing this file and
# the Go pin together still does it, and no mechanism in a repository can stop
# that. The regress terminates at a person reading a diff, and the whole point
# of this file is that the diff is one line, in a file whose entire content is
# the expectation, rather than eleven lines spread through the code that
# happens to implement it.

# The rows that must appear in the verdict table.
MANIFEST_ROWS=(
	self-check
	citations
	bounds
	build
	vet
	gofmt
	shellcheck
	doc-numbers
	gate-roster
	t1
	t2
	unit-suite
	self-drive
	verify-oracle
)
MANIFEST_ROWS_N=14

# The gate commands under internal/gates that must exist and must run.
MANIFEST_GATES=(
	t1
	t2
)
MANIFEST_GATES_N=2

# The shell scripts that must be linted. Cross-checked in both directions
# against a filesystem walk (scenarios unlinted-script, unlinted-shebang-script).
MANIFEST_SHELL_SCRIPTS=(
	verify.sh
	verify.manifest.sh
	scripts/test-verify.sh
	scripts/sweep-doc-numbers.sh
)
MANIFEST_SHELL_SCRIPTS_N=4

# The unit suite's declared-test population, stated rather than derived.
#
# This is the operand round 7 said could not exist. Its bound then was: "a test
# DELETED, rather than disabled, leaves both sides agreeing" — true, because
# both sides were derived from the tree. A literal is not derived from
# anything, so a deleted test moves the measurement and not this number.
#
# ROUND 11: it used to say "It is a FLOOR, not an equality: adding tests must
# not require an edit here." MEASURED 2026-08-30 by review: nothing in the tree
# raises it, so its protection ERODES MONOTONICALLY with every test added.
# Today's margin is zero — 169 against 169 — which is the strongest this
# operand will ever be. At 250 declared tests, 81 could be deleted with nothing
# going red and no instrument having noticed the decay. Three of the four
# manifest lists force their own maintenance; this one asked to be remembered,
# which is the property the whole design exists to remove.
#
# It is now a BAND, enforced by verify.sh in BOTH directions: below it is
# "tests were deleted", above MIN + MAX_DECLARED_MARGIN is "raise this number
# to N". Scenarios min-declared-tests-floor and min-declared-tests-margin.
#
# The Go pin holds a separate literal as a low-water mark, `>=` only. That one
# is NOT maintained in step and is not meant to be: it exists so that lowering
# the number here cannot go below a level somebody once measured.
MIN_DECLARED_TESTS=328

# How far above MIN_DECLARED_TESTS the tree may drift before the row refuses.
#
# It is not zero, and it is not a preference: an oracle scenario plants Go
# tests into its copy of the tree, so under a strict equality every such
# scenario would fail the unit-suite row it is not testing. The number is
# therefore exactly the largest number of test functions ANY ONE scenario
# plants, counting the helpers it calls.
#
# ROUND 13, N11. It used to be a literal sitting at that maximum as measured
# once, under a Go cap of 4 — so the band could be quadrupled one line at a
# time, and each of those edits would look exactly like the maintenance this
# file says it removes. It is now DERIVED and checked:
# TestDeclaredTestMarginIsDerivedFromWhatScenariosPlant reads the oracle,
# computes the maximum, and refuses any other value in EITHER direction. The
# 0..4 caps below and in the Go pin stay as a backstop, not as the check.
#
# BOUNDS, stated rather than claimed away: erosion is CAPPED at this number,
# not eliminated. One test may be added without anyone raising
# MIN_DECLARED_TESTS; the second one fails the row and names the number to
# write. A change that adds one test and deletes another is invisible to both
# edges — the band measures a population size, not its membership. And the
# derivation is a static read of a shell script: it cannot see a helper
# reached through a variable, and it counts a commented-out plant. Both of
# those fail closed, i.e. toward a larger derived number than the truth.
MAX_DECLARED_MARGIN=1

# The self-drive: the rows the ARBITER plants for itself, and the rows that must
# survive the same plant untouched.
#
# ROUND 13. The oracle's entire report is derivable from this file — every
# scenario name, its result class, its row verdict and now its diagnosis — so a
# script that sources this file and prints the contract table back reproduces a
# passing account without running anything. MEASURED 2026-08-30 by review at the
# previous head, and re-driven here: 24 lines, one file, clean under the lint
# gate, all sixty detections gone, whole run green in thirty seconds.
#
# Every check verify.sh could perform on the oracle's REPORT is defeatable by a
# fabricator that knows the check, because the report is the fabricator's own
# output. A replay that asks the same script again is answered by the same
# script; that construction was written this round and deleted for being
# circular. The only non-circular answer is the arbiter doing the work.
#
# So verify.sh plants these defects itself, in one copy, and runs one --inner
# pass. Blinding the arbiter now needs an edit to verify.sh as well as to the
# oracle — which is the difference between round 10's claim, that no single-file
# edit can shrink the arbiter's POPULATION, and the claim it does not make, that
# no single-file edit can BLIND it.
#
# BOUND, and it is a large one: these are seven rows of fourteen with one plant
# each, in ONE tree, chosen so no plant cascades into a row on the survivors
# list. The self-drive is a lower bound on the arbiter's liveness, not a
# substitute for the oracle's sixty-three, and it does not become one. The
# seven rows it does not plant stay blindable by an edit the oracle can no
# longer object to.
#
# MEASURED 2026-08-30, both halves, against the finished tree: fabricate the
# oracle by injection (one file, all sixty-three of its detections gone) AND
# blind the gofmt row in verify.sh AND plant a live unformatted file — the
# shape that gave VERDICT: PASS before this round — and the run ends
# VERDICT: FAIL on `gofmt=PASS (planted, did not redden)`. The fabrication is
# not what the self-drive stops; it is what the fabrication BUYS.
SELF_DRIVE_REDDENS=(
	gofmt
	shellcheck
	citations
	doc-numbers
	vet
	t1
	t2
)
SELF_DRIVE_REDDENS_N=7

# The preservation control, in the same run and against the same plant. Without
# it the self-drive is satisfied by an arbiter that reddens everything, which is
# a check with one possible verdict.
SELF_DRIVE_SURVIVES=(
	self-check
	bounds
	build
	gate-roster
	unit-suite
)
SELF_DRIVE_SURVIVES_N=5

# The wall-clock floor, in seconds, under the oracle's own run.
#
# ROUND 11, and it is the only operand here that binds WORK rather than
# reporting. Everything else in this file asks "is it there" or "did you say
# so". A fake oracle that prints a correct-looking account returns instantly;
# a real one copies the tree once per scenario and runs a race-enabled suite in
# each copy. MEASURED 2026-08-30 on this box, this tree: 262s.
#
# BOUND, and it is weak on purpose: this is a floor against an INSTANTANEOUS
# stub, not proof of work. A fabricator that sleeps defeats it. It is here
# because the cheap edit should not be the quiet one, which is round 8's
# lesson, not because it is hard to get past.
#
# verify.sh checks it LAST, after every content check, so it can never displace
# a truer diagnosis. Scenario oracle-too-fast.
#
# ROUND 13, N12. The floor was the literal 8 standing two lines under a
# measurement of 175 — a number with no stated relationship to the thing it
# bounds, which is §0.2 with the measurement sitting right there. It is now
# DERIVED from that measurement at a stated fraction, and manifest_check
# refuses the two drifting apart.
#
# Why 5% and not more, stated as a trade rather than a preference: the floor is
# paid, in wall clock, by every scenario that has to sleep past it — three of
# them today, each sleeping the floor plus one second. Raising the fraction
# raises that cost linearly, to catch a fabricator that is already free to
# sleep for as long as the floor demands. The floor buys "the cheap edit is not
# the quiet one"; it does not buy proof of work, and no fraction of a
# measurement can.
# How many lines of prose in README.md and docs/*.md may carry a bare number.
#
# ROUND 13, N13. `doc-numbers --check` used to print this count and compare it
# to nothing, so a new derived number — one no pattern in the sweep
# recognises — moved the count and was seen by nobody. MEASURED 2026-08-30 by
# review: a line carrying four live instrument-owned numbers passed.
#
# It is a CEILING, not an equality: prose that says "two" in words, a version
# pin, or a quoted sample can be added without an edit here, and the count
# falling is not a failure. Going over it prints the whole enumeration.
#
# The Go pin holds it from above (docNumberCeilingCap), because the cheap way
# to make this row stop saying anything is to raise the ceiling rather than
# delete the number.
DOC_NUMBER_CEILING=64

ORACLE_MEASURED_SECONDS=262
ORACLE_MIN_PERCENT=5
ORACLE_MIN_SECONDS=$((ORACLE_MEASURED_SECONDS * ORACLE_MIN_PERCENT / 100))

# The oracle's scenarios. The oracle no longer holds this list; it cross-checks
# its sc_* functions against this file, and verify.sh requires the oracle's
# output to account for every name here BY NAME.
MANIFEST_SCENARIOS=(
	control
	verdict-on-abort
	verdict-without-gomod
	roster-gate-deleted
	roster-gate-added
	t1-violation
	t2-violation
	gofmt-violation
	vet-violation
	race-detector
	test-cache
	ceiling-fires
	ceiling-control
	ceiling-band
	gate-panic
	gate-refuses
	self-drive-blinded
	self-drive-reddens-everything
	scenario-death-is-reported
	doc-number-reintroduced
	doc-sweep-deleted
	unlinted-script
	unlinted-shebang-script
	oracle-is-invoked
	hang-bounded
	bounds-ordering
	suite-timeout-detached
	stale-citation
	citation-trailing
	citation-underscore
	citation-whitewash
	citation-vacuous
	citation-url
	citation-after-url
	invoked-by-relative-path
	suite-args-detached
	suite-tests-disabled
	suite-one-package-disabled
	suite-domain-unmeasured-module
	suite-domain-unmeasured-walk
	suite-files-disabled-partial
	suite-roster-unmeasured
	record-refuses-uncounted-pass
	record-refuses-zero-count
	row-deleted
	row-added
	oracle-stub-total
	oracle-stub-partial
	citation-embedded-identifier
	citation-word-start
	go-domain-empty
	manifest-missing
	manifest-row-removed
	manifest-count-lies
	manifest-scenario-removed
	self-check-guard-deleted
	min-declared-tests-floor
	oracle-names-fabricated
	oracle-too-fast
	scenario-body-emptied
	observation-recorder-stubbed
	min-declared-tests-margin
	silent-scenario-named
)
MANIFEST_SCENARIOS_N=63

# What each scenario must OBSERVE. One entry per scenario, same order.
#
# ROUND 11, and it is the round's whole answer. MEASURED 2026-08-30 by review:
# four scenario BODIES were emptied with their NAMES kept, record()'s guard was
# made inert, self_check() was gutted to report PASS unconditionally, and one
# comment was left in place because the row-coverage check grepped the oracle's
# own source for it. `VERDICT: PASS (12 steps)` with a live defect in the tree,
# twice, with every operand in this file satisfied IN FULL.
#
# The diagnosis is one line: A NAME IS NOT A BEHAVIOUR. Everything above
# answers "is it there"; nothing answered "does it do anything".
#
# Format:  name|rc-class|observation-token|diagnosis
#   rc-class    zero     the subject must have exited 0 in this scenario
#               nonzero  the subject must have exited non-zero
#               static   the scenario runs the subject not at all (see below)
#   token       <row>:<PASS|FAIL|ABSENT> — a row the scenario must have READ,
#               in that state, in the subject's verdict table.
#   diagnosis   a substring of the NOTE the arbiter wrote beside that row,
#               squashed to letters, spaces and # (see `squash` in the
#               oracle). "no row" for a static contract, which reads none.
#
# ROUND 13, B15. The token names the row that must go red; the diagnosis names
# WHY it went red, and the arbiter — not the scenario — writes it. That is the
# whole of the round: a contract that pins the outcome and not the cause is
# satisfied by a scenario reddening the right row for the wrong reason, which
# is how four bodies were substituted in round 12 and still passed.
#
# The tokens come from the helpers — `row`, `run_verify`, `run_verify_outer` —
# which are the only ways to run the subject or read its table, so a body
# cannot opt out of being observed. verify.sh compares what the oracle reports
# against what is written here; the check lives in neither the scenario nor the
# oracle.
#
# The consequence is the point: with the contract checked outside the body, a
# body's `note` calls stop being the assertion. Emptying a body no longer
# deletes the assertion, it deletes the OBSERVATION, and no observation is a
# failure.
#
# BOUNDS, stated because a completeness claim here would be this project's
# fifth in five rounds:
#   - The contract pins the outcome AND the diagnosis, but the diagnosis is a
#     SUBSTRING of the arbiter's note. Two plants the arbiter describes with
#     the same words are still indistinguishable here — how narrow that is
#     depends on how specific the arbiter's notes are, which is why round 13
#     rewrote the generic ones (`exit $rc` alone, the roster mismatch) rather
#     than tightening the contract around them.
#   - A driver rewritten to FABRICATE these strings defeats it. To do that it
#     must read this table, i.e. reproduce the expectation it is faking. That
#     is the terminus, and it is a person reading a diff.
#   - `static` is an escape hatch, and its membership is ENUMERATED below in
#     MANIFEST_STATIC_CONTRACTS rather than described here, because an
#     uncapped exemption is how a class spreads and a described one drifts.
#     This paragraph used to read "capped at ONE, its one member
#     ceiling-band" while the Go pin said 2 and the table held two: it was
#     wrong from the moment the second member landed, and nothing could see
#     it, because it was prose. The list below is checked for set equality
#     against the contracts, so adding a static contract without declaring it
#     now fails the suite instead of falsifying a sentence.
MANIFEST_SCENARIO_CONTRACTS=(
	"control|zero|verify-oracle:ABSENT|row absent"
	"verdict-on-abort|nonzero|gate-roster:ABSENT|row absent"
	"verdict-without-gomod|nonzero|gate-roster:FAIL|go list could not enumerate the gates"
	"roster-gate-deleted|nonzero|gate-roster:FAIL|MISSING from the tree"
	"roster-gate-added|nonzero|gate-roster:FAIL|UNDECLARED in the manifest"
	"t1-violation|nonzero|t1:FAIL|VIOLATION"
	"t2-violation|nonzero|t2:FAIL|VIOLATION"
	"gofmt-violation|nonzero|gofmt:FAIL|unformatted proto ugly go"
	"vet-violation|nonzero|vet:FAIL|unreachable code"
	"race-detector|nonzero|unit-suite:FAIL|WARNING DATA RACE"
	"test-cache|nonzero|unit-suite:FAIL|go test reported a cached result"
	"ceiling-fires|nonzero|unit-suite:FAIL|passed but took"
	"ceiling-control|zero|unit-suite:PASS|declared test s all ran across"
	"ceiling-band|static|ceiling-seconds:60|no row"
	"gate-panic|nonzero|t2:FAIL|with no REFUSED line the gate crashed"
	"gate-refuses|nonzero|t1:FAIL|REFUSED the gate could not measure its domain"
	"self-drive-blinded|nonzero|self-drive:FAIL|gofmt PASS planted did not redden"
	"self-drive-reddens-everything|nonzero|self-drive:FAIL|build FAIL unplanted went red"
	"scenario-death-is-reported|static|scenario-death:died before reporting and not a subject failure|no row"
	"doc-number-reintroduced|nonzero|doc-numbers:FAIL|Delete the number and name the instrument"
	"doc-sweep-deleted|nonzero|doc-numbers:FAIL|is missing or not executable"
	"unlinted-script|nonzero|shellcheck:FAIL|scripts extra sh"
	"unlinted-shebang-script|nonzero|shellcheck:FAIL|scripts preflight"
	"oracle-is-invoked|nonzero|verify-oracle:FAIL|oracle stub was invoked"
	"hang-bounded|nonzero|unit-suite:FAIL|test timed out"
	"bounds-ordering|nonzero|bounds:FAIL|does not exceed the"
	"suite-timeout-detached|nonzero|bounds:FAIL|the suite flags do not carry timeout"
	"stale-citation|nonzero|citations:FAIL|TestThisCitationWasNeverWritten"
	"citation-trailing|nonzero|citations:FAIL|TestTrailingCitationNeverWritten"
	"citation-underscore|nonzero|citations:FAIL|BenchmarkNeverWrittenEither"
	"citation-whitewash|nonzero|citations:FAIL|TestWhitewashedByAStringLiteral"
	"citation-vacuous|nonzero|citations:FAIL|a scan that finds no domain is not a passing scan"
	"citation-url|zero|citations:PASS|cited token s all declared among"
	"citation-after-url|nonzero|citations:FAIL|TestRevCPhantomAfterAURL"
	"invoked-by-relative-path|zero|bounds:PASS|and the one suite invocation expands SUITE ARGS"
	"suite-args-detached|nonzero|bounds:FAIL|suite invocation s expanding SUITE ARGS expected exactly"
	"suite-tests-disabled|nonzero|unit-suite:FAIL|reported no ok package line"
	"suite-one-package-disabled|nonzero|unit-suite:FAIL|hold a test go file and ran no test"
	"suite-domain-unmeasured-module|nonzero|unit-suite:FAIL|its domain is UNMEASURED module ##"
	"suite-domain-unmeasured-walk|nonzero|unit-suite:FAIL|its domain is UNMEASURED module github"
	"suite-files-disabled-partial|nonzero|unit-suite:FAIL|declared but never run"
	"suite-roster-unmeasured|nonzero|unit-suite:FAIL|the declared test roster is UNMEASURED"
	"record-refuses-uncounted-pass|nonzero|gofmt:FAIL|no numeric domain size"
	"record-refuses-zero-count|nonzero|gofmt:FAIL|examined # items an empty domain"
	"row-deleted|nonzero|vet:ABSENT|row absent"
	"row-added|nonzero|undeclared-row:PASS|invented"
	"oracle-stub-total|nonzero|verify-oracle:FAIL|printed no ORACLE PASS"
	"oracle-stub-partial|nonzero|verify-oracle:FAIL|verify manifest sh declares"
	"citation-embedded-identifier|zero|citations:PASS|cited token s all declared among"
	"citation-word-start|nonzero|citations:FAIL|TestRevCWordStartNeverWritten"
	"go-domain-empty|nonzero|build:FAIL|an empty domain is not a passing domain"
	"manifest-missing|nonzero|citations:ABSENT|row absent"
	"manifest-row-removed|nonzero|unit-suite:FAIL|TestManifestRowsAreTheRowsPinnedHere"
	"manifest-count-lies|nonzero|citations:ABSENT|row absent"
	"manifest-scenario-removed|nonzero|unit-suite:FAIL|TestManifestFloorsAreNotBelowTheirPins"
	"self-check-guard-deleted|nonzero|self-check:FAIL|record is not enforcing its contract"
	"min-declared-tests-floor|nonzero|unit-suite:FAIL|below the floor of"
	"oracle-names-fabricated|nonzero|verify-oracle:FAIL|reported no passing result for scenario"
	"oracle-too-fast|nonzero|verify-oracle:FAIL|floor in verify manifest sh it reported the right account without doing the work"
	"scenario-body-emptied|nonzero|verify-oracle:FAIL|record refuses uncounted pass wants nonzero"
	"observation-recorder-stubbed|nonzero|verify-oracle:FAIL|control wants zero verify oracle ABSENT"
	"min-declared-tests-margin|nonzero|unit-suite:FAIL|tests were ADDED"
	"silent-scenario-named|nonzero|verify-oracle:FAIL|Silent control"
)
MANIFEST_SCENARIO_CONTRACTS_N=63

# The `static` exemption, enumerated. A scenario is static when it does not run
# verify.sh at all, so it can read no row of the subject's table; both members
# still have to observe something derived from what they DID run.
#
#   ceiling-band                reads SUITE_CEILING_SECONDS out of verify.sh
#                               and observes the value.
#   scenario-death-is-reported  runs the ORACLE in a copy, not verify.sh, and
#                               observes the child's own death line.
#
# Set equality against the contract table is pinned from Go, so this list
# cannot describe a membership the table does not have.
MANIFEST_STATIC_CONTRACTS=(
	ceiling-band
	scenario-death-is-reported
)
MANIFEST_STATIC_CONTRACTS_N=2


# manifest_check — layer 2, run by every reader of this file BEFORE it is
# trusted. A list that has been shortened without its count being edited, or a
# list that has been emptied, is a refusal rather than a smaller domain.
#
# It prints one line and returns non-zero on failure; it does not exit, because
# its two callers report a refusal in two different formats.
manifest_check() {
	local bad=""
	[ "${#MANIFEST_ROWS[@]}" -eq "$MANIFEST_ROWS_N" ] ||
		bad="$bad MANIFEST_ROWS has ${#MANIFEST_ROWS[@]} name(s), MANIFEST_ROWS_N says $MANIFEST_ROWS_N;"
	[ "${#MANIFEST_GATES[@]}" -eq "$MANIFEST_GATES_N" ] ||
		bad="$bad MANIFEST_GATES has ${#MANIFEST_GATES[@]} name(s), MANIFEST_GATES_N says $MANIFEST_GATES_N;"
	[ "${#MANIFEST_SHELL_SCRIPTS[@]}" -eq "$MANIFEST_SHELL_SCRIPTS_N" ] ||
		bad="$bad MANIFEST_SHELL_SCRIPTS has ${#MANIFEST_SHELL_SCRIPTS[@]} name(s), MANIFEST_SHELL_SCRIPTS_N says $MANIFEST_SHELL_SCRIPTS_N;"
	[ "${#MANIFEST_SCENARIOS[@]}" -eq "$MANIFEST_SCENARIOS_N" ] ||
		bad="$bad MANIFEST_SCENARIOS has ${#MANIFEST_SCENARIOS[@]} name(s), MANIFEST_SCENARIOS_N says $MANIFEST_SCENARIOS_N;"
	[ "${#MANIFEST_SCENARIO_CONTRACTS[@]}" -eq "$MANIFEST_SCENARIO_CONTRACTS_N" ] ||
		bad="$bad MANIFEST_SCENARIO_CONTRACTS has ${#MANIFEST_SCENARIO_CONTRACTS[@]} entr(ies), MANIFEST_SCENARIO_CONTRACTS_N says $MANIFEST_SCENARIO_CONTRACTS_N;"
	[ "${#MANIFEST_STATIC_CONTRACTS[@]}" -eq "$MANIFEST_STATIC_CONTRACTS_N" ] ||
		bad="$bad MANIFEST_STATIC_CONTRACTS has ${#MANIFEST_STATIC_CONTRACTS[@]} name(s), MANIFEST_STATIC_CONTRACTS_N says $MANIFEST_STATIC_CONTRACTS_N;"
	[ "${#MANIFEST_SCENARIO_CONTRACTS[@]}" -eq "${#MANIFEST_SCENARIOS[@]}" ] ||
		bad="$bad ${#MANIFEST_SCENARIOS[@]} scenario(s) but ${#MANIFEST_SCENARIO_CONTRACTS[@]} contract(s); a scenario with no contract is a name with no behaviour, which is exactly what round 11 closed;"
	# An empty list is the shape every one of the four rounds above ended in.
	[ "$MANIFEST_ROWS_N" -ge 1 ] || bad="$bad MANIFEST_ROWS_N is not positive;"
	[ "$MANIFEST_GATES_N" -ge 1 ] || bad="$bad MANIFEST_GATES_N is not positive;"
	[ "$MANIFEST_SHELL_SCRIPTS_N" -ge 1 ] || bad="$bad MANIFEST_SHELL_SCRIPTS_N is not positive;"
	[ "$MANIFEST_SCENARIOS_N" -ge 1 ] || bad="$bad MANIFEST_SCENARIOS_N is not positive;"
	[ "$MIN_DECLARED_TESTS" -ge 1 ] || bad="$bad MIN_DECLARED_TESTS is not positive;"
	# The floor and the measurement it comes from, held together. A literal
	# reinstated here in place of the derivation fails as soon as it drifts.
	[ "$ORACLE_MIN_SECONDS" -eq "$((ORACLE_MEASURED_SECONDS * ORACLE_MIN_PERCENT / 100))" ] ||
		bad="$bad ORACLE_MIN_SECONDS is $ORACLE_MIN_SECONDS but ORACLE_MEASURED_SECONDS=$ORACLE_MEASURED_SECONDS at $ORACLE_MIN_PERCENT percent is $((ORACLE_MEASURED_SECONDS * ORACLE_MIN_PERCENT / 100)); the floor no longer derives from the measurement printed beside it;"
	# A margin the tree can widen at will is a floor with no upper edge, which
	# is the state round 11 was sent to fix.
	[ "${#SELF_DRIVE_REDDENS[@]}" -eq "$SELF_DRIVE_REDDENS_N" ] ||
		bad="$bad SELF_DRIVE_REDDENS has ${#SELF_DRIVE_REDDENS[@]} name(s), SELF_DRIVE_REDDENS_N says $SELF_DRIVE_REDDENS_N;"
	[ "${#SELF_DRIVE_SURVIVES[@]}" -eq "$SELF_DRIVE_SURVIVES_N" ] ||
		bad="$bad SELF_DRIVE_SURVIVES has ${#SELF_DRIVE_SURVIVES[@]} name(s), SELF_DRIVE_SURVIVES_N says $SELF_DRIVE_SURVIVES_N;"
	[ "$SELF_DRIVE_REDDENS_N" -ge 1 ] && [ "$SELF_DRIVE_SURVIVES_N" -ge 1 ] ||
		bad="$bad a self-drive with an empty half is a check with one possible verdict;"
	[ "$MAX_DECLARED_MARGIN" -ge 0 ] && [ "$MAX_DECLARED_MARGIN" -le 4 ] ||
		bad="$bad MAX_DECLARED_MARGIN is $MAX_DECLARED_MARGIN, outside 0..4; a wide band is a floor that has stopped saying anything;"
	[ "$ORACLE_MIN_SECONDS" -ge 1 ] || bad="$bad ORACLE_MIN_SECONDS is not positive;"
	[ "$DOC_NUMBER_CEILING" -ge 1 ] || bad="$bad DOC_NUMBER_CEILING is not positive;"
	[ "$MANIFEST_SCENARIO_CONTRACTS_N" -ge 1 ] || bad="$bad MANIFEST_SCENARIO_CONTRACTS_N is not positive;"
	if [ -n "$bad" ]; then
		printf 'the manifest does not agree with itself:%s\n' "$bad"
		return 1
	fi
	return 0
}
