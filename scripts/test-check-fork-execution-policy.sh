#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-fork-execution-policy.sh (#830).
#
# THERE IS NO SEAM IN THE GATE, ON PURPOSE. `gh` is stubbed on PATH, so
# every line of the checker -- the API call, the rc test, the shape
# guard, the drift comparison -- runs here exactly as it runs in CI.
# #827 is why: check-attestation-parity took an env-var seam that
# returned the CLASSIFIED verdict, so the classification it existed to
# perform had never executed while the gate scored clean on every axis.
#
# THE STUB IS PREPENDED, NEVER ASSIGNED. The checker shells out to
# mktemp, tr and cut, and this suite invokes it with `bash`; replacing
# PATH exits 127 before the gate runs a line.
#
# THE STUB IS WITNESSED. Every case asserts how many times `gh` was
# called. Without that, a case where PATH did not take reaches the real
# `gh`, makes a live network call, and -- for the refusal cases -- exits
# with precisely the code the test wanted. Measured on #827: three of
# seven cases returned the right exit code having invoked nothing.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-fork-execution-policy.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

STUB="$TMP/bin"; mkdir -p "$STUB"
export GH_CALLS="$TMP/calls" GH_MODE="$TMP/mode"

cat > "$STUB/gh" <<'STUBEOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GH_CALLS"
field=""
for a in "$@"; do case "$a" in .*) field="${a#.}" ;; esac; done
mode="$(cat "$GH_MODE" 2>/dev/null || echo ok)"
case "$mode" in
    # A 403 from a token without Administration: read -- the shape a
    # lapsed or under-scoped SCORECARD_TOKEN actually produces.
    forbidden) echo "gh: Resource not accessible by integration (HTTP 403)" >&2; exit 1 ;;
    # rc 0 with a JSON error object on STDOUT and STDERR EMPTY. Without
    # the shape guard this is reported as DRIFT rather than refused.
    body)      printf '{"message":"Not Found"}\n'; exit 0 ;;
    empty)     exit 0 ;;
    silent)    exit 7 ;;
    *)         v="$(cat "$GH_MODE.$field" 2>/dev/null)"
               [ -n "$v" ] || v="$(printf '%s' "$mode")"
               printf '%s\n' "$v"; exit 0 ;;
esac
STUBEOF
chmod +x "$STUB/gh"

failures=0
n=0

set_values() {
    printf 'ok' > "$GH_MODE"
    printf '%s' "${1:-all_external_contributors}" > "$GH_MODE.approval_policy"
    printf '%s' "${2:-read}"                     > "$GH_MODE.default_workflow_permissions"
    printf '%s' "${3:-false}"                    > "$GH_MODE.can_approve_pull_request_reviews"
}

# run NAME WANT_EXIT WANT_CALLS [SUBSTR...]
run() {
    local name="$1" want="$2" wantcalls="$3"; shift 3
    : > "$GH_CALLS"
    n=$((n + 1))
    PATH="$STUB:$PATH" REPO="owner/name" bash "$CHECK" > "$TMP/out" 2>&1
    local got=$? calls
    # `grep -c` PRINTS 0 AND EXITS 1 on no match, so `|| echo 0` appended
    # a SECOND zero and `calls` became two lines. `[ "0\n0" -ne 0 ]` is
    # not false -- it is an ERROR, exit 2, and inside an `||` chain that
    # reads exactly like the assertion holding. Every zero-call case
    # would print PASS with its call count unchecked. Nothing expected
    # zero calls until the population observer landed, so the defect
    # shipped unreachable. The guard below is kept because an assertion
    # that cannot be evaluated must not be indistinguishable from one
    # that passed.
    calls=$(grep -c . "$GH_CALLS" 2>/dev/null); calls=${calls:-0}
    case "$calls" in
        ''|*[!0-9]*)
            echo "FAIL: $name -- the call counter read '$calls', which is not a number."
            echo "    An assertion that cannot be evaluated is not an assertion that passed."
            failures=$((failures + 1)); return ;;
    esac
    if [ "$got" -ne "$want" ] || [ "$calls" -ne "$wantcalls" ]; then
        echo "FAIL: $name -- want exit $want / $wantcalls call(s), got $got / $calls"
        [ "$calls" -eq 0 ] && echo "    the stub was never invoked -- PATH did not take, so this case tested nothing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    local missing=""
    for s in "$@"; do grep -F -- "$s" "$TMP/out" >/dev/null || missing="$missing '$s'"; done
    if [ -n "$missing" ]; then
        echo "FAIL: $name -- exit and calls as wanted, output lacks:$missing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name (exit $got, $calls gh call(s))"
}

