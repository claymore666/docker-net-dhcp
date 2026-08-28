#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every scheduled workflow declares what it judges, and the declaration
# decides whether its checkout is pinned to `dev` (#839).
#
# THE HAZARD. A cron only ever fires from the DEFAULT branch. Measured
# 2026-08-27: 100 of the 100 most recent scheduled runs ran on `main`,
# none anywhere else (#846). So a scheduled workflow checks out `main`
# unless it says otherwise — and `main` is a release behind `dev`.
#
# For a workflow that scans THE TREE that is correct: a scanner
# scheduled against the default branch should scan the default branch,
# and pinning it to `dev` would make it judge something nobody has
# released. For a workflow that judges THE TRACKER it is wrong, because
# the tracker advances when work merges to `dev`. A label deleted by a
# PR sitting in `dev` is already gone from the tracker while `main`'s
# .github/labels.yml still declares it, so the daily run reports drift
# that does not exist and tells the reader to re-create something
# somebody deliberately removed — a red naming the wrong remedy,
# standing for a whole release cycle.
#
# WHY A GATE AND NOT A COMMENT. This was fixed once, in
# issue-state-labels.yml (#490), and the fix reached none of its three
# siblings for three releases. ALL THREE of them carried a comment
# describing the hazard they were suffering from, and this change
# removes all three. Measured at dd5e3fe -- a fixed tree, so the
# citation does not go stale when this branch is rebased:
# label-taxonomy.yml:48, milestone-scope.yml:38, starter-tasks.yml:24. The comment was correct and changed nothing,
# which is the finding #839 actually reports: the defect is that ONE
# workflow was fixed and nothing reconciled the others against it.
#
# This paragraph said "two of them" until somebody counted. It
# understated its own argument, which is the cheap direction to be
# wrong in -- and it is still a count in a header meant to be the
# durable record, so it carries a file:line for every item now.
#
# WHY THE CLASS IS DECLARED RATHER THAN DERIVED. Two derivations were
# measured against the real corpus and both fail:
#
#   * "do the scripts this workflow runs query the tracker?" — splits
#     the corpus correctly except for test.yaml, which invokes
#     check-label-taxonomy.sh --static and check-good-first-issues.sh
#     --static. The evidence is the whole script file; the flag that
#     makes those two invocations offline is invisible to it. A false
#     positive that would demand a pin on the PR lane.
#
#   * "does it declare the `issues:` permission?" — splits the corpus
#     exactly, and still fails: this repository's
#     default_workflow_permissions is `read`, so a new sibling that
#     forgets the line works anyway. The key is not load-bearing, and it
#     fails SILENTLY in the direction that matters — a new tracker gate
#     with no `issues:` line would simply leave the domain.
#
# So the class is declared in the workflow, on the model of
# `gate-selftest-runs-in:` in run-gate-selftests.sh — and, like that
# marker, it is NOT taken on trust. The declaration is cross-checked
# against the permission block: claiming `tree` while asking for
# `issues:` is a contradiction, and so is the reverse. Two independent
# statements have to agree, and neither alone is believed.
#
# THE PERMISSION SIDE OF THAT CROSS-CHECK IS AN ENUMERATION OF VALUES,
# SO IT STATES WHERE IT ENDS. `permissions:` takes three kinds of value
# and this gate reads all three:
#
#     permissions:              a block mapping -> `issues:` at depth
#     permissions: {...}        a flow mapping  -> `issues:` inside it,
#                               on that line or on any line until the
#                               brace closes
#     permissions: write-all    a scalar        -> grants `issues: write`
#     permissions: read-all     a scalar        -> grants `issues: read`
#
# Anything else after `permissions:` is a RESIDUE, and the gate REFUSES
# it (exit 2) rather than reading it as absent -- an unterminated flow
# mapping included. That refusal is the point. A gate keyed on spelling
# reproduces its own silence: the block rule and the flow rule each
# match a shape, and a value matching neither used to leave `issues` at
# 0 without a word -- `read-all` and `write-all` both did, and the
# second is full tracker WRITE access declaring `tree`, skipping the
# pin, and passing. Enumerating a third spelling without a residue only
# moves that silence one spelling along.
#
# `read-all` IS A DECLARATION, and this paragraph used to say the
# opposite. The claim was that this repository's
# `default_workflow_permissions` is `read` (measured 2026-08-28,
# `gh api repos/OWNER/REPO/actions/permissions/workflow`, and that half
# is still true), therefore `read-all` grants exactly what a workflow
# with no `permissions:` block already has. The premise does not carry
# the conclusion. The restricted default grants read on `contents` and
# `packages` ONLY; every other scope, `issues:` included, is `none`
# (GitHub docs, "Managing GitHub Actions settings for a repository":
# the token "only has read access for the contents and packages
# scopes"). `read-all` "grants read access across all available
# permissions" (GitHub docs, workflow-syntax `permissions`). So
# `read-all` grants `issues: read` where the default grants
# `issues: none` -- an ESCALATION above the default, by exactly the
# argument this file already used to close `write-all`. A discriminator
# that treats the two differently is not a discriminator; it is the
# same silence with a different name on it. `read-all` therefore
# contradicts a `tree` marker and satisfies a `tracker` one, on the
# same footing as `write-all`.
#
# What that cost, and why it is written here rather than quietly fixed:
# `issues: read` is not a harmless grant in this repository. It is what
# three of the four tracker workflows declare (label-taxonomy.yml:79,
# milestone-scope.yml:67, starter-tasks.yml:56), so a scheduled
# workflow writing `permissions: read-all` beside a `tree` marker held
# full tracker READ access and exited 0 clean (measured 2026-08-28
# against the shipped gate). The false premise was pinned by two
# self-test case names, which is why it had to be corrected rather than
# left to decay: a wrong claim with a green test defending it does not
# decay, it gets defended.
#
# scorecard.yml was the live instance and is now narrowed to
# `contents: read` at the workflow level (#839). Nothing needed the
# wider grant: its one job replaces the top-level block with its own,
# and a job-level block overrides completely -- unspecified scopes
# become `none` -- so its effective tracker access was already none.
# The gate does not model job-level overrides and still does not need
# to; the workflow-level line now says what the workflow means.
#
# THE BOUNDARY THAT LEAVES, WRITTEN BESIDE THE CODE RATHER THAN
# ANYWHERE ELSE. The cross-check corroborates; it fires only when the
# permission is present to disagree. A workflow that really judges the
# tracker, declares `tree`, and asks for no `issues:` at all passes,
# because nothing contradicts its marker. That hole is not new and
# cannot be closed from the permission side: the measurement in WHY THE
# CLASS IS DECLARED RATHER THAN DERIVED is exactly why the permission
# cannot be the key. `write-all` and `read-all` are closable because
# each is an ESCALATION above the repository default that no tree
# workflow can want, and both are closed.
#
# THE KEYS ARE NORMALISED, NOT ENUMERATED, AND THAT IS THE SECOND
# LESSON OF THE SAME DEFECT. Everything above concerns the VALUE after
# `permissions:`. The KEY has spellings too -- YAML lets a key be
# quoted and lets a space stand before the colon -- and the first two
# forms of this gate matched key spellings one at a time: `on:` and
# then `"on":`, `permissions:` and nothing else. Measured 2026-08-28
# against that form, on corpora it called clean, exit 0, on both mawk
# 1.3.4 and gawk 5.2.1, with actionlint 1.7.12 accepting every file and
# python3 yaml.safe_load confirming each one means what it looks like:
#
#     'on':                     never entered the domain at all
#     on :                      never entered the domain at all
#     "permissions": write-all  read as no permission block
#     "issues": write           read as no tracker access
#     "ref": dev                read as no pin, so a tree workflow
#                               pinned to dev passed clean
#
# The first two are the worse half, and they are why the fix is
# normalisation rather than another branch. The VALUE enumeration has a
# residue to fall into; the DOMAIN rule has none and cannot be given
# one. Its domain is "declares a `schedule:`" and the complement of
# that is every push-triggered workflow in the repository, which is not
# an error -- so an unrecognised spelling of `on:` is not reported as
# unclassified, it is not counted at all, and no residue can tell the
# two apart. A domain rule cannot be made to fail closed on a spelling
# it does not know. It can only be made not to have unknown spellings,
# which is what normalising the key does: `key_of()` strips the
# indentation, the quotes and the space before the colon, and every
# rule below compares the result, not the text.
#
# THE MARKER. One line, anywhere in the file, exactly:
#
#     # scheduled-subject: tracker
#     # scheduled-subject: tree
#
# `tracker`  judges live tracker state (issues, labels, milestones)
#            against the tree -> every actions/checkout must pin
#            `ref: dev`, and there must be a checkout at all.
# `tree`     judges the tree itself -> no actions/checkout may pin
#            `ref: dev`; `main`'s copy is the right copy.
#
# WHAT `tree` MEANS AT ITS EDGE. The two classes split on ONE question
# -- does this workflow's verdict depend on live tracker state? --
# and `tree` is the answer "no", not the claim "it reads the tree".
# run-retention.yml is the member that makes the difference visible: it
# deletes old workflow runs through the API and reads nothing out of its
# checkout, so it judges neither subject. It is `tree` because that is
# the class whose operative rule is right for it -- do not pin. Pinning
# it to `dev` would be the over-correction #839 names, and a third class
# would need a third RULE to be worth declaring, which it has not got.
#
# A MISSING MARKER FAILS. That is the part that closes #839 rather than
# patching its instances: a scheduled workflow added tomorrow cannot
# quietly sit outside the domain, because the domain is "every
# scheduled workflow" and an unclassified member is an error, not an
# omission. A universal satisfied by a domain that excludes the case is
# the failure this milestone keeps finding.
#
# THE ONE ROUTE OUT, NAMED RATHER THAN LEFT TO BE REDISCOVERED. The
# domain is "every workflow that declares a `schedule:`", and a REUSABLE
# workflow declares none. So a scheduled caller whose job is
# `uses: ./.github/workflows/<callee>.yml` puts the checkout in the
# callee, and the callee is never examined: the caller passes as `tree`
# on its own terms, because having no checkout at all is legitimate for
# a tree workflow, while the callee checks out `main` holding
# `issues: write` — the opening hazard, through the one door the domain
# does not follow. Measured 2026-08-27: that shape exits 0 clean.
#
# It is the `tree` spelling only. The same delegation declared `tracker`
# is caught, by the rule that a tracker workflow must check something
# out. Both directions are pinned as cases in the self-test, so this
# paragraph cannot quietly stop being true.
#
# It is stated and not coded because there are zero instances today —
# no `workflow_call` and no `uses: ./.github/workflows` anywhere in the
# directory — so following the `uses:` edge would be a domain built for
# no member. The first reusable workflow this repository adds is where
# that decision belongs, and this is the note it should arrive at.
#
# IT FAILS IN BOTH DIRECTIONS. Requiring the pin catches the defect
# #839 reports; forbidding it on tree workflows catches the
# over-correction the issue explicitly names ("pinning everything would
# be wrong"). A guard with one direction only half-states its rule.
#
# THE DOMAIN IS THIS BRANCH'S DIRECTORY; THE POPULATION THAT FIRES IS
# THE DEFAULT BRANCH'S. This gate runs per pull request, so it judges
# the workflows in front of it, while the crons it reasons about fire
# from `main`. The two sets differ for one release window: a workflow
# added on `dev` is judged here before it can fire, and one deleted on
# `dev` leaves this domain while `main`'s copy keeps firing until the
# release ships. Measured 2026-08-28: `main` holds 24 workflow files and
# this branch 25, the extra being run-retention.yml, which is not on
# `main`, 404s as a workflow, and has never fired -- a difference in the
# harmless direction, and nothing on `main` is missing here. Nothing in
# this gate closes that window; the release does.
#
# WHAT IT DOES NOT CLAIM. It reads workflow text, so it cannot tell
# that a tracker workflow's script actually reads the checked-out tree,
# and it does not adjudicate a checkout pinned to a tag or a SHA —
# those are a different question from "which branch's declaration
# judges the tracker", and refusing them here would fire on workflows
# with nothing to fix.
#
# Usage: bash scripts/check-scheduled-ref-pins.sh [workflow-dir]
# Env:   SCHEDULED_REF_PINS_DIR  directory to inspect — the seam the
#                                self-test drives. The positional
#                                argument wins if both are given.
#        SCHEDULED_REF_PINS_AWK  the awk to extract facts with. A test
#                                seam: it is what lets the self-test
#                                drive an awk that FAILS while printing,
#                                which no workflow fixture can produce.
# Exit:  0 every scheduled workflow is classified and pinned to match
#        1 at least one is unclassified, contradicted, or mispinned
#        2 cannot check (no directory, no workflows, no schedules, or a
#          class with no members — a discriminator that no longer
#          discriminates is not a pass)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
DIR="${1:-${SCHEDULED_REF_PINS_DIR:-$ROOT/.github/workflows}}"

