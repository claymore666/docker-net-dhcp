#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-label-taxonomy.sh (#715).
#
# Every rule the gate claims is driven RED here on a fixture that breaks it.
# The three cases marked REGRESSION are the actual defects found on the
# tracker on 2026-08-22 — a duplicate type label with no description, a used
# label that no declaration blessed, and open issues carrying two type labels
# or none. They are reproduced rather than described, because the taxonomy
# had already been repaired once by hand and the prose recording that repair
# did not prevent the second one.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-label-taxonomy.sh"
guarded_tmpdir TMP

failures=0

# run NAME WANT_EXIT WANT_GREP -- <argv...>   (env passed through the caller)
run() {
    local name="$1" want_exit="$2" want_grep="$3"; shift 3
    [ "${1:-}" = "--" ] && shift
    "$@" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ]; then
        command grep -q -- "$want_grep" "$TMP/out" || ok=0
    fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit / grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# ------------------------------------------------------------ fixtures
GOOD_LABELS="$TMP/labels.yml"
cat > "$GOOD_LABELS" <<'EOF'
# a comment, ignored
- name: bug
  role: type
  description: Something isn't working

- name: ci
  role: type
  description: CI, gates, runners, release plumbing

- name: security
  role: area
  description: 'Area: trust boundaries'

- name: go
  role: dependabot
  description: Pull requests that update go code

- name: code-review
  role: status
  description: Found during a code review pass

- name: backlog
  role: status
  description: Tracked, not scheduled for a release
EOF

GOOD_MAP="$TMP/map.yml"
cat > "$GOOD_MAP" <<'EOF'
bug:
  - '/^fix(\([^)]*\))?!?:/i'

ci:
  - '/^ci(\([^)]*\))?!?:/i'
EOF

# The fixture the rule map is judged against (#393). Every static case passes
# one explicitly: the gate refuses to run without it, and a default would
# make the cases depend on the working directory.
GOOD_FIXTURE="$TMP/titles.tsv"
printf 'fix(plugin): a real bug\tbug\n'  > "$GOOD_FIXTURE"
printf 'ci: a workflow change\tci\n'    >> "$GOOD_FIXTURE"
printf 'fixing things generally\t-\n'   >> "$GOOD_FIXTURE"
printf 'Fix(plugin): capitalised\tbug\n'   >> "$GOOD_FIXTURE"

# ------------------------------------------------------------ static: clean
run "well-formed declaration passes" 0 "Label taxonomy OK (static)" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$GOOD_MAP" "$GOOD_FIXTURE"

# ------------------------------------------------- static: usage and inputs
run "no mode is a usage error" 2 "usage:" -- \
    bash "$CHECK" --nonsense
run "missing declaration cannot check" 2 "missing declaration" -- \
    bash "$CHECK" --static "$TMP/absent.yml" "$GOOD_MAP" "$GOOD_FIXTURE"
run "missing map cannot check" 2 "missing:" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$TMP/absent.yml" "$GOOD_FIXTURE"

# ------------------------------------------------ static: declaration rules
mk() { cp "$GOOD_LABELS" "$1"; }

# REGRESSION: `security` shipped with no description and sat on 13 issues.
NO_DESC="$TMP/no-desc.yml"
cat > "$NO_DESC" <<'EOF'
- name: bug
  role: type
  description: Something isn't working

- name: security
  role: area
EOF
run "REGRESSION a label with no description is red" 1 "has no description" -- \
    bash "$CHECK" --static "$NO_DESC" "$GOOD_MAP" "$GOOD_FIXTURE"

NO_ROLE="$TMP/no-role.yml"
printf -- '- name: bug\n  description: x\n' > "$NO_ROLE"
run "a label with no role is red" 1 "has no role" -- \
    bash "$CHECK" --static "$NO_ROLE" "$GOOD_MAP" "$GOOD_FIXTURE"

BAD_ROLE="$TMP/bad-role.yml"
printf -- '- name: bug\n  role: kind-of\n  description: x\n' > "$BAD_ROLE"
run "an unknown role is red" 1 "not one of" -- \
    bash "$CHECK" --static "$BAD_ROLE" "$GOOD_MAP" "$GOOD_FIXTURE"

DUPE="$TMP/dupe.yml"
cat > "$DUPE" <<'EOF'
- name: bug
  role: type
  description: Something isn't working

- name: bug
  role: area
  description: A second entry for the same name
