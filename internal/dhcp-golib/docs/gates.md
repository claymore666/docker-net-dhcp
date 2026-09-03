# The M0 gates, and what each one cannot see

Written at M0, against an empty package, before any protocol code exists. A
gate added after the code it guards gets weakened to fit the code; these were
added first so that any inconvenience shows up now, when it is cheapest.

Nothing below is a completeness claim. Each section states a bound and names
the escape beside it.

Run everything with:

    ./verify.sh

Exit 0 is PASS and is the normal state. Exit 1 is FAIL. A step that cannot be
measured is a FAIL, never a skip.

---

## T1 — ring 1 imports nothing that does I/O

`internal/gates/t1`. Policy lives in `internal/gates/rings`.

Ring 1 (`proto/`) is the state machine and it is pure: `Step(now, rnd, ev)`
takes time and entropy as parameters instead of reading them. That is what
makes the suite instant and offline replay bit-exact. The day ring 1 can read
a clock or open a socket, both properties are gone and no test will say so —
which is why this is a gate and not a comment.

Ring 0 (`wire/`) is held to the same policy, because ring 1 imports it and an
impure ring 0 makes ring 1 impure transitively.

### Four rules

| Rule | What it does | Blind to |
|---|---|---|
| A | Transitive dependency closure via `go list -deps`; a pure ring may depend on no module-internal package outside the pure rings | Files excluded by a build constraint |
| B | Direct imports of **every** `.go` file under a pure ring root, parsed with `go/parser`, build constraints ignored | Anything transitive |
| C | Identifier-level: only allowlisted identifiers of a restricted package may be named | Indirection — a variable holding `fmt.Println` |
| D | Non-vacuity: every declared ring root must exist and hold a non-test `.go` file | A root that exists and is populated but is no longer the real ring |

**A is not an independent second instrument today and must not be described as
one.** Because B scans every file under every pure root, and a pure ring may
currently depend on nothing but another pure root, A's module-internal findings
are a subset of B's. A becomes load-bearing the day a pure ring is allowed a
dependency B does not scan — a third-party package, or a module-internal
package outside the pure roots — because B sees only the import line while A
sees what that import drags in.

**Allowlists, not denylists.** The requirement is written as "ring 1 imports no
`net`, `time`, `os`, `context` or `syscall`", and implementing that literally
would be a denylist keyed on today's spelling, admitting everything nobody
thought to name. `rings.PureStdlib` is the complete set of standard-library
packages a pure ring may import; anything else is refused by default.

Two consequences of that, worth stating rather than discovering:

- `net/netip` is admitted and `net` is not. netip is value types with no
  resolver, no socket and no ambient state. A denylist written as `== "net"`
  would have admitted `net/http`; one written as a `net` prefix would have
  banned netip.
- `fmt` is admitted, because `Errorf` and `Sprintf` are unavoidable, and it is
  the one admitted package that can perform I/O. Rule C closes that by
  restricting which `fmt` identifiers a pure ring may name — `Println` and the
  `Fprint`/`Scan` families are absent.

### What T1 cannot see

1. **Indirection.** `var p = fmt.Println` in a non-pure package, called from
   ring 1 through a function value or an interface. Rule C reads selector
   expressions, not the call graph.
2. **`unsafe`, `go:linkname` and assembly.** A `.s` file or a linkname
   directive reaches anything without an import. Not scanned at all.
3. **Impurity that is not an import.** A package-level `var` initialised from
   a global, a `func init()` with a side effect inside an allowlisted package,
   a map iterated in nondeterministic order. T1 is a claim about the import
   set; it is not a proof of determinism.
4. **Generated code, before it is written to disk.** `go:generate` output is
   checked once it exists as a file, and not before.
5. **`_test.go` files under a pure root.** Deliberate: T1 is a claim about the
   package, not about its tests. A ring-1 test reading a golden file does not
   make the state machine impure. T2 governs test files.
6. **A pure stdlib package that stops being pure.** The allowlist is a set of
   names checked against a policy decision made by a person. If a future Go
   release gives `strconv` a background goroutine, T1 will not notice.
7. **The ring layout itself.** Rule D checks that the four roots exist and are
   populated. It cannot check that `proto/` still contains the state machine.
8. **Files under `testdata/`, `vendor/` and `.git/`.** Both gates share
   `scan.GoFiles`, which skips them. For `testdata/` the reason is the same one
   T2 gives: the go tool does not compile it, so nothing there is part of any
   binary, and it is where the gates' own deliberate violations live. This was
   documented for T2 and not for T1 until 2026-08-29, although the two gates
   have always walked the tree with the same function.
9. **Its live domain at M0 is inert.** MEASURED 2026-08-29: `proto/doc.go` and
   `wire/doc.go` are doc comments with no imports, so on the real tree rules B
   and C judge zero import lines and rule A a closure of two packages — the two
   rings themselves. Rule D is what stops that from being reported as a vacuous
   pass; the rules are actually exercised in `internal/gates/t1`'s own cases,
   against planted violations. T2 states the equivalent bound as item 4 below,
   and T1 did not state it at all until this line was written.

## T2 — no test waits on wall-clock time

`internal/gates/t2`.

This library has no CI and, per the build plan, will not get any: the
self-hosted runners belong to the plugin repository and cannot serve a second
private repo without an organisation. The only thing that runs this suite is a
person running it. A suite slow enough to avoid is a suite that is not run, and
then nothing observes anything.

The older half of the reason: a test that waits gets "fixed" by waiting
longer, and a longer timeout is how a real user-facing failure hid in the v1.x
plugin for months.

### Two rules, in two different instruments

- **Identifier allowlist**, in the gate. In every `_test.go` file, a reference
  to a restricted package must name an identifier on that package's allowlist.
  `time.Sleep`, `After`, `Tick`, `NewTimer`, `NewTicker` and `AfterFunc` are
  absent from it, as is anything the standard library adds later. Aliases are
  resolved from each file's own import declarations, so `import clk "time"`
  then `clk.Sleep` is caught. `context.WithTimeout` and `WithDeadline` are
  restricted too: a deadline on a context is a wall-clock wait under another
  name, because whatever blocks on `ctx.Done()` is blocking until a timer
  fires.
- **A wall-clock ceiling on the suite**, in `verify.sh`, currently 60s. This is
  a genuinely different instrument: the gate reads source, the ceiling reads
  the clock, so a wait the gate cannot see still costs time here.

`time.Now`, `time.Since` and `time.Until` **are** allowed. They read the clock;
they do not wait on it, and T2's subject is waiting.

### What T2 cannot see

1. **A wait that names no restricted package at all.** `exec.Command("sleep")`,
   `syscall.Nanosleep`, a blocking channel receive whose sender is on a timer
   in non-test code, `runtime.Gosched` in a spin. The identifier allowlist has
   no opinion about packages it does not restrict.
2. **A wait built entirely out of ALLOWLISTED identifiers**, which is a
   different and worse case than (1), because the heading above does not cover
   it. `for time.Since(start) < 50*time.Millisecond {}` names `time.Since` and
   `time.Millisecond`, both allowed, and both *correctly* allowed: reading the
   clock is not waiting on it, right up until the read is a loop condition.
   Distinguishing the two is control-flow analysis, not an identifier check.
   MEASURED 2026-08-29: `scripts/test-verify.sh` plants exactly this loop, and
   T2 passes it — the scenario asserts that it does, because the loop is there
   to drive the wall-clock ceiling and the ceiling is only being measured if
   T2 stayed out of the way.
   This bullet and (1) used to be one bullet headed "a wait that is not a
   `time` or `context` identifier", with the busy loop listed underneath it.
   The busy loop is built from nothing but `time` identifiers, so the heading
   denied the case it was filed under.
