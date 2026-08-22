# Process ledger — how four sessions worked on 2026-08-22

This file describes how four sessions worked on 2026-08-22. Nothing here
re-derives itself. Every Rule names the measurement that motivated it and
the condition under which it dies — **check the measurement, not the
rule**, and if the measurement can no longer occur, the rule is already
dead whether or not anyone has removed it.

That last clause is load-bearing. Without it a stale rule binds until
someone does the work of killing it; with it, a stale rule is *already*
void and killing it is bookkeeping. The default when you cannot tell has
to be **"no authority"**, not "still in force".

## What this file cannot do

> A rules file has one hazard a script does not, which is worth naming in
> the file itself: a script that is wrong gets caught the next time
> someone checks its answer against reality. A rules file is never
> checked against anything — it is only ever *cited*. So the failure mode
> is not staleness, it is **citation**: someone wins an argument tomorrow
> with a line whose measurement no longer holds, and nothing in the
> process makes them look. That is why "check the measurement, not the
> rule" has to be the header and not a footnote.
>
> — client2

The underlying distinction, in one sentence: **a gate on the tree
re-derives its answer from the tree every time it runs; this file, like a
CI verdict script, never re-derives anything.** Advisory is a property of
the object, not of its age.

Hence the ordering of this document. **Incidents come first and rules are
derived from them**, not the other way round. An incident is a historical
fact and cannot rot. A rule is the perishable half. Two things follow,
and they are the reason the file exists at all:

1. **"Which rule costs more than it returns" becomes mechanical.** A rule
   is a kill candidate when its incident can no longer happen, or when a
   later mechanism already covers it. The 6b kill below could only be
   argued because the incident was nameable and it was possible to check
   *what actually caught it*. Without the incidents, that question is
   answered from feel — and 2026-08-22 is a long proof that we are bad at
   that.
2. **The reasoning has somewhere to live.** The lead has the last say and
   answers real objections in writing. Message history evaporates and
   every session keeps a differently-worded partial copy. Without a file,
   "I say why in writing" degrades to "I said why once, to one session."
   Six of eight rules that day improved because somebody argued.
   **The arguments are the asset and nothing else retains them.**

## How to read an entry

Four labels. Only the last is optional.

- **Rule** — the observer is **built**, shared, and has been driven
  **red against a violation with a message that names the violation**.
  Binding, and violations are caught. Red alone is not enough: see I15,
  where every other clause was satisfied and the message said
  *regenerate the golden*.
- **Rule, owed** — **binding**. The observer is buildable and not yet
  built; recorded as owed, **with a named owner**, until it is. This is
  not a weaker Rule. It is a Rule enforced by memory, which is the
  weakest way to enforce anything, so the owner is the point.
- **Case** — a **measured** incident that is unmechanisable *in
  principle*. Binding on judgement; cited rather than enforced.
- **Preference** — no measurement behind it. Genuinely optional, and
  never cited as authority.

**Built, not named.** The distinction is the whole taxonomy and it was
paid for. The lead's merge-verdict script called a **41-second-old**
healthy head STALLED, when their own notes had said for days that a
freshly pushed SHA looks exactly like a stalled one and the check must
hold across two polls. That is a mechanism *named* — precise enough to
implement from the sentence alone — and it was never built. **A proposal
that stops at "named" does not prevent its own founding incident.**
Naming is free; building is the bar.

**Measured-but-unmechanisable routes to Case, never to Preference.**
Preference means *no measurement behind it*, full stop. Routing a
measured finding there would strip its evidence and make it un-citable —
which is exactly the invisibility the demotion was meant to avoid,
arriving by the route nobody checked. I9 is the test case: unmechanisable
in principle, measured, and one of the most valuable entries here.

**The standing hazard: a memorable Case wants to be promoted to a
Rule**, because Rules feel more useful. Resist it. A Case turned into a
Rule is a Preference with a good story attached.

**Constitutive carve-out.** Rules about how rules are made cannot be
mechanised from inside the system they govern. They are **Rule, owed —
owner: the lead, permanently.** The taxonomy's first casualty was
itself, and saying so is cheaper than pretending otherwise.

A rule adopted without input from an active session is labelled
**provisional**, so the next reader knows which ones nobody argued with.

*Attribution, recorded as two things because they are two:* the
objection that produced this — that adoption-by-silence lets rules bind
without anyone having tested them — is **client1's**. The
provisional-labelling remedy is **the lead's**. client1 volunteered that
correction against their own credit, which is worth more than the label
it corrects.

### How this taxonomy was decided, since the file exists to retain that

Proposed as *"adopted only with the mechanism **named**"*. Objected to on
the ground that it fails its own founding incident, with the two-poll
clause as the worked example. **Amended to "built" and adopted.** Three
sessions converged on the same word from three different incidents,
independently, which is about as strong a signal as this process
produces.

A second objection — that demoting unmechanisable things to Preference
makes them invisible — was raised *by the proposer* and answered by
routing them to Case instead. **"Rule, owed" was the tier nobody had**,
added from the other side by oversight, and it is where R14 landed.

### Kill reasons

A kill needs a reason of one of these four types:

1. Another mechanism already detects it.
2. Its motivating measurement can no longer occur.
3. It misfires on cases it was not aimed at.
4. **It was a workaround for a missing observer, and the observer now
   exists.** (client2's, and the sharpest of the four: it names *which*
   machinery and gives you something to check for.)

