#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-attestation-parity.sh (#776).
#
# THE CASE THAT CARRIES THE WEIGHT is `everything-404`: a query that
# answers `notfound` for BOTH digests. The Hub half of that is exactly
# what a correct run sees, so a check that read only the Hub row would
# report "the asymmetry is as documented" from an environment where
# nothing could be reached at all -- passing hardest when most broken.
# It must exit 2, and it must name the control side.
#
# THE GUARD HAS A DIRECTION, so both are driven: GHCR losing provenance
# (regression) and Docker Hub gaining it (the pin going stale because
# v1.9.0 fixed it). The second is the one people forget, because it is
# the good news.
#
# TWO SEAMS, DELIBERATELY. ATTEST_QUERY drives the CALLER's branches --
# every verdict the checker can reach, offline, including the ones a live
# release can never exercise. But it returns from `ask` one line ABOVE the
# `gh` call, so nothing here had ever executed the block that runs `gh`
# and discriminates its three answers, which is where the gate's shape
# guard lives (#827). The cases at the end of this file drive that block
# by stubbing `gh` on PATH, each with a per-digest call witness.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

CHECK="$(cd "$(dirname "$0")" && pwd)/check-attestation-parity.sh"
guarded_tmpdir TMP

GHCR="sha256:$(printf 'c%.0s' $(seq 1 64))"
HUB="sha256:$(printf 'e%.0s' $(seq 1 64))"

failures=0
n=0

# fake_query NAME GHCR_ANSWER HUB_ANSWER -> path to a query command
fake_query() {
    local name="$1" g="$2" h="$3"
    local f="$TMP/q-$name.sh"
    cat > "$f" <<EOF
#!/usr/bin/env bash
case "\$1" in
    "$GHCR") printf '%s' '$g' ;;
    "$HUB")  printf '%s' '$h' ;;
    *)       printf 'error:the self-test query was asked about an unexpected digest \$1' ;;
esac
EOF
    chmod +x "$f"
    printf '%s' "$f"
}

# check NAME WANT_EXIT QUERY [GHCR_DIGEST] [HUB_DIGEST]
check() {
    # `${5-...}` and not `${5:-...}`: an EMPTY digest is a case this
    # suite deliberately passes, and `:-` would substitute the good value
    # for it and quietly test something else.
    local name="$1" want="$2" q="$3"
    local gd="${4-$GHCR}" hd="${5-$HUB}"
    n=$((n + 1))
    REPO="owner/name" GHCR_DIGEST="$gd" HUB_DIGEST="$hd" \
        ATTEST_QUERY="$q" CONTROL_ATTEMPTS=2 CONTROL_SLEEP=0 \
        bash "$CHECK" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want" ]; then
        echo "PASS: $name (exit $got)"
    else
        echo "FAIL: $name -- want $want, got $got"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

want_in() {
    if ! grep -F -- "$1" "$TMP/out" > /dev/null; then
        echo "FAIL: previous case's output does not contain '$1'"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# --- the documented state ---------------------------------------------
check "GHCR attested, Hub 404: the pin holds" 0 "$(fake_query ok 'count:1' 'notfound')"
want_in "control OK"
want_in "no provenance attestation"

check "GHCR attested, Hub resolves to zero: the pin holds" 0 \
      "$(fake_query zero 'count:3' 'count:0')"

# --- direction A: the control side loses provenance -------------------
# Resolved and zero, not 404: the endpoint answered, so this is a real
# absence and a real supply-chain regression, not an unreachable API.
check "GHCR resolved to zero attestations" 1 "$(fake_query regress 'count:0' 'notfound')"
want_in "GHCR provenance regressed"

# --- direction B: the pin goes stale because the fix landed -----------
# The good-news direction. It must still fail, and the message must say
# FLIP rather than DELETE, or the next person removes the guard that
# proves their own fix works.
check "Docker Hub gained an attestation" 1 "$(fake_query fixed 'count:1' 'count:1')"
want_in "pin is stale"
want_in "'>= 1'"

# --- the load-bearing case: everything is dark ------------------------
# A query that 404s for every digest. The Hub row alone reads exactly
# like a correct run. Only the control can tell them apart.
check "a query that 404s for everything cannot judge" 2 \
      "$(fake_query dark404 'notfound' 'notfound')"
# The refusal must name the LIKELY cause first. A digest with no
# attestations answers 404, so this branch -- not the count:0 one -- is
# what fires if GHCR loses provenance, and a message that named only the
# transport would aim the next hour at the token and the rate limit.
want_in "LOST its provenance"
want_in "Attest image provenance (GHCR)"
want_in "a token, a permission or a rate limit"

check "the control endpoint errors" 2 \
      "$(fake_query gerr 'error:dial tcp: lookup api.github.com' 'notfound')"
want_in "control side went dark"

# The control answers, the pinned side does not. Distinct message,
# because "the control answered" rules out a token or path problem and
# that is what the next reader needs to know.
check "the pinned endpoint errors while the control answers" 2 \
      "$(fake_query herr 'count:1' 'error:503 Service Unavailable')"
want_in "pinned side went dark"
want_in "not a token or path problem"

# --- a query that answers in a shape nobody agreed to ------------------
check "an unrecognised answer shape refuses" 2 \
      "$(fake_query junk 'yes' 'notfound')"
want_in "none of count:<n>, notfound or error:<text>"

# --- unusable inputs refuse rather than measuring nothing --------------
Q="$(fake_query ok2 'count:1' 'notfound')"
check "a non-digest GHCR input refuses" 2 "$Q" "latest" "$HUB"
want_in "Without the control side"
check "a non-digest Hub input refuses" 2 "$Q" "$GHCR" ""
want_in "Nothing was measured on the pinned side"
check "a truncated digest refuses" 2 "$Q" "sha256:abc123" "$HUB"

REPO="" GHCR_DIGEST="$GHCR" HUB_DIGEST="$HUB" ATTEST_QUERY="$Q" \
    bash "$CHECK" > "$TMP/out" 2>&1
rc=$?
n=$((n + 1))
if [ "$rc" -eq 2 ]; then echo "PASS: an empty REPO refuses (exit 2)"; else
    echo "FAIL: an empty REPO -- want 2, got $rc"; failures=$((failures + 1)); fi

# --- the control is re-asked, and ONLY the control --------------------
# A query that 404s once and then answers must end in a real verdict --
# that is the eventual-consistency case the re-ask exists for. A query
# that answers `count:1` for Hub on its second call must NOT be re-asked
# into a pass, so the counter below proves the Hub side is asked once.
cat > "$TMP/q-count.sh" <<EOF
#!/usr/bin/env bash
f="$TMP/calls-\$(printf '%s' "\$1" | tail -c 8)"
c=\$(cat "\$f" 2>/dev/null || echo 0); c=\$((c + 1)); echo "\$c" > "\$f"
case "\$1" in
    "$GHCR") if [ "\$c" -eq 1 ]; then printf 'notfound'; else printf 'count:1'; fi ;;
    *)       printf 'notfound' ;;
