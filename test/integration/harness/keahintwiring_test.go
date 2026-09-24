// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: this guard reads source and runs in the unit job, on hosts that cannot run the suite (#869).

package harness

import (
	"os"
	"strings"
	"testing"
)

// keaReadinessFailure is the user-visible failure #869 explains.
const keaReadinessFailure = "ephemeral kea did not become ready"

// enclosingCallStatement returns the call whose first line contains marker, ignoring parens in string, rune and line-comment text.
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

// Static: delivering the hint end to end needs Kea installed with its AppArmor profile in enforce mode, which CI is not (#869).
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

	// The call, not the identifier: appArmorKeaHint is defined in keaconfine.go and could be named in a comment (#869).
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

	// The hint's empty-log claim and the printed log must come from one read: readLog can differ between reads (#869).
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
