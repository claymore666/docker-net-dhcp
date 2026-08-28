#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The self-hosted pool's size and per-run job count, checked instead of
# repeated (#879).
#
# WHY THIS EXISTS
#
# Both numbers were stated in prose in six tracked files and checked by
# nothing. Every one of them was wrong: the pool had been resized 8 -> 16
# and the suite matrix widened 2 -> 4 jobs, and each site went on saying
# what was true when it was written. Two of the six are not comments —
# they are the text scripts/ci-queue-watchdog.sh prints to an operator
# DURING a capacity incident, on the STARVATION and POOL SHORT paths. A
# diagnostic that misstates both operands is worse than a stale comment.
#
# Correcting six numbers by hand leaves the class open. This gate closes
# it in the only direction that stays closed: every site that states one
# of these facts is bound to a single canonical value, and the canonical
# value for the derivable half is DERIVED FROM THE WORKFLOW ITSELF rather
# than written down anywhere.
#
# THE TWO FACTS
#
#   jobs-per-run   DERIVED. A real YAML parse of the workflow named by
#                  DHCP_CI_JOBS_PER_RUN_WORKFLOW: the jobs whose
#                  `runs-on` carries the pool label, matrix-expanded.
#                  Nothing declares it, so nothing can declare it wrong.
#
#   pool-size      DECLARED, in .github/ci-pool-facts.env, with the date
#                  it was measured.
#
# WHY pool-size IS NOT ALSO DERIVED, AND THE MEASUREMENT THAT DECIDED IT
#
# Listing a repository's self-hosted runners is `GET /repos/{o}/{r}/
# actions/runners`, which needs repo `administration` rights. A workflow
# GITHUB_TOKEN cannot be granted that AT ALL — `administration` is not
# one of its permission scopes. MEASURED 2026-08-28: actionlint rejects
# `permissions: administration: read` and enumerates the complete set it
# accepts (actions, artifact-metadata, attestations, checks, contents,
# deployments, discussions, id-token, issues, models, packages, pages,
# pull-requests, repository-projects, security-events, statuses); there
# is no member that grants it. scripts/ci-queue-watchdog.sh reached the
# same conclusion independently and asks about competing RUNS for the
# same reason.
#
# So option 1 of #879 — query the runners API from the gate — is not
# available to anything running on the workflow token, and this gate
# takes option 2. That is the WEAKER half and the boundary is stated
# rather than implied: the declared number can still disagree with the
# live pool. What it can no longer do is disagree with the prose that
# quotes it, in either direction.
#
# HOW A SITE IS CHECKED — three-way, so no single edit satisfies it
#
# A prose site opts in with a marker on the line that states the number:
#
#     # ... the pool is 16 runners ...   <!-- pool-facts: NAME=VALUE -->
#
# and the gate requires all three of:
#
#   1. NAME is a fact this gate knows            (else: refusal)
#   2. VALUE equals the canonical value          (else: red — stale)
#   3. the prose on that line, with the marker removed, contains VALUE
#      as a standalone number                    (else: red — the marker
#                                                 was bumped and the
#                                                 sentence was not)
#
# (3) is what stops the marker becoming a second copy that drifts from
# the sentence beside it, and it forces the digit: "a pool of eight"
# cannot satisfy it, which is how the spelling problem is closed by
# construction rather than by enumerating spellings.
#
# WHAT IS NORMAL AND WHAT IS NOT
#
# The NORMAL outcome is exit 0 with a one-line OK naming both values and
# the number of sites checked. Exit 1 is a FINDING: a real site
# disagrees. Exit 2 is a REFUSAL: nothing was measured, and it must never
# be read as routine — every refusal says so in its own message.
#
# THE BOUNDARY, stated because a gate described as "checking the pool
# facts" invites the belief that it reads all prose. It does not. Its
# domain is the marked lines in the tracked tree, so an UNMARKED sentence
# stating the pool size is invisible to it. Two things keep that from
# being an emptying hatch: the gate REFUSES when any declared fact has
# zero marked sites, so the domain cannot be emptied silently, and the
# watchdog's operator text carries no literal at all — it derives from
# this gate at runtime, so there is nothing there left to go stale.
#
# Usage: check-pool-facts.sh [--facts]
#          --facts  print `pool-size=N` and `jobs-per-run=N` and exit;
#                   the seam scripts/ci-queue-watchdog.sh reads so its
#                   operator guidance carries no literal of its own.
# Env:   POOL_FACTS_ROOT  repository to inspect (default: the repo this
#                         script lives in) — the seam the self-test drives.
#        POOL_FACTS_FILE  canonical facts file, relative to the root
#                         (default: .github/ci-pool-facts.env).
# Exit:  0 every marked site agrees, 1 a site disagrees, 2 cannot check.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${POOL_FACTS_ROOT:-$(cd "$HERE/.." && pwd)}"
FACTS_REL="${POOL_FACTS_FILE:-.github/ci-pool-facts.env}"
FACTS="$ROOT/$FACTS_REL"

