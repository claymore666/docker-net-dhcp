#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for govulncheck-gate.sh (#127): synthetic
# govulncheck outputs against a synthetic allowlist.
set -u

GATE="$(dirname "$0")/govulncheck-gate.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ALLOW="$TMP/allow.txt"
cat > "$ALLOW" <<'EOF'
# accepted, daemon-side only
GO-2026-4887
GO-2026-4883
EOF

failures=0
check() {
    local name="$1" want_exit="$2" report="$3"
    bash "$GATE" "$report" "$ALLOW" > "$TMP/out" 2>&1
    local got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

cat > "$TMP/clean.txt" <<'EOF'
No vulnerabilities found.
EOF
check "no findings passes (and warns about stale allowlist)" 0 "$TMP/clean.txt"
grep -q 'WARN  GO-2026-4887' "$TMP/out" || { echo "FAIL: missing stale-entry warning"; failures=$((failures + 1)); }

cat > "$TMP/allowed.txt" <<'EOF'
Vulnerability #1: GO-2026-4887
    Moby AuthZ plugin bypass
Vulnerability #2: GO-2026-4883
    Moby off-by-one
Your code is affected by 2 vulnerabilities from 1 module.
EOF
check "only allowlisted findings passes" 0 "$TMP/allowed.txt"

cat > "$TMP/new.txt" <<'EOF'
Vulnerability #1: GO-2026-4887
    Moby AuthZ plugin bypass
Vulnerability #2: GO-2099-0001
    Something new and scary
Your code is affected by 2 vulnerabilities.
EOF
check "non-allowlisted finding fails" 1 "$TMP/new.txt"

# An ID that appears only in prose (informational sections) must not count.
cat > "$TMP/prose.txt" <<'EOF'
This scan also found GO-2099-0002 in packages you import, but your code
doesn't appear to call it.
No vulnerabilities found.
EOF
check "IDs outside 'Vulnerability #' lines are ignored" 0 "$TMP/prose.txt"

if bash "$GATE" "$TMP/clean.txt" > /dev/null 2>&1; [ $? -eq 2 ]; then
    echo "PASS: usage error exits 2"
else
    echo "FAIL: usage error should exit 2"
    failures=$((failures + 1))
fi

# A scan that never concluded must not gate as "clean". Callers capture
# govulncheck's output with `|| true`, so a crashed scan reaches the
# gate as a report with no verdict line — historically a silent pass.
: > "$TMP/empty.txt"
check "empty report cannot gate" 2 "$TMP/empty.txt"

cat > "$TMP/crashed.txt" <<'EOF'
govulncheck: loading packages:
There are errors with the provided package patterns:

/src/pkg/plugin/plugin.go:1:1: package requires newer Go version go1.26 (application built with go1.25)
EOF
check "errored scan cannot gate" 2 "$TMP/crashed.txt"
grep -q 'did not conclude' "$TMP/out" || { echo "FAIL: missing inconclusive-scan diagnostic"; failures=$((failures + 1)); }

# The conclusiveness check must not shadow a real finding: a report that
# concluded AND lists an unaccepted vulnerability still exits 1, not 2.
cat > "$TMP/conclusive-new.txt" <<'EOF'
Vulnerability #1: GO-2099-0003
    Something new and scary
Your code is affected by 1 vulnerability from 1 module.
EOF
check "conclusive report with new finding still exits 1" 1 "$TMP/conclusive-new.txt"

if [ "$failures" -ne 0 ]; then
    echo "$failures gate test(s) failed"
    exit 1
fi
echo "All govulncheck-gate tests passed"
