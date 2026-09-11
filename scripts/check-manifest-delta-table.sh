#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The release notes' manifest-delta table must agree, field by field,
# with the two manifests it claims to compare (review round 1, finding 1).
#
# WHY THIS EXISTS
#
# `docker plugin upgrade` re-prompts for privileges, and an operator
# decides whether to approve by reading that table. Before this gate the
# table was TYPED: every cell was prose an author wrote from memory, and
# the v1.9.0 manifest it compares against sits in git with nothing
# reading it. It was wrong in the direction that matters. The `env` row
# said v1.9.0 declared four settings; `git show v1.9.0:config.json`
# returns six. The delta was stated as +1 where it is +1/-2, and the
# same file said so sixty lines further down -- so the document
# contradicted itself and every check in this repository was satisfied,
# because check-privilege-sentences.sh compares head against head,
# check-manifest-parity.sh compares the two head manifests, and the Go
# tests read only the head. Nothing could see a claim about a manifest
# that is not in the working tree.
#
# WHAT IS DERIVED AND WHAT IS NOT
#
# DERIVED: both sides of every row. `jq` reduces each manifest to one
# canonical token set per field, for the OLD manifest read out of git at
# the tag the table's own marker names, and for the NEW manifest in the
# tree. The table must carry those tokens verbatim, in backticks. That
# is why the cells are sets and not sentences: "the same four plus
# DOCKER_HOST" is prose about a set, and prose about a set is exactly
# what nothing could check.
#
# NOT DERIVED, and said here rather than discovered later: the
# `prompted` column. Which manifest fields `docker plugin upgrade`
# prompts on is a property of the DAEMON, not of either manifest, so
# this gate cannot compute it and does not judge it. It is prose, and it
# is prose about someone else's software.
#
# ALSO NOT DERIVED: whether the tag in the marker is the right
# comparison point. The gate checks the table against the tag the table
# names. Naming the wrong tag produces a table that is internally
# consistent and about the wrong release.
#
# THE FIELDS. Nine. Seven of them are DERIVED FROM MOBY'S OWN LIST
# rather than from what this manifest happens to carry: the fields
# `computePrivileges` walks when the daemon builds the upgrade prompt
# (moby, daemon/pkg/plugin/backend_linux.go, func computePrivileges --
# network.type, IpcHost, PidHost, mounts[].Source, linux.devices[].Path,
# linux.AllowAllDevices, linux.capabilities). The other two, `env` and
# the mount OPTIONS, are here for reasons stated below.
#
#   linux.capabilities        one token per capability
#   network.type              one token, the type
#   ipchost                   one token, `true` or `false`
#   pidhost                   one token, `true` or `false`
#   mounts                    one token per mount, `source:options`
#   linux.devices             one token per device path, or `(absent)`
#   linux.allowalldevices     one token, `true` or `false`
#   propagatedmount           one token, or `(absent)`
#   env                       one token per setting NAME
#
# THE HEADER USED TO CLAIM SEVEN FIELDS WERE "the manifest's whole
# privilege-bearing surface" AND THEY WERE NOT. `ipchost: true` and
# `linux.allowAllDevices: true` are both prompted by the daemon and were
# projected by neither manifest gate, so either could be added with the
# whole lane green. The list was drawn from what this manifest carries;
# it is now drawn from what the daemon reads, which is the only list
# that can be complete (privilege review, 2026-09-05).
#
# MOUNT OPTIONS ARE PROJECTED AND THE DAEMON DOES NOT PROMPT ON THEM.
# That is deliberate and it is the one place this gate is deliberately
# wider than the prompt. `computePrivileges` takes mounts[].Source
# alone, so flipping /var/run/docker from `ro` to `rw` re-uses the
# operator's existing approval -- and "read-only" on that mount is the
# sentence SECURITY.md's whole grant argument rests on. A reviewer made
# exactly that edit, on both manifests, with the lane green. A row that
# an operator will not be re-prompted about is a row this table has MORE
# reason to state, not less.
#
# `env` is here for the older reason: it is not a privilege, its own
# `prompted` cell says so, and it is the field most likely to drift.
#
# `(absent)` is the empty-set spelling. A field with no value must say
# so, because an empty cell and a cell nobody filled in are the same
# thing to a parser and opposite things to a reader.
#
# REFUSES RATHER THAN PASSES when it cannot compare: no markers, no
# baseline tag in the marker, a tag git cannot resolve, no `config.json`
# at that tag, no rows. A gate that reports success having compared
# nothing is the failure this one exists to remove, one level up.
#
# Usage: check-manifest-delta-table.sh [<repo root>]
# Exit:  0 the table agrees with both manifests, 1 it does not,
#        2 refuses to judge.

