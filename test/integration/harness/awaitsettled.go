// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: what a suite cell fails on must be drivable without a live plugin or root.

package harness

import "time"

// AwaitSettled asks lookup until its answer settles or the budget runs out, and always looks at least once. It returns
// (v, true, nil) when settled, (last, false, nil) when it answered and never settled, and (zero, false, err) when it
// never answered (#1051).
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