# --- the documented state ---------------------------------------------
set_values
run "all three as documented" 0 3 "all 3 settings are as documented"

# --- the enumeration this gate's argument rests on ---------------------
# The header names the workflows that expose the pool to pull requests.
# Until the observer landed, that sentence was checked by nobody -- and
# it was WRONG, claiming five where the tree has two. These cases drive
# the watcher that now stands behind it.
#
# Every case here expects ZERO gh calls: the population is judged before
# any setting is queried, because a header whose premise has moved
# misinforms whatever the settings say.
mkwf() { # DIR -- the two really-exposed workflows
    mkdir -p "$1"
    printf 'name: coverage\non:\n  pull_request:\njobs:\n  c:\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$1/coverage.yml"
    printf 'name: integration\non:\n  pull_request:\njobs:\n  i:\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$1/integration.yml"
}

# runwf NAME WANT_EXIT WANT_CALLS DIR [SUBSTR...]
runwf() {
    local name="$1" want="$2" wantcalls="$3" dir="$4"; shift 4
    : > "$GH_CALLS"
    n=$((n + 1))
    PATH="$STUB:$PATH" REPO="owner/name" WF_DIR="$dir" bash "$CHECK" > "$TMP/out" 2>&1
    local got=$? calls
    # `grep -c` PRINTS 0 AND EXITS 1 on no match, so `|| echo 0` appended
    # a SECOND zero and `calls` became two lines. `[ "0\n0" -ne 0 ]` is
    # not false -- it is an ERROR, exit 2, and inside an `||` chain that
    # reads exactly like the assertion holding. Every zero-call case
    # would print PASS with its call count unchecked. Nothing expected
    # zero calls until the population observer landed, so the defect
    # shipped unreachable. The guard below is kept because an assertion
    # that cannot be evaluated must not be indistinguishable from one
    # that passed.
    calls=$(grep -c . "$GH_CALLS" 2>/dev/null); calls=${calls:-0}
    case "$calls" in
        ''|*[!0-9]*)
            echo "FAIL: $name -- the call counter read '$calls', which is not a number."
            echo "    An assertion that cannot be evaluated is not an assertion that passed."
            failures=$((failures + 1)); return ;;
    esac
    if [ "$got" -ne "$want" ] || [ "$calls" -ne "$wantcalls" ]; then
        echo "FAIL: $name -- want exit $want / $wantcalls call(s), got $got / $calls"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    local missing=""
    for s in "$@"; do grep -F -- "$s" "$TMP/out" >/dev/null || missing="$missing '$s'"; done
    if [ -n "$missing" ]; then
        echo "FAIL: $name -- exit and calls as wanted, output lacks:$missing"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name (exit $got, $calls gh call(s))"
}

set_values

WFOK="$TMP/wf-ok"; mkwf "$WFOK"
runwf "the declared population is the derived one" 0 3 "$WFOK"

# The failure that was invisible: a THIRD workflow gains a self-hosted
# job on pull_request and the header keeps claiming two.
WF3="$TMP/wf-three"; mkwf "$WF3"
printf 'name: newlane\non:\n  pull_request:\njobs:\n  n:\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$WF3/newlane.yml"
runwf "a third exposed workflow refuses before any setting is read" 2 0 "$WF3" \
    "newlane.yml" "has CHANGED"

