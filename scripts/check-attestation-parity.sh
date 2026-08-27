#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Pin the GHCR-only provenance asymmetry, and prove the pin is being
# measured rather than assumed (#776).
#
# WHAT IS ACTUALLY WRONG. `actions/attest-build-provenance` names the GHCR
# image only, in both the amd64 and arm64 paths, while cosign signs both
# registries. Measured on the shipped v1.7.1:
#
#   hub  v1.7.1  sha256:ef137d76...   attestations endpoint -> 404
#   ghcr v1.7.1  sha256:c1301ec9...   attestations endpoint -> 1
#
# Two different digests, so two artifacts. The consequence is not "Hub
# users cannot DISCOVER the attestation through registry referrers" -- it
# is that no provenance attestation for the Hub bytes exists ANYWHERE,
# neither as a referrer nor in GitHub's own store, for every user who
# installs from Docker Hub on every release shipped so far. cosign signs
# the Hub digest, so authenticity holds; provenance does not.
#
# The asymmetry dates to 823608b (#173/#174), is out in the wild on v1.7.1
# and earlier, and its FIX is v1.9.0 work. What this script is, is the
# OBSERVER: it records the state as it actually is, so the fix has
# something to flip and so the state cannot quietly change in either
# direction.
#
# WHY THE GHCR ROW IS NOT DECORATION. "Docker Hub has zero attestations"
# and "I could not ask" are the same observation from outside: a bad
# token, a rate limit, a typo in the path and a genuine absence all
# produce a 404. A check asserting only the Hub row passes hardest exactly
# when it is most broken. The GHCR row -- same endpoint, same token, same
# query shape, one positive answer -- is what makes the Hub 404
# admissible as evidence. It is also a real regression guard: if GHCR ever
# stops being attested, that is a supply-chain regression and this fails.
#
# WHEN v1.9.0 FIXES IT, this does not get deleted. The expected Hub
# verdict flips from "absent" to ">= 1" and the same script becomes the
# proof the fix worked and keeps working. A pin that converts into a
# regression guard is not a workaround.
#
# THREE VERDICTS, because two would collapse "cannot judge" into
# whichever neighbour is nearer -- and here the nearer one is PASS.
#
# Inputs (environment):
#   REPO          owner/name whose attestation store is queried
#   GHCR_DIGEST   sha256:<64 hex> -- the control side
#   HUB_DIGEST    sha256:<64 hex> -- the pinned side
#   ATTEST_QUERY  optional command run as `$ATTEST_QUERY <digest>`; must
#                 print exactly one of `count:<n>`, `notfound`, or
#                 `error:<text>`. Exists so the self-test can drive every
#                 branch, including the ones a live release cannot reach.
#
# Exit: 0 the asymmetry is exactly as documented (GHCR attested, Hub not)
#       1 the state changed -- either GHCR lost provenance, or Hub gained
#         it and this pin is now stale and must be flipped
#       2 CANNOT JUDGE -- a digest is unusable or a side went dark; the
#         message names which side, because that is the whole difference
#         between this and a silent pass

set -uo pipefail

REPO="${REPO:-}"
GHCR_DIGEST="${GHCR_DIGEST:-}"
HUB_DIGEST="${HUB_DIGEST:-}"

# How many times to re-ask the CONTROL side before calling it dark.
# GitHub's attestation store is written by the attest step and read back
# here seconds later; it is eventually consistent, so a first-attempt
# 404 on the control can mean "not indexed yet" rather than "absent".
#
# This is a bounded, announced re-ask of an asynchronous API, not a retry
# that makes a failing assertion pass: it can only turn CANNOT JUDGE into
# a real verdict, and that verdict is still free to be a failure. The Hub
# side is never re-asked -- its expected answer IS 404, so re-asking it
# could only launder a stale read into the answer we wanted.
CONTROL_ATTEMPTS="${CONTROL_ATTEMPTS:-5}"
CONTROL_SLEEP="${CONTROL_SLEEP:-10}"

refuse() {
    echo "::error title=Attestation parity cannot be judged::$*" >&2
    exit 2
}

is_digest() { [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]]; }

[ -n "$REPO" ] || refuse "REPO is empty; there is no attestation store to query."
is_digest "$GHCR_DIGEST" || refuse "GHCR_DIGEST is not a sha256 digest (got: '${GHCR_DIGEST:0:80}'). Without the control side, a Docker Hub 404 proves nothing."
is_digest "$HUB_DIGEST"  || refuse "HUB_DIGEST is not a sha256 digest (got: '${HUB_DIGEST:0:80}'). Nothing was measured on the pinned side."

# ask <digest> -> prints `count:<n>` | `notfound` | `error:<text>`
#
# `gh api --jq` prints a 4xx error BODY on stdout, so the count is guarded
# on SHAPE and never on emptiness -- an unguarded read here would take the
# JSON error object as an answer.
ask() {
    local digest="$1" out err rc detail
    if [ -n "${ATTEST_QUERY:-}" ]; then
        $ATTEST_QUERY "$digest"
        return 0
    fi
    err="$(mktemp)"
    out="$(gh api "repos/$REPO/attestations/$digest" --jq '.attestations | length' 2>"$err")"
    rc=$?
    if [ "$rc" -eq 0 ] && [[ "$out" =~ ^[0-9]+$ ]]; then
        printf 'count:%s' "$out"
    elif grep -q 'HTTP 404' "$err"; then
        printf 'notfound'
    else
        # STDERR IS EMPTY IN EXACTLY THE CASE THIS GUARD EXISTS FOR.
        # The comment above says `gh api --jq` prints a 4xx error BODY on
        # stdout; when it does, the SHAPE test rejects it -- correctly --
        # and stderr holds nothing. A message built from stderr alone is
        # then blank, and reads "could not be reached ... : ", aiming the
        # next reader at a token or a rate limit for a run where the
        # endpoint answered fine. That is the same mis-aim the `notfound`
        # branch below is written to prevent, one branch over.
        #
        # So report whichever stream actually spoke, and the exit status
        # when neither did.
        detail="$(tr '\n' ' ' < "$err" | cut -c1-200)"
        if [ -z "${detail// /}" ]; then
            detail="$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
        fi
        if [ -z "${detail// /}" ]; then
            detail="gh exited $rc and printed nothing on either stream"
        fi
        printf 'error:%s' "$detail"
    fi
    rm -f "$err"
}

