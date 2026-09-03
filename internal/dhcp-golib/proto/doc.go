// Package proto is ring 1: the state machine. It is pure.
//
// No I/O, no clock, no goroutines, no ambient anything. The whole surface is
//
//	func (m *Machine) Step(now Time, rnd uint64, ev Event) (State, []Action)
//
// with time and entropy passed in as parameters rather than read from the
// environment. That is what makes the tests instant and offline replay
// bit-exact, and it is enforced by the T1 gate rather than by this comment.
package proto
