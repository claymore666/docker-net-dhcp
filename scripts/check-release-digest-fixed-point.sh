#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The release lane must have a reachable passing state, and the tree must
# describe the manifest it actually publishes (#910, reviews r2 and r3).
#
# WHAT WENT WRONG. Through 1.x this repository carried a per-release
# block of binary digests in docs/verifying-releases.md, and the release
# workflow compared it against the binaries it had just built. Its
# documented recovery, runbook step 10b, was: take the corrected block
# from the failed run, land it on the TAGGED commit, re-tag.
#
# From 2.0 the tag and the commit are compiled into the binary (O-4:
# -ldflags -X on pkg/buildinfo). So landing the block changes the
# commit, which changes the binary, which changes the digest the block
# states. THE RECOVERY CANNOT CONVERGE: no commit can record the digest
# of a binary built from itself. The gate therefore had no passing state
# to reach, and because it ran between the GHCR push and the Hub push,
# every attempt left a half-published release behind it.
#
# WHAT THIS CHECKS. Claims that all have to hold for the lane to have a
# reachable passing state AND for the tree to describe it truthfully,
# plus one measurement that decides which of them applies:
#
#   A (MEASURED, never skipped) — is the commit still part of the
#     binary's identity? Two builds whose ONLY difference is the commit,
#     with the link flags derived from the Makefile the release uses,
#     plus a repeat of the first as a determinism control. If the
#     control fails, this host cannot measure it and the check refuses
#     rather than guessing.
#
#   B — while A holds, no file in the tree may record a digest of a
#     binary this tree builds. That is the self-reference above, and it
#     is what would go red the moment someone pastes the block back.
#
#   C — the release workflow's signed checksum manifest must cover the
#     binary, UNDER THE PATH THE TARBALL EXTRACTS TO, and the tarball
#     must actually PACK that path. This is where the digests went
#     instead: produced by the build that makes them true, signed with
#     the tarball. Without it, removing the block would mean no
#     published digest at all, which is a worse tree than the one this
#     replaced. The second half is review r3, finding 3, MEASURED: the
#     extracted path was derived from the packaging's -C directory
#     alone, so `tar -czf ART -C plugin config.json` — a tarball with
#     no binary in it — left this claim green.
#
#   D — the container build's link flags are the host build's. Half A
#     measures the HOST `go build` through the Makefile's GO_LDFLAGS,
#     but the RELEASE binary comes out of the Dockerfile, which spells
#     the same -X flags independently. With the two unreconciled,
#     dropping Commit= from GO_LDFLAGS alone flips A to "the commit is
#     not in the binary" while the release build still bakes it in, and
#     a later commit re-adding the digest block passes (review r2,
#     finding 3, MEASURED). So the two spellings are compared as SETS,
#     and each key's binding to VERSION / COMMIT is reconciled by
#     pushing sentinel values through `make`.
#
#   E — the verification document must describe the manifest the
#     workflow writes. Not "mention it": the number of entries, every
#     ordinal that restates it, the names that go missing when only the
#     tarball was fetched, the extraction precondition, and what the
#     manifests share with each other are all DERIVED from release.yml's
#     own `sha256sum` operands and compared against what the document
#     says. docs/verifying-releases.md said "three" while the workflow
#     wrote four, so its exhaustive path exited 1 on a good release and
#     its lenient path skipped the binary in silence (review r2,
#     finding 1, MEASURED).
#
#     E5 and E6 are the two-architecture half (review r3, finding 1,
#     MEASURED). Both manifests record the binary under the path its
#     own tarball extracts to, and that path does not depend on the
#     architecture — so the two manifests record the SAME NAME for two
#     different files. A reader who unpacks both tarballs in one
#     directory keeps the second binary, and `sha256sum -c
#     checksums.txt` then reports FAILED on a good release, which the
#     page's own worked example teaches means tampering.
#     --ignore-missing does not soften it: the file is present. E5
#     compares the manifests' entry names against each other and
#     requires the page to state every name more than one of them
#     records — and, the other way, refuses a statement naming a
#     manifest that does not record it. E6 is the structural half: while
#     such a name exists, no fenced block in the page may extract more
#     than one tarball.
#
# The four cells of A x B are all reachable and all meaningful: A true
# and B violated is the defect; A true and B clean is the shipped state;
# A false and B populated is a tree that could carry digests again; A
# false and B clean is fine. C, D and E are independent of that split
# and are required in every cell, so the B-clean arm is not a universal
# satisfied by an empty domain -- there is always something left to
# fail.
#
# WHAT IT CANNOT SEE.
#   - Half A still measures the HOST `go build`. D reconciles the two
#     spellings of the link flags, so a divergence between them is now
#     loud; a divergence in build SHAPE that both spellings share (a
#     different Go toolchain, a different trimpath setting) is not.
#     `Reproducible build` covers the container half's determinism.
#   - B is keyed on the SHAPE `<64 hex><spaces><path ending in a binary
#     name>` -- what `sha256sum` writes. A digest recorded some other
#     way (a prose sentence, a table cell, base64, a truncated hash) is
#     invisible to it.
#   - C and E read the workflow's TEXT. They assert what the manifest
#     will contain and that the document says the same; they cannot
#     assert a run produced a correct digest, and no tag will be cut to
#     find out.
#   - E's worked-example arm is derived for ONE scenario -- the reader
#     who fetched the tarball and did not extract it, so exactly one
#     entry is readable. A document that adds a second worked example
#     for a different scenario would have to state its own count, and
#     this gate would judge it by the first scenario's arithmetic.
#   - E can only match manifest entries the workflow names LITERALLY. An
#     operand that is a shell variable (the tarball is, `"$ART"`) is
#     counted but its name is not checked against the document, and E5
#     cannot see a collision between two such operands.
#   - E1's ordinal arm reads a DECLARED CONVENTION of this document:
#     every "Nth entry" in it names the last entry of a manifest, so the
#     ordinal is the count restated. A page that wanted to name an entry
#     in the middle would have to write it some other way. A count
#     spelled in a third shape — digits outside an ordinal, a table
#     cell, a sentence counting the entries in words without the
#     `covers **N** files` frame — is invisible to both arms.
#   - E6 counts DISTINCT `tar -x` operands per fenced block. Two blocks,
#     each unpacking one tarball, with no instruction between them about
#     the directory, reads as fine to it; that is what E5's stated
#     collision is for.
#   - E3's precondition is keyed on the entries INSIDE the tarball, not on
#     every entry beyond it. On any tree that reaches a verdict the two
#     populations are the same set, because claim C refuses a manifest
#     that covers no binary at an extracted path -- so this operand
#     cannot change an exit code today. It is the one the sentence beside
#     it means, and it becomes load-bearing the day C is relaxed.
#   - It says nothing about whether a recorded digest is CORRECT. Under
#     A that question has no answer to give; without A it is a different
#     check.
#
# Usage: check-release-digest-fixed-point.sh [TREE] [WORKFLOW] [DOC] [DOCKERFILE]
# Exit:  0 pass, 1 a claim is violated, 2 cannot run.
set -uo pipefail

