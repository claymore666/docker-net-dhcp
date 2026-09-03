// Package lease is ring 2: the manager. One managed lease per
// (interface, family). It turns the actions ring 1 returns into requests on
// ring 3 and reports lease changes outward.
//
// # What ring 2 is FOR
//
// Two things ring 1 cannot do and ring 3 must not decide:
//
//  1. Serialisation. One event at a time per managed lease, with the whole
//     action list drained before the next Step. The design document calls this
//     out as a trap (section 2.4 item 2): actions must execute in the order
//     returned, and a packet arriving mid-drain must not interleave. It is
//     cheap here and a source of heisenbugs anywhere else.
//
//  2. The bridge between the two clocks. Ring 1 works entirely in monotonic
//     Instants, because RFC 2131 section 3.3 needs intervals on a clock that
//     does not step. A lease reported outward, or persisted, needs wall-clock
//     absolute times, because a monotonic reading means nothing to the next
//     process. Ring 2 owns the conversion and it is the only place the two
//     meet.
//
// # The ports
//
// Every effect is an interface declared here and implemented in ring 3. That
// is what lets the whole acquisition path be table-tested with no root, no
// namespace and no network — the fake implementations in this package's tests
// are the same shape as the real ones.
package lease
