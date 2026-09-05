#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-direct-invocations.sh, and for the two runbook blocks it
# gates.
#
# TWO KINDS OF CASE, and the second kind is the point.
#
# Section A drives the GATE: the real tree is the control, and each mutant is
# planted in a COPY of the real tree carrying the real index modes, so the
# thing under test is the shipped parser and not a fixture shaped to please
# it.
#
# Section B drives the RUNBOOK TEXT ITSELF. It reads the fenced block out of
# `docs/release-runbook.md`, substitutes the placeholder version, and
# executes it. That is deliberate and it is what the earlier round got
# wrong: the defeat row that claimed to have run "the runbook's command"
# wrote it as `bash scripts/release-body.sh …`, and the runbook does not.
# Transcribing a documented command into a test re-tests the transcription;
# only the file's own bytes can show that the file's own bytes work.
#
# The scripts come out of the INDEX (`git checkout-index`), so the mode the
# block runs against is the mode that ships, not whatever this working copy
# happens to carry.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
GATE="$HERE/check-direct-invocations.sh"
RUNBOOK="$ROOT/docs/release-runbook.md"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

TMPS=()
cleanup() { for d in ${TMPS+"${TMPS[@]}"}; do [ -n "$d" ] && rm -rf "$d"; done; }
trap cleanup EXIT

# A copy of the tracked tree, as a git repository: WORKING-TREE content with
# INDEX modes, which is the pair the gate reads. Copying the index content
# instead would test the last commit rather than the change in hand, and
# copying the filesystem modes would lose the one bit the finding is about.
mktree() {
    local d line mode rest
    d=$(mktemp -d) || return 1
    TMPS+=("$d")
    while IFS= read -r line; do
        mode=${line%% *}
        rest=${line#*	}
        [ -f "$ROOT/$rest" ] || continue
        mkdir -p "$d/$(dirname "$rest")"
        cp -- "$ROOT/$rest" "$d/$rest" || return 1
        case "$mode" in
            100755) chmod 755 "$d/$rest" ;;
            *)      chmod 644 "$d/$rest" ;;
        esac
    done < <(git -C "$ROOT" ls-files -s)
    git -C "$d" init -q >/dev/null 2>&1 || return 1
    git -C "$d" add -A >/dev/null 2>&1 || return 1
    printf '%s' "$d"
}

# A mutant that fails to apply is not a mutant that was killed. Every planted
# edit below runs through this, so a runbook line that moved shows up as a
# red case rather than a case that quietly did not happen.
planted() { # <name> <rc-of-the-planting>
    [ "$2" -eq 0 ] && return 0
    no "$1 (the mutant could not be planted; the text it edits has moved)"
    return 1
}

# <name> <want-exit> <root> [<expect>] [<forbid>]
gate_case() {
    local name="$1" want="$2" root="$3" expect="${4:-}" forbid="${5:-}" out rc
    out=$(DIRECT_INV_ROOT="$root" bash "$GATE" 2>&1)
    rc=$?
    if [ "$rc" != "$want" ]; then
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" | head -6 >&2
        return
    fi
    if [ -n "$expect" ] && ! printf '%s' "$out" | grep -F -- "$expect" >/dev/null; then
        no "$name (exit $rc as wanted, but the output does not name '$expect')"
        printf '      %s\n' "$out" | head -6 >&2
        return
    fi
    if [ -n "$forbid" ] && printf '%s' "$out" | grep -F -- "$forbid" >/dev/null; then
        no "$name (exit $rc as wanted, but the output names '$forbid', which it must not)"
        printf '      %s\n' "$out" | head -6 >&2
        return
    fi
    ok "$name"
}

# --- Section A: the gate -----------------------------------------------

# A1. THE CONTROL. Without it every refusal below is satisfied by a gate
#     that fails on everything.
gate_case "the shipped tree passes" 0 "$ROOT" "PASS  "

