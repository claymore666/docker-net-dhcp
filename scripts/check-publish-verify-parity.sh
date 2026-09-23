#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every published {registry x architecture} must have an install
# verifier and a floating-tag promotion (#833).
#
# WHAT WENT WRONG. Docker Hub dual-publish began 2026-05-05. Hub
# install-verification landed 2026-08-22 (#777). In between, 20 tags
# shipped a Docker Hub artifact that nothing proved installable -- and
# for the 15 from v1.0.0 on, the GHCR copy WAS install-tested while the
# Hub copy was not.
#
# The gap is closed. This gate exists so the closure is STRUCTURAL
# rather than remembered: the verify set was transcribed -- four
# hand-written jobs -- while the publish set is what actually decides
# what users can pull. Adding a registry did not add a verifier, and
# nothing noticed for three months.
#
# WHY "WE BUILT AND TESTED IT LOCALLY" DOES NOT COVER IT. The Hub
# publish path rebuilds the rootfs under a Hub-prefixed name and re-tars
# it via `docker plugin create`. Those are not the bytes the local test
# exercised. Install-verification of the PUBLISHED artifact is the only
# thing that reads the object users actually pull.
#
# DERIVED, NOT TRANSCRIBED, which is the whole point. A list of four
# expected job names would pass forever while a fifth cell shipped
# unverified -- it would be the same transcription that failed, written
# one layer up. So all three sets come out of the workflow itself:
#
#   publish  `make PLUGIN_NAME="${N}" PLUGIN_TAG="${T}" ... push`
#            or a command whose LAST operand is `"<host>/${N}:${T}"` --
#            the DESTINATION of a copy publishes a name just as a build
#            does
#   verify   `REF="${N}:${T}"` in a job that really installs
#   promote  `crane tag "${N}:${T}" ...`
#
# A COPY PUBLISHES (#972). The Hub alias is the same manifest under a
# second name, copied after signing instead of built again, because a
# second `docker plugin create` re-tars the rootfs and changes the
# digest (#267). Keyed only on `make ... push`, this gate would have
# seen an install verifier and a promotion for a name it did not think
# was published, and said nothing: the alias could have stopped being
# published and the proofs would have gone on verifying whatever was
# already in that repository.
#
# The destination is the LAST reference on the line, which is what the
# anchor at the end of the pattern picks out, and the line itself has to
# sit outside shell quoting for the same reason an install does. An
# echoed copy is an advertisement.
#
# THE COPY IS NOT KEYED ON THE TOOL. It was `oras cp` for one round,
# and then the copy moved into `scripts/publish-hub-alias.sh` so its
# refusal branch could be driven with the transport stubbed -- at which
# point a gate keyed on the tool name reported the alias verified and
# promoted but never published, which is exactly the reverse-direction
# failure a few paragraphs down. The tool is the mechanism; the property
# is that an EXECUTED command line ends in a registry reference it is
# writing to, and `copies()` reads that in three parts: the reference is
# a separate final operand, the line's first word is outside quoting,
# and that first word is not `echo` or `printf`. The last condition is
# where the advertisement is excluded, because `echo "oras cp src"
# "dst"` satisfies the other two and publishes nothing -- exactly as the
# install detection excludes an advertisement by insisting the command
# word is executed.
#
# It follows that the argument order in the caller is load-bearing, and
# `scripts/publish-hub-alias.sh` says so in its own header: put the
# destination anywhere but last and the alias silently leaves the
# published set here.
#
# THE SETS ARE COMPARED IN BOTH DIRECTIONS. A published cell with no
# verifier was the #833 failure; a verifier or a promotion for a cell
# nothing publishes is the same defect approached from the other side,
# and it is what a dropped copy looks like. Both are failures here.
#
# Add a registry and its publish cell appears here with no verifier;
# this fails and names the cell.
#
# THE ARCHITECTURE COMES FROM THE RESOLVED TAG, and that is the one
# design decision in this file worth defending. The obvious key is the
# job's `runs-on`, and it is wrong: `promote-latest` runs on an amd64
# runner and retags BOTH architectures, because `crane tag` is a
# registry operation with no architecture of its own. Reading `runs-on`
# reports the arm64 cells unpromoted -- a false failure produced by the
# instrument, not by the workflow. It was the first thing this gate
# said, and it was wrong.
#
# Keying on the runner for verify jobs, on the tag variable for promote
# and on either for publish would be three mechanisms for one property:
# the same transcription that failed here, rewritten one layer up. So
# every role is keyed on the SAME property -- which published tag the
# line names -- by resolving `${T}` against the enclosing job's own
# `env:` (job-level and step-level, since the release job binds TAG at
# the step) and asking whether the value ends in `-arm64`.
#
# A tag variable that resolves to nothing is a refusal, not a guess. An
# unresolvable name silently treated as amd64 would merge arm64 cells
# into their amd64 neighbours and report parity that was never checked.
#
# ECHOES ARE NOT COMMANDS. The release job prints
# `docker plugin install ${GHCR_NAME}:${TAG}` into the step summary as
# user-facing instructions, and counting one of those as a verifier
# would let a release that installs nothing report full parity.
#
# THIS COMMENT USED TO CREDIT THE WRONG MECHANISM, AND THAT IS THE
# DEFECT #858 SHIPPED WITH. It said `--grant-all-permissions` separated
# the command from the advertisement. The predicate had no notion of an
# echo at all, so an advertisement CARRYING the flag counted:
#
#     echo "docker plugin install --grant-all-permissions $REF"
#
# With all four verify jobs converted to that, the gate emitted its
# strongest pass -- "4 published cell(s), each install-verified and
# promoted" -- over a workflow that installed nothing.
#
# The discriminator is POSITION, not vocabulary: an install counts only
# when the token sits OUTSIDE any shell quoting ON ITS OWN LINE, in
# command position (#883): a bare `echo docker plugin install ...` is
# unquoted and still prints. A re-wording cannot get around either.
#
# THE CLAIM STOPS AT THE LINE, and the boundary is stated here because
# the earlier version of this paragraph did not state one -- it said
# "not something a phrasing can defeat", which is wider than the code
# and would have been read as covering the shapes below. Each was
# driven against the predicate; each is counted as a real install:
#
#   a heredoc body line          docker plugin install --grant... $REF
#   a multi-line echo's 2nd line  ...--grant-all-permissions $REF"
#
# The scan reads one line at a time with quoting state reset at each
# newline. Both are therefore out of reach by construction, not by
# oversight. A trailing comment is not: `#` is no command separator.
#
# The live one is the heredoc: release.yml already writes step
# summaries and already uses heredocs, so ordinary housekeeping could
# turn the advertisement into a heredoc body and silently re-create the
# exact false pass this gate exists to catch. Widening the analyser to
# track heredocs and continuations is a shell parser, which is a larger
# thing than this gate should become -- so the gap is named rather than
# closed, and a future author inherits a bounded claim.
#
# `--grant-all-permissions` is still required, for its own reason
# rather than as an echo test: an install without it stops at the
# permission prompt, so a verify step lacking it is not a working
# verifier whether or not it is quoted. Both conditions, independently.
#
# The comment also cited check-plugin-bind-sources.sh as drawing this
# distinction. It draws a different one -- it skips lines whose first
# non-space character is `#`. That precedent is real and is implemented
# below, but it is comment-stripping, not echo-exclusion.
#
# Exit: 0 every published cell is verified and promoted
#       1 a cell is published without a verifier or without a promotion
#       2 CANNOT JUDGE -- the workflow is unreadable or a set came out
#         empty, which would make every universal below vacuously true
set -uo pipefail

