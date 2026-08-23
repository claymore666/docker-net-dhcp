#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert that no image-publishing job waits on another one (#796).
#
# WHAT #796 CHANGED AND WHY IT NEEDS A GATE. `release-arm64` used to
# carry `needs: release`, so the arm64 build did not start until the
# entire amd64 job had finished, and was SKIPPED outright when amd64
# failed. Its own digest gate therefore never ran, and the first rc of
# a new version could only ever hand back the amd64 block — costing a
# whole extra rc round, every release, each one a separate PR into
# `main` carrying `coverage` on a tree whose Go code had not changed.
#
# The fix was to hoist tag resolution into a `resolve` job and point
# both builds at that instead of at each other. Nothing crosses the
# edge that was removed: the two builds share no artifact, no digest
# and no manifest, only the tag that triggered the run.
#
# THE FAILURE MODE THIS EXISTS FOR IS SILENCE. Re-adding `needs:
# release` breaks nothing. Every job still runs, every check still goes
# green, the release still ships — it just serialises again and the
# arm64 digest block goes back to needing its own rc. The only observer
# would be the next release, noticed by whoever is holding the rc at
# the time, weeks later, under pressure.
#
# ...WHICH IS WHY THE PARSER MUST NOT BE KEYED ON SPELLING. The first
# version of this gate matched `^    needs:` and then deleted brackets
# and whitespace from what followed. That handled the two flow forms
# release.yml happens to use and nothing else, so:
#
#     release-arm64:
#       needs:
#         - resolve
#         - release
#
# — genuinely serialised, confirmed against a YAML loader — was
# reported OK. Block sequence is not an exotic spelling: it is what a
# person reaches for the moment a job gains a second dependency, which
# is EXACTLY the edit that reintroduces this. A gate blind in the
# direction the regression arrives from reproduces the silence it was
# written to end.
#
# So `needs:` is parsed for real, and the accepted grammar is stated
# rather than implied:
#
#     needs: name              needs: [a, b]        needs:
#     needs: "name"            needs: [a, "b",]       - a
#     needs: 'name'            needs: []              - "b"
#
# with an unquoted trailing `# comment` allowed after any of them.
#
# ANYTHING ELSE IS EXIT 2, NOT OK. A form this cannot parse is a form
# whose meaning it does not know, and reporting "no publishing job
# waits on another" about text it did not understand is the same
# failure one level up. Refusing is the shape the rest of the tree
# uses; it fails loudly on the day someone writes a multi-line flow
# sequence, which is the day to teach this parser about it.
#
# KEYED ON THE PROPERTY, NOT THE NAMES. A publishing job is one that
# runs `make` with a `push` target — that is what makes a job a per-arch
# build rather than what it happens to be called. A future
# `release-riscv64` is covered the day it is added, without this file
# being edited, and renaming `release-arm64` does not silently empty
# the rule.
#
# AND THE SAME REFUSAL APPLIES HERE, FOR A REASON THAT WAS ARGUED WRONG
# ONCE. This detector used to be the regex
#
#     make[[:space:]].*[[:space:]]push([[:space:]]|$)
#
# which is line-oriented and demands whitespace immediately before
# `push`, so it misses `make push PLUGIN_NAME=...` and anything split
# over a line continuation. The defence offered for that was: a missed
# publisher drops the count below two and the non-vacuity refusal
# fires, so a miss cannot become a clean pass.
#
# THAT ARGUMENT IS UNSOUND, AND NOT SUBTLY. The refusal's domain is
# computed by the very detector it is supposed to backstop. A missed
# job does not merely go unchecked — it LEAVES THE POPULATION, so it is
# absent from the serialisation check and from the count in the same
# stroke. On a two-publisher file the count happens to fall to one and
# it looks like the argument held. Add a third architecture spelled in
# a way the regex does match, leave the serialised one spelled in a way
# it does not, and the count is two again, the refusal never fires, and
# a genuinely serialised file reports:
#
#     OK: 2 publishing job(s) ... none waiting on another
#
# A measurement cannot backstop itself. So the classifier does not get
# to answer "not a publisher" as a way of saying "I could not tell":
# it joins line continuations, finds every `make` invocation, and when
# it cannot decide whether one publishes — a target that is a variable
# expansion, or no target at all, where the answer lives in the
# Makefile's default goal — it REFUSES with exit 2 and names the line.
# Refusing is loud and cheap. Silently dropping a job from the
# population is the failure this whole file exists to prevent, applied
# to itself.
#
# WHAT IT DOES NOT CLAIM. It reads the workflow text. It cannot know
# whether the runners exist, whether the jobs really start together, or
# whether a push succeeds. It answers one question — "does any
# publishing job wait, directly or transitively, on another" — which is
# the question that was answered wrong.
#
# AND ITS SUBJECT IS BOUNDED, DELIBERATELY. A publishing job here is a
# job that runs `make` with a target literally named `push`. Two things
# are therefore INVISIBLE to this gate, and that is a stated scope, not
# an oversight:
#
#   - a job that publishes without calling make at all (a bare `docker
#     push`, `crane copy`, a registry action);
#   - a make target that publishes under another name, e.g.
#     `make push-arm64`.
#
# Both are decidably outside the subject, so they are answered "not a
# publisher" rather than refused — and a file where every publisher is
# invisible falls below two and lands on the non-vacuity refusal
# anyway. Widening the subject is v1.9.0 work; the point of writing the
# bound down is that the next person reads it here instead of assuming
# a coverage this file never claimed.
#
# Usage: check-build-job-independence.sh [workflow-file]
# Exit:  0 no publishing job depends on another
#        1 one does — the serialised shape is back
#        2 the check cannot render a verdict (unreadable file, a
#          `needs:` form it cannot parse, a `make` invocation it cannot
#          classify, or fewer than two publishing jobs, which would
#          make the rule vacuous)
set -uo pipefail