if [ ! -d "$DIR" ]; then
    echo "::error title=No workflow directory::$DIR is not a directory, so no" \
         "scheduled workflow was examined." >&2
    exit 2
fi

shopt -s nullglob
files=("$DIR"/*.yml "$DIR"/*.yaml)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::error title=No workflows found::$DIR matched no *.yml or *.yaml." \
         "This gate would otherwise pass having examined nothing." >&2
    exit 2
fi
mapfile -t files < <(printf '%s\n' "${files[@]}" | sort)

# facts_of emits one `key=value` line per workflow file:
#
#   scheduled     1 when the `on:` block declares a `schedule:` trigger
#   issues        1 when any permissions value grants `issues:` --
#                 named in a mapping, or implied by `read-all` or
#                 `write-all`
#   unknown_perm  1 when a `permissions:` value was seen that is none of
#                 the three kinds the header enumerates, an unterminated
#                 flow mapping included
#   checkouts     how many actions/checkout steps the file has
#   pinned_dev    how many of those pin ref to dev / refs/heads/dev
#   other_ref     how many pin ref to something else (tag, sha, input)
#
# Blocks are delimited by indentation, which is what YAML actually uses;
# nothing here parses YAML, and it does not need to — every fact is a
# key at a known depth relative to the block that introduces it.
# THE awk BINARY IS A SEAM, and it exists so the refusal below can be
# DRIVEN rather than argued. Two halves decide that refusal -- empty
# stdout, and a non-zero status -- and no workflow fixture can produce
# the second while `-f` and `-r` hold, so without this the status half
# would be a branch with no case, which is how the directory fixture
# came to prove nothing on the runner that matters. The self-test points
# this at a stub that prints a full fact line and exits non-zero.
AWK="${SCHEDULED_REF_PINS_AWK:-awk}"

facts_of() {
    "$AWK" '
        function indent(s,   n) { match(s, /^ */); return RLENGTH }
        function isblank(s) { return s ~ /^[[:space:]]*$/ }
        function iscomment(s) { return s ~ /^[[:space:]]*#/ }
        # A trailing comment cannot declare a trigger or a permission,
        # and both flow-mapping rules below read whole lines. Without
        # this, `on:  # the nightly schedule:` would enter the domain
        # and `permissions: {contents: read}  # not issues:` would read
        # as tracker access -- a gate satisfying itself from prose,
        # which is the shape check-python-deps.sh shipped (#743). In
        # YAML a `#` opens a comment only after whitespace, so that is
        # what is cut.
        function decomment(s) { sub(/[[:space:]]#.*$/, "", s); return s }
        # key_of(s) is the NORMALISED key a line declares, or "" for a
        # line that declares none. YAML permits a key to be quoted and
        # permits space before the colon, so `on:`, `"on":`, QonQ: and
        # `on :` are four spellings of ONE key (Q for a single quote --
        # this comment lives inside a single-quoted awk program and
        # cannot contain one). Every rule below is keyed on this rather
        # than on a spelling, which is the difference between an
        # enumeration that grows a branch per spelling and a rule that
        # is right for all of them at once. See THE KEYS ARE NORMALISED
        # in the header for what the enumerating form let through.
        function key_of(s,   k) {
            if (s !~ /:/) return ""
            k = s
            sub(/:.*$/, "", k)
            sub(/^[[:space:]]+/, "", k)
            sub(/[[:space:]]+$/, "", k)
            if (k ~ /^".*"$/ || k ~ /^'"'"'.*'"'"'$/) k = substr(k, 2, length(k) - 2)
            return k
        }
        # val_of(s) is what stands after that key, comment and trailing
        # space removed. The three permission rules test THIS, never the
        # whole line: an unanchored `permissions:.*[^{]` matches the
        # `contents: read` inside `permissions: {contents: read}` and
        # reads a flow mapping as a scalar residue, which is a refusal
        # on a workflow with nothing wrong with it.
        function val_of(s,   v) {
            v = decomment(s)
            sub(/^[^:]*:[[:space:]]*/, "", v)
            sub(/[[:space:]]+$/, "", v)
            return v
        }

        BEGIN { sched = 0; issues = 0; co = 0; dev = 0; other = 0; unk = 0 }

        # --- the on: block -------------------------------------------
        # A flow mapping puts the trigger on the `on:` line itself:
        #     on: {schedule: [{cron: 0 3 * * *}]}   (quotes elided --
        #                       this comment lives inside a single-quoted
        #                       awk program and cannot contain them)
        # The block-style rule consumes that line with `next`, so the
        # trigger would never be seen and the workflow would leave this
        # the domain of this gate WITHOUT A WORD — a universal satisfied by a
        # domain that excludes the case, which is the failure named
        # eighty lines above. Test the line before consuming it.
        #
        # YAML offers a flow spelling for every block one, and the tree
        # is not the format: nothing stops a future workflow being
        # written this way, and the gate would have counted a corpus of
        # two while the third sat outside it holding `issues: write`.
        indent($0) == 0 && key_of($0) == "on" {
            if (decomment($0) ~ /schedule["'"'"']?[[:space:]]*:/) sched = 1
            in_on = 1; next
        }
        in_on {
            if (!isblank($0) && !iscomment($0) && indent($0) == 0) { in_on = 0 }
            else if (key_of($0) == "schedule") { sched = 1 }
        }

        # --- any permissions: block, top-level or per-job -------------
        key_of($0) == "permissions" && val_of($0) == "" {
            in_perm = 1; perm_indent = indent($0); next
        }
        in_perm {
            if (!isblank($0) && !iscomment($0) && indent($0) <= perm_indent) { in_perm = 0 }
            else if (key_of($0) == "issues") { issues = 1 }
        }

        # The same flow spelling, and here it fails in the DANGEROUS
        # direction. `permissions: {issues: write}` opens no block, so
        # the rule above reads `issues = 0` for a workflow that holds
        # full tracker access. The cross-check is the stated reason the
        # marker is believed at all -- two independent statements that
        # have to agree -- and with the permission silently absent the
        # marker is believed ALONE. A workflow with real tracker access
        # could then declare `tree`, skip the pin, and pass -- the exact
        # defect #839 reports, through the gate built to stop it.
        # A flow mapping is ALSO not required to end on the line that
        # opens it, and that is the third way the same silence was
        # reached. `permissions: {` with `issues: write` on the next
        # line opens no block either, so the block rule above and a
        # single-line flow rule BOTH read `issues = 0` for a workflow
        # holding full tracker access -- measured 2026-08-28, exit 0
        # clean on both awks. Braces are therefore COUNTED rather than
        # assumed balanced on one line, and a mapping still open at EOF
        # is residue (see the END rule): an unterminated flow mapping is
        # a file this gate could not read, and a refusal must not look
        # like an absence.
        #
        # The continuation rule is placed BEFORE the rule that opens the
        # mapping deliberately. awk tries rules in source order, so an
        # opener placed first would fall straight through into its own
        # continuation and count the braces on the opening line twice.
        in_flow {
            fl = decomment($0)
            if (fl ~ /issues["'"'"']?[[:space:]]*:/) issues = 1
            flow_depth += gsub(/\{/, "{", fl) - gsub(/\}/, "}", fl)
            if (flow_depth <= 0) in_flow = 0
        }
        key_of($0) == "permissions" && substr(val_of($0), 1, 1) == "{" {
            pl = val_of($0)
            if (pl ~ /issues["'"'"']?[[:space:]]*:/) issues = 1
            flow_depth = gsub(/\{/, "{", pl) - gsub(/\}/, "}", pl)
            if (flow_depth > 0) in_flow = 1
        }

        # And the SCALAR spelling, which is neither of the two above and
        # was read as `issues = 0` in silence by both. `write-all` grants
        # `issues: write`; `read-all` grants exactly this repository default
        # (measured: default_workflow_permissions is read) and so declares
        # nothing. Any OTHER value is the residue: refused, not assumed,
        # because an enumeration with no residue just relocates the silence
        # the two rules above already produced twice.
        key_of($0) == "permissions" && val_of($0) != "" && substr(val_of($0), 1, 1) != "{" {
            pv = val_of($0)
            gsub(/^["'"'"']|["'"'"']$/, "", pv)
            if (pv == "write-all" || pv == "read-all") issues = 1
            else unk = 1
        }

        # --- step blocks ---------------------------------------------
        # A step opens at `- ` and closes at the next non-blank,
        # non-comment line indented no deeper than its dash. The closing
        # line may itself open the next step, so it is re-tested rather
        # than consumed.
        {
            if (in_step && !isblank($0) && !iscomment($0) && indent($0) <= step_indent) {
                close_step()
            }
            if ($0 ~ /^[[:space:]]*-[[:space:]]/) {
                if (in_step) close_step()
                in_step = 1; step_indent = indent($0)
                step_is_checkout = 0; step_ref = ""
            }
            if (in_step) {
                # Quoted here too, and for the same reason: measured
                # 2026-08-28, a `tree` workflow writing `"ref": dev`
                # passed clean, because the pin rule could not see a
                # pin spelled with a quoted key.
                if ($0 ~ /uses["'"'"']?[[:space:]]*:[[:space:]]*["'"'"']?actions\/checkout/) step_is_checkout = 1
                if (key_of($0) == "ref" && val_of($0) != "") {
                    v = val_of($0)
                    gsub(/^["'"'"']|["'"'"']$/, "", v)
                    step_ref = v
                }
            }
        }

        END {
            if (in_step) close_step()
            # A flow mapping that never closed is a permissions value
            # this program did not finish reading. Reporting it as
            # `issues = 0` is exactly the absence the residue exists to
            # refuse, so it is counted as residue instead.
            if (in_flow) unk = 1
            # unk is a COUNT and never the offending text. These lines
            # are consumed by `eval`, so a value read out of a workflow
            # file must not reach it; the reporting path re-reads the
            # file for the value instead.
            printf "scheduled=%d\nissues=%d\ncheckouts=%d\npinned_dev=%d\nother_ref=%d\nunknown_perm=%d\n",
                   sched, issues, co, dev, other, unk
        }

        function close_step() {
            if (step_is_checkout) {
                co++
                if (step_ref == "dev" || step_ref == "refs/heads/dev") dev++
                else if (step_ref != "") other++
            }
            in_step = 0
        }
    ' "$1"
}

fail=0
cannot=0
tracker_n=0
tree_n=0
sched_n=0

for f in "${files[@]}"; do
    base="$(basename "$f")"

    # Pre-set, then overwritten by the eval, and both halves of the
    # reason were re-measured rather than restated:
    #
    #   * shellcheck cannot see through an eval. Deleting this line
    #     produces six SC2154 warnings (measured, shellcheck 0.10.0), and
    #     `shellcheck -S warning scripts/*.sh` is a lane step, so the lane
    #     goes red. This is the half the self-test cannot observe, and the
    #     line's mutant is killed there rather than here.
    #
    #   * it stops a file being judged on the PREVIOUS file's facts when
    #     facts_of produces nothing. On its own that is defence in depth:
    #     the refusal below `continue`s first, so deleting this line alone
    #     changes no verdict (measured). Delete BOTH and `set -u` aborts
    #     the run on the first unreadable file (measured) — noisy, but
    #     mid-corpus, so the files after it are never examined.
    #
    # A refusal must not look like an absence, and the refusal below is
    # what enforces that; this line is the pair to it, not a substitute.
    scheduled=0; issues=0; checkouts=0; pinned_dev=0; other_ref=0; unknown_perm=0

    # WHETHER THE FILE CAN BE READ IS DECIDED IN THE SHELL, BEFORE awk,
    # BECAUSE THE TWO awks DISAGREE ABOUT IT. Measured 2026-08-28 with
    # the self-test fixture that is a DIRECTORY named *.yml:
    #
    #   mawk 1.3.4   cannot open it, prints nothing, exits 2 -> the
    #                empty-facts refusal below fires. Suite 39/0.
    #   gawk 5.2.1   SKIPS it with a warning and exits 0, so the END
    #                rule still runs and prints a complete all-zero
    #                fact line. The gate then reads a file it never
    #                opened as `scheduled=0` and drops it out of the
    #                domain in SILENCE -- the exact fail-open this
    #                guard exists to prevent, in the gate whose header
    #                argues it fails closed.
    #
    # Same fixture, same gate, opposite verdicts, decided by which awk
    # the runner happens to have -- and the runner has gawk, so the
    # green measured on a mawk box was a measurement whose domain
    # excluded the environment the check runs in. Keying the refusal on
    # awk`s exit status alone does not close it either: gawk`s skip is
    # a warning with status 0. `-f` and `-r` are the uid-independent
    # half, they are the same on every awk, and they are asked first.
    if [ ! -f "$f" ] || [ ! -r "$f" ]; then
        echo "::error file=$f,title=Cannot read workflow::$base is not a readable" \
             "regular file, so no fact could be extracted from it. Treating that as" \
             "\"not scheduled\" would drop the file out of this gate's domain silently," \
             "which is the failure the gate exists to prevent." >&2
        cannot=1
        continue
    fi

    facts="$(facts_of "$f")"
    facts_rc=$?
    # Empty stdout AND a non-zero status, because neither implies the
    # other: gawk prints a full fact line while skipping an argument it
    # never opened (status 0), and an awk that dies part way through a
    # file leaves partial output that is not empty.
    if [ -z "$facts" ] || [ "$facts_rc" -ne 0 ]; then
        echo "::error file=$f,title=Cannot read workflow::extracting facts from $base" \
             "produced nothing. Treating that as \"not scheduled\" would drop the file" \
             "out of this gate's domain silently, which is the failure the gate exists" \
             "to prevent." >&2
        cannot=1
        continue
    fi
    eval "$facts"

    [ "$scheduled" -eq 1 ] || continue
    sched_n=$((sched_n + 1))

    # The marker. Every line of it, so a second one is an error rather
    # than being silently shadowed by the first — two markers mean two
    # readers disagreed and one of them is going to be believed.
    # Leading whitespace is allowed: beside the checkout it governs is
    # where a marker naturally wants to go, and anchoring at `^#` made
    # a correctly classified, correctly pinned workflow read as
    # unclassified -- a red telling the author to add the line they had
    # just added. It failed closed, so nothing unsafe shipped; the cost
    # was the wrong remedy, and that is a limitation worth removing
    # rather than documenting.
    mapfile -t markers < <(sed -n \
        's/^[[:space:]]*#[[:space:]]*scheduled-subject:[[:space:]]*\([^[:space:]]*\)[[:space:]]*$/\1/p' "$f")

    if [ "${#markers[@]}" -eq 0 ]; then
        echo "::error file=$f,title=Unclassified scheduled workflow::$base runs on a" \
             "schedule and carries no \`# scheduled-subject:\` marker, so nothing says" \
             "whether its checkout should be pinned. A cron fires only from the default" \
             "branch: add \`# scheduled-subject: tracker\` if it judges live tracker" \
             "state (then pin \`ref: dev\`), or \`# scheduled-subject: tree\` if it" \
             "judges the tree (then do not pin). See #839." >&2
        fail=1
        continue
    fi

    if [ "${#markers[@]}" -gt 1 ]; then
        echo "::error file=$f,title=Two subject markers::$base declares" \
             "\`# scheduled-subject:\` ${#markers[@]} times (${markers[*]}). One file" \
             "judges one subject; two markers mean the rule applied is whichever a" \
             "reader saw first." >&2
        fail=1
        continue
    fi

    subject="${markers[0]}"
    case "$subject" in
        # Counted here, before any further check, and deliberately so.
        # A file that declares a class and then fails the cross-check or
        # the pin rule is still a MEMBER of that class — counting it
        # only on success would let one broken tracker workflow empty
        # the tracker class and report a definite violation as "cannot
        # check". The self-test pins that: it is what this gate did in
        # its first form.
        tracker) tracker_n=$((tracker_n + 1)) ;;
        tree)    tree_n=$((tree_n + 1)) ;;
        *)
            echo "::error file=$f,title=Unknown subject::$base declares" \
                 "\`# scheduled-subject: $subject\`, which is neither \`tracker\` nor" \
                 "\`tree\`. An unrecognised class would otherwise be judged by no rule" \
                 "at all." >&2
            fail=1
            continue
            ;;
    esac

    # A `permissions:` value the enumeration does not cover disables the
    # CROSS-CHECK and nothing else: with the permission unreadable the
    # marker would be the only witness, and the marker being believed
    # alone is the failure the cross-check exists to prevent. The pin
    # rule below still runs -- it reads checkouts, not permissions -- so
    # the file is still judged on everything that can still be judged.
    # This is a refusal (exit 2), not a violation: the gate could not
    # see, and it says so instead of passing.
    if [ "$unknown_perm" -ne 0 ]; then
        # Captured whole and trimmed with parameter expansion rather
        # than piped through `head`: a consumer that exits early SIGPIPEs
        # the producer, and under pipefail that reports failure on
        # success (check-pipefail-consumers.sh).
        # The key may be quoted and the value may be an unterminated
        # flow mapping, so this pattern is deliberately wider than the
        # one that DECIDED the refusal: it only has to name the line for
        # the reader.
        bad_perm="$(sed -n 's/^[[:space:]]*["'"'"']\{0,1\}permissions["'"'"']\{0,1\}[[:space:]]*:[[:space:]]*\([^[:space:]#][^#]*\)$/\1/p' "$f")"
        bad_perm="${bad_perm%%$'\n'*}"
        bad_perm="$(printf '%s' "$bad_perm" | tr -cd '[:print:]')"
        bad_perm="${bad_perm:0:60}"
        echo "::error file=$f,title=Unclassifiable permissions value::$base writes" \
             "\`permissions: $bad_perm\`, which is neither a block mapping, a closed" \
             "flow mapping, \`read-all\` nor \`write-all\`. The cross-check that keeps the" \
             "\`# scheduled-subject:\` marker from being its own only witness cannot be" \
             "made against a permission this gate cannot read, and reading it as absent" \
             "is how \`write-all\` passed as \`tree\` (#839)." >&2
        cannot=1
    fi

    # The declaration is not taken on trust. `issues:` is the permission
    # a tracker workflow needs and a tree workflow has no use for, so
    # the two statements have to agree. Neither is believed alone: the
    # permission cannot be the key (default_workflow_permissions is
    # `read`, so omitting it still works), and the marker is prose.
    if [ "$unknown_perm" -eq 0 ] && [ "$subject" = "tree" ] && [ "$issues" -eq 1 ]; then
        echo "::error file=$f,title=Marker contradicts permissions::$base declares" \
             "\`# scheduled-subject: tree\` but requests the \`issues:\` permission." \
             "A workflow that judges only the tree has no use for tracker access; one" \
             "of the two statements is wrong." >&2
        fail=1
        continue
    fi
    if [ "$unknown_perm" -eq 0 ] && [ "$subject" = "tracker" ] && [ "$issues" -eq 0 ]; then
        echo "::error file=$f,title=Marker contradicts permissions::$base declares" \
             "\`# scheduled-subject: tracker\` but requests no \`issues:\` permission." \
             "A workflow that judges the tracker declares the access it needs, so that" \
             "the marker has a second statement to agree with." >&2
        fail=1
        continue
    fi

    if [ "$subject" = "tracker" ]; then
        if [ "$checkouts" -eq 0 ]; then
            echo "::error file=$f,title=Tracker workflow checks out nothing::$base" \
                 "judges the tracker against the tree but has no actions/checkout step," \
                 "so there is no tree to judge it against." >&2
            fail=1
            continue
        fi
        if [ "$pinned_dev" -ne "$checkouts" ]; then
            echo "::error file=$f,title=Tracker workflow not pinned to dev::$base has" \
                 "$checkouts actions/checkout step(s) and $pinned_dev pinned to \`dev\`." \
                 "A cron fires only from the default branch, so an unpinned checkout" \
                 "judges the tracker against \`main\`'s declaration — a release behind" \
                 "the tracker it is describing. Add \`ref: dev\`, as" \
                 "issue-state-labels.yml does (#490, #839)." >&2
            fail=1
        fi
        printf 'tracker  %-28s checkouts=%d pinned=%d\n' "$base" "$checkouts" "$pinned_dev"
    else
        if [ "$pinned_dev" -gt 0 ]; then
            echo "::error file=$f,title=Tree workflow pinned to dev::$base judges the" \
                 "tree, and $pinned_dev of its checkout step(s) pin \`ref: dev\`. A" \
                 "scanner scheduled against the default branch should scan the default" \
                 "branch; pinning it to \`dev\` makes it report on something no release" \
                 "contains (#839)." >&2
            fail=1
        fi
        printf 'tree     %-28s checkouts=%d other-ref=%d\n' "$base" "$checkouts" "$other_ref"
    fi
