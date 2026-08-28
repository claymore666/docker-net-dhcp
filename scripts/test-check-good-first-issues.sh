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
#
#   * WHERE the route is, and WHAT counts as one. The first version of
#     the README half searched the whole file for the string, and the
#     only case written for it used a fixture containing no route at
#     all -- so nothing here could ever have caught the shape that
#     mattered: the route present, but not offered by the claim. The
#     README links that route twice, so deleting the entire starter-task
#     bullet stayed green. Those two blind spots are the same one, and
#     the group at "WHERE the route has to be" drives it: the decoy, the
#     renderings that are on the page but not clickable, and every way
#     the claim markers can stop delimiting anything -- against a
#     preservation control for each rendering the review measured as
#     legitimate.
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
#
# THE CLAIM BLOCK. README.md delimits the starter-task claim with a pair
# of HTML comment markers, so the gate judges the ask route where the
# denial is made rather than anywhere in the file (#867 review). Every
# fixture that reaches the route check carries them, because the real
# README does — a fixture written without them would test a README shape
# that does not exist, and the case below that drives their ABSENCE
# would then be indistinguishable from every other case.
BEGIN='<!-- starter-task-claim: begin -->'
END='<!-- starter-task-claim: end -->'
# <block body> -> a whole synthetic README carrying it as the claim.
mk_claim() { printf 'Contributing.\n\n%s\n%s\n%s\n' "$BEGIN" "$1" "$END"; }

mk_readme() { { printf 'Contributing.\n\nPick up a [`good first issue`](%s) to start.\n\n' "$1"
                printf '%s\n- No unclaimed ones; [ask](%s).\n%s\n' "$BEGIN" "$ROUTE" "$END"; } > "$2"; }
# The honest Unmet README: no filter link, but it does name the route.
mk_readme_nolink() { mk_claim "- No starter tasks right now; [ask]($ROUTE) and one will be scoped." > "$1"; }
# The defect review found: the promise is dropped and replaced with an
# invitation to a route the repository does not offer.
mk_readme_noroute() { mk_claim "- No starter tasks right now; open an issue and one will be scoped." > "$1"; }

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
{ printf 'Open a [GitHub issue](%s).\n\n' "$PLAIN"
  mk_claim "- No starter tasks right now; [ask]($ROUTE)."; } > "$TMP/readme.plain"
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

# ---- --static, WHERE the route has to be, and WHAT counts as one -----
# The README half used to be a substring search over the whole file, and
# it failed open. README.md links the same Discussions route twice — once
# from the starter-task claim, once from an unrelated "Questions" bullet
# — so deleting the ENTIRE starter-task bullet left the gate green
# (MEASURED at the pre-fix head: "first task" occurrences 0, rc=0). The
# route is now read as a link TARGET inside the marked claim block, which
# is how the contact-link half above has always been read. Each shape
# that used to pass is driven here, and every legitimate rendering the
# review measured is driven beside it, because "reject everything" would
# score green on the group above.

# THE DECOY, and the one reproduction this group exists for: the claim
# block says nothing about where to ask, while the same route is still
# linked elsewhere in the file.
{ mk_claim "- No starter tasks right now."
  printf -- '- **Questions:** ask in [Discussions](%s).\n' "$ROUTE"; } > "$TMP/readme.decoy"
check "static/Unmet: the route linked only OUTSIDE the claim is not the claim" 1 \
    "$(run --static "$TMP/readme.decoy" "$TMP/badge.unmet")" 'names no ask route'

# On the page but not clickable. Three renderings, one property.
check "static/Unmet: the route only in an HTML comment is not a route" 1 \
    "$(run --static "$(mk_claim "- No starter tasks. <!-- [ask]($ROUTE) -->" > "$TMP/readme.htmlcomment"; echo "$TMP/readme.htmlcomment")" \
        "$TMP/badge.unmet")" 'names no ask route'

