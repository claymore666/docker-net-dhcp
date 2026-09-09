// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

// LogSince returns the portion of data after off, for scoping a census
// to one test process.
//
// An offset past the end means the log was TRUNCATED or replaced since
// the baseline — a plugin reinstall, or a rotation. Falling back to the
// whole log is deliberate: the alternative is judging nothing, and a
// census that silently judges nothing is the failure mode this whole
// mechanism exists to prevent.
func LogSince(data []byte, off int64) []byte {
	if off <= 0 || off > int64(len(data)) {
		return data
	}
	return data[off:]
}

// PluginLogWindow returns the plugin log written after off, for a test
// that asserts on the lines ITS OWN run produced.
//
// NOT LogSince, and the difference is the whole point. LogSince widens
// to the whole log when off is past the end, because a census that
// silently judges nothing is worse than one that judges too much. An
// assertion is the other way round. The plugin log survives every test
// in a suite process and every plugin restart inside it, so a test that
// reads the whole log is asserting over other tests' lines: the
// positive assertions can be satisfied by another test's line and the
// negative ones can be failed by one. That is not theory. The DHCPv6
// squat test asserted that no line cites RFC 5227 section 2.4, and it
// passed on the amd64 lane only because the shard it sits in holds no
// TestConflictCheck_ test; the arm64 lane runs the whole suite in one
// process, and the same assertion read three section 2.4 lines another
// test had written ten minutes earlier.
//
// So a window that cannot be scoped is EMPTY here. An empty window
// fails the positive assertions loudly instead of quietly restoring the
// behaviour this exists to remove.
func PluginLogWindow(data []byte, off int64) []byte {
	if off < 0 || off > int64(len(data)) {
		return nil
	}
	return data[off:]
}
