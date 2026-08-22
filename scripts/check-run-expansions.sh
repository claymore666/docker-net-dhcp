#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that no attacker-influenced expression is expanded into a
# `run:` body (#737).
#
# THE FAILURE THIS PREVENTS. `${{ ... }}` is substituted into the step
# script BEFORE bash parses it. A value carrying a quote and a semicolon
# therefore becomes commands, and validating it on the next line is too
# late — the shell has already run whatever the value said. Passing the
# same value through `env:` makes it data: bash sees a variable, and the
# validation that follows is validating the thing that will actually be
# used.
#
# WHY IT IS WORTH A GATE HERE. The tree already does this correctly in
# coverage-presence.yml, pages.yml, release-backmerge.yml and throughout
# test.yaml, with the reasoning written out — and still had it wrong in
# release.yml, the one workflow holding `id-token: write`. Code running
# in that job can sign an arbitrary artifact as this repository's OIDC
# identity. The rule was known, written down, and not enforced, so the
# one place it mattered most drifted. That is the exact shape this
# project keeps getting bitten by, which is why it is checked instead of
# remembered.
#
# It also caught a second class: the dispatch-ref GUARD job in
# integration.yml expanded the very input it exists to validate. A guard
# subvertible through its own argument protects nothing downstream of
# it.
#
# SECRETS ARE CHECKED TOO, for a different reason. `${{ secrets.X }}`
# inside a run body writes the secret into the step script on disk,
# where anything sharing the runner can read it and where it lands in
# any trace of the command. The correct form is the same one — hand it
# to the step through `env:`, or, for a registry, use the pinned login
# action. release.yml used docker/login-action correctly for GHCR in one
# job and piped the raw token into `docker login` in two others, which
# is how this stayed invisible: the right pattern was present in the
# same file.
#
# WHAT COUNTS AS ATTACKER-INFLUENCED. Dispatch inputs, the head ref, and
# the free-text fields of an event payload — titles, bodies, names,
# emails, refs, labels, commit messages. Fixed enums and integers
# (`github.event_name`, `github.event.pull_request.number`) are not
# flagged: they cannot carry shell syntax, and flagging them would make
# the check demand pointless churn and get switched off.
#
# WHAT IT DOES NOT CLAIM. It reads workflow text. It cannot tell whether
# a value routed through `env:` is then used safely — `eval "$VAR"` is
# just as bad and looks fine here. It answers one question: did the
# untrusted value reach the shell as text or as data.
#
# Usage: bash scripts/check-run-expansions.sh [workflow-dir]
# Exit:  0 no untrusted value and no secret is expanded into a run body
#        1 at least one is
#        2 the check could not run (missing dir, nothing discovered)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
DIR="${1:-$ROOT/.github/workflows}"

if [ ! -d "$DIR" ]; then
    echo "::error title=Workflow directory missing::$DIR is not a directory" >&2
    exit 2
fi

