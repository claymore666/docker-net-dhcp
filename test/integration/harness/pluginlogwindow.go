// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

// LogSince returns data after off; an offset past the end (truncation, reinstall) returns the whole log so a census never judges nothing (#933).
func LogSince(data []byte, off int64) []byte {
	if off <= 0 || off > int64(len(data)) {
		return data
	}
	return data[off:]
}

// PluginLogWindow returns the log written after off, empty when off is past the end (#933). The log outlives every
// test in a suite process, and the arm64 lane runs the whole suite in one: the DHCPv6 squat test read three RFC 5227
// section 2.4 lines a TestConflictCheck_ test had written ten minutes earlier.
func PluginLogWindow(data []byte, off int64) []byte {
	if off < 0 || off > int64(len(data)) {
		return nil
	}
	return data[off:]
}
