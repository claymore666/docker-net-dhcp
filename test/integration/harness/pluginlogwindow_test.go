// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"strings"
	"testing"
)

// Verbatim from the arm64 lane's plugin log: the section 2.4 line is IPv4 ARP from an earlier TestConflictCheck_ test.
const (
	arpConflictLine = `level=warn msg="leased address is in use by another host (RFC 5227 section 2.4)" family=ipv4 endpoint=8f21c0a4`
	dadConflictLine = `level=warn msg="leased address is in use by another host (RFC 4862 section 5.4 Duplicate Address Detection)" family=ipv6 held=false endpoint=b1d0e77a`
)

func TestPluginLogWindow_ExcludesWhatCameBeforeTheMark(t *testing.T) {
	before := arpConflictLine + "\n"
	after := dadConflictLine + "\n"
	log := []byte(before + after)

	win := string(PluginLogWindow(log, int64(len(before))))

	if !strings.Contains(win, "RFC 4862 section 5.4") {
		t.Errorf("the window dropped the line written after the mark:\n%q", win)
	}
	if strings.Contains(win, "RFC 5227 section 2.4") {
		t.Errorf("the window carries a line written before the mark, so a negative assertion "+
			"over it judges another test's output:\n%q", win)
	}
}

func TestPluginLogWindow_MarkAtZeroIsTheWholeLog(t *testing.T) {
	log := []byte(arpConflictLine + "\n" + dadConflictLine + "\n")

	win := string(PluginLogWindow(log, 0))

	if !strings.Contains(win, "RFC 5227 section 2.4") {
		t.Error("a mark of zero must be the whole log; without that reading the fix has no " +
			"before-state and the test above proves nothing")
	}
}

func TestPluginLogWindow_EmptyWindowFailsThePositiveAssertions(t *testing.T) {
	log := []byte(arpConflictLine + "\n")

	win := string(PluginLogWindow(log, int64(len(log))))

	if win != "" {
		t.Errorf("want an empty window when nothing followed the mark, got %q", win)
	}
	if strings.Contains(win, "family=ipv6") {
		t.Error("an empty window cannot satisfy a positive assertion")
	}
}

func TestPluginLogWindow_TruncationIsEmptyHereAndWholeInLogSince(t *testing.T) {
	log := []byte(arpConflictLine + "\n")
	off := int64(len(log) + 4096)

	if got := string(PluginLogWindow(log, off)); got != "" {
		t.Errorf("an offset past the end means the log was replaced; the window must be empty, got %q", got)
	}
	if got := string(LogSince(log, off)); got != string(log) {
		t.Errorf("LogSince must still widen to the whole log for the census (#385, #406); got %q", got)
	}
}

func TestPluginLogWindow_NegativeOffsetIsEmpty(t *testing.T) {
	log := []byte(dadConflictLine + "\n")
	if got := string(PluginLogWindow(log, -1)); got != "" {
		t.Errorf("a negative offset is not a position in the log; want an empty window, got %q", got)
	}
}
