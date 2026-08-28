#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Keep the "good first issue" promise honest (#537, #851; follow-up to #455).
#
# Two public artifacts make claims about live tracker state:
#
#   * .bestpractices.json answers `small_tasks` Met or Unmet, and when it
#     answers Met it justifies that by naming a label filter and issues.
#   * README.md's Contributing section links the same filter — or, when
#     there is nothing behind the promise, deliberately does not.
#
# Both stop being true silently, and in BOTH directions. When the seeded
# starter tasks are picked up and nothing replaces them, the filter
# returns an empty list, the README sends a newcomer to an empty page,
# and the badge asserts something false. That happened: the claim was
# false for four days (#851). But the reverse decays just as quietly —
# once the answer is honestly Unmet, the day somebody files a genuine
# starter task the project is under-claiming a criterion it now meets,
# and nothing says so either.
#
# THE PROPERTY, therefore, is a biconditional across three observables:
#
#   * .bestpractices.json's small_tasks_status  (Met | Unmet)
#   * whether README.md carries a starter-task filter URL at all
#   * how many OPEN issues actually carry the label
#
# No two of them may disagree. Met means the README links the filter and
# the label has open issues; Unmet means the README makes no promise, the
# justification cites no tasks, and the label has none. Anything else is
# red. That is what makes dropping the claim to honest reversible rather
# than permanent: the flip back to Met is enforced, not remembered.
#
# AND THE UNMET ANSWER CARRIES A FOURTH OBSERVABLE, because it is not
# "there are no starter tasks" but "there are none, ask here". Dropping
# the false promise and replacing it with a route that does not exist
# would be the same defect one sentence later, and the first draft of
# the #851 fix did exactly that: it told a newcomer to open an issue,
# while
# .github/ISSUE_TEMPLATE/config.yml disables blank issues and both forms
# auto-apply a type label (`bug`, `enhancement`) that is wrong for "I
# would like a first task". So the route is checked too:
#
#   * --static  the route named below must be DECLARED as a contact link
#               in .github/ISSUE_TEMPLATE/config.yml, and README.md must
#               send the newcomer to that same one.
#   * --live    the repository must actually still offer it.
#
# A README sentence naming a route, with nothing that goes red when the
# route disappears, is precisely the shape this gate exists to refuse.
#
# WHERE THE README HALF LOOKS, and why it is not the whole file (#867
# review). "There are none, ASK HERE" is ONE claim. A search of the whole
# README cannot tell that claim apart from any other mention of the same
# URL, and this README has one: the Contributing section links the same
# Discussions route from its "Questions" bullet for an unrelated reason.
# MEASURED before this was scoped: deleting the ENTIRE nine-line
# starter-task bullet left the gate exiting 0 and printing that the route
# "is published as a contact link" -- about a README that no longer
# invited anyone to ask for a first task at all. So README.md delimits
# the claim with a pair of HTML comment markers,
#
#     <!-- starter-task-claim: begin -->
#     ... the bullet that denies starter tasks and says where to ask ...
#     <!-- starter-task-claim: end -->
#
# and the route is judged inside them, as a clickable link TARGET rather
# than as a byte sequence -- the way the contact-link declaration has
# always been judged. Markers rather than a heading or a phrase because
# headings get renamed and sentences get reworded, and either would make
# this refuse a README that is entirely honest. Marker trouble (absent,
# duplicated, crossed) is exit 2, never a pass; an EMPTY block is exit 1,
# because the gate found the claim and the claim names nowhere to go.
#
# TWO CHECKS, DELIBERATELY SPLIT BY WHAT THEY NEED (see #537, which asks
# for this decision to be made before the code is written).
#
#   --static  Needs no network. Binds the badge status to the README:
#             Met requires both artifacts to carry the same filter URL,
#             so a label rename cannot leave one of them pointing at a
#             query that silently matches nothing; Unmet requires the
#             README to carry no filter URL and the justification to
#             cite no issue numbers. Safe at PR time: it is a property
#             of the tree, so only the commit being tested can break it.
#
#   --live    Needs the tracker API. Binds the badge status to the open
#             count, in both directions. This is NOT safe at PR time — an
#             unrelated PR would go red because somebody closed the last
#             starter issue, charging the cost to whoever pushed next. It
#             runs on a schedule instead.
#
# WHERE THE LABEL NAME COMES FROM, and why it changed (#851). This gate
# used to decode the label out of the promise URL. That works only while
# a promise exists: the moment the honest answer is Unmet there is no URL
# left to decode, and the check would go blind in precisely the state it
# exists to police. So the label is named here and must be DECLARED in
# .github/labels.yml — the in-repo taxonomy #715 added, which is itself
# checked against the tracker in both directions. A rename now goes red
# at PR time against a declaration instead of being caught only if it
# happened to leave a URL behind to compare against.
#
# THE COUPLING TO check-label-taxonomy.sh IS DELIBERATE. If that gate
# goes red because the declaration and the tracker disagree, this one
# goes red too, from one cause. That is by construction and not a bug to
# be "fixed" by giving this gate its own copy of the label name: two
# gates making claims about the same label must fail together, and the
# alternative is one of them working from a name nothing verified.
#
# DECODE, THEN JUDGE — the Unmet arm's detector is the dangerous one.
# It has to answer "does the README link anywhere that lists issues
# carrying this label", and a detector keyed on ONE rendering of that URL
# would let every other rendering through while the promise stayed on the
# page and stayed clickable — the gate reproducing its own silence, which
# is the shape that has already cost this project a check. So every
# github.com URL in either artifact is percent-decoded, '+'-decoded and
# case-folded FIRST, and the label component is then compared as a value.
# The renderings that survive that normalisation to the same thing, and
# are each driven as a case in the self-test:
#
#     issues?q=label%3A%22good+first+issue%22   (what we shipped)
#     <issues?q=label:"good first issue">       (unencoded, in a
#                                                markdown angle-bracket
#                                                target — the only way a
#                                                link with literal spaces
#                                                stays clickable)
#     issues?q=is:open+label:"Good First Issue" (different case)
#     issues?labels=good+first+issue            (the legacy query param)
#     labels/good%20first%20issue               (the label page)
#     issues/labels/good%20first%20issue        (the legacy label page)
#
# The set is complete in the sense that matters: GitHub reaches a
# label-filtered listing through a `label`/`labels` query term or a
# `/labels/<name>` path, and normalisation collapses the encodings of
# each. What it cannot see is a URL shortener or a redirect, which needs
# the network — named here so the limit is stated rather than implied.
#
# One near-miss is deliberately NOT treated as a match: `label:good-first-
# issue`, unquoted and hyphenated, is a filter on a label of that name,
# which is not this one — GitHub needs the quoted form for a label
# containing spaces. So it is read as a listing for a DIFFERENT label,
# and that is the honest reading: as a starter-task promise it is already
# broken, and the Met arm says so by name rather than accepting it.
#
# EXIT CODES ARE PART OF THE CONTRACT:
#   0  the claims hold
#   1  a claim is FALSE — the artifacts and the tracker disagree
#   2  the check COULD NOT SEE (missing file, unparseable, unknown
#      status value, API unreachable). Never silently 0: a gate that
#      cannot look must say so, or its green means nothing.
#
# Env seams, for the self-test:
#   GFI_README  path to README.md
#   GFI_BADGE   path to .bestpractices.json
#   GFI_LABELS  path to .github/labels.yml
#   GFI_ROUTES  path to .github/ISSUE_TEMPLATE/config.yml
#   GFI_ASK_ROUTE  the ask route, so the "this check does not understand
#               that kind of route" refusal can be driven. Empty falls
#               back to the constant below, which is what production uses.
#   GFI_GH      the gh command (default: gh), so --live can be driven
#               against a stub without touching the network.