**"It never fired" is not a kill reason** — that is what a working guard
looks like from outside.

### Carrier enumeration — required, not a nicety

Every claim in this file names its carriers where a re-check will see
them, in the **"both, or neither"** form. A claim that lives in two
sentences and is corrected in one is the defect this file exists to
record; it happened three times in the tree on 2026-08-22, twice to
people who had just written the rule down.

---

# Incidents

## I1 — a verdict is about a SHA, not about a pull request

`d803444` was pushed to #768 while oversight was mid-pass on `6fcf891`,
from edits queued before any freeze rule existed. Cost: one wasted
oversight pass, the scarcest thing in the pipeline. **What caught it:**
the author noticing and reporting within a minute — and, structurally,
the fact that a verdict names a SHA. *(client3)*

## I2 — three times in one day, two green PRs had a relationship neither PR could see

**One incident, three instances. None was caught by CI; all three were
caught by a person, by three different means.** *(client1)*

- **#757 + #761 merged into broken code.** `orphan_sweep.go` compares a
  kernel `/proc/<pid>/comm` — a *basename* — against `dhcpcdBin`. #761
  changed that constant from `dhcpcd` to `/sbin/dhcpcd`. Always unequal,
  so `SweepOrphans` returned a confident zero and #722's SIGKILL path
  never ran. #757 was correct on its own head; #761 did not contain
  `orphan_sweep.go` at all. Merged six minutes apart, both honestly
  green, **no textual conflict for git to report**. Cost: a silent
  regression on dev, fixed by #765. *Caught by a human noticing the sweep
  had gone inert.*
- **#764 → #766.** Detected *before* merge only because the merge product
  was built in a real worktree and the lane run on it. Cost: one rebase.
  *Caught by building the merge product deliberately.*
- **#767 → #766.** #766 had green on all 8 required contexts **and an
  explicit CLEAR at `0ea8b68`** — both merge conditions satisfied — and
  could not merge, because #767 landed as `d9643e5` and took the same
  golden. Cost: a re-pass by oversight. *Caught by polling `mergeable`
  instead of trusting green.*

**The pattern:** a PR is green against *its own head*, never against the
product of itself and whatever merges next. Instances 1 and 3 are the
same defect at different times — 1 found after the damage, 3 before it.

**Git's silence is not evidence.** In instance 1 there was no conflict at
all; in instance 3 the *conflicting* file was the harmless one while the
files that automerged cleanly carried the risk.

## I3 — a number went false a minute after it was frozen, through no act of its author

RELEASE_NOTES.md said "fifteen new health counters". #767 merged at
18:36:46Z bringing `conflict_probe_stale_addrs`; the reviewed head had
been committed at 18:35:28Z. **What caught it:** oversight re-measuring
rather than re-reading — `HealthResponse` json tags, 41 at v1.7.1 → 57 on
dev, so 16 new.

**The structural half, which is worth more than the fix:** #768 asserts
facts that are functions of what has merged, and it merges last. Any
CLEAR given to it is valid only until the next merge — *including a CLEAR
given correctly, driven, at a frozen head*. Not fixable by a better pass
or a stricter freeze.

**And removing the number exposed an older defect.** Making the table the
count forced the table to *be* the set, and it was not: 14 rows against
16 counters. `tombstone_quarantines` was named in prose and missing from
the table, and `conflict_probe_stale_addrs` was absent. **The number and
the list had already disagreed before #767 merged**, and nobody could see
it because the number was doing the reader's counting for them.
*(oversight, client3)*

## I4 — the same confident zero, measured wrong two different ways

Counter observation was measured twice and both were wrong, both
returning plausible zeros:

- Grepping the snake_case exposition name across `*_test.go` reported
  `network_options_rejected: tests=0`. It has **ten** assertion lines in
  `stored_options_names_test.go` — the tests name the Go atomic, not the
  string.
- Deriving lowerCamel from the struct field produced `dHCPRoutesApplied`
  where Go spells it `dhcpRoutesApplied`, giving four more false zeros.

**What caught it:** reading the atomics out of `plugin.go`'s own
declarations with a refuse-below-30 floor. Widening past `pkg/` then
revealed two more counters asserted by
`test/integration/dhcp_server_policy_test.go`. *(client3)*

## I5 — a harness scored an unrun mutant GREEN

The mutation harness gated on `go build ./pkg/plugin/`, which does not
compile test files. A mutant the tests refused to compile against
produced zero `--- FAIL` lines, and zero was read as survival.
*(client2)*

## I6 — parsing a file is not running it

`test -x a b c` checked nothing and broke the build while the lane
reported 50/0/0. The Dockerfile gates parse; none execute.
*(master-release)*

## I7 — a `-run` filter silently narrows the OBSERVER set

Four mutants against the shape gate. Three died. The fourth — repointing
`mountBin` to `/usr/bin/mount`, absolute but not what the image contains
— *appeared to survive*, and died the moment the run stopped being scoped
to `-run TestMountPrep`. What killed it was the Dockerfile parity test.
**A mutant that "survives" can be an artifact of how the tests were
invoked rather than a gap in coverage.** *(master-release)*

## I8 — the sweep was declared done at two copies, and there were three

`docs/internals.md:365` was the third and the worst: a **present-tense
heading** stating the opposite of the PR title, in a teaching document,
with no diff drawing attention to it because nothing else in that PR
touched the file. Found by a reader, after the author's own grep rule had
been applied and the sweep called complete. *(client1)*

