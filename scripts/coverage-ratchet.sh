#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Coverage ratchet (#127): compare per-package coverage against the
# committed baseline. Fails when any baselined package drops more than
# RATCHET_EPSILON points below its floor. The baseline only moves up:
# when a package beats its floor, raise the number in the baseline
# file in the same PR that earned it.
#
# Usage: coverage-ratchet.sh <covdata-percent-output> <baseline-file>
#   <covdata-percent-output>: file containing `go tool covdata percent`
#       output, lines like:
#         github.com/.../pkg/plugin   coverage: 82.4% of statements
#
#       ONE LINE CAN CARRY TWO PACKAGES. `go tool covdata percent` prints
#       a package that contributes no statements as its name alone, with
#       no percentage AND NO NEWLINE, so the next package's entry lands
#       on the same line. Measured on coverage run 34528649522, where
#       pkg/buildinfo (new in 2.0, no statements) swallowed pkg/dhcp:
#         \tgithub.com/.../pkg/buildinfo\t\t\tgithub.com/.../pkg/dhcp\t\tcoverage: 90.5% of statements
#       The ratchet read that file a line at a time, found no row whose
#       FIRST field was pkg/dhcp, and reported a package measured at
#       90.5% as "in baseline but absent from coverage output" -- a
#       vanished-package failure on the release PR's required check,
#       with nothing wrong with the coverage. Both readers below scan
#       the FIELDS of a line instead: a percentage belongs to the name
#       immediately before its `coverage:`, so a swallowed name gets no
#       number and a swallowing name is not credited with one either.
#   <baseline-file>: lines of "<package> <min-percent>", '#' comments ok.
#
# RATCHET_EPSILON (default 0.5): tolerated drop in percentage points,
# absorbing run-to-run noise from timing-dependent integration paths.
#
# Exit: 0 every baselined package holds (a deliberately deleted one is
# DROPPED and does not change this), 1 a package regressed or is floored
# with no coverage and was not deliberately deleted, 2 the ratchet cannot
# render a verdict.
#
# A DELIBERATELY DELETED PACKAGE (2026-09-11). Since #735 the floors come
# from the MERGE BASE, so a PR cannot lower its own. The same rule made a
# deliberate deletion unpassable: 2.0 deletes cmd/dhcp-handler and drops
# its row here, and the row's removal is invisible to a PR whose base
# still carries it — the base floor is read, no coverage is found, and
# the release PR's required check fails on a package that is gone on
# purpose. Measured on run 34541498417 (release PR #937): four packages
# PASS and cmd/dhcp-handler FAILs "absent from coverage output".
#
# A baselined package with no coverage therefore takes one of THREE
# verdicts, and the two inputs are the package's existence AT HEAD and
# the HEAD baseline's own rows:
#
#   present at head                 -> FAIL, as before. The package is
#                                      there and the run measured nothing.
#   gone, still floored at head     -> FAIL. The deletion did not reach
#                                      the baseline; the row is the thing
#                                      to remove, in that same change.
#   gone, row gone from head too    -> DROPPED. Counted as compared, the
#                                      exit code unaffected.
#
# Existence is asked of the toolchain (`go list` of the import path), not
# of a directory glob: the baseline keys on import paths and a package
# can be deleted while its directory survives.
#
# THE SAME FACT IS DERIVED A SECOND TIME ELSEWHERE, and the two rules are
# not identical. scripts/check-coverage-floor.sh asks whether a REMOVED
# row's package is gone, over a git ref with no checkout to run `go list`
# in, so it looks for .go files under the package's directory at that
# ref. The answers differ for a directory whose files are all excluded by
# build tags: present to that gate, absent to this one. Both are red in
# the safe direction -- that gate would call the removal a lowered floor,
# this one would DROP a package the tree still carries files for -- and
# neither can be given the other's input.
#
# WHAT THIS DELIBERATELY DOES NOT DO, stated because the arm reads wider
# than it is. A package deleted to DODGE a floor takes the same DROPPED
# line. That is not this gate's question: it claims that what the release
# ships is covered, not that nothing was deleted. A dodge is visible in
# the two places review actually reads — a DROPPED line in the required
# check's log, naming the package and the floor it carried, and a deleted
# directory in the diff. Making the gate refuse it would mean refusing
# every legitimate deletion, which is the state this replaces.
#
# EXIT 2 IS WHY THIS GATE IS TRUSTWORTHY AT ALL (#734). `coverage` is a
# required context on main and is the one thing between a coverage
# regression and a release, and it used to `exit 0` whenever it compared
# nothing: the loop was `done < "$BASELINE_FILE"` with no `set -e` and no
# post-loop assertion, so a missing baseline file printed a shell error
# to stderr and still reported success. The baseline is 258 lines of
# which 253 are commentary, so a rebase dropping the five data lines,
# a rename, or a relative path resolved from the wrong directory all
# leave a file that still LOOKS populated while the gate enforces
# nothing. Refusing a verdict over empty input is the shape 46 of the
# other 47 gates already use.
#
# THAT GUARD CATCHES ZERO, NOT INCOMPLETENESS (#791). `compared` is
# incremented once per data line and the loop iterates OVER the baseline,
# so `compared` is by construction the number of data lines in the file
# it was handed. The guard can therefore only ever fire on the empty
# case. A baseline arriving with two of its five packages -- a bad
# rebase, a truncated blob, a partial fetch, a merge that dropped lines
# -- compares two, prints two PASS lines and exits 0, and the release
# reads that as a clean ratchet. The 253 commentary lines are what make
# it invisible: the file still looks populated.
#
# The count cannot come from inside this script. Nothing here has a
# second opinion about what a complete baseline is, and deriving the
# expected number from the same file is a measurement backstopping
# itself. It comes from coverage-baseline-at.sh, which resolved the blob
# and is the one place that knows what it handed over. Set
# RATCHET_REPORT to that report and this asserts it compared exactly the
# packages named in it -- by NAME, because "2 of 5" sends someone to read
# a 258-line file and a list of names sends them to two lines.
#
# A run with no report is NOT silently exempt. It says so, loudly, in
# the one line a reader of a release log would otherwise take as a clean
# verdict -- because an unannounced absence of a cross-check is exactly
# the shape this gate keeps being caught by.
#
# AND NEITHER IS A LINE THAT LOST ITS FLOOR (#791). The cross-check above
# compares NAMES and COUNTS; a data line whose percentage is gone is
# present on both sides, so all three of resolver, cross-check and ratchet
# attest "5 of 5" while one package has no floor at all -- awk reads the
# empty field as 0 and every percentage beats it. The floor is therefore
# validated where it is read, and an unreadable one refuses the run.
set -u

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <covdata-percent-output> <baseline-file>" >&2
    exit 2
