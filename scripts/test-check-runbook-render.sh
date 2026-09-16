#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Self-test for check-runbook-render.sh.
#
# gate-selftest-runs-in: docs-site
#
# NOT policy-gates. This gate renders markdown, so it needs
# python-markdown, pymdown-extensions and PyYAML, and the only job that
# installs them is `docs-site` in .github/workflows/pages.yml
# (docs/requirements.txt, hash-pinned). Running this self-test anywhere
# else measures the absence of a toolchain, not the gate.
#
# The fixtures are SYNTHETIC and built here, not copies of the real
# runbook: a self-test that reads docs/release-runbook.md passes or fails
# for whatever that file happens to say today, and its "fixed document
# passes" case would have no fixed document to point at.
#
# Usage: bash scripts/test-check-runbook-render.sh

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
gate="$here/check-runbook-render.sh"

if [ ! -x "$gate" ] && [ ! -f "$gate" ]; then
  echo "test-check-runbook-render: gate not found at $gate" >&2
  exit 2
fi

# shellcheck source=scripts/tmpdir-guard.sh
. "$here/tmpdir-guard.sh"
guarded_tmpdir TMP

cat >"$TMP/mkdocs.yml" <<'YML'
site_name: fixture
markdown_extensions:
  - admonition
  - attr_list
  - md_in_html
  - tables
  - toc:
      permalink: true
  - pymdownx.highlight:
      anchor_linenums: true
  - pymdownx.inlinehilite
  - pymdownx.snippets
  - pymdownx.superfences
  - pymdownx.tabbed:
      alternate_style: true
YML

cat >"$TMP/mkdocs-no-exts.yml" <<'YML'
site_name: fixture
YML

# FIXED: every block that starts after a blank line inside step 9 is
# indented to four spaces, which is what python-markdown continues a list
# item at. The trailing "Every green checklist" line is deliberately left
# at three, so the fixture also proves the gate judges the WALKTHROUGH and
# not the whole step.
cat >"$TMP/fixed.md" <<'MD'
# Fixture

8. **Merge the release PR.** Squash or merge commit.
9. **Pull main, dry-run, then tag:** push the rc and confirm it is
   green. Expected steps, under the names the run shows. Tag resolution
   is its own job: the **release** job runs checkout → setup-go →
   **Sign published images (cosign keyless)** → Install oras → Workflow
   summary.

    Then, as separate jobs:

    - **promote-latest**: since #736 this is where every floating tag
      moves. Steps: *Promote the GHCR floating tags* → *Verify the
      floating tags resolve to the signed digests* → done.

   Every green checklist below includes the arm64 jobs.
10. **Confirm the GitHub Release**.
MD

# BROKEN: the shape at 58bb335. A column-0 HTML comment splits the
# paragraph, and every continuation block sits at three spaces.
cat >"$TMP/broken.md" <<'MD'
# Fixture

8. **Merge the release PR.** Squash or merge commit.
9. **Pull main, dry-run, then tag:** push the rc and confirm it is
   green. Expected steps, under the names the run shows.

<!-- release-walkthrough: release, promote-latest -->
 Tag resolution is its own job: the **release** job runs checkout →
   setup-go → **Sign published images (cosign keyless)** → Install oras
   → Workflow summary.

   Then, as separate jobs:

   - **promote-latest**: since #736 this is where every floating tag
     moves. Steps: *Promote the GHCR floating tags* → *Verify the
     floating tags resolve to the signed digests* → done.

   Every green checklist below includes the arm64 jobs.
10. **Confirm the GitHub Release**.
MD

# ANCHOR GONE: correctly indented, walkthrough intact, but the sentence
# the gate keys the step on has been reworded away.
sed 's/Expected steps, under the names the run shows\. //' \
  "$TMP/fixed.md" >"$TMP/no-anchor.md"

# DOMAIN EMPTY: the anchor is still there and the indentation is still
# right, but the walkthrough it is supposed to judge has been removed.
cat >"$TMP/no-walkthrough.md" <<'MD'
# Fixture

8. **Merge the release PR.** Squash or merge commit.
9. **Pull main, dry-run, then tag:** push the rc and confirm it is
   green. Expected steps, under the names the run shows.

    The walkthrough used to be here and is not any more.

