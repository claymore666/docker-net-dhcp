#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-lint-tag-coverage.sh (#871).
#
# THE CASES THAT MATTER ARE THE ONES WITH NO INSTANCE IN THE TREE. On
# the repository this gate ships into there is exactly one build-tag
# spelling, no negated constraint, no legacy `// +build` line and no
# compound expression — so the compound refusal, the negated-term rule
# and the legacy parser all have an EMPTY domain, and a universal rule
# over an empty domain is satisfied by the domain being empty. Every
# one of them is driven here on a constructed fixture instead.
#
# The gate reads TRACKED files, so each fixture is a real git repo. An
# untracked .go file is not part of what CI lints, and a gate that
# counted it would fail on any dirty tree.
#
# EVERY CASE RUNS IN THREE WORKFLOW SHAPES, and that axis is the reason
# this file exists in its current form. It shipped with 31 assertions
# and every one of them built a bare `- run:` step — `grep -- '- name:'`
# over it returned nothing. The workflow the gate actually protects
# writes `- name: Run staticcheck (default view)` above its `run:`, and
# the gate matched the NAME. So the untagged invocation could be deleted
# from CI while the gate reported full coverage, and no fixture here
# could ever have contained the shape that did it: a property test is
# bounded by its fixture, and this one's population structurally
# excluded the defect.
#
# The fix is not one added case. The step shape is now a VARIABLE —
# `bare`, `named`, `block` — and every case below is driven in all
# three, so a future recognition bug in any one of them fails here
# rather than in production. The `named` shape deliberately puts the
# word `staticcheck` in the step name, because that is what the real
# workflow does and it is what defeated the gate.
#
# Usage: bash scripts/test-check-lint-tag-coverage.sh

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-lint-tag-coverage.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
SHAPE=bare
ok()  { pass=$((pass+1)); echo "  ok   [$SHAPE] $1"; }
bad() { fail=$((fail+1)); echo "  FAIL [$SHAPE] $1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (rc=$2, want $3)"; fi; }

# repo <name> -- a fresh tracked fixture; callers add files then `track`.
repo() {
    R="$TMP/$1"; rm -rf "$R"; mkdir -p "$R/pkg" "$R/.github/workflows"
    git -C "$R" init -q
    git -C "$R" config user.email t@t; git -C "$R" config user.name t
}
track() { git -C "$R" add -A >/dev/null 2>&1; }
run()   { LINT_TAG_ROOT="$R" bash "$GATE" 2>&1; }
rc()    { run >/dev/null 2>&1; echo $?; }

# says <desc> <substring> -- assert the report CONTAINS the substring.
#
# Deliberately not `run | grep -q`. Under `set -o pipefail` a pipeline
# reports the PRODUCER's failure, and this producer is a gate whose
# whole job is to exit 1 on a finding and 2 on a refusal. So every
# assertion that reads a message out of a run that was SUPPOSED to fail
# reports failure no matter what grep found.
#
# That cost five false FAILs when this file was first run, and it hid
# behind the one such assertion that passed: the only one reading a
# CLEAN run. A message check over a failing subject cannot be a
# pipeline, and the tell is that the subject is a gate.
#
# SIGPIPE is a second, independent way the same shape misreports --
# `grep -q` exits on the first match and the producer dies of it -- and
# it is why scripts/check-pipefail-consumers.sh flags `| grep -q` at
# all. It was NOT what happened here; the producer's own exit status
# was enough. Measured: with pipefail the pipeline is 1, without it 0,
# and grep against the captured output is 0.
says() {
    local out; out="$(run)"
    case "$out" in
        *"$2"*) ok "$1" ;;
        *) bad "$1 — the report does not contain '$2':"$'\n'"$out" ;;
    esac
}