fi

PERCENT_FILE="$1"
BASELINE_FILE="$2"
EPSILON="${RATCHET_EPSILON:-0.5}"
# Default to the sidecar coverage-baseline-at.sh writes beside the blob,
# so the release path is cross-checked without a second wiring step that
# could be forgotten. An explicit RATCHET_REPORT overrides; RATCHET_REPORT=
# (empty) is how a caller says "there is no resolver here", and that
# still prints the NOT CROSS-CHECKED line rather than passing quietly.
REPORT="${RATCHET_REPORT-$2.report}"
fail=0

# THE HEAD BASELINE IS FOUND, NOT WIRED. $BASELINE_FILE is the merge
# base's copy on the release path, so it cannot answer "does the branch
# still floor this package". The head's copy is located from this
# script's own path -- the gate ships in the tree it gates -- rather than
# from a new argument in coverage.yml, for the #791 reason: a second
# wiring step is a step someone forgets, and the run that would find out
# is the release PR. RATCHET_HEAD_BASELINE overrides it, which is how the
# self-test drives the arms without editing the repository's own file.
SELF_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd) || SELF_DIR=""
REPO_ROOT=$(dirname -- "${SELF_DIR:-.}")
HEAD_BASELINE="${RATCHET_HEAD_BASELINE-$REPO_ROOT/.github/coverage-baseline.txt}"
GO_BIN=$(command -v go 2>/dev/null) || GO_BIN=""