set -uo pipefail

MODE="${1:-}"
case "$MODE" in
    --static|--live) ;;
    *) echo "usage: $0 --static|--live" >&2; exit 2 ;;
esac

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
README="${GFI_README:-$ROOT/README.md}"
BADGE="${GFI_BADGE:-$ROOT/.bestpractices.json}"
LABELS="${GFI_LABELS:-$ROOT/.github/labels.yml}"
ROUTES="${GFI_ROUTES:-$ROOT/.github/ISSUE_TEMPLATE/config.yml}"
GH="${GFI_GH:-gh}"

# The label this gate watches. See "WHERE THE LABEL NAME COMES FROM".
LABEL='good first issue'

# The route the Unmet answer sends a newcomer to. Named here and anchored
# to the contact-link declaration for the same reason the label name is
# anchored to labels.yml: a gate that derives the route from the promise
# it is guarding cannot fail when the promise is the thing that is wrong.
# Deriving it from the README would mean that deleting the sentence — the
# one failure worth catching — blinds the check.
#
# It is a Discussions route rather than a third issue form on purpose.
# A form would need a type label, the taxonomy in .github/labels.yml
# declares none that fits "I would like a first task", and adding one is
# a taxonomy change with its own gate. Discussions is already enabled on
# the repository, the Q&A category is answerable, and config.yml already
# publishes a contact link for security reports, so the mechanism is
# present and proven here.
ASK_ROUTE="${GFI_ASK_ROUTE:-https://github.com/claymore666/docker-net-dhcp/discussions/new?category=q-a}"

