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
# Every branch of the CALLER is reachable offline through ATTEST_QUERY,
# and a live release can only ever exercise one of them. But ATTEST_QUERY
# returns from `ask()` before the `gh` call, so the cases above drive the
# caller and nothing below that `return`. The whole discrimination the
# gate's header describes -- rc, digit-shape, `HTTP 404` on stderr -- is
# on the far side of the seam, and it was never executed by anything
# until the `gh`-stub section at the bottom of this file.
#
# THE STUB IS WITNESSED, AND THAT IS NOT CEREMONY. With no stub applied,
# the real `gh` answers a genuine 404 for a repo that does not exist, so
# every "cannot judge" case here goes green having made a live network
# call and having tested nothing. The call counter is what tells a real
# run from that one.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-attestation-parity.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

GHCR="sha256:$(printf 'c%.0s' $(seq 1 64))"
HUB="sha256:$(printf 'e%.0s' $(seq 1 64))"

failures=0
n=0

# fake_query NAME GHCR_ANSWER HUB_ANSWER -> path to a query command
#
# Every value returned here is a single path with no space or glob
# character, so the deliberately unquoted `$ATTEST_QUERY "$digest"` in the
# checker expands to one word in every case below. The unquoted form is a
# documented affordance -- the header says "command", so `ATTEST_QUERY="bash
# q.sh"` is meant to work -- that nothing in this suite exercises. Left
# unquoted on purpose; recorded here so the next reader does not "fix" it
# and quietly narrow the contract.
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

# ======================================================================
# THE SHIPPED PATH: no ATTEST_QUERY, a stubbed `gh` on PATH
# ======================================================================
#
# PREPENDED, never assigned. The checker shells out to mktemp, grep, tr,
# cut and rm, and this suite invokes it with `bash`; replacing PATH with
# the stub directory exits 127 before the gate runs a line. Measured.
STUB="$TMP/bin"; mkdir -p "$STUB"
export GH_CALLS="$TMP/gh-calls" GH_MODES="$TMP/gh-modes"; mkdir -p "$GH_MODES"

cat > "$STUB/gh" <<'STUBEOF'
#!/usr/bin/env bash
# Log FIRST and unconditionally: the log is the witness that this stub,
# and not a real gh, produced the verdict being asserted.
printf '%s\n' "$*" >> "$GH_CALLS"
d=""; for a in "$@"; do case "$a" in */attestations/sha256:*) d="${a##*/}" ;; esac; done
case "$(cat "$GH_MODES/$d" 2>/dev/null || echo unset)" in
    count:*)  printf '%s\n' "$(cat "$GH_MODES/$d")" | sed 's/^count://'; exit 0 ;;
    # A real 404: the body lands on STDOUT, the diagnostic on STDERR.
    http404)  printf '{"message":"Not Found","status":"404"}\n'
              echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
    # rc 0, and a JSON error object on STDOUT with STDERR EMPTY. This is
    # the case the checker's own header singles out.
    body200)  printf '{"message":"Bad credentials"}\n'; exit 0 ;;
    netfail)  echo "dial tcp: lookup api.github.com: no such host" >&2; exit 1 ;;
    silent)   exit 9 ;;
    *)        echo "stub: no mode set for '$d'" >&2; exit 3 ;;
esac
STUBEOF
chmod +x "$STUB/gh"

# gh_check NAME WANT_EXIT WANT_CALLS GHCR_MODE HUB_MODE
gh_check() {
    local name="$1" want="$2" wantcalls="$3"
    printf '%s' "$4" > "$GH_MODES/$GHCR"; printf '%s' "$5" > "$GH_MODES/$HUB"
    : > "$GH_CALLS"
    n=$((n + 1))
    PATH="$STUB:$PATH" REPO="owner/name" GHCR_DIGEST="$GHCR" HUB_DIGEST="$HUB" \
        CONTROL_ATTEMPTS=2 CONTROL_SLEEP=0 bash "$CHECK" > "$TMP/out" 2>&1
    local got=$? calls
    calls=$(grep -c . "$GH_CALLS" 2>/dev/null || echo 0)
    if [ "$got" -eq "$want" ] && [ "$calls" -eq "$wantcalls" ]; then
        echo "PASS: [gh] $name (exit $got, $calls gh call(s))"
    else
        echo "FAIL: [gh] $name -- want exit $want / $wantcalls call(s), got $got / $calls"
        [ "$calls" -eq 0 ] && echo "    the stub was never invoked -- PATH did not take, so this case tested nothing"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# WHAT THESE CONTROLS DO NOT PROVE. Every case here drives the control
# side with exactly one attestation, so they show the gate sees one
# attestation in a store containing one -- never that it carries N
# through as N. A checker that hard-coded the count to 1 would pass all
# of them.
gh_check "the documented state, through gh"        0 2 count:1 http404
want_in "control OK"
want_in "no provenance attestation"

gh_check "the pin goes stale, through gh"          1 2 count:1 count:2
want_in "pin is stale"

# One call, not two: the control resolved on the first ask, and the run
# exits before the pinned side is reached. A `2` here would mean the Hub
# side was asked despite the control having already failed.
gh_check "GHCR resolves to zero, through gh"       1 1 count:0 http404
want_in "GHCR provenance regressed"

gh_check "a real 404 on both sides cannot judge"   2 2 http404 http404
want_in "LOST its provenance"

gh_check "a transport failure on the control"      2 2 netfail http404
want_in "control side went dark"
want_in "no such host"

# THE CASE THE CHECKER'S HEADER IS ABOUT, and the one that had never run.
# `gh` exits 0 and prints a JSON error object on stdout. The shape test
# must refuse to read it as a count -- and the refusal must SAY what was
# seen. Before this test the message was built from stderr alone, which
# is empty here, so it read "could not be reached ... : ." and pointed
# the reader at a token for a run where the endpoint answered.
gh_check "a 4xx body on stdout is not a count"     2 2 body200 http404
want_in "Bad credentials"

# Neither stream speaks. The diagnostic must still name something.
gh_check "gh fails silently: rc is reported"       2 2 silent http404
want_in "printed nothing on either stream"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
