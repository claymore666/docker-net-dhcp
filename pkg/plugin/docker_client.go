// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"

	dTypes "github.com/docker/docker/api/types"
	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

// dockerClient is the narrow slice of the Docker API client the plugin
// actually uses. Depending on the interface (rather than the concrete
// *client.Client) lets tests inject a fake and exercise the error arms
// of the recovery / option-fallback paths, which integration cannot
// reach without a real daemon misbehaving. The concrete client
// satisfies this interface as-is.
//
// Ping, ServerVersion and ClientVersion are here for the engine floor
// (#670) and are the whole of what the version probe needs. Both calls
// are GET or HEAD, so they pass the read-only transport's refusal
// (docker_transport.go) rather than being an exception to it.
// ClientVersion takes no context because it reads what the last ping
// NEGOTIATED rather than asking the daemon anything.
type dockerClient interface {
	Ping(ctx context.Context) (dTypes.Ping, error)
	ServerVersion(ctx context.Context) (dTypes.Version, error)
	ClientVersion() string
	NetworkList(ctx context.Context, options dNetwork.ListOptions) ([]dNetwork.Summary, error)
	NetworkInspect(ctx context.Context, networkID string, options dNetwork.InspectOptions) (dNetwork.Inspect, error)
	ContainerInspect(ctx context.Context, containerID string) (dContainer.InspectResponse, error)
	Close() error
}