TREE="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
WF="${2:-$TREE/.github/workflows/release.yml}"
DOC="${3:-$TREE/docs/verifying-releases.md}"
DOCKERFILE="${4:-$TREE/Dockerfile}"

die() { echo "check-release-digest-fixed-point: $*" >&2; exit 2; }
note() { echo "FAIL  $*" >&2; failed=1; }
failed=0

[ -d "$TREE" ] || die "$TREE is not a directory"
[ -f "$TREE/Makefile" ] || die "$TREE/Makefile does not exist — the link flags are derived from it"
[ -f "$WF" ] || die "$WF does not exist"
[ -f "$DOC" ] || die "$DOC does not exist — claim E has lost its subject"
[ -f "$DOCKERFILE" ] || die "$DOCKERFILE does not exist — claim D cannot reconcile the release build's link flags against the host build's"
command -v go >/dev/null 2>&1 || die "go is not on PATH; half A is a measurement, not an assumption"
command -v make >/dev/null 2>&1 || die "make is not on PATH; the link flags are read from the Makefile"

# --- the binaries this tree builds --------------------------------------
# Derived, not listed. `cmd/dhcp-handler` was deleted for 2.0 and the
# release workflow still named it for two rounds; a transcribed list
# here would have inherited exactly that.
mapfile -t BINARIES < <(cd "$TREE/cmd" 2>/dev/null && for d in */; do [ -d "$d" ] && printf '%s\n' "${d%/}"; done)
[ "${#BINARIES[@]}" -gt 0 ] || die "$TREE/cmd holds no command directories — nothing to reason about"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- the link flags, from both spellings --------------------------------
# The Makefile side is EVALUATED, never read as text: GO_LDFLAGS is
# written in terms of $(BUILDINFO_PKG), $(VERSION) and $(COMMIT), and a
# textual read would compare a template against an expansion. Asking
# make is asking the subject, which is the same reason half A builds
# with these flags rather than with flags of its own.
ldflags_for() {
    make -C "$TREE" -s \
        --eval='fp-print-ldflags: ; @printf "%s\n" "$(GO_LDFLAGS)"' \
        fp-print-ldflags COMMIT="$1" VERSION="${2:-fixed-point-probe}" 2>/dev/null
}

x_flags() { grep -oE '\-X +[^ "]+=[^ "]*' <<< "${1:-}" | sed -E 's/^-X +//'; }

# --- A. is the commit part of the binary's identity? --------------------
# Building into a temp directory keeps bin/ untouched: a developer's next
# `make debug` must not run a binary this check stamped.
build_hash() {
    local out="$2"
    ( cd "$TREE" && go build -ldflags "$1" -o "$out" "./cmd/$3" ) >/dev/null 2>&1 || return 1
    sha256sum "$out" | cut -d' ' -f1
}

A_COMMIT=1111111111111111111111111111111111111111
B_COMMIT=2222222222222222222222222222222222222222

la="$(ldflags_for "$A_COMMIT")"
lb="$(ldflags_for "$B_COMMIT")"
# GO_LDFLAGS not varying with COMMIT is not a refusal: it is the OTHER
# world, the one where a recorded digest could be a fixed point again.
# Refusing here would make the whole A-false half of this check
# unreachable, and B would then be a rule nothing could ever satisfy in
# the direction that matters.
commit_dependent=no
if [ "$la" = "$lb" ]; then
    echo "note: the Makefile's link flags do not vary with COMMIT, so the binary's" >&2
    echo "  identity does not depend on the commit and a recorded digest CAN be a fixed" >&2
    echo "  point. If that is deliberate, docs/verifying-releases.md and runbook step" >&2
    echo "  10b explain the absence of a digest list by the opposite fact and are now" >&2
    echo "  stale." >&2
    BINARIES_TO_BUILD=()