3. **The ceiling only holds AT the threshold.** MEASURED 2026-08-29: a planted
   `time.Sleep(50 * time.Millisecond)` in a test did not move the suite from
   1s against a 60s ceiling. The ceiling catches a suite that has drifted into
   waiting; it does not catch one test that waits a little.
3a. **And the ceiling cannot bound a HANG at all.** It is computed from a clock
   read after `go test` returns, so a test that never returns never reaches the
   comparison — the check sits behind a branch the failure it would name can
   never take. That was written here as though the ceiling covered blocking
   tests; it does not, and never did. What bounds a hang is
   `SUITE_TIMEOUT_SECONDS` on the `go test` line, added 2026-08-29 and driven
   by the `hang-bounded` oracle scenario. Two bounds on the bound itself:
   running `go test` by hand outside `verify.sh` gets Go's default of ten
   minutes per test binary instead, settable from the environment; and the
   absence check for this one shows up as the ORACLE hanging rather than as a
   red row — MEASURED 2026-08-29, the scenario returns in seconds with the flag
   and had to be killed at 100s without it. Loud to a person watching, silent
   to any caller that does not impose a timeout of its own.
4. **A wait inside a helper in non-test code**, called from a test. T2's domain
   is `_test.go` files. That is deliberate — ring 3 has a real clock in it —
   and it means a test can wait by delegating.
5. **Its domain was the gates' own self-tests at M0, and is the library's
   tests from M1 on.** That bullet used to end "it will not be guarding any
   protocol test until M1"; M1 landed on 2026-08-29 and it now checks every
   test file in `wire`, `proto`, `lease` and `runtime`. Two of those tests
   BLOCK — one on a real dnsmasq's log line, one spinning on a counter with
   `runtime.Gosched` — and T2 sees neither, by bullets (1) and (4). What bounds
   them is the `go test -timeout` in `verify.sh`, per (3a). It is NOT the suite
   ceiling: this bullet said the ceiling and the ceiling is unreachable for
   exactly the tests it was claiming to cover.
6. **Files under `testdata/`.** Not walked, because the go tool does not
   compile them, so a file there is not part of any test binary. It is also
   where the gates' own deliberate violations live.
7. **Shadowing, in the loud direction.** A local variable named `time` would
   make the gate report a false positive. That direction is deliberate: a gate
   that refuses something innocent is loud, and one that misses something is
   not.
8. **A third-party dependency that sleeps.** There are none today; the gate has
   no view into module cache source.

## verify.sh

One command, every check, one verdict. Details that are not incidental:

- It **builds** the gate binaries and executes them rather than using
  `go run`. MEASURED 2026-08-29: `go run` collapses every non-zero child
  status to 1, which would make a gate REFUSING because it could not measure
  its domain indistinguishable from a gate reporting a violation. The gates
  return 0/1/2 precisely so those are different facts.
- The required gate set is enumerated in the script and cross-checked against
  the gate commands the go tool finds, **in both directions**. A required gate
  that has been deleted is a FAIL; a gate present in the tree but missing from
  the list is also a FAIL. A verifier that discovers its own checklist can be
  silenced by deleting a check.
- **The verdict is printed by an EXIT trap, not by the last line.** MEASURED
  2026-08-28 by review: one unprotected assignment took `set -e` with it, so
  deleting `go.mod` made the verifier exit 1 having printed no verdict at all —
  silent in the one case where the tree was most broken. That assignment is
  also fixed, but the promise "one command, one verdict" is a property of the
  file, so it is now held at a place every exit path passes.
- **Exit 2 is disambiguated.** A Go panic exits 2 and so does a deliberate
  REFUSE. Both are a FAIL, so this was never a correctness hole; but a crash
  reported as "could not measure its domain" sends the reader to the wrong
  place, so the gates' `REFUSED` line is read before the diagnosis is chosen.
- **`-count=1` has an observer.** A cached PASS is a result that was not
  measured on this tree and is indistinguishable from a real one in the exit
  status, so a `(cached)` marker in the suite output is itself a FAIL.

### verify.sh has an oracle

`scripts/test-verify.sh`. It copies the tree, plants ONE defect in the copy,
runs the copy's `./verify.sh --inner`, and asserts both that the run failed and
that **the row which failed is the row that owns the defect**. Attributing the
failure to the row is the same lesson as the fixture-path finding below: a run
that fails for the wrong reason looks exactly like one that fails for the right
one. `verify.sh` runs it as a step, so the verifier is checked by the command
that runs the verifier.

The scenario roster is `MANIFEST_SCENARIOS` in `verify.manifest.sh`, cross-
checked in both directions against the `sc_*` functions in the oracle. Read it
there, not here: a prose copy of a list the script already enforces is an unrun
checklist, and this paragraph was one — it enumerated the scenarios as they
stood before `hang-bounded`, `bounds-ordering` and `stale-citation` were added.

It lived in the oracle until 2026-08-30, beside the functions it was checked
against, which meant deleting a name and its function together was consistent
and silent. That is the round-9 finding and the manifest section below is the
answer.

**Every step's DELETION was driven, not assumed.** MEASURED 2026-08-29 by
removing one step at a time from a copy of `verify.sh` and running the oracle
against that copy. The table covers the nine steps that existed at that
measurement; `citations` and `bounds` were added later the same day, each
driven by its own absence check at introduction — delete the step from a copy,
watch its scenario report ABSENT — rather than by a re-run of this sweep:

| step deleted from `verify.sh` | oracle scenarios that went red |
|---|---|
| `build`     | 1 (`vet-violation`, whose bait must compile) |
| `vet`       | 1 (`vet-violation`) — **0 before that scenario existed** |
| `gofmt`     | 4 |
| `shellcheck`| 2 |
| `gate-roster` | 3 |
| the `t1`/`t2` gate loop | 6 |
| `unit-suite` | REFUSED — a scenario died without reporting |
| `verify-oracle` | 4, but see the bound below |

`vet` is why this sweep is in the document rather than in a transcript. It was
the one step in the file that no scenario drove: with `step "vet" go vet ./...`
deleted, the oracle passed **18 of the 18 scenarios that existed then**. A step
nothing drives is a step that
can be deleted, which is the defect class of every finding this project has
paid for twice.

The `vet-violation` bait is unreachable code, and the choice is about
attribution rather than convenience. `go test` runs a vet subset of its own
(atomic, bool, buildtags, directive, errorsas, ifaceassert, nilfunc, printf,
stringintconv, tests), so a `printf` bait would redden the unit-suite row too
and prove nothing about which step saw it. `unreachable` is in `go vet` and not
in that subset, so the scenario can assert `build`, `gofmt` and `unit-suite`
all still PASS — the preservation control that makes the vet row's FAIL
attributable.

**The oracle also checks that `verify.sh` still runs it.** MEASURED 2026-08-29:
replacing the oracle step with a hardcoded PASS survived every other scenario,
and had to — this script is the *control* for those mutants, so `verify.sh`
dropping the step is invisible from inside it. That is the same defect class as
both blocking findings: a check that cannot see its own domain. It is now
driven by replacing the copy's oracle with a stub and running the copy's
`verify.sh` with **no** flag: the stub answers instead of recursing, this
scenario chooses its answer, and both directions are asserted — a passing stub
must produce a PASS row that quotes the stub, and a failing stub must fail the
run.

