#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for release-body.sh.
#
# Every case drives the SHIPPED script against a fixture notes file. The
# extraction is not restated here: a self-test carrying its own copy of the
# awk would go green on a workflow that had stopped calling the script at
# all, which is the failure the script was extracted from the YAML to close.
#
# The last two cases are the preservation controls, run against the REAL
# RELEASE_NOTES.md and the REAL release.yml: a released version still
# extracts, and the workflow carries no second extraction of its own.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/release-body.sh"
ROOT="$(dirname "$HERE")"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# <name> <want-exit> <tag> <notes-body> [<expect-in-output>]
# Output is stdout AND stderr: a refusal has to be readable, and asserting
# only on the exit code would let every refusal carry the same message.
run_case() {
    local name="$1" want="$2" tag="$3" body="$4" expect="${5:-}"
    local f out rc
    f=$(mktemp)
    printf '%s\n' "$body" > "$f"
    out=$(bash "$GATE" "$tag" "$f" 2>&1)
    rc=$?
    rm -f "$f"
    if [ "$rc" != "$want" ]; then
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
        return
    fi
    if [ -n "$expect" ] && ! printf '%s' "$out" | grep -F -- "$expect" >/dev/null; then
        no "$name (exit $rc as wanted, but the output does not name '$expect')"
        printf '      %s\n' "$out" >&2
        return
    fi
    ok "$name"
}

NOTES_OK='## v2.0.0

The release everyone waited for.

### Fixed