else
    BINARIES_TO_BUILD=("${BINARIES[@]}")
fi

for b in ${BINARIES_TO_BUILD[@]+"${BINARIES_TO_BUILD[@]}"}; do
    h1="$(build_hash "$la" "$TMP/$b.a1" "$b")" || die "could not build ./cmd/$b with the Makefile's link flags"
    h2="$(build_hash "$la" "$TMP/$b.a2" "$b")" || die "could not rebuild ./cmd/$b"
    if [ "$h1" != "$h2" ]; then
        echo "check-release-digest-fixed-point: two builds of ./cmd/$b from identical inputs differ." >&2
        echo "  This host cannot measure whether the COMMIT matters, because nothing here is" >&2
        echo "  stable enough to compare. Refusing rather than reporting the difference as a" >&2
        echo "  commit dependence it has not shown." >&2
        exit 2
    fi
    h3="$(build_hash "$lb" "$TMP/$b.b1" "$b")" || die "could not build ./cmd/$b with the second commit"
    [ "$h1" != "$h3" ] && commit_dependent=yes
done

# --- D. the release build spells the same link flags --------------------
# Half A's verdict is about the host build. It transfers to the release
# binary only while the Dockerfile's -ldflags carry the same -X keys,
# bound to the same build arguments. Sentinels rather than the real
# values, so which key carries VERSION and which carries COMMIT is read
# out of make's own expansion instead of assumed from the spelling.
SENT_V=fixedpointsentinelversionvalue
SENT_C=fixedpointsentinelcommitvalue

mk_ld="$(ldflags_for "$SENT_C" "$SENT_V")"
df_ld="$(grep -oE '\-ldflags +"[^"]*"' "$DOCKERFILE" | sed -E 's/^-ldflags +"//; s/"$//' | tr '\n' ' ')"

mapfile -t MK_PAIRS < <(x_flags "$mk_ld")
mapfile -t DF_PAIRS < <(x_flags "$df_ld")

mk_keys="$(printf '%s\n' ${MK_PAIRS[@]+"${MK_PAIRS[@]}"} | sed 's/=.*//' | grep -v '^$' | sort -u)"
df_keys="$(printf '%s\n' ${DF_PAIRS[@]+"${DF_PAIRS[@]}"} | sed 's/=.*//' | grep -v '^$' | sort -u)"

if [ "$mk_keys" != "$df_keys" ]; then
    note "the Makefile and the Dockerfile do not link the same -X keys into the binary."
    {
        echo "  Makefile GO_LDFLAGS (evaluated):"
        printf '%s\n' "${mk_keys:-  (none)}" | sed 's/^/        /'
        echo "  $(basename "$DOCKERFILE") -ldflags:"
        printf '%s\n' "${df_keys:-  (none)}" | sed 's/^/        /'
        echo "  Half A measures the host build through the Makefile; the released binary is"
        echo "  built by the Dockerfile. While these two disagree, A's answer is about a"
        echo "  binary nobody ships: dropping a key from one side alone flips this gate's"
        echo "  verdict without changing what the release bakes in."
    } >&2
else
    for pair in ${MK_PAIRS[@]+"${MK_PAIRS[@]}"}; do
        key="${pair%%=*}"; val="${pair#*=}"
        case "$val" in
            "$SENT_V") want=VERSION ;;
            "$SENT_C") want=COMMIT ;;
            *) continue ;;
        esac
        dval=""
        for dp in ${DF_PAIRS[@]+"${DF_PAIRS[@]}"}; do
            [ "${dp%%=*}" = "$key" ] && dval="${dp#*=}"
        done
        dvar="$(printf '%s' "$dval" | sed -E 's/^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$/\1/')"
        if [ "$dvar" != "$want" ]; then
            note "$key is bound to \$$want by the Makefile and to '${dval:-nothing}' by $(basename "$DOCKERFILE")."
            echo "  The two builds would stamp different things into the same field, and half A" >&2
            echo "  would be measuring the wrong one." >&2
        fi
    done
fi

# --- B. the tree must not record a digest of a binary it builds ---------
# `sha256sum`'s own output shape, with an optional path in front of the
# name, over the tracked files.
if git -C "$TREE" rev-parse --git-dir >/dev/null 2>&1; then
    mapfile -t FILES < <(git -C "$TREE" ls-files)
else
    mapfile -t FILES < <(cd "$TREE" && find . -type f -not -path './.git/*' | sed 's|^\./||')
fi
[ "${#FILES[@]}" -gt 0 ] || die "no files to scan under $TREE"

names="$(printf '%s|' "${BINARIES[@]}")"; names="${names%|}"
DIGEST_RE="^[0-9a-f]{64}[[:space:]]+[^[:space:]]*(${names})[[:space:]]*$"

records=""
for f in "${FILES[@]}"; do
    [ -f "$TREE/$f" ] || continue
    hits="$(grep -nE "$DIGEST_RE" "$TREE/$f" 2>/dev/null)" || continue
    while IFS= read -r hit; do
        [ -n "$hit" ] && records="${records}${f}:${hit}"$'\n'
    done <<< "$hits"
done

