#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The three settings that stand between a fork PR and root on the
# self-hosted pool (#830).
#
# WHAT IS ACTUALLY AT STAKE. TWO workflows can be reached by a fork
# pull request AND place jobs off the GitHub-hosted images:
# `coverage.yml` and `integration.yml`. Neither carries a fork guard --
# no `if: github.event.pull_request.head.repo.full_name ==
# github.repository` anywhere in the tree. The integration lane runs as
# root. So the approval policy below is not defence in depth; on the day
# it is relaxed it is the ONLY thing that was stopping fork-authored
# code from executing there, and nothing in the repository goes red.
#
# THE NUMBER WAS FIVE HERE UNTIL IT WAS MEASURED. This header claimed
# coverage-presence, release-backmerge and test as well; all three run
# every job on `ubuntu-latest`. That is not a nit in a security file. A
# reader acting on the old sentence adds three `if:` conditions that
# guard nothing and concludes the window is shut -- a false remedy
# printed beside a real risk, overstating exposure two-and-a-half-fold
# in the direction that flatters this gate. The population is now
# DERIVED below and compared against the two named above, so the next
# time it moves this refuses instead of misinforming.
#
# AND THE DERIVATION ITSELF WAS WRONG BEFORE #844. It was two line
# regexes: one spelling of `runs-on`, one trigger. Seven legal shapes
# were invisible and every miss was permissive, so the sentence above
# was checked by something that could not have caught the thing it
# claimed to catch. The scan is a YAML parse now, and the trigger domain
# is "a fork can reach it" rather than "the word pull_request appears" --
# both are argued where the code is, below.
#
# #593 was this class -- an untrusted ref reaching credentialed,
# root, self-hosted jobs -- and the answer then was a gate.
#
# WHY NOT JUST ADD THE FORK GUARD, now that it is two `if:` lines rather
# than five. Because the guard and the policy do different things: the
# approval policy is a maintainer-gated YES -- an outside contributor's
# integration run can be approved and will then execute -- while a fork
# guard is a permanent NO. It would mean an approved fork PR can never
# run integration, ever, with no way for the maintainer to grant it.
# That is a real capability to give up, and it is the reason this is a
# gate and not two lines, even though two lines would be cheaper and
# would prevent rather than notice.
#
# STATED PLAINLY: THIS IS PROPHYLACTIC. No incident sits behind it.
# Precautionary gates are a small minority of the corpus -- see
# `check-apk-pins.sh` for the other shape -- and that is recorded here,
# in the header, because the finding of the CI review is that gates get
# added faster than the reason for them gets recorded, so the person
# later deciding whether this can go needs to see "no incident yet" from
# inside the file rather than from an issue.
#
# The count that used to sit in that sentence is gone on purpose. It
# measured 60 when written, 62 a day later, and the header never named
# the population it was counting, so no reader could check it and no
# author could maintain it. A named example survives; a number rots.
#
# THE SETTINGS LIVE IN THE WEB UI, NOT IN THE TREE. That is exactly why
# they need a gate: `git log` cannot show the change, a diff cannot show
# the change, and review cannot catch it. It is also why this gate is
# the only kind that must reach the network to do its job.
#
# WHY THIS NEEDS AN ELEVATED TOKEN, AND WHICH ONE. The default
# GITHUB_TOKEN cannot read repository administration settings -- the
# tree already records this at scorecard.yml, where SCORECARD_TOKEN, a
# read-only single-repo fine-grained PAT with Administration: read,
# exists for the same reason. This gate reuses it rather than adding a
# second credential. A consequence follows and is not negotiable: this
# must NOT run in a lane that fork pull requests can reach. Secrets are
# withheld from fork PRs, so there it would refuse on every run and be
# discharged as noise within a month.
#
# THREE VERDICTS. "I could not ask" and "the answer is wrong" are
# different, and collapsing them sends the reader at 02:00 to the wrong
# place. A drifted setting is a failure; an unreadable endpoint is a
# refusal that names which side went dark.
#
# Inputs (environment):
#   REPO       owner/name to query (default: the canonical repository)
#   GH_TOKEN   passed through to `gh`; needs Administration: read
#
# Exit: 0 all three settings are as documented
#       1 at least one has drifted -- each is named with want vs got
#       2 CANNOT JUDGE -- a query failed or answered in a shape that is
#         none of the documented values