for f in "$README" "$BADGE" "$LABELS"; do
    [ -r "$f" ] || { echo "::error title=Cannot see::$f is missing or unreadable" >&2; exit 2; }
done

# The declaration is the anchor, so a rename that reaches labels.yml but
# not this gate is loud rather than a silently empty query.
if ! grep -qxF -- "- name: $LABEL" "$LABELS"; then
    echo "::error title=Cannot see::the starter-task label \"$LABEL\" is not declared in" \
         "$LABELS, so this gate does not know what to count. If the label was renamed," \
         "rename it here too; if it was retired, this check retires with it." >&2
    exit 2
fi

# READ BOTH ARTIFACTS ONCE, decode, and hand back values rather than
# renderings. Both are judged against each other, and a second parse
# could disagree with the first.
#
# Eight lines out, in order: the status; the issue numbers the
# justification cites; the README URL that resolves to a listing on OUR
# label; the same for the badge; then, for the rename diagnostic, the
# first label listing each artifact links at all, together with the label
# name it decodes to. An empty line means "none found".
read_artifacts=$(python3 - "$README" "$BADGE" "$LABEL" <<'PY'
import json, re, sys, urllib.parse

readme_path, badge_path, label = sys.argv[1], sys.argv[2], sys.argv[3]

# A bare URL ends at the first space, quote or closing bracket. Markdown
# also allows an angle-bracketed target, which is how a link containing
# literal spaces stays clickable — so both forms are collected, longest
# first, or `[x](<...label:"good first issue">)` would be read as the
# truncated prefix and its promise would go unseen.
URL = re.compile(r'https://github\.com/[^\s)"\]>]+')
BRACKETED = re.compile(r'<(https://github\.com/[^>]+)>')

def norm(u):
    """Percent- and plus-decode, then case-fold: compare values, not renderings."""
    return urllib.parse.unquote_plus(u).casefold()

# A label listing is reached through a `label`/`labels` query term or a
# `/labels/<name>` path. Each pattern captures the label VALUE. The
# quoted form comes first so `label:"good first issue"` yields the whole
# phrase rather than stopping at the first space.
TERMS = [
    r'label:"(?P<v>[^"]*)"',
    r'[?&]labels?=(?P<v>[^&#]*)',
    r'/labels/(?P<v>[^/?#]*)',
    r'label:(?P<v>[^\s&#"]+)',
]

def label_of(url):
    """The label a URL lists on, or None if it is not a label listing."""
    n = norm(url)
    for pat in TERMS:
        m = re.search(pat, n)
        if m:
            return m.group("v").strip()
    return None

def scan(text):
    """(url listing OUR label, first label-listing url, its label)."""
    ours = first = first_label = ""
    want = label.casefold()
    for u in BRACKETED.findall(text) + URL.findall(text):
        lab = label_of(u)
        if lab is None:
            continue
        if not first:
            first, first_label = u, lab
        if not ours and lab == want:
            ours = u
    return ours, first, first_label

try:
    readme_text = open(readme_path, encoding="utf-8").read()
except Exception as e:
    print(f"UNREADABLE {e}", file=sys.stderr)
    sys.exit(2)
try:
    d = json.load(open(badge_path, encoding="utf-8"))
except Exception as e:
    print(f"UNPARSEABLE {e}", file=sys.stderr)
    sys.exit(2)
if "small_tasks_status" not in d:
    print("small_tasks_status is absent", file=sys.stderr)
    sys.exit(2)

status = d["small_tasks_status"]
j = d.get("small_tasks_justification", "") or ""

# Strip URLs before hunting issue numbers: a filter query carries none
# today, but a future one might, and a citation is a claim while a query
# parameter is not.
cited = sorted({m for m in re.findall(r'#(\d{1,7})\b', re.sub(r'https://\S+', ' ', j))}, key=int)

r_ours, r_any, r_any_label = scan(readme_text)
b_ours, b_any, b_any_label = scan(j)

for line in (status if isinstance(status, str) else repr(status),
             " ".join(cited),
             r_ours, b_ours, r_any, r_any_label, b_any, b_any_label):
    print(line)
PY
) || { echo "::error title=Cannot see::$README or $BADGE could not be read" >&2; exit 2; }