set -uo pipefail

ROOT="${1:-.}"
NOTES="$ROOT/RELEASE_NOTES.md"
MANIFEST="$ROOT/config.json"
BEGIN_RE='<!-- manifest-delta: begin baseline=([^ ]+) -->'
END='<!-- manifest-delta: end -->'
ABSENT='(absent)'

fail() { echo "::error title=Manifest delta table::$*" >&2; }

refuse() {
    fail "$*"
    exit 2
}

for f in "$NOTES" "$MANIFEST"; do
    [ -f "$f" ] || refuse "$f is missing; the table and the manifest cannot be compared"
done

command -v jq >/dev/null 2>&1 || refuse "jq is not installed; both sides of this table are derived with it and neither can be guessed"

git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
    refuse "$ROOT is not a git work tree; the previous release's manifest is read from git and there is no other copy of it"

marker=$(grep -E "$BEGIN_RE" "$NOTES" | head -1)
[ -n "$marker" ] || refuse "$NOTES has no '<!-- manifest-delta: begin baseline=<tag> -->' marker; there is no table to check and no tag to check it against"
grep -qF "$END" "$NOTES" || refuse "$NOTES has no '$END' marker, so the table has no end and the rows below it would be read as part of it"

BASELINE=$(printf '%s' "$marker" | sed -E "s/.*$BEGIN_RE.*/\1/")
[ -n "$BASELINE" ] || refuse "the manifest-delta marker in $NOTES names no baseline tag"

# The tag and the blob are checked separately: "no such tag" and "that
# release had no config.json" are different facts and a reader fixing
# one wants to be told which.
git -C "$ROOT" rev-parse -q --verify "$BASELINE^{commit}" >/dev/null 2>&1 ||
    refuse "git cannot resolve '$BASELINE' in this checkout. The table compares against that release's config.json, which is only in history — a shallow clone with no tags cannot judge this table, and passing anyway would report success having compared nothing"

OLD_MANIFEST=$(git -C "$ROOT" show "$BASELINE:config.json" 2>/dev/null) ||
    refuse "'$BASELINE' has no config.json, so there is nothing to compare the current manifest against"

# One canonical token set per field, from a manifest on stdin.
#
# The projection is enumerated here rather than derived from the JSON,
# and that is a real bound: a privilege Docker honours under a key not
# named below is invisible to this gate. The list is moby's (see the
# header) plus env and the mount options, and the fields this manifest
# does not carry are listed precisely so that GAINING one cannot happen
# silently.
#
# A BOOLEAN PROJECTS AS `false` WHEN ABSENT, never as the empty set: a
# manifest that omits `ipchost` and a manifest that sets it to false
# grant the same thing, and rendering the first as `(absent)` would put
# a row in the table that changes wording when nothing changed --
# training the reader to skip it.
#
# Case is not load-bearing in the key: Docker's Go struct field is
# AllowAllDevices and the JSON tag is lowercase, so both spellings are
# accepted rather than one being silently invisible.
field_tokens() {
    local field="$1"
    case "$field" in
        linux.capabilities)    jq -r '.linux.capabilities[]? // empty' ;;
        network.type)          jq -r '.network.type // empty' ;;
        ipchost)               jq -r 'if has("ipchost") then (.ipchost | tostring) else "false" end' ;;
        pidhost)               jq -r 'if has("pidhost") then (.pidhost | tostring) else "false" end' ;;
        mounts)                jq -r '.mounts[]? | ((.source // "") + (if ((.options // []) | length) > 0 then ":" + ((.options | sort) | join(",")) else "" end))' ;;
        env)                   jq -r '.env[]?.name // empty' ;;
        propagatedmount)       jq -r 'if (.propagatedmount // "") == "" then empty else .propagatedmount end' ;;
        linux.devices)         jq -r '.linux.devices[]? | (.path // .name) // empty' ;;
        linux.allowalldevices) jq -r '((.linux.allowAllDevices // .linux.allowalldevices) // false) | tostring' ;;
        *)                     return 1 ;;
    esac
}