MODE="check"
case "${1:-}" in
    --facts) MODE="facts" ;;
    "") ;;
    *) echo "check-pool-facts: unknown argument '$1'" >&2; exit 2 ;;
esac

# refuse: every exit-2 path goes through here, so "nothing was measured"
# is said out loud every time rather than inferred from a quiet log.
refuse() {
    echo "::error title=Cannot check pool facts::$*" >&2
    echo "check-pool-facts: REFUSED — nothing was measured. This is not a pass." >&2
    exit 2
}

git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || refuse "$ROOT is not a git work tree, and this gate enumerates its domain through the git index. It cannot see anything here."

[ -f "$FACTS" ] \
    || refuse "the canonical facts file $FACTS_REL does not exist under $ROOT. Every number this gate checks is bound to that file, so with it gone there is nothing to check against."
[ -r "$FACTS" ] \
    || refuse "the canonical facts file $FACTS_REL exists but cannot be read."

# Parsed line by line rather than sourced: this file is data, and
# `source`-ing it would execute whatever a future edit puts in it.
fact_value() {
    sed -n "s/^[[:space:]]*$1=\\(.*\\)\$/\\1/p" "$FACTS" | tail -1
}

POOL_SIZE=$(fact_value DHCP_CI_POOL_SIZE)
POOL_MEASURED=$(fact_value DHCP_CI_POOL_SIZE_MEASURED)
POOL_LABEL=$(fact_value DHCP_CI_POOL_LABEL)
JOBS_WF=$(fact_value DHCP_CI_JOBS_PER_RUN_WORKFLOW)

case "$POOL_SIZE" in
    "" ) refuse "$FACTS_REL declares no DHCP_CI_POOL_SIZE. An absent number is not a zero and must not become one." ;;
    *[!0-9]* ) refuse "DHCP_CI_POOL_SIZE in $FACTS_REL is '$POOL_SIZE', which is not a decimal integer." ;;
esac
[ "$POOL_SIZE" -gt 0 ] 2>/dev/null \
    || refuse "DHCP_CI_POOL_SIZE in $FACTS_REL is '$POOL_SIZE'. A pool of zero runners is not a measurement, it is a parse that failed."
[ -n "$POOL_MEASURED" ] \
    || refuse "$FACTS_REL declares DHCP_CI_POOL_SIZE without DHCP_CI_POOL_SIZE_MEASURED. This number cannot be derived from the tree, so it is only worth what its date says it is worth; an undated one is a guess."
[ -n "$POOL_LABEL" ] \
    || refuse "$FACTS_REL declares no DHCP_CI_POOL_LABEL, so the jobs-per-run derivation has nothing to match runs-on against."
[ -n "$JOBS_WF" ] \
    || refuse "$FACTS_REL declares no DHCP_CI_JOBS_PER_RUN_WORKFLOW, so there is no workflow to derive the per-run job count from."
[ -f "$ROOT/$JOBS_WF" ] \
    || refuse "$FACTS_REL names $JOBS_WF as the workflow the jobs-per-run fact describes, and no such file exists under $ROOT."

command -v python3 >/dev/null 2>&1 \
    || refuse "python3 is required to parse the workflow the jobs-per-run count is derived from, and it is not on PATH. A line scan is not a substitute: the count comes out of matrix semantics, not out of a spelling."

