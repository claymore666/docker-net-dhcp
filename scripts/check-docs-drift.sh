#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Documentation-drift check (#345). Companion to check-option-docs.sh,
# which already covers driver options. This covers the two other things
# the code exposes to operators, plus the failure mode that let a whole
# section rot unnoticed:
#
#   1. every /Plugin.Health field is documented in the reference
#   2. every settable plugin env var is documented in the reference
#   2b. every settable env var that exists ONLY on the coverage-
#      instrumented manifest is exempt from the reference BY NAME, and
#      documented in the contributor page instead
#   3. no option, counter, or setting is documented in a *second*
#      docs page
#
# (3) is the one that matters most. Before #345 the reference and the
# macvlan page each carried a full copy of the options table and the
# counter table; only the reference was gated, so the duplicate silently
# went a release stale (it never learned about join_start_failures, and
# told operators the wrong healthy contract). One canonical home per
# fact, enforced.
#
# Keys come from the code and the plugin manifest, never a hardcoded
# list here — a list would be one more thing to forget to update.
#
# TWO MANIFESTS, ONE OF THEM EXEMPT — DELIBERATELY AND OUT LOUD.
#
# The plugin ships from config.json; the coverage lane installs
# config-cover.json, which declares two extra settables (GOCOVERDIR,
# REQUEST_CAPTURE_DIR) that the binary really does read. Reading only
# config.json meant those two were settings no gate had ever looked at,
# under a reference that opens by claiming CI enforces every setting is
# documented.
#
# They are NOT added to the reference. `docker plugin set GOCOVERDIR=…`
# is impossible on the plugin a user installs — the setting does not
# exist on that artifact — so documenting it in the operator manual
# would document a knob that is not there, which is a worse defect than
# the one being fixed. That is a decision the tree already made on
# purpose (pkg/plugin/capture.go, docs/internals.md): test
# instrumentation stays out of the shipped manifest.
#
# The exemption is not free, and it is not a comment. A cover-only
# setting must be documented in the CONTRIBUTOR page, and the gate says
# EXEMPT with the reason on every run, so "we decided not to document
# it" cannot silently become "nobody documented it". The failure this
# closes is a real operator setting landing in config-cover.json alone
# — the #317 shape one level up — and thereby escaping rule 2 forever.
#
# Usage: check-docs-drift.sh [<go-package-dir>] [<docs-dir>] [<reference-doc>]
#   defaults: pkg/plugin docs docs/reference.md (run from the repo root)
# Env: MANIFEST        shipped plugin manifest (default config.json)
#      COVER_MANIFEST  coverage-instrumented manifest
#                      (default: config-cover.json beside MANIFEST)
#      CONTRIB_DOC     the page cover-only settings must be documented
#                      in (default: <docs-dir>/internals.md)
#      DOCS_DRIFT_AWK  the awk to extract with. A test seam: it is what
#                      gives the exit-status half of the extraction
#                      backstop a case, which no Go-source fixture can
#                      produce once every operand is proved readable in
#                      the shell first.
#
# Exit: 0 clean, 1 drift found, 2 cannot check (bad usage/inputs).
set -u

PKG_DIR="${1:-pkg/plugin}"
DOCS_DIR="${2:-docs}"
DOC="${3:-docs/reference.md}"
MANIFEST="${MANIFEST:-config.json}"

if [ ! -d "$PKG_DIR" ] || [ ! -d "$DOCS_DIR" ] || [ ! -f "$DOC" ]; then
    echo "usage: $0 [<go-package-dir>] [<docs-dir>] [<reference-doc>]" >&2
    echo "missing: $PKG_DIR, $DOCS_DIR or $DOC" >&2
    exit 2
fi

fail=0

# THE awk BINARY IS A SEAM, for the same reason its two sibling gates in
# this change have one. Both extractions below read awk's exit status,
# and with every operand proved readable in the shell first, no Go-source
# fixture can make awk fail while still printing -- so the status half
# would be a branch with no case, which is the shape this gate was fixed
# for one rule along. The self-test points this at a stub that prints a
# full, plausible name list and then exits non-zero.
AWK="${DOCS_DRIFT_AWK:-awk}"