{ printf 'Contributing.\n\n%s\n- No starter tasks. Someday:\n\n  ```\n  %s\n  ```\n%s\n' \
    "$BEGIN" "$ROUTE" "$END"; } > "$TMP/readme.fenced"
check "static/Unmet: the route only in a fenced block is not a route" 1 \
    "$(run --static "$TMP/readme.fenced" "$TMP/badge.unmet")" 'names no ask route'

# The URL sits INSIDE the span rather than up against its closing
# backtick, on purpose: abutting it, the collector stops on the backtick
# and the route is rejected because it came out mis-spelled, which is a
# rejection by accident. Mid-span the collector yields the route exactly,
# so the only thing that can reject it is the rule that a code span is
# not a link — which is what this case is for.
mk_claim "- No starter tasks; ask at \`$ROUTE and say what interests you\` if you like." \
    > "$TMP/readme.codespan"
check "static/Unmet: the route only in backticks is not a route" 1 \
    "$(run --static "$TMP/readme.codespan" "$TMP/badge.unmet")" 'names no ask route'

# THREE MORE INERT RENDERINGS, and the reason they are here rather than
# assumed away: the three above were the ones the fix first enumerated,
# and enumerating renderings is spelling-keyed by construction. Each of
# the three below was measured INERT against GitHub's own /markdown
# renderer while the gate still counted it as a route — a fail-open on
# the arm that exists to keep the invitation honest.

# A code span may cross LINES. The single-line pattern this replaces
# could not see one, so the page rendered code and the gate read a link.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks; ask at `see\n  %s here` if you like.\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.mlcodespan"
check "static/Unmet: the route only in a MULTI-LINE code span is not a route" 1 \
    "$(run --static "$TMP/readme.mlcodespan" "$TMP/badge.unmet")" 'names no ask route'

# An UNTERMINATED <!-- runs to the next --> on the page, which is the end
# marker's own, so everything after it in the block is comment.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks.\n  <!-- disabled:\n  [ask](%s)\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.untermcomment"
check "static/Unmet: the route after an UNTERMINATED comment is not a route" 1 \
    "$(run --static "$TMP/readme.untermcomment" "$TMP/badge.unmet")" 'names no ask route'

# A link reference definition nothing references renders as NOTHING.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks; ask somewhere.\n\n  [ar]: %s\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.deadref"
check "static/Unmet: an UNREFERENCED link definition is not a route" 1 \
    "$(run --static "$TMP/readme.deadref" "$TMP/badge.unmet")" 'names no ask route'

# PRESERVATION CONTROLS for the three above. Without these, "strip more"
# would score green by refusing everything, and two of the three strips
# have a cheap wrong version that these are what catch:

# The SAME definition, referenced. GitHub renders a real link, so the gate
# must accept it — the half that stops the definition strip from being
# "delete any line with a colon in it".
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks; [ask here][ar] and one will be scoped.\n\n  [ar]: %s\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.usedref"
check "static/Unmet: a REFERENCED link definition is a route" 0 \
    "$(run --static "$TMP/readme.usedref" "$TMP/badge.unmet")" ''

# The same BYTES mid-paragraph are NOT a definition — a definition has to
# start a block. GitHub renders this one as text and autolinks the bare
# URL, so it IS clickable and the gate must accept it. Measured against
# the /markdown renderer, both shapes, because they differ only in a blank
# line: this is what stops the definition strip from keying on the bracket
# syntax alone and refusing a route a newcomer really can follow.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks; ask where it says\n  [ar]: %s\n  and one will be scoped.\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.midref"
check "static/Unmet: a definition-shaped line MID-PARAGRAPH is still a route" 0 \
    "$(run --static "$TMP/readme.midref" "$TMP/badge.unmet")" ''

