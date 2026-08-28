#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every `uses:` in every workflow must name a 40-hex commit SHA (#831).
#
# WHAT THIS IS. Pinning is at 100% today and was measured to be enforced
# by nothing: `actions/checkout@v7` was planted in a workflow and the
# whole gate corpus was run, and the red set came back BYTE-IDENTICAL to
# the clean-tree baseline -- zero new reds. `allowed_actions` is `all` at
# the API level too. The property is real and is held entirely by whoever
# is reviewing that day.
#
# THE CONTROL IS WHY THE ZERO IS BELIEVABLE, and it belongs in the record
# rather than in someone's memory: a substantial minority of the gates
# are red when invoked bare, because they need CI context. Without the
# clean-tree baseline first, the mutant run reads as "N gates caught it"
# -- a false confirmation of exactly the thing being tested for. The
# DIFFERENTIAL is the result; the count of bare reds never was, and it is
# deliberately not quoted: it differs between `main` and `dev`, and
# `check-apk-pins` returns 125 on a network timeout, so the number moves
# with the network. This file is what gets read in a year.
#
# STATED PLAINLY: THIS IS PROPHYLACTIC. No incident sits behind it.
# Precautionary gates are a small minority of the corpus -- `check-apk-
# pins.sh` is the other one -- and that is named here rather than buried,
# because the finding of the CI review is that gates get added faster
# than the reason for them gets recorded, so a gate whose reason is "no
# incident yet" has to say so in its own header, where the person
# deciding whether to delete it will look. No fraction is quoted for the
# same reason as above: the corpus was 60 gates when this was written and
# 62 two days later, and a denominator nobody can check is a denominator
# nobody maintains.
#
# A tag is not a pin. `@v7` and `@main` are mutable: whoever controls the
# action repository can repoint them at any commit, and the next run of
# every workflow executes it. That is the whole reason the ecosystem
# pins, and it is why a MOVED tag and a NEW tag are the same risk.
#
# Inputs (environment):
#   WORKFLOW_DIR      directory scanned instead of .github/workflows.
#   ACTION_SCAN_ROOT  tree searched for composite actions instead of the
#                     repository root.
#
# BOTH MOVE DISCOVERY ONLY -- every judgement below runs on whatever they
# find, so the self-test drives this same code and not a stub of it.
#
# Exit: 0 every `uses:` is SHA-pinned
#       1 at least one is not, and each is named with file:line
#       2 CANNOT JUDGE. A universal over an empty set is true and
#         worthless, and a universal over a set that quietly lost members
#         is worse, so both refuse instead of passing:
#           - no workflow directory, no workflow files, or no `uses:` at
#             all -- the set is empty
#           - a workflow file that cannot be read -- the set is PARTIAL,
#             and a smaller count with a confident success line is the
#             same vacuity in the permissive direction
#           - a `uses:` this parser could not resolve to a ref -- see the
#             residue check below
#           - a composite action outside discovery -- see the boundary
#             note on the `./*` exemption

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW_DIR="${WORKFLOW_DIR:-$ROOT/.github/workflows}"

refuse() {
    echo "::error title=Action pinning cannot be judged::$*" >&2
    exit 2
}

[ -d "$WORKFLOW_DIR" ] || refuse "no workflow directory at $WORKFLOW_DIR."