n_records=$(printf '%s' "$records" | grep -c . || true)

if [ "$n_records" -gt 0 ] && [ "$commit_dependent" = yes ]; then
    note "$n_records file line(s) record the digest of a binary this tree builds, and the commit is part of that binary."
    printf '%s' "$records" | sed 's/^/        /' >&2
    {
        echo "  A commit that states the digest of a binary built from itself changes that"
        echo "  binary by being made, so no such record can ever be correct on the commit"
        echo "  that carries it, and any release gate comparing the two has no passing"
        echo "  state to reach. Measured just now: two builds differing only in COMMIT"
        echo "  hash differently."
        echo "  The digests belong in the release build's signed checksums manifest, which"
        echo "  is produced by the build that makes them true. See"
        echo "  docs/verifying-releases.md and runbook step 10b."
    } >&2
fi

# --- the manifests, and what the workflow puts in them -------------------
# Derived from the workflow: every checksums*.txt it writes, and the
# operands of the sha256sum invocations feeding each one.
mapfile -t MANIFESTS < <(grep -oE 'checksums[A-Za-z0-9_.-]*\.txt' "$WF" | grep -v '\.sigstore' | sort -u)
if [ "${#MANIFESTS[@]}" -eq 0 ]; then
    die "$WF writes no checksums manifest — this check's third claim has lost its subject, which is a bigger change than it can judge"
fi

# The tarball's root directory, from the packaging itself. `tar -czf ART
# -C plugin .` means an operand's path INSIDE the artifact is its path
# relative to `plugin`, which is exactly the path an operator has after
# extracting. Transcribing `rootfs/usr/sbin/` here instead would be a
# second declaration of the layout, wrong the day the layout moves.
mapfile -t TAR_ROOTS < <(grep -oE 'tar -[a-z]*c[a-z]* +[^ ]+ +-C +[^ ]+' "$WF" | awk '{print $NF}' | tr -d '"' | sort -u)
case "${#TAR_ROOTS[@]}" in
    1) TAR_ROOT="${TAR_ROOTS[0]}" ;;
    0) die "$WF packages no tarball with 'tar -c ... -C <dir>' — the extracted path of a manifest entry cannot be derived" ;;
    *) die "$WF packages tarballs from ${#TAR_ROOTS[@]} different -C directories (${TAR_ROOTS[*]}); which one a manifest entry extracts from is ambiguous and this check will not guess" ;;
esac
mapfile -t TARBALL_OPERANDS < <(grep -oE 'tar -[a-z]*c[a-z]* +[^ ]+' "$WF" | awk '{print $NF}' | tr -d '"' | sort -u)

# WHAT the tar packs, not only where it runs. `-C plugin .` packs
# everything under plugin; `-C plugin config.json` packs one file and no
# binary, and claim C was green on exactly that (review r3, finding 3,
# MEASURED) because the extracted path was derived from -C alone. The
# member list is read from the same line as the -C, so the two facts
# cannot drift apart.
# One record per tar INVOCATION, not per line: two of them chained with
# && on one line are two packagings, and reading the line would take the
# first one's operands for both. The -C directories above are found with
# grep -oE, which already sees every occurrence on a line.
tar_member_sets() {
    local line rest tok mems
    while IFS= read -r line; do
        mems=""
        if [[ "$line" =~ -C[[:space:]]+[^[:space:]]+[[:space:]]+(.*)$ ]]; then
            rest="${BASH_REMATCH[1]}"
            rest="${rest%%&&*}"; rest="${rest%%|*}"; rest="${rest%%;*}"; rest="${rest%%#*}"
            for tok in $rest; do
                tok="${tok%\"}"; tok="${tok#\"}"
                tok="${tok%\'}"; tok="${tok#\'}"
                case "$tok" in -*|'') continue ;; esac
                mems="${mems}${tok} "
            done
        fi
        printf '%s\n' "${mems% }"
    done < <(sed -e 's/&&/\n/g' -e 's/;/\n/g' "$WF" | grep -E 'tar +-[a-zA-Z]*c[a-zA-Z]* ')
}
mapfile -t TAR_MEMBER_SETS < <(tar_member_sets | sort -u)
case "${#TAR_MEMBER_SETS[@]}" in
    1) ;;
    *) die "$WF's tarballs are packed from ${#TAR_MEMBER_SETS[@]} different operand sets (${TAR_MEMBER_SETS[*]}); which files end up inside a published artifact is ambiguous and this check will not guess" ;;
esac
read -r -a TAR_MEMBERS <<< "${TAR_MEMBER_SETS[0]}"
[ "${#TAR_MEMBERS[@]}" -gt 0 ] || die "$WF packs its tarball with 'tar -c ... -C $TAR_ROOT' and names no files to put in it — what a manifest entry can be checked against cannot be derived"

