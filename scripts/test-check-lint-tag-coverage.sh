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
# Usage: bash scripts/test-check-lint-tag-coverage.sh

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-lint-tag-coverage.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok   $1"; }
bad() { fail=$((fail+1)); echo "  FAIL $1"; }
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
# Deliberately not `run | grep -q`. Under `set -o pipefail` that reads
# 141, not grep's verdict: `grep -q` exits on the first match and the
# producer dies of SIGPIPE, so a passing assertion reports failure. It
# cost five false FAILs when this file was first run, on a repository
# that ships scripts/check-pipefail-consumers.sh for that exact shape.
says() {
    local out; out="$(run)"
    case "$out" in
        *"$2"*) ok "$1" ;;
        *) bad "$1 — the report does not contain '$2':"$'\n'"$out" ;;
    esac
}

# The two workflow shapes, written once so no case can differ from the
# others by accident.
wf_both() {
    cat > "$R/.github/workflows/test.yaml" <<'EOF'
jobs:
  staticcheck:
    steps:
      - run: go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
      - run: staticcheck ./...
      - run: staticcheck -tags integration ./...
EOF
}
wf_untagged_only() {
    cat > "$R/.github/workflows/test.yaml" <<'EOF'
jobs:
  staticcheck:
    steps:
      - run: go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
      - run: staticcheck ./...
EOF
}
wf_tagged_only() {
    cat > "$R/.github/workflows/test.yaml" <<'EOF'
jobs:
  staticcheck:
    steps:
      - run: staticcheck -tags integration ./...
EOF
}
go_plain()  { printf 'package pkg\n\nfunc A() {}\n' > "$R/pkg/plain.go"; }
go_tagged() { printf '//go:build %s\n\npackage pkg\n\nfunc B() {}\n' "$1" > "$R/pkg/tagged.go"; }

echo "1..N check-lint-tag-coverage"

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
cat > "$R/.github/workflows/test.yaml" <<'EOF'
jobs:
  staticcheck:
    steps:
      - run: go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
EOF
track
chk "an install line is not an invocation" "$(rc)" "2"
says "and it says it found nothing that lints anything" \
    'no staticcheck invocation found'

repo c7c; go_plain; go_tagged integration
printf 'jobs:\n  x:\n    steps:\n      # - run: staticcheck ./...\n' \
    > "$R/.github/workflows/test.yaml"
track
chk "a commented-out invocation does not count" "$(rc)" "2"

# --- 8 SPELLINGS OF -tags THAT MUST BE READ --------------------------
repo c8; go_plain; go_tagged integration
printf 'jobs:\n  x:\n    steps:\n      - run: staticcheck ./...\n      - run: staticcheck -tags=integration ./...\n' \
    > "$R/.github/workflows/test.yaml"
track
chk "-tags=x is read like -tags x" "$(rc)" "0"

repo c8b; go_plain; go_tagged integration
printf '//go:build e2e\n\npackage pkg\n\nfunc C() {}\n' > "$R/pkg/e2e.go"
printf 'jobs:\n  x:\n    steps:\n      - run: staticcheck ./...\n      - run: staticcheck -tags integration,e2e ./...\n' \
    > "$R/.github/workflows/test.yaml"
track
chk "a comma list covers both terms" "$(rc)" "0"

repo c8c; go_plain; go_tagged integration
printf '//go:build e2e\n\npackage pkg\n\nfunc C() {}\n' > "$R/pkg/e2e.go"
printf 'jobs:\n  x:\n    steps:\n      - run: staticcheck ./...\n      - run: staticcheck -tags integration ./...\n' \
    > "$R/.github/workflows/test.yaml"
track
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
printf 'jobs:\n  x:\n    steps:\n      - run: staticcheck ./...\n      - run: staticcheck -tags tools ./...\n' \
    > "$R/.github/workflows/test.yaml"
track
chk "an unfamiliar but covered tag passes" "$(rc)" "0"

# --- 10 MUTANTS: drive the absence of each rule ----------------------
# A rule that is never the reason a case fails is not being measured.
mut_no_untagged="$TMP/mut-untagged.sh"
awk '/^if \[ "\$untagged" -eq 0 \]; then$/{skip=1} skip&&/^fi$/{skip=0;next} !skip' \
    "$GATE" > "$mut_no_untagged"
if bash -n "$mut_no_untagged" 2>/dev/null && ! grep -q 'runs WITHOUT -tags' "$mut_no_untagged"; then
    ok "mutant A built and really lacks the default-view rule"
    repo m1; go_plain; go_tagged integration; wf_tagged_only; track
    LINT_TAG_ROOT="$R" bash "$mut_no_untagged" >/dev/null 2>&1
    if [ "$?" != "1" ]; then ok "without it case 3 passes — case 3 is live"
    else bad "mutant A still failed; case 3 is not measuring that rule"; fi
else
    bad "could not build mutant A; case 3 is unverified"
fi

mut_no_compound="$TMP/mut-compound.sh"
awk '/^if \[ "\${#compound\[@\]}" -ne 0 \]; then$/{skip=1} skip&&/^fi$/{skip=0;next} !skip' \
    "$GATE" > "$mut_no_compound"
if bash -n "$mut_no_compound" 2>/dev/null && ! grep -q 'cannot judge lint coverage' "$mut_no_compound"; then
    ok "mutant B built and really lacks the compound refusal"
    repo m2; go_plain; go_tagged 'integration && linux'; wf_both; track
    LINT_TAG_ROOT="$R" bash "$mut_no_compound" >/dev/null 2>&1
    if [ "$?" != "2" ]; then ok "without it a compound constraint CLEARS — case 5 is live"
    else bad "mutant B still refused; case 5 is not measuring the refusal"; fi
else
    bad "could not build mutant B; case 5 is unverified"
fi

# UNMUTATED CONTROL. A mutant that fails to run at all produces the same
# verdict as one that runs and behaves differently.
repo m3; go_plain; go_tagged integration; wf_both; track
chk "the unmutated gate still passes the control tree" "$(rc)" "0"

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