# PINNED KNOWN OVER-REFUSAL — this asserts the answer the gate gives, and
# that answer is WRONG. A link reference definition is scoped to the whole
# DOCUMENT; this gate reads only the bytes between the markers. So a
# reference-style link inside the claim block whose definition sits later in
# the README renders as a real clickable link on GitHub and the gate still
# refuses it.
#
# MEASURED against GitHub's own /markdown API, the same oracle the two
# controls above are scored with: this exact fixture renders
# <a href="...discussions/new?category=q-a">. The gate exits 1.
#
# It is pinned rather than fixed, deliberately, and the direction is why.
# Every other bound this gate has is fail-OPEN — it accepts something it
# should refuse — and those are the dangerous ones because the green says
# nothing happened. This one is fail-CLOSED: it goes red, and the error
# message names the exact URL it wanted between the markers, so an author
# who writes this shape is told what to write instead and is out of it in a
# minute. Widening the block-scoped read to resolve definitions from the
# whole document is the real fix; it is a widening of the ACCEPT set on a
# gate whose whole job is refusing, so it wants its own preservation
# controls (a definition inside a later fenced block must not count) and it
# is not worth carrying into a docs PR at review round 3.
#
# If you are here because this case went red: you probably fixed it. Good —
# delete this case, keep the two controls above, and add a control for a
# definition that appears only inside a fence.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks; [ask][dq] and one will be scoped.\n'
  printf '%s\n\nLater prose.\n\n[dq]: %s\n' "$END" "$ROUTE"; } > "$TMP/readme.docref"
check "static/Unmet: a definition OUTSIDE the block is refused (known over-refusal)" 1 \
    "$(run --static "$TMP/readme.docref" "$TMP/badge.unmet")" 'names no ask route'

# An UNPAIRED backtick is literal text, not an open code span. This is the
# over-refusal a naive multi-line pattern introduces: match lazily across
# newlines and one stray backtick swallows the real link below it. Code
# spans are paired by RUN LENGTH so that cannot happen.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks (about 3` of them); [ask](%s).\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.straytick"
check "static/Unmet: a stray backtick does not swallow the real link" 0 \
    "$(run --static "$TMP/readme.straytick" "$TMP/badge.unmet")" ''

# PINNED BOUND, not an assertion of correctness. A route reachable only
# from an INDENTED (four-space) code block is inert on the page and this
# gate still counts it, so this case asserts today's WRONG answer (0).
# Closing it needs the enclosing list's content indent — a real CommonMark
# parser, which the runner image does not ship — and every indent rule
# guessed at instead refuses a README that merely reflows its bullet. The
# gate header states the same bound. If this case ever starts failing,
# the gate got BETTER: delete the case, do not weaken the gate.
{ printf 'Contributing.\n\n%s\n' "$BEGIN"
  printf -- '- No starter tasks. Someday:\n\n      %s\n' "$ROUTE"
  printf '%s\n' "$END"; } > "$TMP/readme.indentcode"
check "static/Unmet: KNOWN BOUND — an indented code block is still read as a route" 0 \
    "$(run --static "$TMP/readme.indentcode" "$TMP/badge.unmet")" ''

# A DIFFERENT route that merely CONTAINS the published one. The substring
# check accepted this; equality on the extracted target does not.
mk_claim "- No starter tasks; [ask](${ROUTE}-archive)." > "$TMP/readme.prefix"
check "static/Unmet: a longer route containing ours is not ours" 1 \
    "$(run --static "$TMP/readme.prefix" "$TMP/badge.unmet")" 'names no ask route'

# An EMPTY claim block is red, not vacuously green: the gate found the
# claim and the claim names nowhere to go.
printf 'Contributing.\n\n%s\n%s\n' "$BEGIN" "$END" > "$TMP/readme.emptyclaim"
check "static/Unmet: an EMPTY claim block names no route (1, not 0)" 1 \
    "$(run --static "$TMP/readme.emptyclaim" "$TMP/badge.unmet")" 'names no ask route'