**This section used to say the opposite, and the reason it gave was wrong.** It
said verify.sh could have no automated test because the harness would run
`verify.sh` against a mutated copy and the copy would run the harness again.
That obstacle is real but specific to writing the harness as a **`go test`**:
`verify.sh` runs `go test ./...`, so a Go test that ran `verify.sh` re-enters it
with nowhere to put a flag. As a standalone script with an explicit `--inner`
on the inner invocation, there is no recursion to break. The correction is on
the record because a false justification is worse than an admitted gap: the gap
gets closed, the justification gets believed.

`--inner` is a flag and not an environment variable on purpose. An ambient
variable silences the oracle for anyone who happens to have it set; a flag has
to be typed into the invocation you are reading.

### What the oracle cannot see

0. **`verify.sh` no longer calling the oracle.** The row above says 4 scenarios
   catch it, and that number is honest but the mechanism is not what it looks
   like: the catch is `shellcheck` objecting that deleting the step left
   `--inner`'s variable unused. MEASURED 2026-08-29, composing past that single
   objection — one `disable=SC2034` and a `: "$INNER"` — the copy prints
   a PASS verdict with the arbiter's own arbiter silently gone. This
   is inherent and not fixable from inside: a `verify.sh` that drops the step
   never runs the scenario that checks the step is there. Running
   `scripts/test-verify.sh` by hand is the only check for it, and running it by
   hand is not a wired check. Recorded because an incidental catch is the
   easiest thing in this file to mistake for a designed one.
1. **A defect nobody planted.** It is a list of scenarios, not a proof. The
   scenario list is cross-checked in both directions against the `sc_*`
   functions in the file, so removing a name is a REFUSAL rather than a quiet
   shrinkage — but removing the name **and** its function together is
   consistent and invisible. The cheap edit is loud; the expensive one is not.
2. **The ceiling's VALUE, behaviourally.** Driving it needs a suite lasting
   between the shipped ceiling and a raised one, i.e. minutes of wall clock per
   run. The oracle instead drives the ceiling's *branch* (a 3s suite against a
   ceiling lowered in the copy must FAIL, and the same suite against the
   shipped ceiling must PASS — that second one is the control, without which
   the first proves only that a busy loop breaks something), and checks the
   declared value is inside 5..120. The band check is STRUCTURAL and weaker
   than the rest; it kills "delete the line" and "raise it to 6000" and nothing
   subtler.
3. **`--inner` failing to SUPPRESS the oracle.** The other half — that the
   unflagged invocation still runs it — is driven by the stub scenario above.
   Suppression is asserted (the clean-copy scenario requires the inner run to
   have no `verify-oracle` row) but not mutation-driven, because that mutant is
   unbounded recursion and running it on a shared machine is not worth the
   evidence. MEASURED 2026-08-29 by hand instead, in both directions:
   `./verify.sh` reports exactly one step more than `./verify.sh --inner`, and
   the extra row is `verify-oracle`; the inner run has no such row. The counts
   themselves are not written here — they move with every step added.
4. **Two defensive arms that are unreachable today.** The `*)` unexpected
   exit-code arm — the gates return only 0/1/2 — and the "gate does not
   compile" arm, since a gate that fails to build fails `build`, `vet` and
   `unit-suite` first. Both are correct code and no test is owed; they are
   named here so a reader does not mistake them for gaps.

### Two steps added 2026-08-29, and what each cannot see

- **`citations`** fails the run when a Test/Benchmark/Fuzz/Example token
  appearing after the first `//` on a `.go` line that is not a URL scheme
  separator, or anywhere on a `.md` line, is not DECLARED by some `.go` line
  beginning `func`/`var`/`const`/`type`. It exists
  because converting a fact-comment into a pointer at a test is exactly how an
  invented test name gets written down and believed: one was invented during
  that conversion, and one already in the tree
  (`internal/gates/rings/policy_test.go`) named a test that had never existed.

  **This bullet described a stricter check than the code performed**, and a
  reviewer measured the gap with five planted trees: trailing comments, block
  comments, and names the token pattern did not reach — an underscore suffix,
  and benchmarks — all passed, and a genuinely
  stale citation was whitewashed whenever the same token appeared in a Go
  string literal anywhere in the tree — because "exists" meant "appears on a
  non-comment line". Four of the five are caught now, each with its own
  scenario: `citation-trailing`, `citation-underscore`, `citation-whitewash`,
  and `stale-citation`, whose plant is INDENTED so that narrowing the match
  back to column 0 kills it. `citation-vacuous` drives the other direction — a
  scan that finds no domain at all must FAIL rather than report that every
  citation resolved. The eight remaining bounds are listed beside the
  implementation in `verify.sh` and not restated here; the first is block
  comments, and none of them is a completeness claim.

  The seventh was added 2026-08-30 after a reviewer measured the direction this
  bullet had not: `citations` also produces a FALSE POSITIVE. A URL in an
  ordinary Go string literal was read as a comment, so
  a documentation URL whose last path segment is spelled like a test name
  failed the run over a token nobody cited.
  The `//` chosen is now the first one not preceded by `:`, which covers a
  scheme and nothing else — a `//` inside a string with no colon before it
  still reads as a comment. Driven in BOTH directions, because the risk in this
  fix is that it blinds the gate: `citation-url` plants the reviewer's exact URL
  and must leave the run green, and `citation-after-url` plants a real stale
  citation in a comment following a URL on the same line and must still fail.

  The eighth arrived 2026-08-30, the same way and from the author's own tree:
  the token pattern had no LEFT word boundary, so it matched inside an ordinary
  identifier. A comment on a function called `isTestFuncName` was read as
  citing a test by the embedded name, and the run failed over a token nobody
  wrote. The match now requires the character before the token not to be a
  letter, digit or underscore. Driven in both directions for the same reason
  the URL fix was — `citation-embedded-identifier` must stay green,
  `citation-word-start` must still go red — because a boundary rule that
  blinds the scan would satisfy the first and defeat the gate.
- **`bounds`** fails the run unless the `go test` hang timeout exceeds the
  suite ceiling AND the flags the suite actually runs with carry that timeout.
  The second half was missing, which is why this bullet is longer than its
  first version: comparing two constants declared a hundred lines above the
  `go test` line is adjacency, not a data dependency. MEASURED by a reviewer, a
  hardcoded `-timeout 90s` on the invocation survived both `bounds-ordering`
  and `hang-bounded`. The flags are one array now, the invocation expands it,
  `suite-timeout-detached` plants exactly that mutant.

  The residual bound this bullet used to end on — an invocation that stops
  using the array — is CLOSED as of 2026-08-30 rather than restated. It was
  wider than it read: such an invocation takes `-count=1` with it too, so it
  also defeats the cached-result check, and every row stays green while
  `bounds` prints that the suite runs with the checked flags. The step now
  reads `verify.sh`'s own source and requires exactly one suite invocation
  expanding `"${SUITE_ARGS[@]}"`; a check that reads its own source and cannot
  read it records FAIL rather than falling through to the PASS.
  `suite-args-detached` plants the detached invocation. What remains is a
  spelling check over one line: a second `go test` elsewhere in the file, or
  the array under another name, is outside it.
### The row contract, added 2026-08-30 (round 7)

Everything above describes what individual rows check. This describes what a
row is ALLOWED to conclude, and it is the only structural change in the file.