# ---- 1. health counters -------------------------------------------------
# The json tags of HealthResponse are the wire contract, so they are what
# operators grep for and what the docs must name.
counters=$("$AWK" '
    /type HealthResponse struct \{/ { in_struct = 1; next }
    in_struct && /^\}/              { in_struct = 0 }
    in_struct && match($0, /json:"[a-z0-9_]+"/) {
        tag = substr($0, RSTART + 6, RLENGTH - 7)
        print tag
    }
' "$PKG_DIR"/endpoints.go)

if [ -z "$counters" ]; then
    echo "FAIL  could not extract HealthResponse fields from $PKG_DIR/endpoints.go" >&2
    exit 2
fi

for c in $counters; do
    if grep -qF "\`$c\`" "$DOC"; then
        echo "PASS  counter $c documented"
    else
        echo "FAIL  counter $c is returned by /Plugin.Health but absent from $DOC"
        fail=1
    fi
done

# ---- 2. plugin settings -------------------------------------------------
# Settable env from the plugin manifest: what `docker plugin set` accepts.
if [ -f "$MANIFEST" ]; then
    envs=$(python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
for e in m.get("env", []):
    if e.get("name"):
        print(e["name"])
' "$MANIFEST")
    for e in $envs; do
        if grep -qF "\`$e\`" "$DOC"; then
            echo "PASS  setting $e documented"
        else
            echo "FAIL  setting $e is settable on the plugin but absent from $DOC"
            fail=1
        fi
    done
else
    echo "FAIL  plugin manifest $MANIFEST not found" >&2
    exit 2
fi

# ---- 2b. cover-only settings ------------------------------------------
# See the header. Anything the cover manifest declares that the shipped
# one does not is exempt from the reference by name, and owes the
# contributor page a definition instead.
COVER_MANIFEST="${COVER_MANIFEST:-$(dirname "$MANIFEST")/config-cover.json}"
CONTRIB_DOC="${CONTRIB_DOC:-$DOCS_DIR/internals.md}"

if [ -f "$COVER_MANIFEST" ]; then
    cover_envs=$(python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
for e in m.get("env", []):
    if e.get("name"):
        print(e["name"])
' "$COVER_MANIFEST")
    for e in $cover_envs; do
        # Also on the shipped plugin: rule 2 already judged it.
        # Matched as whole LINES ($envs is newline-separated), and
        # matched without a pipe: `... | grep -q` reports failure on
        # success under pipefail, because the match kills the producer
        # with SIGPIPE (#297, and scripts/check-pipefail-consumers.sh).
        case $'\n'"$envs"$'\n' in *$'\n'"$e"$'\n'*) continue ;; esac
        if [ ! -f "$CONTRIB_DOC" ]; then
            echo "FAIL  $COVER_MANIFEST declares cover-only setting $e but the contributor page $CONTRIB_DOC does not exist — nothing can carry the exemption" >&2
            exit 2
        fi
        if grep -qF "\`$e\`" "$CONTRIB_DOC"; then
            echo "EXEMPT setting $e — declared in $(basename "$COVER_MANIFEST") only (test instrumentation, not settable on the shipped plugin); documented in $(basename "$CONTRIB_DOC")"
        else
            echo "FAIL  setting $e is settable on the coverage plugin, is not in $MANIFEST, and is documented in neither $DOC nor $CONTRIB_DOC — document it where it lives or add it to the shipped manifest"
            fail=1
        fi
    done
else
    echo "note  no cover manifest at $COVER_MANIFEST — no cover-only settings to judge"
fi

# ---- 3. no second home --------------------------------------------------
# A name is "documented" on a page when it *leads a definition* — the
# first cell of a markdown table row, or a bullet that opens with it.
# Both shapes were in use by the duplicates this gate exists to prevent:
# the options and env tables were tables, the counter list was bullets.
# Prose mentions and cross-links match neither, which is deliberate —
# the other pages should keep pointing at the reference.
#
# Driver options join the counters and settings here. check-option-docs.sh
# proves each option is documented *somewhere*; this proves it isn't
# documented twice.
#
# Cover-only settings are deliberately absent from this set: their one
# home is the contributor page, and rule 2b is what holds them to it.
# Listing them here would make the very page that documents them the
# duplicate.
#
# The operands are collected and PROVED READABLE first, rather than
# handed to awk as a bare glob. See the backstop below for what that
# cost when they were not.
shopt -s nullglob
go_files=("$PKG_DIR"/*.go)
shopt -u nullglob
if [ "${#go_files[@]}" -eq 0 ]; then
    echo "FAIL  $PKG_DIR matched no *.go, so no option name could be read" >&2
    exit 2
fi
for g in "${go_files[@]}"; do
    if [ ! -f "$g" ] || [ ! -r "$g" ]; then
        echo "FAIL  $g is not a readable regular file, so the option names it" \
             "declares could not be read. awk is silent about an operand it" \
             "cannot open, and that silence reads exactly like a package with" \
             "no options in it." >&2
        exit 2
    fi
done

options=$("$AWK" '
    /type DHCPNetworkOptions struct \{/ { in_struct = 1; next }
    in_struct && /^\}/                  { in_struct = 0 }
    in_struct {
        line = $0
        sub(/^[ \t]+/, "", line)
        if (line ~ /^\/\// || line == "") next
        if (match(line, /^[A-Z][A-Za-z0-9]*/)) {
            name = substr(line, RSTART, RLENGTH)
            if (match(line, /mapstructure:"[^"]+"/)) {
                tag = substr(line, RSTART, RLENGTH)
                gsub(/mapstructure:"|"/, "", tag)
                print tag
            } else {
                print tolower(name)
            }
        }
    }
