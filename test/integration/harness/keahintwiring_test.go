// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, deliberately. This guard reads a
// source file and needs neither root nor a live Kea, so it belongs in
// the ordinary `go test ./...` job where it fails in seconds rather
// than after a twelve-minute suite -- and, more to the point, where it
// runs at all on a host that cannot run the integration suite. Same
// reasoning counterwindow_guard_test.go documents for itself.

package harness

import (
	"os"
	"strings"
	"testing"
)

// The readiness failure the whole of #869 exists to explain. Matched as
// a string because it is the user-visible text; if it changes, this
// guard must be pointed at the new one deliberately rather than quietly
// matching nothing.
const keaReadinessFailure = "ephemeral kea did not become ready"

// enclosingCallStatement returns the whole call expression whose first
// line contains marker: from the start of that line to the parenthesis
// that closes the call.
//
// Parens inside string literals, rune literals and line comments do not
// count -- the format string this is used on is full of text, and a
// naive depth count would stop in the middle of it.
func enclosingCallStatement(src, marker string) (string, bool) {
	i := strings.Index(src, marker)
	if i < 0 {
		return "", false
	}
	start := strings.LastIndexByte(src[:i], '\n') + 1

	depth, opened := 0, false
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '"', '\'', '`':
			quote := src[j]
			for j++; j < len(src); j++ {
				if src[j] == '\\' && quote != '`' {
					j++
					continue
				}
				if src[j] == quote {
					break
				}
			}
		case '/':
			if j+1 < len(src) && src[j+1] == '/' {
				nl := strings.IndexByte(src[j:], '\n')
				if nl < 0 {
					return src[start:], opened && depth == 0
				}
				j += nl
			}
		case '(':
			depth++
			opened = true
		case ')':
			depth--
			if opened && depth == 0 {
				return src[start : j+1], true
			}
		}
	}
	return "", false
}

// TestKeaHint_IsWiredIntoTheReadinessFailure is what actually delivers
// #869.
//
// Everything else about the hint -- the tier selection, the denial
// record, the wording of each claim -- is a pure function with unit
// tests, and every one of them stays green with the hint disconnected
// from the failure message. Deleting the argument from this Fatalf, or
// reverting it to the two-argument form it had before #869, restores
// the exact failure the issue was filed about: "did not become ready"
// with an empty log and no mention of AppArmor. Nothing went red.
//
// Static rather than behavioural on purpose. Reproducing the delivery
// end to end means a host with the kea package installed and its
// profile loaded in enforce mode, which is precisely the host CI is not
// and must not become. The property is textual, so it is checked
// textually.
func TestKeaHint_IsWiredIntoTheReadinessFailure(t *testing.T) {
	src, err := os.ReadFile("ephemeral.go")
	if err != nil {
		t.Fatalf("read ephemeral.go: %v", err)
	}

	stmt, ok := enclosingCallStatement(string(src), keaReadinessFailure)
	if !ok {
		t.Fatalf("ephemeral.go no longer contains a call carrying %q.\n"+
			"Either the readiness failure was reworded -- in which case update "+
			"keaReadinessFailure -- or it was removed. This guard must not be left "+
			"matching nothing: a guard whose subject has vanished passes vacuously.",
			keaReadinessFailure)
	}

	// Match the CALL, not the identifier. appArmorKeaHint is DEFINED in
	// keaconfine.go, so a whole-package search for the name finds it
	// with the call site deleted; and even within ephemeral.go a bare
	// Contains would be satisfied by a mention in a comment. Scoping to
	// the failing statement is what makes this about delivery.
	const wired = "appArmorKeaHint("
	if !strings.Contains(stmt, wired) {
		t.Errorf("the ephemeral Kea readiness failure no longer passes %s.\n"+
			"statement:\n%s\n"+
			"Without it the failure is back to what #869 was filed about: "+
			"\"did not become ready\" with an empty log, the word AppArmor nowhere in it, "+
			"and the cause only in the kernel log. Every unit test of the hint stays "+
			"green with this argument removed -- this guard is the only thing that "+
			"notices.", wired, stmt)
	}

	// The hint's empty-log claim is only true of the log the reader can
	// actually see, so both must come from ONE read. readLog returns
	// the whole file -- appended to across every Stop/StartAgain cycle
	// -- and a non-empty "(could not read ...)" string on error, so a
	// second read can disagree with the first and the hint would then
	// say "the log above is empty" underneath a log that is not.
	const secondRead = "ef.readLog()"
	if strings.Contains(stmt, secondRead) {
		t.Errorf("the readiness failure calls %s inside the message.\n"+
			"statement:\n%s\n"+
			"Read the log once into a variable and pass the same value to both the "+
			"message and appArmorKeaHint's logEmpty argument; otherwise the hint's "+
			"claim about the log is about a different read than the one printed.",
			secondRead, stmt)
	}
}