## I9 — a stale claim one bullet away from the text that corrects it

`docs/internals.md` also said *"The snapshot is not a single atomic
instant, and that is deliberate"* in the bullet **immediately above** the
one flagged in I8. True of a counter read on its own, and exactly the
reasoning that licenses #730 the moment a rendered value is *combined*
from two. **No grep would have found it** — it contains neither the
symbol nor the claim. Found by asking what the *neighbouring* text now
implies.

> *"I do not know how to make that a rule, which is why I want it said
> plainly: that is the part of review that does not reduce to a
> pattern."* — master-release

## I10 — the right answer from a false premise

A derivation offered as evidence: *"#729 is dev-only because `git ls-tree
v1.7.1 pkg/plugin/` has no `resolvconf.go`."* It does — both
`resolvconf.go` and `resolvconf_test.go` are present at v1.7.1. The
conclusion held for a different reason: `openContainerProc` does not
exist there. **A derivation that reaches the right answer from a false
premise is the most dangerous kind to keep: it gets reused, and next time
the premise decides the answer.** *(client3, caught by oversight)*

## I11 — "no answer" arrived in the same variable as an answer

`gh pr view --json mergeable` returned **`UNKNOWN`** on first read after a
base change; GitHub computes mergeability lazily. It took four polls to
resolve. **`UNKNOWN` is not "not mergeable"**, and reading the first
answer as a verdict would have produced a confident wrong result in
*either* direction. *(client1)*

Same family: `gh api --jq` writes a 4xx error **body to stdout**, so a
failed call is not empty and walks straight past `[ -z "$SHA" ]`. Guard
on **shape** — 40 hex — not on emptiness. *(client2)*

Seen again the same evening on #769, after `dev` moved underneath it:
`mergeable=UNKNOWN` while GitHub recomputed. **Not a blocker, and not a
verdict** — it resolves on polling. The tell is that the instrument has
one variable for "the answer is no" and "I have not worked it out yet".

**And the same shape in our own tooling, which is the version that
should sting:** a check whose only observed behaviour is passing has one
possible verdict. Exercising a script's **refusal** path before its
success path ever runs is the cheap fix — one run against a tree where
it must decline. Done for both the post-#766 and post-#769 edit scripts;
each correctly refused and left the tree untouched.

## I12 — the rule was right and the application was one short

Within one commit: R14 (enumerate a claim's carriers) was written down,
and the tag-time comment applying it named **two** carriers of the
nothing-was-deferred claim. There were **three** — the security-review
section also says *"Everything found is fixed in this release; nothing
was deferred"*, scoped to #457/#699 rather than to all three reviews.

**What caught it:** a mechanical sweep — count-words and merge-state
verbs over all 1,179 lines of the section, with a refuse-below-200-lines
floor. Not a careful reread. The prose had already been read twice.

oversight's statement of it is better than the one this file started
with, and is kept verbatim:

> **The rule was right and the application was one short, which is the
> ordinary way rules fail. Not by being wrong. By being applied by the
> person who just wrote them, in the same sitting, when they are the
> least able to see what they have missed.**

The same sweep found two more, both also in prose its author had reread:
a merge-state count (*"the seven described below are merged"*) and a
parenthetical describing a branch that merges *before* the file
containing it. *(client3, framing by oversight)*

## I13 — a documented derivation that does not reproduce its own table

Not a failure — a check that could have been one, recorded because the
*method* is the transferable part. #768 shipped an HTML comment carrying
the command that derives the counter table. oversight's re-check ran
**that command** rather than their own grep, and diffed its 16 names
against the table's 16 rows, empty both directions.

> A documented command that does not reproduce its own table is the same
> defect one layer down.

The author had verified the table against their own command, in their
own shell, in the session that wrote it — I12's position exactly. The
first independent evidence the comment was worth anything came from
someone running the comment. *(oversight)*

## I14 — a mutant that panics names the wrong observer

Dropping the nil-plugin guard in `notePIDMismatch` (#707) makes a
full-package run report `--- FAIL: TestStart_SurvivesADaemonThatWillNotAnswer`.
The actual guard, `TestNotePIDMismatch_SurvivesANilPlugin`, **never
appears in the output at all**, though it kills the mutant correctly when
run scoped. The panic aborts the test binary and Go credits whichever
test the runner happened to be naming.

**The survival direction is the one that bites.** An early panic can make
an **innocent test look like coverage**: the run is red, someone reads
the name, and a guard that does not exist gets written into a table as an
observer. That is worse than a missed kill, because it manufactures
evidence rather than losing it.

**Rule, owed — owner: client1.** Any mutation row whose output contains
`panic:` is unattributed until re-run with `-run '^TestExact$'`.
*(client1)*

## I15 — an observer that is red, shared, and still not enforcing

A seventh family-split metric **does** go red today, on two tests. Every
clause of *built, shared, driven red against a violation* is satisfied,
so the rule grades as enforced. **It is not enforced**, because the
failure message says *regenerate the golden*. The author regenerates,
goes green, and ships the defect.

Measured on both sides of the same tree: at `637cc8c` the mutant's
message names the golden; at `05d94bc` it names the unheld counter.

**So the standard is amended:** an observer must go **red, and its
message must name the violation.** The diagnostic is client1's:

> *When it fires, does the text tell someone who has never read the rule
> what they actually broke?*

This and I2 are one finding at two levels. **Git's silence is not
evidence, and a detector's noise is not evidence either. What matters is
whether the thing that fires is the thing that would have to be right.**
*(client1)*

## I16 — two measurements agreed, about different objects

A hand-off about #766 stated *"the exposition goes 41 → 63 series, 22
new"*. Measured on its actual head `05d94bc`, confirmed against
`gh api .head.sha` rather than transcribed:

```
                                   dev      #766