**A row cannot record PASS without stating how many things it examined.**
`record` is the single place a row is written; a `PASS` whose count is absent,
non-numeric or zero is rewritten to FAIL. `step()`, which previously recorded
PASS from `rc == 0`, now takes the domain size as a second operand and cannot
pass without it.

It is structural rather than one more guard because the same defect was found
three times, at three levels, in three consecutive review rounds, each time
only where somebody happened to look:

| level | what passed over an absent subject | found |
|---|---|---|
| the suite | all 22 `_test.go` files build-tagged out; 0 tests ran | round 5, by the author |
| the oracle | `scripts/test-verify.sh` replaced by `exit 0` | round 6, by review (B7) |
| the row roster | a `step` call replaced by `true`; `PASS (10 steps)` | round 7, by the author |

All three inherited one default: a command with nothing to do exits 0. Fixing
them one at a time was fixing instances of a class, and the class is what the
contract closes.

**What the contract does NOT do, stated because it is the whole residual.**
Nothing inside `verify.sh` can force a count to be DERIVED rather than written;
`record "build" PASS "ok" 1` satisfies it completely. That is closed from
outside, by a scenario that empties a row's domain and requires the row to go
red.

**This paragraph used to claim there was one such scenario per row. There was
one, for one row of eleven.** MEASURED 2026-08-30 by review, and it is worth
recording as a defect in its own right rather than fixing quietly: the sentence
described the evidence the design *needed*, and nothing checked that the
evidence existed. The `build` row in particular was named by exactly one
assertion in the whole oracle, and that assertion was a control inside the
`vet` scenario.

What exists now, row by row, and what does not:

| row | the scenario that empties its domain |
|---|---|
| `build`, `vet`, `gofmt` | `go-domain-empty` — every `.go` file deleted |
| `t1` | `gate-refuses` — a ring root deleted |
| `t2` | `go-domain-empty` — the walk finds no `_test.go` file |
| `citations` | `citation-vacuous` |
| `unit-suite` | `suite-tests-disabled`, and `min-declared-tests-floor` |
| `verify-oracle` | `oracle-stub-total`, `oracle-names-fabricated` |
| `gate-roster` | none — see below |
| `shellcheck` | none — see below |
| `bounds` | none — see below |
| `self-check` | none — its count is a literal, by construction |

The three with none are structural, not outstanding work, and saying so is the
point of listing them. `shellcheck`'s domain is the shell scripts of the tree,
and the tree cannot hold none of them — emptying it means deleting `verify.sh`.
`gate-roster`'s domain is `MANIFEST_GATES`, and emptying that is refused by
`manifest_check` before any row runs. `bounds` reads constants out of
`verify.sh`, so its domain is empty only when the file is. `self-check` probes
a fixed set of four cases; its evidence is `self-check-guard-deleted`, which
deletes what it probes.

**`MANIFEST_ROWS`** is cross-checked against the rows recorded, in both
directions, refusing on an empty roster. Scenarios `row-deleted`, `row-added`.

- **`verify-oracle`** takes its expectation from `verify.manifest.sh` — a file
  the oracle does not own — and reads the oracle's report against it. This
  sentence used to say the expectation came from the oracle's own SOURCE, the
  `sc_` function definitions; that stopped being true when round 9 moved the
  population into the manifest, and the sentence stayed. Requiring an
  `ORACLE PASS: <n> scenarios` line closes a total stub; requiring one
  `RESULT <name> PASS` line per declared scenario closes the partial stub the
  review named as its own remedy's bound. Scenarios `oracle-stub-total`,
  `oracle-stub-partial`, `oracle-names-fabricated`.

  **The bound this bullet used to state is now closed, and it is what round 11
  was about.** It read: *"this counts DEFINITIONS, so a scenario body emptied
  of its assertions is defined, counted, and says nothing."* Review B14 did
  exactly that — kept four names, deleted four bodies — and got
  `VERDICT: PASS` on twelve steps with a live defect in the tree. Every operand
  answered *is it there*; none answered *does it do anything*. What answers it
  now is `MANIFEST_SCENARIO_CONTRACTS`: see "Contracts, not names" below.
- **`build` and `gofmt` had no non-vacuity guard of their own** and were held
  by their neighbours reddening — adjacency, not a data dependency. MEASURED:
  `go build ./...` over zero packages exits **0**, `go vet ./...` exits **1**,
  which is why the two behaved differently under one caller. Both now count
  their domain like every other row. Scenarios `record-refuses-uncounted-pass`,
  `record-refuses-zero-count`.
- **`unit-suite`** gained a domain check on 2026-08-30, from the same review.
  MEASURED: `go test ./...` exits 0 on a tree with no test files, so adding
  `ignore` to the build constraint of all 22 `_test.go` files took the whole
  script to `VERDICT: PASS (10 steps)` with zero tests executed — `t2` still
  counted 22 files, because it walks the filesystem, and this row still passed,
  because a wall-clock ceiling reads absent as fast. The row now requires every
  directory holding a `_test.go` file to appear in the suite output as having
  run, and refuses if that population is empty or if the output carries no `ok`
  line at all. It is keyed on the population rather than on a test-count floor,
  which is a number somebody has to maintain and which cannot see one package's
  tests being switched off.

  **That reasoning was right and incomplete, and the incompleteness was the
  escape.** MEASURED by review at `86cb3c5`: with the population keyed on
  packages, ten of twenty-two test files could be build-tagged out — keeping
  one file per package — taking the suite from 161 declared tests to 61 with
  every row green. Package granularity was exactly the boundary; including
  `wire`'s only test file DID go red. The comment defended the choice on two
  true grounds and stated no escape, and the measurement was the escape.

  The population is now declared test FUNCTIONS: every `Test`/`Benchmark`/
  `Fuzz`/`Example` function declared in a `_test.go` file must appear in
  `go test -list`. `internal/tools/testroster` derives the declarations by
  walking the filesystem and parsing — walking, because `go list` honours the
  build constraints that hid those ten files; parsing, because
  `internal/gates/t2` embeds test bodies inside raw string literals and a grep
  reports two declarations that do not exist. MEASURED at `16cb791`: declared
  and listed identical; with the review's ten-file plant, the row named every
  test that did not run. The live figure is in the row's own detail column on
  every run — `N declared test(s) all ran across M package(s)` — and is
  deliberately not copied here. Scenario `suite-files-disabled-partial`;
  `suite-roster-unmeasured` drives the walk itself failing. BOUND: `go test
  -list` honours build constraints while the walk does not, so the comparison
  is exact only while every `_test.go` builds on the host running it.

  **The other bound this bullet used to carry — "a test DELETED rather than
  disabled leaves both sides agreeing" — is closed, and how it was closed is
  the round-9 lesson in one sentence.** It was true because both sides were
  derived from the tree, so a deletion moved both at once. `MIN_DECLARED_TESTS`
  is a literal in `verify.manifest.sh`, derived from nothing, and a literal
  does not move when the tree does. Scenario `min-declared-tests-floor`, which
  deletes a whole test package and requires the row to go red.

  The package check is kept beside it for its diagnosis, which names the
  package. `suite-tests-disabled` and
  `suite-one-package-disabled` plant both, and
  `suite-domain-unmeasured-module` / `suite-domain-unmeasured-walk` drive the
  two ways the domain itself can come back empty — the same shape
  `citation-vacuous` drives for the citation scan.

  One thing this round's own fixes did that is worth recording, because it is
  the failure the round was called for: asking each new check what it does when
  it CANNOT do its job found a defect in one of them. `bounds` reads
  `verify.sh`'s own source, and naming that file `"$0"` names the path the
  CALLER typed, which stops resolving as soon as the script cd's to its own
  directory. MEASURED 2026-08-30 on an untouched tree: invoking
  `library/verify.sh` from the parent directory recorded
  `bounds FAIL … is not readable`. The path is resolved after the cd now, and
  `invoked-by-relative-path` is the preservation control — the only scenario
  that does not invoke the copy as `./verify.sh` from inside it, which is why
  no other scenario could reach it.

