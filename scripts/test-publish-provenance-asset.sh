#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for publish-provenance-asset.sh (#1011), with the TRANSPORT
# STUBBED, plus the call sites in release.yml.
#
# WHAT THIS BUYS. The release workflow runs on a tag and nowhere else,
# so inline in a `run:` block none of the four refusals could ever
# execute: a real release does not produce a bundle that attests
# nothing, or one that names the wrong artifact, on demand. Here `gh` is
# a stub on PATH that records what it was called with, so every outcome
# the real step can reach is driven on every lane run:
#
#   - an asset name Scorecard's provenance check would not count
#   - no bundle to copy
#   - a bundle nothing can read
#   - a bundle whose statement carries no subject
#   - a bundle that does not name the artifact being released
#   - a bundle that does not verify
#   - the happy path, every subject verified
#
# THE STUB RECORDS ITS ARGUMENTS, and the cases assert on them, because
# "it exited 0" cannot tell a verification that ran from one that was
# skipped. The happy case asserts `--bundle <the published asset>`: with
# the flag dropped, `gh attestation verify` asks GitHub's attestation
# store instead, which passes whatever the published file contains.
#
# ORDER IS A CLAIM TOO. Every refusal case asserts `not_logged gh.args`:
# a bundle that fails one of them is never reported as verified.
#
# THE CALL SITES ARE PART OF THE SUBJECT. The script can be correct and
# reach no release: the last block reads release.yml and holds every job
# that attests FILES to publishing what it attested, both asset names to
# appearing in the uploaded set and in RELEASE_FILES, and the published
# set to carrying a name Scorecard counts.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

SUBJECT="$HERE/publish-provenance-asset.sh"
WF="$(cd "$HERE/.." && pwd)/.github/workflows/release.yml"
guarded_tmpdir TMP

failures=0
n=0

BIN="$TMP/bin"
mkdir -p "$BIN"
cat > "$BIN/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}/gh.args"
exit "${GH_RC:-0}"
STUB
chmod 755 "$BIN/gh"

ART="net-dhcp-plugin-v9.9.9-linux-amd64.tar.gz"
ASSET="provenance.intoto.jsonl"

# A Sigstore bundle of the shape actions/attest writes: one line, an
# in-toto statement inside a DSSE envelope. Subjects come from "$@".
bundle() { # FILE SUBJECT...
    local out="$1"; shift
    local subs="" sep=""
    local s
    for s in "$@"; do
        subs="${subs}${sep}{\"name\":\"${s}\",\"digest\":{\"sha256\":\"$(printf '%064d' 0)\"}}"
        sep=","
    done
    local stmt
    stmt="{\"_type\":\"https://in-toto.io/Statement/v1\",\"predicateType\":\"https://slsa.dev/provenance/v1\",\"subject\":[${subs}]}"
    printf '{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{},"dsseEnvelope":{"payloadType":"application/vnd.in-toto+json","payload":"%s"}}\n' \
        "$(printf '%s' "$stmt" | base64 | tr -d '\n')" > "$out"
}