' "${go_files[@]}")
options_rc=$?

# THE NON-VACUITY BACKSTOP THE COUNTER RULE ALREADY HAD AND THIS ONE DID
# NOT (#839). `counters` refuses on empty output at :88; `options` had
# nothing, and it is the rule that reads a WHOLE GLOB rather than one
# named file, so it had more ways to read less than it thought.
# Measured 2026-08-28, with a real duplicate planted (`| \`mode\` |` as a
# table row on a second page) and the control confirmed red at rc=1:
#
#   a DIRECTORY named aaa.go beside the sources   mawk rc=0 MISSED
#                                                 gawk rc=1 (skips it)
#   plugin.go at mode 000                         rc=0 MISSED, BOTH awks
#
# The second arm is the awk-independent one and it is the reason the
# guard is not "handle the directory case": awk's silence about a file
# it could not open is the defect, whichever way the silence arrives. So
# readability is proved in the SHELL before awk (`-f` and `-r` are the
# same on every awk and every uid), awk's exit status is read, and an
# empty extraction refuses. A gate that reports intent -- "I looped over
# the file list" -- as if it were coverage is the same shape one level
# up as the finding this branch is about.
if [ "$options_rc" -ne 0 ] || [ -z "$options" ]; then
    echo "FAIL  could not extract DHCPNetworkOptions fields from $PKG_DIR/*.go" \
         "(awk exit $options_rc, $(printf '%s' "$options" | grep -c . ) name(s) read)." \
         "Rule 3 below would otherwise judge the duplicate-home rule against a" \
         "shorter list of names than the code actually declares, and report green" \
         "having looked for fewer things." >&2
    exit 2
fi

names="$counters $envs $options"
ref_base=$(basename "$DOC")