# wf <command>... -- a workflow running exactly these commands, in the
# step shape named by $SHAPE. Written once so no case can differ from
# the others by accident, and parameterised so no case can be blind to
# a shape by accident either.
#
#   bare   - run: <cmd>                     the shape this file only had
#   named  - name: ... / run: <cmd>         the shape test.yaml uses
#   block  - name: ... / run: | / <cmd>     a block scalar body
#
# The `named` and `block` step names contain the word `staticcheck` on
# purpose: `Run staticcheck (default view)` is verbatim what test.yaml
# writes, and a gate that reads names rather than commands passes every
# case here while the command is absent.
wf() {
    local f="$R/.github/workflows/test.yaml" i=0 cmd
    { echo "jobs:"; echo "  staticcheck:"; echo "    steps:"; } > "$f"
    for cmd in "$@"; do
        i=$((i + 1))
        case "$SHAPE" in
            bare)  printf '      - run: %s\n' "$cmd" >> "$f" ;;
            named) printf '      - name: Run staticcheck (view %d)\n        run: %s\n' \
                       "$i" "$cmd" >> "$f" ;;
            block) printf '      - name: Run staticcheck (view %d)\n        run: |\n          %s\n' \
                       "$i" "$cmd" >> "$f" ;;
            *)     echo "  FAIL unknown shape '$SHAPE'"; exit 9 ;;
        esac
    done
}
# The commented-out invocation, in each shape. A YAML comment in the
# two step shapes, and a SHELL comment inside an executed `run: |` body
# for the block shape — the block case is the one a line-oriented
# matcher gets wrong, because the line really is inside something the
# workflow runs.
wf_commented_out() {
    local f="$R/.github/workflows/test.yaml"
    { echo "jobs:"; echo "  staticcheck:"; echo "    steps:"; } > "$f"
    case "$SHAPE" in
        bare)  printf '      # - run: staticcheck ./...\n' >> "$f" ;;
        named) printf '      # - name: Run staticcheck (default view)\n      #   run: staticcheck ./...\n' >> "$f" ;;
        block) printf '      - name: Run staticcheck (default view)\n        run: |\n          # staticcheck ./...\n          echo hi\n' >> "$f" ;;
        *)     echo "  FAIL unknown shape '$SHAPE'"; exit 9 ;;
    esac
}
SC_INSTALL='go install honnef.co/go/tools/cmd/staticcheck@v0.8.1'
wf_both()          { wf "$SC_INSTALL" 'staticcheck ./...' 'staticcheck -tags integration ./...'; }
wf_untagged_only() { wf "$SC_INSTALL" 'staticcheck ./...'; }
wf_tagged_only()   { wf 'staticcheck -tags integration ./...'; }
go_plain()  { printf 'package pkg\n\nfunc A() {}\n' > "$R/pkg/plain.go"; }
go_tagged() { printf '//go:build %s\n\npackage pkg\n\nfunc B() {}\n' "$1" > "$R/pkg/tagged.go"; }

echo "1..N check-lint-tag-coverage"

# EVERY CASE, IN EVERY STEP SHAPE. See the header: the shape was the
# un-varied axis, and the defect lived in exactly the value this file
# never generated.
for SHAPE in bare named block; do

# --- 1 CONTROL: covered tree, both invocations -----------------------
repo c1; go_plain; go_tagged integration; wf_both; track
chk "covered tree passes" "$(rc)" "0"
says "and it says what it inspected" 'clean (2 tracked'

# --- 2 THE DEFECT #871 WAS FILED ON ----------------------------------
repo c2; go_plain; go_tagged integration; wf_untagged_only; track
chk "a tag no invocation names is a gap" "$(rc)" "1"
says "the gap names the tag and counts its files" \
    "build tag 'integration' is carried by 1 tracked"

# --- 3 THE OPPOSITE OMISSION: tagged-only ----------------------------
# The one-widened-flag shortcut. Nothing in the tree fails under it
# today, which is exactly why the rule has to be structural.
repo c3; go_plain; go_tagged integration; wf_tagged_only; track
chk "a tags-only workflow is a gap too" "$(rc)" "1"
says "and it names the missing default view" 'runs WITHOUT -tags'

