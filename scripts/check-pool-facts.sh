#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The self-hosted pool's size and per-run job count, checked instead of
# repeated (#879).
#
# WHY THIS EXISTS
#
# Both numbers were stated in prose in six tracked files and checked by
# nothing. Every one of them was wrong: the pool had been resized, and so
# had the suite matrix, and each site went on saying what was true when it
# was written. Deliberately no figures in that sentence — a "was N, now M"
# in the header of the gate against stale numbers is a copy that nothing
# checks, and the M in it went stale during this change's own rebase. For
# the live values, run this script. Two of the six are not comments —
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
# WHAT THE DERIVATION REFUSES, AND WHY IT IS A REFUSAL AND NOT A SKIP
#
# Every job in that workflow must be RESOLVED — decided to be on the pool
# or decided not to be. A job the parser cannot resolve is a refusal, not
# a job quietly worth zero. That asymmetry was the first version's worst
# bug: an unmodelled MATRIX shape refused loudly with "not knowable from
# the file", while an unmodelled RUNS-ON shape was skipped by the same
# `continue` that skips `ubuntu-latest`. Since the non-vacuity guard only
# fires when NO job matches, one pool job could vanish from the count
# while another still matched, and the gate stayed green on a canonical
# value that was silently too LOW — the direction in which every marked
# site is then held against a wrong number.
#
# Unresolvable, and therefore refused:
#
#   * `runs-on` absent, and `uses:` present — a reusable-workflow call
#     lands on a runner declared in the CALLED workflow. This file does
#     not say which, so this file cannot answer the question.
#   * `runs-on` absent with no `uses:` at all.
#   * any `runs-on` carrying `${{` — in the string, in a list element, or
#     in a mapping value. Its value is a runtime fact.
#   * a `runs-on` mapping naming neither `labels` nor a `group`.
#   * a `runs-on` that is not a string, a list or a mapping.
#   * `strategy: max-parallel:` — it caps how many of a matrix's jobs are
#     in flight at once, so with it present the expansion is no longer the
#     number of runners the run occupies. Nothing in the tree uses it
#     today; it is refused now so that adding one cannot silently change
#     what this number means.
#
# Labels are compared CASE-INSENSITIVELY, because GitHub matches them that
# way: `[self-hosted, DHCP-CI]` is the same pool as `[self-hosted,
# dhcp-ci]`, and a gate that disagreed with the platform on that would
# under-count without ever saying so.
#
# A `runs-on: {group: G, labels: [...]}` matches on either G or a label,
# since a group named for the pool is targeting the pool. The residual
# hole is named rather than papered over: a group under some OTHER name
# whose members are pool runners is not knowable from the file, and this
# gate will count that job out. Membership lives in the org's runner
# settings, not in the repository.
#
# One bound in the other direction: a pool job carrying `if:` is counted
# as though it runs. Whether it does is a runtime fact. That over-counts
# rather than under-counts, which is the safe direction for a capacity
# figure — but it is an approximation, and the workflow this gate reads
# today has exactly such a job.
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
# A prose site opts in with a marker on the line that states the number.
# The marker is the word "pool", a hyphen, the word "facts", a colon, then
# NAME=VALUE. It is described here rather than spelled, and the code
# assembles it from two string literals, for one reason: a literal marker
# in this file would be a site the sweep then tried to check. Written
# MARKER below to stand for it:
#
#     # ... the pool is 99 runners ...   <!-- MARKER pool-size=99 -->
#
# and the gate requires all three of:
#
#   1. NAME is a fact this gate knows            (else: refusal)
#   2. VALUE equals the canonical value          (else: red — stale)
#   3. the prose on that line, with the marker removed, STATES VALUE
#      (else: red — the marker was bumped and the sentence was not)
#
# (3) is what stops the marker becoming a second copy that drifts from
# the sentence beside it, and it forces the digit: "a pool of eight"
# cannot satisfy it, which is how the spelling problem is closed by
# construction rather than by enumerating spellings.
#
# "STATES" is narrower than "contains", and the difference was a real
# defect in the first version of this gate rather than a refinement.
# There, (3) accepted any occurrence of the digits with no digit either
# side. A marker sitting on the line BELOW its sentence was therefore
# satisfied by a runner-name range such as `dhcp-ci-1..99` — which happens
# to end in the pool size — so the canonical value could be bumped, every marked
# line dutifully edited, and two sentences left still stating the OLD pool
# size with the gate green. The check that exists to catch a marker drifting
# from its sentence was itself satisfied by a coincidence.
#
# So VALUE must not be welded to a larger token. It may not be preceded
# by [0-9A-Za-z_.=-] and may not be followed by [0-9A-Za-z_] or by a
# decimal point and another digit. Taking 99 as the value purely so these
# examples cannot be read as claims about the live pool: `99 runners`,
# `is 99.` and `99-runner` state the number; `dhcp-ci-99`, `1..99`, `v99`,
# `OF=99` and `99.5` do not. The direction of the error that remains
# matters: a rejected site goes
# RED and says so, which costs a rewrap; an accepted one goes green and
# says nothing. This gate takes the loud failure.
#
# What it still cannot see, stated rather than implied: a line that
# states the digits in a genuinely delimited way but is talking about
# something else entirely ("99 files changed") satisfies (3). (3) is a
# syntactic check on where the digits sit, not a semantic one on what the
# sentence means, and no regex closes that. Nor does it reach a number
# ARITHMETICALLY DERIVED from a fact — "a SEVENTH job", "two runs fit" —
# which is one step downstream and has no fact of its own.
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

