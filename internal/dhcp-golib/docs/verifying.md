# Verifying dhcp-golib

    ./verify.sh

One command, one verdict. Exit 0 is PASS and is the normal state; exit 1 is
FAIL. A step that cannot be measured is a FAIL, never a skip.

It runs `go build`, `go vet`, `gofmt`, `shellcheck` over the shell scripts, the
gate roster cross-check, the T1 and T2 gates, the race-enabled pure unit suite
under a wall-clock ceiling AND a `go test -timeout` (the ceiling cannot bound a
test that never returns — it is computed after `go test` comes back), the
namespaced dnsmasq tests under a ceiling and a timeout of their own, a check
that the flags each of those runs with carry a hang timeout that exceeds its
ceiling and that each invocation expands its own flag array, a check that every
test function DECLARED in a `_test.go` file actually ran, a citation check, a
byte-for-byte comparison of the README's Usage block against `ExampleClient`,
and its own oracle.

**No row can report PASS on an exit status alone.** A row records PASS only by
also stating how many things it examined, and a count that is absent or zero is
rewritten to FAIL by the one function that writes a row. This is structural
rather than a guard per row, because three rows were found passing over an
absent subject in three consecutive review rounds — the unit suite with every
test build-tagged out, the oracle replaced by `exit 0`, and the row roster
itself with a step call deleted — and all three inherited the same default,
that a command with nothing to do exits 0.

The residual is worth naming beside the claim: nothing inside the script can
force that count to be DERIVED rather than written. What closes that is
external — a scenario that empties a row's domain and requires the row to go
red. This paragraph used to say there was one such scenario per row. There was
one, for one row of eleven; `docs/gates.md` now lists which rows have one,
which do not, and why the ones that do not cannot.

The citation check is stated here as a BOUND, because the sentence that used to
stand in its place was a completeness claim and the tree falsifies it. What it
reads is: a Test/Benchmark/Fuzz/Example token appearing after the first `//` on
a `.go` line that is not a URL scheme separator, or anywhere on a line of a
`.md` file; and it requires each one to appear in a top-level `func`, `var`,
`const` or `type` declaration. It does **not** see block comments, `.sh` files,
or a token in a `.go` line's ordinary text — there are live examples of all
three in this tree, `scripts/test-verify.sh` chief among them. The seven
enumerated escapes, including the one direction in which it produces a false
positive, are in `docs/gates.md`; they are part of the check, not a caveat
about it.

"Every check" used to be bounded by this sentence: *a check runs only if
`verify.sh` calls it, and nothing inside `verify.sh` notices a call that is no
longer there.* That bound was accurate and it rested on the oracle's scenarios
noticing instead — and on 2026-08-30 a review measured that the oracle can be
replaced by a two-line script that exits 0. **An acknowledged bound was
load-bearing on a guard that dies with its subject**, and the composition was
never measured: with the oracle stubbed AND a step call deleted, the arbiter
printed `VERDICT: PASS (10 steps)`.

**That fix was defeated too, and the sentence that used to stand here is the
reason it is worth reading the next one carefully.** It said: the row roster is
cross-checked against the rows recorded, and the oracle's expected scenario
count is derived by `verify.sh` from the oracle's own source, so a stub —
total or partial — is a mismatch rather than an answer. Both halves were true
and both were defeated in one round, the same way, because **every expectation
was derived from the thing it was checking.** MEASURED 2026-08-30 by review:
delete the `shellcheck` gate — its step, its roster entry, its two scenarios —
and eleven rows became ten with `VERDICT: PASS (10 steps)` and four live
`SC2034` findings in the tree. Replace the oracle with forty-five empty stub
functions and one `echo`, and it passes. Delete the count guard together with
the scenarios that drive it, and it passes.

Four rounds found that same shape, one level up each time, and every guard had
a non-vacuity floor. **Every floor was at zero.** Zero is the one size a
population cannot reach by deletion, so shrinking one by a single member was
invisible in all of them.