# BOTH EXTENSIONS, DELIBERATELY. GitHub honours .yml and .yaml alike, and
# this tree contains 24 of one and 1 of the other. A gate matching only
# the common extension would pass over the odd file forever, which is the
# defect class this whole family exists to stop.
mapfile -t FILES < <(find "$WORKFLOW_DIR" -maxdepth 1 -type f \
    \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

[ "${#FILES[@]}" -gt 0 ] || refuse "no .yml or .yaml files under $WORKFLOW_DIR; there is nothing to judge and 'all of nothing is pinned' is not an answer."

# THE COMPOSITE-ACTION BOUNDARY, ENFORCED RATHER THAN DESCRIBED. The
# `./*` exemption below is sound about the REFERENCE and says nothing
# about the ACTION: a local composite action's own `action.yml` can
# `uses:` a third party at a tag, and discovery reads $WORKFLOW_DIR, not
# `.github/actions`. That limit used to be a paragraph telling whoever
# adds the first composite action to widen discovery -- an unrun
# checklist in a file they have no reason to open, and bare `actionlint`
# does not catch it either. So the exemption carries a check: if an
# `action.yml`/`action.yaml` exists anywhere discovery does not cover,
# this refuses. Widening discovery is then a decision someone makes with
# a red gate in front of them.
ACTION_SCAN_ROOT="${ACTION_SCAN_ROOT:-$ROOT}"

if git -C "$ACTION_SCAN_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    # Judge what the repository actually holds. A bare walk of a
    # maintainer's checkout descends into ignored trees -- the git
    # worktrees under .claude/ are whole other branches -- and refuses
    # over a file this branch does not contain. CI checks out fresh and
    # would stay green, so that false red only ever fires on a
    # maintainer's machine, which is how a gate gets learned as noise.
    # `--others --exclude-standard` is there on purpose: a composite
    # action added but not yet committed is exactly the case this is
    # meant to catch, and a tracked-files-only scan cannot see it.
    mapfile -t CANDIDATES < <(git -C "$ACTION_SCAN_ROOT" ls-files -z \
        --cached --others --exclude-standard 2>/dev/null \
        | tr '\0' '\n' | grep -E '(^|/)action\.ya?ml$' \
        | sed "s#^#${ACTION_SCAN_ROOT%/}/#" | sort -u)
else
    # Not a checkout (a release tarball, the self-test's fixtures).
    mapfile -t CANDIDATES < <(find "$ACTION_SCAN_ROOT" -name .git -prune -o \
        -type f \( -name 'action.yml' -o -name 'action.yaml' \) \
        -print 2>/dev/null | sort -u)
fi

for c in "${CANDIDATES[@]}"; do
    [ -n "$c" ] || continue
    # A file discovery already opened is judged, not a blind spot.
    [ "$(dirname "$c")" = "$WORKFLOW_DIR" ] && continue
    refuse "$c is a composite action and discovery does not cover it -- this gate reads $WORKFLOW_DIR only. Its own 'uses:' lines can name a third party at a tag and nothing here would see them. Widen discovery to include it, then delete this refusal."
done

violations=0
seen=0
unparsed=0

for f in "${FILES[@]}"; do
    # PARTIAL VACUITY IS THE SAME DEFECT AS EMPTY VACUITY. The two
    # refusals below close "no files" and "no uses: anywhere"; a file
    # that cannot be READ closes neither. Reading it through a
    # `2>/dev/null || true` grep folded grep's exit 2 into its exit 1, so
    # an unreadable workflow contributed zero references and the gate
    # printed a confident success line over a smaller count that nothing
    # compared to anything -- measured: one `chmod 000` file holding
    # `actions/checkout@v7` and the gate exited 0. One read, one status,
    # one refusal.
    if ! content="$(cat -- "$f")"; then
        refuse "cannot read $f. Its 'uses:' lines would silently count as zero, and a smaller total with a success line is exactly the vacuity this gate exists to refuse."
    fi

    # THE DOMAIN MUST NOT BE A LINE SPELLING. The anchored expression
    # below resolves the two forms this tree uses; it cannot see a flow
    # mapping (`- {uses: a/b@v1}`), a value on the following indented
    # line, whitespace before the colon (`uses : a/b@v1`) or an explicit
    # `? uses` key -- and every one of those is a real action reference:
    # substituting an invalid ref in any of them makes actionlint emit
    # its "ref is missing" diagnostic, identically to the plain form. So
    # a contributor could write any of them, pass this gate, pass
    # actionlint, and run an unreviewed action on the self-hosted pool.
    # FOUR is not the number of such shapes; it is the number found so
    # far, which is why the mechanism below counts rather than enumerates.
    #
    # Chasing spellings loses to the spelling nobody has thought of yet,
    # so this COUNTS instead: every `uses:` standing in a key position is
    # counted, and anything the parser above did not resolve to a ref is
    # residue. Residue refuses. "I found a `uses:` I could not parse" is
    # not a pass, and that is what makes the count independent of which
    # forms anyone remembered -- including whether GitHub's runtime
    # parser and actionlint agree on a form, which stops being a question
    # once an unrecognised form is a refusal.
    #
    # THE BOUND ON THAT CLAIM, AND THE ESCAPE, WRITTEN BESIDE IT. The
    # sentence above holds only for a form that still leaves the token
    # `uses` standing at a key position on SOME LINE. This counter is
    # line-oriented; YAML is not. A `?` indicator alone on its line puts
    # the key on a LATER line, sharing that line with neither a `?` nor
    # a `:`, so nothing here counts it, no residue is produced, and the
    # success line is printed over an unpinned reference. MEASURED on
    # this script: `- ?` / `uses` / `: actions/checkout@v7` beside one
    # pinned reference exits 0, and both oracles read that step as a
    # real action reference -- actionlint answers `invalid format ...
    # ref should not be empty` on the invalid-ref probe, the same oracle
    # every shape here rests on, and PyYAML parses it to a `uses` key.
    # Ten spellings of it were measured; all ten exit 0.
    #
    # WIDENING THE EXPRESSION WOULD MOVE THAT BOUNDARY, NOT REMOVE IT,
    # which is why this states the bound instead of chasing it. A
    # double-quoted key may be split across lines with a `\` escape --
    # `? "us\` then `es"` -- and the token `uses` then occurs NOWHERE in
    # the file. MEASURED: `grep -c uses` returns 1 on a fixture holding
    # TWO references, and both oracles call the second a reference. No
    # line-oriented counter can ever see that one, so calling the class
    # closed would be the same overclaim this paragraph exists to
    # retract. MEASURED as a mutant, too, rather than argued: joining a
    # bare `?` to the line after it turns the simple spelling into a
    # refusal and leaves the split key passing. Closing the class needs
    # a YAML parser, which is a different change with a dependency this
    # repo does not carry. Today's behaviour is PINNED as a case, with
    # the trade written out, in scripts/test-check-action-pins.sh.
    #
    # HOW THE COUNT IS TAKEN, and why it is not a bare `grep uses:`.
    #
    # A `#` INSIDE A QUOTED SCALAR IS NOT A COMMENT, so those are removed
    # first. Stripping comments before this ran was itself a spelling
    # assumption, and in the permissive direction: on
    # `- {name: "release #1", uses: a/b@v7}` the comment expression saw
    # the quoted `#`, deleted the rest of the line, and took the `uses:`
    # with it -- occurrences fell to zero, matched a parsed zero, and the
    # gate printed its success line over a reference it had never seen.
    # MEASURED, exit 0. The step is safe in the one direction that
    # matters: deleting a `#` can only leave MORE text standing, so it
    # can only ever RAISE the count, and a lower bound is allowed to err
    # upward -- an over-count refuses, an under-count passes.
    #
    # Comments go next, so a disabled reference stays not-a-violation --
    # and they are stripped BEFORE flow punctuation is split, or a
    # comment containing a comma would have its tail promoted to a line
    # of its own and counted as a key.
    #
    # Quotes go after that, which does two things at once: a quoted KEY
    # (`"uses":`) is counted, and a `uses:` living inside a quoted VALUE
    # -- `run: echo "uses: x"`, `grep 'uses:' f` -- stops looking like a
    # key. Flow punctuation is then turned into newlines, which puts
    # `- {uses: a}` and `[{uses: a}]` at the head of a line of their own.
    # What is left matches only at a key position, so a step NAME
    # containing the word never counts.
    #
    # THE KEY EXPRESSION MATCHES A KEY, NOT A SPELLING OF ONE. YAML puts
    # no constraint on the space before a `:`, and allows a key to be
    # written explicitly as `? uses` -- the indicator SHARING ITS LINE
    # with the key -- with the value on the following line. (The
    # indicator standing alone on its line is the bound named above, not
    # this arm.) Both are real references -- actionlint answers `ref is
    # missing` on each, byte-identically to the plain form -- and both
    # counted zero here. MEASURED, both exit 0 with a success line.
    # Measured on this tree: 97 parsed, 97 counted, no file differing.
    #
    # The count is a LOWER BOUND on references and is only ever compared
    # upward: fewer counted than parsed is not a finding, more counted
    # than parsed is.
    occurrences="$(printf '%s\n' "$content" \
        | sed -E ':a; s/("[^"]*)#([^"]*")/\1\2/g; ta' \
        | sed -E ":b; s/('[^']*)#([^']*')/\1\2/g; tb" \
        | sed -E 's/(^|[[:space:]])#.*$//' \
        | tr -d '"'"'" \
        | sed -E 's/[{[,]/\n/g' \
        | grep -cE '^[[:space:]]*(-[[:space:]]+)*(uses[[:space:]]*:|\?[[:space:]]+uses[[:space:]]*(:|$))')"
    parsed=0

    while IFS= read -r hit; do
        lineno="${hit%%:*}"
        line="${hit#*:}"
        ref="$(printf '%s' "$line" | sed -E 's/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*//' | awk '{print $1}' | tr -d '"'"'")"
        [ -n "$ref" ] || continue
        seen=$((seen + 1))
        parsed=$((parsed + 1))

        case "$ref" in
            # A local action or reusable workflow is this repository's own
            # tree at this repository's own commit. There is no third
            # party and nothing to pin to.
            #
            # THE BOUNDARY, because the sentence above is true of the
            # REFERENCE and not of the ACTION. A local composite action's
            # own `action.yml` can itself `uses:` a third party at a tag,
            # and discovery never opens it -- this gate reads
            # $WORKFLOW_DIR, not `.github/actions`. The exemption is
            # vacuous today, no `action.yml` existing anywhere in the
            # tree, and it would become a hole the day one did. That is
            # why the composite scan above refuses instead of leaving the
            # limit as a note: the check goes red, rather than a
            # paragraph waiting to be read.
            ./*|.\\*) continue ;;
            docker://*)
                # A DIGEST, NOT A DIGEST-SHAPED STRING. Testing for the
                # `@sha256:` substring alone accepted
                # `docker://alpine@sha256:zz` as pinned -- unpullable, so
                # the outcome is a failed run rather than an unreviewed
                # one, but it is the same pin-versus-pin-shaped
                # distinction the 40-hex rule enforces on the other
                # branch, and a gate that accepts the shape of an answer
                # teaches the wrong thing.
                digest="${ref##*@sha256:}"
                case "$ref" in
                    *@sha256:*)
                        if [[ "$digest" =~ ^[0-9a-f]{64}$ ]]; then
                            continue
                        fi
                        echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                        echo "      '@sha256:$digest' is not a 64-hex digest. It has the shape of a pin and does not name an image." >&2
                        violations=$((violations + 1)) ;;
                    *) echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                       echo "      a docker:// action must be pinned by @sha256: digest, not by tag." >&2
                       violations=$((violations + 1)) ;;
                esac
                continue ;;
        esac

        # owner/repo[/path]@ref -- the ref after the LAST @ must be 40 hex.
        case "$ref" in
            *@*)
                after="${ref##*@}"
                if [[ "$after" =~ ^[0-9a-f]{40}$ ]]; then
                    continue
                fi
                echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                echo "      '@$after' is a tag or branch, not a commit SHA. Whoever controls that repository can repoint it at any commit, and the next run executes it." >&2
                violations=$((violations + 1)) ;;
            *)
                echo "  ${f#"$WORKFLOW_DIR"/}:$lineno  $ref" >&2
                echo "      no '@ref' at all, so this resolves to the action's default branch and changes without warning." >&2
                violations=$((violations + 1)) ;;
        esac
    done < <(printf '%s\n' "$content" \
        | grep -nE '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[^[:space:]]')

    if [ "$occurrences" -gt "$parsed" ]; then
        echo "  ${f#"$WORKFLOW_DIR"/}: $occurrences 'uses:' occurrence(s) present, $parsed resolved to a ref." >&2
        unparsed=$((unparsed + occurrences - parsed))
    fi
done

# THE RESIDUE REFUSES, IT DOES NOT FAIL. An unparsed `uses:` is not a
# proven violation -- it is a reference this gate cannot judge, which is
# the same answer as an empty corpus and gets the same exit code.
[ "$unparsed" -eq 0 ] || refuse "$unparsed 'uses:' occurrence(s) named above were not resolved to an action reference. This gate reads a plain 'uses: <ref>' line, and YAML writes that key in more ways than one -- a flow mapping, a value on the next line, whitespace before the colon and an explicit '? uses' key are four of them, and four is the number found so far, not the number that exist. Each reads as a real reference to GitHub and to actionlint but not to this parser, so it cannot claim they are pinned."

# THE SECOND HALF OF THE NON-VACUITY PREMISE. Files can exist and contain
# no `uses:` at all -- a tree of workflows that only run `run:` steps, or
# a discovery expression that silently matched the wrong thing. "Every
# `uses:` is pinned" is then true over an empty set.
[ "$seen" -gt 0 ] || refuse "found ${#FILES[@]} workflow file(s) under $WORKFLOW_DIR but not one 'uses:' line. Either these are not workflows or the match is wrong; either way nothing was actually checked."

if [ "$violations" -gt 0 ]; then
    echo "::error title=Unpinned action reference::$violations of $seen 'uses:' reference(s) are not pinned to a 40-hex commit SHA. Each is named above with file and line." >&2
    exit 1
fi

echo "action pins: all $seen 'uses:' reference(s) across ${#FILES[@]} workflow file(s) are SHA-pinned."
exit 0
