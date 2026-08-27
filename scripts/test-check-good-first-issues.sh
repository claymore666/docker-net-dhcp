#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-good-first-issues.sh (#537, #851), driven
# through the GFI_README / GFI_BADGE / GFI_LABELS / GFI_GH seams against
# synthetic files and a stub `gh`. Nothing here touches the network.
#
# The cases that matter come in three groups.
#
#   * The 2s. This gate's whole reason to exist is that a claim about
#     live tracker state decays silently, so a version of it that answers
#     0 when it cannot actually look would reproduce the original bug
#     with extra steps. "The API returned nothing" must never be read as
#     "the label has no issues" — absent data is not a zero.
#
#   * The ENCODINGS. The Unmet arm asserts an absence: that the README
#     links no listing of starter tasks. An absence check keyed on one
#     rendering of a URL passes while every other rendering keeps the
#     promise on the page — the gate reproducing its own silence. Each
#     way GitHub spells a label listing is driven as its own case, and
#     the control beside them proves the detector is not simply matching
#     every github.com link.
#
#   * The REVERSE DIRECTION. Unmet with open starter tasks is red too.
#     That is what makes the honest answer reversible rather than a
#     one-way door: nothing else would notice that the criterion is met
#     again.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-good-first-issues.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

REPO='https://github.com/claymore666/docker-net-dhcp'
URL="$REPO/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22"
OTHER="$REPO/issues?q=is%3Aissue+is%3Aopen+label%3A%22starter+task%22"
PLAIN="$REPO/issues"

# The declaration this gate anchors the label name to (#715).
LABELS="$TMP/labels.yml"
cat > "$LABELS" <<'YML'
- name: bug
  role: type
  description: Something is broken

- name: good first issue
  role: status
  description: Self-contained starter task — no deep project context needed
YML
LABELS_RENAMED="$TMP/labels-renamed.yml"
sed 's/^- name: good first issue$/- name: starter task/' "$LABELS" > "$LABELS_RENAMED"

mk_readme() { printf 'Contributing.\n\nPick up a [`good first issue`](%s) to start.\n' "$1" > "$2"; }
mk_readme_nolink() { printf 'Contributing.\n\nNo starter tasks right now; open an issue and one will be scoped.\n' > "$1"; }

mk_badge() {  # <status> <justification> <out>
    python3 - "$1" "$2" "$3" <<'PY'
import json, sys
json.dump({"small_tasks_status": sys.argv[1],
           "small_tasks_justification": sys.argv[2]},
          open(sys.argv[3], "w"))
PY
}

# A stub gh. $STUB_OUT is what `gh issue list` prints; $STUB_RC its exit.
#
# `${STUB_OUT-[]}` and NOT `${STUB_OUT:-[]}`: the colon form substitutes
# the default for an EMPTY value as well as an unset one, which would
# make the "the API answered with nothing" case silently test "[]" — the
# precise distinction that case exists to check.
cat > "$TMP/gh" <<'EOF'
#!/usr/bin/env bash
[ "${STUB_RC:-0}" -ne 0 ] && exit "${STUB_RC}"
printf '%s' "${STUB_OUT-[]}"
EOF
chmod +x "$TMP/gh"

STUB_OUT='[]'; STUB_RC=0
export STUB_OUT STUB_RC

run() { # <mode> <readme> <badge> [gh] [labels]
    GFI_README="$2" GFI_BADGE="$3" GFI_GH="${4:-$TMP/gh}" GFI_LABELS="${5:-$LABELS}" \
        bash "$GATE" "$1" > "$TMP/out" 2>&1
    echo $?
}

check() { # <name> <want_rc> <got_rc> [grep]
    local name="$1" want="$2" got="$3" want_grep="${4:-}"
    local good=1
    [ "$got" = "$want" ] || good=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then good=0; fi
    if [ "$good" -eq 1 ]; then ok "$name"; else
        no "$name (want rc=$want${want_grep:+/grep '$want_grep'}, got rc=$got)"
        sed 's/^/      /' "$TMP/out" >&2
    fi
}

# ---- fixtures --------------------------------------------------------
mk_readme "$URL" "$TMP/readme.ok"
mk_readme_nolink "$TMP/readme.silent"
mk_badge Met "The tracker carries a good first issue label ($URL), seeded with #1 (a) and #2 (b)." "$TMP/badge.met"
mk_badge Unmet "Honestly unmet: the seeded tasks were all picked up and no unclaimed work qualifies." "$TMP/badge.unmet"

# ---- --static, Met ---------------------------------------------------
check "static/Met: both artifacts link the declared label" 0 \
    "$(run --static "$TMP/readme.ok" "$TMP/badge.met")" 'small_tasks is Met'