metrics_exposition.golden series    57        57     <- unchanged
  of those carrying family=         12        12     <- unchanged
HealthResponse json tags            57        63     <- +6
```

**#766 adds six fields and zero series.** The `family="ipv4"` series
already rendered; #730's whole point is that they stop being derived by
subtraction and start being stored. And `41` is not an exposition number
in either direction — **there is no `metrics_exposition.golden` at
v1.7.1**; 41 is the v1.7.1 json-tag count, a count of fields.

63 and 22 are correct **for fields** and were attached to **series**. The
recipient's own field measurements were also 63 and 22, so the two claims
read as mutually confirming.

> **Agreement between two measurements of different objects is worth less
> than disagreement between two measurements of the same one.**

The `41 →` end was verified independently rather than taken:
`git cat-file -e v1.7.1:pkg/plugin/testdata/metrics_exposition.golden`
**fails**. The object the claim points at does not exist at that ref.

**This is a new class, and it survives every control built that day.** It
has a real measurement behind it. It names its instrument. It passes *a
count needs a file:line*; it passes *driven, not read*. It fails only on
**which object the instrument was pointed at**, and the two numbers
matching is what made that invisible.

**How it was caught is a Case and not a technique**, and is labelled one
so nobody mistakes it for a procedure: it was not arithmetic. *"Six new
fields produce six new series"* did not sound right for a change whose
entire purpose is that the series already existed. Domain sense noticing
that a number was too tidy. Nothing checks that.

The nearest thing to a mechanism is the discipline it implies, and that
part **is** checkable in prose review — see R17. *(client3, verified
independently by master-release)*

## I17 — three hand-written mirror guards, one evening, three authors

A check whose expectation is **written by the same hand, in the same
sitting, as the thing it checks**. Three instances, in three files, by
three different people, all measured on 2026-08-22:

- `familyPairs()` checked against a literal `6`.
- The counting-wrapper AST table keyed on a **name**.
- `want := map[string]bool{dhcpcdBin, mountBin, mkdirBin}` — **a third
  copy of the set the test exists to stop becoming two copies.** It
  guarantees three of five: `unsharePath` and `/bin/sh` are unguarded,
  and both oversight and client2 proved it independently — relocate
  `unsharePath` and the whole repo stays green.

**This is I12 generalised out of prose and into code.** There, a rule's
author under-counted its subject in the commit that introduced the rule.
Here, an author wrote a test specifically to stop this class and
instantiated the class while writing it. Same failure, and the same
argument: **mechanical, not careful.**

The corollary, with three instances behind it:

> **An expectation must be derived from the thing it checks, or the check
> is a copy with a test's name on it.**

All three fixes are one move: derive the expectation from the thing it
checks. **The recognition is the Case; the three fixes are Rule-owed,
each with an owner** — because a Case accumulates for free, while a
Rule-owed carries a cost and a name, which is the only thing that makes
the standing kill ratio move. A taxonomy where everything lands in the
free tier only ever grows.

| owed | owner | state |
|---|---|---|
| derive `familyPairs()` from `metricDefs()` | client1 | **closed in `34ef250`** — the seventh-metric mutant re-run on `05d94bc` reddens with a message naming the defect rather than the golden |
| key the counting-wrapper row on the field, not the name | client2 | open, in #769 |
| derive `want` from the package's own `^/` constants | client1 | open, on `fix/707` |

*(master-release, with oversight and client2)*

## I18 — a green destroyed by an edit that changed no code

#766's green was invalidated repeatedly at a head that never moved.
Measured on `05d94bce96349857a35231c4190de5b13d54c51e`:

```
check-runs on that SHA                    35
`test` starts on that SHA                  3
  18:57:44Z  completed  cancelled
  19:00:30Z  completed  success      <- a real green
  19:05:12Z  in_progress              <- and it is already superseded