# --- 4 A NEGATED TERM IS COVERED BY THE DEFAULT VIEW -----------------
# No file in the real tree carries one. If this rule were wrong nothing
# there would show it.
repo c4; go_plain; go_tagged '!integration'; wf_untagged_only; track
chk "a negated term needs no -tags of its own" "$(rc)" "0"

# --- 4b ...AND IT STILL NEEDS THE DEFAULT VIEW -----------------------
repo c4b; go_plain; go_tagged '!integration'; wf_tagged_only; track
chk "a negated term with no default view is a gap" "$(rc)" "1"

# --- 5 COMPOUND CONSTRAINTS: REFUSE, NEVER CLEAR ---------------------
# Empty domain in the real tree. A gate that cannot judge and says
# nothing is the failure #871 is about, so this must be 2 and not 0.
for expr in 'integration && linux' 'integration || e2e' '(integration)'; do
    repo c5; go_plain; go_tagged "$expr"; wf_both; track
    chk "compound '$expr' refuses rather than clears" "$(rc)" "2"
done
repo c5m; go_plain; go_tagged 'integration && linux'; wf_both; track
says "the refusal says it cannot judge" 'cannot judge lint coverage'
says "and names the file it could not judge" 'pkg/tagged.go'

# --- 6 LEGACY // +build IS STILL A CONSTRAINT ------------------------
# Go honours it when no //go:build is present. None in the real tree.
repo c6; go_plain
printf '// +build integration\n\npackage pkg\n\nfunc B() {}\n' > "$R/pkg/tagged.go"
wf_untagged_only; track
chk "a legacy +build tag is a gap when nothing lints it" "$(rc)" "1"

repo c6b; go_plain
printf '// +build integration,linux\n\npackage pkg\n\nfunc B() {}\n' > "$R/pkg/tagged.go"
wf_both; track
chk "a legacy comma is compound, so it refuses" "$(rc)" "2"

# --- 7 NON-VACUITY, both halves --------------------------------------
repo c7; wf_both; track   # no .go files at all
chk "no tracked .go files refuses" "$(rc)" "2"

# A git failure and an empty tree are both exit 2, and they are
# different things to go and fix. mapfile would have merged them.
R="$TMP/notarepo"; rm -rf "$R"; mkdir -p "$R/pkg" "$R/.github/workflows"
printf 'package pkg\n' > "$R/pkg/plain.go"
wf_both
chk "a non-repo root refuses" "$(rc)" "2"
says "and says so rather than claiming no Go files" 'is not a git working tree'

repo c7b; go_plain; go_tagged integration
wf "$SC_INSTALL"; track
chk "an install line is not an invocation" "$(rc)" "2"
says "and it says it found nothing that lints anything" \
    'no staticcheck invocation found'

repo c7c; go_plain; go_tagged integration
wf_commented_out; track
chk "a commented-out invocation does not count" "$(rc)" "2"

# --- 8 SPELLINGS OF -tags THAT MUST BE READ --------------------------
repo c8; go_plain; go_tagged integration
wf 'staticcheck ./...' 'staticcheck -tags=integration ./...'; track
chk "-tags=x is read like -tags x" "$(rc)" "0"

repo c8b; go_plain; go_tagged integration
printf '//go:build e2e\n\npackage pkg\n\nfunc C() {}\n' > "$R/pkg/e2e.go"
wf 'staticcheck ./...' 'staticcheck -tags integration,e2e ./...'; track
chk "a comma list covers both terms" "$(rc)" "0"

repo c8c; go_plain; go_tagged integration
printf '//go:build e2e\n\npackage pkg\n\nfunc C() {}\n' > "$R/pkg/e2e.go"
wf 'staticcheck ./...' 'staticcheck -tags integration ./...'; track
chk "a NEW tag nothing lints is caught" "$(rc)" "1"