# The failure this static half was written for: a label rename updates
# one artifact and leaves the other pointing at a query matching nothing.
mk_readme "$OTHER" "$TMP/readme.renamed"
check "static/Met: a README linking another label is named as such" 1 \
    "$(run --static "$TMP/readme.renamed" "$TMP/badge.met")" 'names another label'

check "static/Met: a README with no link at all cannot see (2, not 0)" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.met")" 'Cannot see'

mk_badge Met "No link here at all, just prose." "$TMP/badge.met.nourl"
check "static/Met: a justification with no link cannot see" 2 \
    "$(run --static "$TMP/readme.ok" "$TMP/badge.met.nourl")" 'Cannot see'

# The unquoted hyphenated form filters on a label of THAT name, which is
# not this one. Read as a different label, not as a match.
mk_readme "$REPO/issues?q=is%3Aopen+label%3Agood-first-issue" "$TMP/readme.hyphen"
check "static/Met: label:good-first-issue is a DIFFERENT label, not a match" 1 \
    "$(run --static "$TMP/readme.hyphen" "$TMP/badge.met")" 'names another label'

# ---- --static, Unmet: the promise must be gone in EVERY rendering ----
check "static/Unmet: no promise anywhere is green" 0 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet")" 'small_tasks is Unmet'

# The control for the group below: the detector must not simply fire on
# any github.com link, or the honest README could never link the tracker.
printf 'Open a [GitHub issue](%s).\n' "$PLAIN" > "$TMP/readme.plain"
check "static/Unmet: a plain tracker link is not a starter-task promise" 0 \
    "$(run --static "$TMP/readme.plain" "$TMP/badge.unmet")" 'small_tasks is Unmet'

# A listing for some OTHER label is not this gate's business either.
mk_readme "$OTHER" "$TMP/readme.otherlabel"
check "static/Unmet: a listing for another label is not our promise" 0 \
    "$(run --static "$TMP/readme.otherlabel" "$TMP/badge.unmet")" 'small_tasks is Unmet'

# Every way GitHub spells a listing of OUR label. Each of these leaves a
# clickable promise on the front page while small_tasks says Unmet.
i=0
for enc in \
    "$REPO/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22" \
    "<$REPO/issues?q=label:\"good first issue\">" \
    "$REPO/issues?q=is%3Aopen+label%3A%22Good+First+Issue%22" \
    "$REPO/issues?labels=good+first+issue" \
    "$REPO/labels/good%20first%20issue" \
    "$REPO/issues/labels/good%20first%20issue" \
    ; do
    i=$((i + 1))
    mk_readme "$enc" "$TMP/readme.enc$i"
    check "static/Unmet: promise still on the page, encoding $i" 1 \
        "$(run --static "$TMP/readme.enc$i" "$TMP/badge.unmet")" 'still promises starter tasks'
done

# "We have none" while naming tasks is the stale-citation defect (#851)
# wearing a different hat. Without this case the rule is prose.
mk_badge Unmet "Honestly unmet; the seeded tasks #534, #535 and #536 were all picked up." "$TMP/badge.unmet.cites"
check "static/Unmet: a justification that still cites tasks is red" 1 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet.cites")" 'cites #534, #535, #536'

# ---- --static, things the gate cannot judge --------------------------
mk_badge 'N/A' "Not applicable, apparently." "$TMP/badge.na"
check "static: a status that is neither Met nor Unmet cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.na")" 'can only judge'

python3 -c 'import json,sys;json.dump({"small_tasks_justification":"no status key"},open(sys.argv[1],"w"))' "$TMP/badge.nostatus"
check "static: an absent small_tasks_status cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.nostatus")" 'Cannot see'

printf '{ not json' > "$TMP/badge.broken"
check "static: unparseable badge JSON cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.broken")"

check "static: a missing README cannot see" 2 \
    "$(run --static "$TMP/does-not-exist" "$TMP/badge.unmet")" 'missing or unreadable'

# The anchor. If the declaration no longer names the label this gate
# watches, the gate does not know what to count and must say so — not
# quietly count a label that no longer exists.
check "static: the label missing from labels.yml cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS_RENAMED")" \
    'is not declared in'
check "static: a missing labels.yml cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$TMP/no-labels.yml")" \
    'missing or unreadable'

# ---- --live, Met -----------------------------------------------------
mk_badge Met "The label ($URL) is seeded with #534 (a), #535 (b) and #536 (c)." "$TMP/badge.live"

STUB_OUT='[{"number":534},{"number":535},{"number":536}]'; STUB_RC=0
check "live/Met: all cited issues open and labelled passes" 0 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'every cited issue'

# THE case this gate was written for: the last starter task got picked up.
STUB_OUT='[]'
check "live/Met: an empty label fails, because the promise is now false" 1 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'No starter tasks left'