for page in "$DOCS_DIR"/*.md; do
    # The same rule, on the other side of the comparison. `grep` on an
    # unreadable page exits 2 and matches nothing, which rule 3 reads as
    # "this page does not document the name" -- a silent pass on exactly
    # the page a second home would be hiding on.
    if [ ! -f "$page" ] || [ ! -r "$page" ]; then
        echo "FAIL  $page is not a readable regular file, so it could not be" \
             "searched for a second home. Treating that as \"documents nothing\"" \
             "would pass this page in silence." >&2
        exit 2
    fi
    [ "$(basename "$page")" = "$ref_base" ] && continue
    # A page may waive a name it legitimately leads a row with for an
    # unrelated reason — the mode-comparison table opens rows with
    # `bridge` as an attachment *mode*, not the `bridge` option. Declare
    # it inline, with the reason, so the waiver is reviewable in the
    # diff that adds it:
    #   <!-- docs-drift-ok: bridge — mode-comparison table, not the option -->
    waivers=$(grep -oE '<!--[[:space:]]*docs-drift-ok:[[:space:]]*[A-Za-z0-9_.]+' "$page" \
        | sed -E 's/.*docs-drift-ok:[[:space:]]*//' || true)
    for n in $names; do
        case " $waivers " in *" $n "*)
            echo "WAIVED $(basename "$page"): \`$n\` (declared in-page)"
            continue ;;
        esac
        # leading table cell: | `name` | ...
        if grep -qE "^\|[[:space:]]*\`$n\`[[:space:]]*\|" "$page"; then
            echo "FAIL  $(basename "$page") documents \`$n\` in a table row — the reference is its only home"
            fail=1
        # leading definition bullet: - `name` — ...  /  * `name`: ...
        elif grep -qE "^[[:space:]]*[-*][[:space:]]+\`$n\`[[:space:]]*[—:-]" "$page"; then
            echo "FAIL  $(basename "$page") documents \`$n\` in a definition bullet — the reference is its only home"
            fail=1
        fi
    done
done