{
    read -r badge_status
    read -r badge_cited
    read -r readme_promise
    read -r badge_promise
    read -r readme_any_url
    read -r readme_any_label
    read -r badge_any_url
    read -r badge_any_label
} <<< "$read_artifacts"

# An unknown status is not a verdict this gate can reach. N/A or "?" for
# a criterion that plainly applies is its own problem, but guessing which
# way it should be read would be worse than saying so.
case "$badge_status" in
    Met|Unmet) ;;
    *) echo "::error title=Cannot see::small_tasks_status is \"$badge_status\";" \
            "this gate can only judge \"Met\" or \"Unmet\"." >&2
       exit 2 ;;
esac

# THE ASK ROUTE, static half. Two bindings, and they fail differently
# on purpose: a route the repository does not publish is "cannot see"
# (exit 2 — the gate no longer knows where a newcomer is sent, exactly
# like a renamed label), while a README that names no route at all is a
# FALSE claim (exit 1 — the Unmet answer promises somewhere to ask and
# there is nowhere).
check_ask_route_static() {
    [ -r "$ROUTES" ] || {
        echo "::error title=Cannot see::$ROUTES is missing or unreadable, so the" \
             "starter-task ask route could not be checked" >&2
        return 2
    }
    ROUTES="$ROUTES" README="$README" ASK_ROUTE="$ASK_ROUTE" python3 - <<'ROUTEPY'
import os, re, sys, urllib.parse

routes_path = os.environ["ROUTES"]
readme_path = os.environ["README"]
want = os.environ["ASK_ROUTE"]


def norm(u):
    """Same normalisation the label comparison uses: values, not renderings."""
    return urllib.parse.unquote_plus(u).casefold().rstrip("/")


# A TARGETED PARSE, not a YAML reader, for the reason
# check-label-taxonomy.sh gives for hand-parsing labels.yml: the CI image
# carries no YAML dependency. Only `url:` fields that are direct children
# of an entry under `contact_links:` count. A URL inside a folded
# `about:` block is prose a newcomer cannot click, and counting it would
# let the declaration be satisfied by a DESCRIPTION of a route rather
# than a published one.
urls = []
in_links = False
field_indent = None
try:
    lines = open(routes_path, encoding="utf-8").read().splitlines()
except Exception as e:
    print(f"::error title=Cannot see::{routes_path}: {e}", file=sys.stderr)
    sys.exit(2)

for line in lines:
    if not line.strip():
        continue
    indent = len(line) - len(line.lstrip(" "))
    if indent == 0:
        in_links = line.strip().startswith("contact_links:")
        field_indent = None
        continue
    if not in_links:
        continue
    m = re.match(r"^(\s*)-\s+([A-Za-z_]+):\s*(.*)$", line)
    if m:
        field_indent = len(m.group(1)) + 2
        if m.group(2) == "url":
            urls.append(m.group(3).strip())
        continue
    if field_indent is not None and indent == field_indent:
        m = re.match(r"^\s*([A-Za-z_]+):\s*(.*)$", line)
        if m and m.group(1) == "url":
            urls.append(m.group(2).strip())
    # Anything deeper than a field is a folded or continued scalar.

if norm(want) not in [norm(u) for u in urls]:
    print(f"::error title=Cannot see::the starter-task ask route is not published as a "
          f"contact link in {routes_path}, so this gate does not know where a newcomer is "
          f"told to ask. If the route moved, move it here too; if it was retired, the "
          f"Unmet answer must stop naming it. Wanted: {want}", file=sys.stderr)
    sys.exit(2)

try:
    text = open(readme_path, encoding="utf-8").read()
except Exception as e:
    print(f"::error title=Cannot see::{readme_path}: {e}", file=sys.stderr)
    sys.exit(2)

# THE README HALF IS SCOPED TO THE CLAIM, NOT TO THE FILE, and it reads
# link TARGETS rather than bytes. Both halves of that mattered, and the
# review of #867 measured why.
#
# The Unmet answer is one claim, not two sentences that happen to share
# a file: "there are none, ASK HERE". A search of the whole README
# cannot tell that claim apart from any other mention of the same URL,
# and this README has one — the Contributing section links the same
# Discussions route from its "Questions" bullet, for an unrelated
# reason. MEASURED before this scoping existed: deleting the ENTIRE
# nine-line starter-task bullet left "first task" nowhere in the file
# and the gate still exited 0, printing that the route "is published as
# a contact link" about a README that no longer invited anyone to ask
# for a first task at all. That deletion is precisely the decay this
# observable exists to catch, and its own decoy was already in the tree.
#
# And a substring search accepts renderings that are not routes: the
# same URL inside an HTML comment, a fenced block or a backticked span
# is text a newcomer cannot click. That is the defect the contact-link
# parse above refuses by construction — "a parser that scanned the file
# for the string would accept a DESCRIPTION of a route as a route" — and
# it sat thirteen lines away in the same function, applied to one half
# and not the other.
#
# THE BLOCK IS DELIMITED BY MARKERS, deliberately, rather than located
# by the heading above it or by a phrase inside it. Headings get renamed
# and sentences get reworded, and either would make this check refuse a
# README that is entirely honest — a guard that fails in the wrong
# direction. HTML comments render as nothing, so the markers cost a
# reader of the page nothing.
#
# NOT BEING ABLE TO FIND THE BLOCK IS EXIT 2, never a pass. Absent,
# duplicated or crossed markers mean the gate no longer knows which
# sentences carry the claim, which is the same "cannot see" as a renamed
# label. An EMPTY block is different and is exit 1: the gate found the
# claim and the claim names no route.
#
# What this still cannot see, stated rather than implied: markers moved
# to wrap some OTHER bullet would satisfy it. That is a deliberate
# defeat, not silent decay, and it is the same residual limit the
# contact-link parse has.
BEGIN = re.compile(r'^[ \t]*<!--[ \t]*starter-task-claim:[ \t]*begin[ \t]*-->[ \t]*$', re.M)
END = re.compile(r'^[ \t]*<!--[ \t]*starter-task-claim:[ \t]*end[ \t]*-->[ \t]*$', re.M)

MARKERS = ("the starter-task claim in %s is delimited by a pair of HTML comment markers, "
           "<!-- starter-task-claim: begin --> and <!-- starter-task-claim: end -->, so that "
           "the ask route is judged where the denial is made and not anywhere in the file. "
           % readme_path)

begins = list(BEGIN.finditer(text))
ends = list(END.finditer(text))
if len(begins) != 1 or len(ends) != 1 or ends[0].start() < begins[0].end():
    print("::error title=Cannot see::" + MARKERS +
          "Found %d begin and %d end marker(s)%s, so this gate cannot tell which sentences "
          "make the claim. Restore exactly one of each, in that order, around the "
          "starter-task bullet."
          % (len(begins), len(ends),
             ", out of order" if (len(begins) == 1 and len(ends) == 1) else ""),
          file=sys.stderr)
    sys.exit(2)

block = text[begins[0].end():ends[0].start()]


def strip_fences(s):
    """Drop fenced code blocks. An unterminated fence swallows the rest,
    which is the safe direction: it can only remove a candidate route."""
    out, fence = [], None
    for line in s.splitlines():
        m = re.match(r'^[ \t]*(`{3,}|~{3,})', line)
        if fence is None:
            if m:
                fence = m.group(1)[0]
                continue
            out.append(line)
        elif m and m.group(1)[0] == fence:
            fence = None
    return "\n".join(out)


# The same two patterns the label scan at the top of this file uses, for
# the same reason: an angle-bracketed target is collected first, or a
# link whose target runs past a character the bare form stops at is read
# as its truncated prefix. They are two copies because these are two
# python processes and there is nowhere to share a definition; change
# one and change the other.
URL = re.compile(r'https://github\.com/[^\s)"\]>]+')
BRACKETED = re.compile(r'<(https://github\.com/[^>]+)>')
COMMENT = re.compile(r'<!--.*?-->', re.S)
CODESPAN = re.compile(r'`[^`\n]*`')

inert = CODESPAN.sub(" ", COMMENT.sub(" ", strip_fences(block)))
# Trailing sentence punctuation is not part of the target GitHub links,
# so a bare URL ending a sentence stays a route.
targets = [u.rstrip(".,;:!") for u in BRACKETED.findall(inert) + URL.findall(inert)]

if not any(norm(u) == norm(want) for u in targets):
    print(f"::error title=README names no ask route::small_tasks_status is Unmet, so the "
          f"README's starter-task claim is not merely denying starter tasks, it is telling "
          f"a newcomer where to ask for one. The claim links no clickable target for the "
          f"published route, so the invitation names nowhere a newcomer can actually go. "
          f"A URL inside an HTML comment, a fenced block or backticks does not count: it is "
          f"on the page but nobody can follow it. Wanted, as a link, between the "
          f"starter-task-claim markers: {want}", file=sys.stderr)
    sys.exit(1)
ROUTEPY
}