What stands now is `verify.manifest.sh`: a file that contains the expectation
and nothing else — the row names, the gate names, the scenario names, a literal
count beside each list, and a band around the declared-test population. Both
`verify.sh` and the oracle read it, so neither derives its expectation from
itself or from the other, and `internal/manifest` pins the same names and
numbers from Go, in another directory, running inside the suite.

**And that, in turn, was defeated — on a different axis, which is why it is
worth its own paragraph.** MEASURED 2026-08-30 by review: keep every name,
delete four scenario BODIES, make the arbiter's own guard inert, and the run
reports `VERDICT: PASS` with a live defect in the tree. The manifest was never
touched and every count was satisfied in full, because **a name is not a
behaviour**. Every operand answered *is it there*; none answered *does it do
anything*.

So the manifest now states, per scenario, what that scenario must be OBSERVED
to have done — the process result class, and a row verdict it read out of the
subject's own table. The oracle reports what each scenario actually observed;
`verify.sh` compares the report against the manifest. The comparison lives in
none of the three places somebody would edit to make a scenario stop working,
so an emptied body is not a passing body: it is a scenario that observed
nothing, named in the diagnosis. A scenario that dies mid-plant reports its own
death for the same reason — silence and success used to be the same output.

That bound was taken. A review built exactly the body described above — kept
only the lines producing the observation, dropped every assertion, planted a
different defect reaching the same row — composed it with the bound beside it,
and got a passing verdict over a live defect with four scenarios testing
nothing. So a contract now names the DEFECT as well as the row: its fourth
field is a fragment of the note the ARBITER wrote beside that row, and the
arbiter is not the scenario that planted anything. Reddening the right row for
the wrong reason produces a different note.

**BOUND, and it is the honest one:** the diagnosis is matched as a substring,
so two plants the arbiter describes in the same words are still
indistinguishable. And every check on the oracle's report is defeatable by a
fabricator that reproduces the report — which is why `verify.sh` also plants
seven defects for ITSELF before calling the oracle, and requires each to redden
its own row while the unplanted rows stay green. That is a lower bound on the
arbiter's liveness, not proof of it; what it buys is that blinding the arbiter
now takes an edit to `verify.sh` too.

The claim is bounded, because a completeness claim here is the sentence this
project has been wrong about four times. What holds is narrower: **the row
roster and the scenario list cannot be shrunk by an edit confined to a single
file**, because `internal/manifest` pins them a second time, in Go, and the
edit then has to be made twice, in two languages. **The escape, named rather
than left to be found:** the self-drive's own detection set is held by a length
literal in `verify.manifest.sh` and by nothing else, so deleting entries there
shrinks it and the run still passes. MEASURED; the plugin's deferred-work
record carries the reproduction.

The step deletions below were driven rather than assumed.
MEASURED 2026-08-29, deleting one step at a time from a copy and running
`scripts/test-verify.sh` against it: eight of the nine steps then present
redden at least one oracle scenario — `gofmt` 4, the gate roster 3, the two
gates 6, the unit suite refuses the run outright, `shellcheck` 2, `build` and
`vet` 1 each. Those counts were taken against the 19-scenario oracle, before
`hang-bounded`, `bounds-ordering` and `stale-citation` were added later the
same day. They are LOWER bounds now rather than equalities: a scenario can only
add a detection, never remove one, and nobody re-ran the nine deletions. The
two steps added after that sweep — the timeout-ordering check and the citation
check — were each driven by deleting the step from a copy and watching the
scenario that owns it report ABSENT.

What landed on 2026-08-30 is a different shape and is described as one: three
checks added INSIDE steps that already existed, so step deletion is not the
drive for them. Each was driven by the mutant it exists to catch — the suite
invocation detached from its flag array, one package's tests and then the whole
library's tests switched off, and a URL in a string literal, that last one in
both directions because the risk in the fix was that it would blind the check.
The oracle carries `suite-args-detached`, `suite-one-package-disabled`,
`suite-tests-disabled`, `citation-url` and `citation-after-url` for them.

