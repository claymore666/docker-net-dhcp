#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-pseudo-version-pin.sh (#1228).
#
# `gh` is a stub earlier on PATH. It answers the compare API by the commit
# it is asked about and logs every call, so each case asserts both the
# verdict and whether the network was consulted at all: a draft, a main or
# a tag target must never reach it.
# The V_* names are read through ${!v}, which shellcheck cannot follow.
# shellcheck disable=SC2034
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"
CHECK="$HERE/check-pseudo-version-pin.sh"
guarded_tmpdir TMP

failures=0
STUB="$TMP/bin"
mkdir -p "$STUB"
LOG="$TMP/gh.log"
cat > "$STUB/gh" <<'STUBEOF'
#!/usr/bin/env bash
# args: api repos/<repo>/compare/<branch>...<sha>?per_page=1 --jq .status
printf '%s\n' "$*" >> "$GH_STUB_LOG"
ep="${2:-}"
sha="${ep#*...}"; sha="${sha%%\?*}"
branch="${ep#*/compare/}"; branch="${branch%%...*}"
case "$sha" in
  aaaaaaaaaaaa) [ "$branch" = dev ] && echo identical || echo behind ;;
  bbbbbbbbbbbb) [ "$branch" = dev ] && echo ahead || echo behind ;;
  cccccccccccc) echo diverged ;;
  222222222222) echo ahead ;;
  dddddddddddd) echo '{"message":"Not Found"}'; echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
  eeeeeeeeeeee) echo 'gh: API rate limit exceeded (HTTP 403)' >&2; exit 1 ;;
  ffffffffffff) echo oops ;;
  111111111111) : ;;
  444444444444)
    n=$(cat "$GH_STUB_LOG.n" 2>/dev/null || echo 0); echo $((n+1)) > "$GH_STUB_LOG.n"
    if [ "$n" -lt 1 ]; then echo 'gh: Bad Gateway (HTTP 502)' >&2; exit 1; fi
    echo identical ;;
  *) echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
esac
STUBEOF
chmod +x "$STUB/gh"

LIB=github.com/claymore666/dhcp-golib
TS=20260101000000
HDR=$'module example.com/m\n\ngo 1.21\n\n'

# mkrepo <dir> <go.mod body after the header>
mkrepo() {
    mkdir -p "$1"
    git -C "$1" init -q
    printf '%s%s\n' "$HDR" "$2" > "$1/go.mod"
}

# run NAME WANT_EXIT WANT_GREP WANT_CALLS BODY EVENT TARGET [DRAFT]
# WANT_CALLS is the number of compare calls the stub must have seen, or -
run() {
    local name="$1" want="$2" grep_for="$3" calls="$4" body="$5" ev="$6" tg="$7" dr="${8-}"
    local d="$TMP/r.$RANDOM$RANDOM"
    mkrepo "$d" "$body"
    : > "$LOG"; rm -f "$LOG.n"
    local args=(--event "$ev" --target "$tg" --tree "$d")
    [ "$#" -ge 8 ] && args+=(--draft "$dr")
    PATH="$STUB:$PATH" GH_STUB_LOG="$LOG" PIN_RETRY_SLEEP=0 bash "$CHECK" "${args[@]}" > "$TMP/out" 2>&1
    local got=$? ok=1 n
    n=$(wc -l < "$LOG")
    [ "$got" -eq "$want" ] || ok=0
    [ -z "$grep_for" ] || command grep -qF -- "$grep_for" "$TMP/out" || ok=0
    [ "$calls" = - ] || [ "$n" -eq "$calls" ] || ok=0
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want / '$grep_for' / $calls calls, got exit $got / $n calls)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

req() { printf 'require %s %s\n' "$1" "$2"; }
V_A="v1.4.1-0.$TS-aaaaaaaaaaaa"
V_B="v0.0.0-$TS-aaaaaaaaaaaa"
V_C="v1.4.1-pre.0.$TS-aaaaaaaaaaaa"
V_LOOSE="v1.4.1-rc.$TS-aaaaaaaaaaaa"
V_INC="v28.5.3-0.$TS-aaaaaaaaaaaa+incompatible"
DOCKER=github.com/docker/docker

# --- preservation: everything legitimate still passes ------------------
tagged=$(req $LIB v1.4.0; printf 'require %s v28.5.2+incompatible\nrequire golang.org/x/sys v0.48.0 // indirect\n' $DOCKER)
for t in dev main v2.5.0; do
    run "tagged versions, +incompatible and // indirect pass into $t" 0 "no refused pin" 0 "$tagged" push "$t"
done
run "a tagged prerelease passes into main" 0 "" 0 "$(req $LIB v1.5.0-rc1)" push main
run "a comment that mentions a pseudo-version is not a pin" 0 "" 0 \
    "// pinned to $V_A while testing