# run NAME WANT_EXIT WANT_GREP -- env assignments... -- args...
run() {
    local name="$1" want_exit="$2" want_grep="$3"; shift 3
    [ "$1" = "--" ] && shift
    local -a envs=()
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs+=("$1"); shift; done
    [ "${1-}" = "--" ] && shift
    n=$((n + 1))
    rm -rf "$TMP/log" "$TMP/work"
    mkdir -p "$TMP/log" "$TMP/work"
    ( cd "$TMP/work" && PATH="$BIN:$PATH" STUB_LOG="$TMP/log" \
        env REPO=claymore666/docker-net-dhcp "${envs[@]}" bash "$SUBJECT" "$@" ) > "$TMP/out" 2>&1
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

assert() { # NAME 1-or-0
    n=$((n + 1))
    if [ "$2" = "1" ]; then
        echo "PASS: $1"
    else
        echo "FAIL: $1"
        failures=$((failures + 1))
    fi
}
logged()     { if [ -f "$TMP/log/$1" ] && grep -qF -- "$2" "$TMP/log/$1"; then echo 1; else echo 0; fi; }
not_logged() { if [ -f "$TMP/log/$1" ]; then echo 0; else echo 1; fi; }
lines()      { if [ -f "$TMP/log/$1" ]; then wc -l < "$TMP/log/$1" | tr -d ' '; else echo 0; fi; }

bundle "$TMP/good.json"     "$ART" sbom.spdx.json sbom.cdx.json
bundle "$TMP/wrongart.json" sbom.spdx.json sbom.cdx.json
bundle "$TMP/nosubj.json"
printf 'not a bundle at all\n' > "$TMP/garbage.json"

# The subject assertion is the one check here that the verification loop
# cannot make, so the two ways of writing it LOOSELY get their own
# fixtures. A subject that merely contains the artifact's name is what a
# substring match accepts; a subject differing only where the artifact's
# name has a regex metacharacter is what an unanchored-but-regex match
# accepts. Both bundles attest the artifact nowhere.
bundle "$TMP/superstring.json" "stale-$ART" sbom.spdx.json
bundle "$TMP/metachar.json"    "${ART//./X}" sbom.spdx.json

# ---------------------------------------------------------------- happy

run "the bundle is published and every subject is verified against it" 0 \
    "3 subjects verified" -- -- "$TMP/good.json" "$ASSET" "$ART"

assert "the published file is the attestation, byte for byte" \
    "$(if cmp -s "$TMP/work/$ASSET" "$TMP/good.json"; then echo 1; else echo 0; fi)"
assert "the released artifact is verified AGAINST THE PUBLISHED FILE" \
    "$(logged gh.args "attestation verify $ART --bundle $ASSET")"
assert "the SBOM subjects are verified too, not only the artifact" \
    "$(if [ "$(logged gh.args "attestation verify sbom.spdx.json --bundle $ASSET")" = 1 ] \
        && [ "$(logged gh.args "attestation verify sbom.cdx.json --bundle $ASSET")" = 1 ]; then echo 1; else echo 0; fi)"
assert "one verification per subject, none skipped" \
    "$(if [ "$(lines gh.args)" = 3 ]; then echo 1; else echo 0; fi)"
assert "the repository the certificate must name is passed through" \
    "$(logged gh.args "--repo claymore666/docker-net-dhcp")"

# ------------------------------------------------- the name Scorecard reads

run "an asset name Scorecard would not count is refused" 2 \
    "does not end in '.intoto.jsonl'" -- -- "$TMP/good.json" provenance.json "$ART"
assert "a name nothing counts is never published" \
    "$(if [ -e "$TMP/work/provenance.json" ]; then echo 0; else echo 1; fi)"
assert "a name nothing counts is never verified either" "$(not_logged gh.args)"

# Scorecard matches a SUFFIX. A name that carries those characters in the
# middle is counted by nothing, and a refusal written as "contains" would
# publish it.
run "an asset name that only contains the suffix is refused too" 2 \
    "does not end in '.intoto.jsonl'" \
    -- -- "$TMP/good.json" provenance.intoto.jsonl.bak "$ART"
assert "a name carrying the suffix in the middle is never published" \
    "$(if [ -e "$TMP/work/provenance.intoto.jsonl.bak" ]; then echo 0; else echo 1; fi)"

# ------------------------------------------------------------- the bundle

run "no bundle to copy is refused" 2 "no file at" \
    -- -- "$TMP/absent.json" "$ASSET" "$ART"
assert "a missing bundle is never verified" "$(not_logged gh.args)"

run "a bundle nothing can read is refused as unreadable" 2 \
    "is not a readable attestation bundle" -- -- "$TMP/garbage.json" "$ASSET" "$ART"
assert "an unreadable bundle is never verified" "$(not_logged gh.args)"

run "a bundle whose statement carries no subject attests nothing" 1 \
    "attests nothing" -- -- "$TMP/nosubj.json" "$ASSET" "$ART"
assert "a bundle attesting nothing is never verified" "$(not_logged gh.args)"

run "a bundle that does not name the released artifact is refused" 1 \
    "is not a subject of" -- -- "$TMP/wrongart.json" "$ASSET" "$ART"
assert "the refusal names what the bundle DOES attest" \
    "$(if grep -q 'sbom.spdx.json' "$TMP/out"; then echo 1; else echo 0; fi)"
assert "a bundle about the wrong artifact is never verified" "$(not_logged gh.args)"

run "a subject that merely CONTAINS the artifact's name is not the artifact" 1 \
    "is not a subject of" -- -- "$TMP/superstring.json" "$ASSET" "$ART"
assert "a bundle attesting a lookalike name is never verified" "$(not_logged gh.args)"

run "a subject matching the artifact's name only as a pattern is not the artifact" 1 \
    "is not a subject of" -- -- "$TMP/metachar.json" "$ASSET" "$ART"
assert "a bundle attesting a pattern match is never verified" "$(not_logged gh.args)"

# -------------------------------------------------------- the verification

run "a bundle that does not verify fails" 1 \
    "does not verify against the published" -- "GH_RC=1" -- "$TMP/good.json" "$ASSET" "$ART"
assert "a failing verification stops at the first subject" \
    "$(if [ "$(lines gh.args)" = 1 ]; then echo 1; else echo 0; fi)"

# -------------------------------------------------------------- arguments

run "a missing artifact argument is refused" 2 "no ARTIFACT given" \
    -- -- "$TMP/good.json" "$ASSET"
run "an extra argument is refused" 2 "unexpected extra argument" \
    -- -- "$TMP/good.json" "$ASSET" "$ART" extra
n=$((n + 1))
rm -rf "$TMP/log" "$TMP/work"; mkdir -p "$TMP/log" "$TMP/work"
( cd "$TMP/work" && PATH="$BIN:$PATH" STUB_LOG="$TMP/log" \
    env -u REPO -u GITHUB_REPOSITORY bash "$SUBJECT" "$TMP/good.json" "$ASSET" "$ART" ) > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "neither REPO nor GITHUB_REPOSITORY" "$TMP/out"; then
    echo "PASS: with no repository to check the certificate against, it refuses"
else
    echo "FAIL: with no repository to check the certificate against, it refuses"
    failures=$((failures + 1))
fi

# ------------------------------------------------------------ the call sites

[ -f "$WF" ] || { echo "FAIL: $WF is missing"; exit 1; }

# Every job that attests FILES (subject-path, as opposed to the image
# attestation's subject-digest) must publish what it attested. Keyed on
# the property, not on a list of job names: add a third architecture and
# this names it.
mapfile -t GAPS < <(awk '
    /^  [a-z][a-z0-9-]*:[[:space:]]*$/ { job = $1; sub(/:$/, "", job) }
    /uses: actions\/attest-build-provenance/ { in_attest = 1 }
    /^      - name:/ { in_attest = 0 }
    in_attest && /subject-path:/ { attests[job] = 1 }
    /bash scripts\/publish-provenance-asset\.sh/ { publishes[job] = 1 }
    END { for (j in attests) if (!(j in publishes)) print j }
' "$WF")
assert "every job that attests release files also publishes the provenance" \
    "$(if [ "${#GAPS[@]}" -eq 0 ]; then echo 1; else echo "0 (${GAPS[*]})"; fi)"

# Two facts per job, read out of the workflow's structure and not out of
# its indentation: which provenance asset a job WRITES (the name it hands
# the script) and which one it UPLOADS for the publishing job (the name in
# its own upload-artifact path block). A grep keyed on leading spaces is
# answered by any line that happens to be indented further, which is how
# this assertion first passed against the release-notes table instead.
SITES=$(awk '
    /^  [a-z][a-z0-9-]*:[[:space:]]*$/ { job = $1; sub(/:$/, "", job); inupload = 0; inpath = 0 }
    /^      - name:/ { inupload = 0; inpath = 0 }
    /uses: actions\/upload-artifact/ { inupload = 1 }
    inupload && /^          path: \|/ { inpath = 1; next }
    inpath && /^          [a-z][a-z0-9-]*:/ { inpath = 0 }
    inpath && /\.intoto\.jsonl/ { gsub(/^[ \t]+|[ \t]+$/, ""); print job "\tupload\t" $0; next }
    /publish-provenance-asset\.sh/ { incall = 1 }
    incall {
        for (i = 1; i <= NF; i++)
            if ($i ~ /\.intoto\.jsonl$/) { print job "\twrite\t" $i; incall = 0 }
    }
' "$WF")

mapfile -t ASSETS < <(printf '%s\n' "$SITES" | awk -F'\t' '$2 == "write" { print $3 }' | sort -u)
assert "both architectures publish a provenance asset" \
    "$(if [ "${#ASSETS[@]}" -eq 2 ]; then echo 1; else echo 0; fi)"

# The whole line, trimmed, not $1: a name in this block carries an
# expression with spaces in it (`${{ needs.release.outputs.tag }}`), and
# reading the first field alone silently truncated both tarballs to
# `net-dhcp-plugin-${{`.
RELEASE_FILES=$(awk '/^      RELEASE_FILES: >-/ { inlist = 1; next }
                     inlist && /^    [a-z]/ { inlist = 0 }
                     inlist { gsub(/^[ \t]+|[ \t]+$/, ""); if (length($0)) print }' "$WF")
missing_published=0
for a in "${ASSETS[@]}"; do
    printf '%s\n' "$RELEASE_FILES" | grep -Fx -- "$a" >/dev/null || missing_published=1
done
assert "every provenance asset the run produces reaches the release page" \
    "$(if [ "$missing_published" -eq 0 ]; then echo 1; else echo 0; fi)"

# Per JOB, not per file: an asset uploaded by the other architecture's job
# is not carried out of this one, and the publishing job downloads both.
missing_uploaded=""
while IFS=$'\t' read -r job kind name; do
    [ "$kind" = write ] || continue
    printf '%s\n' "$SITES" | grep -Fx -- "$(printf '%s\tupload\t%s' "$job" "$name")" >/dev/null \
        || missing_uploaded="${missing_uploaded} ${job}:${name}"
done <<< "$SITES"
assert "every job uploads the provenance asset it wrote" \
    "$(if [ -z "$missing_uploaded" ]; then echo 1; else echo "0 (${missing_uploaded})"; fi)"

# The reverse direction: a name on the release page that nothing writes
# would make `gh release create` fail at the very last step of a release.
orphans=0
while IFS= read -r f; do
    case "$f" in
        *.intoto.jsonl)
            printf '%s\n' "${ASSETS[@]}" | grep -Fx -- "$f" >/dev/null || orphans=1 ;;
    esac
done <<< "$RELEASE_FILES"
assert "no provenance name is published that nothing produces" \
    "$(if [ "$orphans" -eq 0 ]; then echo 1; else echo 0; fi)"

# The release body's Downloads table is generated from RELEASE_FILES with a
# `case` per name and a `*)` fallback that says "release artifact". A new
# asset with no case of its own is therefore published with no explanation,
# silently, on the page that is the project's landing page.
rowless=""
for a in "${ASSETS[@]}"; do
    # The case LABEL, not a line indented some particular way: the sibling
    # assertion below was keyed on leading spaces once already and was
    # answered by a different block of the file.
    awk -v a="$a" '{ sub(/^[ \t]+/, ""); if (index($0, a ")") == 1) found = 1 }
                   END { exit !found }' "$WF" || rowless="${rowless} ${a}"
done
assert "every provenance asset has its own row in the generated Downloads table" \
    "$(if [ -z "$rowless" ]; then echo 1; else echo "0 (${rowless})"; fi)"

# The release body also tells a reader what the signatures cover. That
# sentence is prose, and it went wrong the moment an attached file was
# covered by no checksum manifest. This holds the PUBLISH SET to the four
# kinds that sentence accounts for, so a fifth kind of asset makes this
# red and the sentence gets rewritten with it.
unaccounted=""
while IFS= read -r f; do
    [ -n "$f" ] || continue
    case "$f" in
        *.tar.gz|sbom*.json) ;;                 # listed in a checksums manifest
        checksums*.txt) ;;                      # the manifest itself
        checksums*.txt.sigstore.json) ;;        # the manifest's cosign bundle
        *.intoto.jsonl) ;;                      # a signed attestation in its own right
        *) unaccounted="${unaccounted} ${f}" ;;
    esac