EOF
run "a name declared twice is red" 1 "declared twice" -- \
    bash "$CHECK" --static "$DUPE" "$GOOD_MAP" "$GOOD_FIXTURE"

# The empty-set guard. Without it, "exactly one type label" is satisfied
# vacuously by every issue on the tracker and the live half reports clean.
NO_TYPE="$TMP/no-type.yml"
printf -- '- name: security\n  role: area\n  description: x\n' > "$NO_TYPE"
run "a declaration with no type label is red" 1 "no label has role 'type'" -- \
    bash "$CHECK" --static "$NO_TYPE" "$GOOD_MAP" "$GOOD_FIXTURE"

# The same guard on the other universal. "No issue wears a Dependabot label"
# is satisfied by declaring none, and the live half would then report clean
# over a tracker full of them.
NO_DEPBOT="$TMP/no-dependabot.yml"
printf -- '- name: bug\n  role: type\n  description: x\n' > "$NO_DEPBOT"
run "a declaration with no Dependabot label is red" 1 "no label has role 'dependabot'" -- \
    bash "$CHECK" --static "$NO_DEPBOT" "$GOOD_MAP" "$GOOD_FIXTURE"

EMPTY="$TMP/empty.yml"
printf '# nothing but a comment\n' > "$EMPTY"
run "an empty declaration is red, not clean" 1 "no labels declared" -- \
    bash "$CHECK" --static "$EMPTY" "$GOOD_MAP" "$GOOD_FIXTURE"

# ---------------------------------------------------- static: labeller ties
UNDECLARED_MAP="$TMP/undeclared-map.yml"
printf 'tests:\n  - %s\n' "'/^tests:/i'" > "$UNDECLARED_MAP"
run "a rule label that is not declared is red" 1 "is not declared" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$UNDECLARED_MAP" "$GOOD_FIXTURE"

# The labeller must never reach workflow state. `backlog` is a status label:
# an issue title cannot establish that something is descheduled.
STATUS_MAP="$TMP/status-map.yml"
printf 'backlog:\n  - %s\n' "'/^later:/i'" > "$STATUS_MAP"
run "the labeller may not apply a status label" 1 "may only apply" -- \
    bash "$CHECK" --static "$GOOD_LABELS" "$STATUS_MAP" "$GOOD_FIXTURE"

# ------------------------------- static: the parser's own refusals (#715)
#
# #715 asks that EVERY rule the gate claims goes red on a fixture that
# violates it. Six did not, and all six are in the READER rather than in
# the rules — the half nobody drives, because a malformed declaration
# feels like a thing that cannot happen. It is exactly the thing that
# can: this file is hand-edited, and a reader that quietly keeps the last
# of two values would let a label declare two roles and still report the
# taxonomy clean.
#
# EACH FIXTURE IS THE GOOD DECLARATION PLUS ONE DEFECT. Writing them from
# scratch is what the first draft did, and every one of them was then red
# for an incidental reason — a two-label declaration does not cover the
# labels GOOD_MAP names, so the case passed on a failure it was not
# testing. Derived this way the verdict itself moves when the rule is
# removed, and the case cannot pass for the wrong reason.
mkbad() { # <out> <sed-expr>   — the good declaration with one thing wrong
    sed "$2" "$GOOD_LABELS" > "$1"
}

# A repeated key is not a merge. Python's dict keeps the last silently,
# so `role: type` twice reads as a well-formed label and nothing says a
# second value was ever written.
DUP_ROLE="$TMP/dup-role.yml"
mkbad "$DUP_ROLE" '0,/^  role: type$/s//  role: type\n  role: type/'
run "a label declaring two roles is red" 1 "duplicate 'role'" -- \
    bash "$CHECK" --static "$DUP_ROLE" "$GOOD_MAP" "$GOOD_FIXTURE"

DUP_DESC="$TMP/dup-desc.yml"
mkbad "$DUP_DESC" "0,/^  description: Something/s//  description: Something isn't working\n  description: Something/"
run "a label declaring two descriptions is red" 1 "duplicate 'description'" -- \
    bash "$CHECK" --static "$DUP_DESC" "$GOOD_MAP" "$GOOD_FIXTURE"

