#!/usr/bin/env bash
#
# verify.sh — the one command. Runs every gate and prints one verdict.
#
# Usage:  ./verify.sh            run every gate; the oracle runs when the
#                                arbiter's own files have changed since the
#                                last accepted oracle pass, and is SKIPPED,
#                                by name and by hash, when they have not
#         ./verify.sh --oracle   run the oracle whether or not it would skip
#         ./verify.sh --inner    minus the oracle, the self-drive and the
#                                netns row; the oracle's own scenarios use it
#         ./verify.sh --light    also minus the unit suite and the netns row
#                                (see MANIFEST_SCOPED_OUT_ROWS): a SCOPED run,
#                                which says so in its verdict line
# Exit:   0 = PASS, 1 = FAIL. A step that cannot be measured is a FAIL, never
#         a skip. SKIPPED is not that: it is one row, verify-oracle, declining
#         to repeat a measurement whose subject has not changed, and it names
#         the hash and the files that measurement covered.
#
# DECISION 2026-08-29: no CI here (build plan §5.1) — the runners belong to the
# plugin repository — so this file is the only arbiter and has to be one line a
# person can type.
#
# DECISION 2026-08-29: --inner is a flag, not an environment variable. Its one
# caller is scripts/test-verify.sh, the oracle for this file, which would
# otherwise re-enter it forever; an ambient variable would silence the oracle
# for anyone who happened to have it set.

set -euo pipefail

cd "$(dirname "$0")"
ROOT="$PWD"

# This file's own path, resolved AFTER the cd above, because the bounds step
# reads it. "$0" is what the caller typed and is relative to the caller's
# directory, not to this one: MEASURED 2026-08-30, invoking `library/verify.sh`
# from the parent recorded `bounds FAIL … is not readable`, a false negative
# produced entirely by how the script was invoked. Scenario
# invoked-by-relative-path.
SELF="$ROOT/$(basename "$0")"

INNER=0
LIGHT=0
FORCE_ORACLE=0
for arg in "$@"; do
	case "$arg" in
	--inner) INNER=1 ;;
	--light) LIGHT=1 ;;
	--oracle) FORCE_ORACLE=1 ;;
	*)
		echo "VERDICT: FAIL — unknown argument $arg; nothing was measured." >&2
		exit 1
		;;
	esac
done

# ------------------------------------------------------------- manifest --
# The expectation, stated where it is not the subject of any check it
# parameterises. See verify.manifest.sh for why it is a separate file; the
# short version is that four consecutive rounds found a guard whose domain came
# from the thing it guarded, and a domain like that can always be shrunk by
# editing that thing.
#
# This is a HARD refusal, before any row exists, because a row is something the
# manifest declares. A missing manifest is not a run with fewer rows.
MANIFEST="$ROOT/verify.manifest.sh"
if [ ! -r "$MANIFEST" ]; then
	echo "VERDICT: FAIL — $MANIFEST is missing or unreadable; the arbiter has no statement of what must be there, so nothing was measured." >&2
	exit 1
fi
# shellcheck source=verify.manifest.sh
. "$MANIFEST"
if ! manifest_problem="$(manifest_check)"; then
	echo "VERDICT: FAIL — $manifest_problem" >&2
	echo "Nothing was measured: an expectation that disagrees with itself cannot say what is missing." >&2
	exit 1
fi

# in_list NEEDLE ITEM... — set membership, used against the manifest's row
# lists. Written once because three of them ask the same question and a fourth
# open-coded loop is where the fourth answer differs.
in_list() {
	local needle="$1" x
	shift
	for x in "$@"; do
		[ "$x" = "$needle" ] || continue
		return 0
	done
	return 1
}

# row_omitted NAME — true when THIS RUN's scope leaves the row out.
#
# DECISION 2026-09-05 (machinery batch, item 2): the scope of a run is a
# property of the run, read here from the manifest, and never a property of
# the thing being run. The oracle's scenarios that plant a shell or a document
# defect pass --light and do not pay for the unit suite; which rows that omits
# is MANIFEST_SCOPED_OUT_ROWS, a list in the manifest, pinned from Go, and NOT
# a decision any scenario body can make.
#
# A scoped run is not a quieter run: the verdict line names the rows it did not
# run, and a scenario whose contract expects a verdict from an omitted row
# reads ABSENT and fails its contract.
row_omitted() { # NAME
	[ "$LIGHT" -eq 1 ] || return 1
	in_list "$1" "${MANIFEST_SCOPED_OUT_ROWS[@]}"
}

# The wall-clock ceiling on the unit suite, in seconds — T2's second
# instrument, reading the clock where the identifier gate reads source.
#
# BOUND: a threshold only holds AT the threshold. A 200ms sleep does not move a
# 60s ceiling, so this catches a suite that has drifted into waiting, not one
# test that waits a little.
SUITE_CEILING_SECONDS=60

# The hang bound, in seconds, passed to `go test -timeout`. The ceiling above
# cannot bound a hang: it is computed after `go test` returns (scenario
# hang-bounded). Without this line the only bound is Go's default of ten
# minutes per binary, which is set nowhere here and is removable through
# GOFLAGS.
SUITE_TIMEOUT_SECONDS=180

# The flags `go test` actually runs with, in ONE array, so the constant the
# bounds step reads is the constant the suite uses. Scenarios test-cache,
# race-detector, hang-bounded, bounds-ordering, suite-timeout-detached.
#
# -v is here for the PARTITION, added 2026-09-05 round 2 and not for reading:
# without it the run says nothing about WHICH tests it ran, and the row's
# sentence about how many were held back for the netns row was a restatement of
# the roster rather than a measurement of the run. Scenario
# suite-partition-skip-inert.
SUITE_ARGS=(-race -count=1 -v -timeout "${SUITE_TIMEOUT_SECONDS}s")

# The netns runs — the tests that re-execute themselves into a user and
# network namespace and talk to a real dnsmasq — have their own row, their own
# ceiling and their own hang bound.
#
# DECISION 2026-09-05 (design §A.3.6 Q8, machinery batch): they were inside the
# unit suite and the 60s ceiling was measuring both. MEASURED 2026-09-04:
# 42s at M6 r1, 54s idle and 57-59s loaded at M6 r2, against a 60s ceiling —
# six seconds of headroom, and M7 adds more netns runs. The ceiling is T2's
# clock instrument (has the suite drifted into waiting), and a bound that
# measures a pure suite and a set of runs that wait out real RFC intervals
# measures neither. So the populations are split and each keeps a bound of its
# own; SUITE_CEILING_SECONDS goes on measuring the pure suite.
#
# The two populations are a PARTITION of one roster: the netns names come from
# internal/tools/testroster -netns, the pure suite is everything else, and the
# suite is told to skip exactly the netns names. A test that the classifier
# does not recognise is therefore not excluded from both rows — it runs in the
# pure suite, where its seconds land on the ceiling above.
#
# THE NUMBERS, MEASURED 2026-09-05 on the session box with the partition in
# force: the pure suite 12s and the netns row 52s idle, against 53s for the two
# together before the split. Under load (three parallel oracle copies, same
# box, load average 8.4) the netns row measured 53s — one second over idle,
# because these runs are WAITING on real DHCP and ARP intervals, not competing
# for the CPU. The ceiling is set at 90s, roughly seventy percent over the
# loaded figure, because the M7 rounds add more of these waits; the pure suite
# keeps its 60s with five times the headroom it had this morning.
#
# The bound that number did NOT give when it was written: a box slow enough to
# change what these tests wait for (a dnsmasq that starts late, a kernel that
# is slow to build a namespace) moves the figure in a way a CPU load does not,
# and the ceiling was the headroom for that, measured on nothing. It is no
# longer measured on nothing: the end of the block below carries the two-core
# CI runner's own figure for this row against this box's.
#
# RAISED 90 -> 120 AT M7c (v6 runtime), then 120 -> 140 at M7c's carried rows,
# and the enumeration is the licence for raising it rather than a summary of
# it. MEASURED 2026-09-06 on the session box, run back to back and exactly as
# this file runs the row (-race -count=1 -v): 53s at f901589, 78s at M7c's
# head e173966, and 94s here (90s on a warm build cache; the larger figure is
# the one the ceiling is set off). Per test, taking the outer (re-exec parent)
# figure, M7c's head -> here, in seconds:
#
#   TestARefusedResumeRestartsAgainstRealDnsmasq            1.33 -> 1.29
#   TestARestartResumesItsLeaseAgainstRealDnsmasq           1.32 -> 1.31
#   TestASquatterAfterBoundTakesSection24sPath              2.46 -> 2.51
#   TestASquatterInTheProbeWindowMakesAWaitingClientDecline 2.58 -> 2.54
#   TestASquatterInTheProbeWindowMakesAnAsyncClientDecline  2.37 -> 2.34
#   TestASquatterInTheProbeWindowStillDeclines...Configured 2.23 -> 2.25
#   TestAcquiresFromRealDnsmasq                             1.19 -> 1.17
#   TestAnExpiredResumeDiscoversAgainstRealDnsmasq          1.31 -> 1.32
#   TestAnOffClientPutsNoARPOnTheWire                       4.22 -> 4.18
#   TestDeclineAndReleaseReachRealDnsmasq                   2.16 -> 2.17
#   TestOurOwnTrafficInTheProbeWindowDoesNotDeclineOurLease 4.28 -> 4.24
#   TestPacketTransportDropsWhenTheConsumerStalls           3.64 -> 3.63
#   TestPacketTransportFollowsAPeerToANewHardwareAddress    1.30 -> 1.28
#   TestPacketTransportOnARealLink                          1.24 -> 1.24
#   TestRenewalAndNakReachRealDnsmasq                       7.18 -> 7.22
#   TestTheClientKeepsTheNamespaceItWasBuiltIn              1.18 -> 1.20
#   TestTheDelayBeforeAnAcquisitionIsRFC5227sArithmetic     7.73 -> 8.39
#   TestTheProbeCarriesTheLinkAddressAndNotCHAddr           1.66 -> 1.59
#   TestTheRebuiltJournalMatchesTheServersLeaseFile         1.48 -> 1.41
#   TestAClientOnALinkWithNoRouterStillAcquires             3.39 -> 3.08
#   TestADuplicateAddressOnTheLinkIsDeclined                1.29 -> 1.77
#   TestAManagedLinkWhoseServerIsSilentIsNotALinkWithoutOne 2.82 -> 2.28
#   TestAResumedV6LeaseConfirmsAgainstRealDnsmasq           4.90 -> 4.34
#   TestASLAACOnlyLinkSaysThereIsNoDHCPv6                   1.27 -> 1.22
#   TestAV6ClientAcquiresFromRealDnsmasq                    2.69 -> 3.01
#   TestAV6ClientOnAStatelessLinkIsConfiguredAndNotLeased   2.30 -> 2.00
#   TestAV6ReleaseReachesRealDnsmasq                        4.54 -> 4.25
#   TestTheV6ClientKeepsTheNamespaceItWasBuiltIn            3.24 -> 2.30
#   TestAV6ClientDiscardsAnotherClientsReplyAtTheTransport     = 4.78  (new)
#   TestAV6ClientRefusesALinkThatNeverGetsALinkLocalAddress    = 5.04  (new)
#   TestAV6ClientWaitsForTheKernelToAssignTheLinkLocalAddress  = 2.79  (new)
#
# THE TWENTY-EIGHT TESTS THIS ROUND DID NOT ADD MEASURE 75.53s AGAINST 77.30s
# at M7c's head — downward, so the whole of the 12.61s increase is the three
# new rows and none of it is the old ones slowing. Two of the twenty-eight
# move up by more than a tenth and both are this round's work, named rather
# than averaged away: TestAV6ClientAcquiresFromRealDnsmasq (+0.32) now opens an
# ETH_P_ALL watch and reads the EtherType the solicit left with, which is what
# turns a wrong-ethertype transport into a named red in a second instead of the
# child's 45s timeout; TestADuplicateAddressOnTheLinkIsDeclined (+0.48) pays
# for the serves half of assertMode, a second read of dnsmasq's log at
# Cleanup. The third mover, TestTheDelayBeforeAnAcquisitionIsRFC5227sArithmetic
# (+0.66), waits out RFC 5227's randomised initial delay: its figure is a draw
# from that distribution, and it moved 0.94s the other way last round.
#
# The per-test figures sum to 88.14s against a 94s wall clock; the difference
# is the build, the race instrumentation and the re-exec, and it is why the
# ceiling is set off the WALL figure.
#
# 140 keeps 46s of headroom (49 percent) where 120 kept 42s (54 percent)
# before this round; the number is set so the ratio to the measured row does
# not shrink round on round, since a ceiling that erodes is one that stops
# diagnosing before anybody notices. It is still below NETNS_TIMEOUT_SECONDS,
# which is what makes a row that overruns the ceiling a diagnosis rather than
# a kill, and still above the child's own 45s budget.
#
# THE BOUND THIS NUMBER STILL DOES NOT GIVE is above: a box slow enough to
# change what these tests WAIT FOR moves the figure in a way a CPU load does
# not. What is measured against that: the two-core CI runner ran this row in
# 48s at run 33995090303 where the session box measured 54s for the same head
# — FASTER, on a box 6.4x to 8.7x slower for the oracle — because these tests
# wait on DHCP, ARP and DAD intervals rather than competing for the CPU. That
# is why a figure measured on this box is allowed to set the ceiling CI runs
# under, and it is a measurement, not an assumption.
NETNS_CEILING_SECONDS=140
NETNS_TIMEOUT_SECONDS=180
NETNS_ARGS=(-race -count=1 -v -timeout "${NETNS_TIMEOUT_SECONDS}s")