`vet` had no witness until this measurement was taken; it passed 18 of the 18
scenarios that existed then with the step deleted, and the `vet-violation`
scenario exists because of that.

The oracle step is the one that cannot be closed from the inside: see below.

There is no CI on this repository and there will not be: the self-hosted
runners belong to the plugin repository and cannot serve a second private repo
without an organisation. `verify.sh` is the only arbiter there is, which is why
it is one command and not a paragraph describing what a developer should run —
and why it has an oracle of its own, `scripts/test-verify.sh`, which plants a
defect in a copy of the tree and requires the row that owns that defect to be
the row that fails. `verify.sh` runs it as a step.

That loop closes only while it is wired, and it cannot close itself: a
`verify.sh` that drops its oracle step never runs the scenario that checks the
step is there. MEASURED 2026-08-29 — deleting the step is caught, but only
incidentally, by `shellcheck` objecting that `--inner`'s variable became
unused; composing past that one objection gives a clean PASS verdict with the
arbiter's own arbiter silently gone. Running
`scripts/test-verify.sh` directly is the check for that, and it is a human act,
not a wired one. The rest of what neither can see is in `docs/gates.md`.

### The rows added on 2026-09-05, and the two verdicts that came with them

**`netns-suite`, and why the unit suite got smaller.** The tests that
re-execute themselves into a user and network namespace and talk to a real
dnsmasq used to run inside the unit suite, under the same wall-clock ceiling.
That ceiling is T2's second instrument — it asks whether the suite has drifted
into waiting — and it was measuring two populations at once: a pure suite that
should never wait, and a set of runs whose whole job is to wait out real DHCP
and ARP intervals. MEASURED on the session box the day of the split: the pure
half runs in a fraction of its ceiling now, the namespaced half takes most of a
minute, and together they had left the ceiling with seconds of headroom while
M7 was about to add more namespaced runs.

The two populations are a PARTITION derived from ONE roster:
`internal/tools/testroster -netns` reports the tests that call the re-exec
helper, the pure suite is told to skip exactly those names, and the netns row
is told to run exactly those names. **A test that is excluded from both rows is
the failure this shape exists to refuse**, and it cannot happen by omission: a
test the classifier does not recognise is not skipped, so it runs in the pure
suite and its seconds land on the pure suite's ceiling. A roster that cannot be
derived at all fails BOTH rows rather than falling back to running everything.
Both rows report how many tests they examined; the netns row additionally
requires the set of names the run REPORTED to equal the roster, because a
`-run` regexp that matches nothing exits zero, and it treats a `SKIP` as a
failure, because those tests fail closed rather than skipping.

The other direction is the one the complement does not close, and it was open
until round 2: with the pure suite's `-skip` matching nothing, BOTH rows run
the namespaced tests, `go test` exits zero, and — MEASURED by review — the
combined seconds still fitted the pure suite's ceiling while the row went on
reporting how many tests were "held" for the other row. Both of those numbers
came from the roster that produced the filter, so the sentence was true of the
roster whatever the run did. The pure suite now runs with `-v` and reads the
tests it STARTED off its own output: a roster name appearing there is the
partition broken, and it is a failure with the leaked names in it. Scenario
`suite-partition-skip-inert`.

The netns row is an OUTER row: `--inner` does not have it, so the oracle's
copies of this tree do not each raise namespaces and a dnsmasq of their own.
What that leaves open is written down rather than argued away — the row is
driven inside the oracle by three scenarios, and a namespaced test failing for
a PRODUCT reason is driven by the real run and by no scenario. The three are
`netns-row-empty-domain` (the roster cannot be derived at all),
`netns-row-control` (the honest pass, both rows green with their counts) and
`netns-row-partition-broken` (the row's own `-run` narrowed to a single name,
so the run exits zero over a subset of its population and the set comparison is
the only thing that can see it).