# And the other direction: the exposure disappearing entirely. That is
# this gate's reason evaporating, which must be said out loud rather
# than passing quietly.
WF0="$TMP/wf-none"; mkdir -p "$WF0"
printf 'name: hosted\non:\n  pull_request:\njobs:\n  h:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' > "$WF0/hosted.yml"
runwf "zero exposed workflows refuses rather than passing" 2 0 "$WF0" "ZERO workflows"

# AND AN EMPTY DIRECTORY IS NOT "ZERO EXPOSED WORKFLOWS" EITHER. The two
# read the same in the verdict and mean opposite things: one is an answer
# about the tree, the other is the scan having no input at all. Every
# gate in this repository that reported success over an empty input set
# was hiding something by the time anyone looked.
WFEMPTY="$TMP/wf-empty"; mkdir -p "$WFEMPTY"
runwf "an empty workflow directory refuses rather than deriving zero" 2 0 "$WFEMPTY" \
    "no *.yml or *.yaml files"

# --- the two false positives that produced the wrong number ------------
# A `pull_request` trigger with every job hosted is NOT exposure. This
# is exactly what coverage-presence, release-backmerge and test are, and
# counting them is how the header came to say five.
WFH="$TMP/wf-hosted"; mkwf "$WFH"
printf 'name: hostedonly\non:\n  pull_request:\njobs:\n  h:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' > "$WFH/hostedonly.yml"
runwf "a pull_request workflow with only hosted jobs is not exposure" 0 3 "$WFH"

# A self-hosted job in a workflow that merely MENTIONS pull_request --
# in an `if:` expression, or in prose -- does not trigger on it. A
# file-wide grep counts it; reading the `on:` block does not.
WFM="$TMP/wf-mention"; mkwf "$WFM"
printf 'name: mentions\n# guarded on github.event.pull_request.head.repo.full_name\non:\n  push:\n    branches: [main]\njobs:\n  m:\n    if: github.event.pull_request.head.repo.full_name == github.repository\n    runs-on: [self-hosted, dhcp-ci]\n    steps: [{run: "true"}]\n' > "$WFM/mentions.yml"
runwf "mentioning pull_request in an if: is not a pull_request trigger" 0 3 "$WFM"

# --- THE SEVEN SHAPES THE LINE SCANNER COULD NOT SEE (#844) -------------
# The first version of the observer matched `/runs-on:.*self-hosted/` and
# `pull_request` as line regexes, while this file, the workflow and the
# pull request all claimed a new self-hosted job on `pull_request`
# "cannot be added silently". MEASURED at f1aceb8: seven legal spellings
# were invisible and every miss was permissive. Each is a case here, and
# each is written as a THIRD workflow arriving beside the two declared --
# so a shape that is not seen does not merely go unreported, it produces
# a silent PASS over a changed population, which is the failure exactly.
#
# `wflane DIR CONTENT` writes the newcomer beside the declared two.
wflane() { local d="$1"; shift; mkwf "$d"; printf '%s' "$*" > "$d/newlane.yml"; }

WFA="$TMP/wf-seq"; wflane "$WFA" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on:
      - self-hosted
      - dhcp-ci
    steps: [{run: "true"}]
'
runwf "a block-sequence runs-on is a self-hosted job" 2 0 "$WFA" "newlane.yml" "has CHANGED"

WFB="$TMP/wf-label"; wflane "$WFB" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on: [dhcp-ci]
    steps: [{run: "true"}]
'
runwf "the custom label alone routes to the pool" 2 0 "$WFB" "newlane.yml" "has CHANGED"

WFC="$TMP/wf-matrix"; wflane "$WFC" 'name: newlane
on:
  pull_request:
jobs:
  n:
    strategy:
      matrix:
        include:
          - runner: ubuntu-latest
          - runner: self-hosted
    runs-on: ${{ matrix.runner }}
    steps: [{run: "true"}]
'
runwf "a matrix expression is resolved, not skipped" 2 0 "$WFC" "newlane.yml" "has CHANGED"

# And a matrix entry whose value is itself a LABEL LIST, which is how a
# self-hosted runner is actually addressed.
WFC2="$TMP/wf-matrixlist"; wflane "$WFC2" 'name: newlane
on:
  pull_request:
jobs:
  n:
    strategy:
      matrix:
        include:
          - runner: [self-hosted, dhcp-ci]
    runs-on: ${{ matrix.runner }}
    steps: [{run: "true"}]
'
runwf "a matrix value that is a label list is resolved too" 2 0 "$WFC2" \
    "newlane.yml" "has CHANGED"

WFD="$TMP/wf-group"; wflane "$WFD" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on:
      group: private-pool
    steps: [{run: "true"}]
'
runwf "a runner group is not a hosted image" 2 0 "$WFD" "newlane.yml" "has CHANGED"

WFE="$TMP/wf-flowon"; wflane "$WFE" 'name: newlane
on: {pull_request: {branches: [dev]}}
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a flow-mapping on: still declares the trigger" 2 0 "$WFE" "newlane.yml" "has CHANGED"

WFF="$TMP/wf-quoted"; wflane "$WFF" 'name: newlane
on:
  "pull_request":
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a quoted trigger key still declares the trigger" 2 0 "$WFF" "newlane.yml" "has CHANGED"

# The remaining two spellings GitHub accepts for `on:` itself. Both are
# the same class as the flow mapping above -- a document the line scan
# read one shape of -- and neither costs more than the fixture.
WFQ="$TMP/wf-flowseq"; wflane "$WFQ" 'name: newlane
on: [push, pull_request]
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a flow-sequence on: still declares the trigger" 2 0 "$WFQ" "newlane.yml" "has CHANGED"

WFP="$TMP/wf-scalaron"; wflane "$WFP" 'name: newlane
on: pull_request
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a scalar on: still declares the trigger" 2 0 "$WFP" "newlane.yml" "has CHANGED"

WFG="$TMP/wf-secondjob"; wflane "$WFG" 'name: newlane
on:
  pull_request:
jobs:
  first:
    runs-on: ubuntu-latest
    steps: [{run: "true"}]
  second:
    runs-on:
      - self-hosted
      - dhcp-ci
    steps: [{run: "true"}]
'
runwf "the SECOND job is read too" 2 0 "$WFG" "newlane.yml" "has CHANGED"

# --- THE TRIGGER DOMAIN IS "AN OUTSIDER CAN CAUSE IT", NOT THE WORD ----
# `pull_request_target` is the one that matters most and the one the
# first version explicitly excluded: it runs with repository credentials
# and can be pointed at the fork's head ref, which is the approval policy
# this gate watches being made irrelevant rather than relaxed.
WFT="$TMP/wf-prtarget"; wflane "$WFT" 'name: newlane
on:
  pull_request_target:
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps:
      - uses: actions/checkout@v5
        with:
          ref: ${{ github.event.pull_request.head.sha }}
'
runwf "pull_request_target on the pool is exposure" 2 0 "$WFT" "newlane.yml" "has CHANGED"

WFR="$TMP/wf-wfrun"; wflane "$WFR" 'name: newlane
on:
  workflow_run:
    workflows: [Test]
    types: [completed]
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "workflow_run on the pool is exposure" 2 0 "$WFR" "newlane.yml" "has CHANGED"

WFI="$TMP/wf-issuecomment"; wflane "$WFI" 'name: newlane
on:
  issue_comment:
    types: [created]
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "issue_comment on the pool is exposure" 2 0 "$WFI" "newlane.yml" "has CHANGED"

# THE RESIDUE. The four triggers above are the ones anybody would think
# to enumerate, and enumerating them is the defect this whole change is
# about. The scan names the SAFE triggers instead, so a trigger it has
# never heard of -- a future GitHub event, a typo, anything outside the
# safe list -- counts and refuses. Without the inversion this case is the
# fifth trigger nobody wrote down, and it passes silently.
WFN="$TMP/wf-newtrigger"; wflane "$WFN" 'name: newlane
on:
  some_future_event:
    types: [created]
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a trigger this gate has never heard of counts, not vanishes" 2 0 "$WFN" \
    "newlane.yml" "has CHANGED"