$(req $LIB v1.4.0) // was $V_A" push main
run "a same-path replace to a tagged version passes" 0 "" 0 \
    "$(req $LIB v1.4.0)
replace $LIB v1.4.0 => $LIB v1.4.2" push main
run "a pseudo-version on the replaced (old) side alone pins nothing" 0 "" 0 \
    "$(req $LIB v1.4.0)
replace $LIB $V_A => $LIB v1.4.2" push main

# --- main and release tags: any pseudo-version is red, no network ------
for form in A B C LOOSE; do
    v="V_$form"
    run "main refuses the library at form $form" 1 "only a tagged release may reach 'main'" 0 "$(req $LIB "${!v}")" push main
    run "a release tag refuses the library at form $form" 1 "only a tagged release may reach 'v2.5.0'" 0 "$(req $LIB "${!v}")" push v2.5.0
done
run "main refuses a pseudo-version whose hash is not 12 hex" 1 "only a tagged release may reach 'main'" 0 "$(req $LIB "v0.0.0-$TS-abcdef")" push main
run "dev refuses a pseudo-version whose hash is not 12 hex" 1 "no commit hash could be read" 0 "$(req $LIB "v0.0.0-$TS-abcdef")" push dev
run "main refuses +incompatible pseudo-version" 1 "pseudo-version $V_INC" 0 "$(req $DOCKER "$V_INC")" push main
run "main refuses a pull request carrying one" 1 "" 0 "$(req $LIB "$V_A")" pull_request main false
run "main refuses an // indirect pseudo-version" 1 "" 0 "$(req golang.org/x/sys "$V_A") // indirect" push main
run "main refuses a pseudo-version inside a require block" 1 "" 0 \
    "require (
	$LIB v1.4.0
	example.org/x $V_A // indirect
)" push main
run "main refuses a quoted require" 1 "" 0 "require \"$LIB\" \"$V_A\"" push main
run "main refuses a same-path replace to a pseudo-version" 1 "" 0 \
    "$(req $LIB v1.4.0)
replace $LIB => $LIB $V_A" push main
run "a schedule run on main judges the tree like a push" 1 "" 0 "$(req $LIB "$V_A")" schedule main

# --- dev: the library's pin is judged by reachability ------------------
for form in A B C; do
    v="V_$form"
    run "dev accepts the library form $form reachable from library dev" 0 "is in claymore666/dhcp-golib dev" 1 "$(req $LIB "${!v}")" push dev
done
run "dev accepts a PR whose pin is reachable" 0 "" 1 "$(req $LIB "$V_A")" pull_request dev false
run "dev accepts a pin reachable only from library main" 0 "is in claymore666/dhcp-golib main" 2 "$(req $LIB "v1.4.1-0.$TS-bbbbbbbbbbbb")" push dev
run "dev accepts an // indirect library pin that is reachable" 0 "" 1 "$(req $LIB "$V_A") // indirect" push dev
run "dev accepts a library v2 module path" 0 "" 1 "$(req $LIB/v2 "v2.0.0-$TS-aaaaaaaaaaaa")" push dev
run "dev refuses a commit only on a feature branch" 1 "is on no claymore666/dhcp-golib dev or main" 2 "$(req $LIB "v1.4.1-0.$TS-cccccccccccc")" push dev
run "dev refuses a commit that is ahead of both branches" 1 "is on no" 2 "$(req $LIB "v1.4.1-0.$TS-222222222222")" push dev
run "dev refuses a commit the library does not have" 1 "has no commit dddddddddddd" 1 "$(req $LIB "v1.4.1-0.$TS-dddddddddddd")" push dev
run "dev: a rate-limited read is cannot-judge, never reachable" 2 "never treated as reachable" 3 "$(req $LIB "v1.4.1-0.$TS-eeeeeeeeeeee")" push dev
run "dev: an unknown status word is cannot-judge" 2 "unexpected status 'oops'" 3 "$(req $LIB "v1.4.1-0.$TS-ffffffffffff")" push dev
run "dev: an empty answer is cannot-judge" 2 "never treated as reachable" 3 "$(req $LIB "v1.4.1-0.$TS-111111111111")" push dev
run "dev: one transient failure is retried" 0 "" 2 "$(req $LIB "v1.4.1-0.$TS-444444444444")" push dev
run "dev refuses another module's pseudo-version without asking the API" 1 "not the library" 0 "$(req $DOCKER "$V_INC")" push dev
run "dev refuses a look-alike library name" 1 "not the library" 0 "$(req "$LIB-fork" "$V_A")" push dev
run "dev refuses a non-canonical pseudo-version form" 1 "not the library" 0 "$(req example.org/x "$V_LOOSE")" push dev

# --- replace to a path or a fork is red at every target ----------------
for t in dev main v2.5.0; do
    run "$t refuses a replace to a local path" 1 "points at a local path" 0 \
        "$(req $LIB v1.4.0)