# Re-ask the control only, and say so on every attempt.
ghcr_answer=""
for attempt in $(seq 1 "$CONTROL_ATTEMPTS"); do
    ghcr_answer="$(ask "$GHCR_DIGEST")"
    case "$ghcr_answer" in
        count:*) break ;;
    esac
    if [ "$attempt" -lt "$CONTROL_ATTEMPTS" ]; then
        echo "control side answered '$ghcr_answer' on attempt $attempt/$CONTROL_ATTEMPTS; re-asking in ${CONTROL_SLEEP}s" >&2
        sleep "$CONTROL_SLEEP"
    fi
done

case "$ghcr_answer" in
    count:*) ghcr_count="${ghcr_answer#count:}" ;;
    notfound)
        # TWO CAUSES, AND THE FIRST ONE IS THE LIKELY ONE. A digest with
        # no attestations answers 404, not `count:0` -- measured on the
        # shipped v1.7.1. So if GHCR ever LOSES provenance, this is the
        # branch that fires, not the `count:0` branch below whose message
        # names the attest step. A refusal that said only "the control
        # side went dark" would send whoever reads it at 02:00 to the
        # token, the rate limit and the path -- everything except the
        # step that actually broke.
        refuse "the GHCR attestations endpoint returned 404 for $GHCR_DIGEST after $CONTROL_ATTEMPTS attempt(s), and this run cannot tell which of two causes it is. FIRST, and most likely: the GHCR image LOST its provenance -- an image with no attestations answers 404, so check that the 'Attest image provenance (GHCR)' step still runs and still names this digest. SECOND: the attestations endpoint is unreachable for this run -- a token, a permission or a rate limit. Either way there is no positive answer from the control, so a Docker Hub 404 is indistinguishable from a broken query and this refuses rather than reporting the asymmetry it expected to find." ;;
    error:*)
        refuse "the GHCR attestations endpoint could not be reached for $GHCR_DIGEST: ${ghcr_answer#error:}. The control side went dark." ;;
    *)
        refuse "the attestation query returned '${ghcr_answer:0:80}' for the GHCR digest, which is none of count:<n>, notfound or error:<text>." ;;
esac

[[ "$ghcr_count" =~ ^[0-9]+$ ]] || refuse "the GHCR attestation count is not a number (got '${ghcr_count:0:40}')."

# KEPT DELIBERATELY, THOUGH PROBABLY UNREACHABLE. A digest with no
# attestations was measured to answer 404, which lands in the `notfound`
# branch above, not here. But "probably unreachable" is not "unreachable":
# producing a resolvable-but-unattested digest to prove it would need an
# unattested image in the package, which nobody has been able to
# construct. So this branch is neither deleted nor trusted -- it costs
# nothing and it is right if the API ever answers this way.
if [ "$ghcr_count" -eq 0 ]; then
    echo "::error title=GHCR provenance regressed::The attestations endpoint resolved for $GHCR_DIGEST and reported ZERO attestations." \
         "This is not the documented asymmetry -- it is the GHCR image, the one that IS attested, losing its provenance." \
         "Check that the 'Attest image provenance (GHCR)' step still runs and still names this digest." >&2
    exit 1
fi

echo "control OK: GHCR $GHCR_DIGEST has $ghcr_count attestation(s), so the endpoint answers positively and a Docker Hub 404 is admissible evidence."

hub_answer="$(ask "$HUB_DIGEST")"
case "$hub_answer" in
    error:*)
        refuse "the attestations endpoint could not be reached for the Docker Hub digest $HUB_DIGEST: ${hub_answer#error:}. The pinned side went dark; the control answered, so this is not a token or path problem." ;;
    notfound) hub_count=0 ;;
    count:*)  hub_count="${hub_answer#count:}" ;;
    *)
        refuse "the attestation query returned '${hub_answer:0:80}' for the Docker Hub digest, which is none of count:<n>, notfound or error:<text>." ;;
esac

[[ "$hub_count" =~ ^[0-9]+$ ]] || refuse "the Docker Hub attestation count is not a number (got '${hub_count:0:40}')."

if [ "$hub_count" -gt 0 ]; then
    echo "::error title=The attestation pin is stale::Docker Hub digest $HUB_DIGEST now has $hub_count attestation(s)." \
         "The GHCR-only asymmetry this script pins is GONE, which is the outcome the v1.9.0 fix is supposed to produce." \
         "Do not delete this check: change the expected Hub verdict from 'absent' to '>= 1' so it becomes the guard that the fix keeps working." >&2
    exit 1
fi

echo "::warning title=Docker Hub images carry no provenance attestation::GHCR $GHCR_DIGEST has $ghcr_count attestation(s); Docker Hub $HUB_DIGEST has none." \
     "This is the documented state, not a new failure: every user who installs from Docker Hub gets a cosign signature and no provenance. The fix is v1.9.0 work; this run only records that nothing changed."
echo "attestation parity: pinned as documented (GHCR attested, Docker Hub not)."
exit 0
