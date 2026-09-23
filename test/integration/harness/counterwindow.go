// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: whether a counter delta means anything is tested without a live plugin (#405).

package harness

import (
	"fmt"
	"strings"
	"time"
)

// awaitPollInterval is 250 ms, from the failure suite's poll helper (#254); recoveryobserver.go's budgets add one interval to the product timeout.
const awaitPollInterval = 250 * time.Millisecond

// InstanceVerdict compares the plugin process across two /Plugin.Health reads; counters live for one process (#405).
type InstanceVerdict int

const (
	// InstanceUnknown is the zero value: identity not established is never "same".
	InstanceUnknown InstanceVerdict = iota
	// InstanceSame means both reads came from one plugin process.
	InstanceSame
	// InstanceRecycled means the plugin restarted between the reads, voiding any delta.
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

// instanceIDKey is the JSON key carrying the process identity.
const instanceIDKey = "instance_id"

// CompareInstances reports whether two reads came from one plugin process, InstanceUnknown when a read is nil, predates
// instance_id or has an empty one: two absent ids decode to "" and compare equal (#377, #405).
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

// publishedInstanceID reports whether the decoded payload carried the key; a nil published map means built by hand.
func (h *HealthResponse) publishedInstanceID() bool {
	if h.published == nil {
		return true
	}
	_, ok := h.published[instanceIDKey]
	return ok
}

// CounterWindowError renders why a window's delta cannot be trusted, "" when the verdict is acceptable.
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

// unknownReason names which side failed to identify itself.
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

// short trims an instance id for messages.
func short(id string) string {
	if id == "" {
		return "(empty)"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