# THE FACT THAT THERE ARE NONE OF THOSE TODAY IS PINNED, NOT ASSUMED.
# MEASURED at b9b31bb: no workflow_run and no issue_comment workflow
# exists, and the one `pull_request_target` workflow --
# issue-state-labels.yml -- is on `ubuntu-latest` and pins `ref: dev`.
# That is a fact about the tree, so it is asserted against the REAL
# workflow directory. An enumeration beside the code is an unrun
# checklist; this case runs it.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
runwf "the real workflow tree derives exactly the declared two" 0 3 \
    "$ROOT/.github/workflows" "all 3 settings are as documented"

# AND THE TRIGGER CENSUS ITSELF IS PINNED, for the reason the population
# is: the checker's header used to state it in prose and the prose was
# FALSE at the SHA it named. It said the outsider-causable triggers in
# this tree are `pull_request` and `pull_request_target`; there is a
# third, `issues`, on issue-labeler.yml, which anyone with a GitHub
# account can cause. It survived because nothing re-derived it -- an
# enumeration beside the code is an unrun checklist, in the one file
# whose whole subject is that enumerations rot.
#
# THE SET IS ASSERTED, NOT THE COUNT. The gate prints both; this pins the
# distinct triggers, so a fourteenth `pull_request` workflow costs
# nothing and a first `workflow_run` or `issues` one goes red here. The
# named line beneath it is the specific miss, so a regression says which
# fact moved rather than only that something did.
runwf "the outsider-reachable trigger census is derived, not written down" 0 3 \
    "$ROOT/.github/workflows" \
    "can cause: [issues, pull_request, pull_request_target]" \
    "issue-labeler.yml: issues" \
    "issue-state-labels.yml: pull_request_target"

# --- FAIL CLOSED WHERE IT CANNOT TELL ----------------------------------
# "I could not resolve this runner" must send a human to look. Both of
# these are permissive-by-default in any scanner that only looks for a
# known-bad string.
WFU="$TMP/wf-unresolvable"; wflane "$WFU" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on: ${{ env.RUNNER }}
    steps: [{run: "true"}]
'
runwf "an unresolvable runs-on expression counts as exposure" 2 0 "$WFU" \
    "newlane.yml" "has CHANGED"

WFJ="$TMP/wf-uses"; wflane "$WFJ" 'name: newlane
on:
  pull_request:
jobs:
  n:
    uses: ./.github/workflows/other.yml
'
runwf "a job that delegates to a reusable workflow counts as exposure" 2 0 "$WFJ" \
    "newlane.yml" "has CHANGED"

# --- AND HOSTED STAYS HOSTED, or the gate refuses on every push --------
# A widening needs a preservation control: the cases above prove the new
# shapes are SEEN, and these prove the widening did not swallow the
# ordinary hosted job, which is most of this tree.
WFH2="$TMP/wf-hosted-arm"; wflane "$WFH2" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on: ubuntu-24.04-arm
    steps: [{run: "true"}]
'
runwf "a hosted arm64 image is not the private pool" 0 3 "$WFH2"

WFH3="$TMP/wf-hosted-list"; wflane "$WFH3" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on: [ubuntu-latest]
    steps: [{run: "true"}]
'
runwf "a single-element hosted list is not the private pool" 0 3 "$WFH3"

# THE BOUNDARY BETWEEN THOSE TWO GROUPS HAD NO FIXTURE ON IT. Every case
# above is all-pool or all-hosted, and `all(HOSTED.match(l))` and
# `any(HOSTED.match(l))` agree on both of those -- so the mutant that
# swaps them survived all 41 cases. It is not a no-op: on the mixed set
# below the gate refuses and the mutant passes silently, which is a
# self-hosted job derived as hosted. The header argues this exact rule at
# "not 'contains self-hosted'"; nothing drove it until now. Both orders,
# because a first-element-only reading is the other easy wrong answer.
WFMX="$TMP/wf-mixed"; wflane "$WFMX" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on: [dhcp-ci, ubuntu-22.04]
    steps: [{run: "true"}]