# --- 8d NOT EVERY //go:build PREFIX IS A CONSTRAINT -------------------
# `//go:buildfoo` is an ordinary comment. Matching the prefix without
# the space invents the term 'foo' and fails a tree that is fine.
repo c8d; go_plain
printf '//go:buildfoo bar\n\npackage pkg\n\nfunc C() {}\n' > "$R/pkg/odd.go"
wf_untagged_only; track
chk "a //go:build prefix without a space is not a constraint" "$(rc)" "0"

# --- 9 THE OPPOSITE DIRECTION ----------------------------------------
# Every case above asks whether the gate FIRES. A gate that refused
# everything would look identical from the inside, because all of them
# would still be green. This asks whether an ordinary healthy tree with
# a tag nobody thought about still passes.
repo c9; go_plain
printf '//go:build tools\n\npackage pkg\n\nfunc D() {}\n' > "$R/pkg/tools.go"
wf 'staticcheck ./...' 'staticcheck -tags tools ./...'; track
chk "an unfamiliar but covered tag passes" "$(rc)" "0"


# --- 11 A NAME IS NOT AN INVOCATION (#872) ---------------------------
# The gate shipped matching `staticcheck` on any line under
# .github/workflows/, and the steps it protects are NAMED after it.
# Measured before the fix: deleting `run: staticcheck ./...` and
# leaving only its step name still exited 0 with "full coverage", and
# the tagged-only fixture in the `named` shape — a CI that lints only
# the integration view — read clean. Each case below is that hole with
# one variable moved.
repo c11; go_plain; go_tagged integration
wf 'echo not-a-linter' 'staticcheck -tags integration ./...'
# The first step is named `Run staticcheck (view 1)` in two of the
# three shapes and runs something else entirely. The default view is
# absent, and nothing but the name says otherwise.
track
chk "a step NAME does not stand in for the untagged run" "$(rc)" "1"
says "and it names the missing default view" 'runs WITHOUT -tags'

repo c11b; go_plain; go_tagged integration
wf 'echo not-a-linter'
track
chk "a workflow whose only staticcheck is a step name is vacuous" "$(rc)" "2"
says "and it says it found nothing that lints anything" \
    'no staticcheck invocation found'

# --- 12 A TRAILING COMMENT IS NOT AN INVOCATION ----------------------
# `run: echo hi # staticcheck ./...` executes no linter. Whole-line
# comments were already covered (case 7c); the trailing form was not,
# and it reaches the same permissive verdict by one more door.
repo c12; go_plain; go_tagged integration
wf 'echo hi # staticcheck ./...' 'staticcheck -tags integration ./...'
track
chk "a trailing comment does not stand in for the untagged run" "$(rc)" "1"

# A `#` inside quotes is NOT a comment, and over-stripping would lose a
# real invocation. This is the preservation control for case 12.
repo c12b; go_plain; go_tagged integration
wf 'staticcheck ./... # noqa' 'staticcheck -tags integration ./...'
track
chk "a real invocation with a trailing comment still counts" "$(rc)" "0"

# --- 13 A QUOTED -tags VALUE IS STILL A TAG --------------------------
# `-tags "integration"` made the capture empty, so the invocation was
# filed as UNTAGGED — satisfying rule 1 while covering no term. That is
# case 11's hole reached through the flag parser instead of the matcher.
repo c13; go_plain; go_tagged integration
wf 'staticcheck ./...' 'staticcheck -tags "integration" ./...'
track
chk "a quoted -tags value is read as the tag" "$(rc)" "0"

repo c13b; go_plain; go_tagged integration
wf 'staticcheck -tags "integration" ./...'
track
chk "and a quoted-tag-only workflow is still missing the default view" "$(rc)" "1"

done

# The mutant fixtures below are `bare` unless a case says otherwise.
SHAPE=bare