# Does the import path still build at head? Asked of the toolchain, not
# of the filesystem: the baseline keys on import paths, a directory can
# outlive its package, and `go list` is the thing that decides what a
# package is. Rooted at $REPO_ROOT so the answer does not depend on the
# caller's working directory. GOPROXY=off changes no verdict -- an
# in-module path resolves or fails locally either way, measured -- it
# bounds the WAIT, so a proxy outage cannot hold a required check open.
pkg_at_head() { # 1 = import path; 0 = present, 1 = absent
    if [ -z "$GO_BIN" ]; then
        echo "::error title=Cannot classify an absent package::$1 is floored by $BASELINE_FILE and" \
             "has no coverage in this run, and there is no 'go' on PATH to ask whether the package" \
             "still exists at head. A deliberate deletion and a package whose coverage vanished are" \
             "the same shape without that answer." >&2
        exit 2
    fi
    if GOPROXY=off GOWORK=off GOFLAGS=-mod=readonly \
        "$GO_BIN" list -C "$REPO_ROOT" -- "$1" >/dev/null 2>&1; then
        return 0
    fi

    # A FAILED `go list` HAS TWO MEANINGS AND ONLY ONE OF THEM IS "GONE".
    # It also fails when the toolchain cannot run at all -- an unusable
    # module cache answers "toolchain not available" for a package that is
    # right there in the tree (measured by the reviewer with GOMODCACHE
    # pointed at an empty directory). Folded into "absent", that reads as
    # a deliberate deletion whenever the head baseline has also dropped
    # the row, and DROPS a floor for a package the release still ships.
    #
    # So the failure is CONTROLLED against the same toolchain, the same
    # root and the same flags, over a pattern that must resolve in any
    # working checkout. Control red means the probe cannot answer, which
    # is a refusal, not a verdict.
    if ! GOPROXY=off GOWORK=off GOFLAGS=-mod=readonly \
        "$GO_BIN" list -C "$REPO_ROOT" ./... >/dev/null 2>&1; then
        echo "::error title=Cannot classify an absent package::$1 is floored by $BASELINE_FILE," \
             "has no coverage in this run, and 'go list' will not resolve it -- but 'go list ./...'" \
             "fails in this checkout too, so the toolchain is not answering and the failure says" \
             "nothing about the package. A deletion and a broken toolchain are the same shape here." >&2
        exit 2
    fi
    return 1
}

# Same data-line rule as the loop below and as the resolver's: a leading
# '#' or a blank line is commentary.
head_floors() { # 1 = import path; 0 = the head baseline carries a row for it
    awk -v p="$1" '
        { sub(/^[[:space:]]+/, "") }
        /^#/ { next }
        NF == 0 { next }
        $1 == p { found = 1; exit }
        END { if (found) exit 0; exit 1 }
    ' "$HEAD_BASELINE"
}

for f in "$PERCENT_FILE" "$BASELINE_FILE"; do
    if [ ! -f "$f" ] || [ ! -r "$f" ]; then
        echo "::error title=Nothing to inspect::$f is not a readable file." \
             "The ratchet would otherwise report a clean pass having compared nothing." >&2
        exit 2
    fi
done

# Refused here -- after the two inputs, so a missing handed-in baseline
# keeps its own refusal -- and not at the point of use, because the use
# happens only when a baselined package has no coverage: a head baseline
# resolved to a path that does not exist would answer "the branch does
# not floor it" for EVERY package, silently turning every vanished
# package into a DROPPED one. That is the fail-open direction of this whole arm, and it
# would be invisible on a run where nothing vanished.
if [ ! -f "$HEAD_BASELINE" ] || [ ! -r "$HEAD_BASELINE" ]; then
    echo "::error title=No baseline at head::$HEAD_BASELINE is not a readable file." \
         "The ratchet cannot tell a deliberately deleted package from a vanished one without" \
         "the baseline as it stands at HEAD, and reading every package as unfloored there" \
         "would pass every absence." >&2
    exit 2
fi

compared=0
compared_pkgs=""
floor_bad=0

while read -r pkg want; do
    [ -z "$pkg" ] && continue
    case "$pkg" in '#'*) continue ;; esac
    compared=$((compared + 1))
    compared_pkgs="${compared_pkgs}${pkg}
