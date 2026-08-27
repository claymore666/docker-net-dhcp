#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-openat-cloexec.sh (#729).
#
# The gate it exercises has no runtime observable to fall back on -- the
# descriptors it is about are closed before any caller can inspect them,
# which is why the rule is enforced by reading source at all. So this
# file is the only thing standing between that gate and quietly
# answering "clean" forever.
#
# Two properties are tested, not one:
#
#   1. the gate FINDS the thing (a flag-less open is reported), and
#   2. the gate REFUSES rather than passes when it cannot read its
#      subject -- a dot-import, an Openat2, an empty tree. A gate that
#      degrades to "clean" on input it does not understand is worse than
#      no gate, because it also carries the authority of a green check.
#
# Fixtures start as a copy of the real tree and are then broken in one
# specific way, so a fixture starts out passing for the same reasons the
# repository does and any failure is attributable to the single edit.
#
# The last section runs the whole case list against two mutants -- a
# gate replaced by `exit 0` and one replaced by `exit 1` -- and reports
# how many cases each mutant kills. A suite that no mutant can fail is
# not testing the gate, it is testing that bash runs.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
GATE="$HERE/check-openat-cloexec.sh"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# --- fixture builders ---------------------------------------------------
# Each takes a directory and leaves a tree the gate can be pointed at.

real_tree() {
    mkdir -p "$1"
    cp -r "$REPO/pkg" "$REPO/cmd" "$1/"
}

fx_pristine() { real_tree "$1"; }

fx_flag_removed() {
    real_tree "$1"
    # The exact defect #729 fixed, reintroduced: the flag word drops
    # back to a bare O_RDONLY.
    sed -i 's/unix\.O_RDONLY|unix\.O_CLOEXEC/unix.O_RDONLY/g' \
        "$1/pkg/plugin/resolvconf.go" "$1/pkg/plugin/container_netns.go"
}

fx_aliased_import() {
    mkdir -p "$1/pkg/x"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

import sysunix "golang.org/x/sys/unix"

func open(dir int) (int, error) {
	return sysunix.Openat(dir, "cgroup", sysunix.O_RDONLY, 0)
}
EOF
}

fx_aliased_import_ok() {
    fx_aliased_import "$1"
    sed -i 's/sysunix\.O_RDONLY/sysunix.O_RDONLY|sysunix.O_CLOEXEC/' "$1/pkg/x/a.go"
}

fx_wrapped_call() {
    mkdir -p "$1/pkg/x"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

import "golang.org/x/sys/unix"

func open(dir int) (int, error) {
	return unix.Openat(
		dir,
		"ns/mnt",
		unix.O_RDONLY,
		0,
	)
}
EOF
}

fx_dot_import() {
    mkdir -p "$1/pkg/x"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

import . "golang.org/x/sys/unix"

func open(dir int) (int, error) {
	return Openat(dir, "cgroup", O_RDONLY, 0)
}
EOF
}

fx_openat2() {
    mkdir -p "$1/pkg/x"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

import "golang.org/x/sys/unix"

func open(dir int) (int, error) {
	return unix.Openat2(dir, "cgroup", &unix.OpenHow{Flags: unix.O_RDONLY})
}
EOF
}

fx_syscall_pkg() {
    mkdir -p "$1/pkg/x"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

import "syscall"

func open() (int, error) {
	return syscall.Open("/etc/hosts", syscall.O_RDONLY, 0)
}
EOF
}

fx_test_file_only() {
    mkdir -p "$1/pkg/x"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

func nothing() {}
EOF
    cat > "$1/pkg/x/a_test.go" <<'EOF'
package x

import "golang.org/x/sys/unix"

func openForTest(dir int) (int, error) {
	return unix.Openat(dir, "cgroup", unix.O_RDONLY, 0)
}
EOF
}

# THE OTHER HALF OF THE FIND (#832). Every fixture above plants its
# defect under pkg/, and fx_pristine copies both trees clean -- so
# dropping `cmd` from the gate's `find` left all of them green. The
# domain could halve and this suite would not notice.
#
# pkg/ is deliberately CLEAN here. A cmd-only tree would also go red on a
# narrowed gate, but by way of the "no Go files" refusal (exit 2), which
# is a different mechanism from the one under test. With a clean pkg/ the
# narrowed gate finds files, reads them, and reports exit 0 -- the silent
# pass that is the actual failure mode.
fx_violation_only_in_cmd() {
    mkdir -p "$1/pkg/x" "$1/cmd/y"
    cat > "$1/pkg/x/a.go" <<'EOF'
package x

import "golang.org/x/sys/unix"

func open(dir int) (int, error) {
	return unix.Openat(dir, "cgroup", unix.O_RDONLY|unix.O_CLOEXEC, 0)
}
EOF
    cat > "$1/cmd/y/main.go" <<'EOF'
package main

import "golang.org/x/sys/unix"

func open(dir int) (int, error) {
	return unix.Openat(dir, "cgroup", unix.O_RDONLY, 0)
}
EOF
}

fx_no_go_files() { mkdir -p "$1/pkg" "$1/cmd"; }

fx_no_source_dirs() { mkdir -p "$1/docs"; }