# The gates that MUST run: enumerated, not discovered — a verifier that finds
# its own checklist is silenced by deleting a check. Cross-checked below in
# both directions (scenarios roster-gate-deleted, roster-gate-added).
#
# Enumerated in the manifest rather than here, since round 9: a checklist that
# lives beside the code it checks is deleted by the same edit.
REQUIRED_GATES=("${MANIFEST_GATES[@]}")

# The ROWS that must appear in the verdict table, for the same reason and
# against a defect measured 2026-08-30: the verdict printed
# `VERDICT: PASS (${#NAMES[@]} steps)`, a count DESCRIBED and never CHECKED.
# Replacing `step "vet" go vet ./...` with `true` produced
# `VERDICT: PASS (10 steps)` and nothing refused.
#
# That is the same class as the two findings above it, one level up again: the
# SET OF ROWS was a population with no non-vacuity check. It composed with the
# stubbed oracle rather than sitting beside it — README stated this exact bound
# and rested it on the oracle's scenarios, and the oracle turned out to be
# deletable. Neither half was wrong when it was written; the composition was
# never measured.
#
# Cross-checked in BOTH directions at the verdict (scenarios row-deleted,
# row-added), and refusing on an empty roster, because a universal gate is
# satisfied by emptying its own domain.
#
# ROUND 9, and this is the finding rather than a refinement of it: the list
# used to be written here, so B9's defeat was to delete a row's step AND its
# name in one file. MEASURED 2026-08-30 by review — eleven rows became ten,
# every remaining row was green, and four SC2034 findings sat in the tree the
# deleted gate would have caught. The names now come from the manifest, which
# is pinned from Go (internal/manifest), so the cheap edit is no longer the
# quiet one. Scenarios manifest-row-removed, manifest-count-lies,
# manifest-missing.
REQUIRED_ROWS=("${MANIFEST_ROWS[@]}")

BIN="$(mktemp -d)"

declare -a NAMES=() RESULTS=() NOTES=()
FAILED=0
VERDICT_PRINTED=0
ABORT_LINE=""

# The verdict is printed by a trap so that EVERY exit path prints one.
#
# MEASURED 2026-08-29: as the last line of the script instead, a single
# unprotected assignment above it took `set -e` with it, and deleting go.mod
# made this file exit 1 having printed no verdict at all. Fixing that
# assignment would fix that assignment; the promise is a property of the file.
# Scenarios verdict-on-abort, verdict-without-gomod.
on_exit() {
	local rc=$?
	rm -rf "$BIN"
	if [ "$VERDICT_PRINTED" -eq 0 ]; then
		echo
		echo "VERDICT: FAIL — the verifier aborted before reaching its verdict (exit $rc${ABORT_LINE:+, at line $ABORT_LINE})."
		echo "${#NAMES[@]} step(s) had been recorded. Everything after the abort is UNMEASURED, which is a FAIL and not a skip."
		[ "$rc" -ne 0 ] || rc=1
		exit "$rc"
	fi
}
trap on_exit EXIT
trap 'ABORT_LINE=$LINENO' ERR

# record NAME RESULT NOTE [COUNT] — the ONE place a row is written, and the one
# place PASS is decided.
#
# A PASS must carry COUNT: how many things the row examined. Absent,
# non-numeric or zero and the row is REWRITTEN to FAIL. This is the round-7
# structural change and it exists because three separate rows passed over an
# absent subject, in three consecutive review rounds, each found only where
# somebody happened to look:
#
#   the unit suite, with every test build-tagged out  (round 5, 22 files)
#   the oracle, replaced by `exit 0`                  (round 6, B7)
#   the ROW ROSTER, with a step call deleted          (round 7, measured here)
#
# All three inherited one default: `step()` recorded PASS from rc == 0, and a
# command with nothing to do exits 0. The fix is not another guard beside
# another row — it is that the default is now FAIL, and a row has to say what
# it looked at in order to pass.
#
# BOUND, and it is the design's real residual: nothing here can force COUNT to
# be DERIVED. A row that hard-codes `1` satisfies this completely. What closes
# that is external and is where the round's evidence actually lives — one
# oracle scenario per row that empties THAT row's domain and requires the row
# to go red. See the row-drive scenarios in scripts/test-verify.sh.
# SKIPPED, added 2026-09-05 with the oracle stamp, is the third verdict and it
# is the dangerous one: a row that may decline to run is a row that may be made
# to decline always. Three things hold it here, at the choke point, rather than
# beside the row that uses it:
#
#   1. ONE row may skip. MANIFEST_SKIPPABLE_ROWS names it; any other row
#      recording SKIPPED is rewritten to FAIL, exactly as an uncounted PASS is.
#   2. A SKIPPED row still states a COUNT — the size of the population its
#      stamp covers — so "the oracle was skipped" cannot be said by a row that
#      examined nothing.
#   3. Both directions are driven in process, on every run, by self_check
#      below: a skip by the wrong row must be refused and an honest one must
#      survive. That is not the same as the arm being OBSERVED, and round 1
#      said it was. MEASURED by review: this condition turned to `if false`
#      AND the probe that drives it deleted in one edit left every row green
#      and the whole oracle green, because the probes counted themselves. The
#      count is declared in verify.manifest.sh now, out of reach of the
#      deletion, and scenario self-check-skip-arm-deleted plants the composed
#      edit and requires the row that owns it to go red.
record() { # name result note [count]
	local name="$1" result="$2" note="${3:-}" count="${4:-}"
	if [ "$result" = SKIPPED ] && ! in_list "$name" "${MANIFEST_SKIPPABLE_ROWS[@]}"; then
		note="recorded SKIPPED, which only [${MANIFEST_SKIPPABLE_ROWS[*]}] may do; every other row measures or fails"
		result=FAIL
	fi
	if [ "$result" = PASS ] || [ "$result" = SKIPPED ]; then
		case "$count" in
		'' | *[!0-9]*)
			result=FAIL
			note="recorded PASS with no numeric domain size (got '$count'); a row that cannot say how many things it examined has measured nothing"
			;;
		*)
			if [ "$count" -lt 1 ]; then
				result=FAIL
				note="recorded PASS having examined 0 items; an empty domain is not a passing domain"
			fi
			;;
		esac
	fi
	NAMES+=("$name")
	RESULTS+=("$result")
	NOTES+=("$note")
	[ "$result" = PASS ] || [ "$result" = SKIPPED ] || FAILED=1
}

# step NAME COUNT -- command... — the exit-status rows, which can no longer
# pass on an exit status alone: COUNT is a second operand and record refuses
# a PASS without it.
#
# The detail column falls back to the domain size when the command printed
# nothing. B7's measurement is why: a stubbed oracle produced `verify-oracle
# PASS` with an EMPTY detail column, and an empty cell is the quietest thing a
# table can contain.
# quote_block TEXT — another run's output, quoted into this one's, INDENTED.
#
# ROUND 13. An unindented copy of an inner run's report is indistinguishable
# from this run's own report to anything that parses the stream — and the
# oracle parses the stream. It cost every row of a scenario's reading coming
# back ABSENT, because the scenario read the quoted table instead of the real
# one. Indentation is the half of that fix that lives here; the other half is
# table() in scripts/test-verify.sh taking the LAST table rather than the first.
quote_block() { printf '%s\n' "$1" | sed 's/^/  /'; }

step() { # name count -- command...
	local name="$1" count="$2"
	shift 2
	local out rc=0 detail
	out="$("$@" 2>&1)" || rc=$?
	if [ "$rc" -eq 0 ]; then
		detail="$(printf '%s' "$out" | tail -1)"
		[ -n "$detail" ] || detail="$count item(s) in domain"
		record "$name" PASS "$detail" "$count"
	else
		# ROUND 13, B15. What the command SAID, not only that it exited
		# non-zero. A row whose entire diagnosis is "exit 1" names no defect,
		# so two scenarios planting different defects into it are
		# indistinguishable at every instrument in the tree — which is exactly
		# how a scenario is substituted for another and nothing notices.
		detail="$(printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | tail -1)"
		record "$name" FAIL "exit $rc: ${detail:-the command printed nothing}"
		printf '\n--- %s FAILED (exit %s) ---\n%s\n' "$name" "$rc" "$out" >&2
	fi
}