**`readme-usage`.** README.md's Usage section prints a Go function and the
sentence under it says it is `ExampleClient` in `runtime/example_test.go`
*byte for byte*. Nothing checked that; a claim of identity between two files
was being made by prose. The row extracts both and `diff`s them verbatim — no
normalising, because the sentence says byte for byte and a row comparing a
normalised form would leave that sentence false while passing. Either side
extracting to nothing is a FAIL and not an agreement, since two empty files
diff clean. Driven in both directions: `readme-usage-drifts-in-the-readme`
takes the block out of the README, `readme-usage-drifts-in-the-example` moves
one byte of the function.

**`SKIPPED`, the third verdict, and the only one that means "not measured".**
The oracle is most of the run's wall clock and its subject is `verify.sh`
itself, the manifest and `scripts/`. When those files are byte for byte what
they were the last time the oracle passed here, `verify-oracle` records
SKIPPED, naming the hash and the file set it covers, and the verdict line still
says PASS only because every row that was not skipped passed. `./verify.sh
--oracle` runs it regardless, and that is what should be run before a merge.

The skip is keyed on the CONTENT of those files, recorded in a stamp that is
gitignored and per-clone. A `git diff` against a ref was ruled out for a
specific reason: a fresh clone at a commit that changed the arbiter has nothing
to diff against and would skip, where a stamp makes a fresh clone run the
oracle once. What binds the stamp to a real pass: it is written in one place,
after every check the row makes, and only when `record()` ACCEPTED the pass; it
names the root it was written for, so a stamp copied into another tree — every
oracle scenario copies this one — grants nothing there. Only `verify-oracle`
may record SKIPPED; `record()` rewrites a SKIPPED from any other row to FAIL.

What drives that arm, stated exactly, because the first version of this
sentence overstated it: the `self-check` row puts a skip by a row that may not
skip, and an honest one by the row that may, through `record()` on every run —
and the row also counts its probes and the refusals they earned against
`SELF_CHECK_PROBES_N` and `SELF_CHECK_REFUSALS_N` in `verify.manifest.sh`. The
count is what survives the composed edit: MEASURED by review, disabling the arm
AND deleting the probe that drives it in one edit left every row green and the
whole oracle green, because the only trace was a refusal count derived from the
case list that had just been shortened. Scenarios `self-check-guard-deleted`
(the arm disabled, the probe speaks) and `self-check-skip-arm-deleted` (both
removed together, the declared count speaks).

**Two bounds on the skip, both real.** The stamp is a LOCAL CACHE and not
evidence: it is gitignored and per-clone, and a stamp written BY HAND carrying
the hash `verify.sh` computes grants a skip, because any hand can compute what
`verify.sh` computes. So the merge rule is not "the stamp says it passed" — it
is a reviewer's own `./verify.sh --oracle` run at the head being merged.

The second bound is that the hash covers the ARBITER, so a scenario that
depends on the PRODUCT's shape can go stale without a covered byte moving.
Those scenarios are not typed out here. A typed list was, and it omitted eleven
of them; this one is DERIVED from `scripts/test-verify.sh` and quoted:

```stale-anchor-scenarios
ceiling-control
ceiling-fires
citation-after-url
citation-embedded-identifier
citation-trailing
citation-underscore
citation-url
citation-whitewash
citation-word-start
doc-number-reintroduced
gate-panic
gate-refuses
gofmt-violation
hang-bounded
min-declared-tests-floor
min-declared-tests-margin
netns-row-empty-domain
race-detector
readme-usage-drifts-in-the-example
readme-usage-drifts-in-the-readme
roster-gate-added
roster-gate-deleted
stale-citation
t1-violation
t2-violation
v6-fixture-mode-drift
v6-ra-absent
verdict-without-gomod
vet-violation
```