esac
EOF
chmod +x "$TMP/q-count.sh"
check "a control that indexes late still reaches a verdict" 0 "$TMP/q-count.sh"

# --- the pinned side is asked only AFTER the control has resolved -----
# The counters above say HOW MANY times each side is asked and nothing
# about WHEN. Swap the two blocks in the checker and all of the cases
# above still pass -- the property holds by construction and is observed
# by nothing, which is the same shape as a true-but-unguarded claim.
#
# It matters on the day someone wires provenance for the Hub push: the
# store is eventually consistent, and a Hub ask made before the control
# has proven the endpoint live can read 404 inside the indexing window
# and report "pinned as documented", swallowing the good news.
#
# So record the order and assert it.
cat > "$TMP/q-order.sh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$1" >> "$TMP/ask-order"
if [ "\$1" = "$GHCR" ]; then
    n=\$(grep -c . "$TMP/ask-order")
    if [ "\$n" -eq 1 ]; then printf 'notfound'; else printf 'count:1'; fi
else
    printf 'notfound'
fi
EOF
chmod +x "$TMP/q-order.sh"
rm -f "$TMP/ask-order"
check "ordering: the control resolves before the pinned side is asked" 0 "$TMP/q-order.sh"
n=$((n + 1))
first_hub=$(grep -n -F -x -- "$HUB" "$TMP/ask-order" | head -1 | cut -d: -f1)
first_ok=$(awk -v g="$GHCR" 'NR>1 && $0==g {print NR; exit}' "$TMP/ask-order")
if [ -n "$first_hub" ] && [ -n "$first_ok" ] && [ "$first_hub" -gt "$first_ok" ]; then
    echo "PASS: the pinned side was asked at call $first_hub, after the control resolved at call $first_ok"
else
    echo "FAIL: ask order -- pinned side at '${first_hub:-never}', control resolved at '${first_ok:-never}'; the pinned side must come later"
    sed 's/^/    /' "$TMP/ask-order" 2>/dev/null
    failures=$((failures + 1))
fi
n=$((n + 1))
gcalls=$(cat "$TMP/calls-$(printf '%s' "$GHCR" | tail -c 8)" 2>/dev/null || echo 0)
hcalls=$(cat "$TMP/calls-$(printf '%s' "$HUB" | tail -c 8)" 2>/dev/null || echo 0)
if [ "$gcalls" -eq 2 ] && [ "$hcalls" -eq 1 ]; then
    echo "PASS: the control was re-asked (2) and the pinned side was not (1)"
else
    echo "FAIL: re-ask asymmetry -- control asked $gcalls time(s), pinned side $hcalls time(s); want 2 and 1"
    failures=$((failures + 1))
fi