replace $LIB => ../dhcp-golib" push "$t"
    run "$t refuses a replace to a fork" 1 "points at another module" 0 \
        "$(req $LIB v1.4.0)
replace $LIB => github.com/someone/dhcp-golib v1.4.0" push "$t"
done

# --- draft handling ----------------------------------------------------
BAD="$(req $LIB "$V_A")
replace example.org/x => ../x"
run "a draft PR into main is skipped with the reason printed" 0 "this is a draft pull request" 0 "$BAD" pull_request main true
run "a draft PR into dev is skipped" 0 "marked ready for review" 0 "$BAD" pull_request dev true
for v in false "" True TRUE 1 yes; do
    run "a pull request with draft='$v' is judged" 1 "" - "$BAD" pull_request dev "$v"
done
run "a PR with no --draft at all is judged" 1 "" - "$BAD" pull_request main
run "draft=true on a push never skips" 1 "" - "$BAD" push main true
run "draft=true on a schedule run never skips" 1 "" - "$BAD" schedule main true

# --- which files are read ----------------------------------------------
d="$TMP/nested"; mkrepo "$d" "$(req $LIB v1.4.0)"
mkdir -p "$d/sub" "$d/pkg/testdata"
printf 'module example.com/m/sub\n\ngo 1.21\n\nrequire %s %s\n' $LIB "$V_A" > "$d/sub/go.mod"
printf 'module fixture\n\ngo 1.21\n\nrequire %s %s\n' $LIB "$V_A" > "$d/pkg/testdata/go.mod"
printf 'go 1.21\n\nuse .\n\nreplace %s => ../elsewhere\n' $LIB > "$d/go.work"
git -C "$d" add -A
want_in() {
    local name="$1" want="$2" grep_for="$3"; shift 3
    PATH="$STUB:$PATH" GH_STUB_LOG="$LOG" PIN_RETRY_SLEEP=0 bash "$CHECK" "$@" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want" ] && { [ -z "$grep_for" ] || command grep -qF -- "$grep_for" "$TMP/out"; }; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want / '$grep_for', got exit $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
    fi
}
want_in "a nested module's pin is found" 1 "file=sub/go.mod" --event push --target main --tree "$d"
want_in "a committed go.work replace is found" 1 "file=go.work" --event push --target main --tree "$d"
git -C "$d" rm -qrf --cached sub go.work
rm -rf "$d/sub" "$d/go.work"
want_in "a go.mod under testdata is not read" 0 "1 module file(s)" --event push --target main --tree "$d"

# --- --ref reads the object database, not the work tree -----------------
GITC=(git -c user.email=t@t -c user.name=t)
d="$TMP/byref"; mkrepo "$d" "$(req $LIB "$V_A")"
"${GITC[@]}" -C "$d" add -A; "${GITC[@]}" -C "$d" commit -qm pinned
printf '%s%s\n' "$HDR" "$(req $LIB v1.4.0)" > "$d/go.mod"
want_in "--ref judges the committed go.mod, not the edited work tree" 1 "only a tagged release may reach 'v2.5.0'" \
    --event push --target v2.5.0 --tree "$d" --ref HEAD
want_in "without --ref the edited work tree is what is judged" 0 "no refused pin" --event push --target v2.5.0 --tree "$d"
"${GITC[@]}" -C "$d" tag -a -m t v9 HEAD
want_in "--ref takes a tag name" 1 "pseudo-version" --event push --target v9 --tree "$d" --ref refs/tags/v9
want_in "--ref to nothing is cannot-judge" 2 "names no revision" --event push --target main --tree "$d" --ref refs/heads/nope
d="$TMP/byref2"; mkrepo "$d" "$(req $LIB v1.4.0)"
mkdir -p "$d/pkg/testdata"; printf '%smodule f\n%s\n' "" "$(req $LIB "$V_A")" > "$d/pkg/testdata/go.mod"
"${GITC[@]}" -C "$d" add -A; "${GITC[@]}" -C "$d" commit -qm fixture
want_in "--ref skips a go.mod under testdata" 0 "1 module file(s)" --event push --target main --tree "$d" --ref HEAD
"${GITC[@]}" -C "$d" rm -qf go.mod; "${GITC[@]}" -C "$d" commit -qm nomod
want_in "--ref to a commit with no go.mod is cannot-judge" 2 "no go.mod" --event push --target main --tree "$d" --ref HEAD