WORKFLOW="${1:-.github/workflows/release.yml}"

refuse() {
    echo "::error title=Publish/verify parity cannot be judged::$*" >&2
    exit 2
}

[ -f "$WORKFLOW" ] || refuse "no workflow at '$WORKFLOW'; there is no publish set to derive from."

# Emit one `ROLE<TAB>arch<TAB>var<TAB>job` record per cell.
#
# `jobs:` is tracked explicitly because job names and the `on:` triggers
# sit at the SAME indentation -- `  push:` under `on:` is not a job, and
# a scan keyed on two spaces alone would invent one named "push" in a
# file about pushing.
records=$(python3 - "$WORKFLOW" <<'PARSE'
import re, sys

JOB   = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$")
TOP   = re.compile(r"^[A-Za-z]")
ENVKV = re.compile(r"^\s+([A-Za-z_][A-Za-z0-9_]*):\s*(\S.*?)\s*$")

PUBLISH = re.compile(r'PLUGIN_NAME="\$\{(\w+)\}".*PLUGIN_TAG="\$\{(\w+)\}"')
COPY    = re.compile(r'\s"(?:[A-Za-z0-9.:-]+/)?\$\{(\w+)\}:\$\{(\w+)\}"\s*$')
OPERAND = re.compile(r'"(?:[A-Za-z0-9.:-]+/)?\$\{(\w+)\}:\$\{(\w+)\}"')
PRINTER = re.compile(r'(?:echo|printf)$')
VERIFY  = re.compile(r'REF="\$\{(\w+)\}:\$\{(\w+)\}"')
PROMOTE = re.compile(r'crane tag\s+"\$\{(\w+)\}:\$\{(\w+)\}"')
INSTALL = re.compile(r"docker plugin install\b.*--grant-all-permissions")


def unquoted_offsets(text):
    """Offsets of `text` that lie outside shell quoting.

    The step summary prints `docker plugin install ...` as instructions
    for the reader, and an echoed command is quoted while a real one is
    not. Keying on the flag could not tell them apart -- an
    advertisement is free to carry any flag it likes, because it is
    text. Re-wording the echoed text cannot move the quote.

    THE SCOPE IS ONE LINE. Quoting state is not carried across a
    newline, so a heredoc body, the continuation line of a multi-line
    quoted string, and a trailing `#` comment all read as unquoted.
    See the header for why those are named rather than handled.

    Single quotes suppress everything including backslashes; double
    quotes honour backslash escapes. Both are what a POSIX shell does,
    and `run:` blocks are shell.
    """
    out, q, i = set(), None, 0
    while i < len(text):
        c = text[i]
        if q is None:
            if c in "'\"":
                q = c
            else:
                out.add(i)
        elif c == q:
            q = None
        elif q == '"' and c == "\\":
            i += 1
        i += 1
    return out


KEYWORD = re.compile(r"(?:^|\s)(?:then|do|else|elif|if|while|until|!)$")


# Command position, as check-release-refusal-order.sh reads it (#883):
# unquoted, and only a separator, a keyword or the `run:` key before it.
# A bare `echo docker plugin install ...` is unquoted and still prints.
def command_at(text, start):
    if start not in unquoted_offsets(text):
        return False
    head = re.sub(r"^\s*(?:-\s+)?(?:run:\s*)?", "", text[:start], count=1)
    while True:
        head = head.rstrip()
        if head == "" or head[-1] in ";&|({":
            return True
        m = KEYWORD.search(head)
        if m is None:
            return False
        head = head[:m.start()]


def runs(text, rx):
    """The matches of `rx` that a shell would EXECUTE.
    An occurrence inside quotes or behind a printer is text the step
    prints, not a command it runs. Used for the install detection
    (#858), the verifier's reference and the promotion (#883).
    """
    return [m for m in rx.finditer(text) if command_at(text, m.start())]

def is_install(text):
    """True when `text` runs an install rather than printing one."""
    return bool(runs(text, INSTALL))


def copies(text):
    """The match when `text` is a command line PUBLISHING its last operand.

    Three conditions, none of them the name of a tool (#972):

      1. the line ends in a quoted `<host>/${N}:${T}` operand, preceded
         by whitespace that is itself outside quoting -- so the
         reference is a separate argument and `REF="${N}:${T}"`, which
         has no space before its quote, stays an assignment;
      2. the first word is not a printing builtin. `echo "cmd ..."
         "<dest>"` satisfies (1) and publishes nothing, and nothing
         about the reference itself can tell the two apart -- what
         differs is whether the process started writes to a registry or
         to stdout;
      3. the line names a SECOND registry reference. A copy reads a
         source and writes a destination, so it carries two; a command
         that carries one and ends in a reference is reading it.

    (3) is here because (1) and (2) alone could not tell a write from a
    read, and that was measured, not argued: replacing both copy calls
    with `crane digest "docker.io/${HUB_ALIAS}:${TAG}"` -- a pure read
    of the same reference -- left this gate reporting its strongest
    pass while nothing published the alias at all, and the two install
    proofs and the promotion ran over whatever that repository already
    held. The version this replaced caught that by naming `oras cp -r`,
    and could not survive the tool changing; keying on the operand
    count catches it without naming a tool.

    THE BOUND. This reads arity, not intent. A two-operand READ would
    still be taken for a copy, and no property of the step line
    distinguishes one; what carries that is the copy step's own script
    and the self-test that drives its refusals. In the other direction
    a one-operand WRITE -- a direct `docker plugin push "<ref>"` -- is
    not a copy and not the `make push` shape either, so it is invisible
    to both publish rules.

    A quoted command word is deliberately NOT a condition. A quoted
    word still executes, so excluding it would only have dropped the
    continuation lines of a wrapped command, whose last operand is
    published all the same.
    """
    m = COPY.search(text)
    if m is None:
        return None
    free = unquoted_offsets(text)
    if m.start() not in free:
        return None
    body = text.split()
    if body and PRINTER.match(body[0]):
        return None
    if len(OPERAND.findall(text)) < 2:
        return None
    return m

jobs, cur, in_jobs = [], None, False
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.rstrip("\n")
    if TOP.match(line):
        in_jobs = line.startswith("jobs:")
        continue
    if in_jobs:
        m = JOB.match(line)
        if m:
            cur = {"name": m.group(1), "env": {}, "hits": [], "install": False}
            jobs.append(cur)
            continue
    if cur is None:
        continue
    stripped = line.split("#", 1)[0] if line.lstrip().startswith("#") else line
    m = ENVKV.match(stripped)
    if m and "${" not in m.group(1):
        cur["env"][m.group(1)] = m.group(2)
    for n, t in PUBLISH.findall(stripped):
        cur["hits"].append(("publish", n, t))
    for role, rx in (("verify", VERIFY), ("promote", PROMOTE)):
        for m in runs(stripped, rx):
            cur["hits"].append((role, m.group(1), m.group(2)))
    copied = copies(stripped)
    if copied is not None:
        cur["hits"].append(("publish", copied.group(1), copied.group(2)))
    if is_install(stripped):
        cur["install"] = True

unresolved = []
for j in jobs:
    for role, n, t in j["hits"]:
        if role == "verify" and not j["install"]:
            continue
        val = j["env"].get(t)
        if val is None:
            unresolved.append("%s: %s names ${%s}, which the job never binds" % (j["name"], role, t))
            continue
        arch = "arm64" if val.rstrip("'\"").endswith("-arm64") else "amd64"
        print("%s\t%s\t%s\t%s" % (role, arch, n, j["name"]))

if unresolved:
    sys.stderr.write("UNRESOLVED\n" + "\n".join(unresolved) + "\n")
    sys.exit(3)
PARSE
)
parse_rc=$?
if [ "$parse_rc" -eq 3 ]; then
    refuse "a tag variable could not be resolved against its own job's env, so the cell's architecture is unknown and treating it as amd64 would merge it into a neighbour and report parity that was never checked. See the lines above."
