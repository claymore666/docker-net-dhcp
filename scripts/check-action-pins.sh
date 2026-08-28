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
# this tree holds both -- overwhelmingly one of them, and at least one of
# the other, which is the whole hazard. A gate matching only the common
# extension would pass over the odd file forever, which is the defect
# class this whole family exists to stop.
#
# NO COUNT IS WRITTEN HERE, and one used to be: "24 of one and 1 of the
# other". MEASURED 2026-08-28: 25 and 1. The success line printed by
# this very script said 26 files while this comment said 25, so the file
# disagreed with its own output. Ask the tally line, which counts on
# every run.
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
    #
    # NO COUNT OF SUCH SHAPES IS WRITTEN HERE. This sentence used to say
    # "FOUR is not the number of such shapes; it is the number found so
    # far" -- and the number found so far then grew twice, to a `?`
    # indicator standing alone on its line and to a YAML node property
    # sitting between the dash and the key, while this line did not
    # move. A count of an OPEN SET is a stale number waiting to happen,
    # which is the defect this gate exists to stop. The list above is
    # examples; the mechanism below COUNTS rather than enumerates,
    # precisely so that the size of the list does not matter.
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
    # THE BOUND ON THAT CLAIM, AND THE ESCAPE, WRITTEN BESIDE IT.
    #
    # THIS PARAGRAPH ONCE STATED THAT BOUND WRONGLY, AND THE WRONG
    # VERSION SHIPPED INSIDE THIS PR. It said the counter sees any form
    # that "still leaves the token `uses` standing at a key position on
    # SOME LINE". False as written: a YAML NODE PROPERTY may sit between
    # the block-sequence dash and the mapping's first key --
    # `- &a uses: actions/checkout@v7`, `- !!map uses: ...` -- which
    # leaves `uses` at a key position on that very line and escaped the
    # counter anyway, because the expression admitted only DASHES ahead
    # of the key. MEASURED at the time: exit 0, success line, no
    # residue, over an unpinned reference. It is FIXED above rather than
    # documented -- the counter, the parser feed and the ref extraction
    # all admit a node property now, in lockstep, so those forms are
    # judged and the unpinned ones go red naming file and line. A form
    # this gate PASSES must not depend on GitHub's parser being stricter
    # than this one.
    #
    # WHAT MAY SIT THERE, ENUMERATED, AND WHAT THE ENUMERATION MISSES.
    # YAML allows an anchor (`&name`) and a tag before a node, in either
    # order and in any number; `!`, `!local`, `!!std`, `!<verbatim>` and
    # `!handle!suffix` are all tags. The expression admits any
    # whitespace-free token beginning `&` or `!`, which covers all of
    # them without enumerating them. That rests on ONE assumption,
    # written here so it can be attacked rather than left implicit: a
    # node property never contains whitespace, so a single token is
    # always enough.
    #
    # NO TALLY OF SPELLINGS IS WRITTEN HERE, AND THE ONE THAT USED TO BE
    # WAS FALSE. It said "the three that neither oracle called a
    # reference refuse rather than pass". MEASURED 2026-08-28 in a
    # workflow directory holding one ordinary pinned reference beside
    # them: `- !uses: x`, `&a - uses: x` and `- *a` each exit 0, not 2.
    # They refuse only when they are ALONE in the directory, and then
    # for a different reason -- the non-vacuity guard fires because the
    # scan found no `uses:` line at all, which is a fact about the
    # fixture and not a judgement of the form. The table below already
    # said OPEN for all three, so this file contradicted itself two
    # paragraphs apart and did so in the direction that harms the
    # reader: claiming more strictness than exists.
    #
    # A tally of spellings is also the shape this file refuses twice
    # above -- a count of an open set, with no observer. The POSITIONS
    # are what is enumerated, every row is measured, and every row has a
    # case in scripts/test-check-action-pins.sh. Ask those, not this
    # paragraph.
    #
    # THE ENUMERATION IS OF POSITIONS, NOT OF SPELLINGS, AND THAT IS THE
    # CORRECTION THIS PARAGRAPH CARRIES. It used to name three shapes as
    # outside the expression and read as though everything else were
    # inside it. A fourth position then escaped: a property BETWEEN the
    # `?` indicator and the key -- `- ? &a uses` -- because the property
    # group sat ahead of the ALTERNATION, so it reached a property in
    # front of `uses:` and one in front of `?`, and not one in front of
    # the key inside the `?` arm. MEASURED at the time: exit 0 with the
    # success line over an unpinned reference that PyYAML composes to a
    # real `uses` key. Enumerating spellings is what missed it; the
    # positions are finite and the spellings are not, so the positions
    # are what is written down. Every row is MEASURED and every one has
    # a case in scripts/test-check-action-pins.sh.
    #
    #   before the dash          `&a - uses: x`      OPEN, not a ref
    #   after the dash           `- &a uses: x`      judged  (exit 1)
    #   dashless                 `  &a uses: x`      judged  (exit 1)
    #   own line, key below      `- &a` / `uses:`    judged  (exit 1)
    #   before the `?`           `- &a ? uses`       refused (exit 2)
    #   between `?` and key      `- ? &a uses`       refused (exit 2)
    #   both sides of the `?`    `- &a ? !x uses`    refused (exit 2)
    #   in a flow mapping        `- {? &a uses : x}` refused (exit 2)
    #   after the key            `uses &a : x`       not a `uses` key
    #   on the value side        `uses: &a ref`      OPEN, both ways
    #   `?` alone on its line    `- ?` / `&a uses`   OPEN, unreachable
    #
    # THE OPEN ROWS, each with what it costs. A property before the dash
    # and a tag shorthand that swallows the key (`- !uses: x`) exit 0
    # and NEITHER ORACLE CALLS EITHER A REFERENCE, so passing them costs
    # nothing observable -- one spelling of each was measured, which is
    # a bound and not a proof. An alias (`*a`) is not a node property at
    # all and is caught at its DEFINITION site, which is where the
    # literal `uses:` line has to be written; both the plain alias and
    # the `<<:` merge key are driven as cases. The bound on that:
    # "caught at its definition site" holds for a definition site this
    # gate judges. The one open definition site is `&a - uses:`, before
    # the dash -- and MEASURED 2026-08-28, a document that anchors there
    # and aliases it is not valid YAML at all (PyYAML: "sequence entries
    # are not allowed here"), so there is no workflow in which that
    # combination is a reference this gate missed. The value side is judged
    # rather than skipped, which is worse than either: it false-REDS a
    # pinned ref, and for a tag ending in `@` plus 40 hex it false-GREENS
    # an unpinned one. Both are pinned as cases. The `?`-alone row is the
    # structural bound named further up -- no expression here can reach a
    # key line carrying neither indicator nor colon, whatever the property
    # group admits.
    #
    # THE DASH QUANTIFIER DIVERGES ACROSS THE SITES, DELIBERATELY, and
    # it is recorded here because it is the one place left in this gate
    # keyed on a spelling rather than on a property. The counter admits
    # `(-[[:space:]]+)*` -- zero or MORE dashes -- while the ref
    # extraction and the parser feed admit `(-[[:space:]]+)?`, zero or
    # one. So a nested block sequence (`- - uses: x`) is counted and not
    # extracted, occurrences exceed parsed, and the residue check
    # REFUSES: fail-closed, MEASURED 2026-08-28 in both directions, and
    # a real false red for a workflow that writes its steps that way.
    # Unifying them would be worse than the false red -- the extraction
    # cannot attribute a nested entry to a step, so it would claim to
    # judge a shape it does not understand. The self-test's
    # property-group consistency check compares only the property group
    # and CANNOT see this divergence, so both directions are pinned as
    # cases instead.
    #
    # AND THE ROWS ARE A LOWER BOUND ON THE POSITIONS, not a proof that
    # there are eleven. Two spellings enumerated means a third exists,
    # and this table exists because that was true of the last list.
    #
    # The alias deserves its own sentence because it looks like the
    # dangerous one and MEASURED it is not: to alias a step you must
    # first WRITE that step, and the anchor's definition site carries
    # the literal `uses:` line, which this gate reads. Driven both ways
    # -- `- *s` reusing an anchored unpinned step, and a `<<: *s` merge
    # key pulling one in -- the gate answers exit 1 on each, naming the
    # definition site. An alias cannot hide a reference; it can only
    # repeat one already judged.
    #
    # Two spellings enumerated means a third exists, so that list is a
    # lower bound on the class exactly as the count below is a lower
    # bound on the references.
    #
    # THE ESCAPE THAT REMAINS is the one this counter cannot close: it
    # is line-oriented and YAML is not. A `?` indicator alone on its
    # line puts the key on a LATER line, sharing that line with neither
    # a `?` nor a `:`, so nothing here counts it, no residue is
    # produced, and the success line is printed over an unpinned
    # reference. MEASURED on this script: `- ?` / `uses` /
    # `: actions/checkout@v7` beside one pinned reference exits 0, and
    # both oracles read that step as a real action reference --
    # actionlint answers `invalid format ... ref should not be empty` on
    # the invalid-ref probe, and PyYAML parses it to a `uses` key. Ten
    # spellings of it were measured; all ten exit 0.
    #
    # WIDENING MOVES THAT BOUNDARY, IT DOES NOT REMOVE IT -- and that is
    # now MEASURED rather than predicted, because the widening above is
    # the move this paragraph used to decline. A double-quoted key may
    # be split across lines with a `\` escape -- `? "us\` then `es"` --
    # and the token `uses` then occurs NOWHERE in the file. MEASURED:
    # `grep -c uses` returns 1 on a fixture holding TWO references, and
    # both oracles call the second a reference. No line-oriented counter
    # can ever see that one, so calling the class closed would be the
    # same overclaim this paragraph exists to retract.
    #
    # THE PRICE OF THE WIDENING, MEASURED, not asserted to be zero. This
    # gate has an existing false RED -- a bare `uses:` at the start of a
    # line inside a `run: |` block scalar -- and it now extends to a
    # line whose FIRST token is a `!` or `&` word followed by `uses:`.
    # Four such shapes were measured moving from pass to red;
    # `cmd & uses: x` and `grep uses: f` do NOT move, because the
    # property has to be the first token. That is the false-red class
    # already pinned in the suite, made slightly wider, in the direction
    # that fails loudly. Against the real tree the two runs are
    # byte-identical on both streams.
    #
    # Closing the class needs a YAML PARSER, and that route is open --
    # which it was not when this paragraph was first drafted. #844
    # landed check-fork-execution-policy.sh, which parses the workflow
    # tree with PyYAML and refuses without it, and test.yaml installs
    # PyYAML in the SAME job that runs this gate. So the remaining cost
    # is not a new dependency: it is a rewrite of this gate's judging
    # mechanism, plus a refusal wherever PyYAML is absent -- and
    # scripts/local-lane.sh runs this gate while carrying no
    # PyYAML-dependent gate today. Still a separate change; no longer a
    # blocked one. Today's behaviour is PINNED as cases, with the trade
    # written out, in scripts/test-check-action-pins.sh.
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
    #
    # NO COUNT IS WRITTEN DOWN HERE, DELIBERATELY. This line used to
    # read "Measured on this tree: 97 parsed, 97 counted" -- and the
    # tree moved to 98 without it, which is precisely the stale-number
    # defect this gate exists to stop, sitting inside the gate. A
    # population does not belong in a comment: it is re-derived by the
    # suite's real-tree case on every run, and the property that
    # matters is not the size of the two numbers but their RELATION,
    # stated just below and enforced by the residue refusal rather than
    # by anyone reading this.
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
        | grep -cE '^[[:space:]]*(-[[:space:]]+)*([&!][^[:space:]]*[[:space:]]+)*(uses[[:space:]]*:|\?[[:space:]]+([&!][^[:space:]]*[[:space:]]+)*uses[[:space:]]*(:|$))')"
    parsed=0

    while IFS= read -r hit; do
        lineno="${hit%%:*}"
        line="${hit#*:}"
        # A node property is stripped ahead of the KEY, not ahead of
        # the VALUE. `uses: &a actions/checkout@<40 hex>` therefore
        # extracts `&a` and reports a pinned reference as unpinned --
        # MEASURED, and identical before this expression was widened,
        # so it is pre-existing rather than introduced here. It is
        # pinned as a case in the suite with the trade written out.
        #
        # THE DIRECTION IT FAILS IN IS A BOUND, NOT A GUARANTEE, and
        # this sentence used to state the guarantee. It said the defect
        # "cries unpinned over something pinned, never the reverse".
        # THE ESCAPE, MEASURED: `uses: !a@<40 hex> actions/checkout@v7`
        # exits 0 with the success line, because `awk '{print $1}'`
        # returns the property token and the ref rule below reads ITS
        # tail after the first `@` as 40 hex, so the property is judged
        # and passes while the real reference is never looked at. Both
        # spellings are pinned as cases. The bound on the exposure --
        # also measured, and stated rather than left implied -- is that
        # no ordinary anchor or tag name ends in `@` plus 40 hex, so
        # reaching this needs an adversary, and an adversary already has
        # the `?`-alone class above; it adds no reach that is not already
        # disclosed. The first-@ split narrows it further: the property
        # token must now carry exactly ONE `@`, where the last-@ rule
        # accepted any number of them.
        #
        # Widening THIS sed is the one place a mistake yields a wrong
        # REF rather than a wrong count, so it belongs in a change that
        # carries its own mutants, beside the value-side alias
        # (`uses: *a`).
        ref="$(printf '%s' "$line" | sed -E 's/^[[:space:]]*(-[[:space:]]+)?([&!][^[:space:]]*[[:space:]]+)*uses:[[:space:]]*//' | awk '{print $1}' | tr -d '"'"'")"
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
                #
                # THE SAME FIRST-VERSUS-LAST SPLIT as the ref branch
                # below, found by measuring the neighbour of that fix
                # rather than only the spot it was reported at. Taking the
                # LAST `@sha256:` read a well-formed digest out of
                # `docker://alpine@sha256:zz@sha256:<64 hex>` and passed
                # it. Taking the FIRST makes the whole tail the digest, so
                # it fails. MEASURED that neither ordering is executable --
                # `docker pull` answers `invalid reference format` on both
                # -- so the exposure was a failed run rather than an
                # unreviewed one. That is the same standing as the
                # two-character digest above, and the same reason to refuse
                # it rather than print a pin.
                digest="${ref#*@sha256:}"
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

        # owner/repo[/path]@ref -- the ref is everything after the FIRST
        # @, and it must be 40 hex.
        #
        # IT SPLIT AT THE LAST `@` UNTIL A REVIEWER DROVE IT, which made
        # this gate LOOSER than the parser it models -- the one direction
        # it may not be wrong in, and the outcome it exists to refuse.
        # `evil/action@v7@<40 hex>` is an ordinary single-line `uses:` in
        # the ordinary position, so it defeats a human reader too; under a
        # last-@ split its tail is 40 hex and the gate printed
        # `action pins: all N ... are SHA-pinned` over it.
        #
        # THE SPLIT IS MEASURED, NOT PREFERRED. actionlint v1.7.12 ACCEPTS
        # `actions/checkout@v7@` while rejecting `actions/checkout@` with
        # `owner and repo and ref should not be empty` -- so the ref of the
        # first is `v7@`, non-empty, which only a first-@ split produces.
        # `actions/checkout@@v2` is accepted too, ref `@v2`. And
        # `git check-ref-format refs/tags/v7@<40 hex>` ACCEPTS, so the
        # mutable tag such a reference names is creatable rather than
        # theoretical.
        #
        # actionlint is ONE implementation and not GitHub's runtime parser:
        # the first-@ reading is MEASURED there and INFERRED for GitHub. It
        # is used here only to make this gate STRICTER, never to excuse a
        # pass -- the contract being that a form this gate passes must not
        # depend on GitHub's parser being stricter than this one.
        #
        # No reference in this tree carries two `@`, so this was a
        # completeness defect rather than a shipped hole -- and that is
        # exactly why no test constrained it: at one `@` the two splits
        # agree, so a mutant flipping them killed nothing. The suite drives
        # the disagreement now.
        case "$ref" in
            *@*)
                after="${ref#*@}"
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
        | grep -nE '^[[:space:]]*(-[[:space:]]+)?([&!][^[:space:]]*[[:space:]]+)*uses:[[:space:]]*[^[:space:]]')

    if [ "$occurrences" -gt "$parsed" ]; then
        echo "  ${f#"$WORKFLOW_DIR"/}: $occurrences 'uses:' occurrence(s) present, $parsed resolved to a ref." >&2
        unparsed=$((unparsed + occurrences - parsed))
    fi
done

# THE RESIDUE REFUSES, IT DOES NOT FAIL. An unparsed `uses:` is not a
# proven violation -- it is a reference this gate cannot judge, which is
# the same answer as an empty corpus and gets the same exit code.
[ "$unparsed" -eq 0 ] || refuse "$unparsed 'uses:' occurrence(s) named above were not resolved to an action reference. This gate reads a plain 'uses: <ref>' line, and YAML writes that key in more ways than one -- a flow mapping, a value on the next line, whitespace before the colon and an explicit '? uses' key are among them. That list is examples, not an inventory -- it has grown twice since it was written. Each reads as a real reference to GitHub and to actionlint but not to this parser, so it cannot claim they are pinned."

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