command -v go >/dev/null 2>&1 || {
	echo "VERDICT: FAIL — the go toolchain is not on PATH; nothing was measured." >&2
	exit 1
}

# ------------------------------------------------------------ self-check --
# record()'s guard, driven IN PROCESS, on every run, before any real row.
#
# ROUND 9, B11: the guard was proven only by two oracle scenarios, and deleting
# the guard together with those two scenarios and the plant they edit left the
# arbiter reporting PASS. That is the round's whole class — a witness that dies
# with its subject — and it is why this row exists here rather than only in the
# oracle. Deleting the case block below now reddens a row in the same run,
# with no oracle involved. Scenario self-check-guard-deleted.
#
# It runs FIRST so the probe rows it records are the only rows in flight; it
# discards them and restores FAILED before recording its own verdict. Both
# directions are driven: three shapes that must be refused and one that must
# survive, because a guard that refuses everything measures nothing either.
self_check() {
	local bad="" cases=0 refused=0 i
	[ "${#NAMES[@]}" -eq 0 ] || {
		record "self-check" FAIL "self-check ran after ${#NAMES[@]} row(s); it cannot discard its probes safely"
		return
	}

	# ROUND 2, 2026-09-05, and it is the reason this block is counted at all.
	# MEASURED by review at the previous head: the SKIPPED arm's condition was
	# turned into `if false` and the probe below that drives it was deleted in
	# the SAME edit — the shape this row exists to refuse, one level up. Every
	# row stayed green and the full oracle run passed; the only trace was this
	# note saying "refused 4" where it had said 5, a number computed from the
	# case list itself and therefore moved by the deletion that caused it.
	#
	# The count is now DECLARED, in verify.manifest.sh, where deleting a probe
	# cannot move it, and it is compared against the probes that actually ran.
	# Scenario self-check-skip-arm-deleted plants exactly that composed edit.
	record "__probe__" PASS "no count at all"
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = FAIL ] || bad="$bad a PASS with no count survived;"

	record "__probe__" PASS "count is zero" 0
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = FAIL ] || bad="$bad a PASS over an empty domain survived;"

	record "__probe__" PASS "count is not a number" 7x
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = FAIL ] || bad="$bad a PASS with a non-numeric count survived;"

	record "__probe__" SKIPPED "a row that may not skip" 1
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = FAIL ] || bad="$bad a SKIPPED recorded by a row that may not skip survived;"

	record "${MANIFEST_SKIPPABLE_ROWS[0]}" SKIPPED "a skip with no count at all"
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = FAIL ] || bad="$bad a SKIPPED with no count survived;"

	# The preservation control. Without it this row is satisfied by a record()
	# that rewrites every PASS to FAIL, which would refuse the whole tree and
	# still look like a working guard from here.
	record "__probe__" PASS "an honest counted pass" 1
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = PASS ] || bad="$bad a correctly counted PASS was rejected;"

	# The other preservation control, for the verdict added beside it: a guard
	# that refuses every skip is a row that can never skip, which is a check
	# with one possible verdict wearing the opposite hat.
	record "${MANIFEST_SKIPPABLE_ROWS[0]}" SKIPPED "an honest counted skip" 1
	cases=$((cases + 1))
	[ "${RESULTS[cases - 1]}" = SKIPPED ] || bad="$bad a correctly counted SKIPPED by the row entitled to it was rejected;"

	# The refusals, COUNTED from what record() did rather than derived from the
	# length of the case list. `cases - 2` was the derivation, and it moved
	# with any probe that was deleted — including the probe whose deletion is
	# the whole point of counting.
	for ((i = 0; i < cases; i++)); do
		if [ "${RESULTS[i]}" = FAIL ]; then
			refused=$((refused + 1))
		fi
	done

	NAMES=()
	RESULTS=()
	NOTES=()
	FAILED=0

	if [ -n "$bad" ]; then
		record "self-check" FAIL "record() is not enforcing its contract:$bad"
		echo "--- self-check FAILED: the choke point that decides every PASS does not refuse an uncounted one ---" >&2
	elif [ "$cases" -ne "$SELF_CHECK_PROBES_N" ] || [ "$refused" -ne "$SELF_CHECK_REFUSALS_N" ]; then
		record "self-check" FAIL "this row ran $cases probe(s) of which record() refused $refused, against the $SELF_CHECK_PROBES_N and $SELF_CHECK_REFUSALS_N declared in verify.manifest.sh; a probe that dies with the arm it drives leaves the arm undriven, which is why the count is declared somewhere the deletion cannot reach"
		echo "--- self-check FAILED: fewer probes ran than the manifest declares, so an arm of record() is no longer driven ---" >&2
	else
		record "self-check" PASS "record() refused $refused unrecordable verdict shape(s) of $cases probe(s) and preserved a counted PASS and a counted SKIPPED" "$cases"
	fi
}
self_check

