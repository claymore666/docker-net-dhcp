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

# --- pr / push modes with a stubbed gh ---

STUB_DIR=$(mktemp -d)
trap 'rm -rf "$STUB_DIR"' EXIT
export GATE_REPO="owner/repo"

make_gh() { # $1 = stub body
    printf '#!/usr/bin/env bash\n%s\n' "$1" > "$STUB_DIR/gh"
    chmod +x "$STUB_DIR/gh"
}

# pr mode: stub returns only md files -> skip
make_gh 'printf "README.md\ndocs/index.md\n"'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode skips a docs-only PR" skip "$got"

# pr mode: stub returns a go file among md -> run
make_gh 'printf "README.md\nmain.go\n"'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode runs a mixed PR" run "$got"

# pr mode: gh fails -> run (fail-open)
make_gh 'exit 1'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" pr 99 2>/dev/null)
check "pr mode fails open on API error" run "$got"

# push mode: one prior run whose commit has a MATCHING tree -> skip
make_gh 'case "$*" in
  *actions/workflows*) printf "cafe1234\n" ;;
  *commits/cafe1234*) printf "deadbeeftree\n" ;;
  *) exit 1 ;;
esac'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push deadbeeftree 2>/dev/null)
check "push mode skips an already-passed tree" skip "$got"

# push mode: prior run tree DIFFERS -> run (the semantic-conflict case)
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push othertree000 2>/dev/null)
check "push mode runs a novel tree" run "$got"

# push mode: runs list fails -> run (fail-open)
make_gh 'exit 1'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push deadbeeftree 2>/dev/null)
check "push mode fails open on API error" run "$got"

# push mode: per-commit lookup fails for the only candidate -> run
make_gh 'case "$*" in
  *actions/workflows*) printf "cafe1234\n" ;;
  *) exit 1 ;;
esac'
got=$(PATH="$STUB_DIR:$PATH" bash "$GATE" push deadbeeftree 2>/dev/null)
check "push mode fails open when commit lookup fails" run "$got"

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