# NOT BEING ABLE TO FIND THE CLAIM IS A REFUSAL, NEVER A PASS. Four ways
# the markers stop delimiting anything, each exit 2 — the same "cannot
# see" a renamed label gets. Without these, scoping to a region would
# have introduced a second way to fail open: no markers, no region, no
# judgement.
printf 'Contributing.\n\n- No starter tasks right now; [ask](%s).\n' "$ROUTE" > "$TMP/readme.nomarkers"
check "static/Unmet: no claim markers at all cannot see (2, not 0)" 2 \
    "$(run --static "$TMP/readme.nomarkers" "$TMP/badge.unmet")" 'Found 0 begin and 0 end'

{ mk_claim "- No starter tasks right now; [ask]($ROUTE)."
  mk_claim "- And again, somewhere else."; } > "$TMP/readme.twomarkers"
check "static/Unmet: a claim marked twice cannot see" 2 \
    "$(run --static "$TMP/readme.twomarkers" "$TMP/badge.unmet")" 'Found 2 begin and 2 end'

printf 'Contributing.\n\n%s\n- No starter tasks; [ask](%s).\n' "$BEGIN" "$ROUTE" > "$TMP/readme.unclosed"
check "static/Unmet: an unclosed claim marker cannot see" 2 \
    "$(run --static "$TMP/readme.unclosed" "$TMP/badge.unmet")" 'Found 1 begin and 0 end'

printf 'Contributing.\n\n%s\n- No starter tasks; [ask](%s).\n%s\n' "$END" "$ROUTE" "$BEGIN" \
    > "$TMP/readme.crossed"
check "static/Unmet: markers in the wrong order cannot see" 2 \
    "$(run --static "$TMP/readme.crossed" "$TMP/badge.unmet")" 'out of order'

# THE PRESERVATION CONTROLS for this whole group. Every rendering the
# review measured as legitimate must still pass, or the fix has simply
# traded a gate that accepts everything for one that accepts nothing.
mk_claim "- No starter tasks right now; [ask in Discussions](<$ROUTE>) and one will be scoped." \
    > "$TMP/readme.angle"
check "static/Unmet: an angle-bracketed link target is still a route" 0 \
    "$(run --static "$TMP/readme.angle" "$TMP/badge.unmet")" 'small_tasks is Unmet'

mk_claim "- No starter tasks right now; [ask]($REPO/discussions/new?category=q%2Da)." \
    > "$TMP/readme.pct"
check "static/Unmet: a percent-encoded route is still a route" 0 \
    "$(run --static "$TMP/readme.pct" "$TMP/badge.unmet")" 'small_tasks is Unmet'

mk_claim "- No starter tasks right now; [ask]($REPO/Discussions/New?category=Q-A)." \
    > "$TMP/readme.case"
check "static/Unmet: a differently-cased route is still a route" 0 \
    "$(run --static "$TMP/readme.case" "$TMP/badge.unmet")" 'small_tasks is Unmet'

# GitHub autolinks a bare URL, and trailing sentence punctuation is not
# part of the target, so this is a real route and must not go red.
mk_claim "- No starter tasks right now; ask at $ROUTE." > "$TMP/readme.bare"
check "static/Unmet: a bare autolinked URL ending a sentence is a route" 0 \
    "$(run --static "$TMP/readme.bare" "$TMP/badge.unmet")" 'small_tasks is Unmet'

# The markers are matched with their surrounding whitespace tolerated, so
# a reflow of the README does not turn into a false refusal.
printf 'Contributing.\n\n  %s  \n- No starter tasks; [ask](%s).\n\t%s\n' \
    "$BEGIN" "$ROUTE" "$END" > "$TMP/readme.indented"
check "static/Unmet: indented markers still delimit the claim" 0 \
    "$(run --static "$TMP/readme.indented" "$TMP/badge.unmet")" 'small_tasks is Unmet'

# A route whose target contains a character the BARE url form stops at.
# An angle-bracket target is the only way such a link stays clickable,
# and a collector that ignored it would read the route as its truncated
# prefix and turn a perfectly good README red. Driven through the seam
# because today's route needs no brackets — which is exactly why this
# collection would otherwise be defence nothing exercises.
ROUTE_Q="$REPO/discussions/new?category=q-a&title=\"first-task\""
ROUTES_Q="$TMP/config-q.yml"
cat > "$ROUTES_Q" <<YML
blank_issues_enabled: false
contact_links:
  - name: Ask for a first task
    url: $ROUTE_Q
