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
#
#   * The ASK ROUTE. The Unmet answer is "no tasks, ask here", so it
#     names a route, and a named route that does not exist is the very
#     defect this gate exists to refuse — reintroduced one sentence
#     later. Both halves are driven: the README and the contact-link
#     declaration must name the SAME route (static), and the repository
#     must still offer it (live). Each has a preservation control beside
#     it, because "require a route everywhere" and "reject every route"
#     would both go green without them.
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

# The ask route the Unmet answer names, and the contact-link declaration
# the gate anchors it to. Kept in step with the constant in the gate by
# the committed-tree case at the bottom, which runs against the real
# files rather than these.
ROUTE="$REPO/discussions/new?category=q-a"
ROUTES="$TMP/config.yml"
cat > "$ROUTES" <<YML
blank_issues_enabled: false
contact_links:
  - name: Report a security vulnerability (private)
    url: $REPO/security/advisories/new
    about: >-
      Do NOT open a public issue for anything exploitable.
      Report it privately.
  - name: Ask for a first task, or ask anything else
    url: $ROUTE
    about: Say what interests you and a first task will be scoped against it.
YML

# The same file with the ask route withdrawn — the security link stays,
# so this is "the route was retired", not "contact links were deleted".
ROUTES_NOROUTE="$TMP/config-noroute.yml"
cat > "$ROUTES_NOROUTE" <<YML
blank_issues_enabled: false
contact_links:
  - name: Report a security vulnerability (private)
    url: $REPO/security/advisories/new
    about: >-
      Do NOT open a public issue for anything exploitable.
      Report it privately.
YML

# The route named only INSIDE a folded about: block. It is prose there,
# not a published contact link — a newcomer cannot click it. A parser
# that scanned the file for the string would accept this.
ROUTES_PROSE="$TMP/config-prose.yml"
cat > "$ROUTES_PROSE" <<YML
blank_issues_enabled: false
contact_links:
  - name: Report a security vulnerability (private)
    url: $REPO/security/advisories/new
    about: >-
      Report privately. If you want a first task instead, see
      url: $ROUTE
YML

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

# Every README fixture carries the ask route except the one written to
# lack it, so that a case about the LABEL detector fails for a label
# reason and not for a missing route.
mk_readme() { printf 'Contributing.\n\nPick up a [`good first issue`](%s) to start, or [ask](%s).\n' "$1" "$ROUTE" > "$2"; }
# The honest Unmet README: no filter link, but it does name the route.
mk_readme_nolink() { printf 'Contributing.\n\nNo starter tasks right now; [ask](%s) and one will be scoped.\n' "$ROUTE" > "$1"; }
# The defect review found: the promise is dropped and replaced with an
# invitation to a route the repository does not offer.
mk_readme_noroute() { printf 'Contributing.\n\nNo starter tasks right now; open an issue and one will be scoped.\n' > "$1"; }

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
#
# It dispatches on the subcommand because `issue list` and `api graphql`
# are two different questions: one stub answer for both would make every
# ask-route case secretly test the issue-list fixture.
cat > "$TMP/gh" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
    api)
        [ "${STUB_API_RC:-0}" -ne 0 ] && exit "${STUB_API_RC}"
        printf '%s' "${STUB_API_OUT-$STUB_API_HEALTHY}"
        ;;
    *)
        [ "${STUB_RC:-0}" -ne 0 ] && exit "${STUB_RC}"
        printf '%s' "${STUB_OUT-[]}"
        ;;
esac
EOF
chmod +x "$TMP/gh"

# The default answer is the HEALTHY one, so every case that is not about
# the route keeps testing what it was written to test. It comes from a
# file rather than a literal so that shellcheck reads it as data and not
# as a command line waiting to be word-split.
cat > "$TMP/api-healthy.json" <<'JSON'
{"data":{"repository":{"hasDiscussionsEnabled":true,"discussionCategories":{"nodes":[{"slug":"general"},{"slug":"q-a"}]}}}}
JSON
STUB_API_HEALTHY=$(cat "$TMP/api-healthy.json")
STUB_OUT='[]'; STUB_RC=0
STUB_API_OUT="$STUB_API_HEALTHY"; STUB_API_RC=0
export STUB_OUT STUB_RC STUB_API_HEALTHY STUB_API_OUT STUB_API_RC

# $ROUTE_OVERRIDE, when set, replaces the ask route the gate would use.
# Empty means "the constant in the gate", which is what production uses.
run() { # <mode> <readme> <badge> [gh] [labels] [routes]
    GFI_README="$2" GFI_BADGE="$3" GFI_GH="${4:-$TMP/gh}" GFI_LABELS="${5:-$LABELS}" \
        GFI_ROUTES="${6:-$ROUTES}" GFI_ASK_ROUTE="${ROUTE_OVERRIDE-}" \
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
mk_readme_noroute "$TMP/readme.noroute"
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
printf 'Open a [GitHub issue](%s), or [ask](%s).\n' "$PLAIN" "$ROUTE" > "$TMP/readme.plain"
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

# ---- --static, the ask route ----------------------------------------
# The Unmet answer is not "there are no starter tasks", it is "there are
# none, ask here". Found in review (#851): the first draft dropped the
# false promise and replaced it with an invitation to open an issue,
# while blank issues are disabled and both forms stamp a type label that
# is wrong for the request. So the route is an observable too.
check "static/Unmet: an Unmet README naming NO published route is red" 1 \
    "$(run --static "$TMP/readme.noroute" "$TMP/badge.unmet")" 'names no ask route'

# The route withdrawn from the declaration while the README still sends
# people there. Cannot-see, not false: the gate no longer knows where a
# newcomer is told to go, exactly like a renamed label.
check "static/Unmet: a route the repo does not publish cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS" "$ROUTES_NOROUTE")" \
    'not published as a contact link'