# An entry, written relative to the tar root, is inside the artifact only
# if some packed operand covers it. `.` is one case of this test, not the
# test: transcribing it would be the same defect the -C derivation
# already avoided.
packs() {
    local rel="$1" mem
    for mem in ${TAR_MEMBERS[@]+"${TAR_MEMBERS[@]}"}; do
        mem="${mem%/}"; mem="${mem#./}"
        [ -z "$mem" ] && return 0
        [ "$mem" = "." ] && return 0
        case "$rel" in "$mem"|"$mem"/*) return 0 ;; esac
    done
    return 1
}

# Emits one "cwd<TAB>operand" per manifest entry.
manifest_entries() {
    local m="$1" mre line cwd rest op
    local -a ops
    mre="$(printf '%s' "$m" | sed 's/[.[\*^$]/\\&/g')"
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        cwd=""
        [[ "$line" =~ \(\ *cd\ +([^\ \&\)]+)\ *\&\& ]] && cwd="${BASH_REMATCH[1]}"
        rest="${line#*sha256sum }"
        rest="${rest%%>*}"
        rest="${rest%%)*}"
        read -r -a ops <<< "$rest"
        for op in ${ops[@]+"${ops[@]}"}; do
            op="${op%\"}"; op="${op#\"}"
            op="${op%\'}"; op="${op#\'}"
            [ -n "$op" ] || continue
            case "$op" in -*) continue ;; esac
            printf '%s\t%s\n' "$cwd" "$op"
        done
    done < <(grep -E "sha256sum [^|]*>>? *\"?${mre}\"?" "$WF")
}

# --- C. the signed manifest covers the binary, at its extracted path ----
declare -A ENTRY_COUNT=() MISSING_NAMES=() INSIDE_TARBALL=()
for m in "${MANIFESTS[@]}"; do
    mapfile -t ENTRIES < <(manifest_entries "$m")
    if [ "${#ENTRIES[@]}" -eq 0 ]; then
        note "$WF names $m but no sha256sum invocation writes it."
        continue
    fi
    ENTRY_COUNT["$m"]="${#ENTRIES[@]}"
    covered=no
    missing=""
    inside=""
    for e in "${ENTRIES[@]}"; do
        cwd="${e%%$'\t'*}"; op="${e#*$'\t'}"
        is_tarball=no
        for t in ${TARBALL_OPERANDS[@]+"${TARBALL_OPERANDS[@]}"}; do
            [ "$op" = "$t" ] && is_tarball=yes
        done
        [ "$is_tarball" = no ] && case "$op" in *'$'*) : ;; *) missing="${missing}${op} " ;; esac

        # Two different populations, deliberately not one variable. MISSING
        # is what a reader who fetched only the tarball cannot read, which is
        # every entry but the tarball; INSIDE is the entries that exist only
        # after unpacking. They coincide on this tree and E3 needs the second.
        full_any="$op"; [ -n "$cwd" ] && full_any="$cwd/$op"
        case "${full_any#./}" in "$TAR_ROOT"/*) inside="${inside}${op} " ;; esac

        base="${op##*/}"
        is_bin=no
        for b in "${BINARIES[@]}"; do [ "$base" = "$b" ] && is_bin=yes; done
        [ "$is_bin" = yes ] || continue
        covered=yes

        full="$op"; [ -n "$cwd" ] && full="$cwd/$op"
        full="${full#./}"
        case "$full" in
            "$TAR_ROOT"/*)
                rel="${full#"$TAR_ROOT"/}"
                if ! packs "$rel"; then
                    note "$m records the binary at '$rel' inside the tarball, which 'tar -c ... -C $TAR_ROOT ${TAR_MEMBERS[*]}' does not pack."
                    {
                        echo "  The digest is correct and the entry is unreachable: the published tarball"
                        echo "  carries only ${TAR_MEMBERS[*]}, so an operator who extracts it has no such"
                        echo "  file. 'sha256sum -c $m' then fails on a good release and --ignore-missing"
                        echo "  skips the one entry that is the point of the manifest."
                    } >&2
                fi
                if [ "$rel" != "$op" ]; then
                    note "$m records the binary as '$op', but after extraction that file is at '$rel'."
                    {
                        echo "  The invocation runs in '${cwd:-.}' and the artifact is packed with"
                        echo "  'tar -c ... -C $TAR_ROOT', so an operand is checkable by an operator only"
                        echo "  when it is written relative to $TAR_ROOT. As it stands, 'sha256sum -c $m'"
                        echo "  in the directory where the tarball was unpacked names a file that is not"
                        echo "  there, and --ignore-missing skips it in silence rather than failing."
                    } >&2
                fi
                ;;
            *)
                note "$m records '$full', which the tarball packed from '$TAR_ROOT' does not contain."
                echo "  No operator can produce that file from a published asset, so the entry can" >&2
                echo "  never be checked — only skipped by --ignore-missing." >&2
                ;;
        esac
    done
    MISSING_NAMES["$m"]="$missing"
    INSIDE_TARBALL["$m"]="$inside"
    if [ "$covered" = no ]; then
        note "$m covers no binary: nothing feeding it names $(printf '%s ' "${BINARIES[@]}")under a path."
        {
            echo "  This manifest is the only signed statement of what the published binary"
            echo "  hashes to, now that no commit can carry one. Record the binary under the"
            echo "  path it has inside the release tarball, so an operator who extracts it"
            echo "  can run 'sha256sum --ignore-missing -c $m'."
        } >&2
    fi
done

# --- E. the document describes the manifest the workflow writes ---------
word_for() {
    case "$1" in
        1) echo one ;; 2) echo two ;; 3) echo three ;; 4) echo four ;;
        5) echo five ;; 6) echo six ;; 7) echo seven ;; 8) echo eight ;;
        9) echo nine ;; 10) echo ten ;; 11) echo eleven ;; 12) echo twelve ;;
        *) echo "$1" ;;
    esac
}
num_for() {
    case "$1" in
        one) echo 1 ;; two) echo 2 ;; three) echo 3 ;; four) echo 4 ;;
        five) echo 5 ;; six) echo 6 ;; seven) echo 7 ;; eight) echo 8 ;;
        nine) echo 9 ;; ten) echo 10 ;; eleven) echo 11 ;; twelve) echo 12 ;;
        *) echo "$1" ;;
    esac
}
ord_num() {
    case "$1" in
        first) echo 1 ;; second) echo 2 ;; third) echo 3 ;; fourth) echo 4 ;;
        fifth) echo 5 ;; sixth) echo 6 ;; seventh) echo 7 ;; eighth) echo 8 ;;
        ninth) echo 9 ;; tenth) echo 10 ;; eleventh) echo 11 ;; twelfth) echo 12 ;;
        *) printf '%s\n' "${1%%[a-z][a-z]}" ;;
    esac
}

# The prose arms read the document FLATTENED. A count sentence, an
# ordinal or a shared-entry statement wraps across a newline as readily
# as it fits on one, and a line-keyed grep silently judges only the
# sentences that happened to fit -- MEASURED: `the fourth / entry
# described above` was invisible to the ordinal arm while its two
# unwrapped siblings were reported. The block arms below stay
# line-keyed, because a block's structure is what they are about.
DOC_FLAT="$TMP/doc.flat"
tr '\n' ' ' < "$DOC" | tr -s ' ' > "$DOC_FLAT"

# E1: every manifest the workflow writes is described, and every place
# the document states its size states the size the workflow produces.
for m in "${MANIFESTS[@]}"; do
    want="${ENTRY_COUNT[$m]:-}"
    [ -n "$want" ] || continue
    mre="$(printf '%s' "$m" | sed 's/[.[\*^$]/\\&/g')"
    mapfile -t STATED < <(grep -oE "\`${mre}\` covers \*\*[A-Za-z0-9]+\*\* files" "$DOC_FLAT" | sed -E 's/.*\*\*([A-Za-z0-9]+)\*\*.*/\1/')
    if [ "${#STATED[@]}" -eq 0 ]; then
        note "$(basename "$DOC") never says how many files \`$m\` covers; the workflow puts $want in it."
        {
            echo "  An operator verifies a release from this document. It has to state the"
            echo "  manifest's size in the shape '\`$m\` covers **$(word_for "$want")** files', once per"
            echo "  place it makes the claim, so that adding an entry in release.yml goes red"
            echo "  here rather than silently making the page wrong."
        } >&2
        continue
    fi
    for s in "${STATED[@]}"; do
        got="$(num_for "$(printf '%s' "$s" | tr '[:upper:]' '[:lower:]')")"
        if [ "$got" != "$want" ]; then
            note "$(basename "$DOC") says \`$m\` covers $s files; release.yml writes $want entries into it."
            echo "  Followed as written that document exits 1 on a good release, or skips an" >&2
            echo "  entry in silence. The entries are: $(manifest_entries "$m" | awk -F'\t' '{printf "%s%s ", ($1==""?"":$1"/"), $2}')" >&2
        fi
    done