shopt -s nullglob
files=("$DIR"/*.yml "$DIR"/*.yaml)
if [ "${#files[@]}" -eq 0 ]; then
    echo "::error title=No workflows found::$DIR matched no *.yml or *.yaml." \
         "This check would otherwise pass having examined nothing." >&2
    exit 2
fi

# Every dispatch input, the head ref, and any event field whose value is
# free text a stranger can write. Extended regex, matched against the
# whole expression with the surrounding `${{ }}` already stripped.
UNTRUSTED='^(inputs\.|github\.event\.inputs\.|github\.head_ref$|github\.event\..*\.(title|body|name|email|ref|label|message|description|page_name|login)$)'

# Materialising a secret into the step script is a disclosure problem
# rather than an injection one, so it is reported separately — the fix
# is the same shape, but the sentence explaining why is not.
SECRET='^secrets\.'

# Emit `file:line:expression` for every expansion inside a `run:` body.
#
# Both run shapes are handled: the block form (`run: |`) whose body is
# every following line indented deeper, and the inline form
# (`run: something`) which is its own body. Comment lines are skipped —
# these workflows explain this very rule in prose that quotes the unsafe
# form, and a check that read its own documentation would never go
# green.
scan_file() {
    awk '
    function emit(line, n, i, s, expr) {
        s = line
        while (match(s, /\$\{\{[[:space:]]*[^}]*\}\}/)) {
            expr = substr(s, RSTART, RLENGTH)
            sub(/^\$\{\{[[:space:]]*/, "", expr)
            sub(/[[:space:]]*\}\}$/, "", expr)
            printf "%d\t%s\n", FNR, expr
            s = substr(s, RSTART + RLENGTH)
        }
    }
    {
        indent = match($0, /[^ ]/) - 1
        if (indent < 0) indent = 9999
        stripped = $0
        sub(/^[[:space:]]+/, "", stripped)

        if (in_run) {
            if (stripped != "" && indent <= run_indent) {
                in_run = 0
            } else {
                if (stripped !~ /^#/) emit($0)
                next
            }
        }
        if (stripped ~ /^#/) next
        # Block form: the body is everything indented deeper.
        if (stripped ~ /^-?[[:space:]]*run:[[:space:]]*[|>]/) {
            print "0\t!run"
            in_run = 1; run_indent = indent; next
        }
        # Inline form: this line IS the body.
        if (stripped ~ /^-?[[:space:]]*run:[[:space:]]*[^|>[:space:]]/) {
            print "0\t!run"
            emit($0)
        }
    }
    ' "$1"
}

findings=()
secret_findings=()
examined=0
expansions=0
run_bodies=0

for f in "${files[@]}"; do
    examined=$((examined + 1))
    while IFS=$'\t' read -r line expr; do
        [ -n "${expr:-}" ] || continue
        if [ "$expr" = "!run" ]; then
            run_bodies=$((run_bodies + 1))
            continue
        fi
        expansions=$((expansions + 1))
        if [[ "$expr" =~ $UNTRUSTED ]]; then
            findings+=("$(basename "$f")	$line	$expr")
        elif [[ "$expr" =~ $SECRET ]]; then
            secret_findings+=("$(basename "$f")	$line	$expr")
        fi
    done < <(scan_file "$f")
done

# The sentinel is the number of run bodies PARSED, not the number of
# expansions found in them. A clean tree legitimately has zero of the
# latter; zero of the former means the parser stopped matching and the
# check is about to report green having read nothing. Keyed on
# expansions, this exited 2 on a workflow that was simply correct.
if [ "$run_bodies" -eq 0 ]; then
    echo "::error title=No run bodies parsed::examined $examined workflow file(s)" \
         "and found no 'run:' step at all. A workflow directory with no shell in" \
         "it is not something this check can vouch for, so it refuses rather" \
         "than passing having examined nothing." >&2
    exit 2
fi

if [ "${#secret_findings[@]}" -ne 0 ]; then
    echo "::error title=Secret expanded into a shell::the following secrets are" \
         "written into a step script rather than passed through env (#737):" >&2
    for entry in "${secret_findings[@]}"; do
        IFS=$'\t' read -r file line expr <<<"$entry"
        printf '  %s:%s — ${{ %s }}\n' "$file" "$line" "$expr" >&2
    done
    echo >&2
    echo "The value ends up in the generated script on the runner's disk and in" >&2
    echo "any trace of the command. Pass it through 'env:', or — for a registry" >&2
    echo "login — use the pinned docker/login-action the same file already uses" >&2
    echo "elsewhere." >&2
    echo >&2
fi

if [ "${#findings[@]}" -ne 0 ]; then
    echo "::error title=Untrusted value expanded into a shell::the following" \
         "expressions are substituted into a run: body before bash parses it" \
         "(#737):" >&2
    for entry in "${findings[@]}"; do
        IFS=$'\t' read -r file line expr <<<"$entry"
        printf '  %s:%s — ${{ %s }}\n' "$file" "$line" "$expr" >&2
    done
    echo >&2
    echo "A value carrying a quote and a semicolon becomes commands, and" >&2
    echo "validating it on the next line does not help: the shell has already" >&2
    echo "parsed it. Pass it as data instead —" >&2
    echo >&2
    echo "  - env:" >&2
    echo "      INPUT_TAG: \${{ inputs.tag }}" >&2
    echo "    run: |" >&2
    echo "      [[ \"\$INPUT_TAG\" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+\$ ]] || exit 1" >&2
    echo >&2
    echo "— which is what coverage-presence.yml, pages.yml," >&2
    echo "release-backmerge.yml and test.yaml already do." >&2
    exit 1
fi

if [ "${#secret_findings[@]}" -ne 0 ]; then
    exit 1
fi

echo "OK: $run_bodies run body/bodies across $examined workflow file(s)," \
     "$expansions expansion(s) in them, none carrying an untrusted value or a" \
     "secret."
exit 0
