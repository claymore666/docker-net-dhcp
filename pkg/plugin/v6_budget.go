// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"time"
)

// pluginCallBudget is how long the Docker daemon waits for one
// NetworkDriver call before it stops listening.
//
// It is moby's `pkg/plugins` default request timeout, and it is not a
// setting this plugin can raise: the daemon owns the client. When it
// expires, the daemon has already answered `docker run` with
// "Client.Timeout exceeded while awaiting headers" and whatever the
// plugin says afterwards reaches nobody -- the endpoint is created in
// the plugin's own state and torn down again, and the container does
// not start.
const pluginCallBudget = 30 * time.Second

// pluginCallMargin is what CreateEndpoint keeps back from that budget
// for the work that is not the DHCPv6 acquisition: writing the
// response, and the small change that the daemon's clock started
// before this handler did.
const pluginCallMargin = 4 * time.Second

// v6AcquisitionDeadline is the latest moment the DHCPv6 half of
// CreateEndpoint may still be running.
//
// WHY THE v6 HALF AND NOT BOTH. A dual-stack endpoint runs two
// acquisitions inside ONE daemon call, one after the other, and each
// is given lease_timeout. That is not one clock overrunning its
// budget; it is two budgets sharing a deadline neither of them knows
// about. The v4 half runs first and keeps lease_timeout unchanged --
// on the 2.x bridge fixture it costs about 11s, MEASURED 2026-09-06
// across three v4-only endpoints in run 34056785164 -- and what the v6
// half may spend is what is left.
//
// Without this the arithmetic was 11s + dhcp.V6AcquisitionWindow's
// 21.7s = 32.7s, and all three of the segments that have no DHCPv6
// address to give failed the container start at the daemon's 30s with
// no diagnosis at all: MEASURED in the same run, the no-router arm,
// the managed-but-silent arm and the DECLINE-and-ask-again test.
//
// The remainder can be shorter than RFC 4861's router-discovery
// window, and then the absence verdict describes the deadline rather
// than the segment. That is not silent: dhcp.GetIP logs a warning
// naming both numbers whenever its budget is below that window.
func v6AcquisitionDeadline(callStart time.Time) time.Time {
	return callStart.Add(pluginCallBudget - pluginCallMargin)
}

// withV6AcquisitionDeadline bounds ctx by v6AcquisitionDeadline.
//
// The returned cancel is always non-nil, so every call site can defer
// it without asking which branch it took.
func withV6AcquisitionDeadline(ctx context.Context, callStart time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, v6AcquisitionDeadline(callStart))
}
