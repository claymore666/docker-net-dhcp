// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag, for
// the same reason healthfloor.go does not: the comparison below decides
// whether a counter delta means anything at all, so it has to be
// testable without a live plugin. A guard that has never been observed
// rejecting anything is not known to work. Everything needing a socket
// lives in health.go.

package harness

import (
	"fmt"
	"strings"
)

// InstanceVerdict is the result of comparing the plugin process across
// two /Plugin.Health reads (#405).
//
// The plugin's counters live in memory for the lifetime of the plugin
// *process*, and three tests in this suite deliberately end that
// process mid-run. A before/after pair that straddles one of those
// reads as "no change" — or goes negative and reads as no change again.
// Nothing in the suite noticed this for 29 measurement sites, which is
// what #405 is about.
type InstanceVerdict int

const (
	// InstanceUnknown is the zero value on purpose. Every way of
	// failing to establish the plugin's identity has to land somewhere,
	// and the safe landing spot is "cannot tell", never "same".
	InstanceUnknown InstanceVerdict = iota
	// InstanceSame means both reads came from one plugin process, so a
	// delta between them is meaningful.
	InstanceSame
	// InstanceRecycled means the plugin restarted between the reads.
	// Any delta computed across them is void.
	InstanceRecycled
)

func (v InstanceVerdict) String() string {
	switch v {
	case InstanceSame:
		return "same-instance"
	case InstanceRecycled:
		return "recycled"
	default:
		return "unknown"
	}
}

// instanceIDKey is the JSON key carrying the process identity. Named
// once so the presence check and the failure text cannot drift apart.
const instanceIDKey = "instance_id"

// CompareInstances reports whether before and after came from the same
// plugin process.
//
// It returns InstanceUnknown rather than guessing whenever identity
// cannot be established: a nil read, a payload that never carried
// instance_id (an older plugin), or an empty id. That distinction is
// the entire point. An absent JSON string decodes to "", and two ""
// values compare equal — so a naive `before.InstanceID ==
// after.InstanceID` would report "same process" most confidently
// exactly when it knows least, which is the mistake this package has
// already made once with counters that were never published (#377).
func CompareInstances(before, after *HealthResponse) InstanceVerdict {
	if before == nil || after == nil {
		return InstanceUnknown
	}
	if !before.publishedInstanceID() || !after.publishedInstanceID() {
		return InstanceUnknown
	}
	if before.InstanceID == "" || after.InstanceID == "" {
		return InstanceUnknown
	}
	if before.InstanceID == after.InstanceID {
		return InstanceSame
	}
	return InstanceRecycled
}

// publishedInstanceID reports whether the payload this value was
// decoded from actually carried the key.
//
// A nil published map means "built by hand, not decoded" — the same
// convention CheckHealthFloor uses — so hand-built values in unit tests
// are judged on their field alone.
func (h *HealthResponse) publishedInstanceID() bool {
	if h.published == nil {
		return true
	}
	_, ok := h.published[instanceIDKey]
	return ok
}

// CounterWindowError renders the failure for a window whose delta
// cannot be trusted. counters names what the caller was about to
// compare, so the message says which numbers are void rather than
// leaving the reader to work it out.
//
// Returns "" when the verdict is acceptable, so callers can branch on
// the empty string and keep the accept/reject decision in one place.
func CounterWindowError(v InstanceVerdict, expectRecycle bool, before, after *HealthResponse, counters ...string) string {
	switch {
	case v == InstanceSame && !expectRecycle:
		return ""
	case v == InstanceRecycled && expectRecycle:
		return ""
	}

	var b strings.Builder
	switch v {
	case InstanceRecycled:
		fmt.Fprintf(&b, "the plugin restarted inside this measurement window: "+
			"instance %s → %s.\n", short(before.InstanceID), short(after.InstanceID))
		b.WriteString("Its counters are in-memory and went back to zero with it, so any\n" +
			"delta across this window is void — including a delta that reads as\n" +
			"zero, which is how this failure has always looked (#405).")
	case InstanceSame:
		// Only reachable with expectRecycle set.
		fmt.Fprintf(&b, "expected the plugin to restart inside this window, but it did not: "+
			"instance %s throughout.\n", short(before.InstanceID))
		b.WriteString("The test asked for a recycle, so either the action meant to trigger\n" +
			"one silently did nothing, or it is no longer reaching the plugin.")
	default:
		b.WriteString("cannot tell whether the plugin restarted inside this measurement window.\n")
		b.WriteString(unknownReason(before, after))
		b.WriteString("\nTreating this as a failure on purpose: an unverifiable delta is not a\n" +
			"passing one, and reporting it as clean is the bug (#405).")
	}

	if len(counters) > 0 {
		fmt.Fprintf(&b, "\nCounters this window was about to compare: %s.", strings.Join(counters, ", "))
	}
	if before != nil && after != nil {
		fmt.Fprintf(&b, "\nUptime across the window: %.0fs → %.0fs.", before.UptimeSeconds, after.UptimeSeconds)
	}
	return b.String()
}

// unknownReason explains which side failed to identify itself, so the
// reader is not left diffing two payloads by hand.
func unknownReason(before, after *HealthResponse) string {
	switch {
	case before == nil && after == nil:
		return "  Neither health read succeeded."
	case before == nil:
		return "  The opening health read is missing."
	case after == nil:
		return "  The closing health read is missing."
	}
	var missing []string
	for _, s := range []struct {
		label string
		h     *HealthResponse
	}{{"opening", before}, {"closing", after}} {
		switch {
		case !s.h.publishedInstanceID():
			missing = append(missing, fmt.Sprintf("the %s read carried no %s key at all "+
				"(a plugin older than #405?)", s.label, instanceIDKey))
		case s.h.InstanceID == "":
			missing = append(missing, fmt.Sprintf("the %s read carried an empty %s", s.label, instanceIDKey))
		}
	}
	return "  " + strings.Join(missing, "; ") + "."
}

// short trims an instance id for messages while staying long enough to
// be visibly different between two processes.
func short(id string) string {
	if id == "" {
		return "(empty)"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