# --- the case list ------------------------------------------------------
# name|builder|expected exit
CASES=(
    "the repository as it stands is clean|fx_pristine|0"
    "the #729 defect reintroduced is reported|fx_flag_removed|1"
    "an aliased unix import is still read|fx_aliased_import|1"
    "an aliased import that does pass the flag is clean|fx_aliased_import_ok|0"
    "a call broken across lines is read whole|fx_wrapped_call|1"
    "a dot-import is refused, not passed|fx_dot_import|2"
    "Openat2 is refused, not passed|fx_openat2|2"
    "syscall.Open is in scope too|fx_syscall_pkg|1"
    "a test file is out of scope|fx_test_file_only|0"
    "a tree with no Go files is refused|fx_no_go_files|2"
    "a tree with no pkg/ or cmd/ is refused|fx_no_source_dirs|2"
    "a violation under cmd/ only is still found|fx_violation_only_in_cmd|1"
)

# verdict <gate> <builder> -> exit code
verdict() {
    local gate="$1" builder="$2"
    local d
    d=$(mktemp -d "$TMP/fx.XXXXXX")
    "$builder" "$d"
    bash "$gate" "$d" >/dev/null 2>&1
    local rc=$?
    rm -rf "$d"
    echo "$rc"
}

echo "--- the gate itself ---"
for c in "${CASES[@]}"; do
    IFS='|' read -r desc builder want <<< "$c"
    got=$(verdict "$GATE" "$builder")
    if [ "$got" = "$want" ]; then
        ok "$desc"
    else
        no "$desc — want exit $want, got $got"
    fi
done

# --- ORTHOGONALITY for the cmd/ half of the domain ---------------------
# The comment above fx_violation_only_in_cmd explains why that fixture
# exists. It did not CHECK it. Its two siblings --
# test-check-dispatch-reachable.sh and test-check-plugin-bind-sources.sh --
# both reproduce the narrowed domain and assert it ACCEPTS the planted
# fixture; this suite had the paragraph and not the assertion, in a PR
# whose whole subject is a domain halving in silence.
#
# Why the acceptance assertion is the load-bearing half: "the cmd-only
# case goes red on the real gate" is satisfied by any red at all. It is
# equally consistent with the `cmd` half of the find doing the work and
# with the fixture being broken in some unrelated way. Only running the
# NARROWED gate over the same fixture separates them -- if it also goes
# red, the case below proves nothing about which half of `find pkg cmd`
# earned the verdict.
#
# The narrowing is applied to the real gate by sed, not to a copy of its
# find line pasted here: a copy drifts from the original silently and
# then tests itself.
narrowed="$TMP/narrowed-openat.sh"
sed -e "s|^FILES=\$(find pkg cmd |FILES=\$(find pkg |" "$GATE" > "$narrowed"
if ! grep -q 'find pkg -type f' "$narrowed"; then
    no "the narrowing sed did not match — the orthogonality check below would" \
       "pass vacuously against an unmodified gate"
elif [ "$(verdict "$narrowed" fx_violation_only_in_cmd)" = "0" ]; then
    ok "a pkg-only domain ACCEPTS the cmd-only violation (orthogonality confirmed)"
else
    no "the pkg-only domain did not accept the cmd-only violation, so the" \
       "'violation under cmd/ only' case above is red for some other reason"
fi
# And the control in the other direction: the same narrowing must leave a
# pkg-planted violation red. Without it, a sed that broke the gate outright
# would satisfy the acceptance assertion above by finding nothing at all.
if [ "$(verdict "$narrowed" fx_flag_removed)" = "1" ]; then
    ok "the narrowed gate still reports a pkg/ violation (it was narrowed, not broken)"
else
    no "the narrowed gate no longer reports a pkg/ violation — the sed disabled" \
       "the gate rather than narrowing its domain, and the check above is vacuous"
fi
rm -f "$narrowed"

# The two lines the issue named, by file and line, in the tree as it was
# before the fix. "It would have caught it" is a claim; this is the
# claim executed.
d=$(mktemp -d "$TMP/fx.XXXXXX")
fx_flag_removed "$d"
out=$(bash "$GATE" "$d" 2>&1)
# grep without -q, redirected: under pipefail a -q exits at the first
# match and kills the producer with SIGPIPE, so the pipeline reports
# failure on success. check-pipefail-consumers.sh caught this exact line.
if printf '%s' "$out" | grep 'pkg/plugin/resolvconf.go:' >/dev/null \
   && [ "$(printf '%s' "$out" | grep -c 'unix.Openat')" -ge 2 ]; then
    ok "the report names the offending file and line"
else
    no "the report does not name the offending call sites: $out"
fi
rm -rf "$d"

# --- mutants ------------------------------------------------------------
echo "--- mutants ---"
mutant() {
    local path="$TMP/mutant-$1.sh"
    printf '#!/usr/bin/env bash\nexit %s\n' "$1" > "$path"
    echo "$path"
}

for code in 0 1; do
    m=$(mutant "$code")
    killed=0
    for c in "${CASES[@]}"; do
        IFS='|' read -r desc builder want <<< "$c"
        [ "$(verdict "$m" "$builder")" = "$want" ] || killed=$((killed + 1))
    done
    if [ "$killed" -gt 0 ]; then
        ok "the always-exit-$code mutant is killed by $killed of ${#CASES[@]} cases"
    else
        no "the always-exit-$code mutant survives every case — this suite proves nothing"
    fi
done

echo
if [ "$fail" -gt 0 ]; then
    echo "check-openat-cloexec meta-test: $pass passed, $fail FAILED" >&2
    exit 1
fi
echo "check-openat-cloexec meta-test: $pass passed, 0 failed"
