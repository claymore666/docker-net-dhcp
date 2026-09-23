// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"time"
)

// pluginCallBudget is moby's pkg/plugins timeout for one driver call, which the plugin
// cannot raise; past it the daemon has already failed `docker run` (#911).
const pluginCallBudget = 30 * time.Second

const pluginCallMargin = 4 * time.Second

// v6AcquisitionDeadline is when the DHCPv6 half of a dual-stack CreateEndpoint must stop:
// the v4 half runs first (about 11 s on the bridge fixture, measured 2026-09-06 in run
// 34056785164), and 11 s plus dhcp.V6AcquisitionWindow's 21.7 s passed the daemon's 30 s (#911).
func v6AcquisitionDeadline(callStart time.Time) time.Time {
	return callStart.Add(pluginCallBudget - pluginCallMargin)
}

func withV6AcquisitionDeadline(ctx context.Context, callStart time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, v6AcquisitionDeadline(callStart))
}