Every scenario named above was driven by its own absence: with the step it
guards deleted from a copy, or with the widening it drives reverted, each one
goes red. Six mutants across seven runs, MEASURED 2026-08-29 — the step
deleted, the comment match narrowed back to column 0 (two scenarios), "exists"
reverted to any non-comment line, the token pattern narrowed back to
`Test[A-Z]`, the non-vacuity branch deleted, and the flags check deleted. It is
a statement about those six and about nothing else.

That the detached invocation was a real escape is MEASURED, not reasoned:
before 2026-08-30, replacing `go test "${SUITE_ARGS[@]}"` with a `go test` line
carrying its own literal `-timeout 300s` left `bounds` PASS. With the source
read added, the same plant records
`found 0 suite invocation(s) expanding SUITE_ARGS`.

`shellcheck -S warning` runs on `verify.sh` and on the oracle as one of
`verify.sh`'s own steps. The linted list is enumerated AND cross-checked
against the shell scripts the tree holds, for the reason the gate roster is: a
list that discovers itself is silenced by moving a file, and a list that is
only enumerated is silenced by adding one.

"Shell script" means a regular file ending in `.sh` **or** opening with a shell
shebang. MEASURED 2026-08-29 by review: the walk keyed on the suffix alone
while the surrounding comments described the domain as "every executable shell
script" and as "every tracked `.sh`" — three descriptions, none of which was
the code. A `scripts/preflight` with a `#!/bin/sh` line was linted by nothing
and tripped neither direction of the cross-check. The oracle now plants exactly
that file, deliberately WITHOUT an exec bit, since being a shell script is what
makes it need linting.

It covers shell defects and says nothing about whether the verdicts are right.

**N7, recorded because it is a strength that is easy to over-read.** The
`shellcheck` row accidentally holds ONE direction of the count guard: if a
derived count stops being REFERENCED — `go_files_n` computed and then not
passed to `record` — `SC2034` fires and the row goes red. That is genuinely
useful and nobody designed it. It is not the direction that matters. The
failure this project keeps paying for is a count that is still referenced and
no longer derived, and `shellcheck` cannot see that at all.

### The manifest, added 2026-08-30 (round 9)

The row contract above closed "a row passes on an exit status alone". It did
not close "a row stops existing", and the two are the same defect at different
levels.

MEASURED 2026-08-30 by review, against the head that added the row contract:
delete the `shellcheck` gate — its `step` call, its name in the row roster, and
its two oracle scenarios — and `verify.sh` printed `VERDICT: PASS (10 steps)`
with four live `SC2034` findings in the tree the deleted gate would have
caught. Replacing the whole oracle with forty-five empty `sc_fakeN(){}`
definitions plus one `echo` also passed. So did deleting the count guard
together with the two scenarios that drive it and the plant they edit.

**One cause, and it had survived four rounds: every guard derived its domain
from the thing it guarded.** The oracle's expected count was a `grep` over the
oracle. The row-coverage check read the row roster out of `verify.sh`. The
roster cross-check compared the roster against the rows produced under it.
Every one of them had a non-vacuity floor and every floor was at zero — and
zero is the one size a population cannot reach by deleting a member, so
shrinking a domain by one was invisible in all of them.

`verify.manifest.sh` is the answer, and it is a property rather than a
mechanism: **an expectation the subject cannot shrink.** The file contains
declarations and nothing else — row names, gate names, shell-script names,
scenario names, a literal count beside each list, and `MIN_DECLARED_TESTS`, a
floor under the suite's declared-test population. Four layers rest on it:

1. **It is sourced by both readers.** `verify.sh` and `scripts/test-verify.sh`
   read the same file, so neither derives its expectation from itself or from
   the other. Missing or unreadable is a hard refusal before any row is
   recorded, because a row is something the manifest declares. Scenario
   `manifest-missing`.
2. **Each list's length must equal the literal beside it.** Removing a name
   without editing that number is a refusal — one edit is not enough even
   inside this file. Checked from the shell by `manifest_check` and, separately,
   from Go. Scenario `manifest-count-lies`.
3. **`internal/manifest` pins the same names and numbers in Go**, in another
   language and another directory, with no derivation shared with the shell. It
   runs inside the unit suite. Scenarios `manifest-row-removed` (the review's
   own defeat, replayed) and `manifest-scenario-removed`.
4. **`MIN_DECLARED_TESTS` is a floor that is not zero.** Deleting the Go pin,
   or any test file, takes the suite below it. This closes the bound round 7
   declared unclosable — "a test DELETED rather than disabled leaves both sides
   agreeing" — which was true only because both sides were derived from the
   tree. Scenario `min-declared-tests-floor`.

Two more things moved with it. `verify-oracle` now requires the oracle to
report a passing result for **every scenario name the manifest declares**,
rather than to report a count matching a `grep` over its own source; and
`record`'s count guard is driven IN PROCESS by a `self-check` row on every run,
four probes with a preservation control, so deleting the guard reddens a row in
the same run with no oracle involved. Scenarios `oracle-names-fabricated`,
`self-check-guard-deleted`.

**What this does NOT claim, stated in the form the claim has to take here.**
Not "the arbiter cannot be shrunk". The claim is: **no edit confined to a
single file can shrink the arbiter's population.** Editing the manifest and the
Go pin together still does it, and nothing in a repository can prevent that —
the regress terminates at a person reading a diff. Two named residuals:

- A stub oracle that READS the manifest and prints a correct `RESULT` line per
  declared name defeats the name check. That is strictly harder than the stub
  that defeated round 7, which needed to know nothing. `oracle-is-invoked`
  builds exactly such a stub, so the bound is executed rather than asserted.
- A scenario whose body asserts nothing is declared, defined, counted and says
  nothing. The row-coverage check is a spelling check and reads as one.

### Contracts, not names — added 2026-08-30 (round 11)

The second residual above was not a residual. It was the next defeat, and the
review took it: keep every scenario NAME, delete four scenario BODIES, make
`record()`'s guard inert while leaving the anchor text its scenario plants on,
gut `self_check()` to report `PASS` unconditionally, delete one comment — and
`verify.sh` reports `VERDICT: PASS` with a live defect in the tree. The
manifest and the Go pin were never touched. Every count was satisfied in full.

**The row-coverage check was the whole distance**, and it was `grep -q` over
the oracle's own source for the string `row <name>`. One deleted comment
defeated it. It has been removed rather than tightened: a better pattern is a
better spelling check, and the fault was not the pattern.

**Rounds 5 through 9 closed shrinking the POPULATION. This closes shrinking
DETECTION while the population is intact.** Every operand up to here answered
*is it there*. None answered *does it do anything*.

**`MANIFEST_SCENARIO_CONTRACTS`** states, per scenario,
`name|rc-class|token|diagnosis` (the fourth field was added in round 13, below):

- **rc-class** — `zero`, `nonzero`, or `static`. What the scenario must have
  seen the subject's process do. `static` means it does not run the subject at
  all; it does NOT mean it may observe nothing.