WF="${1:-.github/workflows/release.yml}"

if [ ! -f "$WF" ] || [ ! -r "$WF" ]; then
    echo "::error title=Nothing to inspect::$WF is not a readable file." \
         "The rule would otherwise pass having examined nothing." >&2
    exit 2
fi

# Emits `job<TAB>publishes<TAB>needs,needs,...` per job, or a BAD line
# naming a `needs:` spelling it will not guess at.
parsed="$(awk '
    # Cut an unquoted trailing comment. Tracks quote state so a `#`
    # inside "a#b" survives; anything else after whitespace is prose.
    function decomment(s,   i, c, q, out) {
        q = ""
        for (i = 1; i <= length(s); i++) {
            c = substr(s, i, 1)
            if (q != "") { if (c == q) q = ""; out = out c; continue }
            if (c == "\"" || c == "\x27") { q = c; out = out c; continue }
            if (c == "#" && (i == 1 || substr(s, i - 1, 1) ~ /[[:space:]]/)) break
            out = out c
        }
        return out
    }
    function trim(s) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", s); return s }
    function unquote(s) {
        s = trim(s)
        if (s ~ /^".*"$/ || s ~ /^\x27.*\x27$/) s = substr(s, 2, length(s) - 2)
        return s
    }
    function isname(s) { return s ~ /^[A-Za-z0-9_-]+$/ }
    function bad(what, line) { printf "BAD\t%d\t%s\n", line, what; broke = 1 }

    function addneed(t) { needs = (needs == "" ? t : needs "," t) }

    # A flow value: bare/quoted scalar, or [ ... ]. Returns 0 on a form
    # it does not recognise, and the caller refuses a verdict.
    function parse_flow(s,   inner, m, parts, i, t) {
        s = trim(decomment(s))
        if (s == "") return -1                      # block sequence follows
        if (s ~ /^\[/) {
            if (s !~ /\]$/) return 0                # multi-line flow: unhandled
            inner = trim(substr(s, 2, length(s) - 2))
            if (inner == "") return 1               # needs: []  -- no deps
            m = split(inner, parts, ",")
            for (i = 1; i <= m; i++) {
                t = unquote(parts[i])
                if (t == "") continue               # tolerate a trailing comma
                if (!isname(t)) return 0
                addneed(t)
            }
            return 1
        }
        t = unquote(s)
        if (!isname(t)) return 0
        addneed(t)
        return 1
    }

    # Split a line into shell command segments. This is deliberately
    # not a shell parser: splitting inside a quoted string manufactures
    # segments, and until the command position gained a verdict of its
    # own those segments were harmlessly ignored. They are not ignored
    # any more, so the two places a manufactured segment can now lie
    # are handled where they arise -- `echo`/`printf` carry data rather
    # than commands (classify_make), and only the body of a `run:` is
    # read at all (the main rules below).
    function classify_line(s, fnr,   n, i, segs, v) {
        gsub(/&&/, "\x01", s); gsub(/\|\|/, "\x01", s)
        gsub(/;/,  "\x01", s); gsub(/\|/,  "\x01", s)
        n = split(s, segs, "\x01")
        for (i = 1; i <= n; i++) {
            v = classify_make(segs[i])
            if (v == "publishes") pub = 1
            else if (v == "undecided")
                bad("cannot tell whether this `make` publishes -> " trim(segs[i]), fnr)
            else if (v == "indirect")
                bad("a `make` token that is not a literal `make` invocation -> " trim(segs[i]), fnr)
        }
    }

    # A make invocation publishes when `push` is among its TARGETS.
    # Targets are what is left after flags and VAR=value assignments,
    # so argument order does not matter -- `make push VAR=x` and
    # `make VAR=x push` are the same command, and the regex this
    # replaced recognised only the second.
    function classify_make(seg,   toks, n, i, t, sawpush, undec, sawtarget) {
        sub(/^[[:space:]]*/, "", seg)
        sub(/^-[[:space:]]+/, "", seg)          # a YAML sequence dash
        sub(/^[[:space:]]*/, "", seg)
        # `echo` and `printf` do not execute their arguments, so a
        # `make` token inside one is prose. test.yaml:834 is exactly
        # this -- `echo "make create created ..."` in the failure
        # message of another gate -- and the command-position rule below called
        # it indirect. A command substitution inside the arguments DOES
        # execute, so that exit is closed off when one is present.
        if (seg ~ /^(echo|printf)([[:space:]]|$)/ &&
            index(seg, "$(") == 0 && index(seg, "`") == 0)
            return "none"
        # THE COMMAND POSITION GETS THE SAME RULE AS THE TARGET
        # POSITION. Refusing on an unreadable target while answering
        # "none" here would leave the last silent exit in the file: a
        # segment holding a `make` token that this cannot read is
        # undecidable by exactly the standard applied below, and
        # "none" removes its job from the population. `${MAKE} push`
        # and `sh -c "make push"` both took that exit.
        #
        # The word test is case-insensitive and bounded by non-letters,
        # so `${MAKE}` and `$MAKE` are caught while `Makefile` and
        # `MAKEFLAGS` are not.
        #
        # It DOES fire on real text elsewhere in the tree, which was
        # worth measuring rather than asserting: run this over every
        # workflow and `integration-hosted.yml` refuses on
        # `sudo env "PATH=$PATH" ... make integration-test`. That is a
        # true positive by the standard this gate sets itself -- a make
        # reached through a wrapper, whose targets it cannot claim to
        # have read -- and the file is outside the subject anyway,
        # having no publishing job at all. The two refusals that were
        # NOT true positives (a step `name:` holding the word "Make",
        # and a `make` inside an `echo`) are closed above and below.
        if (seg !~ /^make([[:space:]]|$)/) {
            if (seg ~ /(^|[^A-Za-z])[Mm][Aa][Kk][Ee]([^A-Za-z]|$)/)
                return "indirect"
            return "none"
        }
        sub(/^make[[:space:]]*/, "", seg)
        n = split(seg, toks, /[[:space:]]+/)
        sawpush = 0; undec = 0; sawtarget = 0
        for (i = 1; i <= n; i++) {
            t = toks[i]
            if (t == "") continue
            if (t ~ /^-/) continue                          # a flag
            if (t ~ /^[A-Za-z_][A-Za-z0-9_]*=/) continue    # VAR=value
            sawtarget = 1
            if (t == "push") { sawpush = 1; continue }
            if (t ~ /[$`]/) undec = 1     # expands to an unknown target
        }
        if (sawpush) return "publishes"
        if (undec) return "undecided"
        if (!sawtarget) return "undecided"   # the default goal lives in the Makefile
        return "no"
    }

    function flush() { if (job != "") printf "%s\t%s\t%s\n", job, pub, needs }

    # Shell text enters here and nowhere else. `\`-continuations are
    # joined first: a `make` invocation split over two lines is one
    # command, and a line-oriented reader sees neither half as a
    # publisher.
    function feed(line, fnr) {
        if (line ~ /^[[:space:]]*#/ && cont == "") return   # a shell comment
        if (cont != "") { line = cont " " line; cont = "" }
        if (line ~ /\\[[:space:]]*$/) {
            sub(/\\[[:space:]]*$/, "", line)
            cont = line
            return
        }
        classify_line(line, fnr)
    }

    # -1 is the sentinel for "not inside a block scalar", and an unset
    # awk variable is 0, which compares >= 0 and would put the reader
    # in a block from the first line of the file.
    BEGIN { runind = -1 }

    /^jobs:[[:space:]]*$/ { injobs = 1; next }
    !injobs { next }
    /^[^[:space:]#]/ { flush(); injobs = 0; runind = -1; next }

    # INSIDE A BLOCK SCALAR, EVERYTHING DEEPER IS SHELL. Checked before
    # the structural rules so that a body line is never mistaken for
    # one; a line at or left of the `run:` key ends the block, and falls
    # through to be read as YAML again.
    runind >= 0 {
        if ($0 ~ /^[[:space:]]*$/) next
        if (match($0, /[^[:space:]]/) - 1 > runind) { feed($0, FNR); next }
        runind = -1
        # fall through
    }

    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
        flush()
        job = $1; sub(/:$/, "", job)
        pub = 0; needs = ""; inneeds = 0; runind = -1
        next
    }

    # Comments never carry behaviour. This file is heavily commented,
    # including prose that quotes `needs: release` while explaining this
    # very rule, so counting one would make the gate fire on its own
    # documentation.
    /^[[:space:]]*#/ { next }

    /^    needs:/ {
        rest = $0
        sub(/^    needs:/, "", rest)
        r = parse_flow(rest)
        if (r == 0) { bad("unparseable needs: value -> " trim(rest), FNR); inneeds = 0; next }
        inneeds = (r == -1) ? 1 : 0
        next
    }

    inneeds {
        if ($0 ~ /^[[:space:]]*$/) next
        if ($0 ~ /^      -[[:space:]]/) {
            item = $0
            sub(/^      -[[:space:]]*/, "", item)
            item = unquote(trim(decomment(item)))
            if (!isname(item)) { bad("unparseable needs: item -> " trim($0), FNR); next }
            addneed(item)
            next
        }
        # Indented deeper than a job key and not an item: this is not a
        # shape we know. Ending the list silently here is how the first
        # version lost entries.
        if ($0 ~ /^      /) { bad("unexpected line inside a needs: block -> " trim($0), FNR); next }
        inneeds = 0
        # fall through: it is an ordinary line of this job
    }

    # A `run:` VALUE IS THE ONLY SHELL IN A WORKFLOW. The first version
    # of the command-position rule read every line of every job, so a
    # step called `- name: Make gh-pages available to mike`
    # (pages.yml:162) was reported as a `make` token that could not be
    # read -- a refusal, on prose, in a file with no publishing job in
    # it. A gate that refuses over the name of a step gets discharged
    # as noisy long before it catches the edit it exists for.
    #
    # Both spellings of the key are accepted, `run:` and `- run:`. A
    # block scalar ends at the column of the `run:` KEY rather than at
    # the dash, because that is the column YAML itself measures the
    # scalar against.
    #
    # THE TWO COLUMNS DIFFER EXACTLY WHEN A SIBLING KEY FOLLOWS THE
    # BLOCK. A step is a mapping, so `name:`, `env:`, `if:`, `shell:`
    # and `working-directory:` all sit at the same column as `run:`
    # and any of them may be written after it:
    #
    #     - run: |
    #         make PLUGIN_TAG=x push
    #       name: publish via ${MAKE} on the builder
    #
    # The key column ends the block at `name:`. The dash column, being
    # shallower, keeps it inside and hands the name of the step itself to the
    # classifier -- which refuses it, because it holds a `make` token
    # it cannot read. Measured on exactly that document: shipped exit
    # 1 (serialised, correct), dash column exit 2 (Cannot classify).
    #
    # An earlier version of this comment claimed the two were
    # indistinguishable by any legal document. That was false, and it
    # was the second false universal on this branch. The case that
    # refutes it is now in the suite, so this paragraph is described
    # by something that goes red rather than being trusted.
    /^[[:space:]]*(-[[:space:]]+)?run:([[:space:]]|$)/ {
        ind = index($0, "run:") - 1
        rest = $0
        sub(/^[[:space:]]*(-[[:space:]]+)?run:[[:space:]]*/, "", rest)
        if (rest ~ /^[|>]/) { runind = ind; next }   # a block scalar
        runind = -1
        feed(rest, FNR)
        next
    }

    END {
        if (cont != "") classify_line(cont, FNR)
        flush()
        if (broke) exit 3
    }
' "$WF")"
awkrc=$?

if [ "$awkrc" -eq 3 ] || printf '%s\n' "$parsed" | grep '^BAD	' >/dev/null; then
    echo "::error title=Cannot classify::this gate will not guess at a" \
         "\`needs:\` spelling or a \`make\` invocation it does not" \
         "understand, because reporting OK about text it could not read is" \
         "the silence it exists to end (#796)." >&2
    printf '%s\n' "$parsed" | awk -F'\t' '$1 == "BAD" { printf "  %s:%s: %s\n", wf, $2, $3 }' wf="$WF" >&2
    echo >&2
    echo "  Accepted needs:  'needs: name', 'needs: [a, b]' (items may be" >&2
    echo "  quoted), and a block sequence of '      - name' items." >&2
    echo "  Accepted make:   an invocation whose targets are literal words," >&2
    echo "  so 'push' can be found among them whatever the argument order." >&2
    echo "  Teach the parser the new form -- do not reword the workflow to" >&2
    echo "  suit the gate, and do not let it answer 'not a publisher' when" >&2
    echo "  what it means is 'I could not tell'." >&2
    exit 2
fi

publishers="$(printf '%s\n' "$parsed" | awk -F'\t' '$2 == 1 { print $1 }')"
npub="$(printf '%s' "$publishers" | grep -c . || true)"

# NON-VACUITY. "No publishing job depends on another" is satisfied for
# free by a file with one publishing job, or none. A rule that passes
# by having an empty domain is the failure this tree has hit before, so
# it refuses a verdict instead -- and this doubles as the backstop for
# the `make ... push` regex: a publisher that stops being recognised
# lands here rather than in a clean pass.
if [ "$npub" -lt 2 ]; then
    echo "::error title=Nothing to compare::$WF has $npub job(s) running" \
         "\`make ... push\`; the rule needs at least two to mean anything." \
         "Either the per-arch builds were removed or the step that" \
         "identifies them was rewritten -- re-derive this check." >&2
    exit 2
fi

verdict="$(printf '%s\n' "$parsed" | awk -F'\t' -v pubs="$publishers" '
    BEGIN { n = split(pubs, p, "\n"); for (i = 1; i <= n; i++) if (p[i] != "") ispub[p[i]] = 1 }
    $1 != "BAD" { needs[$1] = $3 }
    function reaches(from, target,   i, m, parts) {
        if (from in visiting) return 0
        visiting[from] = 1
        m = split(needs[from], parts, ",")
        for (i = 1; i <= m; i++) {
            if (parts[i] == "") continue
            if (parts[i] == target) { delete visiting[from]; return 1 }
            if (reaches(parts[i], target)) { delete visiting[from]; return 1 }
        }
        delete visiting[from]
        return 0
    }
    END {
        for (a in ispub) for (b in ispub) {
            if (a == b) continue
            delete visiting
            if (reaches(a, b)) printf "%s\t%s\n", a, b
        }
    }
')"

if [ -n "$verdict" ]; then
    echo "::error title=Publishing jobs are serialised::a job that publishes" \
         "images waits on another one, so the second architecture cannot" \
         "start until the first finishes and is skipped when it fails (#796)." >&2
    printf '%s\n' "$verdict" | while IFS=$'\t' read -r a b; do
        [ -n "$a" ] || continue
        echo "  $a reaches $b through needs:" >&2
    done
    echo >&2
    echo "  Both builds derive everything they need from the tag that" >&2
    echo "  triggered the run. Depend on the job that resolves it, not on" >&2
    echo "  each other. The contract that must stay is downstream:" >&2
    echo "  promote-latest and github-release name BOTH builds, so no" >&2
    echo "  floating tag moves unless both succeeded." >&2
    exit 1
fi

echo "OK: $npub publishing job(s) in $WF, none waiting on another:" \
     "$(printf '%s' "$publishers" | tr '\n' ' ')"