done

# E1b: the count restated as an ORDINAL. The shape above reads one
# sentence per manifest; the page also points at the binary as "that
# fourth entry", in three places, and a fifth entry left all three stale
# while E1 stayed green (review r3, carried unverified, MEASURED). Every
# ordinal-entry phrase in this page names a manifest's LAST entry, so
# the ordinal is the count written another way and is judged as one.
COUNTS_WRITTEN=""
for m in "${MANIFESTS[@]}"; do
    COUNTS_WRITTEN="${COUNTS_WRITTEN}${ENTRY_COUNT[$m]:-0} "
done
while IFS= read -r phrase; do
    [ -n "$phrase" ] || continue
    ord="$(printf '%s' "$phrase" | tr '[:upper:]' '[:lower:]' | awk '{print $1}')"
    n="$(ord_num "$ord")"
    case "$n" in ''|*[!0-9]*) continue ;; esac
    hit=no
    for c in $COUNTS_WRITTEN; do [ "$c" = "$n" ] && hit=yes; done
    if [ "$hit" = no ]; then
        note "$(basename "$DOC") calls something the '$phrase' of a manifest, and release.yml writes ${COUNTS_WRITTEN}entr(ies)."
        {
            echo "  An ordinal here is the entry count restated: this page names the binary as"
            echo "  the LAST entry of the manifest that records it. Add an entry to release.yml"
            echo "  and this sentence has to move with the count in the shape above, or the page"
            echo "  points a reader at an entry that is no longer the one it means."
        } >&2
    fi
done < <(grep -oiE '(first|second|third|fourth|fifth|sixth|seventh|eighth|ninth|tenth|eleventh|twelfth|[0-9]+(st|nd|rd|th)) entr(y|ies)' "$DOC_FLAT")

# E5. Two architectures, one path. Each manifest records the binary
# under the path ITS OWN tarball extracts to, and that path does not
# depend on the architecture -- so both record the same name for two
# different files, and a reader who unpacks both tarballs in one
# directory is told a good release was tampered with (review r3,
# finding 1, MEASURED). Nothing compared the two manifests' entry names.
# This does, from the workflow's own operands, and reads the page's
# statement about them in both directions.
declare -A RECORDED_BY=()
for m in "${MANIFESTS[@]}"; do
    while IFS= read -r e; do
        [ -n "$e" ] || continue
        op="${e#*$'\t'}"
        case "$op" in *'$'*) continue ;; esac
        RECORDED_BY["$op"]="${RECORDED_BY[$op]:-}$m "
    done < <(manifest_entries "$m")