# A1b. The preservation control for the interpreter-prefixed callers. Ten of
#      this tree's scripts are only ever reached as `bash scripts/x.sh` or
#      sourced, and several of those are mode 100644 on purpose. The gate
#      must not demand a bit from them; A1 passing with those files in the
#      tree is that proof, and naming one makes it explicit.
if [ "$(git -C "$ROOT" ls-files -s scripts/branch-glob.sh | cut -d' ' -f1)" = "100644" ]; then
    gate_case "a sourced-only 100644 script is not demanded to be executable" 0 "$ROOT" "PASS  " "branch-glob.sh"
else
    no "scripts/branch-glob.sh is no longer 100644, so the preservation control has no subject"
fi

# A2. THE DEFECT, RESTORED. release-body.sh shipped 100644 and the runbook
#     invokes it directly.
NAME="the executable bit dropped in the index is caught"
t=$(mktree)
git -C "$t" update-index --chmod=-x scripts/release-body.sh
if planted "$NAME" $?; then
    gate_case "$NAME" 1 "$t" "is mode 100644 in the index"
fi

# A2b. THE LINE-SCAN CONTROL. A workflow names script PATHS in places that
#      are not shell -- `sparse-checkout:` lists one, and release.yml's list
#      names release-body.sh today. A gate that scanned lines instead of
#      `run:` scalars would read those as invocations and demand a bit from
#      a file the workflow never executes. The fixture adds a 100644 script
#      that nothing runs to that list; the answer must not change.
NAME="a script named in a sparse-checkout list is not an invocation"
t=$(mktree)
python3 - "$t/.github/workflows/release.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p, encoding='utf-8').read()
old = "            scripts/release-body.sh\n"
assert s.count(old) == 1, "release.yml no longer sparse-checks-out release-body.sh"
open(p, 'w', encoding='utf-8').write(s.replace(old, old + "            scripts/branch-glob.sh\n"))
PY
if planted "$NAME" $?; then
    git -C "$t" add -A >/dev/null 2>&1
    gate_case "$NAME" 0 "$t" "PASS  "
fi

# A3. The propagation removed from step 9's block: the refusal prints and
#     `git tag` runs anyway.
NAME="a documented check that does not stop the block is caught"
t=$(mktree)
python3 - "$t/docs/release-runbook.md" <<'PY'
import sys
p = sys.argv[1]
s = open(p, encoding='utf-8').read()
old = "scripts/release-body.sh vX.Y.Z RELEASE_NOTES.md >/dev/null &&"
assert s.count(old) == 1, "the runbook line the mutant edits is not there"
open(p, 'w', encoding='utf-8').write(s.replace(old, old[:-3]))
PY
if planted "$NAME" $?; then
    git -C "$t" add -A >/dev/null 2>&1
    gate_case "$NAME" 1 "$t" "nothing carrying its"
fi

# A4. The alternative the failure message offers has to work, or the message
#     is advice the gate rejects.
NAME="a block opening with set -e satisfies the propagation rule"
t=$(mktree)
python3 - "$t/docs/release-runbook.md" <<'PY'
import sys
p = sys.argv[1]
s = open(p, encoding='utf-8').read()
old = """   git checkout main && git pull --ff-only &&"""
assert s.count(old) == 1
s = s.replace(old, """   set -e
   git checkout main && git pull --ff-only""")
s = s.replace("scripts/release-body.sh vX.Y.Z RELEASE_NOTES.md >/dev/null &&",
              "scripts/release-body.sh vX.Y.Z RELEASE_NOTES.md >/dev/null")
s = s.replace("""   git tag -s vX.Y.Z -m "vX.Y.Z — <one-liner>" &&   # signed (#175)""",
              """   git tag -s vX.Y.Z -m "vX.Y.Z — <one-liner>"   # signed (#175)""")
open(p, 'w', encoding='utf-8').write(s)
PY
if planted "$NAME" $?; then
    git -C "$t" add -A >/dev/null 2>&1
    gate_case "$NAME" 0 "$t" "PASS  "
fi

# A5. The workflow arm. integration-arm64.yml runs scripts/integration-timing.sh
#     from a `run:` block with no interpreter; clearing its bit must be seen.
NAME="a workflow run: invocation of a non-executable script is caught"
t=$(mktree)
git -C "$t" update-index --chmod=-x scripts/integration-timing.sh
if planted "$NAME" $?; then
    gate_case "$NAME" 1 "$t" "integration-arm64.yml"
