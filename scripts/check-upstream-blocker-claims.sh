#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# An upstream-blocked issue must not be described as waiting on a Docker
# engine version (#673).
#
# WHY THIS EXISTS
#
# #673 corrected the claim that engine ">= 28" unblocks #125. It does
# not: `interface_name` waits on moby/moby#52866, the remote-driver
# change that stops the proxy dropping a plugin's `DstName`. No engine
# floor changes that, and the suite decides at runtime with a capability
# probe rather than a version comparison.
#
# The correction was applied under docs/ and the same sentence stayed
# in `ci/runner-image/README.md` and `ci/runner-image/Dockerfile` — a
# docs-scoped review cannot see a claim that lives outside docs/. The
# claim is not cosmetic: it tells whoever rebuilds the runner image that
# bumping the engine will turn those tests on, so the next engine bump
# is met with "why are they still skipping" instead of "the upstream PR
# has not merged".
#
# WHAT IT CHECKS
#
# For each issue the roadmap lists as blocked upstream: no chunk of any
# file may tie an engine version to that issue without naming the
# upstream project that is the actual blocker. "Chunk" is a table row, a
# list item, or a run of consecutive non-blank lines — a paragraph, or
# one block of comment. That granularity is the whole design: the
# Dockerfile's version and its `#125` were on two different lines of one
# comment, and a line-scoped check would have read them apart and passed.
#
# The blocked-issue list is DERIVED from the roadmap's "Blocked upstream"
# table, never listed here. A third blocked issue is covered the day the
# roadmap grows a row for it; a shipped one stops being checked the day
# the row goes.
#
# WHAT IT DELIBERATELY DOES NOT DO
#
# It does not judge whether the sentence is true — it judges whether the
# author confronted the real blocker. These are all legitimate and pass:
#
#   * "Compose `interface_name`, engine 28+" — a true statement about the
#     Compose KEY, which needs no issue reference and gets no version
#     verdict from this gate.
#   * "the runner image needs Engine >= 28" — true, for its own reasons.
#   * "milestoned for engine 29.8.0; it unblocks #125" beside
#     moby/moby#52866 — the version that carries the upstream fix is the
#     one thing a version legitimately says about a blocked issue, and
#     the upstream reference is right there.
#
# What fails is a version tied to a blocked issue with the upstream
# nowhere in sight, which is exactly the shape that shipped. Naming moby
# in the same breath as the version is a low bar on purpose: it is not
# proof, it is the point at which a reviewer reading the sentence can
# see the contradiction. A gate that tried to decide which engine
# release carries which upstream commit would be guessing.
#
# Untracked-but-unignored files are scanned too. An index-scoped gate
# cannot see a file that has not been `git add`ed yet, so a new page
# would go unread on exactly the run that was supposed to check it.
#
# Usage: check-upstream-blocker-claims.sh [--root <dir>] [--roadmap <file>]
# Exit:  0 clean
#        1 a version-threshold claim is tied to a blocked issue
#        2 cannot check — no root, no roadmap, or a roadmap whose
#          blocked-upstream table declares nothing

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
ROADMAP=""

while [ $# -gt 0 ]; do
    case "$1" in
        --root) ROOT="${2:-}"; shift 2 || exit 2 ;;
        --roadmap) ROADMAP="${2:-}"; shift 2 || exit 2 ;;
        *) echo "usage: $0 [--root <dir>] [--roadmap <file>]" >&2; exit 2 ;;
    esac
done
[ -n "$ROADMAP" ] || ROADMAP="$ROOT/docs/roadmap.md"

if [ ! -d "$ROOT" ]; then
    echo "::error title=Upstream-claim root missing::$ROOT is not a directory" >&2
    exit 2
fi
if [ ! -f "$ROADMAP" ]; then
    echo "::error title=Roadmap missing::$ROADMAP" >&2
    echo "      The blocked-issue list is derived from it; without it this gate" >&2
    echo "      would pass having checked nothing." >&2
    exit 2
fi

# The gate's own script and self-test carry the forbidden phrasings as
# fixtures. Excluded by exact path, not by pattern — a pattern would
# quietly excuse the next file whose name happens to match.
SELF="scripts/$(basename "$0")"
SELF_TEST="scripts/test-$(basename "$0")"

python3 - "$ROOT" "$ROADMAP" "$SELF" "$SELF_TEST" <<'PY'
import os, re, subprocess, sys

root, roadmap, self_path, self_test = sys.argv[1:5]

# ---- the blocked-issue list, derived ---------------------------------
text = open(roadmap, encoding="utf-8").read()
sec = re.search(r"^##+\s+Blocked upstream.*?(?=^##\s|\Z)", text, re.S | re.M)
if not sec:
    print("::error title=No blocked-upstream section::%s has no "
          "'## Blocked upstream' section — this gate derives its issue list "
          "from it and will not guess." % roadmap, file=sys.stderr)
    sys.exit(2)