# ------------------------------------------------------------- citations --
# The rule, stated as the code implements it rather than as a summary of it:
#
#   CITED  = every Test/Benchmark/Fuzz/Example token in the part of a .go line
#            that follows the first "//" NOT preceded by ":", or anywhere on a
#            line of a .md file.
#   KNOWN  = every such token that a .go line DECLARES, i.e. a line beginning
#            "func"/"var"/"const"/"type" followed by the token. That admits the
#            exported maps TestIdents and TestRefusedIdents, which are
#            identifiers rather than citations.
#   VERDICT = every CITED token must be KNOWN.
#
# BOUNDS, and there are six because two were not enough — each one is a shape
# this cannot see, not a shape it forgives:
#   1. Block comments. A /* ... */ citation is invisible; nothing in the tree
#      uses them and an awk state machine is the price of covering them.
#   2. A declaration written inside a Go raw string literal counts as a
#      declaration. internal/gates/t2 embeds test bodies that way on purpose.
#   3. .sh files are outside the domain entirely, because the oracle plants
#      test bodies into heredocs.
#   4. Prose cannot use a PLACEHOLDER name: nothing distinguishes one from a
#      citation. That is the loud direction and it stays. MEASURED three times
#      now, each time against a sentence written to DESCRIBE this check —
#      twice on 2026-08-29 and again on 2026-08-30, when the bound-7 paragraph
#      quoted an example URL ending in a test-shaped name.
#   5. Import aliasing and shadowing are not resolved; this is textual.
#   6. "No unseen shape is present in the tree" is a measurement at one head,
#      never a property. A new file reintroduces one silently.
#   8. The token must start a word: a citation is matched only where the
#      character before it is not a letter, digit or underscore. Without that
#      an ordinary camelCase identifier mentioned in prose reads as a citation
#      of a test that does not exist. The direction it still cannot see is a
#      token that starts a word but is part of a longer WORD-BOUNDED name, and
#      that one is indistinguishable from a citation by any textual rule.
#      Scenarios citation-embedded-identifier, citation-word-start.
#   7. The ":" rule covers a URL scheme and nothing else. A "//" inside an
#      ordinary string literal with no colon before it ("a//b") still reads as
#      a comment, and a token after it is still cited. That direction is a
#      FALSE POSITIVE — a build that fails for a reason that is not true — and
#      it is left visible on purpose: narrowing the comment match back toward
#      column 0 would trade it for the false negatives the widening removed.
#      Scenarios citation-url, citation-after-url.
cite_scan() {
	find . -type f \( -name '*.go' -o -name '*.md' \) -not -path './.git/*' -print0 |
		xargs -0 awk -v want="$1" '
			function emit(s,   pre) {
				while (match(s, /(Test|Benchmark|Fuzz|Example)[_A-Z][A-Za-z0-9_]*/)) {
					# The token must START a word. Without this the pattern
					# matches INSIDE an identifier: MEASURED 2026-08-30, the
					# comment on isTestFuncName was read as citing a test
					# called TestFuncName, and the run failed over a token
					# nobody wrote. Bound 8.
					pre = (RSTART > 1) ? substr(s, RSTART - 1, 1) : " "
					if (pre !~ /[A-Za-z0-9_]/) {
						print substr(s, RSTART, RLENGTH)
					}
					s = substr(s, RSTART + RLENGTH)
				}
			}
			want == "cited" {
				if (FILENAME ~ /\.md$/) { emit($0); next }
				# The first "//" that is not a URL scheme separator. Taking
				# index($0,"//") outright reads the tail of an https:// string
				# literal as a comment and fails the run on a token nobody
				# cited (bound 7).
				rest = $0; off = 0
				while (match(rest, /\/\//)) {
					pos = off + RSTART
					if (pos > 1 && substr($0, pos - 1, 1) == ":") {
						off = pos + 1
						rest = substr($0, off + 1)
						continue
					}
					emit(substr($0, pos + 2))
					break
				}
				next
			}
			FILENAME ~ /\.go$/ && match($0, /^(func|var|const|type)[ \t]+(Test|Benchmark|Fuzz|Example)[_A-Z][A-Za-z0-9_]*/) {
				tok = substr($0, RSTART, RLENGTH)
				sub(/^(func|var|const|type)[ \t]+/, "", tok)
				print tok
			}
		' | sort -u
}

cited_f="$(mktemp)"
known_f="$(mktemp)"
cite_scan cited >"$cited_f"
cite_scan known >"$known_f"
cited_n="$(wc -l <"$cited_f" | tr -d ' ')"
known_n="$(wc -l <"$known_f" | tr -d ' ')"
missing="$(comm -23 "$cited_f" "$known_f" | tr '\n' ' ' | sed 's/ $//')"
rm -f "$cited_f" "$known_f"

# Non-vacuity, both sides. A universal claim over an empty domain is a PASS
# that measured nothing, and the cited side is exactly the side this project's
# method keeps shrinking: the sweep replaced facts with pointers.
if [ "$known_n" -eq 0 ] || [ "$cited_n" -eq 0 ]; then
	record "citations" FAIL "measured nothing: $cited_n cited token(s), $known_n declared; a scan that finds no domain is not a passing scan"
elif [ -z "$missing" ]; then
	record "citations" PASS "$cited_n cited token(s), all declared among $known_n" "$cited_n"
else
	record "citations" FAIL "cited but never declared: $missing"
	echo "--- citations FAILED: a comment or document names a test that is not declared anywhere ---" >&2
fi

# Three things, because any two of them are adjacency rather than a data
# dependency: the hang timeout must exceed the ceiling, the flags array must
# carry that timeout, AND the suite invocation must expand that array.
#
# The third matters more than it looks: an invocation that stops expanding
# SUITE_ARGS takes -count=1 with it as well, so it defeats the cached-result
# check too, and every row stays green while this one reports that the suite
# runs with the checked flags.
#
# BOUND, and it is a real one: this checks the SPELLING of the one invocation
# below. A second `go test` elsewhere in this file, or the array under another
# name, is outside it. Scenarios suite-timeout-detached, suite-args-detached.
#
# ROUND 2026-09-05: the same three questions are now asked of the netns row's
# pair, and a fourth of the CHILD's own budget. The netns tests re-execute
# themselves into a namespace; the child there is bounded by its own
# -test.timeout, and if that budget is not strictly below the parent's the
# parent kills the child first and the child's dnsmasq log — the only thing
# that says WHICH wait was stuck — dies with it. That was the state M6 carried
# as row 4, and it is a relation between two numbers in two languages, so it
# is checked rather than remembered.
bounds_src=""
netns_src=""
if [ -r "$SELF" ]; then
	bounds_src="$(grep -c 'go test "${SUITE_ARGS\[@\]}" -skip "\$netns_skip" \./\.\.\.' "$SELF" || true)"
	netns_src="$(grep -c 'go test "${NETNS_ARGS\[@\]}" -run "\$netns_run" \./runtime/' "$SELF" || true)"
fi
netns_child_file="$ROOT/runtime/dnsmasq_linux_test.go"
netns_child_budget=""
if [ -r "$netns_child_file" ]; then
	netns_child_budget="$(sed -n 's/^const netnsChildTimeout = "\([0-9][0-9]*\)s"$/\1/p' "$netns_child_file")"
fi
if [ "$SUITE_TIMEOUT_SECONDS" -le "$SUITE_CEILING_SECONDS" ]; then
	record "bounds" FAIL "hang timeout ${SUITE_TIMEOUT_SECONDS}s does not exceed the ${SUITE_CEILING_SECONDS}s ceiling; a slow suite would be killed before the ceiling could diagnose it"
elif [[ " ${SUITE_ARGS[*]} " != *" -timeout ${SUITE_TIMEOUT_SECONDS}s "* ]]; then
	record "bounds" FAIL "the suite flags do not carry -timeout ${SUITE_TIMEOUT_SECONDS}s, so the checked constant is not the one in force: ${SUITE_ARGS[*]}"
elif [ -z "$bounds_src" ]; then
	# A check that reads its own source and cannot read it has not measured
	# anything, and must say so rather than fall through to the PASS.
	record "bounds" FAIL "$SELF is not readable, so whether the suite runs with these flags is UNMEASURED"
elif [ "$bounds_src" -ne 1 ]; then
	record "bounds" FAIL "found $bounds_src suite invocation(s) expanding SUITE_ARGS, expected exactly 1; the checked constants are not the ones in force"
elif [ "$NETNS_TIMEOUT_SECONDS" -le "$NETNS_CEILING_SECONDS" ]; then
	record "bounds" FAIL "netns hang timeout ${NETNS_TIMEOUT_SECONDS}s does not exceed the ${NETNS_CEILING_SECONDS}s netns ceiling; a slow netns row would be killed before the ceiling could diagnose it"
elif [[ " ${NETNS_ARGS[*]} " != *" -timeout ${NETNS_TIMEOUT_SECONDS}s "* ]]; then
	record "bounds" FAIL "the netns flags do not carry -timeout ${NETNS_TIMEOUT_SECONDS}s, so the checked constant is not the one in force: ${NETNS_ARGS[*]}"
elif [ -z "$netns_src" ] || [ "$netns_src" -ne 1 ]; then
	record "bounds" FAIL "found ${netns_src:-no} netns invocation(s) expanding NETNS_ARGS, expected exactly 1; the checked constants are not the ones in force"
elif [ -z "$netns_child_budget" ]; then
	record "bounds" FAIL "$netns_child_file declares no netnsChildTimeout in seconds, so the deadline every netns wait runs under is UNMEASURED"
elif [ "$netns_child_budget" -ge "$NETNS_TIMEOUT_SECONDS" ]; then
	record "bounds" FAIL "the netns child's own budget ${netns_child_budget}s is not below the parent's ${NETNS_TIMEOUT_SECONDS}s; the parent kills the child first and the child's log — which names the wait that stuck — dies with it"
else
	record "bounds" PASS "suite ${SUITE_TIMEOUT_SECONDS}s > ceiling ${SUITE_CEILING_SECONDS}s, netns ${NETNS_TIMEOUT_SECONDS}s > ceiling ${NETNS_CEILING_SECONDS}s > child ${netns_child_budget}s, and each invocation expands its own flag array" "$((bounds_src + netns_src))"
fi

# ---------------------------------------------------------------- toolchain --
# The Go domain, derived once and handed to the rows that examine it. MEASURED
# 2026-08-30: `go build ./...` over zero packages exits 0, so build had no
# guard of its own and was held only by its neighbours reddening — adjacency,
# not a data dependency. `go vet ./...` over zero packages exits 1, which is
# why the two behaved differently under the same caller and why grouping them
# as one adjudication in round 5 was wrong.
go_pkgs_n="$(go list ./... 2>/dev/null | grep -c . || true)"
go_files_n="$(find . -name '*.go' -not -path './.git/*' -printf 'x\n' | grep -c . || true)"

step "build" "$go_pkgs_n" go build ./...
step "vet" "$go_pkgs_n" go vet ./...

# gofmt -l exits 0 whether or not it lists anything, so its output is the
# signal and its exit code is not.
fmt_out="$(gofmt -l . 2>&1)" || true
if [ -n "$fmt_out" ]; then
	record "gofmt" FAIL "unformatted: $(printf '%s' "$fmt_out" | tr '\n' ' ')"
else
	record "gofmt" PASS "all $go_files_n .go file(s) formatted" "$go_files_n"
fi

# Enumerated in the manifest, then cross-checked in both directions against the
# scripts the tree holds (scenarios unlinted-script, unlinted-shebang-script).
SHELL_SCRIPTS=("${MANIFEST_SHELL_SCRIPTS[@]}")

# shell_files prints every shell script in the tree, one per line, relative to
# the root: a regular file ending in .sh, OR one opening with a shell shebang.
#
# MEASURED 2026-08-29 by review: this used to key on the suffix alone while the
# comments around it described the domain two other ways, so all three
# disagreed and none matched the code; a scripts/preflight with a shebang and
# no extension was linted by nothing.
#
# DECISION 2026-08-29: a filesystem walk, not `git ls-files` — git is
# unavailable inside the oracle's copies of the tree, and a check that
# silently does nothing where it is being tested has no observer.
shell_files() {
	find . -type f -not -path './.git/*' -printf '%P\n' | while IFS= read -r f; do
		case "$f" in
		*.sh)
			printf '%s\n' "$f"
			;;
		*)
			if head -n 1 -- "$f" 2>/dev/null | grep -qE '^#!.*[ /](ba|da|k|z|a)?sh$|^#!.*[ /](ba|da|k|z|a)?sh '; then
				printf '%s\n' "$f"
			fi
			;;
		esac
	done
	return 0
}

if command -v shellcheck >/dev/null 2>&1; then
	shell_expected="$(printf '%s\n' "${SHELL_SCRIPTS[@]}" | sort | tr '\n' ' ')"
	shell_found="$(shell_files | sort | tr '\n' ' ')"
	if [ "$shell_expected" != "$shell_found" ]; then
		record "shellcheck" FAIL "the linted list [$shell_expected] is not every shell script in the tree [$shell_found]"
	else
		linted=()
		for sh in "${SHELL_SCRIPTS[@]}"; do linted+=("$ROOT/$sh"); done
		step "shellcheck" "${#linted[@]}" shellcheck -S warning "${linted[@]}"
	fi
else
	record "shellcheck" FAIL "shellcheck is not installed; the shell scripts were not linted"
fi

# ------------------------------------------------------------- doc-numbers --
# Round 9 deleted thirteen numbers from README.md and docs/*.md because an
# instrument recomputes each of them, and argued the class was closed "by
# removal, not by vigilance". Round 10 measured what that argument was worth
# without an observer: the same round wrote a fresh derived number into
# docs/gates.md, in the very sentence explaining the deletions.
#
# So the sweep is a row. The domain is the prose files themselves, counted here
# so that an empty one is a FAIL rather than a vacuous pass.
doc_files_n="$(find . -maxdepth 2 \( -name 'README.md' -o -path './docs/*.md' \) -printf 'x\n' | grep -c . || true)"
if [ ! -x "$ROOT/scripts/sweep-doc-numbers.sh" ]; then
	record "doc-numbers" FAIL "scripts/sweep-doc-numbers.sh is missing or not executable; the numbers round 9 removed from prose have no observer"
else
	step "doc-numbers" "$doc_files_n" "$ROOT/scripts/sweep-doc-numbers.sh" --check
fi

# ------------------------------------------------------------ readme-usage --
# README.md's Usage section prints a Go function and says, in the sentence
# under it, that it is `ExampleClient` in runtime/example_test.go "byte for
# byte, so it is compiled by the suite rather than transcribed into this file".
# Nothing checked that. A claim of identity between two files is exactly the
# claim a diff makes, and it was being made by prose.
#
# The comparison is VERBATIM — `diff` over the two extracted blocks, no
# normalising of whitespace, no reflow — because the sentence in the README
# says byte for byte, and a row that compares a normalised form would leave
# that sentence false while passing. Both directions are driven (scenarios
# readme-usage-drifts-in-the-readme, readme-usage-drifts-in-the-example).
#
# Either side extracting to nothing is a FAIL and not an agreement: two empty
# files diff clean, which is how this row would go vacuous.
readme_f="$ROOT/README.md"
example_f="$ROOT/runtime/example_test.go"
ru_readme="$BIN/readme-usage.md"
ru_example="$BIN/readme-usage.go"
: >"$ru_readme"
: >"$ru_example"
if [ -r "$readme_f" ]; then
	awk '
		/^## Usage[[:space:]]*$/ { sec = 1; next }
		sec && /^## / { sec = 0 }
		sec && /^```go[[:space:]]*$/ { blk = 1; next }
		blk && /^```[[:space:]]*$/ { blk = 0; next }
		blk { print }
	' "$readme_f" >"$ru_readme"
fi
if [ -r "$example_f" ]; then
	sed -n '/^func ExampleClient() {$/,/^}$/p' "$example_f" >"$ru_example"
fi
ru_readme_n="$(grep -c '' <"$ru_readme" || true)"
ru_example_n="$(grep -c '' <"$ru_example" || true)"
if [ ! -r "$readme_f" ] || [ ! -r "$example_f" ]; then
	record "readme-usage" FAIL "README.md or runtime/example_test.go is unreadable, so the two sides of the byte-for-byte claim cannot be compared"
elif [ "$ru_readme_n" -eq 0 ]; then
	record "readme-usage" FAIL "the README has no fenced go block under ## Usage; the claim that it holds ExampleClient byte for byte has nothing on its own side"
elif [ "$ru_example_n" -eq 0 ]; then
	record "readme-usage" FAIL "runtime/example_test.go declares no ExampleClient the README could be quoting; the README's Usage block is now transcription"
elif ! ru_diff="$(diff -u "$ru_example" "$ru_readme" 2>&1)"; then
	record "readme-usage" FAIL "the README Usage block and ExampleClient differ; the README says byte for byte and they are not"
	printf '\n--- readme-usage: --- is runtime/example_test.go, +++ is README.md ---\n%s\n' "$ru_diff" >&2
else
	record "readme-usage" PASS "the README Usage block and ExampleClient agree over $ru_example_n line(s), byte for byte" "$ru_example_n"
