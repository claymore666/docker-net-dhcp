package plugin

import (
	"context"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

// dockerClient is the narrow slice of the Docker API client the plugin
// actually uses. Depending on the interface (rather than the concrete
// *client.Client) lets tests inject a fake and exercise the error arms
// of the recovery / option-fallback paths, which integration cannot
// reach without a real daemon misbehaving. The concrete client
// satisfies this interface as-is.
type dockerClient interface {
	NetworkList(ctx context.Context, options dNetwork.ListOptions) ([]dNetwork.Summary, error)
	NetworkInspect(ctx context.Context, networkID string, options dNetwork.InspectOptions) (dNetwork.Inspect, error)
	ContainerInspect(ctx context.Context, containerID string) (dContainer.InspectResponse, error)
	Close() error
}