# The empty-label rule needs its OWN input. With a justification that
# cites issue numbers, the citation check fails those numbers too and the
# verdict is red either way — so a mutant that switched the empty-label
# red off survived until this case existed. A Met claim whose
# justification names no numbers is the input that isolates it.
mk_badge Met "The label ($URL) is seeded with self-contained starter tasks." "$TMP/badge.met.nocites"
STUB_OUT='[{"number":900}]'; STUB_RC=0
check "live/Met: an uncited Met claim with open tasks is green" 0 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.met.nocites")" 'open issue(s) carry'
STUB_OUT='[]'
check "live/Met: an uncited Met claim with an empty label is still red" 1 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.met.nocites")" 'No starter tasks left'

# Other tasks exist, but the justification names work that is done.
STUB_OUT='[{"number":534},{"number":999}]'
check "live/Met: a cited issue that is no longer open/labelled fails" 1 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'cites #535'

# ---- --live, Unmet: the reverse decay -------------------------------
STUB_OUT='[]'; STUB_RC=0
check "live/Unmet: no open issues means the Unmet claim is still true" 0 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'still true'

# The standing check #851 asks for: somebody filed a real starter task,
# so the honest answer has changed and nothing else would say so.
STUB_OUT='[{"number":900}]'
check "live/Unmet: a starter task exists again, so Unmet is now false" 1 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'Starter tasks exist again'
check "live/Unmet: and the remedy names all three artifacts" 1 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" "restore the README's link"

# ---- --live, the ways this could go quietly green while blind --------
# This header said TWO and drove two. There was a third, the gate had a
# guard written for it, and the guard was dead: `rc=$?` after
# `mapfile < <(python3 ...)` reads mapfile's status, so an unparseable
# response arrived as zero issues. A section header naming a count is a
# completeness claim, and it was satisfied by not counting the case that
# got away. It names no count now, and every arm below is driven under
# BOTH badge states, because the Unmet arm is the one that fails open.
STUB_OUT='[{"number":534}]'; STUB_RC=1
check "live: an API error cannot see (2, not 0)" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'Cannot see'
check "live/Unmet: an API error cannot see either" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'Cannot see'

# Absent data is not a zero: an empty response must not be read as "the
# label has no issues" — under Met that fails for the wrong reason, and
# under Unmet it would report the claim TRUE while having seen nothing.
STUB_OUT=''; STUB_RC=0
check "live/Met: an EMPTY response is 'cannot see', never 'zero issues'" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'empty response'
check "live/Unmet: an EMPTY response is 'cannot see', never a confirmation" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'empty response'

# A NON-EMPTY response that does not parse. Distinct from the empty
# string above: that one is caught before the parser runs, this one gets
# there. Under Unmet the old code exited 0 saying the claim was still
# true, having read nothing — a false green in the exact arm that is the
# argument for this being a gate and not a snapshot. Under Met it exited
# 1 and accused the repo of having no starter tasks left, on the same
# non-evidence.
STUB_OUT='not json at all'; STUB_RC=0
check "live/Unmet: unparseable is 'cannot see', never a confirmation" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'unparseable issue list'
check "live/Met: unparseable is 'cannot see', never 'no tasks left'" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'unparseable issue list'

# Well-formed JSON of the wrong SHAPE reaches the parser and dies inside
# it, which is a different failure from a syntax error and must not be
# the one case nobody drove.
STUB_OUT='{"number":7}'; STUB_RC=0
check "live/Unmet: JSON of the wrong shape is 'cannot see'" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'unparseable issue list'

# THE CONTROL for all four above. A legitimately empty list is the same
# zero-element array an unparseable response used to produce, so without
# this the fix could have been "exit 2 on everything" and still gone
# green. Unmet + no open issues is the honest state: the gate must PASS.
STUB_OUT='[]'; STUB_RC=0
check "live/Unmet: a genuinely empty list still passes" 0 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" ''

STUB_OUT='[]'; STUB_RC=0
check "live: a missing gh cannot see" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live" "$TMP/no-such-gh")" 'not available'

# ---- usage -----------------------------------------------------------
GFI_README="$TMP/readme.ok" GFI_BADGE="$TMP/badge.met" GFI_LABELS="$LABELS" bash "$GATE" > "$TMP/out" 2>&1
check "no mode is a usage error, not a pass" 2 "$?" 'usage:'
GFI_README="$TMP/readme.ok" GFI_BADGE="$TMP/badge.met" GFI_LABELS="$LABELS" bash "$GATE" --wat > "$TMP/out" 2>&1
check "an unknown mode is a usage error" 2 "$?" 'usage:'

# ---- the committed tree ---------------------------------------------
# The seams above prove the logic; this proves it is pointed at real
# files that actually parse. A gate wired to a path that happens not to
# exist is the way this class of check goes quietly green.
if bash "$GATE" --static > "$TMP/out" 2>&1; then
    ok "the committed README, badge and labels.yml agree"
else
    no "the committed tree fails --static"
    sed 's/^/      /' "$TMP/out" >&2
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
