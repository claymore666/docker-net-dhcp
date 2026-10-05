#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Which dependency pins may reach dev, main or a release tag (#1228).
#
# Expires-when: the library ships one tagged release per plugin fix and no
#   branch pins an unreleased commit any more (#1228).
#
# A plugin branch may pin dhcp-golib to an unreleased commit (a Go
# pseudo-version) while it tests a library fix. The rule by target:
#
#   draft pull request   skipped, reason printed; ready_for_review re-runs
#   dev                  a pseudo-version is allowed only for the library,
#                        and only when its commit is reachable from the
#                        library's dev or main branch
#   main, release tag    any pseudo-version is red
#   every target         a `replace` to a local path or to another module
#                        (a fork) is red
#
# A pseudo-version of any OTHER module is red on dev too: reachability is
# only measured against the library's branches, so a third-party commit
# has no authority to consult.
#
# Versions come from `go mod edit -json`, which tokenises go.mod as Go
# does (blocks, quotes, comments, `// indirect`); a regex over the text
# would read a comment as a pin. The pseudo-version pattern is x/mod's
# module.IsPseudoVersion, plus a looser `<14 digits>-<12 hex>` net.
#
# Reachability is the compare API: `compare/<branch>...<sha>` answers
# `behind` or `identical` when the commit is in the branch. An API answer
# that is not one of the four statuses, a rate limit or an outage is exit
# 2, never "reachable"; a 404 (unknown commit or branch) is a finding.
#
# Usage: check-pseudo-version-pin.sh --event <name> --target <ref-name>
#          [--draft <true|false>] [--tree <dir>] [--ref <git-ref>]
#   --event   github.event_name; only a pull_request can be a draft
#   --target  the base branch of a PR, or the pushed branch or tag
#   --draft   only the literal `true`, on a pull_request, skips the gate
#   --ref     read the module files out of that commit in --tree's object
#             database instead of its work tree. release.yml uses it: the
#             tag under judgement is never checked out there (#914).
# Env: PIN_RETRY_SLEEP seconds between API attempts (default 3; the
#   self-test sets 0). The API is read through `gh`, so GH_TOKEN applies.
#
# Exit: 0 clean or skipped, 1 a pin is refused, 2 cannot judge.

set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

LIB_REPO='claymore666/dhcp-golib'
LIB_MODULE="github.com/$LIB_REPO"
event='' target='' draft='' tree='' gitref=''
while [ "$#" -gt 0 ]; do
    case "$1" in
        --event|--target|--draft|--tree|--ref)
            [ "$#" -ge 2 ] || gate_refuse "$1 wants a value"
            case "$1" in
                --event) event="$2" ;;
                --target) target="$2" ;;
                --draft) draft="$2" ;;
                --tree) tree="$2" ;;
                --ref) gitref="$2" ;;
            esac
            shift 2 ;;
        *) gate_refuse "unknown argument '$1'; usage is in the script header" ;;
    esac
done
[ -n "$event" ] || gate_refuse "--event is required"
[ -n "$target" ] || gate_refuse "--target is required"

if [ "$event" = pull_request ] && [ "$draft" = true ]; then
    echo "check-pseudo-version-pin: skipped, this is a draft pull request into '$target'." \
         "A draft cannot be merged, so an interim pin is safe here; the gate re-runs" \
         "when the pull request is marked ready for review."
    exit 0
fi

export GOTOOLCHAIN=local
command -v go >/dev/null 2>&1 || gate_refuse "go is not on PATH; go.mod is read with 'go mod edit'"
command -v jq >/dev/null 2>&1 || gate_refuse "jq is not on PATH"

tree="${tree:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
[ -d "$tree" ] || gate_refuse "$tree is not a directory"
git -C "$tree" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || gate_refuse "$tree is not in a git work tree; the modules are discovered through git"
if [ -n "$gitref" ]; then
    git -C "$tree" rev-parse -q --verify "$gitref^{commit}" >/dev/null \
        || gate_refuse "$gitref is not a commit in $tree"
    mapfile -d '' -t listed < <(git -C "$tree" ls-tree -r -z --name-only "$gitref"; printf 'rc=%s\0' "$?")
else
    mapfile -d '' -t listed < <(git -C "$tree" ls-files -z --cached --others --exclude-standard; printf 'rc=%s\0' "$?")
fi
[ "${listed[-1]}" = rc=0 ] || gate_refuse "listing the files of $tree failed (${listed[-1]})"
unset 'listed[-1]'
# Filtered in bash: no grep -z here, ugrep reads -z as --decompress.
files=()
for p in "${listed[@]}"; do
    [[ $p =~ (^|/)go\.(mod|work)$ ]] || continue
    [[ $p =~ (^|/)testdata/ ]] && continue
    [ -n "$gitref" ] || [ -e "$tree/$p" ] || continue
    files+=("$p")
done
[ "${#files[@]}" -gt 0 ] || gate_refuse "no go.mod under $tree; a pass here would have read nothing"
scratch=''
if [ -n "$gitref" ]; then
    scratch="$(mktemp -d)" || gate_refuse "mktemp failed"
    trap 'rm -rf "$scratch"' EXIT
