// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "sync/atomic"

// This file holds the counters for the places this package REFUSES
// operator-supplied input, or cannot confirm it did what the operator
// asked (#780).
//
// The rule these exist for: a refusal recorded only as a log line is a
// silent no-op from the operator's point of view. They set an option,
// nothing happened, and the only trace is a warning nobody reads on a
// healthy plugin. Health counters are this project's observability
// surface, and new failure-mode work adds a counter rather than a log
// line.
//
// # Why package-level rather than a field on DHCPClientOptions
//
// Both counted events happen inside this package, and one of them
// (directive dropping) happens during config rendering, which no caller
// watches. Threading a sink through DHCPClientOptions would put the
// wiring at FIVE construction sites in pkg/plugin — an enumeration
// beside the code, which is an unrun checklist: the sixth site added
// next year compiles, runs, refuses input and counts nothing. A pull
// through RefusalCounts has one read point and no site to forget.
//
// The cost is process-global state, which makes a test that asserts an
// absolute value order-dependent. Tests therefore read DELTAS across the
// operation under test, the same way the integration suite's counter
// windows do.
//
// # What a zero means, and why that question decides the design
//
// A counter reading zero because nothing was refused, and one reading
// zero because the increment was never wired, are the same reading.
// That is the defect class this project has repeatedly paid for — an
// error folded into a value, indistinguishable from a legitimate one.
// So the tests for these do not merely assert "zero when nothing
// happens": each has a POSITIVE CONTROL that must move the counter in
// the same run, which is what separates "wired and quiet" from "not
// wired at all". Deleting either increment turns those tests red.
var (
	// directivesRefused counts dhcpcd directives dropped by directive()
	// because their value carried a control character.
	//
	// dhcpcd.conf has no quoting for directive values, so a value
	// containing a newline is not escapable — it would become a second
	// directive. Dropping is the only safe handling and is correct. What
	// was missing is that the operator's value then never reaches the
	// DHCP server and nothing said so.
	directivesRefused atomic.Int32

	// mountPrepFailures counts individual mountPrep commands that failed
	// inside the client's private mount namespace.
	//
	// mountPrep builds the isolation the per-interface state directory
	// and dhcpcd's control socket depend on. Its commands are chained
	// with `;`, so a failure does not stop the chain and dhcpcd starts
	// anyway — a deliberate degrade rather than a hard failure, and one
	// that is invisible: the shell's stderr cannot be raised to a
	// warning without raising all of dhcpcd's routine output with it.
	//
	// The consequence of an unnoticed failure is not cosmetic. Two
	// containers whose container-side link is the default eth0 collide
	// deterministically on dhcpcd's control socket, and the second
	// container's client becomes a no-op that never renews or releases
	// its lease. See dhcpcdRunDir.
	//
	// Counts COMMANDS, not clients: a client whose namespace setup fails
	// in three of four steps adds 3.
	mountPrepFailures atomic.Int32
)

// RefusalCounts reports how many operator-supplied inputs this process
// has refused, and how many private-namespace preparation commands
// failed.
//
// A pull rather than a push, so the plugin's health snapshot reads them
// in one place instead of every DHCPClientOptions being responsible for
// carrying a sink.
func RefusalCounts() (directives, mountPrep int32) {
	return directivesRefused.Load(), mountPrepFailures.Load()
}