if [ "$MODE" = "--static" ]; then
    if [ "$badge_status" = "Unmet" ]; then
        # The promise must be GONE, not merely downgraded in the JSON —
        # and gone in every rendering, not just the one we shipped.
        if [ -n "$readme_promise" ]; then
            echo "::error title=README still promises starter tasks::small_tasks_status is" \
                 "Unmet, but README.md still links a listing of issues labelled \"$LABEL\"," \
                 "so the front page promises what the badge denies:" >&2
            echo "  README: $readme_promise" >&2
            exit 1
        fi
        # A justification that says "we have none" while naming tasks is
        # the stale-citation defect (#851) in a different costume.
        if [ -n "$badge_cited" ]; then
            echo "::error title=Unmet justification cites tasks::small_tasks_status is Unmet," \
                 "but small_tasks_justification still cites #${badge_cited// /, #} as starter" \
                 "tasks. An Unmet answer names no tasks." >&2
            exit 1
        fi
        # The Unmet answer names a route. It has to be one that exists.
        route_rc=0
        check_ask_route_static || route_rc=$?
        [ "$route_rc" -eq 0 ] || exit "$route_rc"
        echo "static: small_tasks is Unmet — the README makes no starter-task promise," \
             "the justification cites none, and the ask route it does name is published" \
             "as a contact link"
        exit 0
    fi

    # Met: both artifacts must send a newcomer to a listing of THIS label.
    #
    # This replaces a byte-identity comparison of the two URLs. Identity
    # was a proxy for "the same label", and a proxy keyed on spelling:
    # two correct links written in different encodings failed it, while
    # the thing it was actually protecting — a rename leaving one
    # artifact behind — is what comparing the labels checks directly.
    static_fail=0
    check_side() { # <who> <promise> <any_url> <any_label>
        [ -n "$2" ] && return 0
        if [ -n "$3" ]; then
            echo "::error title=Starter-task link names another label::small_tasks_status is" \
                 "Met, but the $1 links a listing for \"$4\", not the declared starter-task" \
                 "label \"$LABEL\". One of the two was renamed and the other was not: $3" >&2
            return 1
        fi
        echo "::error title=Cannot see::small_tasks_status is Met, but the $1 links no" \
             "listing of issues labelled \"$LABEL\". Either it lost the link — in which case" \
             "the badge is now over-claiming — or this check no longer recognises the URL" \
             "form it uses." >&2
        return 2
    }
    check_side "README" "$readme_promise" "$readme_any_url" "$readme_any_label" || static_fail=$?
    if [ "$static_fail" -eq 0 ]; then
        check_side "badge justification" "$badge_promise" "$badge_any_url" "$badge_any_label" \
            || static_fail=$?
    fi
    [ "$static_fail" -eq 0 ] || exit "$static_fail"

    echo "static: small_tasks is Met — README and badge justification both link the" \
         "\"$LABEL\" listing"
    exit 0