"

    # A FLOOR THAT LOST ITS NUMBER IS NOT A FLOOR (#791). `read -r pkg want`
    # leaves `want` EMPTY when a line carries only a package name, and awk
    # evaluates an empty string as numeric 0 -- so `got > want` is true for
    # every percentage and the package prints
    #
    #     PASS  .../pkg/plugin: 0.1% beats baseline % -- raise the floor
    #
    # while nothing is enforced. Two of the four causes named in the header
    # (a truncated blob, a partial fetch) damage a line rather than delete
    # it, and the completeness cross-check below cannot see this at all: the
    # resolver counts a floor-less line too, so resolver, cross-check and
    # ratchet ALL AGREE on "compared 5 of 5" with one of the five gone. An
    # attestation over a missing floor is worse than no attestation.
    #
    # This is the third zero-shape defect in this repo's gates. An unset or
    # unparseable numeric field is 0, never a sentinel -- and 0 on the
    # RIGHT-HAND side of the comparison is the fail-OPEN direction, which is
    # why this one had to be validated and the left-hand `got` did not: a
    # garbage `got` reads as 0% and fails the package closed.
    #
    # A case glob, not a subprocess, for the reason given at the count check
    # below. Accepts an integer or one-decimal-point floor; rejects empty,
    # a bare `.`, two dots, and anything carrying a non-digit.
    case "$want" in
        ''|.|*[!0-9.]*|*.*.*) want_bad=1 ;;
        *)                    want_bad=0 ;;
    esac

    # RECORD AND CONTINUE, refuse after the loop. A merge that damaged one
    # data line has usually damaged more than one, and a gate that names
    # the first and stops makes the next person fix them one round trip at
    # a time. Every damaged line is named in one run; the verdict is still
    # a refusal, and it still outranks a regression found further down.
    if [ "$want_bad" -eq 1 ]; then
        echo "::error title=Unreadable baseline floor::$BASELINE_FILE gives $pkg no readable floor" \
             "(got '${want:0:40}'). An empty or unparseable floor is numeric 0 to awk, so every" \
             "percentage beats it and the package reports PASS while no floor is enforced." \
             "The completeness cross-check cannot see this: it counts the line, which is present." >&2
        floor_bad=1
        continue
    fi

    # The name is looked for at EVERY field, not only the first, for the
    # two-packages-on-one-line shape in the header. The percentage is the
    # field after this package's own `coverage:` and no other, so a name
    # printed without one stays absent, which is what keeps the
    # vanished-package rule below meaning what it says.
    got=$(awk -v p="$pkg" '{
              for (i = 1; i + 2 <= NF; i++)
                  if ($i == p && $(i + 1) == "coverage:") {
                      pct = $(i + 2); gsub(/%/, "", pct); print pct; exit
                  }
          }' "$PERCENT_FILE")
    # THREE VERDICTS, not two -- see the header. The order is existence
    # first: a package that is still there has not been deleted, whatever
    # the head baseline says about it.
    if [ -z "$got" ]; then
        if pkg_at_head "$pkg"; then
            echo "FAIL  $pkg: in baseline but absent from coverage output — deleted/renamed? The package still builds at head. Update $BASELINE_FILE deliberately."
            fail=1
        elif head_floors "$pkg"; then
            echo "FAIL  $pkg: deleted at head but still floored in $HEAD_BASELINE — remove its row in the same change that deletes the package."
            fail=1
        else
            echo "DROPPED  $pkg: deleted at head and removed from $HEAD_BASELINE (base floor was ${want})"
        fi
        continue
    fi

    verdict=$(awk -v got="$got" -v want="$want" -v eps="$EPSILON" 'BEGIN {
        if (got + eps < want)      print "regressed"
        else if (got > want)       print "improved"
        else                       print "held"
    }')
    case "$verdict" in
        regressed)
            echo "FAIL  $pkg: ${got}% is below baseline ${want}% (epsilon ${EPSILON})"
            fail=1
            ;;
        improved)
            echo "PASS  $pkg: ${got}% beats baseline ${want}% — raise the floor in $BASELINE_FILE"
            ;;
        held)
            echo "PASS  $pkg: ${got}% holds baseline ${want}%"
            ;;
    esac
done < "$BASELINE_FILE"

# A line that lost its floor is not a verdict either. Refused HERE and not
# at the point of detection so that one run names every damaged line, and
# ahead of every other post-loop check because "this file does not say what
# the floors are" outranks anything derived from those floors.
if [ "$floor_bad" -ne 0 ]; then
    echo "::error title=Unreadable baseline floor::$BASELINE_FILE holds data line(s) with no" \
         "readable floor, named above. The ratchet cannot render a verdict over a floor it" \
         "cannot read, and reading one as 0 would pass every package silently." >&2
    exit 2
fi

# A baseline that parsed to no comparisons is not a pass. It is the
# gate having read a file and learned nothing from it — see the header.
if [ "$compared" -eq 0 ]; then
    echo "::error title=Nothing to inspect::$BASELINE_FILE holds no <package> <percent> lines." \
         "The ratchet would otherwise report a clean pass having compared nothing." >&2
    exit 2
fi

# --- the completeness cross-check (#791) ---------------------------------
if [ -z "$REPORT" ] || [ ! -f "$REPORT" ]; then
    echo
    echo "NOT CROSS-CHECKED: no resolver report${REPORT:+ at $REPORT}. This run compared" \
         "${compared} package(s) and CANNOT TELL whether that is all of them —" \
         "the count comes from the baseline it was handed, so a truncated baseline" \
         "agrees with itself. On the release path coverage-baseline-at.sh writes the" \
         "report beside the blob; if you are seeing this there, that step did not run."