# A key declared twice used to resolve silently to the last one, so a
# stray second DHCP_CI_POOL_SIZE could move the canonical value with
# nothing said. "The file is the single canonical declaration" has to
# mean the file says each thing ONCE; a duplicate is a refusal.
fact_lines() {
    sed -n "s/^[[:space:]]*$1=.*/x/p" "$FACTS" | wc -l | tr -d '[:space:]'
}

for _key in DHCP_CI_POOL_SIZE DHCP_CI_POOL_SIZE_MEASURED DHCP_CI_POOL_LABEL \
            DHCP_CI_JOBS_PER_RUN_WORKFLOW; do
    _n=$(fact_lines "$_key")
    case "$_n" in
        ""|*[!0-9]*) refuse "could not count the declarations of $_key in $FACTS_REL." ;;
    esac
    [ "$_n" -le 1 ] \
        || refuse "$FACTS_REL declares $_key $_n times. Which one wins is a property of the parser rather than of the declaration, so this file no longer says what the canonical value is."
done

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


def has_expression(value):
    """True if any scalar reachable from `value` carries a ${{ }} expression."""
    if isinstance(value, str):
        return "${{" in value
    if isinstance(value, list):
        return any(has_expression(v) for v in value)
    if isinstance(value, dict):
        return any(has_expression(v) for v in value.values())
    return False


def runs_on_labels(job, name):
    """(labels, None) for a resolvable runs-on, (None, reason) otherwise.

    Every job must be DECIDED — on the pool or not on it. A job whose
    runner cannot be read out of this file is a refusal, never a skip:
    skipping it silently lowers the canonical count while the gate stays
    green, which is the failure mode this whole script exists to close.
    """
    if "uses" in job:
        return None, ("job '%s' calls a reusable workflow (`uses:`), so the runner it "
                      "lands on is declared in the called workflow and is not knowable "
                      "from this file. It might be on the pool." % name)

    spec = job.get("runs-on")
    if spec is None:
        return None, ("job '%s' declares no runs-on, so there is nothing to decide it "
                      "against." % name)
    if has_expression(spec):
        return None, ("job '%s' builds its runs-on from an expression (%r), so the runner "
                      "is a runtime fact and is not knowable from the file." % (name, spec))

    if isinstance(spec, str):
        return [spec], None
    if isinstance(spec, list):
        bad = [x for x in spec if not isinstance(x, str)]
        if bad:
            return None, ("job '%s' has a runs-on list containing a non-string entry (%r)."
                          % (name, bad[0]))
        return list(spec), None
    if isinstance(spec, dict):
        # runs-on: {group: .., labels: [..]}. A group named for the pool is
        # targeting the pool, so it counts as a label for matching purposes.
        out = []
        got = spec.get("labels")
        if isinstance(got, str):
            out.append(got)
        elif isinstance(got, list):
            out.extend(x for x in got if isinstance(x, str))
        elif got is not None:
            return None, ("job '%s' has a runs-on mapping whose labels: is neither a string "
                          "nor a list." % name)
        group = spec.get("group")
        if isinstance(group, str):
            out.append(group)
        elif group is not None:
            return None, "job '%s' has a runs-on mapping whose group: is not a string." % name
        if not out:
            return None, ("job '%s' has a runs-on mapping naming neither labels nor a group."
                          % name)
        return out, None

    return None, ("job '%s' has a runs-on that is neither a string, a list nor a mapping "
                  "(%r)." % (name, type(spec).__name__))