blocked = {}
for line in sec.group(0).splitlines():
    m = re.match(r"^\|\s*\[?#(\d+)\]?", line)
    if m:
        blocked[m.group(1)] = re.findall(r"moby/moby#\d+", line) or ["the upstream change"]

if not blocked:
    print("::error title=No blocked issues declared::%s has a blocked-upstream "
          "section with no issue rows. Either the table's shape changed or "
          "nothing is blocked; both need a human, neither is a pass."
          % roadmap, file=sys.stderr)
    sys.exit(2)

# ---- what a claim looks like ------------------------------------------
# A version threshold: ">= 28", "≥ 28", "28+", "engine 29.8.0". Bare
# numbers are not thresholds — "26.x" and a bug number must not count.
VERSION = re.compile(
    r"(?:(?:>=|≥|>)\s*\d+(?:\.\d+)*)"
    r"|(?:\b\d+(?:\.\d+)*\+)"
    r"|(?:\bengines?\s+\*{0,2}\d+(?:\.\d+)*)", re.I)
# The verb that turns a version into a claim about the issue. "needs"
# and "requires" are left out on purpose: they attach to whatever is
# nearest, and every blocked issue's own description says what it NEEDS
# from upstream.
CLAIM = re.compile(r"\b(unblocks?|unblocked|unblocking|unlocks?|unlocked"
                   r"|enables?|enabled|gated|gates|gating)\b", re.I)
# The version has to be about the engine, not about a Go release or a
# plugin tag that happens to sit in the same paragraph.
ENGINE = re.compile(r"\bengine\b|\bdocker-ce\b|\bdockerd\b", re.I)
# Naming the upstream project is what clears the chunk.
UPSTREAM = re.compile(r"\bmoby\b", re.I)

ITEM = re.compile(r"^\s*(?:[-*+]\s|\d+[.)]\s)")


def chunks(body):
    """Table rows and list items stand alone; everything else groups by
    blank lines. Without the first two a whole table or bullet list reads
    as one blob and unrelated rows contaminate each other."""
    out, buf, start = [], [], 0
    lines = body.splitlines()

    def flush():
        if buf:
            out.append((start + 1, "\n".join(buf)))
            buf.clear()

    for i, line in enumerate(lines):
        s = line.strip()
        if not s:
            flush()
            continue
        if s.startswith("|") or ITEM.match(line):
            flush()
            start = i
            buf.append(line)
            continue
        if not buf:
            start = i
        buf.append(line)
    flush()
    return out


# ---- the files --------------------------------------------------------
try:
    listing = subprocess.run(
        ["git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard"],
        capture_output=True, text=True, check=True).stdout
except (OSError, subprocess.CalledProcessError) as exc:
    print("::error title=Cannot list files::%s is not a readable git "
          "checkout (%s)" % (root, exc), file=sys.stderr)
    sys.exit(2)

files = [f for f in listing.splitlines() if f]
if not files:
    print("::error title=No files to scan::%s listed nothing. This step "
          "would otherwise pass having read no file at all." % root,
          file=sys.stderr)
    sys.exit(2)

skip = {self_path, self_test}
findings = []
scanned = 0
for rel in files:
    if rel in skip:
        continue
    path = os.path.join(root, rel)
    if not os.path.isfile(path) or os.path.islink(path):
        continue
    try:
        body = open(path, encoding="utf-8").read()
    except (OSError, UnicodeDecodeError):
        continue          # binary or unreadable: nothing to read a claim in
    scanned += 1
    for issue, prs in blocked.items():
        if "#" + issue not in body:
            continue
        for lineno, chunk in chunks(body):
            if "#" + issue not in chunk:
                continue
            if not (VERSION.search(chunk) and CLAIM.search(chunk)
                    and ENGINE.search(chunk)):
                continue
            if UPSTREAM.search(chunk):
                continue
            findings.append((rel, lineno, issue, prs, chunk))

if findings:
    for rel, lineno, issue, prs, chunk in findings:
        print("FAIL  %s:%d ties a Docker engine version to #%s without naming "
              "the upstream change that actually blocks it (%s):"
              % (rel, lineno, issue, ", ".join(prs)))
        for line in chunk.splitlines()[:6]:
            print("    %s" % line)
        print()
    print("#%s is blocked on an upstream change, not on an engine floor "
          "(#673). Either name the upstream PR in the same paragraph, row or "
          "comment block, or drop the version from the sentence — a reader "
          "who bumps the engine and finds the tests still skipping has been "
          "told the wrong thing by this repository."
          % ", #".join(sorted({f[2] for f in findings})))
    sys.exit(1)

print("PASS  %d file(s) scanned; no engine-version claim is tied to %s"
      % (scanned, ", ".join("#" + i for i in sorted(blocked))))
PY