fi

# -------------------------------------------------------------- gate roster --
# Structural, not a glob over directory names: ask the go tool which packages
# under internal/gates are commands. The rc is captured rather than allowed to
# propagate, because `go list` failing is a measurable outcome of this step and
# "aborted at line N" is a worse diagnosis than the one it can give.
#
# N6, round 8: stderr used to be merged into stdout here, so a `go` warning
# line was passed through the same sed as an import path and became a gate
# name. A diagnostic that can be read as a measurement is worse than a missing
# one. The two streams are separated, and only stdout is parsed.
roster_rc=0
roster_err="$BIN/gate-roster.err"
roster_raw="$(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./internal/gates/... 2>"$roster_err")" || roster_rc=$?
discovered="$(printf '%s\n' "$roster_raw" | sed -n 's|^\(.*/\)\{0,1\}\([a-zA-Z0-9_.-]\{1,\}\)$|\2|p' | sort | tr '\n' ' ' | sed 's/ $//')"
expected="$(printf '%s\n' "${REQUIRED_GATES[@]}" | sort | tr '\n' ' ' | sed 's/ $//')"
if [ "$roster_rc" -ne 0 ]; then
	record "gate-roster" FAIL "go list could not enumerate the gates (exit $roster_rc); the roster is UNMEASURED"
	printf '\n--- gate-roster could not be measured ---\n%s\n%s\n' "$roster_raw" "$(cat "$roster_err" 2>/dev/null || true)" >&2
elif [ "$discovered" = "$expected" ]; then
	record "gate-roster" PASS "gates present: $discovered" "${#REQUIRED_GATES[@]}"
else
	# ROUND 13, B15. Naming the DIRECTION, not only the mismatch. "required
	# [a b] but tree has [a]" and "… but tree has [a b c]" differ by one token
	# in a list, so a deleted gate and an invented one read almost alike — and
	# two scenarios whose diagnoses read alike are interchangeable, which is
	# the finding this round exists to close.
	roster_missing="$(comm -23 <(printf '%s\n' $expected | sort) <(printf '%s\n' $discovered | sort) | tr '\n' ' ' | sed 's/ $//')"
	roster_extra="$(comm -13 <(printf '%s\n' $expected | sort) <(printf '%s\n' $discovered | sort) | tr '\n' ' ' | sed 's/ $//')"
	record "gate-roster" FAIL "required [$expected] but tree has [$discovered]${roster_missing:+; MISSING from the tree: $roster_missing}${roster_extra:+; UNDECLARED in the manifest: $roster_extra}"
	echo "--- gate-roster FAILED: the set of gate commands does not match MANIFEST_GATES in verify.manifest.sh ---" >&2
fi

# ------------------------------------------------------------------- gates --
# Built, then executed. MEASURED 2026-08-29: `go run` collapses every non-zero
# child status to 1, which would make a gate REFUSING (2) indistinguishable
# from a gate reporting a violation (1). Scenarios gate-panic, gate-refuses.
for g in "${REQUIRED_GATES[@]}"; do
	if ! go build -o "$BIN/$g" "./internal/gates/$g" 2>"$BIN/$g.err"; then
		record "$g" FAIL "gate does not compile"
		cat "$BIN/$g.err" >&2
		continue
	fi
	rc=0
	out="$("$BIN/$g" -root "$ROOT" 2>&1)" || rc=$?
	case "$rc" in
	# The gate's own domain size, read out of the line it prints. Both gates
	# already REFUSE on an empty domain (t1 rule D, t2 rule B), so this is the
	# second reading of a fact they already enforce — and that is the point of
	# a choke point: the row cannot pass without stating it, whether or not the
	# subject also checks itself.
	0)
		gate_n="$(printf '%s' "$out" | sed -n 's/.*[^0-9]\([0-9][0-9]*\) \(transitive dep(s)\|test file(s)\) checked.*/\1/p' | tail -1)"
		record "$g" PASS "$(printf '%s' "$out" | tail -1)" "$gate_n"
		;;
	1) record "$g" FAIL "VIOLATION" ;;
	2)
		# A Go panic also exits 2. Both are a FAIL, but they are different
		# diagnoses, and the gates print a REFUSED line so the two can be told
		# apart.
		if printf '%s' "$out" | grep -q 'REFUSED'; then
			record "$g" FAIL "REFUSED — the gate could not measure its domain"
		else
			record "$g" FAIL "exit 2 with no REFUSED line — the gate crashed (a Go panic exits 2 too)"
		fi
		;;
	*) record "$g" FAIL "unexpected exit $rc" ;;
	esac
	[ "$rc" -eq 0 ] || printf '\n--- %s ---\n%s\n' "$g" "$out" >&2
done

# ------------------------------------------------------------- unit suite --
# -count=1 defeats the test cache: a cached PASS is a result that was not
# measured on this tree. Scenario test-cache.
#
# MEASURED 2026-08-30: `go test ./...` exits 0 on a tree with no test files at
# all, printing "? pkg [no test files]". So the exit status cannot tell a suite
# that passed from a suite that did not run, and adding `ignore` to the build
# constraint of all 22 _test.go files took this whole script to
# "VERDICT: PASS (10 steps)" with zero tests executed — t2 still counted 22
# files, because it walks the filesystem, and this row still said 0s, because a
# ceiling reads absent as fast. Hence the domain check below.
#
# It is keyed on the POPULATION rather than on a floor: a floor is a number
# somebody has to maintain, and it cannot see a single package's tests being
# disabled. Scenarios suite-tests-disabled, suite-one-package-disabled.
# THE PARTITION, added 2026-09-05 with the netns row (design §A.3.6 Q8).
#
# The netns names come from internal/tools/testroster -netns, which reports the
# tests that call the re-exec helper; the pure suite is told to -skip exactly
# those names and the netns row is told to -run exactly those names. One
# roster, two complementary filters: a test the classifier does not recognise
# is NOT excluded from both rows — it stays in the pure suite, where its
# seconds land on SUITE_CEILING_SECONDS. That is the failure this batch was
# told to name and refuse, and this is where it is refused.
#
# The classifier REFUSING is a FAIL of both rows rather than a run of
# everything: an empty skip list is a pure suite that quietly contains the
# netns tests again, which is the state this replaced.
netns_roster=""
netns_roster_rc=0
netns_n=0
netns_skip=""
netns_run=""
if ! row_omitted unit-suite || ! row_omitted netns-suite; then
	netns_roster="$(go run ./internal/tools/testroster -netns "$ROOT" 2>&1)" || netns_roster_rc=$?
	if [ "$netns_roster_rc" -eq 0 ]; then
		netns_n="$(printf '%s\n' "$netns_roster" | grep -c . || true)"
		netns_alt="$(printf '%s\n' "$netns_roster" | grep . | paste -sd '|' -)"
		netns_skip="^(${netns_alt})$"
		netns_run="^(${netns_alt})$"
	fi
fi
netns_unmeasured="the netns population is UNMEASURED (testroster -netns exit $netns_roster_rc): $(printf '%s' "$netns_roster" | tail -1 | sed "s|$ROOT/\{0,1\}|<root>/|g")"

if row_omitted unit-suite; then
	: # a scoped run; the verdict line names this row as one it did not run
elif [ "$netns_roster_rc" -ne 0 ] || [ "$netns_n" -eq 0 ]; then
	record "unit-suite" FAIL "$netns_unmeasured; the suite cannot be told which tests are the other row's"