done

COLLISIONS=()
for op in "${!RECORDED_BY[@]}"; do
    read -r -a rb <<< "${RECORDED_BY[$op]}"
    [ "${#rb[@]}" -gt 1 ] && COLLISIONS+=("$op")
done

# Every statement in the page that claims two manifests share an entry,
# parsed once and used for both directions. The manifests are read as a
# SET, so the sentence may name them in either order.
declare -A STATED_SHARE=()
while IFS= read -r stmt; do
    [ -n "$stmt" ] || continue
    mapfile -t toks < <(printf '%s\n' "$stmt" | grep -oE '`[^`]+`' | tr -d '`')
    [ "${#toks[@]}" -ge 3 ] || continue
    shared_name="${toks[$(( ${#toks[@]} - 1 ))]}"
    unset "toks[$(( ${#toks[@]} - 1 ))]"
    mapfile -t named < <(printf '%s\n' "${toks[@]}" | sort -u)
    bad=""
    for t in "${named[@]}"; do
        case " ${RECORDED_BY[$shared_name]:-} " in *" $t "*) ;; *) bad="$bad $t" ;; esac
    done
    if [ -n "$bad" ]; then
        note "$(basename "$DOC") says \`$shared_name\` is recorded by$bad, and release.yml writes no such entry into it."
        {
            echo "  A shared-entry warning the workflow does not have teaches a reader to take a"
            echo "  precaution against nothing, and it hides the one they do need. release.yml"
            echo "  records \`$shared_name\` in: ${RECORDED_BY[$shared_name]:-(no manifest)}"
        } >&2
    fi
    STATED_SHARE["$shared_name"]="${STATED_SHARE[$shared_name]:-}|$(printf '%s ' "${named[@]}")"
done < <(grep -oE '(`[^`]+`[ ,]+(and )?)+(both|all) record `[^`]+`' "$DOC_FLAT")

