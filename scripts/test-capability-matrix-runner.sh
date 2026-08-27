#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for capability-matrix-runner.sh (#690).
#
# THE CASES THAT CARRY THE WEIGHT are the two where `go test` exits 1
# without the scenario having failed: a package that will not build, and
# a -run pattern that selected nothing. The caller records exit 1 as
# "this capability is required" -- the finding the whole job exists to
# produce -- so both must come back as 2. A run that exits 1 for a reason
# other than the assertion is how a compile error becomes a discovery
# about capabilities.
#
# A SKIP is the third of those. It exits 0, which the caller would record
# as "the capability was not needed", and a scenario that skips itself
# under one capability set is precisely the shape that produces.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
SUT=$HERE/capability-matrix-runner.sh
D=$(mktemp -d); trap 'rm -rf "$D"' EXIT
mkdir -p "$D/bin"

pass=0; fail=0
eq() { if [ "$2" = "$3" ]; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (want '$3', got '$2')"; fail=$((fail+1)); fi; }
has() { if grep -F "$2" "$D/err" > /dev/null; then echo "ok   $1"; pass=$((pass+1)); else echo "FAIL $1 (wanted on stderr: $2)"; sed 's/^/    /' "$D/err"; fail=$((fail+1)); fi; }

# A `go` whose output and exit code come from the fixture named in
# GO_FIXTURE. Only the output shape matters; nothing here runs Go.
cat > "$D/bin/go" <<'EOF'
#!/usr/bin/env bash
cat "$GO_FIXTURE"
exit "${GO_RC:-0}"
EOF
chmod +x "$D/bin/go"

run() { # fixture rc -> exit code, stderr in $D/err
    GO_FIXTURE="$1" GO_RC="$2" PATH="$D/bin:$PATH" \
        bash "$SUT" some-plugin:tag TestLifecycleBridge_GoldenPath > "$D/out" 2>"$D/err"
    echo $?
}

cat > "$D/pass.txt" <<'EOF'
=== RUN   TestLifecycleBridge_GoldenPath
--- PASS: TestLifecycleBridge_GoldenPath (12.03s)
PASS
ok  	github.com/claymore666/docker-net-dhcp/test/integration	12.1s
EOF
eq "a passing scenario is 0" "$(run "$D/pass.txt" 0)" "0"

cat > "$D/fail.txt" <<'EOF'
=== RUN   TestLifecycleBridge_GoldenPath
    lifecycle_test.go:88: container never got an address
--- FAIL: TestLifecycleBridge_GoldenPath (30.02s)
FAIL
EOF
eq "a failing scenario is 1" "$(run "$D/fail.txt" 1)" "1"

# --- exit 1 without a failing assertion --------------------------------
cat > "$D/build.txt" <<'EOF'
# github.com/claymore666/docker-net-dhcp/test/integration [build failed]
test/integration/harness/plugin.go:31:2: undefined: os
FAIL	github.com/claymore666/docker-net-dhcp/test/integration [build failed]
EOF
eq  "a build failure is 2, not 1" "$(run "$D/build.txt" 1)" "2"
has "and says there is no verdict" "no verdict for TestLifecycleBridge_GoldenPath"

cat > "$D/norun.txt" <<'EOF'
testing: warning: no tests to run
PASS
ok  	github.com/claymore666/docker-net-dhcp/test/integration	0.004s [no tests to run]
EOF
eq "a pattern that selected nothing is 2, not 0" "$(run "$D/norun.txt" 0)" "2"

cat > "$D/nodaemon.txt" <<'EOF'
plugin ghcr.io/claymore666/docker-net-dhcp:capmatrix is not enabled
FAIL	github.com/claymore666/docker-net-dhcp/test/integration	0.3s
EOF
eq "a harness that could not start is 2" "$(run "$D/nodaemon.txt" 1)" "2"

cat > "$D/skip.txt" <<'EOF'
=== RUN   TestLifecycleBridge_GoldenPath
    lifecycle_test.go:40: engine does not apply DstName; see moby/moby#52866
--- SKIP: TestLifecycleBridge_GoldenPath (0.41s)
PASS
ok  	github.com/claymore666/docker-net-dhcp/test/integration	0.5s
EOF
eq  "a skip is 2, not 0" "$(run "$D/skip.txt" 0)" "2"
has "and says a skip is not a verdict" "a skip is not a verdict"

# --- usage --------------------------------------------------------------
# EXIT 2 IS NOT ENOUGH HERE. Drop the check and a one-argument call
# runs `go test -run ^$` and lands on the no-verdict path -- which also
# exits 2, so an exit-code-only assertion passes against a script that
# never noticed the missing argument. The message is what separates
# "you called this wrong" from "the scenario produced no verdict".
GO_FIXTURE="$D/pass.txt" PATH="$D/bin:$PATH" bash "$SUT" only-one-arg >/dev/null 2>"$D/err"
eq  "one argument is a usage error" "$?" "2"
has "and says so, rather than reporting no verdict" "usage:"
GO_FIXTURE="$D/pass.txt" PATH="$D/bin:$PATH" bash "$SUT" >/dev/null 2>"$D/err"
eq  "no arguments is a usage error" "$?" "2"
has "and says so too" "usage:"

# --- the pattern is anchored -------------------------------------------
# `-run TestLease` also selects TestLeaseRenewIPv6_HonorsT1. A verdict
# line for a DIFFERENT test must not be read as this one's.
cat > "$D/other.txt" <<'EOF'
=== RUN   TestLifecycleBridge_GoldenPathExtended
--- PASS: TestLifecycleBridge_GoldenPathExtended (1.0s)
ok  	github.com/claymore666/docker-net-dhcp/test/integration	1.1s
EOF
eq "another test's PASS is not this one's" "$(run "$D/other.txt" 0)" "2"

echo; echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