FIELDS=(linux.capabilities network.type ipchost pidhost mounts env propagatedmount linux.devices linux.allowalldevices)

derive() { # <field> <manifest text>
    local field="$1" text="$2" out
    out=$(printf '%s' "$text" | field_tokens "$field" | sed '/^$/d' | sort -u)
    if [ -z "$out" ]; then
        printf '%s\n' "$ABSENT"
    else
        printf '%s\n' "$out"
    fi
}

block=$(awk -v e="$END" '
    /<!-- manifest-delta: begin baseline=/ { inb = 1; next }
    index($0, e) { inb = 0; next }
    inb { print }
' "$NOTES")

rows=$(printf '%s\n' "$block" | grep -E '^\| *`[^`]+` *\|' || true)
[ -n "$rows" ] || refuse "the manifest-delta block in $NOTES has no rows; an empty table agrees with every manifest"

status=0
seen_fields=""

# cell_tokens extracts the backticked tokens of the Nth column.
cell_tokens() { # <row> <column index, 1-based over the pipe-split fields>
    printf '%s' "$1" | awk -F'|' -v n="$2" '{print $(n+1)}' |
        grep -oE '`[^`]+`' | tr -d '`' | sed '/^$/d' | sort -u
}

while IFS= read -r row; do
    [ -n "$row" ] || continue
    field=$(printf '%s' "$row" | sed -E 's/^\| *`([^`]+)`.*/\1/')

    if ! printf '%s' '' | field_tokens "$field" >/dev/null 2>&1; then
        status=1
        fail "$NOTES has a row for '$field', which is not a manifest field this gate derives (${FIELDS[*]}). A row nothing computes is a row nothing can contradict"
        continue
    fi
    case " $seen_fields " in
        *" $field "*)
            status=1
            fail "$NOTES has two rows for '$field'; whichever one is wrong, one of them is being checked and the other is being read"
            continue
            ;;
    esac
    seen_fields="$seen_fields $field"

    want_old=$(derive "$field" "$OLD_MANIFEST")
    want_new=$(derive "$field" "$(cat "$MANIFEST")")
    got_old=$(cell_tokens "$row" 2)
    got_new=$(cell_tokens "$row" 3)

    for side in old new; do
        if [ "$side" = old ]; then
            want="$want_old"; got="$got_old"; which="$BASELINE"
        else
            want="$want_new"; got="$got_new"; which="the current config.json"
        fi
        if [ -z "$got" ]; then
            status=1
            fail "the '$field' row's $which cell in $NOTES carries no backticked token. Write '$ABSENT' for an empty set: an empty cell and an unfilled cell are the same thing to this gate and opposite things to a reader"
            continue
        fi
        extra=$(comm -13 <(printf '%s\n' "$want") <(printf '%s\n' "$got"))
        missing=$(comm -23 <(printf '%s\n' "$want") <(printf '%s\n' "$got"))
        while IFS= read -r tok; do
            [ -n "$tok" ] || continue
            status=1
            fail "the '$field' row says $which has '$tok' and it does not — the table claims a privilege or setting that is not there"
        done <<< "$extra"
        while IFS= read -r tok; do
            [ -n "$tok" ] || continue
            status=1
            fail "$which has '$tok' in '$field' and the '$field' row does not list it — the table under-reports what changed, which is how a removal reads as 'nothing was dropped'"
        done <<< "$missing"
    done
done <<< "$rows"

for f in "${FIELDS[@]}"; do
    case " $seen_fields " in
        *" $f "*) ;;
        *)
            status=1
            fail "$NOTES has no row for the manifest field '$f'. A field with no row is a field the table silently says nothing about, and this table is the operator's whole account of the upgrade prompt"
            ;;
    esac
done

echo "baseline: $BASELINE"
for f in "${FIELDS[@]}"; do
    printf '  %-20s %s  ->  %s\n' "$f" \
        "$(derive "$f" "$OLD_MANIFEST" | paste -sd, -)" \
        "$(derive "$f" "$(cat "$MANIFEST")" | paste -sd, -)"
done

if [ "$status" -ne 0 ]; then
    echo "manifest delta table check FAILED" >&2
    exit 1
fi
echo "PASS  the manifest-delta table in RELEASE_NOTES.md agrees with $BASELINE:config.json and config.json, field by field"
