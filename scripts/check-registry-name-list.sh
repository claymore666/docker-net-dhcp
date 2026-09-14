#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The published registry names are bound ONCE, and everything that has
# to mean the same name derives from that binding (#972).
#
# WHAT WENT WRONG. `GHCR_NAME`, `HUB_NAME` and `HUB_ALIAS` were
# transcribed into five job-level `env:` blocks. Every gate downstream
# keys a cell on the VARIABLE NAME, because that is what a job's step
# lines carry, so two jobs binding one variable to two strings read as
# one cell: the publish/verify parity gate would go on reporting that
# the name being pushed is the name being install-verified while the
# verifier proved a different repository entirely. Nothing in the tree
# compared the bindings, and nothing could, because "the same string in
# five places" is not a property any one place holds.
#
# A workflow-level `env:` makes divergence inexpressible rather than
# detectable, which is the better fix. This gate is what keeps it that
# way, and it checks four things.
#
#   1. THE LIST EXISTS AND IS NOT EMPTY. Every claim below is about the
#      names in it, so an empty list would make all of them vacuously
#      true -- the strongest possible pass, measured over nothing.
#
#   2. NOTHING REBINDS A LISTED NAME. A job or step `env:` that binds
#      one of these variables again is the transcription coming back,
#      and it wins over the workflow-level value for that job, silently.
#
#   3. NO LITERAL ANYWHERE ELSE. A repository string written out in a
#      `run:` line or an action input is a second binding that does not
#      even look like one. Comments are prose and are excluded: a gate
#      that failed on a sentence would be answered by editing the
#      sentence.
#
#   4. EACH HUB NAME IS REACHED BY EVERY CONSUMER THAT IS OTHERWISE A
#      TRANSCRIPTION. A name can be in the list, used by the push, and
#      still be the name whose description is never synced or whose
#      mirror is missing from the release page -- which is what happened
#      by hand between v2.0.0 and v2.1.0.
#
# THE DIVISION OF LABOUR IS DELIBERATE. Publish, install-verify and
# promote-to-:latest are `scripts/check-publish-verify-parity.sh`'s
# three sets, derived there from the workflow and compared in both
# directions. This gate does not re-derive them: two mechanisms for one
# property is the transcription problem one layer up, and the looser
# derivation would decide. What is left over -- the signature, the
# description sync and the release-notes mirror line -- has no other
# observer, and those are the three roles checked here.
#
# Usage: bash scripts/check-registry-name-list.sh [workflow]
# Exit:  0 one list, no rebinding, no stray literal, every consumer reached
#        1 a rebinding, a stray literal, or a Hub name a consumer misses
#        2 CANNOT JUDGE -- the workflow is unreadable or the list is empty
set -uo pipefail

WORKFLOW="${1:-.github/workflows/release.yml}"

refuse() {
    echo "::error title=The registry name list cannot be judged::$*" >&2
    exit 2
}

[ -f "$WORKFLOW" ] || refuse "no workflow at '$WORKFLOW'; there is no name list to derive from."