check "static/Unmet: a missing contact-link file cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS" "$TMP/no-config.yml")" \
    'missing or unreadable'

# The route mentioned only inside a folded about: block is prose, not a
# published link. A check that grepped the file for the string would
# accept a DESCRIPTION of a route as a route.
check "static/Unmet: a route named only in prose is not published" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS" "$ROUTES_PROSE")" \
    'not published as a contact link'

# THE PRESERVATION CONTROL for the group above. Met sends a newcomer to
# real tasks, so it makes no ask-route promise and must not be judged
# against one — without this, "require the route in every arm" would
# score green.
check "static/Met: the Met arm makes no route promise and is not judged on one" 0 \
    "$(run --static "$TMP/readme.ok" "$TMP/badge.met" "$TMP/gh" "$LABELS" "$ROUTES_NOROUTE")" \
    'small_tasks is Met'

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

# Well-formed JSON of the wrong SHAPE. TWO fixtures, because the shape
# splits on emptiness and only one half raises: '{"number":7}' iterates
# to a key and dies inside the parser, while '{}' iterates to NOTHING and
# exits 0 having printed nothing — indistinguishable from "[]" unless the
# shape is checked before the loop. The first version of this case drove
# only the non-empty one, which is a fixture that cannot exercise the
# empty instance of the very class it was written for.
STUB_OUT='{"number":7}'; STUB_RC=0
check "live/Unmet: JSON of the wrong shape is 'cannot see'" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'unparseable issue list'

STUB_OUT='{}'; STUB_RC=0
check "live/Unmet: an EMPTY object is a wrong shape, not zero issues" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'expected a JSON array'
check "live/Met: an EMPTY object is a wrong shape, not 'no tasks left'" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'expected a JSON array'

STUB_OUT='null'; STUB_RC=0
check "live/Unmet: a bare null is a wrong shape too" 2 \
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

# ---- --live, the ask route -------------------------------------------
# Static binds the README to the declaration. Neither can see the
# repository turning Discussions off or dropping the category, which
# unlinks the route without touching the tree — the same silent decay as
# the label's open count, so it is checked in the same place.
STUB_OUT='[]'; STUB_RC=0

STUB_API_OUT='{"data":{"repository":{"hasDiscussionsEnabled":false,"discussionCategories":{"nodes":[{"slug":"q-a"}]}}}}'
check "live/Unmet: Discussions switched off makes the invitation false" 1 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'has Discussions turned off'

STUB_API_OUT='{"data":{"repository":{"hasDiscussionsEnabled":true,"discussionCategories":{"nodes":[{"slug":"general"},{"slug":"ideas"}]}}}}'
check "live/Unmet: the category deleted makes the invitation false" 1 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'no discussion category'

# The 2s again, on the new arm. A live check that cannot see the route
# must say so; reading "could not ask" as "the route is fine" would be
# the false green this whole gate exists to refuse, in a fourth place.
STUB_API_RC=1; STUB_API_OUT="$STUB_API_HEALTHY"
check "live/Unmet: an API error on the route cannot see (2, not 0)" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'could not ask'

STUB_API_RC=0; STUB_API_OUT=''
check "live/Unmet: an EMPTY route response cannot see, never a confirmation" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'empty response asking'

STUB_API_OUT='not json'
check "live/Unmet: an unparseable route answer cannot see" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'unparseable answer'

STUB_API_OUT='[]'
check "live/Unmet: a route answer of the wrong SHAPE cannot see" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'expected a JSON object'

STUB_API_OUT='{"data":{"repository":null}}'
check "live/Unmet: a route answer naming no repository cannot see" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'names no repository'

# A ROUTE KIND THIS CHECK DOES NOT UNDERSTAND IS REFUSED, NOT SKIPPED.
# The live half knows how to verify one shape of route. If the route is
# changed to another, the honest answer is "I cannot look" — a live check
# that quietly passes on a route it never examined is the vacuity the
# whole gate exists to refuse, and the comment saying so was prose until
# these two cases existed. Both halves of the shape are driven: a host
# this check knows nothing about, and a github.com URL that is not a
# new-discussion one.
ROUTE_OVERRIDE='https://example.invalid/ask-us-anything'
check "live/Unmet: an unknown route KIND is refused, not skipped" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'does not know how to verify'

ROUTE_OVERRIDE="$REPO/issues/new"
check "live/Unmet: a github URL that is not a new-discussion route is refused" 2 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'does not know how to verify'

# The control beside them: the route kind it DOES understand must still
# be verified rather than refused, or "refuse everything" scores green.
unset ROUTE_OVERRIDE

# THE PRESERVATION CONTROLS. Without the first, "exit 2 on every route
# answer" would score green; without the second, "probe the route in
# every arm" would, and the Met arm would go red for a route it never
# promised.
STUB_API_OUT="$STUB_API_HEALTHY"
check "live/Unmet: a route that is still offered keeps the claim true" 0 \
    "$(run --live "$TMP/readme.silent" "$TMP/badge.unmet")" 'the ask route is still offered'

STUB_API_RC=1
STUB_OUT='[{"number":534},{"number":535},{"number":536}]'
check "live/Met: the Met arm does not probe the ask route" 0 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'every cited issue'
STUB_API_RC=0; STUB_API_OUT="$STUB_API_HEALTHY"; STUB_OUT='[]'

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
