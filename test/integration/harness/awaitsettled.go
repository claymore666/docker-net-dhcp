// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag, for
// the reason counterwindow.go and attachlog.go give: the decision here
// is what a suite cell fails on, so it has to be drivable without a
// live plugin and without root -- in both directions.

package harness

import "time"

// AwaitSettled asks lookup until what it answers has settled, or the
// budget runs out. It is for the reads a suite makes of the kernel
// while the plugin is still changing it.
//
// THREE OUTCOMES, and a caller acts on each differently:
//
//	(v, true, nil)     something answered and it had settled
//	(last, false, nil) something answered and it never settled
//	(zero, false, err) nothing ever answered; err is the last reason
//
// The last two are separate because they are different claims. A value
// that never settles is the subject being wrong about something a
// caller can print; a lookup that never answers at all is the subject
// missing, and the caller says so in its own words.
//
// One look always happens, whatever the budget, so a zero budget is a
// single read and not a skipped one.
func AwaitSettled[T any](budget, interval time.Duration, lookup func() (T, error), settled func(T) bool) (T, bool, error) {
	var (
		zero     T
		last     T
		answered bool
		lastErr  error
	)
	deadline := time.Now().Add(budget)
	for {
		v, err := lookup()
		if err == nil {
			last, answered, lastErr = v, true, nil
			if settled(v) {
				return v, true, nil
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if !answered {
				return zero, false, lastErr
			}
			return last, false, nil
		}
		time.Sleep(interval)
	}
}
