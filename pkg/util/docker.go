// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"context"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
)

const (
	OptionsKeyGeneric = "com.docker.network.generic"
)

// ContainerInspector is the one Docker-client method AwaitContainerInspect needs, so a test can pass a fake.
type ContainerInspector interface {
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
}

// NotFound returns at once with its chain intact: every caller resolved the ID from the daemon moments before, so it
// means removed, and retrying it spent the whole budget turning a short-lived container into a counted fault (#401).
// Other errors are retried and the deadline error carries them (#317).

// AwaitContainerInspect polls ContainerInspect in the caller's goroutine until it succeeds or ctx ends.
func AwaitContainerInspect(ctx context.Context, docker ContainerInspector, id string, interval time.Duration) (container.InspectResponse, error) {
	var dummy container.InspectResponse
	var firstErr, lastErr error
	attempts := 0
	for {
		ctr, err := docker.ContainerInspect(ctx, id)
		if err == nil {
			return ctr, nil
		}
		if cerrdefs.IsNotFound(err) {
			return dummy, err
		}
		attempts++
		if firstErr == nil {
			firstErr = err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr == nil {
				return dummy, ctx.Err()
			}
			// The last attempt is usually the deadline itself; the first error and the count tell a refusal from no answer,
			// and one attempt means the context was dead on arrival (#406).
			if attempts > 1 && firstErr.Error() != lastErr.Error() {
				return dummy, fmt.Errorf("%w (%d attempts; first: %w; last: %w)",
					ctx.Err(), attempts, firstErr, lastErr)
			}
			return dummy, fmt.Errorf("%w (%d attempts; last: %w)", ctx.Err(), attempts, lastErr)
		case <-time.After(interval):
		}
	}
}
