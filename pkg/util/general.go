// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"context"
	"time"
)

// AwaitCondition polls cond in the caller's goroutine every interval until it returns true, an error, or ctx ends.
func AwaitCondition(ctx context.Context, cond func() (bool, error), interval time.Duration) error {
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