- something (#1)

## v1.9.0

older
'

# 1. THE EXACT HEADING. The control for every refusal below: without it a
#    script that refused everything would satisfy the whole suite.
run_case "an exact '## vX.Y.Z' heading yields its section" 0 v2.0.0 "$NOTES_OK" \
    "The release everyone waited for."

# The section must STOP at the next version. A body that ran on would
# publish the previous release's notes underneath this one's.
out=$(printf '%s\n' "$NOTES_OK" > /tmp/rb.$$ && bash "$GATE" v2.0.0 /tmp/rb.$$ 2>/dev/null; rm -f /tmp/rb.$$)
if printf '%s' "$out" | grep -F 'older' >/dev/null; then
    no "the section ran on into the next version's notes"
else
    ok "the section stops at the next '## ' heading"
fi

# `###` is a subsection INSIDE the version, and must not end it.
if printf '%s' "$out" | grep -F '### Fixed' >/dev/null; then
    ok "a '### ' subsection stays inside the section"
else
    no "a '### ' subsection was treated as the end of the section"
fi

# 2. THE LIVE FAILURE. `## v2.0.0 (unreleased)` is not `## v2.0.0`, and the
#    shipped workflow answered that with a placeholder and exit 0.
run_case "a heading carrying '(unreleased)' REFUSES and quotes the suffix" 1 v2.0.0 \
    '## v2.0.0 (unreleased)

notes
' \
    '## v2.0.0 (unreleased)'

# Keyed on "the heading carries something after the version", not on the
# word. Two spellings enumerated means a third exists, so the second
# decoration is driven with a different word entirely.
run_case "any decoration after the version REFUSES, not just '(unreleased)'" 1 v2.0.0 \
    '## v2.0.0 — final

notes
' \
    'carries something after it'

# THE OTHER DIRECTION of that rule: `## v2.0.0-rc1` starts with the string
# `## v2.0.0` but is a DIFFERENT VERSION, not a decorated one. A prefix
# test without the whitespace requirement would refuse this file, which
# would make every release with a published rc section unreleasable.
run_case "a neighbouring pre-release heading is not a decoration" 0 v2.0.0 \
    '## v2.0.0-rc1

the dry run
## v2.0.0

the real thing
' \
    'the real thing'

# 3. NO SECTION AT ALL. The tag is named, and so is what the file does
#    have, because "no section" with no census sends the releaser to grep.
run_case "no section for the tag REFUSES naming the tag" 1 v3.1.4 "$NOTES_OK" \
    "no '## v3.1.4' section"

# 4. AN EMPTY SECTION. Exit 0 with nothing on stdout is the placeholder
#    failure with the placeholder removed: a release page with no body.
run_case "an empty section REFUSES" 1 v2.0.0 \
    '## v2.0.0

## v1.9.0

older
' \
    'is empty'

# 5. TWO SECTIONS FOR ONE VERSION. Which one ships would be position.
run_case "two headings for one version REFUSE" 1 v2.0.0 \
    '## v2.0.0

first

## v2.0.0

second
' \
    "2 '## v2.0.0' headings"

# --- pre-release tags -------------------------------------------------
#
# An rc is a dry run OF a version and this repository writes no notes
# section per rc, so a fail-closed extraction that demanded `## vX.Y.Z-rcN`
# would turn every rc release red. The fallback is between two EXACT
# headings and it is announced.
run_case "an rc tag publishes its release version's section" 0 v2.0.0-rc2 "$NOTES_OK" \
    "has no section of its own"

run_case "an rc tag prefers its OWN section when the file has one" 0 v2.0.0-rc2 \
    '## v2.0.0-rc2

just the rc

## v2.0.0

the release
' \
    'just the rc'

# The fallback does not reach a decorated heading: the live tree's
# `## v2.0.0 (unreleased)` must refuse for an rc tag exactly as it does for
# the release tag, or the rc would publish notes the maintainer has not
# signed off as final.
run_case "an rc tag REFUSES a decorated release heading" 1 v2.0.0-rc2 \
    '## v2.0.0 (unreleased)

notes
' \
    'carries something after it'

run_case "an rc tag with no section anywhere REFUSES naming both" 1 v3.1.4-rc1 "$NOTES_OK" \
    "none for '## v3.1.4'"

# --- refusals that are not verdicts ------------------------------------
run_case "a tag that is not vX.Y.Z-shaped cannot be judged" 2 main "$NOTES_OK"
run_case "a notes file with no '## ' heading at all cannot be judged" 2 v2.0.0 \
    'just prose, no headings
'

out=$(bash "$GATE" v2.0.0 /nonexistent/RELEASE_NOTES.md 2>&1); rc=$?
if [ "$rc" = "2" ]; then ok "an unreadable notes file cannot be judged"
else no "an unreadable notes file gave exit $rc, want 2: $out"; fi

out=$(bash "$GATE" 2>&1); rc=$?
if [ "$rc" = "2" ]; then ok "no tag argument cannot be judged"
else no "no tag argument gave exit $rc, want 2: $out"; fi

# --- preservation controls, against the real tree ----------------------
#
# A suite of fixtures cannot tell whether the shipped file still works.
out=$(bash "$GATE" v1.9.0 "$ROOT/RELEASE_NOTES.md" 2>&1); rc=$?
if [ "$rc" = "0" ] && [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]; then
    ok "the real RELEASE_NOTES.md still yields a body for the shipped v1.9.0"
else
    no "the real RELEASE_NOTES.md did not yield a v1.9.0 body (exit $rc)"
fi

# THE SECOND COPY IS THE FAILURE THIS SCRIPT EXISTS TO PREVENT. If the
# workflow keeps its own extraction, every case above tests a file the
# release does not use. Comments are stripped first, so this file's own
# prose and the workflow's cannot satisfy or trip it.
WF="$ROOT/.github/workflows/release.yml"
if [ ! -f "$WF" ]; then
    no "release.yml is missing, so nothing was checked about the caller"
else
    body=$(grep -vE '^[[:space:]]*#' "$WF")
    if printf '%s' "$body" | grep -F 'RELEASE_NOTES.md' >/dev/null \
       && ! printf '%s' "$body" | grep -F 'scripts/release-body.sh' >/dev/null; then
        no "release.yml reads RELEASE_NOTES.md without calling scripts/release-body.sh"
    elif printf '%s' "$body" | grep -E 'awk .*hdr|Pre-release / dry-run build' >/dev/null; then
        no "release.yml still carries its own extraction or the placeholder fallback"
    else
        ok "release.yml has no extraction of its own and calls the script"
    fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
