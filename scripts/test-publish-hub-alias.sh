#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for publish-hub-alias.sh (#972), with the TRANSPORT STUBBED.
#
# WHAT THIS BUYS. Inline in the release workflow, the digest-equality
# refusal could only be reached by a registry actually serving a
# different manifest under the alias, so the branch that stops a rebuild
# from shipping had never executed. Here `oras`, `docker` and `cosign`
# are stub scripts on PATH, each reading what it should do out of the
# environment, so every outcome the real step can reach is driven on
# every lane run:
#
#   - the copy fails
#   - the copy succeeds and the alias digest equals the signed digest
#   - the copy succeeds and the alias digest DIFFERS (the rebuild)
#   - the registry returns no digest at all
#   - the digests match but the signature does not verify through the
#     alias name (the lost referrer)
#
# THE STUBS RECORD WHAT THEY WERE CALLED WITH, and the cases assert on
# that, because "it exited 0" cannot tell a copy that ran from a copy
# that was skipped. `oras cp -r` is asserted to carry `-r`: without it
# the referrers stay behind, the signature does not travel, and the only
# thing that notices is the verify at the end of the script.
#
# ORDER IS A CLAIM TOO. The rebuild case asserts `not_logged cosign.args`:
# a mismatched digest stops BEFORE cosign runs. A script that verified
# first and compared afterwards would pass every exit-code assertion here
# while reporting a rebuilt manifest as verified.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

SUBJECT="$(cd "$(dirname "$0")" && pwd)/publish-hub-alias.sh"
guarded_tmpdir TMP

failures=0
n=0

BIN="$TMP/bin"
mkdir -p "$BIN"

cat > "$BIN/oras" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/oras.args"
exit "${ORAS_RC:-0}"
STUB

cat > "$BIN/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/docker.args"
printf '%s' "${ALIAS_DIGEST_OUT-}"
[ -n "${ALIAS_DIGEST_OUT-}" ] && echo
exit "${DOCKER_RC:-0}"
STUB

cat > "$BIN/cosign" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/cosign.args"
exit "${COSIGN_RC:-0}"
STUB

chmod 755 "$BIN/oras" "$BIN/docker" "$BIN/cosign"

SIGNED="sha256:1111111111111111111111111111111111111111111111111111111111111111"
REBUILT="sha256:2222222222222222222222222222222222222222222222222222222222222222"
SRC="docker.io/claymore666/net-dhcp:v2.2.0"
DST="docker.io/claymore666/docker-net-dhcp:v2.2.0"