// TestKeaHint_GuardWouldCatchThePrePRForm is the negative control.
//
// A guard that has never been observed rejecting anything is not known
// to work, and this repo has already shipped one that passed with the
// call it was guarding deleted. Rather than corrupting ephemeral.go,
// this feeds the detector the exact shapes it exists to reject --
// including the two-argument form the Fatalf had before #869.
func TestKeaHint_GuardWouldCatchThePrePRForm(t *testing.T) {
	const preIssue869 = "\tef.t.Fatalf(\"ephemeral kea did not become ready; config:\\n%s\\nlog:\\n%s\",\n" +
		"\t\tef.renderedConfig, ef.readLog())\n"
	stmt, ok := enclosingCallStatement(preIssue869, keaReadinessFailure)
	if !ok {
		t.Fatal("the extractor cannot even find the pre-#869 statement; it would pass vacuously")
	}
	if strings.Contains(stmt, "appArmorKeaHint(") {
		t.Errorf("the detector accepts the pre-#869 form, which carried no hint:\n%s", stmt)
	}
	if !strings.Contains(stmt, "ef.readLog()") {
		t.Errorf("the second-read detector misses an inline readLog call:\n%s", stmt)
	}

	// A hint mentioned only in a comment beside the failure is not
	// delivery. The extractor must stop at the call, not swallow the
	// surrounding lines.
	const commentOnly = "\t// appArmorKeaHint(ef.tmpDir, true) used to be passed here.\n" +
		"\tef.t.Fatalf(\"ephemeral kea did not become ready; config:\\n%s\",\n" +
		"\t\tef.renderedConfig)\n"
	stmt, ok = enclosingCallStatement(commentOnly, keaReadinessFailure)
	if !ok {
		t.Fatal("the extractor lost the statement under a preceding comment")
	}
	if strings.Contains(stmt, "appArmorKeaHint(") {
		t.Errorf("a mention in a neighbouring comment satisfied the detector:\n%s", stmt)
	}

	// The current form must be accepted, or the guard is a check with
	// one possible verdict.
	const current = "\tkeaLog := ef.readLog()\n" +
		"\tef.t.Fatalf(\"ephemeral kea did not become ready; config:\\n%s\\nlog:\\n%s\\n%s\",\n" +
		"\t\tef.renderedConfig, keaLog, appArmorKeaHint(ef.tmpDir, keaLog == \"\"))\n"
	stmt, ok = enclosingCallStatement(current, keaReadinessFailure)
	if !ok {
		t.Fatal("the extractor cannot find the current statement")
	}
	if !strings.Contains(stmt, "appArmorKeaHint(") {
		t.Errorf("the detector rejects the shape it is supposed to accept:\n%s", stmt)
	}
	if strings.Contains(stmt, "ef.readLog()") {
		t.Errorf("the second-read detector fires on the single-read form:\n%s", stmt)
	}

	// A format string containing an unbalanced paren must not truncate
	// the statement -- that would silently drop the argument list and
	// turn this guard into a false red.
	const parenInString = "\tef.t.Fatalf(\"ephemeral kea did not become ready :-( config:\\n%s\",\n" +
		"\t\tef.renderedConfig, appArmorKeaHint(ef.tmpDir, true))\n"
	stmt, ok = enclosingCallStatement(parenInString, keaReadinessFailure)
	if !ok {
		t.Fatal("a paren inside the format string broke the extractor")
	}
	if !strings.Contains(stmt, "appArmorKeaHint(") {
		t.Errorf("the extractor stopped inside the string literal:\n%s", stmt)
	}
}