fi

# A6. AN EMPTIED DOMAIN IS NOT A PASS. Remove the file that carries every
#     documented invocation and rule 2 has nothing to judge.
NAME="no documented invocation left refuses rather than passes"
t=$(mktree)
git -C "$t" rm -q --cached docs/release-runbook.md >/dev/null 2>&1 && rm -f "$t/docs/release-runbook.md"
if planted "$NAME" $?; then
    gate_case "$NAME" 2 "$t" "an empty domain is not a pass"
fi

# A7. The other empty domain: a tree with no direct invocation at all.
NAME="a tree with no direct invocation refuses"
t=$(mktemp -d); TMPS+=("$t")
git -C "$t" init -q
if planted "$NAME" $?; then
    gate_case "$NAME" 2 "$t" "resolving to a tracked file"
fi

# --- Section B: the runbook's own text, executed -------------------------

# Pull one fenced block out of the shipped runbook. `marker` picks which of
# the two release-body blocks; finding anything other than exactly one is a
# failure of this test, not a pass of the subject.
extract_block() { # <marker>
    python3 - "$RUNBOOK" "$1" <<'PY'
import re, sys
marker = sys.argv[2]
lines = open(sys.argv[1], encoding='utf-8').read().splitlines()
blocks, i = [], 0
while i < len(lines):
    m = re.match(r'^(\s*)```(\w*)\s*$', lines[i])
    if m:
        indent, body, j = m.group(1), [], i + 1
        while j < len(lines) and not re.match(r'^\s*```\s*$', lines[j]):
            body.append(lines[j][len(indent):] if lines[j].startswith(indent) else lines[j].lstrip())
            j += 1
        blocks.append('\n'.join(body))
        i = j + 1
    else:
        i += 1
hit = [b for b in blocks if 'release-body.sh' in b and marker in b]
if len(hit) != 1:
    sys.stderr.write("found %d runbook blocks with release-body.sh and %r\n" % (len(hit), marker))
    sys.exit(3)
sys.stdout.write(hit[0].replace('vX.Y.Z', 'v2.0.0'))
PY
}

# A sandbox holding the SHIPPED script at its SHIPPED mode, plus a notes file
# and, optionally, a `git` that records instead of acting.
mksandbox() { # <notes-content>
    local d
    d=$(mktemp -d) || return 1
    TMPS+=("$d")
    mkdir -p "$d/scripts" "$d/bin"
    git -C "$ROOT" checkout-index --prefix="$d/" -- scripts/release-body.sh >/dev/null 2>&1 || return 1
    printf '%s' "$1" > "$d/RELEASE_NOTES.md"
    cat > "$d/bin/git" <<'GITEOF'
#!/bin/sh
# Records what the runbook block would have done. Never touches a repository:
# the question is whether these commands RUN, and a real git would answer it
# by pushing a tag.
printf '%s\n' "$*" >> "$GITLOG"
exit 0
GITEOF
    chmod +x "$d/bin/git"
    printf '%s' "$d"
}

NOTES_DECORATED='## v2.0.0 (unreleased)

Notes that a `## v2.0.0` heading match cannot find.

## v1.9.0

older
'
NOTES_GOOD='## v2.0.0

The release everyone waited for.

## v1.9.0

older
'

# B1. Step 4, exactly as printed, against the heading shape this repository
#     carries between releases. It must REFUSE -- which means it must first
#     have RUN. mode 100644 gives a shell error and no refusal, and that is
#     the difference this case exists to see.
#
#     AND IT MUST SAY SO IN ITS EXIT STATUS. Until this round the block was
#     `release-body.sh … | head -20`, and a pipeline reports for `head`: it
#     exited 0 on a refusal and 0 on notes that assemble, while the prose
#     beside it promised two outcomes. A block whose refusal only reaches a
#     human's eye is not a check anyone can build on, and the operator
#     copying it into a script would have got the placeholder release the
#     whole chain exists to prevent.
blk=$(extract_block 'head -20')
if [ -z "$blk" ]; then
    no "step 4's block could not be extracted from the runbook"