```

Each start pairs with an `Issue state labels` run on
`pull_request_target`, which is the signature of a **pull-request body
edit**. Not one byte of the tree changed.

**A green can be destroyed by an action that changes no code.** Same
family as *a CLEAR expires on every subsequent merge*, arriving from the
direction nobody looked: that one is invalidation by **other work**, this
is invalidation by **no work**.

The other half of the mechanism was already known: editing a **merged**
PR's body re-runs the range gates against a deleted branch and paints it
red permanently. Same cause, opposite end of the lifecycle.

**Operational rule:** finish body edits, *then* take the verdict, *then*
merge — and do not touch the body in between. Otherwise the strong test
flips back to NOT-YET-DONE indefinitely and the PR never converges.
*(master-release, re-measured by client3)*

## I19 — two instruments disagreed, and the one being quoted was wrong

Chasing an unrelated correction, a golden-churn histogram reported **47
changed lines** where `diff | grep -c '^<'` reported **48**. The
histogram script had parsed **55 of 57 series** and **misread one value**
— `lease_changed{family="ipv4"}` as `190 → 120` where dev holds `200` —
so its `-80` bucket printed `-70` and its `+60` bucket was one short.

That output had been quoted across two rebases and had reached a pull
request body.

**Fifth instrument-answered-confidently instance of the day, and the
first found by cross-checking rather than by the answer looking wrong.**
Note the direction of the failure: the broken instrument produced a
**plausible** number, one off — not a crash, not a zero.

> **A count that does not reconcile with the tool beside it is not a
> measurement.**

The replacement asserts parsed-count against series-count *and*
changed-count against the diff count, and **refuses rather than
printing**. Corrected figures: 57 series both sides, 48 changed,
`{-80:1, -60:1, -40:1, -30:3, +60:42}` — 42 of the 48 pure index
arithmetic, which is v1.9.0 item 18's evidence with the right
denominator. *(client1)*

## I20 — a claim that goes false under a NON-merge

Every other claim this cycle went false when *something else merged*.
This one goes false when something else **does not**.

`RELEASE_NOTES.md` asserts that all ten lifecycle faults are fixed in
this release. Derived from the milestone rather than from memory: nine of
the ten carry `in-dev`; **#731 carries `has-pr`**, so it is not on `dev`.
The sentence is a forward claim resting entirely on #769 landing.

**And there is no durable phrasing.** "Nine of ten" is wrong the other
way, and wrong at exactly the moment it matters most — after the tag. The
sentence is not a claim about the present that decays; it is a claim
about *the release*, and the only way to make it true is to make it true.

> **This is a sequencing constraint, not a staleness bug. #768 must not
> merge before #769.**

Worth separating from the rest because every control built for the other
species is useless here: the table cannot go stale silently because a
derivation reproduces it, but **nothing derives the ten-issue claim** —
three sentences carry it and only a human re-running the milestone query
sees a change. Careful wording does not touch it either. It belongs on
the merge order, not in the text. *(client3, framing by oversight)*

## I21 — GREEN by the strong test, on a head carrying a proven defect

Run against #769 while it was on HOLD: **all 8 required contexts present
and success, exit 0.** The head carries a defect oversight proved —
below a short `lease_timeout` the server-policy ladder still starves
every attempt — and the test that would catch it **names the correct
behaviour in its title and asserts neither half of it.**

CI has nothing to say about this and never will. There is no stricter
verdict script that reaches it, because the verdict script's subject is
whether the checks passed and the checks are the thing that is wrong.

> **Green is a fact about the checks. It was never a merge decision.**

This is the single strongest justification for requiring an explicit
CLEAR alongside green, and it arrived as one command's output rather
than as an argument. *(master-release)*

## I22 — a check that transcribes its own subject, in the check built to catch that

A tripwire was built to guard the release notes' claim that ten
lifecycle faults ship. It read the milestone and refused if any of the
ten lacked `in-dev`. Its author described it as a derivation. **The ten
issue numbers were written into it by hand.**

So it could see *"one of the ten I wrote down is missing"* and could
never see *"the ten are not the ten"* — an eleventh finding, an issue
moved off the milestone, one of the five reparented away from its
tracker. All pass silently.

**It is fully derivable, measured:**

```
milestone 22 + code-review + lifecycle : 11    <- eleven, not ten
  #726 sub_issues_summary.total = 5            <- the tracker, excludes ITSELF
  every other member             = 0
  findings                             : 10
  NOT in-dev: #731