fi

# x/mod module.IsPseudoVersion, with [0-9] for \d.
pseudo_re='^v[0-9]+\.(0\.0-|[0-9]+\.[0-9]+-([^+]*\.)?0\.)[0-9]{14}-[A-Za-z0-9]+(\+[0-9A-Za-z.-]+)?$'
loose_re='[-.][0-9]{14}-([0-9a-fA-F]{12})'

bad=0
finding() { bad=1; printf '::error file=%s,title=Pseudo-version pin::%s\n' "$1" "$2" >&2; }

# reach <sha> <branch>: 0 in the branch, 1 not in it, 3 not found, 2 unreadable.
reach() {
    local sha="$1" branch="$2" attempt out err rc
    for attempt in 1 2 3; do
        err="$(mktemp)" || gate_refuse "mktemp failed"
        out="$(gh api "repos/$LIB_REPO/compare/$branch...$sha?per_page=1" --jq .status 2>"$err")"
        rc=$?
        if [ "$rc" -eq 0 ]; then
            rm -f "$err"
            case "$out" in
                behind|identical) return 0 ;;
                ahead|diverged) return 1 ;;
            esac
            REACH_WHY="unexpected status '$out'"
        elif grep -q 'HTTP 404' "$err"; then
            rm -f "$err"
            return 3
        else
            REACH_WHY="$(head -c 200 "$err" | tr '\n' ' ')"
            rm -f "$err"
        fi
        [ "$attempt" -eq 3 ] || sleep "${PIN_RETRY_SLEEP:-3}"
    done
    return 2
}

judge_pin() { # <file> <module> <version>
    local f="$1" mod="$2" v="$3" sha branch rc=1
    if [[ ! $v =~ $pseudo_re ]] && [[ ! $v =~ $loose_re ]]; then
        return 0
    fi
    [[ $v =~ $loose_re ]] && sha="${BASH_REMATCH[1]}"
    case "$target" in
        dev) ;;
        *) finding "$f" "$mod is pinned to the pseudo-version $v; only a tagged release may reach '$target'."
           return 0 ;;
    esac
    case "$mod" in
        "$LIB_MODULE"|"$LIB_MODULE"/v[0-9]*) ;;
        *) finding "$f" "$mod is pinned to the pseudo-version $v; a pin is judged against the library's branches, and this is not the library."
           return 0 ;;
    esac
    [ -n "${sha:-}" ] || { finding "$f" "$mod at $v: no commit hash could be read from it."; return 0; }
    for branch in dev main; do
        reach "$sha" "$branch"; rc=$?
        case "$rc" in
            0) echo "check-pseudo-version-pin: $mod $v is in $LIB_REPO $branch"; return 0 ;;
            2) gate_refuse "cannot read whether $sha is in $LIB_REPO $branch: ${REACH_WHY:-no answer}. An unreadable answer is never treated as reachable." ;;
            3) finding "$f" "$mod at $v: $LIB_REPO has no commit $sha (or no branch '$branch'); push it to the library's dev first."
               return 0 ;;
        esac
    done
    finding "$f" "$mod at $v: commit $sha is on no $LIB_REPO dev or main branch; merge it to the library's dev first."
}

for rel in "${files[@]}"; do
    f="$tree/$rel"
    if [ -n "$gitref" ]; then
        f="$scratch/$rel"
        mkdir -p "$(dirname "$f")"
        git -C "$tree" show "$gitref:$rel" > "$f" || gate_refuse "cannot read $rel at $gitref"
    fi
    case "$rel" in
        *go.work) edit=(go work edit -json "$f") ;;
        *) edit=(go mod edit -json "$f") ;;
    esac
    json="$("${edit[@]}" 2>&1)" || gate_refuse "'${edit[*]:0:3}' failed on $rel: ${json:0:200}"
    rows="$(printf '%s' "$json" | jq -r '
        (.Require[]? | ["require", .Path, .Version, "-", "-"]),
        (.Replace[]? | ["replace", .Old.Path, "-", .New.Path, (.New.Version // "-")])
        | join("|")')" || gate_refuse "jq could not read the module description of $rel"
    while IFS='|' read -r kind path ver newpath newver; do
        [ -n "$kind" ] || continue
        case "$kind" in
            require) judge_pin "$rel" "$path" "$ver" ;;
            replace)
                if [ "$newver" = - ]; then
                    finding "$rel" "replace $path => $newpath points at a local path; no runner has that directory and the build would not be the one go.sum covers."
                elif [ "$newpath" != "$path" ]; then
                    finding "$rel" "replace $path => $newpath $newver points at another module; a fork is not a release of $path."
                else
                    judge_pin "$rel" "$newpath" "$newver"
                fi ;;
        esac
    done <<< "$rows"
done

[ "$bad" -eq 0 ] || exit 1
echo "check-pseudo-version-pin: ${#files[@]} module file(s) judged for '$target' ($event); no refused pin"