# run NAME WANT_EXIT WANT_GREP -- env assignments... -- args...
run() {
    local name="$1" want_exit="$2" want_grep="$3"; shift 3
    [ "$1" = "--" ] && shift
    local -a envs=()
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
    [ "${1-}" = "--" ] && shift
    n=$((n + 1))
    rm -rf "$TMP/log"
    mkdir -p "$TMP/log"
    ( PATH="$BIN:$PATH" STUB_LOG="$TMP/log" \
      COSIGN_IDENTITY_REGEXP='^test-identity$' \
      env "${envs[@]}" bash "$SUBJECT" "$@" ) > "$TMP/out" 2>&1
    local got=$?
    local ok=1
    [ "$got" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

assert() { # NAME CONDITION-DESCRIPTION  (evaluated by the caller)
    n=$((n + 1))
    if [ "$2" = "1" ]; then
        echo "PASS: $1"
    else
        echo "FAIL: $1"
        failures=$((failures + 1))
    fi
}

logged() { # FILE PATTERN -> 1 when present
    if [ -f "$TMP/log/$1" ] && grep -qF -- "$2" "$TMP/log/$1"; then echo 1; else echo 0; fi
}
not_logged() { # FILE -> 1 when the tool was never called
    if [ -f "$TMP/log/$1" ]; then echo 0; else echo 1; fi
}

# ---------------------------------------------------------------- happy

run "the alias is the signed manifest and verifies under its own name" 0 \
    "verifies under its own name" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" -- --expect-digest "$SIGNED" "$SRC" "$DST"

assert "the copy actually ran, source then alias" \
    "$(logged oras.args "cp -r $SRC $DST")"
assert "the copy carried -r, so the referrers travel with the manifest" \
    "$(logged oras.args '-r')"
assert "the alias digest is re-derived through the ALIAS name" \
    "$(logged docker.args 'claymore666/docker-net-dhcp:v2.2.0')"
assert "the inspect drops the docker.io host prefix imagetools does not want" \
    "$(if grep -q 'docker.io' "$TMP/log/docker.args"; then echo 0; else echo 1; fi)"
assert "cosign verifies the alias repository at the derived digest" \
    "$(logged cosign.args "claymore666/docker-net-dhcp@$SIGNED")"

# ------------------------------------------------------------- the rebuild

run "a different digest under the alias is refused as a rebuild" 1 \
    "this is a rebuild and not a copy" \
    -- "ALIAS_DIGEST_OUT=$REBUILT" -- --expect-digest "$SIGNED" "$SRC" "$DST"

assert "the rebuild refusal names both digests" \
    "$(if grep -q "$REBUILT" "$TMP/out" && grep -q "$SIGNED" "$TMP/out"; then echo 1; else echo 0; fi)"
assert "a rebuilt manifest is never handed to cosign" \
    "$(not_logged cosign.args)"

# ------------------------------------------------------------- transport

run "a failed copy fails, and says nothing was published under the alias" 1 \
    "Nothing was published under the alias name" \
    -- "ORAS_RC=1" "ALIAS_DIGEST_OUT=$SIGNED" -- --expect-digest "$SIGNED" "$SRC" "$DST"

assert "a failed copy is not followed by an inspect" \
    "$(not_logged docker.args)"

run "an empty digest from the registry cannot be judged" 2 \
    "cannot say whether the alias is the signed manifest" \
    -- "ALIAS_DIGEST_OUT=" -- --expect-digest "$SIGNED" "$SRC" "$DST"

run "equal digests with a signature that does not verify through the alias fails" 1 \
    "does not verify through the alias name" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" "COSIGN_RC=1" -- --expect-digest "$SIGNED" "$SRC" "$DST"

# --------------------------------------------------------------- refusals

run "no expected digest is a refusal, not a copy on trust" 2 \
    "copying without that comparison" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" -- "$SRC" "$DST"

assert "nothing is copied when there is no digest to compare against" \
    "$(not_logged oras.args)"

run "a missing alias argument is a refusal" 2 "no ALIAS reference given" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" -- --expect-digest "$SIGNED" "$SRC"

run "a missing source argument is a refusal" 2 "no SOURCE reference given" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" -- --expect-digest "$SIGNED"

run "an extra argument is a refusal, because the order carries meaning" 2 \
    "unexpected extra argument" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" -- --expect-digest "$SIGNED" "$SRC" "$DST" extra

run "an unknown option is a refusal" 2 "unknown option" \
    -- "ALIAS_DIGEST_OUT=$SIGNED" -- --nope --expect-digest "$SIGNED" "$SRC" "$DST"

# ------------------------------------------- the workflow calls it this way
#
# A self-test that only ever calls the script the way the script's own
# author would is bounded by that author. The two release jobs are the
# only production callers, so their call is asserted here against the
# workflow text.
#
# THE ARGUMENT THE REFUSAL DEPENDS ON IS PART OF THE CALL. The first
# version of this block counted alias-last lines only, and measured:
# strip `--expect-digest "${HUB_DIGEST}"` from both call sites and every
# assertion here still passed, all four gates still exited 0, and the
# script then refuses at release time -- after both registries hold
# :vX.Y.Z and after the signature, which is the half-published point
# the runbook documents. An expectation the caller never states is an
# expectation that cannot be violated.
#
# The digest is matched as a VARIABLE, not as the name HUB_DIGEST: what
# the script needs is the digest this run signed, and the step binds it
# from the signing step's output. The two shapes are asserted
# separately so a failure says which half is missing.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WF="$ROOT/.github/workflows/release.yml"
calls=$(grep -c 'publish-hub-alias\.sh .*"docker\.io/\${HUB_ALIAS}:\${[A-Z_]*}"$' "$WF" || true)
assert "both release jobs call it with the alias reference last" \
    "$(if [ "$calls" -eq 2 ]; then echo 1; else echo 0; fi)"

expects=$(grep -c 'publish-hub-alias\.sh --expect-digest "\${[A-Z_]*}"' "$WF" || true)
assert "both release jobs hand it the digest to expect" \
    "$(if [ "$expects" -eq 2 ]; then echo 1; else echo 0; fi)"

# And that the digest they hand it is the one the signing step output,
# not some other variable that happens to be in scope. Scoped to the
# calling STEP: that binding also appears in steps this script never
# runs in, so a file-wide count would be answered by the wrong step.
signed=$(awk '
    /^      - name:/ { env_ok = 0 }
    /HUB_DIGEST: \$\{\{ steps\.signimg\.outputs\.hub_digest \}\}/ { env_ok = 1 }
    /publish-hub-alias\.sh --expect-digest/ { if (env_ok) n++ }
    END { print n + 0 }
' "$WF")
assert "the digest they hand it is the digest the signing step published" \
    "$(if [ "$signed" -eq 2 ]; then echo 1; else echo 0; fi)"

echo
if [ "$failures" -eq 0 ]; then
    echo "test-publish-hub-alias: $n assertions, all passed"
    exit 0
fi
echo "test-publish-hub-alias: $n assertions, $failures failed"
exit 1