# A field above the first entry belongs to nothing. Attaching it to the
# next label would be a guess about what the author meant, and a reader
# that guesses is a reader that is quietly wrong.
ORPHAN="$TMP/orphan-field.yml"
{ printf '  role: type\n\n'; cat "$GOOD_LABELS"; } > "$ORPHAN"
run "a field before the first entry is red" 1 "field before any" -- \
    bash "$CHECK" --static "$ORPHAN" "$GOOD_MAP" "$GOOD_FIXTURE"

# THE ONE THAT MATTERS MOST OF THE SIX. A line this reader does not
# understand has to be an error and never a skip: silently ignoring it is
# how a declaration comes to say less than its author wrote, and the file
# exists precisely to be the artifact a reviewer reads.
UNPARSEABLE="$TMP/unparseable.yml"
mkbad "$UNPARSEABLE" '0,/^  role: type$/s//  role: type\n  colour: ff0000/'
run "a line the reader does not understand is red, not skipped" 1 "cannot parse" -- \
    bash "$CHECK" --static "$UNPARSEABLE" "$GOOD_MAP" "$GOOD_FIXTURE"

# Added as a fourth entry rather than by blanking an existing name, so the
# labels GOOD_MAP references all still exist and the empty name is the only
# thing wrong.
EMPTY_NAME="$TMP/empty-name.yml"
{ cat "$GOOD_LABELS"; printf "\n- name: ''\n  role: area\n  description: an entry with no name at all\n"; } > "$EMPTY_NAME"
run "a label with an empty name is red" 1 "empty name" -- \
    bash "$CHECK" --static "$EMPTY_NAME" "$GOOD_MAP" "$GOOD_FIXTURE"

# ------------------------------- static: the rule map and its fixture (#393)
#
# The issue label-map gate was folded in here by #745. Each case below is one
# of its assertions, driven on the good declaration plus one defect.
rmap() { # <name> <body>  -> path of a rule map
    printf '%s' "$2" > "$TMP/$1.yml"; echo "$TMP/$1.yml"
}
st() { # <labels> <map> <fixture>
    bash "$CHECK" --static "$1" "$2" "$3"
}

BAD_REGEX="$(rmap bad-regex "$(printf "bug:\n  - '/^fix(\\\\([^)]*\\\\)?!?:/i'\n")")"
run "an uncompilable rule regex is red" 1 "bad regex" -- \
    st "$GOOD_LABELS" "$BAD_REGEX" "$GOOD_FIXTURE"

BAD_FLAG="$(rmap bad-flag "$(printf "bug:\n  - '/^fix:/s'\n")")"
run "an unsupported regex flag is red" 1 "not supported" -- \
    st "$GOOD_LABELS" "$BAD_FLAG" "$GOOD_FIXTURE"

# The action honours only i; m and s change what a pattern means and would
# pass here while the labeller treats them differently (#745).
BAD_FLAG_M="$(rmap bad-flag-m "$(printf "bug:\n  - '/^fix:/m'\n")")"
run "the multiline regex flag is red too" 1 "flag 'm' is not supported" -- \
    st "$GOOD_LABELS" "$BAD_FLAG_M" "$GOOD_FIXTURE"

# The regression the fixture exists for: a pattern that stops matching what
# the fixture says it must.
DRIFTED="$(rmap drifted "$(printf "bug:\n  - '/^bugfix:/i'\nci:\n  - '/^ci:/i'\n")")"
run "a pattern that no longer matches the fixture is red" 1 "want \[bug\]" -- \
    st "$GOOD_LABELS" "$DRIFTED" "$GOOD_FIXTURE"

# Several patterns under one label are ANDed, as the action does. Under OR
# the first title would classify as bug and its '-' row would go red.
AND_MAP="$(rmap and-map "$(printf "bug:\n  - '/^fix/i'\n  - '/urgent/i'\n")")"
AND_FIXTURE="$TMP/and.tsv"
printf 'fix: plain\t-\nfix: urgent\tbug\nurgent matter\t-\n' > "$AND_FIXTURE"
run "several patterns under one label must all match" 0 "Label taxonomy OK (static)" -- \
    st "$GOOD_LABELS" "$AND_MAP" "$AND_FIXTURE"

# The action matches against the title plus a blank line, and 'g' is
# meaningless for a single match: both must hold for this map to pass.
TAIL_MAP="$(rmap tail-map "$(printf "bug:\n  - '/^fix: x\\\\n\\\\n\$/gi'\n")")"
TAIL_FIXTURE="$TMP/tail.tsv"
printf 'fix: x\tbug\nfix: y\t-\n' > "$TAIL_FIXTURE"
run "the target is the title plus a blank line, and the g flag is tolerated" 0 "Label taxonomy OK (static)" -- \
    st "$GOOD_LABELS" "$TAIL_MAP" "$TAIL_FIXTURE"