```

**Excluding the tracker by number would have put a magic number inside
the script whose whole job is catching magic numbers.** `sub_issues_
summary.total > 0` removes it by shape instead.

**Five instances, five files, five authors, one night** — and the first
that would have been shipped knowingly:

1. `familyPairs()` checked against a literal `6`.
2. An AST table keyed on a name rather than a field.
3. `want` as a **third** copy of the set it guards (I17).
4. This tripwire, transcribing the ten it exists to check.
5. The test harness itself: a `gh` stub answered the history query with
   a **pre-computed scalar**, so `verdict.sh`'s own jq never ran and p90
   was indistinguishable from max *by construction* — a stub written
   specifically to stop the suite mirroring the script, mirroring it at
   exactly the call the change under test lived in.

**The common property is not carelessness: every one was written by
someone who had just been thinking about this exact class.** And the
pattern across the five is sharper than that — **we spent the evening
checking the code for mirrors and never checked the instruments.**

**What deriving the subject buys, stated narrowly:** it widens what the
check can *see*. It does not make it fire, and it does not replace the
sequencing constraint.

> **A derived check that nobody runs is a transcribed check with better
> provenance.**

*(oversight, verified independently by client3; framing from
master-release)*

**Caveat that ships with it:** it reads labels and milestones, which are
human artefacts — it derives the labels' state, not the code's. Strictly
better than a transcribed list; strictly weaker than a derivation that
reads source. This repo has already labelled the wrong issue by
following a `(#675)` in a subject into a pull-request body.

## I23 — the family with "no driven suite of any kind" had 56 of them

A Rule-owed was assigned on the premise that the `check-*.sh` family has
**no driven suite of any kind**, and that all seven of its historical
incidents were found by someone tripping over a gate in production use
rather than by a test.

```
scripts/check-*.sh              : 56
scripts/test-*.sh               : 74
gates WITHOUT a self-test       : 0
driven by                       : scripts/local-lane.sh:121
                                  "gate self-tests|-|bash scripts/run-gate-selftests.sh"
```

They drive failures, not clean paths only. `test-check-version-pins.sh`
runs seven cases through `run_check <name> <want_exit>`, three expecting
non-zero, and asserts the gate **names** the offending version rather
than merely reddening — which is the standard the Rule-owed was about to
introduce, already built, in the family thought to have nothing.

**The corrected finding is sharper than the claim it replaces.** The
seven incidents happened *in the family that has suites*. The suites
were not driving the thing that broke: `check-version-pins` matching
only well-formed pins is not a missing test, it is **a test that shared
the gate's blind spot, by construction, because the same hand wrote
both.** So the owed observer is not "every gate needs a suite" — it is
*a suite must be driven against inputs the instrument was not written to
expect*. And the genuinely unobserved layer is the one **above** the
gates: `verdict.sh` until that evening, the mutation harnesses, the
waiters, the ad-hoc scripts.

**Two attempts to size the gap, both wrong, both refuted by one sample.**

| attempt | said | refuted by |
|---|---|---|
| regex for a literal asserted exit code | 49 of 56 never drive a failure | `test-check-version-pins.sh` drives three, through a helper comparing `"$got_exit" -eq "$want_exit"` — a variable |
| same, plus version-pins as a control that **must** classify as driving | 19 of 56 | `test-check-lane-hygiene.sh`, sampled *from the nineteen*: "a lane with no teardown should exit 1", "an empty workflow directory is rc2, not a pass" |

Neither number is reportable and neither was reported. What survives is
bounded: **at least 37 drive a failing verdict, the true figure is
higher, and it could not be measured with a regex** — a pattern over
shell that must recognise arbitrary helper indirection was never going
to work, and one read of one file said so in thirty seconds.

**The control set is what caught the first. Sampling the negative bucket
is what caught the second, and it is the cheaper control.** A control
proves the instrument can see a positive; only sampling what it calls
empty proves it is not lying about the absence — **and absence is the
answer this class always gives.**

*(premise from master-release, measured and corrected by client3;
master-release verified the correction independently and re-scoped the
Rule-owed rather than dropping it.)*

---

## I24 — a `+` in a struct diff is not a new field

#769's diff shows three added lines carrying a json tag:

```
+	DHCPServerPolicyTimeouts int32 `json:"dhcp_server_policy_timeouts"`
+	DHCPTimeouts             int32 `json:"dhcp_timeouts"`
+	LeaseReleaseFailures     int32 `json:"lease_release_failures"`
```

**One is a new field.** gofmt realigns the whole block when the longest
name changes, so two untouched fields are rewritten and appear as
additions. The set difference says one: `dhcp_timeouts` is line 11 of
`origin/dev`'s tags and `lease_release_failures` line 26. 63 tags on
dev, 64 at the PR head.

Reading the diff would have put **three** rows in the release notes, two
of them for counters that shipped in an earlier release — and both would
have looked right, because the field names are real and the counters
exist. This is I16's shape in a new carrier: **a plausible answer about
the wrong object.** The diff answers "what did this commit rewrite"; the
question was "what does this commit add".

---

## I25 — a pre-flight that silently tested the unmodified thing

Two appliers were built for the release-notes edit, each refusing until
its precondition held. Both refusals were driven and both name their
violation. **Neither edit half had ever executed** — each sat behind a
refusal and would first run at the moment it must not be wrong. That is
I15's shape inverted: not a check that has only ever passed, but an edit
that has only ever been skipped.

So both were driven against copies, with the pull request's head served
in place of `origin/dev`. **The harness failed twice while reporting
success.**

1. A substitute `sh()` was injected into the exec globals. The subject
   defines its own `sh()`; the exec overwrote it.
2. A substitute `subprocess` module was injected. The subject's own
   `import subprocess` overwrote that too.

Both runs printed **63 tags — the real number, for the wrong tree.** The
only reason it was caught is that 63 was already known from
`origin/dev` and the run was supposed to show 64.

**Anything handed IN through the globals can be shadowed by the
subject's own definitions.** The fix was to patch the shared module
object, which the subject's `import` then resolves *to* — plus an
assertion that the interception fired at all, because twice it had not
and said nothing.

> **A pre-flight that silently exercises the unmodified thing is worse
> than no pre-flight, because it reports success.**

Same evening, same author, one turn after writing I23's rule about
sampling the bucket an instrument calls empty.

---

# Rules

Each names its incident and the condition under which it dies.

| # | Rule | From | Dies when |
|---|------|------|-----------|
| R1 | Every fix gets a test that is RED against the real pre-fix tree, not against a hand-built fixture. | I5, and the mirror-fixture regression in I2 | Never expected to; this is the base case. |
| R2 | Never weaken a failing test. A `t.Skip` for "host variance" counts. | `check-test-weakening.sh` findings | The gate covers all three cases without a waiver route. |
| R3 | Never merge a red check. **An ABSENT check is not a green check.** | a merge made while suites were still running | Branch protection refuses to report a PR mergeable with a required context missing. |
| R4 | The issue ref goes in the **commit subject**, not only the body. | `in-dev` derivation missing a body-only `Closes #N` | `check-issue-ref.sh` reads bodies as authoritative. |
| R5 | No AI attribution anywhere — no trailers, no session lines. | standing | Never. |
| R6 | No internal hostnames, gateway IPs or LAN details in any public artifact. | standing | Never. |
| R7 | No comment posted on an issue or PR without the lead seeing the draft. | standing | Never — this one is about publication, not correctness. |
| R8 | `git add -A` **before** `scripts/local-lane.sh`. Index-discovery gates cannot see untracked files. | a lane that passed over files it could not see | Gates discover from the working tree rather than the index. |
| R9 | At most two concurrent integration runs. | 4 self-hosted jobs per run against a pool of 8 | The pool grows, or jobs stop being self-hosted. |
| R10 | Set PR fields with `gh api ... --method PATCH` and **read them back**. `gh pr edit` fails here with a projects-classic error and **writes nothing silently**. | measured | `gh` is upgraded past the projects-classic break. |
| R11 (6a) | **A CLEAR names its SHA.** A CLEAR whose SHA is not the current head is refused. | I1 | A merge queue makes the reviewed head and the merged head the same object. |
| R12 | Green **and** CLEAR are both required. Either alone is nothing. | I2 instance 3 — green *and* CLEAR, still unmergeable | Something evaluates the merge product before it becomes dev. |
| R13 | Derive names from the declaration, not from a naming rule, and **refuse below a floor**. Report what you inspected. | I4 | The floor rule ships as a declaration gate (v1.9.0 ledger item 16). |
| R14 | Every claim enumerates its carriers where a re-check will see them: **both, or neither**. **Rule, owed — owner: client3.** | I8, I3, I12 | The carrier-enumeration gate exists. |
| R15 | You may push a head that is with a reviewer; you **must** tell them in the same minute, and the restarted pass is on you. | replaces the killed 6b, below | R11 stops being the detector. |
| R16 | Run the mechanical sweep against **your own** work first, not last. **Rule, owed — owner: client3.** | I12 | The carrier-enumeration gate exists — this is the same gate pointed at a different moment, so R14 and R16 may collapse into one piece of work. |
| R17 | **Name the object, never the bare number.** "63 fields", not "63" — and never a noun borrowed from the neighbouring sentence. **Rule, owed — owner: client3.** | I16 | A gate can check that a numeral in changed prose is followed by the noun it counts; this may fall out of R14's gate for free. |
| R18 | A summary count **reconciles against a second tool before it prints**, or it refuses. | I19 | Never — this is a shape, and it costs one assertion. |
| R20 | A check **derives its own subject**. Writing the set it guards into itself makes "the set is wrong" invisible — and excluding a member by name is the same defect wearing a smaller costume. | I17, I22 | Never; four instances in one day, all by authors thinking about this class at the time. |
| R19 | Where this file records that something happened, **prefer a SHA to a status word.** A SHA is checkable forever; a status word is a present-tense claim nothing updates. | I3, and the owed table below | Never. |
| R21 | **Sample the bucket your instrument calls empty.** A control set proves it can see a positive; only sampling the negatives proves it is not lying about the absence. | I23 | Never — it costs one file read, and absence is the answer this class always gives. |
| R22 | A harness that substitutes a dependency **asserts the substitution fired**, and substitutes at a level the subject cannot shadow. | I25 | Never — one assertion, and it caught nothing twice because it was absent. |
| R23 | Ask a diff what changed; ask the **set** what exists. A `+` line is evidence of a rewrite, not of an addition. | I24, I16 | A formatter stops realigning unrelated lines, which is not a thing formatters do. |

## Owed — R14, R16 and R17, and why they are hard

R14, R16 and R17 are all binding and none has an observer. **They are
probably one piece of work**: R14 asks whether a claim's carriers were
all touched, R16 asks that the check run before review rather than after
it, R17 asks that a numeral in changed prose be followed by the noun it
counts. The same gate at three moments — and R17 is the only one of the
three a machine could plausibly do well, because "is this numeral
followed by a noun" is a syntactic question and "is this a claim" is
not.

R14 is binding and has no observer. It was volunteered for demotion by
its own owner and kept binding instead, on the grounds that honesty
about a rule's weakness is not licence to weaken it: R14 has caught
three instances in one evening, two of them in its own author's prose,
and is enforced entirely by memory.

**What has been considered, so the next person does not rebuild the
thing already ruled out.** The obvious shape — a gate that reads changed
prose and recognises "claims" — is the fuzzy pattern-matching that
produced `check-version-pins` matching only well-formed pins. A gate
cannot derive what a claim *is*. Do not build that.

**The only shape worth trying is the declaration form**, the same as the
floor gate: a change touching a claim-bearing document declares which
claims it moved, and the gate checks mechanically that every declared
claim's carriers were all touched — not that the declaration is
*correct*, which is unknowable, but that it is *complete with respect to
itself*. **The gate cannot know what a claim is. The author can.**

That is a sketch and not a design. "We tried and here is why it is hard"
is a legitimate entry, and it is worth more here than an unattempted
promise.

## Killed

**6b — the head freeze** (*"once a PR is with oversight, its head is
frozen"*). Killed the same day it was made, **kill reason 4: it was a
workaround for a missing observer, and the observer exists.**

6b was priced as a safety rule and never was one. Check its own incident:
the `d803444` collision (I1) was caught by *the verdict naming a SHA*,
not by any freeze. **R11 is the safety property. 6b prevented nothing
except a wasted pass** — a courtesy. A detector beats a prohibition: R11
fires at the point of use with nothing to remember.

The evidence that the prohibition was mispriced is how it died: a green
PR sat still because a rule designed to save a reviewer's time was
spending an author's, on an edit the lead had asked for — and by then the
rule had already been killed, in a message that had not yet arrived.

---

# Cases

Cases command nothing. They are here because a pattern recognised is
worth more than a rule followed.

- **I5 / I6 / I7 — an unrun measurement reads as a result.** A harness
  that compiles nothing, a `test -x` that parses, a `-run` filter that
  narrows the observers. In all three the measurement never ran and
  nothing said so. The actionable half is already R13; what remains is
  the *shape*, which is the part that transfers.
- **The inverse control, and it is what makes a mutation table mean
  anything:** a table is evidence only if the same harness has been shown
  to report **SURVIVAL** against a test that genuinely cannot see the
  defect. On #766, 24 double-`Load` mutants report 24 dead against the
  new concurrency test and **24 survived** against
  `TestMetrics_GoldenExposition`. Without that second run, "24/24 killed"
  and "the harness silently did nothing 24 times" are the same output.
- **I9 — read the text *around* each hit.** A claim that survives a
  mechanical sweep is more often adjacent to the one that was corrected
  than identical to it. Not gateable.
- **I10 — a right answer from a wrong premise survives review and is
  reused.** Check the basis of a derivation even when you agree with
  where it lands.
- **I3 — remove the number, not the error.** A count beside a list is a
  second copy that can disagree with the first. Deleting the count is a
  fix with no failure mode; correcting it is a fix with a shelf life.
  *Removing the number also exposed the list being wrong, which the
  number had been hiding.*
- **I12 — run the mechanical sweep against your own work FIRST, not
  last.** A rule enforced by its own author in the same sitting is
  enforced by memory at its weakest point. Both residuals the #768 sweep
  found were the author's, in prose already reread twice, and the sweep
  found them only because it was mechanical rather than careful. This is
  also the strongest argument for the mechanism proposal, arriving from
  a direction neither party used to make it.
- **I13 — check a documented derivation by RUNNING it, not by
  reproducing its result.** Agreement between your grep and their table
  proves the table. Only running their command proves the *comment*, and
  the comment is what the next reader will use.
- **A symptom observer looks idle right up until the day it is the only
  thing watching.** Driven over 24 double-load mutants, disabling one
  property at a time: **P1 alone kills 24/24, P2 alone 0, P3 alone 0**,
  and P1+P2 with P3 off still kills 24. So P3 kills nothing P1 does not
  already kill — **and it stays**, because it is the only property
  stated in the operator's terms, and the only one that would fire if
  the load-once invariant held and a counter went backwards anyway.

  > **A redundancy measurement is not a deletion argument.**

  This is the correct counter to the instinct to delete what nothing
  observes, and it is the same reason *"it never fired"* is not a valid
  kill reason. *(client1)*
- **Measure the object, not the thing standing next to it** — the tree
  not the pull request, the field count not the series count, the claim
  not the proxy. Four times in one evening.

  **And the honest half: three of those four times the proxy would have
  given the same answer.** It cost a round trip each time and paid once.
  That is the same shape as R17's 26 candidates producing one real
  finding, and it is the same defence: **judged on precision this habit
  dies; judged on recall it is the only thing that caught #766's series
  claim, the transcribed tripwire, and a table that was six rows short.**
  The two belong together — when the miss is silent and the false
  positive is a glance, cheap-and-usually-redundant is the correct trade.
- **Wait on the effect, not on the precondition.** A waiter armed for
  #766 watched `origin/dev`'s tree — the `HealthResponse` json tags —
  rather than the pull request's merge state. *"The PR merged"* and
  *"dev carries the fields"* are different claims, and only the second
  is what the dependent work needs. A waiter keyed on merge state
  asserts GitHub's opinion about a merge; one keyed on the tree asserts
  the tree.

  **This is the third costume the same class wore in one day.** The
  first was `container_netns_test.go` asserting the plugin's opinion
  (the error sentinel) and never the evidence (the counter). The second
  was a golden exposition file proving a counter *renders* and
  establishing nothing about whether anything increments it. Measured,
  unmechanisable in general, and worth recognising by shape:
  **something's opinion about the state is not the state.**
- **Resolve a generated file by regenerating, never by taking a side** —
  taking a side is a hand-written expectation wearing a generated file's
  clothes. Then **write down the churn you predict before you diff**. A
  prediction that matches is evidence; one that misses by a line is a
  question worth asking. *(client1, three times in one day)*

---

# Preferences

Labelled, and not citable as authority.

- Status reports are three lines: head SHA, verdict, what you drove.
  Length is for disagreeing with an instruction, or for a finding with a
  measurement behind it.
- One reporting thread when several sessions run: peers report to the
  lead, the lead aggregates.

---

# Standing question, every round

**Which rule is costing more than it returns?** Answer it with a kill
reason from the four types, or say "none" and mean it.

**Judge a rule on recall, not precision, when the miss is silent and the
false positive is a glance.** R17's first run over a release-notes
section produced **26 candidates, 25 of them ordinary pronouns and one
real** — a count that had borrowed the wrong noun from its neighbouring
sentence, in prose its author had reread four times. A rule judged on
precision dies at 1-in-26. The value was never the hit rate; it was that
the one real instance was invisible to four careful readings and took a
mechanical pass forty seconds to surface.

This matters *because* of the kill question rather than despite it: a
kill test that measures precision removes the good rules first. The cost
side of the ratio is what a false positive costs to dismiss, not how
many there are. On 2026-08-22
eight rules were added and one was killed. That ratio is the thing this
section exists to move.
