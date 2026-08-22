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

What caught it was not arithmetic: *"six new fields produce six new
series"* did not sound right for a change whose entire purpose is that
the series already existed. *(client3)*

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

## Owed — R14 and R16, and why they are hard

R14 and R16 are both binding and neither has an observer. **They are
probably one piece of work**: R14 asks whether a claim's carriers were
all touched, R16 asks that the check run before review rather than after
it. The same gate, pointed at a different moment.

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
reason from the four types, or say "none" and mean it. On 2026-08-22
eight rules were added and one was killed. That ratio is the thing this
section exists to move.