# A fixture saved with CRLF endings or a trailing space still says '-'.
WS_FIXTURE="$TMP/ws.tsv"
printf 'unrelated words\t- \r\nfix: a bug\tbug \r\n' > "$WS_FIXTURE"
run "fixture rows tolerate trailing whitespace and CR" 0 "Label taxonomy OK (static)" -- \
    st "$GOOD_LABELS" "$GOOD_MAP" "$WS_FIXTURE"

# A title the map must not classify: the gate fails when it does.
NEG_FIXTURE="$TMP/neg.tsv"
printf 'fix(plugin): a real bug\t-\n' > "$NEG_FIXTURE"
run "a negative row that the map classifies is red" 1 "want \[-\]" -- \
    st "$GOOD_LABELS" "$GOOD_MAP" "$NEG_FIXTURE"

EMPTY_LABEL="$(rmap empty-label "$(printf "bug:\nci:\n  - '/^ci:/i'\n")")"
run "a rule label with no patterns is red" 1 "has no patterns" -- \
    st "$GOOD_LABELS" "$EMPTY_LABEL" "$GOOD_FIXTURE"

NO_RULES="$(rmap no-rules "$(printf '# nothing but a comment\n')")"
run "a rule map with no rules is red" 1 "no rules found" -- \
    st "$GOOD_LABELS" "$NO_RULES" "$GOOD_FIXTURE"

ORPHAN_PAT="$(rmap orphan-pattern "$(printf "  - '/^fix:/i'\nbug:\n  - '/^fix:/i'\n")")"
run "a pattern before any label is red" 1 "pattern before any label" -- \
    st "$GOOD_LABELS" "$ORPHAN_PAT" "$GOOD_FIXTURE"

UNPARSEABLE_MAP="$(rmap unparseable-map "$(printf "bug:\n  - '/^fix:/i'\n  weight: 3\n")")"
run "a rule-map line the reader does not understand is red" 1 "cannot parse" -- \
    st "$GOOD_LABELS" "$UNPARSEABLE_MAP" "$GOOD_FIXTURE"

BAD_ROW="$TMP/bad-row.tsv"
printf 'no tab on this row\n' > "$BAD_ROW"
run "a fixture row without a tab is red" 1 "want '<title>" -- \
    st "$GOOD_LABELS" "$GOOD_MAP" "$BAD_ROW"

# The fixture is part of the contract: absent, the rule map is unjudged, and
# that is a refusal and never a pass.
run "a missing fixture cannot check" 2 "missing:" -- \
    st "$GOOD_LABELS" "$GOOD_MAP" "$TMP/absent.tsv"

# --------------------------------------------------------------- live
#
# THE STUB IS A `gh`, NOT A FIXTURE FILE (#840).
#
# These cases used to drive the live rules through LABELS_TSV / ISSUES_TSV,
# which supplied the rendered result. Ten of them exercised the rules well
# and none of them ever executed one of the gate's three `gh` queries: the
# seams sat above the fetch. They also hand-wrote the format the query's
# `--jq` produced, so the fixture and the query were two encodings of one
# thing with nothing tying them together.
#
# Now the stub prints the JSON the API prints and the gate's own parsing
# turns it into the values under test, which is how check-good-first-issues.sh
# and check-milestone-scope.sh already do it.
#
# THE CALL LOG IS THE WITNESS, and it is not decoration. Measured on #827
# while establishing this pattern elsewhere: with the stub off PATH, three of
# seven cases returned the CORRECT exit code having invoked nothing at all —
# the real `gh` answered a real 404 for a nonexistent repository. They passed
# while testing nothing, and made live API calls from a self-test. An exit
# code alone cannot tell that apart from a case that worked, so every live
# case below asserts how many times `gh` was called as well.
GH_LOG="$TMP/gh-calls.log"
STUB="$TMP/gh"
cat > "$STUB" <<'EOF'
#!/usr/bin/env bash
# Log FIRST and unconditionally: a call that is going to fail is still a
# call, and the count is what proves the gate reached the transport.
printf '%s\n' "$*" >> "$GH_LOG"
case "$1 $2" in
    "repo view")  body="${STUB_REPO-}";   rc="${STUB_REPO_RC:-0}"   ;;
    "label list") body="${STUB_LABELS-}"; rc="${STUB_LABELS_RC:-0}" ;;
    "issue list") body="${STUB_ISSUES-}"; rc="${STUB_ISSUES_RC:-0}" ;;
    *)
        printf 'the self-test stub was asked for an unexpected subcommand: %s\n' "$*" >&2
        exit 99
        ;;
