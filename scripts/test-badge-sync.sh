#!/usr/bin/env bash
# Table-driven tests for badge-sync.py's answers-file validator, plus a
# check of the repository's real .bestpractices.json.
#
# The validator is the part that has to hold: a malformed answers file
# would otherwise be discovered only when a push to the live badge entry
# half-applied. Everything here is offline — no network, no session.
set -u

SYNC="$(dirname "$0")/badge-sync.py"
REAL="$(dirname "$0")/../.bestpractices.json"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

check() { # check <name> <want_exit> <json-file>
    local name="$1" want_exit="$2" file="$3" got_exit
    python3 "$SYNC" --check --file "$file" > "$TMP/out" 2>&1
    got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

write() { # write <file> <<'EOF' ... EOF
    cat > "$1"
}

write "$TMP/good.json" <<'EOF'
{
  "governance_status": "Met",
  "governance_justification": "https://example.com/GOVERNANCE.md",
  "dco_status": "Unmet",
  "dco_justification": "No DCO or CLA today.",
  "crypto_tls12_status": "N/A",
  "crypto_tls12_justification": "No TLS in the software.",
  "bus_factor_status": "?"
}
EOF
check "well-formed file passes" 0 "$TMP/good.json"

write "$TMP/bad-status.json" <<'EOF'
{
  "governance_status": "met",
  "governance_justification": "lowercase status is not a valid value"
}
EOF
check "invalid status value fails" 1 "$TMP/bad-status.json"

write "$TMP/missing-justification.json" <<'EOF'
{
  "governance_status": "Met"
}
EOF
check "answered criterion without justification fails" 1 "$TMP/missing-justification.json"

write "$TMP/empty-justification.json" <<'EOF'
{
  "governance_status": "Unmet",
  "governance_justification": ""
}
EOF
check "empty justification fails" 1 "$TMP/empty-justification.json"

write "$TMP/orphan-justification.json" <<'EOF'
{
  "governance_justification": "text with no matching status field"
}
EOF
check "orphaned justification fails" 1 "$TMP/orphan-justification.json"

write "$TMP/stray-field.json" <<'EOF'
{
  "governance_status": "Met",
  "governance_justification": "fine",
  "notes": "a field that is neither a status nor a justification"
}
EOF
check "unrecognised field fails" 1 "$TMP/stray-field.json"

write "$TMP/unanswered.json" <<'EOF'
{
  "bus_factor_status": "?"
}
EOF
check "unanswered criterion needs no justification" 0 "$TMP/unanswered.json"

write "$TMP/non-string.json" <<'EOF'
{
  "governance_status": true,
  "governance_justification": "status must be a string"
}
EOF
check "non-string value fails" 1 "$TMP/non-string.json"

printf 'not json at all\n' > "$TMP/broken.json"
check "unparseable file fails" 1 "$TMP/broken.json"

check "missing file fails" 1 "$TMP/does-not-exist.json"

# The real file is the point of the exercise; it must always validate.
check "repository .bestpractices.json validates" 0 "$REAL"

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed"
    exit 1
fi
echo "all badge-sync validator tests passed"