fi

# THE ASK ROUTE, live half. The static half proves the README and the
# contact-link declaration name the same route; it cannot see the route
# stop existing, which happens without a commit — Discussions switched
# off in repository settings, or the category renamed or deleted. That is
# the same class of silent decay as the label's open count, so it is
# checked in the same place and on the same schedule.
#
# The repository, and the category, are derived from ASK_ROUTE rather
# than restated, so there is one copy of the fact and not three.
#
# A route kind this cannot verify is exit 2, never a skip: a live check
# that quietly passes on a route it did not look at is the vacuity this
# whole gate exists to refuse.
check_ask_route_live() {
    local parts owner repo category route_json
    parts=$(ASK_ROUTE="$ASK_ROUTE" python3 -c '
import os, sys, urllib.parse
u = urllib.parse.urlsplit(os.environ["ASK_ROUTE"])
seg = [p for p in u.path.split("/") if p]
q = urllib.parse.parse_qs(u.query)
if u.netloc != "github.com" or len(seg) != 4 or seg[2:4] != ["discussions", "new"] \
        or not q.get("category"):
    sys.exit("not a GitHub new-discussion URL")
print(seg[0]); print(seg[1]); print(q["category"][0])
' 2>&1) || {
        echo "::error title=Cannot see::this check does not know how to verify the ask route" \
             "\"$ASK_ROUTE\" ($parts). It only understands a GitHub new-discussion URL. Teach it" \
             "the new kind rather than letting the route go unchecked." >&2
        return 2
    }
    { read -r owner; read -r repo; read -r category; } <<< "$parts"

    route_json=$("$GH" api graphql -f query='
query($owner:String!, $name:String!) {
  repository(owner:$owner, name:$name) {
    hasDiscussionsEnabled
    discussionCategories(first: 100) { nodes { slug } }
  }
}' -f owner="$owner" -f name="$repo" 2>/dev/null) || {
        echo "::error title=Cannot see::could not ask $owner/$repo whether the starter-task" \
             "ask route still exists" >&2
        return 2
    }
    # Absent data is not a zero, here as well: an empty answer is
    # unreachable, not "the category is gone".
    [ -n "$route_json" ] || {
        echo "::error title=Cannot see::empty response asking $owner/$repo about the ask route" >&2
        return 2
    }

    ROUTE_JSON="$route_json" CATEGORY="$category" SLUG_OWNER="$owner/$repo" \
        ASK_ROUTE="$ASK_ROUTE" python3 -c '
import json, os, sys

# sys.exit(str) would exit 1, and in the exit-code contract at the top of
# this file 1 means "the claim is FALSE". Cannot-see is 2, so each of
# these says so explicitly rather than borrowing a wrong code from a
# convenience.
def cannot_see(msg):
    print("::error title=Cannot see::" + msg, file=sys.stderr)
    sys.exit(2)


try:
    d = json.loads(os.environ["ROUTE_JSON"])
except Exception as e:
    cannot_see("unparseable answer about the ask route: %s" % e)
if not isinstance(d, dict):
    cannot_see("expected a JSON object about the ask route, got " + type(d).__name__)
repo = (d.get("data") or {}).get("repository")
if not isinstance(repo, dict) or "hasDiscussionsEnabled" not in repo:
    cannot_see("the answer about the ask route names no repository")
if not repo["hasDiscussionsEnabled"]:
    print("::error title=The ask route is gone::small_tasks_status is Unmet and the README "
          "sends a newcomer to %s, but %s has Discussions turned off. The invitation now "
          "leads nowhere: turn Discussions back on, or name a route that exists in the "
          "README and in .github/ISSUE_TEMPLATE/config.yml."
          % (os.environ["ASK_ROUTE"], os.environ["SLUG_OWNER"]), file=sys.stderr)
    sys.exit(1)
nodes = ((repo.get("discussionCategories") or {}).get("nodes")) or []
slugs = [n.get("slug") for n in nodes if isinstance(n, dict)]
want = os.environ["CATEGORY"]
if want not in slugs:
    print("::error title=The ask route is gone::small_tasks_status is Unmet and the README "
          "sends a newcomer to %s, but %s has no discussion category \"%s\" any more (it has: "
          "%s). Point both the README and .github/ISSUE_TEMPLATE/config.yml at a category "
          "that exists." % (os.environ["ASK_ROUTE"], os.environ["SLUG_OWNER"], want,
                            ", ".join(sorted(s for s in slugs if s)) or "none"), file=sys.stderr)
    sys.exit(1)
' || return $?
}