YML
mk_claim "- No starter tasks right now; [ask](<$ROUTE_Q>)." > "$TMP/readme.qtarget"
ROUTE_OVERRIDE="$ROUTE_Q"
check "static/Unmet: an angle-bracket target is the whole route, not its prefix" 0 \
    "$(run --static "$TMP/readme.qtarget" "$TMP/badge.unmet" "$TMP/gh" "$LABELS" "$ROUTES_Q")" \
    'small_tasks is Unmet'
unset ROUTE_OVERRIDE

# And the route must still be found when the claim block ALSO carries an
# inert copy — stripping the inert renderings must not strip the link.
mk_claim "- No starter tasks. <!-- moved --> Ask at \`somewhere\`: [ask]($ROUTE)." \
    > "$TMP/readme.mixed"
check "static/Unmet: an inert copy beside a real link does not hide it" 0 \
    "$(run --static "$TMP/readme.mixed" "$TMP/badge.unmet")" 'small_tasks is Unmet'

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

# A README path that exists and is readable but yields no text. `[ -r ]`
# is true for a directory, so this gets PAST the guard above and has to
# be refused by the read itself. Driven as a directory rather than with
# chmod 000, which proves nothing when the suite runs as root.
mkdir -p "$TMP/readme-is-a-dir"
check "static: a README that cannot be read as a file cannot see" 2 \
    "$(run --static "$TMP/readme-is-a-dir" "$TMP/badge.unmet")" 'could not be read'

# The anchor. If the declaration no longer names the label this gate
# watches, the gate does not know what to count and must say so — not
# quietly count a label that no longer exists.
check "static: the label missing from labels.yml cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS_RENAMED")" \
    'is not declared in'
check "static: a missing labels.yml cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$TMP/no-labels.yml")" \
    'missing or unreadable'

# THE ANCHOR IS A DECLARATION, NOT A MENTION, and until #867 nothing said
# so. Every fixture above renames the label out of the file ENTIRELY, so a
# gate that had degraded from `grep -qxF -- "- name: $LABEL"` to a bare
# substring search would have passed all of them: with the phrase gone,
# both spellings answer "not found" and agree by accident.
#
# MEASURED by mutation: loosening that grep to `grep -qF -- "$LABEL"`
# survived the whole suite. The witness below separates them — a rename
# that leaves the old phrase behind in a neighbouring description is
# exactly how a taxonomy edit really looks, and under the loosened
# spelling the gate would go on counting a label the tracker no longer
# has.
LABELS_RENAMED_DECOY="$TMP/labels-renamed-decoy.yml"
cat > "$LABELS_RENAMED_DECOY" <<'YML'
- name: bug
  role: type
  description: Something is broken

- name: starter task
  role: status
  description: Replaces the old good first issue label — no deep context needed
YML
check "static: a rename leaving the old name in prose still cannot see" 2 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS_RENAMED_DECOY")" \
    'is not declared in'

# The preservation control for the case above: the guard fails in one
# direction, so drive the other. A decoy mention BESIDE a real
# declaration must still pass — a gate that started refusing labels.yml
# because some other entry's description quotes the label name would be
# the same defect pointing the expensive way.
LABELS_DECOY_OK="$TMP/labels-decoy-ok.yml"
cat > "$LABELS_DECOY_OK" <<'YML'
- name: bug
  role: type
  description: Something is broken — not a good first issue

- name: good first issue
  role: status
  description: Self-contained starter task — no deep project context needed
YML
check "static: a decoy mention beside a real declaration still passes" 0 \
    "$(run --static "$TMP/readme.silent" "$TMP/badge.unmet" "$TMP/gh" "$LABELS_DECOY_OK")" \
    'small_tasks is Unmet'

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
