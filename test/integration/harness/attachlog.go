// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag, for
// the reason counterwindow.go gives: what is read out of the plugin log
// decides what #403's distribution says, so the reading has to be
// drivable without a live plugin and without root.
//
// The integration package refuses to run as a non-root user, so a
// parser that lives there is never executed by anything a change can
// run locally. Both defects this parser has already had were of that
// kind: a backreference Go's RE2 rejects, which is a panic in init that
// takes every test in the package with it, and a quoted-value mismatch
// that counted a phase summary that was present as missing.

package harness

import (
	"regexp"
	"strings"
	"time"
)

var (
	attachLine = regexp.MustCompile(`Attach completed`)
	// Both fields accept a quoted or a bare value, as ALTERNATIVES and
	// not as a backreference: Go's regexp is RE2 and rejects `\1`.
	//
	// logrus quotes a value that contains a space, so `took` arrives
	// bare and `phases` — a space-separated list of name=1.23s —
	// arrives quoted. A pattern that assumed either one would miss
	// half of what it counts.
	tookField   = regexp.MustCompile(`took=("[^"]*"|\S+)`)
	phasesField = regexp.MustCompile(`phases=("[^"]*"|\S*)`)
)

// AttachDurations pulls the elapsed time out of every successful-attach
// line in a plugin log.
func AttachDurations(log string) []time.Duration {
	var out []time.Duration
	for _, line := range strings.Split(log, "\n") {
		if !attachLine.MatchString(line) {
			continue
		}
		m := tookField.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		d, err := time.ParseDuration(unquoteField(m[1]))
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out
}

// MalformedAttachLines counts attach lines whose duration did not
// parse. Counted separately from the durations so a parse that
// silently drops half the population cannot read as a small run.
func MalformedAttachLines(log string) int {
	var bad int
	for _, line := range strings.Split(log, "\n") {
		if !attachLine.MatchString(line) {
			continue
		}
		m := tookField.FindStringSubmatch(line)
		if m == nil {
			bad++
			continue
		}
		if _, err := time.ParseDuration(unquoteField(m[1])); err != nil {
			bad++
		}
	}
	return bad
}

// AttachLinesWithoutPhases counts successful-attach lines whose phase
// breakdown is missing or empty. Separate from the duration parse: a
// line can carry a good duration and no phases, and that is exactly
// what the success-side phase recording being dropped looks like.
func AttachLinesWithoutPhases(log string) int {
	var n int
	for _, line := range strings.Split(log, "\n") {
		if !attachLine.MatchString(line) {
			continue
		}
		m := phasesField.FindStringSubmatch(line)
		v := ""
		if m != nil {
			v = unquoteField(m[1])
		}
		if m == nil || strings.TrimSpace(v) == "" || strings.Contains(v, "no phase completed") {
			n++
		}
	}
	return n
}

// Percentile is the nearest-rank percentile of an already sorted slice.
func Percentile(sorted []time.Duration, p int) string {
	if len(sorted) == 0 {
		return "n/a"
	}
	i := (p*len(sorted) + 99) / 100
	if i < 1 {
		i = 1
	}
	if i > len(sorted) {
		i = len(sorted)
	}
	return sorted[i-1].String()
}

// unquoteField strips the quotes logrus puts around a value containing
// a space. A quoted value compared with its quotes reads as a different
// string, which is how a phase summary that IS present gets counted as
// missing.
func unquoteField(v string) string {
	return strings.Trim(v, `"`)
}