'
runwf "one pool label among hosted ones is still the pool" 2 0 "$WFMX" \
    "newlane.yml" "has CHANGED"

WFMX2="$TMP/wf-mixed-rev"; wflane "$WFMX2" 'name: newlane
on:
  pull_request:
jobs:
  n:
    runs-on: [ubuntu-22.04, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a pool label after a hosted one is still the pool" 2 0 "$WFMX2" \
    "newlane.yml" "has CHANGED"

WFH4="$TMP/wf-hosted-matrix"; wflane "$WFH4" 'name: newlane
on:
  pull_request:
jobs:
  n:
    strategy:
      matrix:
        include:
          - runner: ubuntu-latest
          - runner: ubuntu-24.04-arm
    runs-on: ${{ matrix.runner }}
    steps: [{run: "true"}]
'
runwf "a matrix of hosted images is not the private pool" 0 3 "$WFH4"

WFS="$TMP/wf-selfhosted-nopr"; wflane "$WFS" 'name: newlane
on:
  workflow_dispatch:
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a self-hosted job no outsider can trigger is not exposure" 0 3 "$WFS"

# --- THE `on:`-SCOPING, WITH AN INPUT THAT ACTUALLY DISTINGUISHES IT ----
# The case above ("mentioning pull_request in an if:") does NOT test the
# scoping: its fixture writes `github.event.pull_request.head...`, where
# the word is followed by a dot and was already excluded by the line
# regex's own boundary. MEASURED at f1aceb8: deleting the `in_on &&`
# scoping left the suite 14/14 GREEN.
#
# These two do distinguish it. A push-only workflow with a self-hosted
# job whose COMMENT ends on the bare word -- once inside the `on:` block,
# once outside it -- derives as exposure the moment the scan reads
# anything other than the resolved `on:` mapping. MEASURED at f1aceb8
# with `in_on &&` removed: exit 2 instead of 0.
WFC1="$TMP/wf-comment-in-on"; wflane "$WFC1" 'name: newlane
on:
  # deliberately not run on pull_request
  push:
    branches: [main]
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a comment inside on: ending on the bare word is not a trigger" 0 3 "$WFC1"

WFC2="$TMP/wf-comment-out"; wflane "$WFC2" '# deliberately not run on pull_request
name: newlane
on:
  push:
    branches: [main]
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a comment above on: ending on the bare word is not a trigger" 0 3 "$WFC2"

# --- A WORKFLOW THIS GATE CANNOT READ IS NOT A WORKFLOW WITHOUT JOBS ----
# Drive the absence: take the parser's input away and the gate must go
# red naming the file, never derive a smaller population and pass. An
# incomplete population is not a smaller one, it is a wrong one.
WFX="$TMP/wf-broken"; wflane "$WFX" 'name: newlane
on:
  pull_request:
jobs:
  n:
   runs-on: [self-hosted
    steps: [{run: "true"}]
'
runwf "an unparseable workflow refuses rather than deriving fewer" 2 0 "$WFX" \
    "newlane.yml" "could not read every workflow"

# And the last place "I could not tell" could have become "nothing to
# report": a workflow that parses, triggers on something an outsider can
# cause, and whose jobs are not a mapping this gate can walk.
WFY="$TMP/wf-nojobs"; wflane "$WFY" 'name: newlane
on:
  pull_request:
'
runwf "an outsider-reachable workflow with no readable jobs refuses" 2 0 "$WFY" \
    "newlane.yml" "no readable 'jobs:' mapping"

# AND THE MIRROR IMAGE OF IT, which was asymmetric until review caught
# it: a workflow whose `on:` cannot be read used to be SKIPPED, while
# the identical shape one level down -- unreadable `jobs:` -- refused.
# Same argument, opposite verdict, and the skipping one is the permissive
# half. Both of these place a job squarely on the pool, so a skip here
# hides a self-hosted job from the population.
WFZ="$TMP/wf-null-on"; wflane "$WFZ" 'name: newlane
on:
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a workflow whose on: is null refuses rather than being skipped" 2 0 "$WFZ" \
    "newlane.yml" "no readable 'on:' trigger block"

WFZ2="$TMP/wf-empty-on"; wflane "$WFZ2" 'name: newlane
on: []
jobs:
  n:
    runs-on: [self-hosted, dhcp-ci]
    steps: [{run: "true"}]
'
runwf "a workflow whose on: names no trigger refuses too" 2 0 "$WFZ2" \
    "newlane.yml" "no readable 'on:' trigger block"

# --- THE PARSER GOING MISSING IS A REFUSAL THAT NAMES IT ---------------
# This gate has three verdicts and no fallback: the line scan it replaced
# read one spelling out of seven, so silently degrading to it would be
# worse than saying nothing. The exit-3 branch is an error path, and an
# error path no test executes is #827 exactly, so it is driven here with
# python3 stubbed to the shape a missing PyYAML produces.
NOPY="$TMP/nopy"; mkdir -p "$NOPY"
cat > "$NOPY/python3" <<'PYEOF'
#!/usr/bin/env bash
cat > /dev/null
# THE STUB MUST NOT SPEAK THE GATE'S WORDS. This printed the same
# sentence the gate prints, and the assertion below then matched the
# STUB's stderr -- which the suite captures with 2>&1 -- so deleting the
# gate's exit-3 branch entirely left this case GREEN. Measured: mutant
# M8 SURVIVED until this line was changed. The discriminator has to be
# text only the gate can produce.
echo "ModuleNotFoundError: No module named 'yaml'" >&2
exit 3
PYEOF
chmod +x "$NOPY/python3"
: > "$GH_CALLS"
n=$((n + 1))
PATH="$NOPY:$STUB:$PATH" REPO="owner/name" WF_DIR="$WFOK" bash "$CHECK" > "$TMP/out" 2>&1
nopy_rc=$?
nopy_calls=$(grep -c . "$GH_CALLS" 2>/dev/null); nopy_calls=${nopy_calls:-0}
if [ "$nopy_rc" -ne 2 ] || [ "$nopy_calls" -ne 0 ] \
   || ! grep -F -- "does not fall back to a line scan" "$TMP/out" >/dev/null; then
    echo "FAIL: a missing PyYAML refuses and names it -- want exit 2 / 0 call(s), got $nopy_rc / $nopy_calls"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
else
    echo "PASS: a missing PyYAML refuses and names it (exit $nopy_rc, $nopy_calls gh call(s))"
fi

set_values

# --- each setting drifts on its own, and the message names the stakes --
set_values "first_time_contributors"
run "the approval policy is relaxed" 1 3 \
    "approval_policy is 'first_time_contributors'" "runs as root" "1 of 3 settings"

set_values "" "write"
run "the default token becomes writable" 1 3 \
    "default_workflow_permissions is 'write'" "push access"

set_values "" "" "true"
run "Actions may approve pull requests" 1 3 \
    "can_approve_pull_request_reviews is 'true'" "branch protection"

set_values "none" "write" "true"
run "all three drift at once" 1 3 "3 of 3 settings"

# --- CANNOT JUDGE is not a pass and not a failure ---------------------
# A token that cannot read administration settings is the likely
# real-world fault: SCORECARD_TOKEN is a PAT and PATs expire.
printf 'forbidden' > "$GH_MODE"
run "a token without Administration: read refuses" 2 1 \
    "could not read approval_policy" "Nothing was measured"

# THE CASE THE SHAPE GUARD EXISTS FOR. `gh api --jq` prints a 4xx body
# on stdout with stderr empty. Reported as drift, this sends the reader
# to the settings page to fix a setting that was never read.
printf 'body' > "$GH_MODE"
run "a JSON error body refuses rather than reporting drift" 2 1 \
    "not one of the values this setting can hold" "Nothing was measured"

printf 'empty' > "$GH_MODE"
run "an empty answer refuses" 2 1 "could not read approval_policy"

printf 'silent' > "$GH_MODE"
run "gh failing silently refuses and reports rc" 2 1 "printed nothing on either stream"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
