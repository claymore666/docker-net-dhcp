package util

import (
	"context"
	"time"
)

// AwaitCondition polls cond every interval until it returns ok=true, an
// error, or ctx is cancelled. The poll runs synchronously in the caller's
// goroutine: a previous async-poller form leaked a goroutine on every
// ctx-cancel because it kept calling cond forever (typically Docker
// NetworkInspect, ~10/s/leaked-goroutine). cond is expected to be
// reasonably fast or to honor ctx itself; we don't try to interrupt it.
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