10. **Confirm the GitHub Release**.
MD

fails=0
pass=0

run_case() {
  # $1 label, $2 expected exit, $3 doc, $4 mkdocs, $5 (optional) text the
  # stderr must contain
  local label="$1" want="$2" doc="$3" cfg="$4" needle="${5:-}"
  local out rc=0
  out="$(bash "$gate" "$doc" "$cfg" 2>&1)" || rc=$?
  if [ "$rc" -ne "$want" ]; then
    printf 'FAIL %-38s expected exit %s, got %s\n' "$label" "$want" "$rc"
    printf '%s\n' "$out" | sed 's/^/       /'
    fails=$((fails + 1))
    return
  fi
  if [ -n "$needle" ] && ! printf '%s' "$out" | grep -F -- "$needle" >/dev/null; then
    printf 'FAIL %-38s exit %s as expected, but the message never said %s\n' \
      "$label" "$want" "$needle"
    printf '%s\n' "$out" | sed 's/^/       /'
    fails=$((fails + 1))
    return
  fi
  printf 'ok   %-38s exit %s\n' "$label" "$rc"
  pass=$((pass + 1))
}

run_case "fixed document passes"        0 "$TMP/fixed.md"          "$TMP/mkdocs.yml" "renders inside"
run_case "broken document fails"        1 "$TMP/broken.md"         "$TMP/mkdocs.yml" "renders OUTSIDE"
run_case "broken names the line number" 1 "$TMP/broken.md"         "$TMP/mkdocs.yml" "must be indented 4"
run_case "anchor removed refuses"       2 "$TMP/no-anchor.md"      "$TMP/mkdocs.yml" "CANNOT JUDGE"
run_case "empty domain refuses"         2 "$TMP/no-walkthrough.md" "$TMP/mkdocs.yml" "no walkthrough content to judge"
run_case "missing runbook refuses"      2 "$TMP/absent.md"         "$TMP/mkdocs.yml" "does not exist"
run_case "missing mkdocs.yml refuses"   2 "$TMP/fixed.md"          "$TMP/absent.yml" "does not exist"
run_case "no extensions refuses"        2 "$TMP/fixed.md" "$TMP/mkdocs-no-exts.yml" "no markdown_extensions"

# The domain-emptying case is the one that matters most, so drive it from
# the other direction too: a document that keeps the walkthrough but moves
# it out of the step must FAIL, not refuse. Without this, "empty domain
# refuses" could be satisfied by a gate that refuses on everything.
run_case "walkthrough present but outside" 1 "$TMP/broken.md" "$TMP/mkdocs.yml" \
  "Promote the GHCR floating tags"

# The offender list is bounded by the last stray anchor. The broken
# fixture's trailing "Every green checklist" line is under-indented too,
# and the fixed fixture keeps it under-indented and still passes -- so a
# diagnostic that named it would be asking for an edit the gate does not
# require. Asserted here because it is invisible from the exit code.
out="$(bash "$gate" "$TMP/broken.md" "$TMP/mkdocs.yml" 2>&1 || true)"
if printf '%s' "$out" | grep -F -- "Every green checklist" >/dev/null; then
  printf 'FAIL %-38s named a line after the last stray anchor\n' \
    "offender list stops at the walkthrough"
  printf '%s\n' "$out" | sed 's/^/       /'
  fails=$((fails + 1))
else
  printf 'ok   %-38s bounded\n' "offender list stops at the walkthrough"
  pass=$((pass + 1))
fi

# ...and it must still name the lines that ARE the cause, or the bound
# above would be satisfied by printing nothing at all.
if printf '%s' "$out" | grep -F -- "Then, as separate jobs" >/dev/null; then
  printf 'ok   %-38s still named\n' "the causing lines"
  pass=$((pass + 1))
else
  printf 'FAIL %-38s the bound emptied the offender list\n' "the causing lines"
  printf '%s\n' "$out" | sed 's/^/       /'
  fails=$((fails + 1))
fi

printf '\n%d passed, %d failed\n' "$pass" "$fails"
[ "$fails" -eq 0 ]
