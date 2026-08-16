#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-good-first-issues.sh (#537), driven
# through the GFI_README / GFI_BADGE / GFI_GH seams against synthetic
# files and a stub `gh`. Nothing here touches the network.
#
# The cases that matter are the 2s. This gate's whole reason to exist is
# that a claim about live tracker state decays silently, so a version of
# it that answers 0 when it cannot actually look would reproduce the
# original bug with extra steps. "The API returned nothing" must never
# be read as "the label has no issues" — absent data is not a zero.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-good-first-issues.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

URL='https://github.com/claymore666/docker-net-dhcp/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22'
OTHER='https://github.com/claymore666/docker-net-dhcp/issues?q=is%3Aissue+is%3Aopen+label%3A%22starter+task%22'

mk_readme() { printf 'Pick up a [`good first issue`](%s) to start.\n' "$1" > "$2"; }
mk_badge() {  # <url> <cited-text> <out>
    python3 - "$1" "$2" "$3" <<'PY'
import json, sys
url, cited, out = sys.argv[1], sys.argv[2], sys.argv[3]
json.dump({"small_tasks_status": "Met",
           "small_tasks_justification": f"The tracker carries a good first issue label ({url}), seeded with {cited}."},
          open(out, "w"))
PY
}

# A stub gh. $STUB_OUT is what `gh issue list` prints; $STUB_RC its exit.
#
# `${STUB_OUT-[]}` and NOT `${STUB_OUT:-[]}`: the colon form substitutes
# the default for an EMPTY value as well as an unset one, which would
# make the "the API answered with nothing" case silently test "[]" — the
# precise distinction that case exists to check.
mk_gh() {
    cat > "$TMP/gh" <<'EOF'
#!/usr/bin/env bash
[ "${STUB_RC:-0}" -ne 0 ] && exit "${STUB_RC}"
printf '%s' "${STUB_OUT-[]}"
EOF
    chmod +x "$TMP/gh"
}
mk_gh

STUB_OUT='[]'; STUB_RC=0
export STUB_OUT STUB_RC

run() { # <mode> <readme> <badge> [gh]
    GFI_README="$2" GFI_BADGE="$3" GFI_GH="${4:-$TMP/gh}" \
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

# ---- --static --------------------------------------------------------
mk_readme "$URL" "$TMP/readme.ok"
mk_badge "$URL" '#1 (a), #2 (b)' "$TMP/badge.ok"
check "static: agreeing links pass" 0 "$(run --static "$TMP/readme.ok" "$TMP/badge.ok")" 'agree on label'

# The failure this static half exists for: a label rename updates one
# artifact and leaves the other pointing at a query matching nothing.
mk_readme "$OTHER" "$TMP/readme.renamed"
check "static: a renamed label in only one artifact fails" 1 \
    "$(run --static "$TMP/readme.renamed" "$TMP/badge.ok")" 'point at different filters'

printf 'Contributing, but no link at all.\n' > "$TMP/readme.nolink"
check "static: a README with no filter URL cannot see (2, not 0)" 2 \
    "$(run --static "$TMP/readme.nolink" "$TMP/badge.ok")" 'Cannot see'

mk_badge "" 'nothing' "$TMP/badge.nourl"
check "static: a justification with no filter URL cannot see" 2 \
    "$(run --static "$TMP/readme.ok" "$TMP/badge.nourl")" 'Cannot see'

check "static: a missing README cannot see" 2 \
    "$(run --static "$TMP/does-not-exist" "$TMP/badge.ok")" 'missing or unreadable'

printf '{ not json' > "$TMP/badge.broken"
check "static: unparseable badge JSON cannot see" 2 \
    "$(run --static "$TMP/readme.ok" "$TMP/badge.broken")"

# ---- --live ----------------------------------------------------------
mk_badge "$URL" '#534 (a), #535 (b), #536 (c)' "$TMP/badge.live"

STUB_OUT='[{"number":534},{"number":535},{"number":536}]'; STUB_RC=0
check "live: all cited issues open and labelled passes" 0 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'every cited issue'

# THE case this gate exists for: the last starter task got picked up.
STUB_OUT='[]'
check "live: an empty label fails, because the promise is now false" 1 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'No starter tasks left'

# Other tasks exist, but the justification names work that is done.
STUB_OUT='[{"number":534},{"number":999}]'
check "live: a cited issue that is no longer open/labelled fails" 1 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'cites #535'

# The two ways this gate could go quietly green while blind.
STUB_OUT='[{"number":534}]'; STUB_RC=1
check "live: an API error cannot see (2, not 0)" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'Cannot see'

# Absent data is not a zero: an empty response must not be read as "the
# label has no issues", which would fail the PR for the wrong reason and
# send someone hunting for starter tasks that are in fact still there.
STUB_OUT=''; STUB_RC=0
check "live: an EMPTY response is 'cannot see', never 'zero issues'" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live")" 'empty response'

STUB_OUT='[]'; STUB_RC=0
check "live: a missing gh cannot see" 2 \
    "$(run --live "$TMP/readme.ok" "$TMP/badge.live" "$TMP/no-such-gh")" 'not available'

# ---- usage -----------------------------------------------------------
GFI_README="$TMP/readme.ok" GFI_BADGE="$TMP/badge.ok" bash "$GATE" > "$TMP/out" 2>&1
check "no mode is a usage error, not a pass" 2 "$?" 'usage:'
GFI_README="$TMP/readme.ok" GFI_BADGE="$TMP/badge.ok" bash "$GATE" --wat > "$TMP/out" 2>&1
check "an unknown mode is a usage error" 2 "$?" 'usage:'

# ---- the committed tree ---------------------------------------------
# The seams above prove the logic; this proves it is pointed at real
# files that actually parse. A gate wired to a path that happens not to
# exist is the way this class of check goes quietly green.
if bash "$GATE" --static > "$TMP/out" 2>&1; then
    ok "the committed README and .bestpractices.json agree"
else
    no "the committed tree fails --static"
    sed 's/^/      /' "$TMP/out" >&2
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