set -uo pipefail

REPO="${REPO:-claymore666/docker-net-dhcp}"

refuse() {
    echo "::error title=Fork-execution policy cannot be judged::$*" >&2
    exit 2
}

# THE ENUMERATION NEEDS A WATCHER, OR IT IS A RULE ENFORCED BY READING.
# The header's whole argument rests on WHICH workflows expose the pool.
# The settings are watched; the population whose exposure is the reason
# to watch them was not, so nothing went red the day a third
# `pull_request` workflow gained a self-hosted job. This gate's own
# thesis is that a rule nothing executes is not enforced -- and until
# now this enumeration was one.
#
# So derive it and refuse on divergence. `check-dispatch-reachable.sh`
# is the precedent: derive the subjects, and refuse at zero rather than
# pass over nothing.
#
# THE FIRST VERSION OF THIS OBSERVER WAS KEYED ON A SPELLING, and its
# claim was wider than its code. It matched `/runs-on:.*self-hosted/`
# and nothing else, while this header, the workflow and the pull request
# all said a new self-hosted job "cannot be added silently". MEASURED at
# f1aceb8: it detected exactly ONE spelling out of the shapes GitHub
# accepts, and EVERY miss was in the permissive direction --
#
#   runs-on:                     a block sequence
#     - self-hosted
#   runs-on: [dhcp-ci]           the custom label alone, which routes to
#                                the private pool just as well
#   runs-on: ${{ matrix.runner }}    with self-hosted in the matrix; and
#                                `runner-image.yml` ALREADY uses this form
#   runs-on:                     a runner GROUP
#     group: private
#   on: {pull_request: ...}      a flow mapping
#   "pull_request":              a quoted key
#
# -- plus a block-sequence `runs-on:` in a file's SECOND job, which the
# line scanner missed for a different reason again. Every one of those
# is a workflow that reaches the pool and derived as if it did not, so
# the divergence refusal would never have fired and the header would
# have gone on being trusted.
#
# A GATE WHOSE CLAIM IS "ANY SHAPE" CANNOT BE WRITTEN IN LINE REGEXES.
# The scan below hands the file to a YAML parser and asks the resolved
# document, which is the only way the seven shapes above collapse into
# one question. `check-publish-verify-parity.sh` and seven other gates
# here already shell out to python3 for a parse of this kind; this
# follows that convention rather than inventing one.
#
# THE `on:` BLOCK, NOT THE WHOLE FILE, is now structural rather than a
# heuristic: a file-wide grep for `pull_request` matches
# `github.event.pull_request...` in an `if:` and matches prose in a
# comment -- including the comments above -- and both would admit
# workflows that do not trigger on it at all. A parsed document has no
# comments in it and no `if:` expressions in its `on:` mapping.
#
# WHICH TRIGGERS COUNT, AND WHY IT IS NOT JUST `pull_request` (#844).
# The question this gate answers is "can a pull request from a fork
# reach the private pool", not "does the word pull_request appear in
# `on:`". Other triggers carry fork-controlled input, and one of them
# defeats the very setting this gate watches: a workflow with
#
#     on: pull_request_target
#     jobs: { x: { runs-on: [self-hosted, dhcp-ci],
#                  steps: [{uses: actions/checkout@v5,
#                           with: {ref: '${{ github.event.pull_request.head.sha }}'}}] } }
#
# runs fork code on the pool AND bypasses the approval policy entirely,
# because bypassing it is what `pull_request_target` is FOR. `workflow_run`
# and `issue_comment` are the same class.
#
# AND THE TRIGGER SET IS NOT WRITTEN AS AN ENUMERATION OF THE DANGEROUS
# ONES. A list of four dangerous triggers is the same shape as the
# `runs-on` regex this change removes: it excludes the fifth one
# silently, in the permissive direction. The scan names the triggers no
# outsider can cause -- push, schedule, workflow_dispatch and the rest --
# and counts EVERYTHING ELSE, so a trigger nobody thought of refuses
# rather than disappearing. The list and its price are argued where the
# constant is.
#
# WHICH OF THOSE EXIST TODAY IS DERIVED AND PRINTED, not asserted here.
# Every run prints the outsider-reachable census -- the distinct triggers
# and the workflows carrying them -- and a self-test case pins the set
# against the REAL workflow directory. The two facts worth knowing while
# reading this file, both of which that case will contradict the moment
# they stop being true: the one `pull_request_target` workflow is
# `issue-state-labels.yml`, safe today because every job is on
# `ubuntu-latest` and it pins `ref: dev`; and the one `issues` workflow
# is `issue-labeler.yml`, safe today for the same reason.
#
# THIS PARAGRAPH IS WHERE A CENSUS SENTENCE GOT IT WRONG. It said the
# triggers in use were push, schedule, workflow_dispatch, pull_request
# and pull_request_target, MEASURED at a named SHA, and `issues` was in
# the tree at that SHA. The code was right; the sentence was not; nothing
# checked it. A fact left true by the accident of the current tree is an
# unrun checklist -- and so is a fact left FALSE by it.
#
# WHAT COUNTS AS REACHING THE POOL. Any label set that is not entirely
# GitHub-hosted -- `ubuntu-*`, `windows-*`, `macos-*`. Not "contains
# self-hosted": `runs-on: [dhcp-ci]` routes to this project's private
# runners without the word appearing. The boundary of that rule, stated
# rather than discovered: a self-hosted runner deliberately labelled
# `ubuntu-something` would read as hosted here. Nothing in this project
# does that, and the alternative -- an allowlist of exact hosted labels
# -- goes stale in the permissive direction every time GitHub adds an
# image, which is the failure this whole file is about.
#
# AND IT FAILS CLOSED. A `runs-on` this parser cannot resolve -- an
# expression that is not `matrix.<key>`, a job with no `runs-on` because
# it `uses:` a reusable workflow whose runners are in another file -- is
# counted as reaching the pool. "I could not tell" must send a human to
# look, never render a clean population.
EXPOSED_DECLARED="coverage.yml integration.yml"
WF_DIR="${WF_DIR:-.github/workflows}"