else
	suite_start=$(date +%s)
	rc=0
	suite_raw="$(mktemp)"
	go test "${SUITE_ARGS[@]}" -skip "$netns_skip" ./... >"$suite_raw" 2>&1 || rc=$?
	suite_elapsed=$(($(date +%s) - suite_start))
	# THE PARTITION, MEASURED rather than restated (round 2, finding 3).
	#
	# This row used to say "N declared test(s), M of them held for the
	# netns-suite row" with both numbers read out of the roster — the same
	# roster that produced the -skip pattern. The sentence was therefore true
	# of the roster whatever the run did, and MEASURED 2026-09-05 by review:
	# with -skip naming a test no package declares, the netns tests ran HERE
	# too, the two rows stopped being a partition, the combined 52s still fit
	# the 60s ceiling, and this note still said nineteen were held.
	#
	# So "held" is now read off the run: -v makes go test name every test it
	# starts, and a netns name appearing in this list is the partition broken.
	# The direction the complement already closed is the other one (a test the
	# classifier does not recognise runs here rather than nowhere); this is the
	# direction where the FILTER, not the roster, is what failed.
	suite_ran_f="$(mktemp)"
	netns_owned_f="$(mktemp)"
	sed -n 's/^=== RUN[[:space:]]\{1,\}\([^ /]*\)$/\1/p' "$suite_raw" | LC_ALL=C sort -u >"$suite_ran_f"
	printf '%s\n' "$netns_roster" | grep . | LC_ALL=C sort -u >"$netns_owned_f"
	suite_ran_n="$(grep -c . <"$suite_ran_f" || true)"
	suite_netns_leak="$(LC_ALL=C comm -12 "$netns_owned_f" "$suite_ran_f" | tr '\n' ' ' | sed 's/ $//')"
	rm -f "$suite_ran_f" "$netns_owned_f"
	# The report every check below reads, with the per-test lines -v added
	# taken back out. Two reasons, and the second is not tidiness: 445 tests
	# make 200KB of PASS lines, and every `printf "$suite_out" | grep -q` in
	# this file and in the oracle's scenarios runs under `set -o pipefail` —
	# grep -q exits on the first match, printf dies of SIGPIPE, the pipeline
	# reports 141 and the check reads as its own opposite. MEASURED here: the
	# ok-package-line check inverted on the first run after -v was added.
	suite_out="$(grep -vE '^(=== (RUN|PAUSE|CONT)|[[:space:]]*--- PASS: )' "$suite_raw" || true)"
	rm -f "$suite_raw"
	# Every directory holding a _test.go file, as the import path go test prints.
	module="$(go list -m 2>/dev/null || true)"
	tested_dirs="$(find . -name '*_test.go' -not -path './.git/*' -printf '%h\n' | sort -u)"
	missing=""
	tested_n=0
	while IFS= read -r d; do
		[ -n "$d" ] || continue
		tested_n=$((tested_n + 1))
		case "$d" in
		.) ip="$module" ;;
		*) ip="$module/${d#./}" ;;
		esac
		if grep -qE "^\?[[:space:]]+${ip//./\\.}[[:space:]]+\[no test files\]" <<<"$suite_out"; then
			missing="$missing $ip"
		fi
	done <<-EOF
		$tested_dirs
	EOF

	# The population that actually matters, and the one the package check above
	# cannot see. MEASURED 2026-08-30 by review: with the package check in force,
	# ten of twenty-two test files could be build-tagged out — keeping one file per
	# package — taking the suite from 161 declared tests to 61 with every row
	# green. Package granularity was exactly the boundary: including wire's only
	# test file DID go red.
	#
	# So: every test function DECLARED in a _test.go file must appear in
	# `go test -list`. internal/tools/testroster derives the declarations by
	# walking the filesystem and parsing, which is what makes it independent of the
	# build constraints that hid those ten files. It exits non-zero on an empty
	# roster rather than printing nothing, because "every declared test ran" is
	# vacuously true of no declarations at all.
	#
	# BOUNDS, stated rather than a completeness claim:
	#   - `go test -list` honours build constraints and the walk does not, so this
	#     is exact only while every _test.go builds on the host running it. The
	#     tree has no platform-conditional test file that fails to build here
	#     today; a _windows_test.go would read as declared-and-not-listed.
	#
	# ROUND 9 closes the bound this comment used to open with — "a test DELETED,
	# rather than disabled, leaves both sides agreeing" — and it is worth saying
	# how, because the reason it stood for two rounds was not difficulty. Both
	# sides were DERIVED FROM THE TREE, so deleting a test moved both of them at
	# once. MIN_DECLARED_TESTS is a literal in the manifest, derived from nothing,
	# and a literal does not move when the tree does. Scenario
	# min-declared-tests-floor.
	roster_out=""
	roster_rc2=0
	roster_out="$(go run ./internal/tools/testroster "$ROOT" 2>&1)" || roster_rc2=$?
	declared_n=0
	undeclared=""
	unlisted=""
	if [ "$roster_rc2" -eq 0 ]; then
		listed_f="$(mktemp)"
		declared_f="$(mktemp)"
		printf '%s\n' "$roster_out" | LC_ALL=C sort -u >"$declared_f"
		go test -list '.*' ./... 2>/dev/null | grep -E '^(Test|Benchmark|Fuzz|Example)' | LC_ALL=C sort -u >"$listed_f" || true
		declared_n="$(grep -c . <"$declared_f" || true)"
		unlisted="$(LC_ALL=C comm -23 "$declared_f" "$listed_f" | tr '\n' ' ' | sed 's/ $//')"
		undeclared="$(LC_ALL=C comm -13 "$declared_f" "$listed_f" | tr '\n' ' ' | sed 's/ $//')"
		rm -f "$listed_f" "$declared_f"
	fi

	if [ "$rc" -ne 0 ]; then
		record "unit-suite" FAIL "exit $rc after ${suite_elapsed}s: $(printf '%s\n' "$suite_out" | grep -E '^(--- FAIL|FAIL|panic:|WARNING: DATA RACE|.*test timed out)' | head -1 | sed 's/^[[:space:]]*//')"
		printf '\n--- unit-suite FAILED ---\n%s\n' "$suite_out" >&2
	elif grep -q '(cached)' <<<"$suite_out"; then
		record "unit-suite" FAIL "go test reported a cached result; -count=1 was not in force, so the suite was not measured on this tree"
		printf '\n--- unit-suite was served from the test cache ---\n%s\n' "$suite_out" >&2
	elif [ -z "$module" ] || [ "$tested_n" -eq 0 ]; then
		# The two ways this row's own domain check can fail to be computed. Both
		# are a refusal, because a comparison against an empty population passes
		# for the same reason a suite with no tests does.
		record "unit-suite" FAIL "the suite exited 0, but its domain is UNMEASURED (module '$module', $tested_n director(ies) holding a _test.go file)"
	elif ! grep -q '^ok[[:space:]]' <<<"$suite_out"; then
		record "unit-suite" FAIL "the suite exited 0 but reported no 'ok' package line; its output was not the account of a run"
		printf '\n--- unit-suite produced no ok line ---\n%s\n' "$suite_out" >&2
	elif [ -n "$missing" ]; then
		record "unit-suite" FAIL "exited 0, but package(s)${missing} hold a _test.go file and ran no test"
		printf '\n--- unit-suite: these packages have test files that did not run ---\n%s\n%s\n' "$missing" "$suite_out" >&2
	elif [ "$roster_rc2" -ne 0 ]; then
		# The tail is rewritten relative to the tree root before it is recorded.
		# docs/gates.md already requires diagnostics to name positions relative to
		# the root, and this one carried the absolute path of whatever directory it
		# was run in — which, under the oracle, is a fresh mktemp name. MEASURED
		# 2026-08-30 running the oracle twice: this was the ONE observation in
		# sixty that differed between runs, and it is the one thing that would have
		# made the replay above unusable.
		record "unit-suite" FAIL "the declared-test roster is UNMEASURED (testroster exit $roster_rc2): $(printf '%s' "$roster_out" | tail -1 | sed "s|$ROOT/\{0,1\}|<root>/|g")"
	elif [ -n "$unlisted" ]; then
		record "unit-suite" FAIL "declared but never run: $unlisted"
		printf '\n--- unit-suite: these tests are declared in a _test.go file and go test did not list them ---\n%s\n' "$unlisted" >&2
	elif [ -n "$undeclared" ]; then
		# The other direction, and it is an INSTRUMENT failure rather than a suite
		# failure: go test found a test the walk did not. Reported so that the
		# comparison cannot quietly become one-sided.
		record "unit-suite" FAIL "go test listed test(s) the declaration walk did not find: $undeclared"
	elif [ "$declared_n" -lt "$MIN_DECLARED_TESTS" ]; then
		record "unit-suite" FAIL "$declared_n declared test(s), below the floor of $MIN_DECLARED_TESTS in verify.manifest.sh; tests were deleted, and every check above this one derives its population from the tree and so moved with them"
	elif [ "$declared_n" -gt "$((MIN_DECLARED_TESTS + MAX_DECLARED_MARGIN))" ]; then
		# ROUND 11. This was a FLOOR with no upper edge, and MEASURED 2026-08-30 by
		# review: nothing in the tree raised it, so its protection eroded with every
		# test added and nobody would ever see the decay. Today's margin was zero,
		# the strongest it would ever be.
		#
		# A strict equality was tried first and is what put this comment here: it
		# fails every oracle scenario that plants a Go test into its own copy, which
		# is three of them plus the ceiling helper. The band keeps the lower edge
		# exactly where it was and caps the erosion at MAX_DECLARED_MARGIN instead
		# of leaving it unbounded. Scenario min-declared-tests-margin.
		record "unit-suite" FAIL "$declared_n declared test(s) against $MIN_DECLARED_TESTS (+$MAX_DECLARED_MARGIN) in verify.manifest.sh; tests were ADDED — set MIN_DECLARED_TESTS=$declared_n, because a floor with margin is a floor that has started to decay"
	elif [ "$suite_ran_n" -eq 0 ]; then
		# The vacuity floor for the two arms below. An empty list makes "no
		# netns name ran here" true of nothing, which is the shape every one of
		# rounds 5 to 8 ended in.
		record "unit-suite" FAIL "the suite exited 0 and named no test it started, so WHICH tests ran here is UNMEASURED and the partition against the netns row cannot be read from this run"
	elif [ -n "$suite_netns_leak" ]; then
		record "unit-suite" FAIL "this run started test(s) the netns-suite row owns: $suite_netns_leak; the -skip filter did not hold them back, so the two rows are not a partition and their seconds land on the wrong ceiling"
		printf '\n--- unit-suite ran tests belonging to the netns row ---\n%s\n' "$suite_netns_leak" >&2
	elif [ "$suite_elapsed" -gt "$SUITE_CEILING_SECONDS" ]; then
		# LAST of the failing arms, and it moved here in round 2 with the arm
		# above it. The ceiling asks "has this suite drifted into waiting"; a
		# suite that is running the other row's tests has not drifted into
		# anything, it is measuring the wrong population, and reporting the
		# clock for it would name the symptom over the cause. Every arm above
		# is a statement about WHAT ran; this one is about how long it took.
		record "unit-suite" FAIL "passed but took ${suite_elapsed}s, over the ${SUITE_CEILING_SECONDS}s ceiling"
		echo "--- unit-suite exceeded the T2 wall-clock ceiling: something is waiting ---" >&2
	else
		record "unit-suite" PASS "${suite_elapsed}s, ceiling ${SUITE_CEILING_SECONDS}s, $declared_n declared test(s), $suite_ran_n started here and none of the $netns_n the netns-suite row owns, across $tested_n package(s)" "$declared_n"
	fi
fi

# ------------------------------------------------------------- netns suite --
# The other half of the partition: the tests that re-execute themselves into a
# user and network namespace and wait out real DHCP and ARP intervals with a
# real dnsmasq.
#
# They are OUTER-only, like the oracle and the self-drive: an inner run is one
# of sixty-odd copies of this tree, and sixty-odd sets of namespaces and
# dnsmasq processes is not a cost the oracle can carry. What that leaves open
# is stated in the handover: this row is driven by outer scenarios only — which
# ones is written down in MANIFEST_SCENARIO_CONTRACTS and not counted here —
# and a netns test failing for a PRODUCT reason is driven by the real run.
#
# The row's own domain check is set equality against the roster, not a count:
# a -run regexp that matches nothing produces an exit 0 and an empty table, and
# a count of what ran is a count of what the regexp happened to select. A SKIP
# is a FAIL here — these tests fail closed rather than skipping when the kernel
# or dnsmasq is missing, so a --- SKIP line is one of them having been made
# quiet.
if [ "$INNER" -eq 1 ] || row_omitted netns-suite; then
	: # not an omission the verdict can be read past: see the roster check
elif [ "$netns_roster_rc" -ne 0 ] || [ "$netns_n" -eq 0 ]; then
	record "netns-suite" FAIL "$netns_unmeasured"
else
	netns_start=$(date +%s)
	rc=0
	netns_out="$(go test "${NETNS_ARGS[@]}" -run "$netns_run" ./runtime/ 2>&1)" || rc=$?
	netns_elapsed=$(($(date +%s) - netns_start))
	# Column 0 only: a subtest's own --- PASS is indented, and so is the
	# child's output, which the parent streams through its own log.
	netns_reported_f="$(mktemp)"
	netns_wanted_f="$(mktemp)"
	printf '%s\n' "$netns_out" | sed -n 's/^--- \(PASS\|FAIL\|SKIP\): \([^ ]*\).*/\2/p' | LC_ALL=C sort -u >"$netns_reported_f"
	printf '%s\n' "$netns_roster" | grep . | LC_ALL=C sort -u >"$netns_wanted_f"
	netns_skipped="$(printf '%s\n' "$netns_out" | sed -n 's/^--- SKIP: \([^ ]*\).*/\1/p' | tr '\n' ' ' | sed 's/ $//')"
	netns_absent="$(LC_ALL=C comm -23 "$netns_wanted_f" "$netns_reported_f" | tr '\n' ' ' | sed 's/ $//')"
	netns_surplus="$(LC_ALL=C comm -13 "$netns_wanted_f" "$netns_reported_f" | tr '\n' ' ' | sed 's/ $//')"
	rm -f "$netns_reported_f" "$netns_wanted_f"
	if [ "$rc" -ne 0 ]; then
		record "netns-suite" FAIL "exit $rc after ${netns_elapsed}s: $(printf '%s\n' "$netns_out" | grep -E '^(--- FAIL|FAIL|panic:|.*test timed out)' | head -1 | sed 's/^[[:space:]]*//')"
		printf '\n--- netns-suite FAILED ---\n%s\n' "$netns_out" >&2
	elif [ -n "$netns_skipped" ]; then
		record "netns-suite" FAIL "namespaced test(s) skipped themselves: $netns_skipped; these tests fail closed rather than skipping, so a skip is one of them having been silenced"
	elif [ -n "$netns_absent" ]; then
		record "netns-suite" FAIL "the roster names $netns_n namespaced test(s) and the run reported no verdict for: $netns_absent"
		printf '\n--- netns-suite ran fewer tests than the roster names ---\n%s\n' "$netns_out" >&2
	elif [ -n "$netns_surplus" ]; then
		record "netns-suite" FAIL "the run reported test(s) the netns roster does not name: $netns_surplus; the two rows' populations are no longer a partition"
	elif [ "$netns_elapsed" -gt "$NETNS_CEILING_SECONDS" ]; then
		record "netns-suite" FAIL "passed but took ${netns_elapsed}s, over the ${NETNS_CEILING_SECONDS}s netns ceiling"
		echo "--- netns-suite exceeded its wall-clock ceiling: something is waiting longer than the intervals it waits out ---" >&2
	else
		record "netns-suite" PASS "${netns_elapsed}s, ceiling ${NETNS_CEILING_SECONDS}s, $netns_n namespaced test(s) each reported its own verdict" "$netns_n"
	fi
