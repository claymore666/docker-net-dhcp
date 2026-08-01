#!/usr/bin/env bash
# Meta-test for integration-run-gate.sh (#311, #312). The failure mode
# these guard against is the gate going too wide: a skip decision on a
# diff or tree that actually needs the suite. Every ambiguous case must
# come out "run" (fail-open).
set -euo pipefail

GATE="$(dirname "$0")/integration-run-gate.sh"
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

# --- classify mode (pure path classifier, #311) ---

got=$(printf 'README.md\ndocs/reference.md\n' | bash "$GATE" classify && echo docs-only || echo not)
check "md-only diff classifies docs-only" docs-only "$got"

got=$(printf 'README.md\npkg/plugin/state.go\n' | bash "$GATE" classify && echo docs-only || echo not)
check "mixed md+go diff is NOT docs-only" not "$got"

got=$(printf 'scripts/coverage-ratchet.sh\n' | bash "$GATE" classify && echo docs-only || echo not)
check "script-only diff is NOT docs-only" not "$got"

got=$(printf '.github/workflows/integration.yml\n' | bash "$GATE" classify && echo docs-only || echo not)
check "workflow-only diff is NOT docs-only" not "$got"

got=$(printf '' | bash "$GATE" classify && echo docs-only || echo not)
check "empty diff is NOT docs-only" not "$got"

got=$(printf 'README.mdx\n' | bash "$GATE" classify && echo docs-only || echo not)
check ".mdx does not sneak past the .md match" not "$got"

# --- pr / push modes with a stubbed curl ---
# The gate talks to the API with curl + jq (the runner image has no gh
# CLI). The stub keys off the request URL in "$*"; real jq parses the
# canned JSON, so the stub output must be valid API-shaped JSON.

STUB_DIR=$(mktemp -d)
trap 'rm -rf "$STUB_DIR"' EXIT
export GATE_REPO="owner/repo"

make_curl() { # $1 = stub body
    printf '#!/usr/bin/env bash\n%s\n' "$1" > "$STUB_DIR/curl"
    chmod +x "$STUB_DIR/curl"
}

# pr mode: stub returns only md files -> skip
make_curl 'printf "[{\"filename\":\"README.md\"},{\"filename\":\"docs/index.md\"}]"'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode skips a docs-only PR" skip "$got"

# pr mode: stub returns a go file among md -> run
make_curl 'printf "[{\"filename\":\"README.md\"},{\"filename\":\"main.go\"}]"'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode runs a mixed PR" run "$got"

# pr mode: curl fails -> run (fail-open)
make_curl 'exit 22'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode fails open on API error" run "$got"

# pr mode: a FULL first page (100 md files) must not be judged alone —
# page 2 carries a go file, so the verdict is run. Guards the
# truncated-view bug class. (Match on "&page=N" — a bare "page=1" also
# matches inside "per_page=100".)
make_curl 'case "$*" in
  *"&page=1"*) jq -n "[range(100) | {filename: (\"doc\" + tostring + \".md\")}]" ;;
  *"&page=2"*) printf "[{\"filename\":\"main.go\"}]" ;;
  *) exit 22 ;;
esac'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode pages past a full page before judging" run "$got"

# ...and a short second page of md keeps the skip.
make_curl 'case "$*" in
  *"&page=1"*) jq -n "[range(100) | {filename: (\"doc\" + tostring + \".md\")}]" ;;
  *"&page=2"*) printf "[{\"filename\":\"last.md\"}]" ;;
  *) exit 22 ;;
esac'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode still skips a paginated all-md diff" skip "$got"

# push mode: one prior run whose commit has a MATCHING tree -> skip
make_curl 'case "$*" in
  *actions/workflows*) printf "{\"workflow_runs\":[{\"head_sha\":\"cafe1234\"}]}" ;;
  *commits/cafe1234*) printf "{\"commit\":{\"tree\":{\"sha\":\"deadbeeftree\"}}}" ;;
  *) exit 22 ;;
esac'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push deadbeeftree 2>/dev/null)
check "push mode skips an already-passed tree" skip "$got"

# push mode: prior run tree DIFFERS -> run (the semantic-conflict case)
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push othertree000 2>/dev/null)
check "push mode runs a novel tree" run "$got"

# push mode: runs list fails -> run (fail-open)
make_curl 'exit 22'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push deadbeeftree 2>/dev/null)
check "push mode fails open on API error" run "$got"

# push mode: per-commit lookup fails for the only candidate -> run
make_curl 'case "$*" in
  *actions/workflows*) printf "{\"workflow_runs\":[{\"head_sha\":\"cafe1234\"}]}" ;;
  *) exit 22 ;;
esac'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push deadbeeftree 2>/dev/null)
check "push mode fails open when commit lookup fails" run "$got"

# missing tools: no curl/jq anywhere on PATH -> run (the gh-CLI lesson:
# a runner without the dependency must fail open, not fail silent).
# bash itself is resolved before PATH is emptied.
BASH_BIN=$(command -v bash)
got=$(PATH="" "$BASH_BIN" "$GATE" pr 99 2>/dev/null)
check "pr mode fails open without curl/jq on PATH" run "$got"
got=$(PATH="" "$BASH_BIN" "$GATE" push deadbeeftree 2>/dev/null)
check "push mode fails open without curl/jq on PATH" run "$got"

# dispatch mode: never skippable (#419). A manual run is a request for
# fresh evidence about a tree that has NOT changed — three consecutive
# green runs is this project's bar — so the duplicate-tree skip would
# answer the wrong question, and answer it green in ~13 seconds having
# executed nothing.
got=$(bash "$GATE" dispatch 2>/dev/null)
check "dispatch mode always runs" run "$got"

# ...and it must not depend on the API or on curl/jq being present:
# there is nothing to look up, so nothing can make it skip.
got=$(PATH="" "$BASH_BIN" "$GATE" dispatch 2>/dev/null)
check "dispatch mode runs even without curl/jq" run "$got"

# The counterpart — push mode still taking the duplicate-tree skip — is
# covered by "push mode skips an already-passed tree" above.

# unknown mode / missing args -> run
got=$(bash "$GATE" bogus 2>/dev/null)
check "unknown mode falls back to run" run "$got"
got=$(bash "$GATE" pr 2>/dev/null)
check "pr mode without number falls back to run" run "$got"

if [ "$fails" -ne 0 ]; then
    echo "integration-run-gate tests FAILED"
    exit 1
fi
echo "All integration-run-gate tests passed"
