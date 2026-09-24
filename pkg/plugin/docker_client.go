// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"

	dTypes "github.com/docker/docker/api/types"
	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

// dockerClient is the part of the Docker client the plugin calls, all GET or HEAD
// so the read-only transport passes them (#670).
type dockerClient interface {
	Ping(ctx context.Context) (dTypes.Ping, error)
	ServerVersion(ctx context.Context) (dTypes.Version, error)
	ClientVersion() string
	NetworkList(ctx context.Context, options dNetwork.ListOptions) ([]dNetwork.Summary, error)
	NetworkInspect(ctx context.Context, networkID string, options dNetwork.InspectOptions) (dNetwork.Inspect, error)
	ContainerInspect(ctx context.Context, containerID string) (dContainer.InspectResponse, error)
	Close() error
}