fi

# ------------------------------------------------------------- self-oracle --
# See scripts/test-verify.sh for what this can and cannot see.
#
# The expectation is the MANIFEST'S scenario list, and the oracle has to
# account for every name in it BY NAME.
#
# The history is worth keeping because the previous two versions of this block
# were each written as the closure of the one before, and each was defeated the
# same way. Round 6: `verify-oracle PASS` on an oracle replaced by `exit 0`,
# because the only thing checking the oracle was the oracle. Round 7 answered
# that by counting `sc_` definitions HERE — which is still a count taken from
# the file being checked, so round 8 replaced the oracle with 45 empty
# `sc_fakeN(){}` stubs plus one echo and got `VERDICT: PASS (11 steps)`.
#
# A count over the subject can always be satisfied by the subject. Names from
# the manifest cannot: a stub now has to reproduce fifty-three specific
# scenario names, none of which are written in the file it replaced.
#
# N5, corrected: the comment here used to say the total stub — `#!/bin/sh` +
# `exit 0` — was closed by "no sc_ definitions → expected 0 → refused as an
# empty domain". It is not. That stub prints nothing, so the branch that fires
# is the one below testing for a missing `ORACLE PASS: <n> scenarios` line, and
# scenario oracle-stub-total asserts exactly that diagnosis. Naming the wrong
# branch is the same defect as a wrong line number: it survives because the
# scenario passes either way.
#
# BOUND, and it is real: a stub that READS THE MANIFEST and prints a correct
# RESULT line per name defeats this. That is strictly harder than the stub that
# defeated round 7 — which needed no knowledge of anything — and it is named
# here rather than argued away. What actually stops it is the same thing that
# stops any coordinated edit: somebody reading the diff.
# --------------------------------------------------------------- self-drive --
# The arbiter detecting planted defects END TO END, itself, without the oracle.
# See SELF_DRIVE_REDDENS in verify.manifest.sh for why this exists.
if [ "$INNER" -eq 0 ]; then
	sd_dir="$(mktemp -d)"
	sd_bad=""
	mkdir -p "$sd_dir/tree"
	tar -cf - -C "$ROOT" --exclude=./.git . | tar -xf - -C "$sd_dir/tree"

	# One plant per row in SELF_DRIVE_REDDENS, each chosen so it cannot cascade
	# into the rows in SELF_DRIVE_SURVIVES: an unformatted but compiling file,
	# an unused variable appended to a script that is already linted, a comment
	# citing a test that does not exist, and a number an instrument owns.
	printf 'package wire\n\nfunc  selfDriveIsNotFormatted( ) {}\n' >"$sd_dir/tree/wire/selfdrive_plant.go"
	printf '\nself_drive_unused_variable=1\n' >>"$sd_dir/tree/scripts/sweep-doc-numbers.sh"
	printf '\n// See TestSelfDriveCitedButNeverWritten.\n' >>"$sd_dir/tree/wire/selfdrive_plant.go"
	printf '\nCoverage today is 22 of the 102 allowlisted identifiers.\n' >>"$sd_dir/tree/README.md"
	printf 'package proto\n\nfunc selfDriveUnreachable() int {\n\treturn 0\n\treturn 1\n}\n' >"$sd_dir/tree/proto/selfdrive_vet.go"
	printf 'package proto\n\nimport _ "net"\n' >"$sd_dir/tree/proto/selfdrive_import.go"
	# A _test.go that names the clock without DECLARING a test: T2 must see it,
	# and the declared-test count must not move, or this plant would redden the
	# unit-suite row it is not testing.
	printf 'package proto\n\nimport "time"\n\nvar selfDriveClockBait = time.Sleep\n' >"$sd_dir/tree/proto/selfdrive_clock_test.go"

	sd_out="$(cd "$sd_dir/tree" && ./verify.sh --inner 2>&1 || true)"
	sd_row() { printf '%s\n' "$sd_out" | awk -v n="$1" '$1 == n { print $2; f = 1 } END { if (!f) print "ABSENT" }'; }

	for r in "${SELF_DRIVE_REDDENS[@]}"; do
		[ "$(sd_row "$r")" = FAIL ] || sd_bad="$sd_bad $r=$(sd_row "$r") (planted, did not redden);"
	done
	# The preservation half. An arbiter that reddens everything detects nothing.
	for r in "${SELF_DRIVE_SURVIVES[@]}"; do
		[ "$(sd_row "$r")" = PASS ] || sd_bad="$sd_bad $r=$(sd_row "$r") (unplanted, went red);"
	done
	rm -rf "$sd_dir"

	if [ -n "$sd_bad" ]; then
		record "self-drive" FAIL "the arbiter did not detect what it planted for itself:$sd_bad"
		printf '\n--- self-drive FAILED, the planted run said: ---\n' >&2
		quote_block "$sd_out" >&2
	else
		record "self-drive" PASS "planted ${#SELF_DRIVE_REDDENS[@]} defect(s), each reddened its own row; ${#SELF_DRIVE_SURVIVES[@]} unplanted row(s) stayed green" "$((SELF_DRIVE_REDDENS_N + SELF_DRIVE_SURVIVES_N))"
	fi
fi