else
    want=$(sed -n 's/^count //p' "$REPORT" | head -1)
    # A case glob, not `printf | grep -q`: a piped `grep -q` exits at its
    # first match and SIGPIPEs the producer, so under pipefail the
    # pipeline can report failure on success (scripts/check-pipefail-
    # consumers.sh). No subprocess is needed to ask whether a string is
    # digits.
    case "$want" in
        ''|*[!0-9]*) want_bad=1 ;;
        *)           want_bad=0 ;;
    esac
    if [ "$want_bad" -eq 1 ]; then
        echo "::error title=Unreadable resolver report::$REPORT carries no 'count <n>' line" \
             "(got '${want:0:40}'). The cross-check cannot be made, and a run that cannot" \
             "verify its own completeness must not report a clean ratchet." >&2
        exit 2
    fi

    # `paste -sd', '` was used here and it does NOT join with ", ":
    # -d takes a LIST of delimiters and cycles them, so four items come
    # out "a,b c,d". Driven rather than read. The names in an error
    # message are what a reader greps the baseline for, and a stray
    # space inside a comma-separated list makes two of them look like
    # one token.
    # Compared BY NAME, not by count alone. Two files can hold the same
    # number of packages and not the same packages -- a substitution
    # keeps the count and changes the verdict, which a count check reads
    # as complete.
    sed -n 's/^package //p' "$REPORT" | sort -u > "$BASELINE_FILE.want.$$"
    printf '%s\n' "$compared_pkgs" | grep . | sort -u > "$BASELINE_FILE.got.$$"
    missing=$(comm -23 "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$" | paste -sd, - | sed 's/,/, /g')
    extra=$(comm -13 "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$" | paste -sd, - | sed 's/,/, /g')
    rm -f "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$"

    if [ "$compared" -ne "$want" ] || [ -n "$missing" ] || [ -n "$extra" ]; then
        echo "::error title=The baseline is incomplete::the resolver handed over ${want} package floor(s)" \
             "and this run compared ${compared}.${missing:+ NOT COMPARED: ${missing}.}${extra:+ COMPARED BUT NOT RESOLVED: ${extra}.}" \
             "A baseline that lost lines between resolution and comparison still compares what survived and" \
             "reports every one of them as holding, which is a clean ratchet over a floor that is no longer there." >&2
        exit 2
    fi
    echo
    echo "Cross-checked: compared ${compared} of ${want} resolved package floor(s)$(sed -n 's/^blob /, blob /p' "$REPORT" | head -1)."

    # A SECOND OPINION THAT DOES NOT COME FROM THE BLOB.
    #
    # Everything above compares two parses of one object: the resolver
    # read the baseline blob, this script read the same bytes, and they
    # agree. A blob that was ALREADY short when it was resolved is
    # therefore agreed to by both sides -- which is exactly the failure
    # the resolver's header names (a rebase, a truncated blob, a partial
    # fetch, a merge that dropped lines) and exactly the one neither side
    # can see.
    #
    # $PERCENT_FILE is not derived from the blob. It names every package
    # the run actually measured and comes out of `go tool covdata`. So
    # "the run measured N packages and the baseline floors M of them" is
    # a question the cross-check above cannot ask.
    #
    # IT WARNS, IT DOES NOT REFUSE, and the reason is that the signal is
    # ambiguous by construction: a package measured but not floored is
    # what a truncated baseline looks like AND what a legitimately new
    # package looks like, and nothing here can tell those apart. A gate
    # that fails on both would fire on every new package until someone
    # updated the baseline, and a gate that cries wolf gets discharged.
    # Naming them costs a line and lets a human decide in one glance.
    # Field-scanned for the same reason as the lookup above: read a line
    # at a time, this missed every package whose entry shared a line, and
    # the warning that exists to name an unfloored package stayed silent
    # about the one package the run measured and the baseline did not
    # floor.
    measured=$(awk '{ for (i = 1; i < NF; i++) if ($(i + 1) == "coverage:") print $i }' \
                   "$PERCENT_FILE" | sort -u)
    unfloored=$(comm -13 <(sed -n 's/^package //p' "$REPORT" | sort -u) \
                         <(printf '%s\n' "$measured" | grep .) | paste -sd, - | sed 's/,/, /g')
    if [ -n "$unfloored" ]; then
        echo "::warning title=Measured but not floored::the run measured package(s) the baseline" \
             "does not floor: ${unfloored}. If those are new packages, add floors. If they are" \
             "not new, the baseline blob was already short when it was resolved -- which the" \
             "cross-check above cannot see, because both of its sides parse that same blob."
    fi
fi

if [ "$fail" -ne 0 ]; then
    echo
    echo "Coverage ratchet failed. Add tests covering what this change touches;"
    echo "lowering a floor in $BASELINE_FILE requires a recorded decision in the PR."
fi
exit "$fail"