def matrix_count(job, name):
    """How many jobs one job definition expands to. None + reason if unmodelled."""
    strategy = job.get("strategy")
    if strategy is None:
        return 1, None
    if not isinstance(strategy, dict):
        return None, "job '%s' has a non-mapping strategy" % name
    if "max-parallel" in strategy:
        return None, ("job '%s' sets strategy: max-parallel:, which caps how many of its "
                      "matrix jobs are in flight at once. With it present the expansion is "
                      "no longer the number of runners the run occupies, so the fact this "
                      "gate derives would quietly change meaning. Teach it the semantics "
                      "rather than letting it keep the old answer." % name)
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
want_label = label.strip().lower()
for name, job in jobs.items():
    if not isinstance(job, dict):
        print("ERROR\tjob '%s' in %s did not parse to a mapping, so it could not be "
              "decided on or off the pool" % (name, path))
        sys.exit(0)
    labels, why = runs_on_labels(job, name)
    if labels is None:
        print("ERROR\t%s" % why)
        sys.exit(0)
    # GitHub matches runner labels case-insensitively; so does this.
    if want_label not in [x.strip().lower() for x in labels]:
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
# The leading boundary is load-bearing, and its absence was caught here by
# this gate running on its own source: without it the word matches inside
# `check-pool-facts:`, this script's own name, which appears in every
# message it prints and in its self-test's assertions. That is the same
# token-boundary defect as a bare value matching inside `dhcp-ci-1..99`,
# arriving from the other side.
BOUNDARY = r"(?<![A-Za-z0-9_-])"
MARKER = re.compile(BOUNDARY + r"pool" + r"-facts:\s*([A-Za-z][A-Za-z0-9-]*)=([0-9]+)")

# The bare marker WORD, with no requirement that a well-formed NAME=VALUE
# follows it. Every occurrence of the word must parse as a complete marker
# or the sweep refuses: a marker that does not parse binds nothing and,
# before this check existed, said nothing about it. That was not
# hypothetical -- it was found on this gate's own facts file, where a
# rewrap left the value and an opening `(pool` on one line and the rest of
# the marker on the next. The site LOOKED bound to a reader, the sweep skipped it in
# silence, and the count went up by one anyway because an unrelated site
# had just been added. A silently unbound site is the same defect as a
# stale one, minus the evidence.
MARKER_WORD = re.compile(BOUNDARY + r"pool" + r"-facts:")


def STATES(value):
    """Does the prose STATE this number, rather than merely contain the digits?

    The digits must not be welded into a larger token. Rejecting the left
    neighbours [0-9A-Za-z_.=-] kills `dhcp-ci-16`, `1..16`, `v16` and
    `OF=6`; rejecting the right neighbours [0-9A-Za-z_] and a following
    `.<digit>` kills `16x` and `16.5`. A trailing `.` or `-` survives, so
    `... is 99.` and `a 99-runner pool` still count as statements.

    An over-strict rule here costs a rewrap and says why; an over-loose
    one goes green over a stale sentence. This takes the loud side.
    """
    return re.compile(r"(?<![0-9A-Za-z_.=-])" + re.escape(value)
                      + r"(?![0-9A-Za-z_])(?!\.[0-9])")

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
        words = list(MARKER_WORD.finditer(line))
        hits = list(MARKER.finditer(line))
        if len(words) != len(hits):
            findings.append(
                "REFUSE\t%s:%d\tcarries the marker word %d time(s) but only %d of them "
                "parse as `NAME=VALUE`. A marker that does not parse is not a site: it "
                "binds nothing, and it is refused rather than skipped because a reader "
                "cannot tell the difference by looking. The usual cause is a line wrap "
                "splitting the marker; keep it whole, on the line that states the number."
                % (name, lineno, len(words), len(hits)))
            continue
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
            if not STATES(value).search(prose):
                findings.append(
                    "FAIL\t%s:%d\tmarks %s=%s, but the sentence on that line does not state "
                    "%s as a number of its own. Either the marker was updated and the prose "
                    "was not -- the exact decay this gate exists to catch, one layer in -- or "
                    "the only %s on the line is welded into a larger token such as a runner "
                    "name or a version, which is a coincidence rather than a statement. Put "
                    "the marker on the line that says the number."
                    % (name, lineno, fact, value, value, value))

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