# --- the gh path: the block ATTEST_QUERY has always short-circuited ----
# A PREFIX on PATH, never a replacement: the checker also needs mktemp,
# grep, tr, cut and rm, and a replaced PATH exits 127 before reaching any
# of the logic these cases are here to grade.
#
# EVERY CASE CARRIES A PER-DIGEST WITNESS. Measured in #827: with the stub
# not applied, cases returned the CORRECT exit code having made ZERO `gh`
# calls, because the real binary answered them from the network. The call
# log is the only thing that tells "the stub answered" apart from "GitHub
# did", and one case below asserts a count of zero, which no live call
# could produce.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<'STUB'
#!/usr/bin/env bash
# The checker makes exactly one shape of call:
#   gh api repos/<repo>/attestations/<digest> --jq <expr>
digest="${2##*/}"
printf '%s\n' "$digest" >> "$GH_CALLS"
. "$GH_PLAN"
STUB
chmod +x "$TMP/bin/gh"

# gh_case NAME WANT_EXIT WANT_CONTROL_CALLS WANT_PINNED_CALLS  < plan
#
# The plan is read from stdin and sourced by the stub with $digest set;
# its exit status becomes gh's, and what it prints goes where gh's would.
gh_case() {
    local name="$1" want="$2" wg="$3" wh="$4" got g h
    cat > "$TMP/gh-plan"
    : > "$TMP/gh-calls"
    n=$((n + 1))
    # ATTEST_QUERY empty, not unset: an exported one in the environment
    # would silently put every case below back on the seam being retired.
    REPO="owner/name" GHCR_DIGEST="$GHCR" HUB_DIGEST="$HUB" \
        ATTEST_QUERY="" CONTROL_ATTEMPTS=2 CONTROL_SLEEP=0 \
        GH_CALLS="$TMP/gh-calls" GH_PLAN="$TMP/gh-plan" \
        PATH="$TMP/bin:$PATH" \
        bash "$CHECK" > "$TMP/out" 2>&1
    got=$?
    g=$(grep -c -F -x -- "$GHCR" "$TMP/gh-calls") || g=0
    h=$(grep -c -F -x -- "$HUB" "$TMP/gh-calls") || h=0
    if [ "$got" -eq "$want" ] && [ "$g" -eq "$wg" ] && [ "$h" -eq "$wh" ]; then
        echo "PASS: $name (exit $got; gh calls control $g, pinned $h)"
    else
        echo "FAIL: $name -- want exit $want with gh calls $wg/$wh, got $got with $g/$h"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

gh_case "gh: control attested, pinned side 404" 0 1 1 <<EOF
case "\$digest" in
    "$GHCR") printf '1\n' ;;
    *)       echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
esac
EOF
want_in "control OK"
want_in "no provenance attestation"

gh_case "gh: control resolved to zero attestations" 1 1 0 <<EOF
case "\$digest" in
    "$GHCR") printf '0\n' ;;
    *)       echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
esac
EOF
want_in "GHCR provenance regressed"

gh_case "gh: the pinned side gained an attestation" 1 1 1 <<'EOF'
printf '2\n'
EOF
want_in "pin is stale"

# The pinned side is never asked, so its witness is 0 -- a count the real
# `gh` cannot produce for a case that reaches a verdict at all.
gh_case "gh: a 404 for every digest cannot judge" 2 2 0 <<'EOF'
echo 'gh: Not Found (HTTP 404)' >&2
exit 1
EOF
want_in "LOST its provenance"

# --- the two diagnostic cases (#827) ----------------------------------
# rc is non-zero, the shape guard correctly rejects the body, and stderr
# is EMPTY -- so before the fix the refusal named the transport and then
# said nothing at all about what the endpoint actually replied.
gh_case "gh: a 4xx body on stdout with an empty stderr is quoted in the refusal" 2 2 0 <<'EOF'
printf '{"message":"Bad credentials","status":"401"}\n'
exit 1
EOF
want_in "Bad credentials"
want_in "control side went dark"

# The same hole one exit code over: rc ZERO with a body the shape guard
# rejects. Driven on the pinned side, whose refusal is a different string.
gh_case "gh: a non-numeric body with rc 0 is quoted in the pinned-side refusal" 2 1 1 <<EOF
case "\$digest" in
    "$GHCR") printf '1\n' ;;
    *)       printf '{"message":"Moved Permanently"}\n' ;;
esac
EOF
want_in "Moved Permanently"
want_in "pinned side went dark"

# PRESERVATION CONTROL for the two above: when stderr does carry the
# diagnosis, that is still what the refusal quotes.
gh_case "gh: a transport error still quotes stderr" 2 2 0 <<'EOF'
echo 'dial tcp: lookup api.github.com: no such host' >&2
exit 1
EOF
want_in "no such host"
want_in "control side went dark"

# BOTH streams carry text. stderr is gh's own diagnosis and stdout is only
# the body it was reading, so stderr is what the refusal quotes. Every
# other case leaves exactly one stream non-empty, which leaves the
# fallback's ORDER unobserved: reversing it survived a mutation run.
gh_case "gh: with both streams filled the refusal quotes stderr" 2 2 0 <<'EOF'
printf '{"message":"Not Found"}\n'
echo 'gh: this API operation is rate limited (403)' >&2
exit 1
EOF
want_in "rate limited"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