# THE SKIP, added 2026-09-05 (machinery batch, item 1).
#
# The oracle is 1021s of a 19-minute run and it tests THIS FILE. Running it
# again on a tree where this file, the manifest and scripts/ are byte for byte
# what they were when it last passed re-measures a subject that has not moved.
#
# Keyed on CONTENT, not on a diff against a ref: a fresh clone at a commit that
# changed the arbiter has nothing to diff against and would skip. A stamp makes
# a fresh clone run the oracle once, which is the direction that matters.
#
# What binds the stamp to a real pass, in order:
#   - it is written in ONE place, on the far side of every check the row makes,
#     and only when record() ACCEPTED the pass (a rewritten PASS writes no
#     stamp; scenario oracle-skip-needs-a-real-pass drives a stub that exits 0
#     without the ORACLE PASS line, twice);
#   - it names the ROOT it was written for, so a stamp copied into another tree
#     — every oracle scenario copies this one — grants nothing there;
#   - it is gitignored, so it never travels with a commit;
#   - --oracle ignores it entirely.
#
# THE BOUND, and it is not closable here: the stamp is a local cache and not
# evidence. One written BY HAND with the right hash grants a skip, exactly as
# editing any other single file in this tree defeats the thing that file
# decides — any hand can compute what this block computes. What is closed is
# the accident and the cheap forgery, not the deliberate one, and the merge
# rule is a reviewer's own --oracle run at the head rather than a stamp.
#
# The second bound is the one to read before trusting a skip: this hash covers
# the ARBITER, and scenarios that depend on the PRODUCT's shape can go stale
# without any covered byte changing. Which scenarios those are is not typed
# here or in the doc — a typed list omitted eleven of them, MEASURED
# 2026-09-05 by review. It is derived from the oracle and quoted in
# docs/verifying.md, and TestStaleAnchorBoundNamesWhatTheOracleDerives fails
# when the derivation and the quote disagree.
if [ "$INNER" -eq 0 ]; then
	ORACLE_STAMP="$ROOT/.verify-oracle-stamp"
	oracle_covered="$( {
		printf 'verify.sh\nverify.manifest.sh\n'
		find scripts -type f -printf '%p\n' 2>/dev/null
	} | LC_ALL=C sort -u)"
	oracle_covered_n="$(printf '%s\n' "$oracle_covered" | grep -c . || true)"
	# The PATHS are hashed beside the bytes: a renamed script is a changed
	# arbiter, and a hash over contents alone cannot see a rename.
	oracle_hash="$(printf '%s\n' "$oracle_covered" | while IFS= read -r f; do
		if [ -r "$ROOT/$f" ]; then
			printf '%s  %s\n' "$(sha256sum <"$ROOT/$f" | cut -d' ' -f1)" "$f"
		else
			printf 'ABSENT  %s\n' "$f"
		fi
	done | sha256sum | cut -d' ' -f1)"
	stamp_root=""
	stamp_hash=""
	stamp_scn=""
	if [ -r "$ORACLE_STAMP" ]; then
		stamp_root="$(sed -n 's/^root //p' "$ORACLE_STAMP" | head -1)"
		stamp_hash="$(sed -n 's/^hash //p' "$ORACLE_STAMP" | head -1)"
		stamp_scn="$(sed -n 's/^scenarios //p' "$ORACLE_STAMP" | head -1)"
	fi
	if [ "$FORCE_ORACLE" -eq 0 ] &&
		[ -n "$stamp_hash" ] &&
		[ "$stamp_root" = "$ROOT" ] &&
		[ "$stamp_hash" = "$oracle_hash" ] &&
		[ "$stamp_scn" = "${#MANIFEST_SCENARIOS[@]}" ]; then
		record "verify-oracle" SKIPPED "${oracle_covered_n} arbiter file(s) — verify.sh, verify.manifest.sh, scripts/ — hash ${oracle_hash:0:16}, which already produced ORACLE PASS: $stamp_scn scenarios in this tree; ./verify.sh --oracle runs it regardless" "$oracle_covered_n"
	elif [ -x "$ROOT/scripts/test-verify.sh" ]; then
		orc_start=$(date +%s)
		orc_rc=0
		orc_out="$("$ROOT/scripts/test-verify.sh" 2>&1)" || orc_rc=$?
		orc_elapsed=$(($(date +%s) - orc_start))
		oracle_reported="$(printf '%s\n' "$orc_out" | sed -n 's/^ORACLE PASS: \([0-9][0-9]*\) scenarios.*/\1/p' | tail -1)"
		accounted=0
		unaccounted=""
		breached=""
		# ROUND 11. Each scenario is held to what it must have OBSERVED, not to
		# its name appearing in a line. The contract comes from the manifest,
		# the observation from the oracle, and the comparison happens here — so
		# it is in none of the three places an author would edit to make a
		# scenario stop working.
		for contract in "${MANIFEST_SCENARIO_CONTRACTS[@]}"; do
			IFS='|' read -r sc_name want_rc want_tok want_diag <<<"$contract"
			if ! printf '%s\n' "$orc_out" | grep -qE "^[[:space:]]*RESULT $sc_name PASS obs="; then
				unaccounted="$unaccounted $sc_name"
				continue
			fi
			accounted=$((accounted + 1))
			got="$(printf '%s\n' "$orc_out" | sed -n "s/^[[:space:]]*RESULT $sc_name PASS obs=//p" | tail -1)"
			sc_ok=1
			case "$want_rc" in
			zero) printf '%s' ",$got," | grep -q ',rc:0,' || sc_ok=0 ;;
			nonzero) printf '%s' ",$got," | grep -qE ',rc:[1-9][0-9]*,' || sc_ok=0 ;;
			static) [ -n "$got" ] || sc_ok=0 ;;
			*) sc_ok=0 ;;
			esac
			# Unconditional. The "-" escape this used to carry meant "demand no
			# observation", which is one manifest entry away from the defeat
			# this whole check answers.
			printf '%s' ",$got," | grep -q ",$want_tok," || sc_ok=0
			# ROUND 13, B15. The row's own ACCOUNT of what it found, not only
			# that it went red. A scenario cut down to the lines producing its
			# contracted observation, planting whatever reaches the same row,
			# satisfied everything up to here — because a verdict names a row
			# and nothing named the defect. The note is written by the arbiter,
			# so the scenario cannot supply it by planting something else.
			case "$want_tok" in
			*:FAIL | *:PASS | *:ABSENT)
				sc_row="${want_tok%%:*}"
				# Every note recorded for that row, not the first: a scenario
				# may run the subject more than once (oracle-is-invoked runs a
				# stub and then the real thing), and the reading that carries
				# the diagnosis is not always the first one.
				sc_note="$(printf '%s' "$got" | tr ',' '\n' | sed -n "s/^why:$sc_row://p")"
				printf '%s\n' "$sc_note" | grep -qF -- "$want_diag" || {
					sc_ok=0
					want_tok="$want_tok/$want_diag"
				}
				;;
			esac
			# The SCOPE the scenario ran at, against the manifest's
			# declaration of the scope it is entitled to (item 2, 2026-09-05).
			# The scope is set by the oracle's dispatcher from that same list
			# and recorded by the run helpers; a body that scopes itself down
			# to skip the row it exists to drive reports a scope the manifest
			# does not declare for it, and is a breach here — beside the older
			# and stronger refusal, which is that the row it scoped away then
			# reads ABSENT and fails its own contract.
			#
			# static scenarios never run verify.sh, so there is no scope to
			# observe; they are exempt by rc-class, not by name.
			if [ "$want_rc" != static ]; then
				want_scope=full
				in_list "$sc_name" "${MANIFEST_LIGHT_SCENARIOS[@]}" && want_scope=light
				printf '%s' ",$got," | grep -q ",scope:$want_scope," || {
					sc_ok=0
					want_tok="$want_tok/scope:$want_scope"
				}
			fi
			[ "$sc_ok" -eq 1 ] || breached="$breached $sc_name(wants $want_rc,$want_tok; observed [$got])"
		done
		if [ "$orc_rc" -ne 0 ]; then
			# The breach list rides along rather than waiting its turn. Both
			# statements are true at once, and MEASURED 2026-08-30 replaying
			# B14: the oracle's own exit 1 arrived first and "exit 1" was the
			# entire diagnosis, while four scenarios had been emptied and were
			# reporting nothing — the finding the operator most needed.
			# ROUND 13, N14. "exit 137" is not a diagnosis. The oracle's own
			# refusal line names WHICH scenario went silent, and dropping it
			# here left that naming reachable, correct and unread.
			# `|| true`, and it is not cosmetic: pipefail plus set -e means a
			# grep that matches nothing ABORTS the run, and an aborted run
			# records no row at all. That is how this line first shipped, and
			# the scenario below caught it as verify-oracle:ABSENT.
			orc_detail="$(printf '%s\n' "$orc_out" | grep -E '^ORACLE (REFUSED|FAIL)' | tail -1 || true)"
			# NAME them in the ROW, not only in the dump. "1 of 63 scenarios
			# did not behave" is the same defect as the population count that
			# would not say which scenario went silent: it is true, and it
			# sends the reader to a diff. MEASURED 2026-08-30: one such run
			# happened here and the name was unrecoverable afterwards, because
			# the reader had captured the table and not the dump.
			orc_failed="$(printf '%s\n' "$orc_out" |
				sed -n 's/^[[:space:]]*RESULT \([^ ]*\) FAIL.*/\1/p' | tr '\n' ' ' | sed 's/ $//')"
			record "verify-oracle" FAIL "exit $orc_rc${orc_detail:+: $orc_detail}${orc_failed:+; the scenario(s) that did not behave: $orc_failed}${breached:+; and scenario(s) reported PASS without observing what verify.manifest.sh says they must:$breached}"
			printf '\n--- verify-oracle FAILED (exit %s) ---\n' "$orc_rc" >&2
			quote_block "$orc_out" >&2
		elif [ -z "$oracle_reported" ]; then
			record "verify-oracle" FAIL "the oracle exited 0 but printed no 'ORACLE PASS: <n> scenarios' line; its answer is not the account of a run"
			printf '\n--- verify-oracle produced no verdict line ---\n' >&2
			quote_block "$orc_out" >&2
		elif [ -n "$unaccounted" ]; then
			record "verify-oracle" FAIL "the oracle reported no passing result for scenario(s)$unaccounted, which verify.manifest.sh requires; $accounted of ${#MANIFEST_SCENARIO_CONTRACTS[@]} were accounted for by name"
			printf '\n--- verify-oracle did not account for every declared scenario ---\n' >&2
			quote_block "$orc_out" >&2
		elif [ -n "$breached" ]; then
			record "verify-oracle" FAIL "scenario(s) reported PASS without observing what verify.manifest.sh says they must:$breached"
			printf '\n--- verify-oracle: a scenario passed without doing its job ---\n' >&2
			quote_block "$orc_out" >&2
		elif [ "$oracle_reported" != "${#MANIFEST_SCENARIOS[@]}" ]; then
			record "verify-oracle" FAIL "the oracle reports $oracle_reported scenario(s); verify.manifest.sh declares ${#MANIFEST_SCENARIOS[@]}"
			printf '\n--- verify-oracle ran a different set than the manifest declares ---\n' >&2
			quote_block "$orc_out" >&2
		elif [ "$orc_elapsed" -lt "$ORACLE_MIN_SECONDS" ]; then
			# LAST on purpose: a duration is the weakest thing said about this
			# row, and it must never displace a diagnosis that names a specific
			# scenario. It is here only so that the cheap defeat — a fake that
			# returns instantly — is not also the quiet one.
			record "verify-oracle" FAIL "the oracle answered in ${orc_elapsed}s, under the ${ORACLE_MIN_SECONDS}s floor in verify.manifest.sh; it reported the right account without doing the work"
			printf '\n--- verify-oracle returned too fast to have run ---\n' >&2
			quote_block "$orc_out" >&2
		else
			record "verify-oracle" PASS "$(printf '%s\n' "$orc_out" | tail -1), ${orc_elapsed}s" "$accounted"
			# The one place the stamp is written, and it is written only if
			# the row above it was ACCEPTED: record() rewrites an uncountable
			# PASS to FAIL, and a stamp written before that check would record
			# a pass the arbiter refused.
			if [ "${RESULTS[${#RESULTS[@]} - 1]}" = PASS ]; then
				printf 'root %s\nhash %s\nscenarios %s\nfiles %s\nwritten %s\n' \
					"$ROOT" "$oracle_hash" "$oracle_reported" "$oracle_covered_n" \
					"$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >"$ORACLE_STAMP"
			fi
		fi
	else
		record "verify-oracle" FAIL "scripts/test-verify.sh is missing or not executable; verify.sh was not itself checked"
	fi
fi

# ------------------------------------------------------------------ verdict --
echo
echo "step                 result  detail"
echo "----                 ------  ------"
for i in "${!NAMES[@]}"; do
	printf '%-20s %-7s %s\n' "${NAMES[$i]}" "${RESULTS[$i]}" "${NOTES[$i]}"
done
echo

# The roster cross-check. --inner does not run the oracle, so the expected set
# is the declared one minus verify-oracle in that mode; the row is expected
# exactly when it is meant to have run.
# The rows an inner run does not have (MANIFEST_OUTER_ROWS) and the rows a
# scoped run leaves out (MANIFEST_SCOPED_OUT_ROWS) come from the manifest
# rather than from a case arm here: two lists that say which rows exist under
# which flag, in the file that is pinned from Go, and one comparison.
expected_rows=()
omitted_rows=()
for r in "${REQUIRED_ROWS[@]}"; do
	if [ "$INNER" -eq 1 ] && in_list "$r" "${MANIFEST_OUTER_ROWS[@]}"; then continue; fi
	if row_omitted "$r"; then
		omitted_rows+=("$r")
		continue
	fi
	expected_rows+=("$r")
done
rows_expected="$(printf '%s\n' "${expected_rows[@]}" | LC_ALL=C sort | tr '\n' ' ' | sed 's/ $//')"
rows_present="$(printf '%s\n' "${NAMES[@]}" | LC_ALL=C sort | tr '\n' ' ' | sed 's/ $//')"

if [ "${#REQUIRED_ROWS[@]}" -eq 0 ]; then
	echo "VERDICT: FAIL — MANIFEST_ROWS is empty, so the roster check measured nothing." >&2
	VERDICT_PRINTED=1
	exit 1
fi
if [ "$rows_expected" != "$rows_present" ]; then
	echo "VERDICT: FAIL — the rows recorded are not the rows required." >&2
	echo "  required: [$rows_expected]" >&2
	echo "  recorded: [$rows_present]" >&2
	VERDICT_PRINTED=1
	exit 1
fi

# A scoped run says so ON THE VERDICT LINE. "VERDICT: PASS" read off a run
# that did not run the suite is the whole risk of item 2, and the answer is
# that the line cannot be read without the omission.
scope_note=""
[ "${#omitted_rows[@]}" -eq 0 ] || scope_note=", SCOPED: ${omitted_rows[*]} not run"

VERDICT_PRINTED=1
if [ "$FAILED" -eq 0 ]; then
	echo "VERDICT: PASS (${#NAMES[@]} steps$scope_note)"
	exit 0
fi
echo "VERDICT: FAIL${scope_note}"
exit 1