- **token** — `<row>:<PASS|FAIL|ABSENT>`, a verdict the scenario read out of
  the subject's own table. There is no "demand nothing" escape; the one that
  existed had a single user, and that user now reports the value it read.

The oracle records an OBSERVATION every time a scenario reads a row verdict or
a process result, and prints them beside its per-scenario result. `verify.sh`
reads the report against the manifest. **The comparison is in none of the three
places somebody would edit to make a scenario stop working** — not the
scenario, not the manifest, not the oracle's roster.

An emptied body therefore reports `PASS` having observed nothing, and *that* is
the failure, named by scenario, with what it should have observed. Driven end
to end by `scenario-body-emptied`, in two halves so it does not cost a nested
oracle run: an emptied body reports an empty observation, and an empty
observation fails the row.

**Three further operands, each closing a way to satisfy the check while doing
nothing:**

- `observation-recorder-stubbed` — gut the recorder and every contract becomes
  vacuous in one edit. It is the fail-closed drive: no observations at all must
  fail, not pass.
- `ORACLE_MIN_SECONDS` — a wall-clock floor under the oracle's own run, checked
  LAST so a duration can never displace a diagnosis that names a scenario.
  **BOUND: it is a floor against an INSTANTANEOUS fabricator, not proof of
  work. A fabricator that sleeps defeats it.** `oracle-too-fast`.
- `scenario-death-is-reported` — a scenario whose plant no longer applies used
  to print nothing, which is byte for byte what a deleted scenario prints.
  Three scenarios went silent that way while this round was being written, each
  because a literal anchor duplicated a manifest value that had moved. Death is
  now reported as a FAIL in the scenario's own name, and the population count's
  refusal names who went silent instead of only how many.

**BOUND on the whole design, stated rather than claimed away:** a contract
binds a scenario to observing a verdict, not to observing the right thing for
the right reason. A body reduced to just the lines that produce its observation
would satisfy it. What the design buys is that every cheap defeat — empty it,
stub it, keep the name, fake the report, return instantly — fails loudly, and
the remaining defeat is no longer cheaper than doing the work.

**That bound was taken, and the section below is the answer.** It is worth
naming what happened rather than quietly editing the paragraph: the review
built the body this paragraph describes, composed it with the *other* bound
stated three paragraphs up — that the contract pins the outcome and not the
plant — and got `VERDICT: PASS` with a live defect and four scenarios testing
nothing. Two bounds stated separately are not two residuals. They compose.

### The declared-test band, and why it is not an equality

`MIN_DECLARED_TESTS` was a floor. MEASURED 2026-08-30 by review: nothing in the
tree ever raised it, so its margin — and its protection — eroded with every
test added, and today's margin of zero was the strongest it would ever be. A
floor whose distance from the tree only grows is a floor on its way to saying
nothing.

A strict equality was implemented first and reverted, and the reason is worth
recording because it is not obvious: **oracle scenarios plant Go tests into
their own copies of the tree**, so under an equality every such scenario fails
the `unit-suite` row it is not testing. Three of them do, plus the ceiling
helper.

So it is a BAND: `MIN_DECLARED_TESTS` to `MIN_DECLARED_TESTS +
MAX_DECLARED_MARGIN`, with the margin at the largest number of tests any single
scenario plants. The lower edge is exactly where it was; the erosion is capped
instead of unbounded, and the diagnosis names the number to write.
`min-declared-tests-floor` drives down, `min-declared-tests-margin` drives up
with `MAX_DECLARED_MARGIN + 1` tests derived from the manifest, and
`ceiling-control` — which plants exactly one — is the preservation control that
stops the band collapsing back to an equality.

**The margin is DERIVED, not measured once.** Round 12's review pointed out
that a literal sitting at today's maximum under a cap of four could be
quadrupled one line at a time, each edit looking exactly like the maintenance
this design claims to remove.
`TestDeclaredTestMarginIsDerivedFromWhatScenariosPlant` reads the oracle,
walks each scenario into the helpers it calls, counts the test functions each
one plants, and refuses any other value in either direction. The Go cap stays
as a backstop rather than as the check.

**BOUND on the derivation:** it is a static read of a shell script. A helper
reached through a variable is invisible to it, and a commented-out plant still
counts. Both fail toward a larger number than the truth, which is the direction
that goes red rather than the direction that goes quiet.

### `doc-numbers` — the sweep is a row now

Round 9 removed thirteen numbers from `README.md` and `docs/*.md` that an
instrument recomputes, and argued the class was closed "by removal, not by
vigilance". Round 10 measured what that argument was worth with no observer
behind it: **the same round wrote a fresh derived number into this document, in
the sentence explaining the deletions.**

`scripts/sweep-doc-numbers.sh` is that sweep, executable, and `verify.sh` runs
it as a row. Two fixes to the method, not the result:

- **The date filter dropped whole LINES.** A number was invisible to it
  whenever it shared a line with a date — which is exactly how this project is
  asked to write a measurement. The domain excluded the thing being swept for.
  Dates, RFC numbers and section marks are now blanked as TOKENS and the line
  is kept; that made five previously invisible lines visible, four structural
  and one a live derived number.
- **Nothing re-ran it.** `--check` refuses the shapes round 9 removed, so a
  removed number coming back goes red. `doc-number-reintroduced` drives it and
  `doc-sweep-deleted` drives the sweep's own absence — the row must not pass
  when its subject is gone, which is the failure round 8 found one level up.

**BOUND:** `--check` refuses the shapes that were removed, not every derived
number that could ever be written. A new instrument's number is uncovered until
its shape is added.

Round 12's review drove that bound: one added line carrying four live
instrument-owned numbers passed, and the only thing that moved was the
population count — printed by `--check`, compared to nothing. So it is compared
now. `DOC_NUMBER_CEILING` holds the population from above, going over it prints
the whole enumeration rather than the count, and the four shapes from the
review's own probe line are refused by name.

**BOUND on the ceiling:** it is a size, not a membership. Deleting one bare
number and adding another is invisible to it, exactly as the declared-test band
is blind to a swap.

### Naming the DEFECT, not only the row — added 2026-08-30 (round 13)

Round 11's contracts pinned, per scenario, which row must go red. Round 12's
review took the two bounds round 11 stated — *a body reduced to just the lines
that produce its observation would satisfy it*, and *the contract pins the
outcome, not the plant* — **built both, composed them, and got `VERDICT: PASS`
on thirteen steps with a live defect in the tree and four scenarios testing
nothing they were written to test.** The preservation control is what makes it
bite: *emptying* one of those same bodies still fails and names itself, so the
machinery works and the substitution is precisely what passes.

**The missing property in one sentence: the design named the row a scenario
must redden, and nothing named the defect it must plant.**

**The fourth contract field is the diagnosis, and the ARBITER writes it.** A
verdict says a row went red; only the note says why. The note is emitted by
`verify.sh`, not by the scenario that planted the defect, so a scenario that
reddens the right row by planting a different defect now reports a different
note and no longer passes for it. Where the arbiter's own note was generic —
`exit N` with no output, a roster mismatch that did not say which direction —
the ARBITER was improved rather than the contract loosened.

**BOUND:** the diagnosis is matched as a SUBSTRING of the arbiter's note. Two
plants the arbiter describes in the same words are still indistinguishable, and
how narrow that is depends on how specific its notes are.