The rule is: a scenario names a path inside its copy of the tree that is not
`verify.sh`, not `verify.manifest.sh` and not under `scripts/` — directly, or
through a helper it calls. `TestStaleAnchorBoundNamesWhatTheOracleDerives`
performs that derivation and fails when it and this block differ, so the list
cannot go stale the way the sentence it replaces did. Its BOUND, stated rather
than argued away: the derivation is textual, so a scenario reaching the product
through a glob, a `find`, or a tool it runs inside the copy — with no path
written down — is invisible to it, and `suite-tests-disabled` and
`suite-partition-skip-inert` are both exactly that today. Which
is why this is the list a skipped run is not re-checking rather than a claim
that nothing else can go stale.

**`--light`, the scope, and why a scenario cannot hide behind it.** The oracle
plants one defect in a copy of the tree and runs the copy's `verify.sh` end to
end, once per scenario. A scenario that plants a shell or a document defect was
paying for the whole unit suite in its own copy in order to watch a lint row go
red. Those scenarios now run `--inner --light`, which omits the two expensive
rows and nothing else, and a run at that scope NAMES the rows it did not run on
its own verdict line. Which scenarios are light is declared in the manifest
beside the contract, applied by the oracle's dispatcher and by no scenario
body, and recorded as an observation that `verify.sh` compares against the
declaration. **A scenario cannot pass by scoping away the row it exists to
drive:** its contract demands a verdict from that row, an omitted row records
nothing, and a row that recorded nothing reads ABSENT. The manifest refuses the
combination outright, in the shell and again in Go, so it cannot be written
down.

### The two gates

- **T1 — ring 1 imports nothing that does I/O.** Enforced by parsing the import
  set, not by convention.
- **T2 — no test waits on wall-clock time.** Enforced by an identifier
  allowlist over test files, plus a wall-clock ceiling on the suite.

Both are load-bearing guarantees rather than hygiene, and both are checked
against a planted violation rather than trusted. The allowlists the gates read
are checked too, and separately: a correct gate enforcing a widened table is
the failure a gate test cannot see. What each one **cannot** see
is written down in `docs/gates.md`; read that before relying on either.

### One thing that surprised us, recorded because it will surprise the next reader

A reply from a server on the SAME HOST arrives with its UDP checksum **not
computed**. Linux writes the folded pseudo-header sum into the field and leaves
completing it to hardware, so an AF_PACKET reader on the far side of a veth
pair sees `CHECKSUM_PARTIAL` bytes. MEASURED 2026-08-29 against dnsmasq 2.91:
the captured OFFER held `0x24f6` in a field whose completed value is `0xe58c`.
Both are fixtures in `runtime/ipudp_test.go`, asserted against the captured
bytes. `0x24f6` is the pseudo-header sum for that source, destination and
length and nothing else — flip a payload octet and the completed checksum
moves while the field does not, which is a test, and is exactly why the field
says nothing about the payload.

A client that verifies the checksum strictly therefore never sends a REQUEST —
which is precisely what the first run of the dnsmasq test did, for two minutes,
retransmitting DISCOVER while the server answered every one of them. The parser
recognises that exact value, reports the payload as unverified, and the
transport counts it — though the counter says only what a field held, never
where the sender was.

**The bound, both halves of it:** neither an uncompleted checksum nor RFC 768's
zero checks the payload, so a corrupt payload is accepted under both — more
cheaply under the zero. The accepting value in the uncompleted case is not a
lucky collision either: it is a pure function of source, destination and UDP
length, all read from the frame itself, so anyone who can put a frame on the
link can compute it. Closing it needs `PACKET_AUXDATA`, whose
`TP_STATUS_CSUMNOTREADY` states the deferral as a fact instead of leaving us to
infer it — new I/O, and a later milestone.