command -v python3 >/dev/null 2>&1 || refuse \
    "python3 is required to derive which workflows expose the self-hosted pool, and it is not on PATH. The population this gate's whole argument rests on cannot be read, so nothing below is a measurement."

derive_exposed() {
    local files=()
    shopt -s nullglob
    files=("$WF_DIR"/*.yml "$WF_DIR"/*.yaml)
    shopt -u nullglob
    if [ "${#files[@]}" -eq 0 ]; then
        printf 'ERROR\t%s\tno *.yml or *.yaml files in this directory\n' "$WF_DIR"
        return 0
    fi
    python3 - "${files[@]}" <<'PARSE'
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.stderr.write("PyYAML is not importable by this python3\n")
    sys.exit(3)

# THIS IS A DENYLIST INVERTED ON PURPOSE, and the inversion is the whole
# point. The obvious spelling is a set of dangerous triggers --
# {pull_request, pull_request_target, workflow_run, issue_comment} -- and
# that set is an ENUMERATION, which is the exact defect this gate is being
# fixed for: an enumeration silently excludes everything added after it
# was written, and it excludes it in the permissive direction. A fifth
# fork-influenceable trigger, or one GitHub has not shipped yet, would
# read as "not exposure" and nothing would go red.
#
# So the constant below names the triggers NO OUTSIDER CAN CAUSE OR STEER
# -- a `push` needs write access, a `schedule` is the repository's own
# clock, a `workflow_dispatch` needs a permission -- and EVERYTHING ELSE
# counts as fork-reachable. A trigger this list has never heard of is
# therefore counted, the derived population diverges, and this gate
# refuses and sends a human to look. That is the residue: the unknown
# case fails closed instead of vanishing.
#
# The price is stated rather than discovered: a workflow on an
# outsider-visible-but-harmless trigger with a job off the hosted images
# will refuse and need a human to widen this list deliberately.
#
# WHETHER ANYTHING PAYS THAT PRICE TODAY IS NOT WRITTEN HERE, and the
# reason is that it WAS written here and it was wrong. This comment used
# to name the triggers in use -- push, schedule, workflow_dispatch,
# pull_request, pull_request_target -- as MEASURED over 25 workflows.
# The count was right and the census was not: `issue-labeler.yml`
# triggers on `issues`, which any GitHub account can cause. The code
# counted it correctly; only the sentence was wrong, and it made the
# outsider-reachable surface look like two triggers when it is three.
# A new un-derived enumeration in the file whose entire subject is that
# enumerations rot -- which is why there is no replacement sentence.
# The census is DERIVED and printed on every run, and the set is pinned
# against the real tree by a self-test case.
#
# `pull_request_target` deserves its own sentence because it is the one
# that defeats rather than relaxes what this gate watches: it runs with
# repository credentials and can be pointed at the fork's head ref, so a
# self-hosted job on it would route around the approval policy entirely,
# which is what that trigger exists to do.
SAFE_TRIGGERS = {
    "branch_protection_rule",
    "create",
    "delete",
    "deployment",
    "deployment_status",
    "merge_group",
    "page_build",
    "push",
    "registry_package",
    "release",
    "repository_dispatch",
    "schedule",
    "workflow_call",
    "workflow_dispatch",
}

# The GitHub-hosted image families. Everything else -- `self-hosted`, a
# bare custom label, a runner group, an expression that cannot be
# resolved -- reaches this project's private pool.
HOSTED = re.compile(r"^(ubuntu|windows|macos)-[A-Za-z0-9._-]+$")

# A `runs-on` that is EXACTLY one expression, e.g. `${{ matrix.runner }}`.
EXPR = re.compile(r"^\$\{\{\s*(.+?)\s*\}\}$")


def on_triggers(doc):
    # YAML 1.1 -- which is what PyYAML implements -- resolves a bare
    # `on` to the boolean True, so the key of a workflow's trigger block
    # is `True` and not the string "on" unless it was quoted. Reading
    # only doc["on"] finds nothing in every real workflow in this tree.
    #
    # RETURNS None, NOT AN EMPTY SET, WHEN THERE IS NOTHING TO READ, and
    # the caller refuses on it. Every valid workflow has a trigger block,
    # so an absent one -- or one that resolves to null, or to a
    # collection with no trigger names in it -- is a parse this gate does
    # not understand. The `jobs:` arm below already reasoned exactly this
    # way; returning an empty set here made the same shape SKIP the file
    # instead, which is the permissive answer to "I could not tell" and
    # the asymmetry was against this file's own rule.
    for key, value in doc.items():
        if key is True or (isinstance(key, str) and key.lower() == "on"):
            if isinstance(value, str):
                return {value} if value else None
            if isinstance(value, (list, dict)):
                names = {v for v in value if isinstance(v, str)}
                return names or None
            return None
    return None


def matrix_values(job, name):
    strategy = job.get("strategy")
    matrix = strategy.get("matrix") if isinstance(strategy, dict) else None
    if not isinstance(matrix, dict):
        return []
    out = []
    direct = matrix.get(name)
    if isinstance(direct, list):
        out.extend(direct)
    elif direct is not None:
        out.append(direct)
    include = matrix.get("include")
    if isinstance(include, list):
        for entry in include:
            if isinstance(entry, dict) and name in entry:
                out.append(entry[name])
    return out


def expand(value, job):
    """A single runs-on term -> the concrete values it can take.

    `None` in the returned list means this parser could not resolve it,
    which the caller treats as reaching the pool.
    """
    if not isinstance(value, str):
        return [value]
    match = EXPR.match(value.strip())
    if match is None:
        return [None] if "${{" in value else [value]
    expr = match.group(1)
    if expr.startswith("matrix."):
        values = matrix_values(job, expr[len("matrix."):])
        return values if values else [None]
    return [None]


def label_sets(runs_on, job):
    """-> a list of candidate label sets; `None` means unresolvable."""
    if runs_on is None:
        return [None]
    if isinstance(runs_on, dict):
        # `runs-on: {group: ..., labels: [...]}`. A runner GROUP is a
        # self-hosted concept; naming one is enough to reach the pool.
        if "group" in runs_on:
            return [["group:%s" % runs_on["group"]]]
        return label_sets(runs_on.get("labels"), job)
    if isinstance(runs_on, list):
        labels, unresolved = [], False
        for item in runs_on:
            for candidate in expand(item, job):
                if candidate is None:
                    unresolved = True
                elif isinstance(candidate, list):
                    labels.extend(str(x) for x in candidate)
                else:
                    labels.append(str(candidate))
        return [None] if unresolved else [labels]
    out = []
    for candidate in expand(runs_on, job):
        if candidate is None:
            out.append(None)
        elif isinstance(candidate, list):
            out.append([str(x) for x in candidate])
        else:
            out.append([str(candidate)])
    return out


def reaches_pool(labels):
    if not labels:
        return True
    return not all(HOSTED.match(label) for label in labels)


for path in sys.argv[1:]:
    try:
        with open(path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
    except Exception as exc:                      # noqa: BLE001
        detail = " ".join(str(exc).split())[:200]
        print("ERROR\t%s\t%s" % (path, detail))
        continue
    if not isinstance(doc, dict):
        print("ERROR\t%s\ttop level is not a mapping" % path)
        continue
    raw = on_triggers(doc)
    if raw is None:
        print("ERROR\t%s\thas no readable 'on:' trigger block, so whether an "
              "outsider can reach it cannot be read" % path)
        continue
    triggers = sorted(raw - SAFE_TRIGGERS)
    if not triggers:
        continue
    # THE CENSUS IS EMITTED HERE, for every outsider-reachable workflow
    # and not only for the ones that also reach the pool. The header used
    # to carry that census as a written sentence and it was false at the
    # very SHA it named -- it missed `issues`. A derived line cannot be
    # false; a sentence can, and did, in the flattering direction.
    print("FORKREACHABLE\t%s\t%s" % (os.path.basename(path), ",".join(triggers)))
    # A fork-reachable workflow whose jobs cannot be read is the one
    # place left where "I could not tell" could have become "nothing to
    # report". Every valid workflow has a `jobs:` mapping, so an absent
    # or malformed one is a parse this gate does not understand, and it
    # says so rather than deriving a smaller population.
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        print("ERROR\t%s\ttriggers on %s but has no readable 'jobs:' mapping"
              % (path, ",".join(triggers)))
        continue
    for job_id, job in jobs.items():
        if not isinstance(job, dict):
            print("ERROR\t%s\tjob '%s' is not a mapping, so its runner cannot be read"
                  % (path, job_id))
            continue
        if any(reaches_pool(s) for s in label_sets(job.get("runs-on"), job)):
            print("EXPOSED\t%s\t%s\t%s"
                  % (os.path.basename(path), job_id, ",".join(triggers)))
            break
PARSE
}

scan="$(derive_exposed)"
scan_rc=$?
if [ "$scan_rc" -eq 3 ]; then
    refuse "PyYAML is not importable by python3, so the set of workflows that reach the self-hosted pool could not be derived. This gate does not fall back to a line scan: the line scan is what #844 replaced, and it read one spelling out of seven. Install PyYAML in the lane that runs this."
fi
if [ "$scan_rc" -ne 0 ]; then
    refuse "the workflow scan exited $scan_rc. The population this gate's argument rests on was not derived, so nothing below is a measurement."
fi

scan_errors="$(printf '%s\n' "$scan" | awk -F'\t' '$1 == "ERROR" { print "    " $2 ": " $3 }')"
if [ -n "$scan_errors" ]; then
    refuse "could not read every workflow in $WF_DIR, so the derived population is incomplete and an incomplete population is not a smaller one -- it is a wrong one:"$'\n'"$scan_errors"
fi

# THE OUTSIDER-REACHABLE CENSUS IS PRINTED, NOT DESCRIBED.
#
# This gate's argument has two halves: which triggers in this tree can be
# caused by someone with no write access, and which of those workflows
# also place a job off the GitHub-hosted images. The second half has
# refused on divergence since #830. The first half sat in a header
# comment -- "the triggers in use are push, schedule, workflow_dispatch,
# pull_request and pull_request_target" -- and it was FALSE at the SHA it
# named: `issue-labeler.yml` triggers on `issues`, which anyone with a
# GitHub account can cause. The code counted it correctly the whole time.
# Only the sentence was wrong, and it was wrong in the direction that
# made the fork-reachable surface look smaller than it is.
#
# That is the same defect this change exists to remove, so it gets the
# same remedy rather than a corrected sentence: the census is derived on
# every run and printed where a human reading CI sees it, and the
# self-test pins the set against the real tree. There is deliberately no
# refusal on this half -- a new `issues` workflow on a hosted image is
# not a security event and must not go red in CI -- but it does go red in
# the self-test, which is where a stale record belongs.
# The per-file lines carry the COUNT, which moves whenever any workflow
# is added; the summary line carries the SET of distinct triggers, which
# is the thing the header used to assert and the thing a reader reasons
# about. The self-test pins the set and not the count, so a fourteenth
# `pull_request` workflow costs nothing while a first `workflow_run` one
# goes red.
forkreachable=$(printf '%s\n' "$scan" | awk -F'\t' '$1 == "FORKREACHABLE" { print "    " $2 ": " $3 }' | sort)
forkreachable_n=$(printf '%s\n' "$forkreachable" | grep -c .)
forkreachable_n=${forkreachable_n:-0}
forkreachable_set=$(printf '%s\n' "$scan" | awk -F'\t' '$1 == "FORKREACHABLE" { print $3 }' | tr ',' '\n' | grep . | sort -u | paste -sd, - | sed 's/,/, /g')
# THE SET IS PRINTED BRACKETED, and that is not decoration. The
# self-test asserts this line as a substring, and an unbracketed tail is
# a PREFIX -- adding a trigger that sorts last leaves the old text intact
# inside the new line and the assertion still passes. Measured: with the
# set printed bare, dropping a `workflow_run` workflow into the real
# directory left the census case GREEN. A subset assertion standing in
# for a set assertion is the same fail-open shape this whole gate is
# about, reproduced inside the check written to close it. The closing
# bracket is what makes the assertion terminate.
echo "$forkreachable_n workflow(s) in $WF_DIR trigger on something an outsider can cause: [$forkreachable_set]"
[ "$forkreachable_n" -gt 0 ] && printf '%s\n' "$forkreachable"

exposed_now=$(printf '%s\n' "$scan" | awk -F'\t' '$1 == "EXPOSED" { print $2 }' | sort -u | tr '\n' ' ')
exposed_now="${exposed_now% }"
exposed_want=$(printf '%s\n' $EXPOSED_DECLARED | sort | tr '\n' ' ')
exposed_want="${exposed_want% }"

if [ -z "$exposed_now" ]; then
    refuse "derived ZERO workflows that a fork pull request can reach AND that place a job off the GitHub-hosted images, while this gate's entire justification is that '$exposed_want' do. Either the pool is no longer reachable from a pull request -- in which case this gate's reason is gone and it should be reconsidered, not left passing -- or, far more likely, the scan over $WF_DIR stopped matching."
fi

if [ "$exposed_now" != "$exposed_want" ]; then
    refuse "the set of workflows exposing the self-hosted pool to fork-reachable triggers has CHANGED. This header documents '$exposed_want'; the tree now has '$exposed_now'. That enumeration is the reason this gate exists and the thing a reader acts on, so it is corrected here before any setting is judged. If the new set is right, update EXPOSED_DECLARED and the header together."$'\n'"$(printf '%s\n' "$scan" | awk -F'\t' '$1 == "EXPOSED" { print "    " $2 ": job \"" $3 "\" is not on a hosted image, and the workflow triggers on " $4 }')"
fi

# ask <endpoint> <jq> -> prints `value:<v>` | `error:<text>`
#
# `gh api --jq` PRINTS A 4xx ERROR BODY ON STDOUT, so a value read here
# is guarded on SHAPE by its caller and never on emptiness -- an
# unguarded read would take the JSON error object as an answer. That is
# not hypothetical: it is the defect #827 found in check-attestation-
# parity, in a block no test had ever executed. There is deliberately no
# seam above this: the self-test stubs `gh` on PATH so that every line
# below runs exactly as it runs in CI.
ask() {
    local ep="$1" jqexpr="$2" out err rc detail
    err="$(mktemp)"
    out="$(gh api "repos/$REPO/$ep" --jq "$jqexpr" 2>"$err")"
    rc=$?
    if [ "$rc" -eq 0 ] && [ -n "$out" ]; then
        printf 'value:%s' "$out"
    else
        detail="$(tr '\n' ' ' < "$err" | cut -c1-200)"
        if [ -z "${detail// /}" ]; then
            detail="$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
        fi
        if [ -z "${detail// /}" ]; then
            detail="gh exited $rc and printed nothing on either stream"
        fi
        printf 'error:%s' "$detail"
    fi
    rm -f "$err"
}

# expect <label> <endpoint> <jq> <valid-regex> <wanted> <consequence>
#
# THE VALID-REGEX IS NOT DECORATION, AND IT IS THE HALF THAT IS EASY TO
# LEAVE OUT. Without it, `gh` printing a JSON error object on stdout
# yields a non-empty string that is simply != the wanted value, and this
# gate reports DRIFT -- "approval_policy is '{"message":"Not Found"}',
# documented as 'all_external_contributors'" -- sending whoever reads it
# to the settings page to fix a setting that was never measured. An
# unreadable answer must refuse, never disagree.
drift=0
expect() {
    local label="$1" ep="$2" jqexpr="$3" valid="$4" want="$5" why="$6" answer got
    answer="$(ask "$ep" "$jqexpr")"
    case "$answer" in
        value:*) got="${answer#value:}" ;;
        error:*) refuse "could not read $label from $ep: ${answer#error:}. Nothing was measured, so this is not a pass." ;;
        *)       refuse "reading $label returned '${answer:0:60}', which is neither value:<v> nor error:<text>." ;;
    esac

    if ! [[ "$got" =~ $valid ]]; then
        refuse "$label came back as '${got:0:80}', which is not one of the values this setting can hold. The endpoint answered with something that is not this setting -- an error body, a changed schema, or the wrong field. Nothing was measured."
    fi

    if [ "$got" = "$want" ]; then
        echo "  ok   $label = $got"
        return
    fi
    echo "::error title=Fork-execution policy drifted::$label is '$got', documented as '$want'. $why" >&2
    drift=$((drift + 1))
}

expect "approval_policy" \
       "actions/permissions/fork-pr-contributor-approval" \
       ".approval_policy" \
       "^(all_external_contributors|first_time_contributors|first_time_contributors_new_to_github|none)$" \
       "all_external_contributors" \
       "This is the only barrier between a fork pull request and execution on the self-hosted pool, where the integration lane runs as root, because no workflow carries a fork guard. Relaxing it means fork-authored code runs there on the next PR."

expect "default_workflow_permissions" \
       "actions/permissions/workflow" \
       ".default_workflow_permissions" \
       "^(read|write)$" \
       "read" \
       "A write-by-default GITHUB_TOKEN hands every job in every workflow push access, including jobs that only needed to read."

expect "can_approve_pull_request_reviews" \
       "actions/permissions/workflow" \
       ".can_approve_pull_request_reviews" \
       "^(true|false)$" \
       "false" \
       "Letting Actions approve pull requests lets a workflow satisfy the review requirement that branch protection exists to impose."

if [ "$drift" -gt 0 ]; then
    echo "::error title=Fork-execution policy drifted::$drift of 3 settings differ from what is documented. Each is named above with what it is and what it should be." >&2
    exit 1
fi

echo "fork-execution policy: all 3 settings are as documented for $REPO."
exit 0
