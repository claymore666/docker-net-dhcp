#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert the two vulnerability allowlists still agree (#741).
#
# THE PRUNING HAPPENED IN ONE FILE. This repository accepts advisories
# in two places under two naming schemes: `allow-ghsas` in
# .github/dependency-review-config.yml, and .github/vuln-allowlist.txt
# for the govulncheck gate. Both headers state that the lists stay in
# sync at or above fail-on-severity. Neither enforced it.
#
# GO-2026-5746 was dropped from vuln-allowlist.txt on 2026-07-28,
# because govulncheck stopped reporting it as reachable from our call
# graph — that file's own documented trigger for removal. GO-2026-5617
# went on 2026-08-14 in #525, under the rule that "an entry that accepts
# nothing is an entry nobody will re-review". Their GHSA twins stayed in
# allow-ghsas, where dependency-review runs at fail-on-severity: high.
# So for weeks a PR reintroducing `PUT /containers/{id}/archive executes
# container binary on the host` would have passed the gate, at high
# severity, in a project that had reviewed that exact advisory and
# concluded it was no longer acceptable.
#
# Both files warn about the "allowlist nobody prunes". This is that
# failure reached from the other side: the pruning was done, carefully,
# with a written reason — in one of the two files. Discipline applied to
# half a pair is not discipline, it is drift with a good changelog.
#
# WHAT IT CHECKS. Every GHSA in `allow-ghsas` must have a declared Go id
# in .github/ghsa-go-map.txt, and that Go id must be LIVE in
# vuln-allowlist.txt — present as an entry, not merely named in the
# prose explaining why it was removed. That last distinction is the
# whole job: vuln-allowlist.txt documents its removals in comments, so
# a plain grep for the id finds it in both cases and reports the drifted
# state as clean.
#
# WHICH DIRECTION IT FAILS IN, named because a guard has one. It catches
# dependency-review being LOOSER than govulncheck. It cannot catch the
# reverse — a high-severity advisory live in vuln-allowlist.txt but
# absent from allow-ghsas — because vuln-allowlist.txt records no
# severity, and inferring it would mean either a network call at gate
# time or a third list to keep in sync. That direction fails safe: the
# dependency-review gate would reject a PR the govulncheck gate accepts,
# which is a red check someone must look at, not a silent pass.
#
# Usage: check-allowlist-parity.sh [config] [allowlist] [map]
# Exit:  0 in sync, 1 drifted, 2 cannot check.
set -uo pipefail

CONFIG="${1:-.github/dependency-review-config.yml}"
ALLOWLIST="${2:-.github/vuln-allowlist.txt}"
MAP="${3:-.github/ghsa-go-map.txt}"

for f in "$CONFIG" "$ALLOWLIST" "$MAP"; do
    if [ ! -r "$f" ]; then
        echo "::error title=Nothing to inspect::$f is not readable." \
             "This gate would otherwise report the two allowlists in sync" \
             "having compared nothing." >&2
        exit 2
    fi
done

# --- the three inputs, comments stripped -------------------------------
#
# vuln-allowlist.txt documents every removal in a comment that NAMES the
# id it removed, so stripping comments is not tidiness here — it is the
# difference between "GO-2026-5746 is accepted" and "GO-2026-5746 was
# accepted and then was not". A grep over the raw file cannot tell them
# apart, and would have called the drifted tree clean.
mapfile -t GHSAS < <(
    grep -vE '^[[:space:]]*#' "$CONFIG" \
        | grep -oE '^[[:space:]]*-[[:space:]]*GHSA-[[:alnum:]]+-[[:alnum:]]+-[[:alnum:]]+' \
        | grep -oE 'GHSA-.*' \
        | sort -u
)

mapfile -t LIVE_GO < <(
    grep -vE '^[[:space:]]*#' "$ALLOWLIST" \
        | grep -oE '^[[:space:]]*GO-[0-9]{4}-[0-9]+' \
        | grep -oE 'GO-.*' \
        | sort -u
)

if [ "${#LIVE_GO[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::$ALLOWLIST parses to zero live entries." \
         "Either every acceptance was removed — in which case allow-ghsas must be" \
         "empty too and this gate should be told so — or the parse is wrong and" \
         "every GHSA below would be reported as drift." >&2
    exit 2
fi

declare -A GO_OF=()
map_entries=0
while read -r ghsa go rest; do
    [ -n "$ghsa" ] || continue
    case "$ghsa" in \#*) continue ;; esac
    if [ -n "$rest" ] || [[ ! "$ghsa" =~ ^GHSA- ]] || [[ ! "$go" =~ ^GO-[0-9]{4}-[0-9]+$ ]]; then
        echo "::error title=Nothing to inspect::$MAP has a line this gate cannot" \
             "parse: '$ghsa $go $rest'. The format is one 'GHSA-id GO-id' pair per" \
             "line; a line it skips is a mapping it silently does not have." >&2
        exit 2
    fi
    GO_OF["$ghsa"]="$go"
    map_entries=$((map_entries + 1))
done < <(grep -vE '^[[:space:]]*(#|$)' "$MAP")

if [ "$map_entries" -eq 0 ]; then
    echo "::error title=Nothing to inspect::$MAP declares no pairs, so no GHSA can" \
         "be resolved and this gate would compare nothing." >&2
    exit 2
fi

fail=0

# --- the comparison ----------------------------------------------------
for ghsa in "${GHSAS[@]}"; do
    go="${GO_OF[$ghsa]:-}"
    if [ -z "$go" ]; then
        echo "FAIL  $ghsa is in allow-ghsas with no declared Go id in $MAP." >&2
        echo "      Add the pair in the same commit that adds the GHSA. Without it" >&2
        echo "      nothing can compare this acceptance against the govulncheck side," >&2
        echo "      which is how the two lists drifted apart in the first place." >&2
        fail=1
        continue
    fi
    if ! printf '%s\n' "${LIVE_GO[@]}" | grep -Fx "$go" >/dev/null; then
        echo "FAIL  $ghsa (= $go) is accepted by dependency-review but $go is NOT a" >&2
        echo "      live entry in $ALLOWLIST." >&2
        echo "      dependency-review runs at fail-on-severity: high, so a PR" >&2
        echo "      reintroducing this advisory is waved through — after the" >&2
        echo "      govulncheck side deliberately stopped accepting it." >&2
        echo "      Remove it from allow-ghsas, or say why it is still accepted there" >&2
        echo "      and restore the entry on the other side." >&2
        fail=1
    fi
done

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "::error title=Allowlist drift::the advisory allowlists disagree." \
         "Both files state the sync rule in their headers; this is what enforces it." >&2
    exit 1
fi

if [ "${#GHSAS[@]}" -eq 0 ]; then
    # A real and legitimate answer — but stated, not folded into a PASS,
    # because it is also exactly what a broken parse of the config looks
    # like. The count of live govulncheck entries is printed beside it so
    # a zero that should not be zero is visible on the same line.
    echo "PASS  allow-ghsas is empty; ${#LIVE_GO[@]} live entry/entries in $ALLOWLIST," \
         "$map_entries declared pair(s)"
    exit 0
fi

echo "PASS  ${#GHSAS[@]} GHSA(s) in allow-ghsas, each mapped and live in $ALLOWLIST" \
     "(${#LIVE_GO[@]} live, $map_entries declared pair(s))"