done <<< "$RELEASE_FILES"
assert "every attached file is one of the kinds the release body's signature sentence accounts for" \
    "$(if [ -z "$unaccounted" ]; then echo 1; else echo "0 (${unaccounted})"; fi)"

# ossf/scorecard v5.5.0 probes/releasesHaveProvenance/impl.go:43 matches
# this suffix and no other. Without a match the whole change is invisible
# to the check it exists for.
counted=$(printf '%s\n' "$RELEASE_FILES" | grep -c '\.intoto\.jsonl$')
assert "the published set carries a name Scorecard's provenance check counts" \
    "$(if [ "$counted" -ge 1 ]; then echo 1; else echo 0; fi)"

# The bundle each call publishes must be the one the attest step in the
# SAME job produced. Scoped to the calling step: that binding appears in
# steps this script never runs in, so a file-wide count would be
# answered by the wrong step.
bound=$(awk '
    /^      - name:/ { env_ok = 0 }
    /BUNDLE: \$\{\{ steps\.attest-artifacts\.outputs\.bundle-path \}\}/ { env_ok = 1 }
    /bash scripts\/publish-provenance-asset\.sh/ { if (env_ok) n++ }
    END { print n + 0 }
' "$WF")
assert "both calls publish the bundle the attest step in their own job wrote" \
    "$(if [ "$bound" -eq 2 ]; then echo 1; else echo 0; fi)"

echo
if [ "$failures" -eq 0 ]; then
    echo "test-publish-provenance-asset: $n assertions, all passed"
    exit 0
fi
echo "test-publish-provenance-asset: $n assertions, $failures failed"
exit 1