# --- 10 MUTANTS: drive the absence of each rule ----------------------
# A rule that is never the reason a case fails is not being measured.
#
# THE MUTANT MUST STILL BE THE GATE. Each is written to $TMP, so the
# gate's `. "$HERE/workflow-shell-lines.sh"` would resolve to nothing
# there: the source fails, workflow_shell_lines is undefined, the
# invocation list comes back empty and the mutant exits 2 for want of a
# sibling rather than for want of the rule. Mutant B scored a FAIL that
# way and mutant A scored a spurious ok, because `!= 1` accepts 2. So
# the helper is copied alongside, and each verdict now keys on the
# MESSAGE as well as the code — a mutant refused by a different guard
# is not evidence about this one.
cp "$HERE/workflow-shell-lines.sh" "$TMP/workflow-shell-lines.sh"

mut_no_untagged="$TMP/mut-untagged.sh"
awk '/^if \[ "\$untagged" -eq 0 \]; then$/{skip=1} skip&&/^fi$/{skip=0;next} !skip' \
    "$GATE" > "$mut_no_untagged"
if bash -n "$mut_no_untagged" 2>/dev/null && ! grep -q 'runs WITHOUT -tags' "$mut_no_untagged"; then
    ok "mutant A built and really lacks the default-view rule"
    repo m1; go_plain; go_tagged integration; wf_tagged_only; track
    mo=$(LINT_TAG_ROOT="$R" bash "$mut_no_untagged" 2>&1); mrc=$?
    case "$mo" in
        *"no staticcheck invocation found"*)
            bad "mutant A refused for a DIFFERENT reason ($mo)" ;;
        *)  if [ "$mrc" = "0" ]; then ok "without it case 3 passes — case 3 is live"
            else bad "mutant A still failed (rc=$mrc); case 3 is not measuring that rule"; fi ;;
    esac
else
    bad "could not build mutant A; case 3 is unverified"
fi

mut_no_compound="$TMP/mut-compound.sh"
awk '/^if \[ "\${#compound\[@\]}" -ne 0 \]; then$/{skip=1} skip&&/^fi$/{skip=0;next} !skip' \
    "$GATE" > "$mut_no_compound"
if bash -n "$mut_no_compound" 2>/dev/null && ! grep -q 'cannot judge lint coverage' "$mut_no_compound"; then
    ok "mutant B built and really lacks the compound refusal"
    repo m2; go_plain; go_tagged 'integration && linux'; wf_both; track
    mo=$(LINT_TAG_ROOT="$R" bash "$mut_no_compound" 2>&1); mrc=$?
    case "$mo" in
        *"no staticcheck invocation found"*)
            bad "mutant B refused for a DIFFERENT reason ($mo)" ;;
        *)  if [ "$mrc" != "2" ]; then ok "without it a compound constraint CLEARS — case 5 is live"
            else bad "mutant B still refused; case 5 is not measuring the refusal"; fi ;;
    esac
else
    bad "could not build mutant B; case 5 is unverified"
fi