# ---- 4. no rootfs route to a bind-mounted path --------------------------
# A path the manifest bind-mounts is reachable at that path on the HOST.
# It is not reachable by walking into the plugin rootfs: the mount is
# applied in the plugin's own mount namespace, so from the host that
# route finds a bare mount point, or nothing.
#
# This is the shape that rotted when #440 mounted STATE_DIR. Both the
# audit-ledger recipe and a troubleshooting row kept sending operators
# to `/var/lib/docker/plugins/*/rootfs/var/lib/net-dhcp`, which is empty
# — and a wrong path that returns no data reads exactly like "the
# feature produced nothing" (#489).
#
# Destinations come from the manifest, so a future mount is covered the
# day it is added. Paths the manifest does NOT mount are untouched: the
# plugin log genuinely does live at `…/rootfs/var/log/net-dhcp.log` and
# must stay documented that way.
mounts=$(python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
for mnt in m.get("mounts", []):
    if mnt.get("type") == "bind" and mnt.get("destination"):
        print(mnt["destination"])
' "$MANIFEST")

pages=("$DOCS_DIR"/*.md)
readme="$(dirname "$DOCS_DIR")/README.md"
# `-e`, not `-f`. Absent is a legitimate state -- the docs tree can be
# checked on its own -- but `-f` folds "absent" together with "present
# and not a regular file", and a directory named README.md then leaves
# the page set without a word.
if [ -e "$readme" ]; then
    pages+=("$readme")
fi

# THE SAME REFUSAL RULE 3 GOT, OVER THE WHOLE PAGE SET, AND THIS IS
# WHERE IT BELONGS RATHER THAN BESIDE ONE LOOP. Rules 4 and 5 search
# these pages with `grep`, and `grep` on an unreadable page exits 2
# having matched nothing -- read as "this page documents nothing",
# which is a silent pass on exactly the page a stale route would be
# hiding on. Their only test was `[ -f "$page" ]`, and a file at mode
# 000 IS a regular file.
#
# Rule 3 iterates `$DOCS_DIR/*.md` and refuses there, so pages under
# `docs/` were protected only INCIDENTALLY -- rule 3 exits 2 before
# rules 4 and 5 are reached. README.md is in this array and in no
# other, so it was protected by nothing. Measured 2026-08-28 with the
# control confirmed red first: a real rule-4 finding and a real rule-5
# finding, each planted in README.md, each reported at exit 1 while
# readable and each LOST at mode 000 -- exit 0, `docs-drift gate
# passed`, under both mawk and gawk.
#
# Applied to the ARRAY and not inside the two loops on purpose: a page
# added to `pages` later inherits the refusal instead of needing its
# own copy of it, which is how README.md came to have none.
for page in "${pages[@]}"; do
    if [ ! -f "$page" ] || [ ! -r "$page" ]; then
        echo "FAIL  $page is not a readable regular file, so it could not be" \
             "searched for a stale rootfs route or an install procedure." \
             "Treating that as \"documents nothing\" would pass this page in" \
             "silence, which is how a finding in README.md was lost." >&2
        exit 2
    fi
done

for d in $mounts; do
    for page in "${pages[@]}"; do
        # No `-f` test here: readability is decided once, above, for the
        # whole array. A per-loop `|| continue` is what let README.md be
        # skipped in silence, and two copies of a guard are two places
        # for one of them to be missing.
        # The stale shape is literal: "rootfs" immediately followed by
        # the mounted destination, whatever glob precedes it.
        if grep -qF "rootfs$d" "$page"; then
            echo "FAIL  $(basename "$page") routes a reader through the plugin rootfs to \`$d\`, which the manifest bind-mounts — read it on the host at \`$d\`"
            fail=1
        fi
    done
done

# ---- 5. a documented install creates the bind source it needs -----------
# The mirror of rule 4. That one is about *destinations* — where a reader
# is told to look. This one is about *sources* — what has to exist on the
# host before the plugin will start at all.
#
# Docker does not create a missing bind source; `plugin enable` fails and
# leaves the plugin disabled. So every procedure that runs
# `docker plugin install` must create every bind source first, or it is a
# procedure that does not work.
#
# Scoped to the fenced code block, not the page: a block is one
# copy-pasteable procedure, and that is the unit a reader follows. It is
# also what makes this catch the case it was written for (#494) — the
# install block in reference.md creates the directory and the *upgrade*
# block, further down the same page, did not.
sources=$(python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
for mnt in m.get("mounts", []):
    if mnt.get("type") == "bind" and mnt.get("source"):
        print(mnt["source"])
' "$MANIFEST")

for page in "${pages[@]}"; do
    # Same as rule 4: the array was proved readable once, above.
    for src in $sources; do
        # `/run/docker.sock` and friends are daemon-owned and always
        # present; only paths the plugin itself owns are the operator's
        # to create. The manifest cannot express that, so the discriminator
        # is whether any documented procedure creates it anywhere.
        grep -qF "mkdir -p $src" "$page" || continue
        offenders=$(SRC="$src" python3 -c '
import os, re, sys

src = os.environ["SRC"]
text = open(sys.argv[1], encoding="utf-8").read()
# Fenced blocks only. A prose mention of `docker plugin install` is not a
# procedure and must not be judged as one.
for i, block in enumerate(re.findall(r"^```[^\n]*\n(.*?)^```", text, re.S | re.M)):
    if "docker plugin install" not in block:
        continue
    if f"mkdir -p {src}" in block:
        continue
    first = next(l.strip() for l in block.splitlines()
                 if "docker plugin install" in l)
    print(f"{first}")
' "$page")
        if [ -n "$offenders" ]; then
            while IFS= read -r line; do
                echo "FAIL  $(basename "$page") installs the plugin without creating \`$src\`, which the manifest bind-mounts and Docker will not create — \`plugin enable\` fails and leaves it disabled: $line"
                fail=1
            done <<< "$offenders"
        fi
    done
done

if [ "$fail" -eq 0 ]; then
    echo "docs-drift gate passed"
fi
exit "$fail"
