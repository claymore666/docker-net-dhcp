package util

import (
	"context"
	"time"

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
func AwaitContainerInspect(ctx context.Context, docker ContainerInspector, id string, interval time.Duration) (container.InspectResponse, error) {
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