elif [ "$parse_rc" -ne 0 ]; then
    refuse "could not parse '$WORKFLOW' (python3 exit $parse_rc)."
fi

cell() { printf '%s' "$1" | awk -F'\t' -v r="$2" '$1 == r { print $2 "/" $3 }' | sort -u; }

published=$(cell "$records" publish)
verified=$(cell  "$records" verify)
promoted=$(cell  "$records" promote)

# NON-VACUITY, on the set that carries every claim below. "Every
# published cell is verified" is TRUE of no published cells, so a parser
# that silently stopped matching would report the strongest possible
# pass. The verify and promote sets are checked too: an empty one makes
# the failure real rather than vacuous, but an empty one is far more
# likely to mean the detector broke, and saying so beats reporting four
# cells unverified and sending someone at the release path.
[ -n "$published" ] || refuse "derived ZERO published cells from $WORKFLOW. Every claim this gate makes is about published cells, so it would report a clean pass having measured nothing. The 'make PLUGIN_NAME=... push' pattern this derives from has probably changed."
[ -n "$verified" ]  || refuse "derived ZERO install-verified cells from $WORKFLOW, while $(printf '%s\n' "$published" | wc -l) cell(s) are published. That is either a total loss of install verification or -- far more likely -- the 'docker plugin install --grant-all-permissions' pattern no longer matches."
[ -n "$promoted" ]  || refuse "derived ZERO promoted cells from $WORKFLOW, while $(printf '%s\n' "$published" | wc -l) cell(s) are published. Either ':latest' stopped moving or the 'crane tag' pattern no longer matches."

