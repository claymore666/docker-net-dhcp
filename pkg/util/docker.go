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

// ContainerInspector is the one Docker-client method AwaitContainerInspect
// needs. Taking an interface (not the concrete *client.Client) lets callers
// inject a fake in tests; the real client satisfies it as-is.
type ContainerInspector interface {
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
}

// AwaitContainerInspect polls docker.ContainerInspect until it succeeds,
// ctx is cancelled, or interval-paced retries exhaust. Synchronous for
// the same reason as AwaitNetNS — the previous async form leaked a
// poller goroutine on ctx-cancel that kept hitting the Docker API forever.
//
// "No such container" ends the wait immediately instead of being
// retried. Polling for a container that has been removed can only ever
// end in the deadline, and callers then read that deadline as "the
// daemon was slow" when what actually happened is "the container is
// gone". Those need opposite responses: one is a fault worth counting,
// the other is an ordinary short-lived container. Retrying an absence
// spent the whole budget converting the second into the first — nine to
// twelve times per integration run (#401).
//
// A NotFound arriving here means removed, not "not yet": every caller
// resolves the container ID from the daemon first, so it existed
// moments ago. The error is returned with its chain intact so callers
// can classify with cerrdefs.IsNotFound.
//
// Other errors are still retried, and the last one is reported
// alongside the deadline — the same contract AwaitNetNS and
// AwaitLinkByIndex already keep. Discarding it here was why a Join
// timeout said only "context deadline exceeded" while its sibling
// failures named a missing file.
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
			// The last attempt is almost always "context deadline
			// exceeded" — the request the deadline landed on — which
			// says nothing about why the earlier ones failed. Reporting
			// the first error and the attempt count separates a daemon
			// that refused once from a daemon that never answered, and
			// a budget spent here from one already spent before this
			// call was reached: one attempt means the context was
			// already dead on arrival (#406).
			if attempts > 1 && firstErr.Error() != lastErr.Error() {
				return dummy, fmt.Errorf("%w (%d attempts; first: %w; last: %w)",
					ctx.Err(), attempts, firstErr, lastErr)
			}
			return dummy, fmt.Errorf("%w (%d attempts; last: %w)", ctx.Err(), attempts, lastErr)
		case <-time.After(interval):
		}
	}
}