# --- derive jobs-per-run ----------------------------------------------
#
# A real parse, deliberately. The count is the matrix expansion of every
# job in the workflow whose `runs-on` carries the pool label, and a grep
# cannot see either half of that.
#
# It REFUSES on any matrix shape it does not model rather than guessing —
# an approximate job count is exactly the kind of number this gate exists
# to stop people trusting.
JOBS_OUT=$(python3 - "$ROOT/$JOBS_WF" "$POOL_LABEL" <<'PARSE'
import sys

try:
    import yaml
except Exception:
    print("ERROR\tPyYAML is not importable by this python3, so the workflow cannot be parsed")
    sys.exit(0)

path, label = sys.argv[1], sys.argv[2]

try:
    with open(path, "r", encoding="utf-8") as fh:
        doc = yaml.safe_load(fh)
except Exception as exc:
    print("ERROR\t%s could not be parsed as YAML: %s" % (path, exc))
    sys.exit(0)

if not isinstance(doc, dict):
    print("ERROR\t%s did not parse to a mapping" % path)
    sys.exit(0)

jobs = doc.get("jobs")
if not isinstance(jobs, dict) or not jobs:
    print("ERROR\t%s declares no jobs" % path)
    sys.exit(0)


def runs_on_labels(spec):
    """Every label of a runs-on, in each of the shapes GitHub accepts."""
    if isinstance(spec, str):
        return [spec]
    if isinstance(spec, list):
        return [x for x in spec if isinstance(x, str)]
    if isinstance(spec, dict):
        # runs-on: {group: .., labels: [..]}
        got = spec.get("labels")
        if isinstance(got, str):
            return [got]
        if isinstance(got, list):
            return [x for x in got if isinstance(x, str)]
        return []
    return []


def matrix_count(job, name):
    """How many jobs one job definition expands to. None + reason if unmodelled."""
    strategy = job.get("strategy")
    if strategy is None:
        return 1, None
    if not isinstance(strategy, dict):
        return None, "job '%s' has a non-mapping strategy" % name
    matrix = strategy.get("matrix")
    if matrix is None:
        return 1, None
    if isinstance(matrix, str):
        return None, ("job '%s' builds its matrix from an expression (%r), so the "
                      "per-run job count is not knowable from the file" % (name, matrix))
    if not isinstance(matrix, dict):
        return None, "job '%s' has a matrix that is neither a mapping nor an expression" % name
    if "exclude" in matrix:
        return None, ("job '%s' uses matrix exclude:, which this gate does not model. "
                      "Teach it the semantics rather than letting it guess a count." % name)

    include = matrix.get("include")
    dims = {k: v for k, v in matrix.items() if k not in ("include", "exclude")}

    if dims and include is not None:
        return None, ("job '%s' combines matrix dimensions with include:, whose expansion "
                      "depends on which include entries extend an existing combination and "
                      "which add a new one. This gate refuses rather than approximating." % name)

    if include is not None:
        if not isinstance(include, list) or not include:
            return None, "job '%s' has an include: that is not a non-empty list" % name
        return len(include), None

    if not dims:
        return None, "job '%s' declares an empty matrix" % name

    total = 1
    for key, values in dims.items():
        if not isinstance(values, list) or not values:
            return None, ("job '%s' matrix dimension '%s' is not a non-empty list, so it "
                          "cannot be expanded" % (name, key))
        total *= len(values)
    return total, None


total = 0
matched = []
for name, job in jobs.items():
    if not isinstance(job, dict):
        continue
    if label not in runs_on_labels(job.get("runs-on")):
        continue
    count, why = matrix_count(job, name)
    if count is None:
        print("ERROR\t%s" % why)
        sys.exit(0)
    matched.append("%s=%d" % (name, count))
    total += count

if not matched:
    print("ERROR\tno job in %s targets the '%s' label. Either the pool label moved or the "
          "scan stopped matching; both mean this gate's domain is empty, which is the one "
          "state a universal check must never report as a pass." % (path, label))
    sys.exit(0)

if total <= 0:
    print("ERROR\tderived a per-run job count of %d, which cannot be right" % total)
    sys.exit(0)

print("OK\t%d\t%s" % (total, ",".join(matched)))
PARSE
)
python_rc=$?
[ "$python_rc" -eq 0 ] || refuse "the workflow parser exited $python_rc without producing a verdict."

case "$JOBS_OUT" in
    "OK	"*) ;;
    "ERROR	"*) refuse "${JOBS_OUT#ERROR	}" ;;
    *) refuse "the workflow parser produced no usable output. An empty parse must not become a zero." ;;
esac

JOBS_PER_RUN=$(printf '%s' "$JOBS_OUT" | cut -f2)
JOBS_BREAKDOWN=$(printf '%s' "$JOBS_OUT" | cut -f3)

case "$JOBS_PER_RUN" in
    ""|*[!0-9]*) refuse "the derived per-run job count is '$JOBS_PER_RUN', which is not a decimal integer." ;;
esac

if [ "$MODE" = "facts" ]; then
    echo "pool-size=$POOL_SIZE"
    echo "jobs-per-run=$JOBS_PER_RUN"
    exit 0
fi

# --- sweep the tracked tree -------------------------------------------
#
# The domain is `git ls-files` — the WHOLE tracked tree, not a list of
# files someone remembered. Any tracked text file may carry a marker.
FILE_LIST=$(mktemp) || refuse "could not create a temporary file."
trap 'rm -f "$FILE_LIST"' EXIT
git -C "$ROOT" ls-files -z > "$FILE_LIST" \
    || refuse "git ls-files failed in $ROOT, so the domain could not be enumerated."