esac
# `${STUB_x-}` and never `${STUB_x:-}`: an EMPTY body is a case this suite
# deliberately drives — the query that succeeded and said nothing — and the
# colon form would substitute a default for it and test something else.
[ -n "${STUB_STDERR-}" ] && printf '%s\n' "$STUB_STDERR" >&2
[ "$rc" -ne 0 ] && exit "$rc"
printf '%s' "$body"
EOF
chmod +x "$STUB"
export GH_LOG

# The API's own shapes. `description` is null rather than absent when a label
# was created without one, which is the state #715 was written about.
LIVE_OK_LABELS='[
  {"name":"bug","description":"Something isn'"'"'t working"},
  {"name":"ci","description":"CI, gates, runners, release plumbing"},
  {"name":"security","description":"Area: trust boundaries"},
  {"name":"go","description":"Pull requests that update go code"},
  {"name":"code-review","description":"Found during a code review pass"},
  {"name":"backlog","description":"Tracked, not scheduled for a release"}
]'

LIVE_OK_ISSUES='[
  {"number":1,"labels":[{"name":"bug"},{"name":"security"}]},
  {"number":2,"labels":[{"name":"ci"}]},
  {"number":3,"labels":[{"name":"bug"},{"name":"code-review"}]}
]'

# live NAME WANT_EXIT WANT_GREP WANT_CALLS <labels-json> <issues-json>
#
# WANT_CALLS is the whole point of the log: it pins WHERE the gate stopped.
# Two means it asked for labels and for issues; one means it refused after
# the first answer; zero means it never reached the transport at all, which
# is what a case that proves nothing looks like.
live() {
    local name="$1" want_exit="$2" want_grep="$3" want_calls="$4"
    local labels="$5" issues="$6"
    : > "$GH_LOG"
    REPO=fixture/repo LT_GH="$STUB" \
        STUB_LABELS="$labels" STUB_ISSUES="$issues" \
        run "$name" "$want_exit" "$want_grep" -- \
        bash "$CHECK" --live "$GOOD_LABELS"
    local got_calls
    got_calls=$(wc -l < "$GH_LOG")
    if [ "$got_calls" -eq "$want_calls" ]; then
        echo "PASS: $name — gh called $got_calls time(s)"
    else
        echo "FAIL: $name — gh called $got_calls time(s), wanted $want_calls"
        sed 's/^/    called: /' "$GH_LOG"
        failures=$((failures + 1))
    fi
}

# The control runs FIRST and green, so that every red case below is known to
# have gone red from its own fixture rather than from a harness that was
# already broken.
live "live: a conforming tracker passes" 0 "Label taxonomy OK (live)" 2 \
    "$LIVE_OK_LABELS" "$LIVE_OK_ISSUES"

# The queries themselves, which nothing checked until #840. A gate that asks
# the wrong repository, or drops `--state open` and judges closed issues,
# produces a verdict that looks exactly like a correct one.
: > "$GH_LOG"
REPO=fixture/repo LT_GH="$STUB" STUB_LABELS="$LIVE_OK_LABELS" \
    STUB_ISSUES="$LIVE_OK_ISSUES" bash "$CHECK" --live "$GOOD_LABELS" >/dev/null 2>&1
if command grep -q -- '--repo fixture/repo' "$GH_LOG" \
   && command grep -q -- 'issue list .*--state open' "$GH_LOG" \
   && command grep -q -- 'label list .*--json name,description' "$GH_LOG"; then
    echo "PASS: live: the queries name the repo, open issues, and the fields read below"
else
    echo "FAIL: live: the queries are not what the rules below assume"
    sed 's/^/    called: /' "$GH_LOG"
    failures=$((failures + 1))
fi

