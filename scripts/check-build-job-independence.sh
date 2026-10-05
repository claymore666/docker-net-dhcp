#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert that no image-publishing job waits on another one (#796).
#
# Expires-when: both architectures publish from one job or one
#   multi-platform build, so no publishing job can wait on another (#796).
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
# THE SUBJECT IS A JOB THAT UPLOADS IMAGE BYTES (#798). A registry command in
# a `run:` body is BYTES when local content becomes an image (make `push`;
# docker/image/plugin/compose push; a build with a push output; crane
# push/append/mutate/rebase/flatten; oras push, blob/manifest push, cp from a
# local layout; cosign upload/load), REFERENCE when it writes a name, signature
# or attachment over a digest already there (crane tag/copy, imagetools create,
# manifest push, oras cp/tag/attach, cosign sign/attest/attach), else NONE. One
# BYTES step makes a publisher. REFERENCE jobs stay out on purpose:
# promote-latest and github-release must wait on both builds (#796).
#
# NOTHING UNCLASSIFIED ANSWERS "NOT A PUBLISHER" (#798). Exit 2 for: a verb
# in no table; podman, buildah, skopeo, regctl, nerdctl, ko; a tool word off
# command position (sudo, sh -c); a variable or substitution at it; a flag
# before the verb; a make target naming push/publish other than `push`; a
# `uses:` outside the action table; a `shell:` other than bash or sh. Bound,
# not seen: a script or make recipe the workflow calls; a tool in no table
# (curl); a copy from a registry the workflow did not fill; a plain scalar
# continued on deeper lines. Make `push` is trusted by its name.
#
# WHAT THE QUOTE SCANNER CAN STILL MISS (#798). It reads a `run:` body as
# bash does, with these bounds: a `#` after whitespace is a comment even
# inside `${...}` or `[[ ]]`, where bash may read data, and the rest of that
# line is not seen; a heredoc whose delimiter is `<<$X`, a digit-led word or
# empty is not recognised, and its body is then read with quote tracking;
# the quote reader is qsplit's only, so a `$'...'` string is one word in
# the other readers (a tool word in it refuses). Heredoc bodies are read as
# commands, so prose naming a registry tool there refuses rather than passes.
#
# THE COUNT IS NOT A BACKSTOP. A missed job leaves the population, so it
# drops out of the serialisation check and the count together: a third
# publisher the classifier does see keeps the count at two and the file
# reports OK while serialised. So `make` lines are joined over continuations,
# and a target that is a variable, or no target at all (the Makefile's default
# goal), refuses like any other unclassified command (#798).
#
# WHAT IT DOES NOT CLAIM. It reads the workflow text. It cannot know
# whether the runners exist, whether the jobs really start together, or
# whether a push succeeds. It answers one question — "does any
# publishing job wait, directly or transitively, on another" — which is
# the question that was answered wrong.
#
# Usage: check-build-job-independence.sh [workflow-file]
# Exit:  0 no publishing job depends on another
#        1 one does — the serialised shape is back
#        2 the check cannot render a verdict (unreadable file, a
#          `needs:` form it cannot parse, a command, action or shell it
#          cannot classify, or fewer than two publishing jobs, which would
#          make the rule vacuous)
set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

WF="${1:-.github/workflows/release.yml}"

if [ ! -f "$WF" ] || [ ! -r "$WF" ]; then
    echo "::error title=Nothing to inspect::$WF is not a readable file." \
         "The rule would otherwise pass having examined nothing." >&2
    exit 2
fi