# ---- --live ----------------------------------------------------------
command -v "$GH" >/dev/null 2>&1 || {
    echo "::error title=Cannot see::$GH is not available, so the tracker claims were not checked" >&2
    exit 2
}

open_json=$("$GH" issue list --label "$LABEL" --state open --limit 100 --json number 2>/dev/null) || {
    echo "::error title=Cannot see::could not list open issues labelled \"$LABEL\"" >&2
    exit 2
}
# An API that answers with nothing at all is unreachable, not empty. An
# empty RESULT is "[]"; an empty STRING is a failure we must not read as
# zero (absent data is not a zero).
[ -n "$open_json" ] || {
    echo "::error title=Cannot see::empty response listing issues labelled \"$LABEL\"" >&2
    exit 2
}

# The parse runs in its OWN command substitution so that its exit status
# is the one tested. A first version wrote
#
#   mapfile -t open_nums < <(python3 -c '...')
#   rc=$?
#
# where $? is MAPFILE's status, not the parser's -- mapfile succeeds at
# reading zero lines from a process that died. Measured: garbage in gives
# rc=0 with 0 elements, and so does a legitimately empty "[]". The guard
# below could never fire, and the two states were indistinguishable, so
# an unreadable response was counted as "no open issues" -- which passes
# the Unmet arm silently. That is the failure the empty-STRING check four
# lines above exists to prevent, lost four lines later (#851).
# The shape is checked BEFORE iterating, because a wrong shape that is
# EMPTY does not raise. "{}" iterates to nothing, so the parser exits 0
# having printed nothing, and that is indistinguishable from "[]" -- the
# same false green this whole block exists to close, through a narrower
# door. The first fixture written for the wrong-shape case was
# '{"number":7}', which dies inside the parser and therefore could not
# see it: a non-empty fixture cannot exercise the empty instance of its
# own class (#851).
open_list=$(printf '%s' "$open_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
if not isinstance(d, list):
    sys.exit("expected a JSON array, got " + type(d).__name__)
for i in d:
    print(i["number"])
' 2>&1) || {
    echo "::error title=Cannot see::unparseable issue list for \"$LABEL\": ${open_list##*$'\n'}" >&2
    exit 2
}
# An empty parse result is zero issues and must stay a zero-element
# array: printf '%s\n' "" would feed mapfile one blank line and count it
# as an issue, turning "no starter tasks" into one.
open_nums=()
if [ -n "$open_list" ]; then
    mapfile -t open_nums < <(printf '%s\n' "$open_list")
