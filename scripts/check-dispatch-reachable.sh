#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert that every workflow_dispatch workflow is actually DISPATCHABLE.
#
# THE RULE GITHUB ENFORCES AND NOTHING HERE DID. A workflow_dispatch
# workflow can only be triggered if the file exists on the repository's
# DEFAULT branch. Not the branch you pass to `--ref` — the default one.
# Until then the workflow is not merely unrunnable, it is not in the
# workflows API at all, and `gh workflow run <file>` answers 404, which
# reads like a typo or a missing scope rather than "this does not exist
# yet".
#
# This project develops on `dev` and releases to `main`, and `main` is
# the default branch. So a new dispatch-only workflow merged to `dev` is
# undispatchable until the next release ships — which is a whole release
# cycle in which its documentation is wrong.
#
# That is not hypothetical. capture-fixtures.yml (#665) was merged to
# dev, docs/internals.md was changed to make `gh workflow run
# capture-fixtures.yml` the PRIMARY route for re-recording the request
# fixtures, and the first dispatch 404'd. The remedy a gate points at
# has to exist — the same rule check-capture-lane.sh applies to the
# script a workflow names, one level up, applied to the workflow itself.
#
# WHY AN ALLOWLIST RATHER THAN A HARD FAILURE. "Not on the default
# branch yet" is the normal, correct state of a new workflow for one
# release cycle. Failing on it outright would just block adding one. So
# a pending workflow is DECLARED, with the reason and what clears it —
# the same bargain as .github/vuln-allowlist.txt, and for the same
# reason: an accepted condition with a written expiry, never a bare
# entry.
#
# A stale declaration fails too. Once the workflow reaches the default
# branch the entry has stopped meaning anything, and an allowlist nobody
# prunes is how a temporary exception becomes permanent.
#
# A PULL REQUEST INTO THE DEFAULT BRANCH IS A THIRD STATE (#977). The two
# verdicts above are each right on their own, and on a release PR no
# commit satisfies both: the workflow is not on the default branch yet,
# so the entry is required, and one commit later — after the merge — the
# same entry is stale. The v2.1.0 release merge turned this gate red on
# the default branch for exactly that reason, and the runbook sentence
# "the release PR removes it" could not be followed by anyone.
#
# So on a pull request whose base IS the default branch, a dispatchable
# workflow in the tree counts as REACHABLE: merging this pull request is
# what puts it there. Its entry is then stale ON THE PULL REQUEST, which
# is what makes the runbook step executable — the release PR removes the
# entry, and the PR and the default branch are both green. Outside a
# release, pushes and pull requests into any other branch keep today's
# verdicts; the paragraph after next says what a release changes.
#
# The base is read from the event and compared against a DERIVED default
# branch, never against a literal `main`. A gate that hard-codes the name
# exempts every pull request into `dev` on the day the default branch
# moves, which is the #665 failure reintroduced by the fix for this one.
#
# THAT EXEMPTION ALONE MOVES THE DEADLOCK FROM `main` TO `dev` (#977,
# review round 1). The release pull request is `dev` -> `main`, so a
# commit is only in it by being on `dev` first, and the runbook's route
# onto `dev` is step 5, `release/vX.Y.Z` -> `dev`. That pull request has
# base `dev`, gets no exemption, and the entry it removes is the entry
# keeping it green. Measured on a fixture ledger: the release PR says
# "remove it here", and the only pull request that can put the removal
# there is red. Every pull request into `dev` between the two merges is
# red for the same reason, because they are tested as the merge product
# and `dev` already carries the removal.
#
# So the removal is IN TRANSIT for the length of the release, and this
# gate has to say so. WHAT IT ACTUALLY DERIVES IS TWO VERSION PINS: the
# `ghcr.io/<ns>/docker-net-dhcp:vX.Y.Z` pin in README.md, read the same
# way from this tree and from the default branch, the pin
# scripts/bump-version.sh rewrites at runbook step 2 and
# scripts/check-version-pins.sh keeps consistent. While the tree pins the
# IMMEDIATE SUCCESSOR of what the default branch pins, a dispatchable
# workflow the default branch lacks does not need an entry. Mid-cycle the
# two sides are equal and nothing is suspended.
#
# THAT IS NOT A DERIVATION OF "A RELEASE IS HAPPENING", and neither this
# comment nor anything this gate prints may pretend otherwise (#977 round
# 2). It does not look for a release branch, a pruned ledger entry or a
# release pull request. A bare `scripts/bump-version.sh vNEXT` on any
# branch produces the same state, and check-version-pins.sh deliberately
# permits a tree to lead the latest tag, so that is a false positive this
# gate cannot exclude. It is named in the output of every run the
# suspension applies to, and it is the first bound below.
#
# THE BOUNDS, stated beside the claim.
#
#   - A pin bump that is not a release suspends the finding just the
#     same, repo-wide, and travels to `dev` when it merges.
#   - While suspended, the finding is off for EVERY dispatchable
#     workflow, not only the one being released, so a workflow merged
#     undeclared in that window is not caught until the pins agree
#     again.
#   - THE BOUND IS A DISTANCE, NOT A DURATION (#977 round 3). What is
#     measured is how far apart the two pins are: the suspension holds
#     only while this tree pins the IMMEDIATE SUCCESSOR of what the
#     default branch pins. Nothing here reads a clock, a tag or the
#     state of a release, so a release parked after runbook step 5 sits
#     exactly one step ahead and stays suspended for as long as the
#     pins differ, however long that is. What the ledger's "written
#     expiry" buys here is a bounded SHAPE, and that is the whole of
#     it: two steps apart is refused, a year one step apart is not.
#   - The STALE rule is never suspended, on any route. That is the half
#     that keeps the default branch green after the merge.
#
# Usage: bash scripts/check-dispatch-reachable.sh [workflow-dir] [allowlist]
# Env:   BASE_REF (default origin/main) — the default branch to test against.
#        GITHUB_EVENT_NAME, GITHUB_BASE_REF, GITHUB_EVENT_PATH — read, never
#          required. They are what identifies a pull request into the default
#          branch; with none of them set the verdict is the pre-#977 one.
#        VERSION_PIN_FILE (default README.md) — the file the release-in-flight
#          comparison reads the published-image pin from, on both sides.
# Exit:  0 reachable or declared (also when the default branch cannot be
#          read — reported as NOT INSPECTED, never a silent pass),
#        1 an undeclared or stale entry,
#        2 cannot check at all.
set -uo pipefail