# Emits `job<TAB>publishes<TAB>needs,needs,...<TAB>reference-writes` per
# job (the fourth column is #798), or a BAD line naming what it will not
# guess at.
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
    function classify_line(s, fnr,   n, i, segs, v, orig) {
        orig = s
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
        n = qsplit(orig, segs)
        for (i = 1; i <= n; i++) {
            v = classify_reg(segs[i])
            if (v == "bytes") pub = 1
            else if (v == "ref") refw = 1
            else if (v != "none") bad(v " -> " trim(segs[i]), fnr)
        }
    }

    # Registry commands split at && || ; | outside quotes only (#798): a `|`
    # inside an echo string must not make a command position. The quote
    # state QS carries across the lines of one `run:` body, so a string
    # closed on this line and one opened on it leave the command between.
    # Text bash does not parse as a string never reaches that state: a `#`
    # at a word start cuts the line (CUT), and the lines after a `<<WORD`
    # up to WORD are split with no quote tracking, which reads every
    # command in them (bash <<EOF) and carries nothing out (#798).
    # dry=1 only reports CUT and leaves QS and the heredoc queue alone.
    function qsplit(s, segs, dry,   i, c, q, out, t, w) {
        CUT = 0
        if (HDN > 0) {
            t = s; sub(/^[[:space:]]+/, "", t)
            if (t == HDT[1]) { if (!dry) hd_shift(); return 0 }
            gsub(/&&|\|\||;|\|/, "\x01", s)
            return split(s, segs, "\x01")
        }
        q = QS; out = ""
        for (i = 1; i <= length(s); i++) {
            c = substr(s, i, 1)
            if (c == "\\" && q != 1) { out = out c substr(s, i + 1, 1); i++; continue }
            if (q == 0 && c == "$" && substr(s, i + 1, 1) == "\x27") { q = 3; out = out c "\x27"; i++; continue }
            if (c == "\x27" && q != 2) { if (q == 1 || q == 3) q = 0; else q = 1 }
            else if (c == "\"" && q != 1 && q != 3) { if (q == 2) q = 0; else q = 2 }
            else if (q == 0 && c == "#" && (i == 1 || substr(s, i - 1, 1) ~ /[[:space:];&|(]/)) { CUT = 1; break }
            else if (q == 0 && c == "<" && substr(s, i + 1, 1) == "<") {
                if (substr(s, i + 2, 1) == "<") { out = out "<<"; i += 2 }
                else if (!dry && (w = hd_word(substr(s, i + 2))) != "") HDT[++HDN] = w
            }
            else if (q == 0 && (c == ";" || c == "|")) c = "\x01"
            else if (q == 0 && c == "&" && substr(s, i + 1, 1) == "&") { c = "\x01"; i++ }
            out = out c
        }
        if (!dry) QS = q
        return split(out, segs, "\x01")
    }

    # The delimiter word after `<<`, or "" when it is not a heredoc (`<<2`,
    # `<<$X`): a miss only drops back to the quote-tracking split (#798).
    function hd_word(r,   qc) {
        sub(/^-/, "", r); sub(/^[[:space:]]*/, "", r)
        if (r ~ /^[\x27"]/) {
            qc = substr(r, 1, 1); r = substr(r, 2)
            if (index(r, qc) == 0) return ""
            return substr(r, 1, index(r, qc) - 1)
        }
        if (r !~ /^\\?[A-Za-z_]/) return ""
        sub(/^\\/, "", r); sub(/[^A-Za-z0-9_.-].*$/, "", r)
        return r
    }
    function hd_shift(   k) { for (k = 1; k < HDN; k++) HDT[k] = HDT[k + 1]; HDN-- }

    # A quoted string is one word (#798): `x="$a $b"` assigns, it runs no `$b"`.
    function qword(s,   i, c, q, out) {
        q = 0; out = ""
        for (i = 1; i <= length(s); i++) {
            c = substr(s, i, 1)
            if (c == "\\" && q != 1) { out = out c substr(s, i + 1, 1); i++; continue }
            if (c == "\x27" && q != 2) { if (q == 1) q = 0; else q = 1 }
            else if (c == "\"" && q != 1) { if (q == 2) q = 0; else q = 2 }
            else if (q != 0 && c ~ /[[:space:]]/) c = "\x03"
            out = out c
        }
        return out
    }

    function bare(w) { gsub(/["\x27]/, "", w); sub(/^[({]+/, "", w); sub(/[)};]+$/, "", w); return w }
    function istool(w) { return w ~ /^(docker|crane|oras|cosign|podman|buildah|skopeo|regctl|nerdctl|ko)$/ }
    function isvar(w) { return w ~ /[$`]/ }
    function unknown(tool) { return "a `" tool "` verb this gate has no class for" }

    # `$(` and a backtick start a new command, so a tool inside a
    # substitution is read at its own command position (release.yml reads
    # digests with `X=$(crane digest ...)`, #798).
    function classify_reg(seg,   p, np, k, v, out) {
        sub(/^[[:space:]]*/, "", seg); sub(/^-[[:space:]]+/, "", seg)
        gsub(/\\`/, "", seg)
        gsub(/\$\(/, "\x02", seg); gsub(/`/, "\x02", seg)
        np = split(seg, p, "\x02"); out = "none"
        if (np > 1 && reg_piece(p[1]) == "empty") return "a command named by a variable"
        for (k = 1; k <= np; k++) {
            v = reg_piece(p[k])
            if (v == "empty") continue
            if (v != "none" && v != "ref" && v != "bytes") return v
            if (v == "bytes" || out == "none") out = v
        }
        return out
    }

    # Assignments and reserved words keep the command position; any other
    # word before a tool is a wrapper whose effect this cannot read, and a
    # command that is a variable could be any tool (#798). "empty": no
    # command word and no trailing assignment, so a `$(` after it is one.
    function reg_piece(s,   t, n, i, j, cmd, w, a, na, last, ww, nw, m) {
        n = split(qword(s), t, /[[:space:]]+/); last = ""
        for (i = 1; i <= n; i++) {
            if (t[i] == "") continue
            last = t[i]
            if (t[i] ~ /^[A-Za-z_][A-Za-z0-9_]*=/) continue
            if (t[i] ~ /^(if|then|else|elif|do|while|until|!|[{(]|time)$/) continue
            break
        }
        if (i > n && last ~ /^[A-Za-z_][A-Za-z0-9_]*=/) return "none"
        if (i > n) return "empty"
        if (isvar(t[i])) return "a command named by a variable"
        cmd = bare(t[i])
        if (cmd ~ /^(echo|printf)$/) return "none"
        na = 0
        for (j = i + 1; j <= n; j++) {
            if (t[j] == "") continue
            nw = split(bare(t[j]), ww, "\x03")
            for (m = 1; m <= nw; m++)
                if (istool(bare(ww[m]))) return "a registry tool that is not at command position"
            a[++na] = bare(t[j])
        }
        w = cmd; sub(/.*\//, "", w)
        if (!istool(w)) return "none"
        if (w ~ /^(podman|buildah|skopeo|regctl|nerdctl|ko)$/)
            return "`" w "` is a registry tool with no verb table here"
        if (na == 0) return "none"
        if (a[1] ~ /^-/) return "a flag before the `" w "` verb"
        if (isvar(a[1])) return "a `" w "` verb that is a variable"
        if (isvar(a[2]) && (w == "docker" && a[1] ~ /^(image|builder|plugin|manifest|trust|buildx|compose)$/ ||
                            w == "oras" && a[1] ~ /^(blob|manifest|repo)$/))
            return "a `" w " " a[1] "` verb that is a variable"
        if (a[2] == "imagetools" && isvar(a[3])) return "a `" w " buildx imagetools` verb that is a variable"
        if (w == "docker") return docker_verb(a, na)
        if (w == "crane") {
            if (a[1] ~ /^(push|append|mutate|rebase|flatten)$/) return "bytes"
            if (a[1] ~ /^(tag|copy|cp|delete|index)$/) return "ref"
            if (a[1] ~ /^(auth|blob|catalog|config|digest|export|ls|manifest|pull|validate|version|help|completion)$/) return "none"
            return unknown(w)
        }
        if (w == "oras") {
            if (a[1] == "push") return "bytes"
            if (a[1] ~ /^(cp|copy)$/ && from_local(a, na)) return "bytes"
            if (a[1] ~ /^(attach|cp|copy|tag)$/) return "ref"
            if (a[1] ~ /^(discover|login|logout|pull|resolve|version|help|completion)$/) return "none"
            if (a[1] == "blob" && a[2] == "push") return "bytes"
            if (a[1] == "blob" && a[2] == "delete") return "ref"
            if (a[1] == "blob" && a[2] == "fetch") return "none"
            if (a[1] == "manifest" && a[2] == "push") return "bytes"
            if (a[1] == "manifest" && a[2] ~ /^(delete|index)$/) return "ref"
            if (a[1] == "manifest" && a[2] ~ /^(fetch|fetch-config)$/) return "none"
            if (a[1] == "repo" && a[2] ~ /^(ls|tags)$/) return "none"
            return unknown(w)
        }
        if (a[1] ~ /^(upload|load)$/) return "bytes"
        if (a[1] ~ /^(sign|attest|attach|copy|clean)$/) return "ref"
        if (a[1] ~ /^(sign-blob|attest-blob|verify|verify-attestation|verify-blob|verify-blob-attestation|version|triangulate|tree|download|save|generate|generate-key-pair|import-key-pair|public-key|env|initialize|login|manifest|dockerfile|help|completion)$/) return "none"
        return unknown(w)
    }

    # oras cp/copy reads a local OCI layout with --from-oci-layout[-path];
    # those bytes are uploaded, not referenced (#798, oras 1.2 docs).
    function from_local(a, na,   k) {
        for (k = 2; k <= na; k++) if (a[k] ~ /^--from-oci-layout/) return 1
        return 0
    }

    # The docker CLI commands (27.x); a verb outside them is a CLI
    # plugin, which may publish (`docker pushrm`), so it refuses (#798).
    function docker_verb(a, na) {
        if (a[1] == "push") return "bytes"
        if (a[1] == "build") return push_output(a, na, 2)
        if (a[1] == "image" && a[2] == "push") return "bytes"
        if (a[1] ~ /^(image|builder)$/ && a[2] == "build") return push_output(a, na, 3)
        if (a[1] == "plugin" && a[2] == "push") return "bytes"
        if (a[1] == "manifest" && a[2] == "push") return "ref"
        if (a[1] == "trust" && a[2] ~ /^(sign|revoke|signer)$/) return "ref"
        if (a[1] ~ /^(image|builder|plugin|manifest|trust)$/) return "none"
        if (a[1] == "buildx") {
            if (a[2] ~ /^(build|b)$/) return push_output(a, na, 3)
            if (a[2] == "bake" && push_output(a, na, 3) == "none")
                return "a bake whose outputs live in its definition file"
            if (a[2] == "bake") return push_output(a, na, 3)
            if (a[2] == "imagetools" && a[3] == "create") return "ref"
            if (a[2] == "imagetools" && a[3] == "inspect") return "none"
            if (a[2] ~ /^(create|debug|dial-stdio|du|history|inspect|ls|prune|rm|stop|use|version)$/) return "none"
            return unknown("docker buildx")
        }
        if (a[1] == "compose") {
            if (a[2] ~ /^-/) return "a flag before the `docker compose` verb"
            if (a[2] ~ /^(push|publish)$/) return "bytes"
            if (a[2] == "build") return push_output(a, na, 3)
            if (a[2] ~ /^(attach|commit|config|cp|create|down|events|exec|export|images|kill|logs|ls|pause|port|ps|pull|restart|rm|run|scale|start|stats|stop|top|unpause|up|version|wait|watch)$/) return "none"
            return unknown("docker compose")
        }
        if (a[1] ~ /^(attach|checkpoint|commit|config|container|context|cp|create|diff|events|exec|export|history|images|import|info|init|inspect|kill|load|login|logout|logs|network|node|pause|port|ps|pull|rename|restart|rm|rmi|run|save|search|secret|service|stack|start|stats|stop|swarm|system|tag|top|unpause|update|version|volume|wait)$/) return "none"
        return unknown("docker")
    }

    # A build publishes only through a push output; an output this cannot
    # read refuses rather than reading as a local build (#798).
    function push_output(a, na, from,   k) {
        for (k = from; k <= na; k++) {
            if (a[k] == "--push" || a[k] ~ /push=true|type=registry/) return "bytes"
            if (a[k] ~ /^--output=/ && isvar(a[k])) return "a build output that is a variable"
            if (a[k] ~ /^(--output|-o)$/ && isvar(a[k + 1])) return "a build output that is a variable"
        }
        return "none"
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
            if (tolower(t) ~ /push|publish/) undec = 1     # push-arm64 (#798)
        }
        if (sawpush) return "publishes"
        if (undec) return "undecided"
        if (!sawtarget) return "undecided"   # the default goal lives in the Makefile
        return "no"
    }

    # Only a POSIX shell body is read as commands (#798).
    function checkshell(line, fnr,   v) {
        v = line; sub(/^[[:space:]]+(-[[:space:]]+)?shell:/, "", v)
        v = unquote(trim(decomment(v))); sub(/[[:space:]].*/, "", v); sub(/.*\//, "", v)
        if (v !~ /^(bash|sh)$/) bad("a shell whose body this does not read as commands -> " v, fnr)
    }

    function flush() { if (job != "") printf "%s\t%s\t%s\t%s\n", job, pub, needs, refw }

    # Shell text enters here and nowhere else. `\`-continuations are
    # joined first: a `make` invocation split over two lines is one
    # command, and a line-oriented reader sees neither half as a
    # publisher.
    function feed(line, fnr,   tmp) {
        if (line ~ /^[[:space:]]*#/ && cont == "" && QS == 0) return   # a shell comment
        if (cont != "") { line = cont " " line; cont = "" }
        if (line ~ /\\[[:space:]]*$/) {
            qsplit(line, tmp, 1)   # a backslash inside a comment joins nothing (#798)
            if (!CUT) {
                sub(/\\[[:space:]]*$/, "", line)
                cont = line
                return
            }
        }
        classify_line(line, fnr)
    }

    # -1 is the sentinel for "not inside a block scalar", and an unset
    # awk variable is 0, which compares >= 0 and would put the reader
    # in a block from the first line of the file.
    BEGIN { runind = -1 }

    /^jobs:[[:space:]]*$/ { injobs = 1; next }
    !injobs && /^[[:space:]]+(-[[:space:]]+)?shell:[[:space:]]*[^[:space:]#]/ { checkshell($0, FNR); next }
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
        pub = 0; refw = 0; needs = ""; inneeds = 0; runind = -1
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

    # A `run:` VALUE IS THE ONLY SHELL THIS FILE READS, and in this
    # repo the only shell there is. (An action taking a script as a
    # `with:` input would be shell too; an action outside the table
    # below refuses, #798.) The first version
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

    # Actions are a closed table (#798): an action outside it may publish
    # (docker/build-push-action), so it refuses rather than reads as none.
    # A job-level `uses:` is a reusable workflow, whose steps are unread.
    /^    uses:/ { bad("a job-level reusable workflow, whose steps this does not read", FNR); next }
    /^[[:space:]]*(-[[:space:]]+)?uses:([[:space:]]|$)/ {
        rest = $0
        sub(/^[[:space:]]*(-[[:space:]]+)?uses:/, "", rest)
        rest = unquote(trim(decomment(rest))); sub(/@.*/, "", rest)
        if (rest ~ /^(actions\/(checkout|setup-go|upload-artifact|download-artifact)|anchore\/sbom-action\/download-syft|docker\/login-action|sigstore\/cosign-installer)$/) next
        if (rest ~ /^(actions\/attest-build-provenance|peter-evans\/dockerhub-description)$/) { refw = 1; next }
        bad("an action this gate has no class for -> " rest, FNR)
        next
    }
    /^[[:space:]]+(-[[:space:]]+)?shell:[[:space:]]*[^[:space:]#]/ { checkshell($0, FNR); next }

    /^[[:space:]]*(-[[:space:]]+)?run:([[:space:]]|$)/ {
        ind = index($0, "run:") - 1
        rest = $0
        sub(/^[[:space:]]*(-[[:space:]]+)?run:[[:space:]]*/, "", rest)
        QS = 0; HDN = 0
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
         "\`needs:\` spelling, a command, an action or a shell it does not" \
         "understand, because reporting OK about text it could not read is" \
         "the silence it exists to end (#796)." >&2
    printf '%s\n' "$parsed" | awk -F'\t' '$1 == "BAD" { printf "  %s:%s: %s\n", wf, $2, $3 }' wf="$WF" >&2
    echo >&2
    echo "  Accepted needs:  'needs: name', 'needs: [a, b]' (items may be" >&2
    echo "  quoted), and a block sequence of '      - name' items." >&2
    echo "  Accepted make:   an invocation whose targets are literal words," >&2
    echo "  so 'push' can be found among them whatever the argument order." >&2
    echo "  Accepted registry commands: docker, crane, oras and cosign verbs" >&2
    echo "  in the tables in this file, at command position; actions in its" >&2
    echo "  action table; bash or sh as the shell." >&2
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
# it refuses a verdict instead. It is not a backstop for the classifier:
# its count comes from the classifier (#798).
if [ "$npub" -lt 2 ]; then
    echo "::error title=Nothing to compare::$WF has $npub job(s) uploading" \
         "image bytes; the rule needs at least two to mean anything." \
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
echo "Reference writes only, free to wait on publishers:" \
     "$(printf '%s\n' "$parsed" | awk -F'\t' '$1 != "BAD" && $2 == 0 && $4 == 1 { printf "%s ", $1 }')"
