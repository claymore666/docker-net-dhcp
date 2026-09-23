// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: the #403 attach-log parser must run without a live plugin or root.

package harness

import (
	"regexp"
	"strings"
	"time"
)

var (
	attachLine = regexp.MustCompile(`Attach completed`)
	// logrus quotes a value containing a space, so `took` arrives bare and `phases` quoted; RE2 rejects a `\1`
	// backreference, so the forms are alternatives (#403, #417).
	tookField   = regexp.MustCompile(`took=("[^"]*"|\S+)`)
	phasesField = regexp.MustCompile(`phases=("[^"]*"|\S*)`)
)

// AttachDurations returns the elapsed time of every successful-attach line.
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

// MalformedAttachLines counts attach lines whose duration did not parse.
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

// AttachLinesWithoutPhases counts successful-attach lines with a missing or empty phase breakdown (#417).
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

// unquoteField strips the quotes logrus puts around a value containing a space.
func unquoteField(v string) string {
	return strings.Trim(v, `"`)
}