WF_DIR="${1:-.github/workflows}"
ALLOWLIST="${2:-.github/dispatch-pending.txt}"
BASE_REF="${BASE_REF:-origin/main}"
# The fetch fallback below REWRITES BASE_REF to FETCH_HEAD, and a hosted
# runner takes that path on every run: actions/checkout fetches one ref,
# so `origin/main` is not present. The #977 exemption has to compare the
# branch the operator NAMED, not what the fallback left behind, or it is
# inert in the only place it matters.
BASE_REF_SPEC="$BASE_REF"

[ -d "$WF_DIR" ] || { echo "FAIL  no workflow directory '$WF_DIR'" >&2; exit 2; }

# A MISSING DIRECTORY WAS CAUGHT; AN EMPTY ONE WAS NOT (#743). This was
# the last of the 47 gates that would render a verdict over no input:
# `bash check-dispatch-reachable.sh /tmp/empty` printed "PASS  every
# workflow_dispatch workflow is on origin/main" and exited 0, having
# examined nothing. 46 of its siblings already refuse. The counter below
# is what makes the detector's own narrowness loud instead of silent —
# if the `workflow_dispatch` match ever stops matching, the gate goes to
# zero subjects and says so rather than passing.
shopt -s nullglob
WF_FILES=("$WF_DIR"/*.yml "$WF_DIR"/*.yaml)
shopt -u nullglob

if [ "${#WF_FILES[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::no *.yml or *.yaml files in $WF_DIR." \
         "This gate would otherwise report a clean pass having read nothing." >&2
    exit 2
fi

# The comparison target is a branch, not this checkout. CI clones one
# ref; fetch the default branch rather than assuming it is present.
if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
    git fetch --no-tags --quiet origin "${BASE_REF#origin/}" 2>/dev/null || true
    git rev-parse --verify --quiet "FETCH_HEAD" >/dev/null && BASE_REF=FETCH_HEAD
fi

if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
    # Deliberately not a pass and not a failure. A workstation with no
    # network cannot answer this, and answering it wrong in the green
    # direction is how a gate stops meaning anything.
    echo "NOT INSPECTED  cannot read '$BASE_REF' — no default branch to compare against."
    echo "  This is the one thing this check needs; CI fetches it, so the"
    echo "  verdict there is the authoritative one."
    exit 0
fi

# THE #977 EXEMPTION, DECIDED ONCE, HERE.
#
# Three things have to hold, and each one is a separate way the fix
# could be wrong rather than a belt-and-braces list:
#
#   - the run is a PULL REQUEST. `GITHUB_BASE_REF` is set on a pull
#     request and empty on a push, but an environment can carry a stale
#     one, and "a push was exempted" is silent in both directions. The
#     event name is what says which kind of run this is.
#   - its base IS the default branch, DERIVED. Comparing against the
#     string `main` would exempt every pull request into `dev` the day
#     the default branch is renamed, which is the #665 failure produced
#     by the fix for #977.
#   - the ref this gate compares against is that same branch. Otherwise
#     the exemption would report "reachable" about a branch nobody asked
#     about.
#
# Two derivations, because neither is available everywhere: a hosted
# runner has the event payload and no `origin/HEAD`; a clone has
# `origin/HEAD` and no event payload. Where BOTH answer and they
# DISAGREE, the exemption is refused rather than letting the looser one
# decide.
#
# Declining is printed. A fix that silently does nothing on the one run
# it was written for is indistinguishable from a fix that works, until
# the release it was supposed to unblock.
branch_name() {
    local r="${1#refs/heads/}"
    r="${r#refs/remotes/}"
    printf '%s' "${r#origin/}"
}

DEF_FROM_EVENT=""
if [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -r "${GITHUB_EVENT_PATH:-}" ] \
   && command -v jq >/dev/null 2>&1; then
    DEF_FROM_EVENT=$(jq -r '.repository.default_branch // empty' \
        "${GITHUB_EVENT_PATH}" 2>/dev/null)
fi
DEF_FROM_HEAD=$(git symbolic-ref --short --quiet refs/remotes/origin/HEAD 2>/dev/null)
[ -z "$DEF_FROM_HEAD" ] || DEF_FROM_HEAD=$(branch_name "$DEF_FROM_HEAD")

DEFAULT_BRANCH=""
MERGES_INTO_DEFAULT=0
PR_BASE=""
case "${GITHUB_EVENT_NAME:-}" in
    pull_request|pull_request_target) PR_BASE=$(branch_name "${GITHUB_BASE_REF:-}") ;;
esac

if [ -n "$PR_BASE" ]; then
    decline=""
    if [ -n "$DEF_FROM_EVENT" ] && [ -n "$DEF_FROM_HEAD" ] \
       && [ "$DEF_FROM_EVENT" != "$DEF_FROM_HEAD" ]; then
        decline="the event payload says the default branch is '$DEF_FROM_EVENT' and origin/HEAD says '$DEF_FROM_HEAD'"
    else
        DEFAULT_BRANCH="${DEF_FROM_EVENT:-$DEF_FROM_HEAD}"
        if [ -z "$DEFAULT_BRANCH" ]; then
            decline="the default branch could not be derived — no readable event payload and no origin/HEAD"
        elif [ "$PR_BASE" = "$DEFAULT_BRANCH" ]; then
            if [ "$(branch_name "$BASE_REF_SPEC")" = "$DEFAULT_BRANCH" ]; then
                MERGES_INTO_DEFAULT=1
            else
                decline="this gate was pointed at '$BASE_REF_SPEC', which is not the default branch '$DEFAULT_BRANCH'"
            fi
        fi
    fi
    if [ "$MERGES_INTO_DEFAULT" -eq 0 ] && [ -n "$decline" ]; then
        echo "NOTE  this is a pull request into '$PR_BASE' and the merge-reaches-the-default-branch"
        echo "      exemption (#977) was NOT applied: ${decline}."
        echo "      The verdict below is the one a push would get."
    fi
fi

# IS A RELEASE IN FLIGHT? (#977 round 1.)
#
# One fact, derived the same way on both sides, from the pin
# scripts/bump-version.sh rewrites on the release branch at runbook
# step 2. Reading it from one file on each side and requiring exactly
# one distinct version there is what keeps this from being a second,
# looser idea of "the version": zero pins and two different pins both
# mean "cannot tell", and cannot-tell is NOT in flight, so the default
# is the strict pre-existing verdict.
VERSION_PIN_FILE="${VERSION_PIN_FILE:-README.md}"

# pin_version <text> -> the single vX.Y.Z pinned in it, or nothing
pin_version() {
    local vs
    vs=$(printf '%s' "$1" \
        | grep -oE 'ghcr\.io/[^/[:space:]]+/docker-net-dhcp:v[0-9]+\.[0-9]+\.[0-9]+' \
        | sed 's/.*://' | sort -u)
    [ "$(printf '%s' "$vs" | grep -c .)" -eq 1 ] || return 0
    printf '%s' "$vs"
}

TREE_VERSION=""
BASE_VERSION=""
[ -f "$VERSION_PIN_FILE" ] && TREE_VERSION=$(pin_version "$(cat "$VERSION_PIN_FILE")")
BASE_VERSION=$(pin_version "$(git show "${BASE_REF}:${VERSION_PIN_FILE}" 2>/dev/null)")

# THE SUSPENSION IS BOUNDED BY SHAPE, BECAUSE THE LEDGER IT SERVES
# DEMANDS A BOUND (#977 round 2) AND SHAPE IS THE ONLY BOUND TWO PINS CAN
# CARRY (#977 round 3). "An accepted condition with a written expiry,
# never a bare entry" is the bargain at the top of this file, and a
# suspension that applies to any pair of differing pins is a bare entry.
#
# What is measured is the DISTANCE between the two pins, from the two
# pins alone. A release moves the version one step -- the next patch,
# the next minor, or the next major -- so the suspension holds only
# while the tree pins the immediate successor of what the default branch
# pins, and two steps is refused.
#
# THAT IS NOT A BOUND ON HOW LONG, and the first version of this comment
# said it was. A release parked after runbook step 5 leaves `dev` pinning
# exactly ONE version more than `main`, which is the shape this accepts,
# so the finding stays suspended for as long as nobody finishes or
# unwinds that release -- measured: 51 commits later, still suspended.
# Nothing here reads a clock, a tag or the state of a release, and no
# duration is derivable from two version strings. Bounding time would
# mean inventing a parameter this file cannot measure, which is the
# other way to get a sentence wider than the code. So the bound is
# stated as what it is, in the four places that state it.
successors() {
    local v="${1#v}" maj min pat
    IFS=. read -r maj min pat <<< "$v"
    case "${maj}${min}${pat}" in *[!0-9]*|'') return 0 ;; esac
    printf 'v%d.%d.%d\nv%d.%d.0\nv%d.0.0\n' \
        "$maj" "$min" "$((pat + 1))" "$maj" "$((min + 1))" "$((maj + 1))"
}

RELEASE_IN_FLIGHT=0
PINS_EXPIRED=""
if [ -n "$TREE_VERSION" ] && [ -n "$BASE_VERSION" ] \
   && [ "$TREE_VERSION" != "$BASE_VERSION" ]; then
    if successors "$BASE_VERSION" | grep -Fx "$TREE_VERSION" >/dev/null; then
        RELEASE_IN_FLIGHT=1
    elif [ "$(printf '%s\n%s\n' "$TREE_VERSION" "$BASE_VERSION" | sort -V | tail -n1)" = "$TREE_VERSION" ]; then
        PINS_EXPIRED="this tree pins ${TREE_VERSION} and ${BASE_REF} pins ${BASE_VERSION}, which is more than one release step"
    fi
fi

# THE LEDGER'S OWN RULES WERE ENFORCED BY NOTHING (#849). Its header says
# "An entry requires a reason and what clears it. No bare paths." Strip the
# Reason and Clears blocks off the one real entry, leave the bare path, and
# this gate returned a BYTE-IDENTICAL pass -- measured at 03bd18d before
# this change. It read an entry for PRESENCE and never for content, which
# is the same defect as the allowlist it is modelled on accepting a bare
# CVE id.
#
# The old extraction was `awk '{print $1}'` over every non-comment line, so
# the first word of every prose continuation became a "declared path": 14
# junk tokens (`Reason:`, `An`, `just`, `100` …) beside the one real entry.
# They matched nothing only because `grep -Fx` compares them against real
# paths and no sentence begins with one. Harmless by coincidence, and a
# prose line that happened to start with a workflow path would have
# silently declared it. A path is now a line at COLUMN ZERO; a field is an
# indented `Name: value`; anything else indented continues the field above.
# THE DELIMITER CANNOT BE WHITESPACE. This parser emitted tab-separated
# fields and the reader was `while IFS=$'\t' read -r p_path p_r p_c p_tg`.
# Tab is IFS whitespace, so a RUN of tabs collapses into one separator and
# every later value shifts left:
#
#   printf 'p\t\tC\tT\n'         | IFS=$'\t'   read a b c d  -> b=[C] c=[T] d=[]
#   printf 'p\x1f\x1fC\x1fT\n'    | IFS=$'\x1f' read a b c d  -> b=[]  c=[C] d=[T]
#
# An entry with an empty Reason therefore made Clears read the TRIGGERS
# text and Triggers read empty -- and empty is the CORRECT answer for a
# workflow with no default-branch-only trigger, so the mismatch rule
# agreed and the entry PASSED. Two rules, the missing-Reason one and the
# false-Triggers one, were disabled together by the absence of one of
# them. Measured on the pre-fix tree: an entry with no Reason and a false
# `Triggers: schedule` on a push-only workflow gave rc=0; adding a Reason
# and moving nothing else gave rc=1.
#
# The same shift produced a FALSE MESSAGE in the ordinary case: an entry
# missing only `Clears:` was reported as a trigger mismatch whose remedy
# line was already present in the file, with no mention of the rule
# actually broken.
#
# 0x1f is the ASCII unit separator: not IFS whitespace, and it cannot
# occur in a path, a field name or ledger prose.
US=$'\x1f'
declare -A DECL_REASON DECL_CLEARS DECL_TRIGGERS
declared=""
if [ -r "$ALLOWLIST" ]; then
    parsed=$(awk -v US="$US" '
      function flush() {
        if (path != "") printf "%s%s%s%s%s%s%s\n", path, US, r, US, c, US, tg
        r=""; c=""; tg=""
      }
      /^[[:space:]]*(#|$)/ { next }
      /^[^[:space:]]/ { flush(); path=$1; field=""; next }
      {
        line=$0
        if (match(line, /^[[:space:]]+[A-Za-z][A-Za-z-]*:/)) {
          field=substr(line, RSTART, RLENGTH); sub(/:$/,"",field)
          gsub(/^[[:space:]]+/,"",field)
          val=substr(line, RSTART+RLENGTH); gsub(/^[[:space:]]+/,"",val)
        } else {
          val=line; gsub(/^[[:space:]]+/,"",val)
        }
        f=tolower(field)
        if (f=="reason")   r = r " " val
        if (f=="clears")   c = c " " val
        if (f=="triggers") tg = tg " " val
      }
      END { flush() }
    ' "$ALLOWLIST")

    # NON-VACUITY ON THE PARSER ITSELF. A ledger with content but zero
    # parsed entries means the format moved, not that nothing is declared,
    # and "nothing is declared" is the answer that lets everything through.
    content=$(grep -cvE '^[[:space:]]*(#|$)' "$ALLOWLIST")
    if [ "${content:-0}" -gt 0 ] && [ -z "$parsed" ]; then
        echo "::error title=Ledger unparsed::${ALLOWLIST} has ${content} non-comment line(s)" \
             "and this gate parsed no entry out of it. Every workflow would read as" \
             "undeclared, which is not a verdict about the tree." >&2
        exit 2
    fi

    while IFS="$US" read -r p_path p_r p_c p_tg; do
        [ -n "$p_path" ] || continue
        declared="$declared$p_path"$'\n'
        DECL_REASON[$p_path]="$(printf '%s' "$p_r"  | tr -s ' ' | sed 's/^ //;s/ $//')"
        DECL_CLEARS[$p_path]="$(printf '%s' "$p_c"  | tr -s ' ' | sed 's/^ //;s/ $//')"
        DECL_TRIGGERS[$p_path]="$(printf '%s' "$p_tg" | tr -s ' ' | sed 's/^ //;s/ $//')"
    done <<< "$parsed"
    declared=$(printf '%s' "$declared" | sort -u)
fi

# The set of triggers GitHub serves ONLY from the default branch. The
# ledger's framing bug (#846) was reasoning about which trigger survives
# instead of measuring: two entries eleven minutes apart said the cron
# keeps working off the default branch, in different words. It does not,
# and neither does workflow_dispatch, so an entry has to account for
# BOTH -- derived from the file, never from the sentence. A gate keyed
# on the wording would have passed both, because both were fluent and
# neither used the same phrasing.
#
# IT READS THE `on:` MAPPING, NOT THE FILE. The first version grepped
# the whole file, so a workflow whose header comment said
#
#     # This workflow deliberately has no schedule: a daily run would
#     # cost pool time for nothing.
#
# derived [schedule workflow_dispatch] from a file that declares only
# workflow_dispatch. The only way to green was to write a false claim
# about the workflow into the ledger -- the exact failure #846 was
# about, manufactured by the gate built to prevent it. A gate that can
# only be satisfied by a lie is worse than no gate: it teaches the
# reader that the ledger is decorative.
#
# Any other column-zero key ends the block, so `jobs:` closes it.
#
# THE SAME SHAPE AS check-fork-execution-policy.sh, which parses the
# `on:` block structurally for this reason rather than grepping the
# file. That script reached `dev` and `main` with the v1.9.0 release.
# This citation was previously dropped while it existed only on the
# unmerged #830 branch: a claim true of the union of the branches in
# flight is not a claim about the merged tree.
#
# COMMENTS INSIDE THE BLOCK ARE STRIPPED TOO, and that is one scope
# smaller than the bug above rather than a different bug. Narrowing the
# scan from the file to the `on:` mapping left
#
#     on:
#       workflow_dispatch:  # not on a schedule: manual only
#
# deriving [schedule workflow_dispatch] — the gate demanding a false
# claim again, just from a comment that had to be inside the block
# instead of anywhere in the file. Both spellings are stripped: a
# whole-line `#` comment, and a trailing one, which YAML introduces
# with a `#` preceded by whitespace.
#
# Measured over every workflow in this tree at the time of writing: 25
# inspected, 0 derive differently with the strip than without it. So
# this was latent -- and latent is exactly how the whole-file version
# of it shipped.
dead_triggers() {
    local f="$1"
    awk '
      {
        line = $0
        sub(/^[[:space:]]*#.*$/, "", line)
        sub(/[[:space:]]+#.*$/,  "", line)
      }
      line ~ /^[A-Za-z_"'"'"'-]+:/ { in_on = (line ~ /^(on|"on"|'"'"'on'"'"'):/) }
      in_on && line ~ /(^|[[:space:],[{])workflow_dispatch([[:space:]]*:|[[:space:]]*[],}]|$)/ { d = 1 }
      in_on && line ~ /(^|[[:space:],[{])schedule([[:space:]]*:|[[:space:]]*[],}]|$)/          { c = 1 }
      END { if (c) printf "schedule "; if (d) printf "workflow_dispatch" }
    ' "$f" | sed 's/ $//'
}

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

# EVERY DECLARED ENTRY MUST CARRY ITS OWN RULES, and the trigger list is
# derived from the workflow rather than read as prose. Keying on the
# SPELLING of the reason would reproduce exactly the silence #846 was
# about: both false entries said "the schedule works from any branch" in
# perfectly well-formed English. The subject is property-keyed -- what the
# file actually declares -- and only the predicate, the field name, is a
# spelling this gate can demand.
#
# THE DESIGN LIMIT, STATED HERE RATHER THAN DISCOVERED LATER. A derived
# fact placed BESIDE a written one does not contradict it. `Triggers:`
# can only ever be correct next to a false Reason, never instead of one,
# and this gate has no way to tell a true reason from a fluent invented
# one -- checking that a reason is PRESENT is not checking that it is
# TRUE. That is not a gap to be closed by a cleverer pattern; it is the
# boundary of what a mechanical check can say about prose. The entry
# this file guards is itself the standing example: its reason carried a
# retracted claim about authorship for as long as the retraction sat in
# the header above it, with the derived line beside it correct
# throughout.
while IFS= read -r rel; do
    [ -n "$rel" ] || continue

    if [ ! -e "$rel" ]; then
        note "'$rel' is declared in ${ALLOWLIST} but no such file exists."
        echo "  An entry for a file that is gone declares nothing. Remove it." >&2
        continue
    fi

    [ -n "${DECL_REASON[$rel]:-}" ] || {
        note "'$rel' has no 'Reason:' in ${ALLOWLIST}."
        echo "  The ledger's own header says an entry requires a reason and what" >&2
        echo "  clears it, and no bare paths. Until #849 nothing checked that:" >&2
        echo "  a bare path passed byte-identically to a full entry." >&2
    }
    [ -n "${DECL_CLEARS[$rel]:-}" ] || {
        note "'$rel' has no 'Clears:' in ${ALLOWLIST}."
        echo "  An accepted condition with no written expiry is how a temporary" >&2
        echo "  exception becomes permanent." >&2
    }

    want=$(dead_triggers "$rel")
    got=$(printf '%s' "${DECL_TRIGGERS[$rel]:-}" | tr ' ' '\n' | grep -v '^$' | sort -u | tr '\n' ' ' | sed 's/ $//')
    if [ "$got" != "$want" ]; then
        note "'$rel' declares triggers [$want] but its entry says [${got:-none}]."
        echo "  Every trigger in the first list is served ONLY from the default" >&2
        echo "  branch, so every one of them is dead while this entry stands." >&2
        if [ -n "$want" ]; then
            echo "  Write 'Triggers: $want' in the entry. This is read from the" >&2
        else
            echo "  This workflow declares NO default-branch-only trigger, so the" >&2
            echo "  entry claims something the file does not say. Drop the claim," >&2
            echo "  or the entry. This is read from the" >&2
        fi
        echo "  workflow file, not from the reason: the two false entries in" >&2
        echo "  #846 were fluent English in two different phrasings, so a gate" >&2
        echo "  keyed on wording would have passed both." >&2
    fi
done <<< "$declared"

pending=""
merging=""
travelling=""
expired=""
inspected=0
for f in "${WF_FILES[@]}"; do
    [ -e "$f" ] || continue
    # `on:` may be block or inline, and the comment above this line used
    # to promise that while the pattern delivered only the block form
    # (#743). Verified against GitHub's accepted spellings: `on:
    # [workflow_dispatch]` and `on: {workflow_dispatch: null}` both
    # produced zero matches. All 24 workflows happen to use the block
    # form, so it was latent — but latent plus a vacuous pass means a
    # reformat would have emptied this gate's input set with nothing to
    # notice, which is the pairing that makes each half worse.
    #
    # A key in a block, an item in a flow sequence, or a key in a flow
    # mapping. Anchored to a boundary either side so a workflow merely
    # mentioning the word in prose is not counted as declaring it.
    grep -E '(^|[[:space:]]|[[,{])workflow_dispatch([[:space:]]*:|[[:space:]]*[],}]|$)' \
        "$f" >/dev/null || continue

    inspected=$((inspected + 1))

    rel="${f#./}"
    on_default=0
    git cat-file -e "${BASE_REF}:${rel}" 2>/dev/null && on_default=1

    # Reachable, by either route: it is on the default branch already, or
    # this pull request is what puts it there (#977). The stale rule is
    # NOT suspended by the second route — that is the half that keeps the
    # default branch green after the merge, and the reason the release PR
    # is where the entry gets removed.
    if [ "$on_default" -eq 1 ] || [ "$MERGES_INTO_DEFAULT" -eq 1 ]; then
        [ "$on_default" -eq 1 ] || merging="$merging $rel"
        if printf '%s\n' "$declared" | grep -Fx "$rel" >/dev/null; then
            if [ "$on_default" -eq 1 ]; then
                note "'$rel' is on ${BASE_REF} but still declared in ${ALLOWLIST}."
                echo "  It is dispatchable now; the entry has stopped meaning anything." >&2
                echo "  Remove it." >&2
            else
                note "'$rel' is still declared in ${ALLOWLIST} and merging this pull request puts it on ${DEFAULT_BRANCH}."
                echo "  The entry is stale the moment this merges, and this same gate then" >&2
                echo "  goes red on ${DEFAULT_BRANCH} one commit later (#977). Remove it here," >&2
                echo "  in this pull request." >&2
            fi
        fi
        continue
    fi

    if printf '%s\n' "$declared" | grep -Fx "$rel" >/dev/null; then
        # DECLARED pending, and that is the only list this word may
        # name. It used to collect every workflow that reached this
        # point, suspended ones included, and the summary then reported
        # a workflow as "declared pending" with an empty ledger (#977
        # round 2, F1).
        pending="$pending $rel"
    else
        # The finding is suspended while the pins differ by one release
        # step (#977 round 1). In a release that is the removal in
        # transit: the runbook prunes the ledger beside the pin bump, so
        # the entry is gone here and reaches the default branch with the
        # workflow. This gate does not check that a release is
        # happening; see the bound in the header.
        if [ "$RELEASE_IN_FLIGHT" -eq 1 ]; then
            travelling="$travelling $rel"
            continue
        fi
        [ -z "$PINS_EXPIRED" ] || expired="$expired $rel"
        note "'$rel' declares workflow_dispatch but is not on ${BASE_REF}."
        echo "  GitHub only exposes a dispatchable workflow from the DEFAULT" >&2
        echo "  branch, so 'gh workflow run $(basename "$rel")' answers 404 today —" >&2
        echo "  and any documentation telling a reader to run it is wrong until" >&2
        echo "  the next release ships." >&2
        echo "  Either say so where it is documented and add an entry to" >&2
        echo "  ${ALLOWLIST} with the reason and what clears it, or do not" >&2
        echo "  present it as a route yet." >&2
    fi
done

# EVERY LINE HERE IS A FACT THIS RUN DERIVED, and the one thing it did
# NOT derive is labelled as such (#977 round 2, F2). The first version
# of this block announced a release, a release branch, a pruned entry
# and a release pull request, none of which this gate ever looks for.
if [ -n "$travelling" ]; then
    echo "NOTE  this tree pins ${TREE_VERSION} and ${BASE_REF} pins ${BASE_VERSION}, one release step behind it."
    echo "      The undeclared-workflow finding is suspended for:${travelling}"
    echo "      WHAT THIS RUN CHECKED IS THE TWO PINS, AND NOTHING ELSE. It did not"
    echo "      look for a release branch, a pruned ledger entry or a release pull"
    echo "      request, and it cannot tell a release from a bare pin bump on any"
    echo "      branch, which scripts/check-version-pins.sh permits. In a release"
    echo "      this state is the removal in transit: the runbook prunes the ledger"
    echo "      beside the pin bump at step 2, so the entries are gone here and"
    echo "      reach ${BASE_REF} with the workflows when the release merges."
    echo "      While the pins differ this suspends the finding for EVERY"
    echo "      dispatchable workflow, so one merged undeclared here is not caught"
    echo "      until they agree again. The stale rule is not suspended."
fi

# THE EXPIRY, SAID OUT LOUD WHEN IT BITES. Without this the refusal above
# reads as "add an entry" to someone who has a bumped pin and believes
# the suspension applies.
if [ -n "$expired" ]; then
    echo "NOTE  the pin suspension did NOT apply:${expired}" >&2
    echo "      ${PINS_EXPIRED} ahead. A release moves the version exactly one step," >&2
    echo "      so two steps means the first release never landed and this" >&2
    echo "      suspension has expired. Finish or unwind that release, or declare" >&2
    echo "      the workflow in ${ALLOWLIST} as any other pending workflow." >&2
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi

# Zero dispatchable workflows out of a non-empty directory is a real
# answer today, but it is also exactly what a broken detector looks
# like, so it is stated rather than hidden inside a "PASS".
if [ "$inspected" -eq 0 ]; then
    echo "::error title=Nothing to inspect::${#WF_FILES[@]} workflow(s) in $WF_DIR" \
         "and none declares workflow_dispatch. Either that is new, or this" \
         "gate's detector has stopped matching the form in use." >&2
    exit 2
fi

if [ -n "$merging" ]; then
    # Never the "are on ${BASE_REF}" line here: they are not, and a false
    # sentence in the evidence trail of a release is worth more than the
    # one branch it saves.
    echo "PASS  ${inspected} dispatch target(s) reachable; merging this pull request into" \
         "${DEFAULT_BRANCH} is what puts these there:$merging"
elif [ -n "$pending" ] || [ -n "$travelling" ]; then
    # NEVER "reachable on ${BASE_REF}" for either list: neither kind is
    # there, which is the whole reason it is being reported. Same rule as
    # the merging line above, and F1 of #977 round 2 is what happens when
    # one of the two lists is folded into a sentence written for the
    # other.
    line="PASS  ${inspected} dispatch target(s) inspected"
    [ -z "$pending" ] || line="$line; not on ${BASE_REF} and declared in ${ALLOWLIST}:$pending"
    [ -z "$travelling" ] || line="$line; not on ${BASE_REF}, not declared, and suspended by the pin comparison above:$travelling"
    echo "$line"
else
    echo "PASS  all ${inspected} workflow_dispatch workflow(s) are on ${BASE_REF}"
fi