# MUTANT C: the invocation matcher, back to the shape #872 rejected.
# The gate shipped grepping every line under .github/workflows/ for
# `staticcheck`, which a step NAME satisfies. Restore exactly that and
# the `named` tagged-only fixture — a CI that lints only the integration
# view — must go from a reported gap back to a reported clean. If this
# mutant does not clear, the new cases are not measuring the fix.
mut_grep_all="$TMP/mut-grepall.sh"
awk '
/^    workflow_shell_lines "\$WORKFLOWS" \|$/ {
    print "    grep -rhE \047(^|[[:space:]|;&(])staticcheck[[:space:]]\047 \"$WORKFLOWS\" 2>/dev/null |"
    print "        sed \047s/^[[:space:]]*//\047 | grep -v \047^#\047 |"
    print "        cat |"
    next
}
{ print }' "$GATE" > "$mut_grep_all"
# The landing proof is on CODE, not on any string that could also occur
# in prose: the mutant must DIFFER from the gate, must no longer call
# the extractor, and must carry the restored grep on a non-comment line.
# The sibling suite made exactly that mistake — it asserted a mutation
# had landed by grepping for a string the file's own header quotes —
# which is this pull request's finding turned on its own harness.
mut_code="$(grep -v '^[[:space:]]*#' "$mut_grep_all")"
c_built=1
cmp -s "$GATE" "$mut_grep_all" && c_built=0
case "$mut_code" in *'workflow_shell_lines "$WORKFLOWS"'*) c_built=0 ;; esac
case "$mut_code" in *'grep -rhE'*) : ;; *) c_built=0 ;; esac
if [ "$c_built" -eq 1 ] && bash -n "$mut_grep_all" 2>/dev/null; then
    ok "mutant C built, differs from the gate, and really restores the line-wide grep"
    SHAPE=named
    repo m4; go_plain; go_tagged integration; wf_tagged_only; track
    mo=$(LINT_TAG_ROOT="$R" bash "$mut_grep_all" 2>&1); mrc=$?
    if [ "$mrc" = "0" ]; then
        ok "with it, a tagged-only CI reads CLEAN — the new cases are live"
    else
        bad "mutant C still reported a gap (rc=$mrc); case 11 is not measuring the matcher"
    fi
    # And the unmutated gate on the same fixture, so the pair isolates
    # the matcher and nothing else.
    if [ "$(rc)" = "1" ]; then ok "and the real gate calls the same fixture a gap"
    else bad "the real gate cleared the tagged-only named fixture"; fi
else
    bad "could not build mutant C; case 11 is unverified"
fi

# --- the pinned-library exemption (D21) -------------------------------
#
# internal/dhcp-golib/ is a copy of another repository at a fixed SHA,
# linted in that repository's own lane. Its build tags are not this
# repo's obligation, and editing a file there to satisfy this gate
# would falsify internal/dhcp-golib/SOURCE. Three cases, one per way an
# exemption goes wrong.
SHAPE=bare
go_vendored() {
    mkdir -p "$R/internal/dhcp-golib/runtime"
    printf '//go:build %s\n\npackage runtime\n\nfunc C() {}\n' "$1" \
        > "$R/internal/dhcp-golib/runtime/tagged.go"
}

# 1. It applies: a term carried ONLY by the pinned copy is not a gap.
repo v1; go_plain; go_tagged integration; go_vendored linux; wf_both; track
chk "a build tag carried only by the pinned library copy is not a gap" "$(rc)" "0"

# 2. It is anchored at the repository root. The same directory name
#    deeper in the tree is this repo's own source and still counts.
repo v2; go_plain; wf_both; track
mkdir -p "$R/pkg/internal/dhcp-golib"
printf '//go:build linux\n\npackage x\n\nfunc D() {}\n' > "$R/pkg/internal/dhcp-golib/t.go"
track
chk "the same path deeper in the tree is NOT exempt" "$(rc)" "1"

# 3. A universal gate is satisfied by emptying its domain. If every Go
#    file were exempt this gate would report full coverage having read
#    none, so it must refuse: exit 2 is "cannot see", not "clean".
repo v3; wf_both; go_vendored linux; track
chk "a tree whose only Go files are exempt is a refusal, not a pass" "$(rc)" "2"

# And the exemption is ANNOUNCED, on a run that passed: an exemption a
# green run does not mention is one nobody re-examines.
repo v4; go_plain; go_tagged integration; go_vendored linux; wf_both; track
says "the exempt count is announced on a passing run" "not inspected here"

# UNMUTATED CONTROL. A mutant that fails to run at all produces the same
# verdict as one that runs and behaves differently.
for SHAPE in bare named block; do
    repo m3; go_plain; go_tagged integration; wf_both; track
    chk "the unmutated gate still passes the control tree" "$(rc)" "0"
done
SHAPE=bare

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