# --- cannot judge ------------------------------------------------------
mkdir -p "$TMP/empty-git"; git -C "$TMP/empty-git" init -q
want_in "a repository with no go.mod is cannot-judge, not a pass" 2 "no go.mod" --event push --target main --tree "$TMP/empty-git"
mkdir -p "$TMP/nogit"; printf 'module x\n' > "$TMP/nogit/go.mod"
want_in "a directory outside git is cannot-judge" 2 "not in a git work tree" --event push --target main --tree "$TMP/nogit"
run "a malformed go.mod is cannot-judge" 2 "failed on go.mod" 0 'require (' push main
run "a branch name as a version is cannot-judge" 2 "failed on go.mod" 0 "require $LIB master" push main
want_in "no arguments is cannot-judge" 2 "--event is required"
want_in "no target is cannot-judge" 2 "--target is required" --event push
want_in "an unknown flag is cannot-judge" 2 "unknown argument" --event push --target main --bogus
want_in "a flag without a value is cannot-judge" 2 "wants a value" --event

# --- wiring: the workflow must hand the gate what it judges on ---------
WF="$HERE/../.github/workflows/test.yaml"
if command grep -qE '^    types: \[.*ready_for_review.*\]' "$WF"; then
    echo "PASS: test.yaml re-runs on ready_for_review"
else
    echo "FAIL: test.yaml pull_request types lack ready_for_review; the draft skip would never be re-judged"
    failures=$((failures + 1))
fi
# The step is compared with the expected text byte for byte (#1228): the
# target is the PR's base branch, not its head; the draft flag is the live
# API answer; nothing may follow the read that rewrites it; no trap or
# option may turn a refusal into a pass. Any edit to a line, including one
# that lints and passes every case above, is red. A deliberate change to
# the step is made here in the same commit. The step ends at its first
# blank line.
step=$(awk '/- name: Refuse a pseudo-version pin/{f=1} f&&/^$/{exit} f{print}' "$WF")
expected=$(cat <<'PIN_STEP'
      - name: Refuse a pseudo-version pin outside a draft pull request
        env:
          EVENT: ${{ github.event_name }}
          TARGET: ${{ github.event.pull_request.base.ref || github.ref_name }}
          PR_NUMBER: ${{ github.event.pull_request.number }}
          REPO: ${{ github.repository }}
          GH_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail
          draft=''
          if [ "$EVENT" = pull_request ]; then
            draft="$(gh api "repos/$REPO/pulls/$PR_NUMBER" --jq .draft)" || {
              echo "::error title=Draft flag unreadable::could not read the draft flag of pull request $PR_NUMBER; the pin gate will not guess." >&2
              exit 2
            }
          fi
          bash scripts/check-pseudo-version-pin.sh \
            --event "$EVENT" --target "$TARGET" --draft "$draft"
PIN_STEP
)
if [ -z "$step" ]; then
    echo "FAIL: test.yaml no longer has the pin step"; failures=$((failures + 1))
elif [ "$step" = "$expected" ]; then
    echo "PASS: the pin step is exactly the judged text ($(wc -l <<< "$step") lines)"
else
    echo "FAIL: the pin step differs from the judged text; judge the change, then update this test:"
    diff <(printf '%s\n' "$expected") <(printf '%s\n' "$step") | sed 's/^/    /' || true
    failures=$((failures + 1))
fi

REL="$HERE/../.github/workflows/release.yml"
rstep=$(awk '/- name: Refuse a tag that pins a library commit/{f=1} f{print} f&&/--tree .resolver/{exit}' "$REL")
if [ -z "$rstep" ]; then
    echo "FAIL: release.yml no longer runs the pin gate on the tag; an rc cut from dev could ship a pin"
    failures=$((failures + 1))
else
    if command grep -qF -- '--ref refs/pin-under-judgement' <<< "$rstep" \
        && command grep -qF -- 'bash .resolver/scripts/check-pseudo-version-pin.sh' <<< "$rstep" \
        && command grep -qF -- '--target "$TAG"' <<< "$rstep" \
        && command grep -qF -- '--event "$GITHUB_EVENT_NAME"' <<< "$rstep" \
        && ! command grep -qE '^\s+(if|continue-on-error):' <<< "$rstep"; then
        echo "PASS: release.yml judges the tag's go.mod from the object database, unconditionally"
    else
        echo "FAIL: the release pin step is conditional, or reads something other than the fetched tag"; failures=$((failures + 1))
    fi
fi
rjob=$(awk '/^  resolve:/{f=1;next} /^  [a-z-]+:$/{f=0} f' "$REL")
if command grep -qE '^\s+scripts/check-pseudo-version-pin\.sh$' <<< "$rjob" && command grep -qE '^\s+scripts/gatelib\.sh$' <<< "$rjob" \
   && command grep -qF 'Refuse a tag that pins a library commit' <<< "$rjob"; then
    echo "PASS: the pin step lives in resolve, with its two scripts in the sparse checkout"
else
    echo "FAIL: the pin step or its sparse-checkout paths are missing from release.yml's resolve job"
    failures=$((failures + 1))
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "failed: $failures"
    exit 1
fi
echo "all check-pseudo-version-pin.sh tests passed"