# REGRESSION: `tests` was created on the tracker, duplicating `testing`,
# and no file said it was not allowed to exist.
EXTRA=$(printf '%s' "$LIVE_OK_LABELS" | sed 's/^\]$/,{"name":"tests","description":null}]/')
live "REGRESSION live: an undeclared tracker label is red" 1 "is not declared in" 2 \
    "$EXTRA" "$LIVE_OK_ISSUES"

# `ci` dropped from the tracker while the declaration still names it.
MISSING='[
  {"name":"bug","description":"Something isn'"'"'t working"},
  {"name":"security","description":"Area: trust boundaries"},
  {"name":"go","description":"Pull requests that update go code"},
  {"name":"code-review","description":"Found during a code review pass"},
  {"name":"backlog","description":"Tracked, not scheduled for a release"}
]'
live "live: a declared label absent from the tracker is red" 1 "does not exist on" 2 \
    "$MISSING" "$LIVE_OK_ISSUES"

DESC=$(printf '%s' "$LIVE_OK_LABELS" | sed 's/Something isn.t working/something else entirely/')
live "live: a drifted description is red" 1 "description differs" 2 \
    "$DESC" "$LIVE_OK_ISSUES"

# A label created through the web UI has a null description, not an empty
# one, and the old `--jq` collapsed the two before any fixture could see the
# difference. It reaches the rules now, so the assertion is on the DIAGNOSTIC
# rather than on the verdict: the verdict is red either way here, because the
# declaration for `bug` does carry a description. What the mutant moves is
# what the reader is told the tracker holds — `''` if null is read as an
# empty description, Python's `None` leaking into a sentence about GitHub if
# it is not.
NULLDESC='[{"name":"bug","description":null}]'
live "live: a null description is reported as empty, not as None" 1 "tracker:  ''" 2 \
    "$NULLDESC" "$LIVE_OK_ISSUES"

# REGRESSION: ten open issues carried two type labels, seven carried none.
TWO='[{"number":9,"labels":[{"name":"bug"},{"name":"ci"}]}]'
live "REGRESSION live: two type labels is red" 1 "carries 2 type labels" 2 \
    "$LIVE_OK_LABELS" "$TWO"

NONE='[{"number":9,"labels":[{"name":"security"}]}]'
live "REGRESSION live: no type label is red" 1 "carries no type label" 2 \
    "$LIVE_OK_LABELS" "$NONE"

# An issue with no labels at all arrives as an empty array, which is a
# different shape from the empty string the old TSV fixture supplied.
BARE='[{"number":9,"labels":[]}]'
live "live: an issue with no labels at all is red" 1 "carries no type label" 2 \
    "$LIVE_OK_LABELS" "$BARE"

# REGRESSION: 21 issues wore a Dependabot label applied by hand, 4 of them
# open — `github_actions` borrowed as a "CI work" marker, `go` as a "Go code"
# one.
# .github/labels.yml had forbidden it in prose since #715 and nothing checked.
DEPBOT='[{"number":9,"labels":[{"name":"bug"},{"name":"go"}]}]'
live "REGRESSION live: a hand-applied Dependabot label is red" 1 "never belong on an issue" 2 \
    "$LIVE_OK_LABELS" "$DEPBOT"

# `backlog` with a milestone belongs to check-milestone-scope.sh. This gate
# must stay silent on it, or one defect goes red in two places and the rule
# has two homes.
BACKLOG='[{"number":9,"labels":[{"name":"bug"},{"name":"backlog"}]}]'
live "live: backlog is left to check-milestone-scope.sh" 0 "Label taxonomy OK (live)" 2 \
    "$LIVE_OK_LABELS" "$BACKLOG"

# ------------------------------------------- how the query can go wrong
#
# Absent data is not a zero, and the three ways of having none are now three
# messages rather than one. Each is asserted on its own text: the whole point
# of #840's second defect is that a reader of a red scheduled run could not
# tell a revoked token from an empty tracker.

# WANT_CALLS is 2 and not 1 on purpose. FETCH BOTH, THEN JUDGE: the shape
# rules live with the rules, in the one python block below, so a body that
# cannot be read is discovered after both queries rather than between them.
# Refusing earlier would mean a second parser in shell deciding what "a
# usable body" is, and two parsers that can disagree about one format is
# defect 1 of #840 rebuilt one layer down. It costs one wasted query on a
# tracker that is already broken.
live "live: an empty label list cannot check" 2 "returned an empty list" 2 \
    '[]' "$LIVE_OK_ISSUES"
