package util

import (
	"context"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

const (
	OptionsKeyGeneric = "com.docker.network.generic"
)

// AwaitContainerInspect polls docker.ContainerInspect until it succeeds,
// ctx is cancelled, or interval-paced retries exhaust. Synchronous for
// the same reason as AwaitNetNS — the previous async form leaked a
// poller goroutine on ctx-cancel that kept hitting the Docker API forever.
func AwaitContainerInspect(ctx context.Context, docker *client.Client, id string, interval time.Duration) (container.InspectResponse, error) {
	var dummy container.InspectResponse
	for {
		ctr, err := docker.ContainerInspect(ctx, id)
		if err == nil {
			return ctr, nil
		}
		select {
		case <-ctx.Done():
			return dummy, ctx.Err()
		case <-time.After(interval):
		}
	}
}