fi

if [ "$badge_status" = "Unmet" ]; then
    # THE REVERSE DECAY, and the reason #851's fix is reversible rather
    # than a one-way door: a genuine starter task now exists, so the
    # honest answer has changed and nothing else would say so.
    if [ "${#open_nums[@]}" -gt 0 ]; then
        echo "::error title=Starter tasks exist again::the label \"$LABEL\" has" \
             "${#open_nums[@]} open issue(s) (${open_nums[*]}), but small_tasks_status is" \
             "Unmet. The criterion is met again: set it to Met, cite the tasks in the" \
             "justification, and restore the README's link to the filter." >&2
        exit 1
    fi
    # THE ROUTE, live half. Static binds the README to the declaration;
    # neither can see the repository turning Discussions off or deleting
    # the category, which unlinks the route without touching the tree.
    route_rc=0
    check_ask_route_live || route_rc=$?
    [ "$route_rc" -eq 0 ] || exit "$route_rc"
    echo "live: small_tasks is Unmet, \"$LABEL\" has no open issues and the ask route" \
         "is still offered — the claim is still true"
    exit 0
fi

fail=0
if [ "${#open_nums[@]}" -eq 0 ]; then
    echo "::error title=No starter tasks left::the label \"$LABEL\" has no OPEN issues." \
         "README.md and .bestpractices.json both promise a newcomer somewhere to start," \
         "and that promise is currently false. Seed a real one, or change both claims" \
         "(README link out, small_tasks_status to Unmet, citations removed)." >&2
    fail=1
else
    echo "live: ${#open_nums[@]} open issue(s) carry \"$LABEL\": ${open_nums[*]}"
fi

# Every issue the justification names by number must still be open and
# still labelled — a justification citing closed work is a stale claim
# even when other starter tasks exist.
if [ -n "$badge_cited" ]; then
    for n in $badge_cited; do
        hit=0
        for o in "${open_nums[@]}"; do [ "$o" = "$n" ] && hit=1 && break; done
        if [ "$hit" -eq 0 ]; then
            echo "::error title=Stale badge justification::small_tasks_justification cites #$n" \
                 "as a starter task, but it is not an open issue labelled \"$LABEL\" any more." \
                 "Update the justification to name tasks that actually exist." >&2
            fail=1
        fi
    done
    [ "$fail" -eq 0 ] && echo "live: every cited issue ($badge_cited) is still open and labelled"
fi

exit "$fail"