else
    sb=$(mksandbox "$NOTES_DECORATED")
    out=$( (cd "$sb" && eval "$blk") 2>&1 )
    rc=$?
    if [ "$rc" -eq 0 ]; then
        no "step 4's block exited 0 on a decorated heading it refused"
        printf '      %s\n' "$out" | head -4 >&2
    elif printf '%s' "$out" | grep -F 'Release body refused' >/dev/null &&
         printf '%s' "$out" | grep -F '## v2.0.0 (unreleased)' >/dev/null; then
        ok "step 4's block, as printed, executes, refuses the decorated heading and exits non-zero"
    else
        no "step 4's block, as printed, did not execute and refuse"
        printf '      %s\n' "$out" | head -4 >&2
    fi
fi

# B1b. THE OTHER DIRECTION. A block that always exits non-zero satisfies B1
#      and is useless; on notes that assemble, step 4 must exit 0 AND print
#      the section, because the operator reads the preview before signing
#      the notes off.
if [ -n "${blk:-}" ]; then
    sb=$(mksandbox "$NOTES_GOOD")
    out=$( (cd "$sb" && eval "$blk") 2>&1 )
    rc=$?
    if [ "$rc" -ne 0 ]; then
        no "step 4's block exited $rc on notes that assemble"
        printf '      %s\n' "$out" | head -4 >&2
    elif ! printf '%s' "$out" | grep -F 'The release everyone waited for' >/dev/null; then
        no "step 4's block exited 0 without previewing the section"
        printf '      %s\n' "$out" | head -4 >&2
    else
        ok "step 4's block previews the section and exits 0 when the notes assemble"
    fi
fi

# B2. Step 9's block, as printed, with a refusal in the middle of it. The
#     tag must not be pushed. The recorded git log is the evidence: this is
#     what the operator's terminal did, not what the block says it does.
blk=$(extract_block 'git push origin')
if [ -z "$blk" ]; then
    no "step 9's block could not be extracted from the runbook"
else
    sb=$(mksandbox "$NOTES_DECORATED")
    log="$sb/git.log"
    : > "$log"
    out=$( (cd "$sb" && GITLOG="$log" PATH="$sb/bin:$PATH" eval "$blk") 2>&1 )
    rc=$?
    if [ "$rc" -eq 0 ]; then
        no "step 9's block exited 0 with a refused release body"
    elif grep -E '(^| )tag( |$)' "$log" >/dev/null || grep -E '(^| )push( |$)' "$log" >/dev/null; then
        no "step 9's block pushed the tag after the release body was refused"
        printf '      git calls: %s\n' "$(tr '\n' '|' < "$log")" >&2
    elif ! grep -F 'checkout main' "$log" >/dev/null; then
        no "step 9's block never reached its first command, so the case proves nothing"
        printf '      %s\n' "$out" | head -4 >&2
    else
        ok "step 9's block stops at the refusal, before git tag and git push"
    fi
fi

# B3. THE SAFE SHAPE. Without it, B2 is satisfied by a block that always
#     stops -- a typo in the extraction, an `exit` planted anywhere -- and
#     "the tag was not pushed" would be measuring the wrong thing.
if [ -n "${blk:-}" ]; then
    sb=$(mksandbox "$NOTES_GOOD")
    log="$sb/git.log"
    : > "$log"
    out=$( (cd "$sb" && GITLOG="$log" PATH="$sb/bin:$PATH" eval "$blk") 2>&1 )
    rc=$?
    if [ "$rc" -ne 0 ]; then
        no "step 9's block failed on notes that assemble (exit $rc)"
        printf '      %s\n' "$out" | head -4 >&2
    elif ! grep -E '(^| )tag( |$)' "$log" >/dev/null || ! grep -E '(^| )push( |$)' "$log" >/dev/null; then
        no "step 9's block did not reach git tag / git push on notes that assemble"
        printf '      git calls: %s\n' "$(tr '\n' '|' < "$log")" >&2
    else
        ok "step 9's block runs through to the tag when the notes assemble"
    fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