report=$(python3 - "$WORKFLOW" <<'PARSE'
import re, sys

path = sys.argv[1]
lines = open(path, encoding="utf-8").read().split("\n")

# A repository reference: an optional registry host, then namespace and
# name. Keyed on the SHAPE of the value, not on a list of variables, so
# a fourth published name joins the list by being written in it.
REPO = re.compile(r"^(?:[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/)?"
                  r"[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$")
KV   = re.compile(r"^(\s*)([A-Za-z_][A-Za-z0-9_]*):\s*(\S.*?)\s*$")

def is_comment(line):
    return line.lstrip().startswith("#")

# 1. the workflow-level env block: `env:` in column 0, entries at two
#    spaces, ending at the next column-0 key.
names, start, end = {}, None, None
for i, line in enumerate(lines):
    if line == "env:":
        start = i
        continue
    if start is not None and end is None:
        if line and not line[0].isspace():
            end = i
            break
        if is_comment(line):
            continue
        m = KV.match(line)
        if m and m.group(1) == "  " and REPO.match(m.group(3)):
            names[m.group(2)] = m.group(3)
if start is not None and end is None:
    end = len(lines)

if not names:
    print("REFUSE\tno workflow-level env: block binding a registry repository "
          "reference. That block is this gate's whole subject, so without it "
          "every rule below is true of nothing and this would pass having "
          "measured no names at all.")
    raise SystemExit(0)

findings = []

# 2. a registry repository bound anywhere but the one list.
#
# Keyed on the VALUE's shape, not on the variable being one this file
# already lists. A name can leave the list two ways: rebound under a
# name the list still carries, or carried out of the list entirely and
# transcribed into the jobs that used it. The second shape is the one
# this file replaced, and a rule that only knows the listed names
# cannot see it -- de-listing a name makes every job-level copy of it
# invisible to the very rule written to forbid copies.
for i, line in enumerate(lines):
    if start is not None and start <= i < end:
        continue
    if is_comment(line):
        continue
    m = KV.match(line)
    if not (m and m.group(1) != "" and REPO.match(m.group(3))):
        continue
    if m.group(2) in names:
        findings.append("%s:%d rebinds %s, which the list at the top of the "
                        "file already binds. A job-level value wins for that "
                        "job, so this is two names wearing one variable."
                        % (path, i + 1, m.group(2)))
    else:
        findings.append("%s:%d binds %s to the repository '%s' outside the "
                        "workflow-level list. Every published name belongs to "
                        "that one block; a binding here is a second name that "
                        "no rule below ranges over."
                        % (path, i + 1, m.group(2), m.group(3)))

# 3. a transcribed literal, IN A REGISTRY REFERENCE POSITION.
#
# `val in line` was a plain substring test and it misdirected twice.
# `https://github.com/claymore666/docker-net-dhcp/releases` contains the
# alias repository, and the advice it printed -- write ${HUB_ALIAS}
# here -- would have broken the URL. And GHCR_NAME's value CONTAINS
# HUB_ALIAS's value, so one legitimate GHCR literal produced two
# findings, the second of them wrong.
#
# So the occurrence has to be a registry reference and not a substring
# of one: the whole token is taken, it must parse as `[host/]ns/name`
# with an optional `:tag` or `@digest`, and it is attributed to the
# LONGEST listed value that matches. A URL with a scheme or a third path
# segment is not a reference, and the longest match means a GHCR literal
# is reported once, as GHCR.
TOKEN = re.compile(r"""[^\s"'`,;]+""")
REF   = re.compile(r"^(?:([a-z0-9][a-z0-9.-]*(?::[0-9]+)?)/)?"
                   r"([a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*)"
                   # The tag or digest may be an expansion: the shapes
                   # this gate has to catch are written `:${TAG}` and
                   # `@${DIGEST}` far more often than with a literal
                   # version, and a pattern that only knew literals
                   # would have passed every transcription the workflow
                   # could actually contain.
                   r"(?:[:@](?:\$\{\{?[^}]*\}\}?|[A-Za-z0-9._:-])+)?$")

by_length = sorted(names.items(), key=lambda kv: -len(kv[1]))

for i, line in enumerate(lines):
    if start is not None and start <= i < end:
        continue
    if is_comment(line):
        continue
    for tok in TOKEN.findall(line):
        m = REF.match(tok)
        if m is None:
            continue
        # A Hub name is the same name written `docker.io/ns/name` or
        # `ns/name`, so both spellings are offered; the longest listed
        # value that matches is the one it belongs to, which is what
        # keeps a GHCR literal from also being reported as the Hub
        # alias whose value is a tail of it.
        host, repo = m.group(1), m.group(2)
        spellings = {repo}
        if host is not None:
            spellings.add("%s/%s" % (host, repo))
        for var, val in by_length:
            if val in spellings:
                findings.append("%s:%d writes the literal '%s' instead of "
                                "${%s}. A transcribed name is a second binding "
                                "that does not look like one."
                                % (path, i + 1, val, var))
                break

# 3b. THE POPULATION. Every variable the step lines USE as a registry
#     name must be one this list binds.
#
# Rules 1 to 3 all range over the list, so all three are satisfied by
# taking a name OUT of it. Measured on this very file: delete HUB_ALIAS
# from the workflow-level block, transcribe it into the seven job-level
# env: blocks that referenced it, and this gate printed OK over the two
# names that were left -- the exact shape the block exists to forbid,
# reported as clean. Point one of those job-level bindings at a
# different repository and the install proof proves a different
# repository, still clean.
#
# So the domain is derived from the other side: a variable used as
# `${VAR}:` or `${VAR}@` is being used as a registry name, whatever it
# is called. The ones the file ASSIGNS are local shell variables
# (`REF="${GHCR_NAME}:${TAG}"`, `local reg="$1"`) and carry no binding
# of their own; what is left must come from an env: block, and the only
# env: block allowed to bind a repository is the one at the top.
USED   = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}[:@]")
ASSIGN = re.compile(r"(?:^|[;&|(]|\blocal\s+|\bexport\s+)\s*"
                    r"([A-Za-z_][A-Za-z0-9_]*)=")

assigned, used = set(), {}
for i, line in enumerate(lines):
    if is_comment(line):
        continue
    for m in ASSIGN.finditer(line):
        assigned.add(m.group(1))
for i, line in enumerate(lines):
    if start is not None and start <= i < end:
        continue
    if is_comment(line):
        continue
    for m in USED.finditer(line):
        used.setdefault(m.group(1), i + 1)

population = {v: ln for v, ln in used.items() if v not in assigned}