done

# Non-vacuity. Each of these is a way for the gate to be green having
# decided nothing, and each has to be louder than a pass rather than
# quieter — the whole corpus of findings this gate belongs to is
# universals satisfied by an empty domain.
if [ "$sched_n" -eq 0 ]; then
    echo "::error title=No scheduled workflows::$DIR holds ${#files[@]} workflow(s) and" \
         "none declares a \`schedule:\` trigger. This gate's entire subject is the" \
         "scheduled ones, so it just passed having judged nothing." >&2
    cannot=1
fi
if [ "$sched_n" -gt 0 ] && [ "$tracker_n" -eq 0 ]; then
    echo "::error title=No tracker workflows::$sched_n scheduled workflow(s) and not one" \
         "classified \`tracker\`. Either every tracker gate was deleted, or the marker" \
         "stopped being read — and a discriminator that puts everything in one class" \
         "is not a pass." >&2
    cannot=1
fi
if [ "$sched_n" -gt 0 ] && [ "$tree_n" -eq 0 ]; then
    echo "::error title=No tree workflows::$sched_n scheduled workflow(s) and not one" \
         "classified \`tree\`. Same reading as the tracker case: one populated class" \
         "means the split is no longer being made." >&2
    cannot=1
fi

# A violation outranks a non-vacuity refusal, and the order matters.
# Exit 2 means "could not see"; having named a specific workflow and a
# specific rule it breaks, the gate plainly did see. Reporting that as
# "cannot check" hands the reader the wrong remedy — they would go
# looking for a broken discriminator instead of fixing the workflow the
# error names. Both messages are still printed; only the exit code is
# decided here.
if [ "$fail" -eq 1 ]; then
    exit 1
fi
if [ "$cannot" -eq 1 ]; then
    exit 2
fi

echo "scheduled workflows: $sched_n ($tracker_n tracker pinned to dev, $tree_n tree unpinned)"