rc=0
# `verb` reads as a predicate ("install-verifies it"); `noun` names the
# same thing in a title slot. One string in both places produced
# "A published cell is not install-verifies" and "nothing promotes to
# :latest it" -- the title is what the Actions UI shows, so it is the
# half most people read.
report() { # verb noun set
    local verb="$1" noun="$2" have="$3" missing
    missing=$(comm -23 <(printf '%s\n' "$published") <(printf '%s\n' "$have"))
    [ -n "$missing" ] || return 0
    rc=1
    while IFS= read -r c; do
        [ -n "$c" ] || continue
        echo "::error title=A published cell has no ${noun}::${c} is published by $WORKFLOW but nothing ${verb}." >&2
        echo "FAIL: $c is published, but nothing ${verb}" >&2
    done <<< "$missing"
}

report "install-verifies it"      "install verifier"      "$verified"
report "promotes it to :latest"  "promotion to :latest"  "$promoted"

# THE OTHER DIRECTION (#972). A verifier or a promotion for a cell that
# nothing publishes is not a harmless extra: it is what a dropped
# publish step looks like from here. The proof goes on installing, and
# the tag goes on moving, over whatever was already in that repository
# -- which is the previous release. The publish set is the one that
# decides what users can pull, so anything claiming to cover a cell
# outside it is claiming to cover an object this workflow does not make.
orphan() { # noun set
    local noun="$1" have="$2" extra art="a"
    case "$noun" in [aeiou]*) art="an" ;; esac
    extra=$(comm -13 <(printf '%s\n' "$published") <(printf '%s\n' "$have"))
    [ -n "$extra" ] || return 0
    rc=1
    while IFS= read -r c; do
        [ -n "$c" ] || continue
        echo "::error title=${art^} ${noun} for a cell nothing publishes::${c} has ${art} ${noun} in $WORKFLOW, but no step in this workflow publishes that cell. It would keep passing over whatever is already in that repository." >&2
        echo "FAIL: $c has ${art} ${noun}, but nothing publishes it" >&2
    done <<< "$extra"
}
orphan "install verifier"     "$verified"
orphan "promotion to :latest" "$promoted"

if [ "$rc" -eq 0 ]; then
    echo "OK: $(printf '%s\n' "$published" | wc -l) published cell(s), each install-verified and promoted, and nothing verified or promoted that is not published:"
    printf '%s\n' "$published" | sed 's/^/  /'
fi
exit "$rc"