if not population:
    print("REFUSE\tno line in %s uses a variable as a registry reference "
          "(`${NAME}:` or `${NAME}@`), so the list at the top has nothing to "
          "be the list OF and rule 3b would range over nothing." % path)
    raise SystemExit(0)

for var in sorted(population):
    if var not in names:
        findings.append("%s:%d uses ${%s} as a registry name, and the "
                        "workflow-level list does not bind it. A name that "
                        "reaches the steps from somewhere else is a name no "
                        "rule here ranges over: it can be pushed under one "
                        "repository and verified under another with every "
                        "gate green."
                        % (path, population[var], var))

# 4. every Hub name is reached by each consumer that has no other observer.
#
# The three roles are read off the file as SHAPES. `cosign` covers both
# directions the workflow uses: the source name is signed and the alias
# is verified through its own name, and either one is this workflow
# establishing that the name carries a signature.
body = "\n".join(l for l in lines if not is_comment(l))

# THE RELEASE-NOTES ROLE IS SCOPED TO THE STEP THAT WRITES THE BODY,
# found by the file it writes rather than by the step's name or by a
# word in the sentence. A probe keyed on prose ("mirrored to Docker
# Hub") is answered by rewording the prose.
STEP = re.compile(r"^\s+- (?:name|uses):")
steps, cur = [], []
for line in lines:
    if is_comment(line):
        continue
    if STEP.match(line):
        steps.append(cur)
        cur = []
    cur.append(line)
steps.append(cur)
notes_steps = [s for s in steps if any("notes.md" in l for l in s)]
if not notes_steps:
    print("REFUSE\tno step in %s writes notes.md, so the release-notes role "
          "below has nothing to range over and would pass every name "
          "vacuously." % path)
    raise SystemExit(0)
notes_body = "\n".join(l for s in notes_steps for l in s)

ROLES = [
    # For HUB_NAME this is `cosign sign`; for the alias it is the copy
    # step, which is where this workflow establishes that the alias
    # carries a verifiable signature -- the verify itself is inside
    # scripts/publish-hub-alias.sh and that script's self-test drives
    # it. Either way the claim is the same: the workflow does something
    # about this name's signature.
    ("a cosign signature, or the copy step that carries one",
     # The copy alternative counts only for the name being copied TO --
     # the last operand. The SOURCE name appears on the same line, and
     # accepting it there let a workflow with no `cosign sign` at all
     # pass this rule because the unsigned name was mentioned as the
     # thing being read.
     lambda v: re.search(r"cosign (?:sign|verify)[^\n]*\$\{%s\}" % v, body)
               or re.search(r"publish-hub-alias\.sh[^\n]*\$\{%s\}:\$\{\w+\}\"\s*$"
                            % v, body, re.M)),
    ("a Docker Hub description sync",
     lambda v: re.search(r"repository:\s*\$\{\{\s*env\.%s\s*\}\}" % v, body)),
    ("a mention in the step that writes the release notes",
     lambda v: re.search(r"\$\{%s\}" % v, notes_body)),
]

# Only the Hub names carry all three: GHCR has no description sync and
# no second name to mirror, and inventing a role for it would be a rule
# written to be satisfiable rather than one read off the workflow.
hub = {v: val for v, val in names.items() if not val.startswith("ghcr.io/")}
if not hub:
    print("REFUSE\tthe name list binds no Docker Hub repository. Rule 4 is "
          "about Hub names and has nothing to range over.")
    raise SystemExit(0)

for var in sorted(hub):
    for role, probe in ROLES:
        if not probe(var):
            findings.append("${%s} is in the name list but nothing in %s gives "
                            "it %s. A name that is pushed and then left out of "
                            "one of these is published and unfindable, or "
                            "published and undocumented, and the release page "
                            "is where a reader would have learned otherwise."
                            % (var, path, role))

print("NAMES\t" + " ".join("%s=%s" % (k, names[k]) for k in sorted(names)))
for f in findings:
    print("FAIL\t" + f)
PARSE
)
rc=$?
[ "$rc" -eq 0 ] || refuse "could not parse '$WORKFLOW' (python3 exit $rc)."

if printf '%s\n' "$report" | grep '^REFUSE' >/dev/null; then
    refuse "$(printf '%s\n' "$report" | sed -n 's/^REFUSE\t//p')"
fi

names_line=$(printf '%s\n' "$report" | sed -n 's/^NAMES\t//p')
fails=$(printf '%s\n' "$report" | sed -n 's/^FAIL\t//p')

if [ -n "$fails" ]; then
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        echo "::error title=The registry name list is not the only binding::$f" >&2
        echo "FAIL: $f" >&2
    done <<< "$fails"
    exit 1
fi

echo "OK: one binding for each published name, and every Hub name is signed, described and mirrored in the notes:"
printf '%s\n' "$names_line" | tr ' ' '\n' | sed 's/^/  /'