**`self-drive` — the arbiter plants defects for itself.** Every check on the
oracle's REPORT is defeatable by a fabricator that knows the check; that is the
terminus round 11 named. Round 12's review executed it: a short
single-file fabricator that sources the manifest and sleeps past the wall-clock
floor removes all sixty detections and passes in thirty seconds. The only
non-circular answer is the arbiter doing detection work ITSELF, so before it
calls the oracle it copies the tree, plants seven defects it chose, runs one
`--inner` pass, and requires each planted row to redden — with a preservation
half in the same run requiring the unplanted rows to stay green, because a
self-drive satisfied by an arbiter that reddens everything is a check with one
possible verdict. `self-drive-blinded` and `self-drive-reddens-everything`
drive both halves.

**BOUND:** seven rows of fourteen, in one tree, with plants chosen not to
cascade into the rows on the preservation list. It is a lower bound on the
arbiter's liveness, not proof of it, and the seven rows it does not plant stay
blindable. What it changes is the price: blinding one of those seven now needs
an edit to `verify.sh` as well as to the oracle.

**And the fabrication itself is NOT stopped, which the tree says out loud.**
MEASURED: a single-file fabricator that *injects* before the oracle's
dispatcher, rather than overwriting the file, still gives `VERDICT: PASS` in
thirty-six seconds with all sixty-three of the oracle's detections gone. What
the self-drive removes is what that buys: fabricate the oracle AND blind the
`gofmt` row AND plant a live unformatted file — the combination that passed
before this round — and the run ends `VERDICT: FAIL`.

**Numbers that were literals sitting at today's value.** Three, all raised by
the review as the same defect one level in: `MAX_DECLARED_MARGIN` is now
derived from what the scenarios actually plant (above); `ORACLE_MIN_SECONDS` is
derived from the measurement written beside it at a stated percentage, with
`manifest_check` refusing the two drifting apart; and the `static` contract
exemption is enumerated in `MANIFEST_STATIC_CONTRACTS` and held to set equality
from Go, after the comment describing it turned out to have been wrong since
the second member landed — it said "capped at ONE, its one member" while the
pin said two and the table held two.

**Anchors.** Three scenarios still anchored their plant on a literal copy of a
constant in `verify.sh`; they read it from the copy now. `edit()` refuses an
anchor that occurs more than once instead of silently patching the first —
which immediately caught two scenarios whose row-name anchor had stopped being
unique, and those two now delete from a NAMED ARRAY rather than matching a
line. Round 11 learned that a neighbour is not an anchor; round 13 adds that a
LITERAL is not an anchor either, and that a pattern used to find an anchor must
not match the line that contains the pattern.

**Quoted output is indented.** When the self-drive fails it prints the planted
run's report. Unindented, that report's table is indistinguishable from the
run's own table to anything parsing the stream — and the oracle parses the
stream. It cost every row of two scenarios' readings coming back `ABSENT`.
`verify.sh` indents every quoted sub-report and the oracle reads the LAST table
rather than the first; either alone would have fixed it, and both are cheap.

**`silent-scenario-named`.** A scenario that dies loudly is caught by the death
reporter. A scenario that dies SILENTLY — killed before it can print — is
caught only by the population count, and the oracle's refusal names which one
went quiet. That naming was reachable, correct, and asserted on by nothing;
`verify.sh` now also carries the oracle's own refusal line into the
`verify-oracle` row, because `exit 137` is not a diagnosis.

### The contaminated oracle run of round 9, settled

Round 9 reported a scenario failing twice and then passing on a frozen tree,
and offered wall-clock pressure under parallelism as the mechanism. **That
mechanism is REFUTED by measurement** (review, 2026-08-30): serial and
eight-way concurrent inner runs differ by about a tenth against the ceiling,
all green, and `ceiling-control` — which deliberately burns time and still
demands a pass — is strictly more exposed than the scenario that fired and did
not fire. What is supported is the other half: the tree was being edited while
the oracle was copying it. **The ceiling is not raised.** Do not edit the tree
while the oracle runs; that is the whole remedy.

## The policy is itself under test

Everything the gates enforce is a table in `internal/gates/rings/rings.go`. A
gate can be perfect and enforce a widened table, and MEASURED 2026-08-28 by
review, that was the state: mutating the gate logic killed every mutant, and
mutating the **policy** — adding `net`, `time`, `os`, `syscall` to the ring-1
allowlist, adding `Sleep`, `After`, `Tick` to the test allowlist — was never
attempted by the author. When the reviewer tried it, 7 of 16 widenings
survived. Re-derived here at 21 widenings, 12 survived.

Two layers now stand behind those tables, and they fail differently on purpose.

**Derived (`internal/gates/rings/policy_test.go`).** These do not read a list of
names; they compute the answer from the standard library and compare.

| Check | What it derives | Killed by |
|---|---|---|
| `TestPureStdlibClosureIsClean` | `go list -deps` of every admitted package; any whose closure reaches `os`, `syscall`, `net`, `time`, `context`, … must carry an identifier restriction | admitting an impure package |
| `TestAllowlistedIdentifiersExist` | two signals per entry — `go doc pkg.Ident` resolving AND the name appearing verbatim in `go doc -all pkg` | a typo, or an identifier the stdlib removed |
| `TestAllowlistsExcludeStreamAPIs` | signatures naming `io.Writer`/`io.Reader`/…, per package, refusing when it matches nothing | admitting a stream API into a pure ring; and, via witnesses, the pattern going inert |
| `TestTimeAllowlistExcludesWaiters` | signatures returning `<-chan Time`, `*Timer`, `*Ticker` | admitting a waiting primitive into tests |
| `TestContextAllowlistExcludesDeadlines` | constructors whose signature names `time.Duration` or `time.Time` | admitting a deadline constructor into tests |

The derived layer covers identifiers nobody enumerated, which is the point: it
found a hole neither human pass did. `encoding/hex` was admitted to ring 1
unrestricted; its dependency closure reaches `os` and `syscall`, and
`hex.Dumper` takes an `io.Writer`. It now carries a restriction.

**A derived check must refuse when it cannot derive.** MEASURED 2026-08-29 by
review, and this was a blocking finding: the stream derivation guarded against
`go doc` breaking — a signature it could not read was a failure — but not
against its own pattern going inert. Neutering the regexp so it matched nothing
left the whole suite green, and a ring-1 file calling `fmt.Fprintf` into a
`bytes.Buffer` then passed the full lane with a PASS verdict. A check
with one possible verdict reports that verdict.

Two things stand behind it now, and they are different guards rather than one
guard twice. **Per-package non-vacuity:** if the pattern matches no signature
in a restricted package, the test REFUSES and says how many signatures it read,
so "the pattern is broken" is distinguishable from "the toolchain answered
nothing". **Witnesses:** `streamWitnesses` names three identifiers per
restricted package that provably take or return a stream (`fmt.Fprintf`,
`fmt.Fprintln`, `fmt.Fscanf`; `hex.Dumper`, `hex.NewEncoder`, `hex.NewDecoder`),
and the control asserts the pattern matches each one *directly* — not that the
check recorded a match, which a mutant that records everything defeats. A
second test cross-checks the witness map against the restricted packages in
both directions, so emptying it is a refusal rather than a shrinkage.

Each guard alone suffices and the composition proves it: with the pattern
neutered, deleting either guard still goes red; deleting **both** returns
exactly the original green. That is the evidence that neither is decoration.

`go doc pkg.Ident` is an existence probe and **not** an exact oracle — MEASURED
2026-08-29, `go doc time.now` exits 0, so it is case-insensitive and a
lower-cased typo resolves to the unexported original. The existence check
therefore takes a second signal: the name must also appear as a whole word in
`go doc -all pkg`, whose output is case-sensitive. Validated over every
allowlisted identifier with no false miss.

