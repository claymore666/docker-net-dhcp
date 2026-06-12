#!/usr/bin/env bash
# Option-docs drift check (#132): every driver-option key the plugin
# parses must appear (backticked) in the reference manual. Adding an
# option without documenting it turns CI red — staleness becomes
# impossible rather than reviewable.
#
# Keys are extracted from the code, not hardcoded here:
#   1. Network-level options: fields of the DHCPNetworkOptions struct —
#      the mapstructure tag when present, the lowercased field name
#      otherwise (mapstructure's case-insensitive default match).
#   2. Per-endpoint options: string literals indexing an options map,
#      i.e. `options["<key>"]` / `Options["<key>"]`.
#
# Usage: check-option-docs.sh [<go-package-dir>] [<reference-doc>]
#   defaults: pkg/plugin docs/reference.md (run from the repo root)
set -u

PKG_DIR="${1:-pkg/plugin}"
DOC="${2:-docs/reference.md}"

if [ ! -d "$PKG_DIR" ] || [ ! -f "$DOC" ]; then
    echo "usage: $0 [<go-package-dir>] [<reference-doc>]" >&2
    echo "missing: $PKG_DIR or $DOC" >&2
    exit 2
fi

# Non-test Go sources only — tests synthesize option maps freely.
src_files=$(find "$PKG_DIR" -maxdepth 1 -name '*.go' ! -name '*_test.go')
if [ -z "$src_files" ]; then
    echo "FAIL  no Go sources found under $PKG_DIR" >&2
    exit 2
fi

# shellcheck disable=SC2086  # word-splitting src_files is intended
struct_keys=$(awk '
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
' $src_files)

if [ -z "$struct_keys" ]; then
    echo "FAIL  DHCPNetworkOptions struct not found in $PKG_DIR — moved/renamed? Update $0 deliberately." >&2
    exit 1
fi

# shellcheck disable=SC2086
endpoint_keys=$(grep -hoE '[Oo]ptions\["[a-z0-9_]+"\]' $src_files \
    | sed -E 's/.*\["([a-z0-9_]+)"\].*/\1/' | sort -u)

fail=0
for key in $(printf '%s\n%s\n' "$struct_keys" "$endpoint_keys" | sort -u); do
    if grep -q "\`$key\`" "$DOC"; then
        echo "PASS  $key documented in $DOC"
    else
        echo "FAIL  $key: parsed by the code but not documented in $DOC"
        fail=1
    fi
done

if [ "$fail" -ne 0 ]; then
    echo
    echo "Option-docs drift: add the missing key(s) to the driver-options"
    echo "table in $DOC (backticked) in the same PR that introduces them."
fi
exit "$fail"