for op in $(printf '%s\n' ${COLLISIONS[@]+"${COLLISIONS[@]}"} | sort); do
    read -r -a rb <<< "${RECORDED_BY[$op]}"
    mapfile -t rb < <(printf '%s\n' "${rb[@]}" | sort -u)
    want="$(printf '%s ' "${rb[@]}")"
    case "${STATED_SHARE[$op]:-}" in
        *"|$want"*) ;;
        *)
            joined=""
            for i in "${!rb[@]}"; do
                if [ "$i" -eq 0 ]; then joined="\`${rb[0]}\`"
                elif [ "$i" -eq $(( ${#rb[@]} - 1 )) ]; then joined="$joined and \`${rb[$i]}\`"
                else joined="$joined, \`${rb[$i]}\`"; fi
            done
            quant=both; [ "${#rb[@]}" -gt 2 ] && quant=all
            note "$(basename "$DOC") never says that ${#rb[@]} manifests record \`$op\`."
            {
                echo "  ${rb[*]} each record that name, for a different file. A reader who unpacks"
                echo "  both tarballs in one directory keeps the last one extracted, and every other"
                echo "  manifest then reports it FAILED on a good release -- which this page's own"
                echo "  worked example says means the release was tampered with. --ignore-missing"
                echo "  does not soften it, because the file is present."
                echo "  Say so, in the shape '$joined $quant record \`$op\`', and tell the reader"
                echo "  to verify one architecture per directory."
            } >&2
            ;;
    esac
done

# E2/E3: the document's blocks. A block that runs `sha256sum -c <m>` for
# a manifest with an entry INSIDE the tarball has to unpack the tarball
# first, in that same block -- otherwise the reader who follows it
# either fails on a good release or skips the binary without being told.
# A worked example of a partial check has to tally with the manifest it
# is an example of.
mapfile -t DOC_LINES < <(cat "$DOC")
in_block=0
block=""
check_blocks=0
example_blocks=0
flush_block() {
    local body="$1" m mre tarline shaline n_stated n_failed missing ok_m name
    local -a xops
    [ -n "$body" ] || return 0

    # E6, the structural half of the two-architecture claim. While two
    # manifests record one name, a block that unpacks two tarballs is
    # the trap itself: the second extraction overwrites the first
    # architecture's file, and the manifest that recorded it reports
    # FAILED on a good release. Keyed on the collision, so a release
    # whose manifests share nothing may unpack whatever it likes.
    if [ "${#COLLISIONS[@]}" -gt 0 ]; then
        mapfile -t xops < <(printf '%s\n' "$body" |
            grep -oE '(^|[^a-z])tar +-[a-zA-Z]*x[a-zA-Z]* +[^ ]+' |
            awk '{print $NF}' | tr -d '"' | sort -u)
        if [ "${#xops[@]}" -gt 1 ]; then
            note "$(basename "$DOC") has one block that unpacks ${#xops[@]} tarballs: ${xops[*]}"
            {
                echo "  ${COLLISIONS[*]} is recorded by more than one manifest, so the second"
                echo "  extraction overwrites the first archive's copy and the first manifest then"
                echo "  reports it FAILED on a good release. One tarball per block, one"
                echo "  architecture per directory."
            } >&2
        fi
    fi

    for m in "${MANIFESTS[@]}"; do
        mre="$(printf '%s' "$m" | sed 's/[.[\*^$]/\\&/g')"
        printf '%s\n' "$body" | grep -E "sha256sum[^|]* -c +\"?${mre}\"?" >/dev/null || continue
        # Counted here, BEFORE the precondition applies: this is "the
        # document checks a manifest at all", which is what the refusal at
        # the end asks about. Counting it inside the precondition's own
        # domain would make the refusal fire on a document that checks a
        # manifest with nothing inside the tarball -- a correct document.
        check_blocks=$((check_blocks + 1))
        # The precondition itself is only for a manifest that records
        # something INSIDE the tarball. An SBOM sitting beside it needs no
        # unpacking, and demanding one would be this gate inventing a
        # precondition the manifest does not have.
        [ -n "${INSIDE_TARBALL[$m]:-}" ] || continue
        shaline="$(printf '%s\n' "$body" | grep -nE "sha256sum[^|]* -c +\"?${mre}\"?" | head -1 | cut -d: -f1)"
        tarline="$(printf '%s\n' "$body" | grep -nE '(^|[^a-z])tar +-[a-zA-Z]*x' | head -1 | cut -d: -f1)"
        if [ -z "$tarline" ] || [ "$tarline" -ge "$shaline" ]; then
            note "$(basename "$DOC") runs 'sha256sum -c $m' in a block that never unpacks the tarball first."
            {
                echo "  $m records an entry under the path it has once the tarball is extracted."
                echo "  Without the extraction the reader either sees FAILED open or read on a good"
                echo "  release, or -- with --ignore-missing -- never checks that entry and is not"
                echo "  told. Put the 'tar -x' in the same block, ahead of the check."
            } >&2
        fi
    done

    printf '%s\n' "$body" | grep 'listed files could not be read' >/dev/null || return 0
    example_blocks=$((example_blocks + 1))
    n_stated="$(printf '%s\n' "$body" | sed -nE 's/.*WARNING: ([0-9]+) listed files could not be read.*/\1/p' | head -1)"
    n_failed="$(printf '%s\n' "$body" | grep -c 'FAILED open or read' || true)"
    if [ "$n_stated" != "$n_failed" ]; then
        note "$(basename "$DOC")'s worked example says $n_stated listed files could not be read and shows $n_failed."
        return 0
    fi
    ok_m=""
    for m in "${MANIFESTS[@]}"; do
        [ "$(( ${ENTRY_COUNT[$m]:-0} - 1 ))" = "$n_stated" ] || continue
        missing=yes
        for name in ${MISSING_NAMES[$m]:-}; do
            printf '%s\n' "$body" | grep -F -- "$name: FAILED open or read" >/dev/null || missing=no
        done
        [ "$missing" = yes ] && ok_m="$m"
    done
    if [ -z "$ok_m" ]; then
        note "$(basename "$DOC")'s worked example does not tally with any manifest release.yml writes."
        {
            echo "  It shows $n_stated files unreadable. The example is the reader who fetched the"
            echo "  tarball and did not extract it, so that number is one less than the manifest's"
            echo "  entry count and the unreadable names are the other entries. release.yml writes:"
            for m in "${MANIFESTS[@]}"; do
                echo "    $m: ${ENTRY_COUNT[$m]:-0} entries; without the tarball: ${MISSING_NAMES[$m]:-(none named literally)}"
            done
        } >&2
    fi
}
for line in ${DOC_LINES[@]+"${DOC_LINES[@]}"}; do
    trimmed="${line#"${line%%[![:space:]]*}"}"
    trimmed="${trimmed#> }"
    trimmed="${trimmed#"${trimmed%%[![:space:]]*}"}"
    case "$trimmed" in
        '```'*)
            if [ "$in_block" = 1 ]; then flush_block "$block"; block=""; in_block=0; else in_block=1; fi
            continue ;;
    esac
    [ "$in_block" = 1 ] && block="${block}${line}"$'\n'
done
if [ "$check_blocks" -eq 0 ]; then
    die "$(basename "$DOC") contains no block that checks a manifest with 'sha256sum -c'. Claim E's precondition arm has nothing to judge, and reporting a clean pass over that is how a check goes quiet instead of red."
fi
if [ "$example_blocks" -eq 0 ]; then
    die "$(basename "$DOC") carries no worked example of a partial check ('listed files could not be read'). That example is what tells a reader a good release can print FAILED lines; without it claim E's tally arm has an empty domain."
fi

[ "$failed" -eq 0 ] || exit 1

echo "PASS  release digests are a fixed point: commit-in-binary=${commit_dependent}, ${n_records} in-tree digest record(s) of ${#BINARIES[@]} built binar(ies), ${#MANIFESTS[@]} signed manifest(s) covering them at extracted paths the tarball packs from '$TAR_ROOT' (${TAR_MEMBERS[*]}), link flags reconciled across Makefile and $(basename "$DOCKERFILE"), $(basename "$DOC") in step with ${#MANIFESTS[@]} manifest(s) over ${check_blocks} check block(s) and ${example_blocks} worked example(s), ${#COLLISIONS[@]} entry name(s) recorded by more than one manifest and stated"