**Enumerated, and driven through the real binary**
(`t1/policy_driven_test.go`, `t2/policy_driven_test.go`). `PureRefusedPkgs`,
`PureRefusedIdents` and `TestRefusedIdents` are things the tables must never
admit. Membership in a map proves nothing about behaviour, so each case is
generated into a fixture and run through the built gate: the gate must exit
VIOLATION. Widening the allowlist makes it exit PASS and the case goes red.

**Preservation controls, of two kinds, because one kind is not enough.** A
guard fails in one direction, and a policy that refuses everything passes every
refusal test.

*Generated.* `TestPureAllowlistIsAccepted` imports every admitted package and
names every restricted identifier in one ring-1 fixture and requires PASS.
`TestTestAllowlistIsAccepted` names every allowlisted identifier across `time`
and `context` in a test fixture and requires PASS. Both build their fixture from
the tables, so they cover a package ADDED to a table that nobody wrote a test
for.

The sizes of those tables are deliberately not written here. They were, and
they were three more numbers that an instrument recomputes on every run — the
same shape as the coverage figures below, which drifted and were caught only by
a reviewer running the test.

*Hand-written.* `TestRealisticRing1CodeIsAccepted` and
`TestFakeClockTestIsAccepted` are ordinary code of the shape M1 will contain —
option parsing, wire encoding, address formatting; a table-driven lease
lifecycle on a `fakeClock` — written out rather than generated.

**The split is not stylistic and it was measured.** MEASURED 2026-08-29:
deleting `bytes` from the ring-1 allowlist SURVIVED the generated control, and
so did deleting `Since` from the test allowlist. A control that builds its
fixture from the table it is testing shrinks with the table: the fixture simply
stopped importing `bytes` and passed. A measurement cannot backstop itself.
With the hand-written controls in place, nine narrowings — five packages, four
identifiers — all die.

The two kinds fail in opposite directions and neither subsumes the other. The
generated ones cover ADDITIONS to a table; the hand-written ones cover
REMOVALS.

**The bound on the hand-written half is open today, not in future.** Most
allowlisted identifiers are named in no `_test.go` file at all, so most of the
allowlist could be narrowed with nothing going red. Identifier narrowings
against today's tables were measured by review both surviving and dying;
PACKAGE narrowings are covered, all of them dying.

**The figures are not written here on purpose, and the reason is a defect this
document committed three times.** A ratio, four per-package ratios and a count
stood here as literals; every one was stale, and the paragraph named
`TestNarrowingCoverageIsMeasured` as its authority two sentences later — the
test that falsifies all six in a fraction of a second. Round 9 deleted them and
then, in the same edit, wrote the correcting figures into the sentence that
explained the deletion. That third instance is why the sweep is now
`scripts/sweep-doc-numbers.sh` and a `doc-numbers` row rather than a paragraph:
prose about not writing numbers is still prose.

A number an instrument recomputes on every run does not belong beside a pointer
to that instrument. The pointer is the whole value; the copy can only go stale
and contradict it.

Run it:

```
go test ./internal/gates/... -run TestNarrowingCoverageIsMeasured -v
```

It refuses rather than reporting zero coverage when it cannot find the test
files, so it is a measurement a run makes rather than a sentence in a document.

This paragraph also used to read "a package admitted **later** and never
written into those fixtures", which described a present-tense escape as a
future one — a completeness claim wearing a bound's clothes.

Why it is tolerated at M0 rather than closed: a narrowing makes the gate REFUSE
honest code, loudly, at the point of use, naming the identifier — a
self-announcing failure. A widening is silent, and the widening direction is
covered. Naming every identifier in a hand-written fixture would rebuild the
generated control by hand and misrepresent what M1 needs.

### What the policy guards cannot see

1. **A widening that is genuinely correct.** They cannot tell a considered
   policy change from a careless one; they make the change loud, not illegal.
   Deleting a case remains available to anyone who means it.
2. **`time.Sleep`, from the derived side.** MEASURED 2026-08-29: the waiter
   derivation matches signatures *returning* a channel or a timer, and
   `func Sleep(d Duration)` returns nothing. It matches 5 of the 6 waiting
   primitives; `Sleep` is carried by the enumerated layer alone. The two layers
   are not redundant and neither is a superset of the other.
3. **A package it does not restrict.** The closure check demands a restriction
   for an admitted package whose closure is impure. It says nothing about which
   identifiers that restriction should hold beyond the stream-API rule.
4. **`context.AfterFunc`, and this is the one place a mutant legitimately
   survived.** It runs `f` on cancellation, not on a clock, so it is not
   obviously a T2 violation and it is deliberately NOT listed as refused —
   claiming otherwise would assert an adjudication nobody made. Its signature
   `(ctx Context, f func()) (stop func() bool)` names no `time` type, so the
   deadline derivation correctly does not match it. That left its refusal held
   by nothing but the absence of a key in a map, so it is **pinned as a case**
   (`TestContextAfterFuncIsRefusedByDefault`): today's answer is refused,
   admitting it means deleting the case and writing down why. A pin records a
   decision that has not been made; it does not make one.
5. **A narrowing of the identifiers no fixture names.** Most allowlisted
   identifiers appear in no test file, so they can be removed from an allowlist
   with nothing going red — the generated controls cannot see it, being derived
   from the same table, and the hand-written ones name only what realistic code
   uses. This is open now. The count is printed by
   `TestNarrowingCoverageIsMeasured`; it is deliberately not repeated here,
   because this bullet is where it was repeated and where it went stale.
6. **The stdlib moving under them.** `go doc` is queried at test time against
   the toolchain in use, so an identifier removed upstream turns the existence
   probe red — which is correct — but a *newly added* waiting primitive is
   simply not on the allowlist, and is refused by default rather than noticed.

## Diagnostics name positions relative to the tree root

MEASURED 2026-08-28 by review: six self-test cases asserted a rule had fired by
looking for a substring in the gate's output, and the output carried the
fixture's `t.TempDir()` path — which Go names after the subtest. A case named
`third_party` was satisfied by the words `third_party` in the temp directory,
not by the diagnosis. Every one of those assertions would have passed with the
rule deleted.

Fixed twice, on purpose. At the source: `scan.Rel` makes every position
relative to the tree root, so no diagnostic carries the caller's directory. And
at the one place every self-test passes through: `gatetest.Run` fails any case
whose gate output contains the fixture root. A source fix holds only until the
next diagnostic is written; the choke point holds after that.

`scan.RelErr` is the same fix for the other half. MEASURED 2026-08-29 by
review: the first pass at keeping roots out of diagnostics dropped the *error*
along with the path, so a refusal said the tree was unreadable without saying
why. `RelErr` relativises the message and keeps the cause. `gatetest.Run`
already covers the root half at every call site; `TestRelErr` covers the cause
half, which nothing else did.

`Rel` falls back to the **basename** for a path it cannot relativise, and a
basename satisfies the choke point perfectly well — it does not contain the
root either. MEASURED 2026-08-29: mutating `Rel` to take that fallback always
survived the whole suite. `TestT1DiagnosticsNameTheRing` now plants the same
basename in two rings and requires the diagnosis to tell them apart, and
`TestRel` pins both directions of the function itself.

**What this cannot see:** it fixes the shape of a position, not its accuracy. A
gate emitting a plausible but wrong relative path passes all of it.