live "live: an empty issue list cannot check" 2 "returned an empty list" 2 \
    "$LIVE_OK_LABELS" '[]'

# The query said nothing at all — distinct from saying "[]", and the one the
# collapsed message used to hide.
live "live: a query that returns nothing is not an empty tracker" 2 "returned nothing" 1 \
    '' "$LIVE_OK_ISSUES"

# rc set, and a reason on stderr: it must survive into the output, because
# that is the line the person reading the red run acts on.
: > "$GH_LOG"
REPO=fixture/repo LT_GH="$STUB" STUB_LABELS="$LIVE_OK_LABELS" \
    STUB_ISSUES="$LIVE_OK_ISSUES" STUB_LABELS_RC=1 \
    STUB_STDERR='HTTP 401: Bad credentials' \
    run "live: a failing query reports its own stderr" 2 "Bad credentials" -- \
    bash "$CHECK" --live "$GOOD_LABELS"

# rc set with BOTH streams empty. A bare exit number is not a diagnostic and
# must not be printed as though it were.
: > "$GH_LOG"
REPO=fixture/repo LT_GH="$STUB" STUB_LABELS="$LIVE_OK_LABELS" \
    STUB_ISSUES="$LIVE_OK_ISSUES" STUB_LABELS_RC=7 \
    run "live: a silent failure says it was silent" 2 "printed nothing on stderr" -- \
    bash "$CHECK" --live "$GOOD_LABELS"

# THE MODE THAT PRODUCES A WRONG VERDICT RATHER THAN AN ERROR: a 4xx whose
# body lands on stdout with stderr empty and rc 0. It is not empty, so no
# emptiness guard sees it; it is JSON, so it parses. Only asking what SHAPE
# came back catches it.
live "live: a 4xx error document is not a label list" 2 "not the expected list" 2 \
    '{"message":"Not Found","documentation_url":"https://docs.github.com/"}' \
    "$LIVE_OK_ISSUES"

# The same failure without valid JSON — a proxy's HTML error page.
live "live: a non-JSON body is refused, not parsed" 2 "did not return JSON" 2 \
    '<html><body>502 Bad Gateway</body></html>' "$LIVE_OK_ISSUES"

# ------------------------------------------------ the transport itself
#
# `gh` missing is exit 2 and not a pass. This is the case the command seam
# makes reachable at all: with the old result seam the gate skipped its own
# `command -v` whenever a fixture was supplied, so the check never ran.
: > "$GH_LOG"
REPO=fixture/repo LT_GH="$TMP/definitely-not-a-command" \
    run "live: a missing gh cannot check" 2 "is required for --live" -- \
    bash "$CHECK" --live "$GOOD_LABELS"
if [ ! -s "$GH_LOG" ]; then
    echo "PASS: live: a missing gh is refused before any query is attempted"
else
    echo "FAIL: live: a missing gh still reached the transport"
    sed 's/^/    called: /' "$GH_LOG"
    failures=$((failures + 1))
fi

# REPO unset asks `gh` for it, which is the third query and the one with no
# fixture of its own before #840.
: > "$GH_LOG"
REPO='' LT_GH="$STUB" STUB_REPO='{"nameWithOwner":"fixture/repo"}' \
    STUB_LABELS="$LIVE_OK_LABELS" STUB_ISSUES="$LIVE_OK_ISSUES" \
    run "live: REPO is discovered from gh when unset" 0 "match fixture/repo" -- \
    bash "$CHECK" --live "$GOOD_LABELS"
got=$(wc -l < "$GH_LOG")
if [ "$got" -eq 3 ]; then
    echo "PASS: live: discovering REPO costs a third query — gh called 3 times"
else
    echo "FAIL: live: wanted 3 gh calls when REPO is unset, got $got"
    sed 's/^/    called: /' "$GH_LOG"
    failures=$((failures + 1))
fi

: > "$GH_LOG"
REPO='' LT_GH="$STUB" STUB_REPO='{"nameWithOwner":""}' \
    STUB_LABELS="$LIVE_OK_LABELS" STUB_ISSUES="$LIVE_OK_ISSUES" \
    run "live: a repo query that names nothing cannot check" 2 "cannot determine the repository" -- \
    bash "$CHECK" --live "$GOOD_LABELS"
echo
if [ "$failures" -ne 0 ]; then
    echo "failed: $failures"
    exit 1
fi
echo "all check-label-taxonomy.sh tests passed"
