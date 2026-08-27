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
# siblings for three releases. Two of them carried a comment describing
# the hazard they were suffering from. The comment was correct and
# changed nothing, which is the finding #839 actually reports: the
# defect is that ONE workflow was fixed and nothing reconciled the
# others against it.
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
# A MISSING MARKER FAILS. That is the part that closes #839 rather than
# patching its instances: a scheduled workflow added tomorrow cannot
# quietly sit outside the domain, because the domain is "every
# scheduled workflow" and an unclassified member is an error, not an
# omission. A universal satisfied by a domain that excludes the case is
# the failure this milestone keeps finding.
#
# IT FAILS IN BOTH DIRECTIONS. Requiring the pin catches the defect
# #839 reports; forbidding it on tree workflows catches the
# over-correction the issue explicitly names ("pinning everything would
# be wrong"). A guard with one direction only half-states its rule.
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
#   issues        1 when any permissions block grants `issues:`
#   checkouts     how many actions/checkout steps the file has
#   pinned_dev    how many of those pin ref to dev / refs/heads/dev
#   other_ref     how many pin ref to something else (tag, sha, input)
#
# Blocks are delimited by indentation, which is what YAML actually uses;
# nothing here parses YAML, and it does not need to — every fact is a
# key at a known depth relative to the block that introduces it.
facts_of() {
    awk '
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

        BEGIN { sched = 0; issues = 0; co = 0; dev = 0; other = 0 }

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
        /^"?on"?:/ {
            if (decomment($0) ~ /schedule[[:space:]]*:/) sched = 1
            in_on = 1; next
        }
        in_on {
            if (!isblank($0) && !iscomment($0) && indent($0) == 0) { in_on = 0 }
            else if ($0 ~ /^[[:space:]]+schedule:/) { sched = 1 }
        }

        # --- any permissions: block, top-level or per-job -------------
        /^[[:space:]]*permissions:[[:space:]]*(#.*)?$/ {
            in_perm = 1; perm_indent = indent($0); next
        }
        in_perm {
            if (!isblank($0) && !iscomment($0) && indent($0) <= perm_indent) { in_perm = 0 }
            else if ($0 ~ /^[[:space:]]+issues:/) { issues = 1 }
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
        /^[[:space:]]*permissions:[[:space:]]*\{/ {
            if (decomment($0) ~ /issues[[:space:]]*:/) issues = 1
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
                if ($0 ~ /uses:[[:space:]]*actions\/checkout/) step_is_checkout = 1
                if ($0 ~ /^[[:space:]]*ref:[[:space:]]*[^[:space:]]/) {
                    v = $0
                    sub(/^[[:space:]]*ref:[[:space:]]*/, "", v)
                    sub(/[[:space:]]*#.*$/, "", v)
                    gsub(/^["'"'"']|["'"'"']$/, "", v)
                    sub(/[[:space:]]+$/, "", v)
                    step_ref = v
                }
            }
        }

        END {
            if (in_step) close_step()
            printf "scheduled=%d\nissues=%d\ncheckouts=%d\npinned_dev=%d\nother_ref=%d\n",
                   sched, issues, co, dev, other
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

    # Pre-set, then overwritten by the eval. Two reasons, and the
    # second is the load-bearing one: shellcheck cannot see through an
    # eval (SC2154), and without the reset a facts_of that produced
    # nothing would leave the PREVIOUS file's values in place — a
    # workflow would inherit its neighbour's verdict, and an awk that
    # stopped working would look like a corpus of unscheduled files.
    # A refusal must not look like an absence.
    scheduled=0; issues=0; checkouts=0; pinned_dev=0; other_ref=0
    facts="$(facts_of "$f")"
    if [ -z "$facts" ]; then
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

    # The declaration is not taken on trust. `issues:` is the permission
    # a tracker workflow needs and a tree workflow has no use for, so
    # the two statements have to agree. Neither is believed alone: the
    # permission cannot be the key (default_workflow_permissions is
    # `read`, so omitting it still works), and the marker is prose.
    if [ "$subject" = "tree" ] && [ "$issues" -eq 1 ]; then
        echo "::error file=$f,title=Marker contradicts permissions::$base declares" \
             "\`# scheduled-subject: tree\` but requests the \`issues:\` permission." \
             "A workflow that judges only the tree has no use for tracker access; one" \
             "of the two statements is wrong." >&2
        fail=1
        continue
    fi
    if [ "$subject" = "tracker" ] && [ "$issues" -eq 0 ]; then
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
