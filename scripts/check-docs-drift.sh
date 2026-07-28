#!/usr/bin/env bash
# Documentation-drift check (#345). Companion to check-option-docs.sh,
# which already covers driver options. This covers the two other things
# the code exposes to operators, plus the failure mode that let a whole
# section rot unnoticed:
#
#   1. every /Plugin.Health field is documented in the reference
#   2. every settable plugin env var is documented in the reference
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
# Usage: check-docs-drift.sh [<go-package-dir>] [<docs-dir>] [<reference-doc>]
#   defaults: pkg/plugin docs docs/reference.md (run from the repo root)
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

# ---- 1. health counters -------------------------------------------------
# The json tags of HealthResponse are the wire contract, so they are what
# operators grep for and what the docs must name.
counters=$(awk '
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
options=$(awk '
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
' "$PKG_DIR"/*.go)

names="$counters $envs $options"
ref_base=$(basename "$DOC")

for page in "$DOCS_DIR"/*.md; do
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

if [ "$fail" -eq 0 ]; then
    echo "docs-drift gate passed"
fi
exit "$fail"