[ -s "$FILE_LIST" ] \
    || refuse "git ls-files returned nothing in $ROOT. A gate whose domain is empty reports success having checked nothing."

SWEEP=$(python3 - "$ROOT" "$FILE_LIST" "$POOL_SIZE" "$JOBS_PER_RUN" <<'SWEEP'
import re
import sys

root, listing, pool_size, jobs_per_run = sys.argv[1:5]

CANONICAL = {"pool-size": pool_size, "jobs-per-run": jobs_per_run}

# The marker. Assembled from parts so that this line -- and the same line
# in the self-test -- is not itself a site the sweep then tries to check.
MARKER = re.compile(r"pool" + r"-facts:\s*([A-Za-z][A-Za-z0-9-]*)=([0-9]+)")

with open(listing, "rb") as fh:
    names = [n for n in fh.read().split(b"\0") if n]

findings = []
sites = {name: 0 for name in CANONICAL}
checked = 0

for raw in names:
    name = raw.decode("utf-8", "surrogateescape")
    try:
        with open(root + "/" + name, "rb") as fh:
            blob = fh.read()
    except OSError:
        # A tracked path that is not a readable regular file (a submodule,
        # a broken symlink) carries no marker and is not a finding.
        continue
    if b"\0" in blob:
        continue
    try:
        text = blob.decode("utf-8")
    except UnicodeDecodeError:
        continue
    for lineno, line in enumerate(text.splitlines(), 1):
        hits = list(MARKER.finditer(line))
        if not hits:
            continue
        prose = MARKER.sub("", line)
        for hit in hits:
            fact, value = hit.group(1), hit.group(2)
            checked += 1
            if fact not in CANONICAL:
                findings.append(
                    "REFUSE\t%s:%d\tmarks an unknown fact '%s'. A typo here would silently "
                    "drop the site out of this gate's domain, so it is refused rather than "
                    "ignored." % (name, lineno, fact))
                continue
            sites[fact] += 1
            want = CANONICAL[fact]
            if value != want:
                findings.append(
                    "FAIL\t%s:%d\tstates %s=%s; the canonical value is %s."
                    % (name, lineno, fact, value, want))
                continue
            if not re.search(r"(?<![0-9])" + re.escape(value) + r"(?![0-9])", prose):
                findings.append(
                    "FAIL\t%s:%d\tmarks %s=%s, but the sentence on that line does not state "
                    "%s. The marker was updated and the prose was not -- which is the exact "
                    "decay this gate exists to catch, one layer in."
                    % (name, lineno, fact, value, value))

for fact, count in sorted(sites.items()):
    if count == 0:
        findings.append(
            "REFUSE\t-\tthe fact '%s' has ZERO marked sites in the tracked tree. Either every "
            "site was deleted or the marker spelling drifted; both empty this gate's domain, "
            "and an empty domain is the one result a universal check must never report as a "
            "pass." % fact)

print("CHECKED\t%d" % checked)
for f in findings:
    print(f)
SWEEP
)
sweep_rc=$?
[ "$sweep_rc" -eq 0 ] || refuse "the tracked-tree sweep exited $sweep_rc without producing a verdict."

case "$SWEEP" in
    "CHECKED	"*) ;;
    *) refuse "the tracked-tree sweep produced no usable output." ;;
esac

CHECKED=$(printf '%s\n' "$SWEEP" | sed -n '1s/^CHECKED\t//p')
REFUSALS=$(printf '%s\n' "$SWEEP" | sed -n 's/^REFUSE\t//p')
FAILURES=$(printf '%s\n' "$SWEEP" | sed -n 's/^FAIL\t//p')

if [ -n "$REFUSALS" ]; then
    printf '%s\n' "$REFUSALS" >&2
    refuse "the tracked-tree sweep could not be completed as a measurement (above)."
fi

if [ -n "$FAILURES" ]; then
    echo "::error title=Stale pool facts::a tracked site disagrees with the canonical pool facts" >&2
    printf '%s\n' "$FAILURES" | sed 's/^/  /' >&2
    echo >&2
    echo "canonical: pool-size=$POOL_SIZE (declared in $FACTS_REL, measured $POOL_MEASURED)" >&2
    echo "canonical: jobs-per-run=$JOBS_PER_RUN (derived from $JOBS_WF: $JOBS_BREAKDOWN)" >&2
    exit 1
fi

echo "check-pool-facts: OK — $CHECKED marked site(s) agree." \
     "pool-size=$POOL_SIZE (declared in $FACTS_REL, measured $POOL_MEASURED);" \
     "jobs-per-run=$JOBS_PER_RUN (derived from $JOBS_WF: $JOBS_BREAKDOWN)."
