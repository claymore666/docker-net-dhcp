#!/usr/bin/env bash
# Table-driven tests for check-option-docs.sh (#132). Synthesizes a Go
# package with a DHCPNetworkOptions struct + endpoint-opt parsing and a
# reference doc, asserting: full documentation passes, a missing tagged
# key / untagged field / endpoint opt each fail, test files are
# ignored, and a vanished struct is an explicit failure.
set -u

CHECK="$(dirname "$0")/check-option-docs.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PKG="$TMP/pkg"
mkdir -p "$PKG"

cat > "$PKG/plugin.go" <<'EOF'
package plugin

// DHCPNetworkOptions mirrors the real struct's shapes: tagged fields,
// untagged fields (mapstructure matches the lowercased name), and
// comment lines inside the struct body.
type DHCPNetworkOptions struct {
	Mode   string `mapstructure:"mode"`
	Bridge string
	// LeaseTimeout has a comment line above it.
	LeaseTimeout time.Duration `mapstructure:"lease_timeout"`
	AuditLog     bool          `mapstructure:"audit_log"`
}
EOF

cat > "$PKG/network.go" <<'EOF'
package plugin

func parseDriverOptIP(options map[string]interface{}) {
	_ = options["ip"]
}
EOF

# Option keys referenced only in tests must NOT be required.
cat > "$PKG/network_test.go" <<'EOF'
package plugin

func TestSomething(t *testing.T) {
	_ = map[string]interface{}{}["only_in_tests"]
}
EOF

DOC="$TMP/reference.md"
all_documented() {
    cat > "$DOC" <<'EOF'
| `mode` | the mode |
| `bridge` | the bridge |
| `lease_timeout` | the timeout |
| `audit_log` | the ledger |
| `ip` | endpoint static IP |
EOF
}

failures=0
check() {
    local name="$1" want_exit="$2"
    bash "$CHECK" "$PKG" "$DOC" > "$TMP/out" 2>&1
    local got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# 1. Everything documented -> green.
all_documented
check "all keys documented passes" 0

# 2. Missing tagged key -> red, named in output.
all_documented && sed -i '/audit_log/d' "$DOC"
check "missing tagged key fails" 1
grep -q 'FAIL  audit_log' "$TMP/out" || { echo "FAIL: missing key not named"; failures=$((failures + 1)); }

# 3. Missing untagged field (lowercased name) -> red.
all_documented && sed -i '/bridge/d' "$DOC"
check "missing untagged field fails" 1

# 4. Missing endpoint driver-opt -> red.
# shellcheck disable=SC2016  # literal backticks in the sed pattern
all_documented && sed -i '/`ip`/d' "$DOC"
check "missing endpoint opt fails" 1

# 5. Keys only in _test.go files are not required.
all_documented
check "test-only keys ignored" 0

# 6. Struct vanished (renamed/moved) -> explicit failure, not silent green.
all_documented
mv "$PKG/plugin.go" "$PKG/plugin.go.bak"
check "vanished struct fails" 1
mv "$PKG/plugin.go.bak" "$PKG/plugin.go"

# 7. Bad usage -> exit 2.
bash "$CHECK" "$TMP/nonexistent" "$DOC" > "$TMP/out" 2>&1
if [ $? -eq 2 ]; then echo "PASS: bad usage exits 2"; else echo "FAIL: bad usage"; failures=$((failures + 1)); fi

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed"
    exit 1
fi
echo "all check-option-docs tests passed"
