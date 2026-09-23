// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

// metricsListener is the part of *plugin.Plugin listenMetricsFromEnv needs, so it is testable without a plugin.
type metricsListener interface {
	ListenMetrics(addr string) error
}

// Off by default: the process holds CAP_NET_ADMIN on the host network namespace. A bad address fails startup. It
// lives outside main() because only the cover plugin runs main(), and it does not set METRICS_ADDR (#651).

// listenMetricsFromEnv opens the TCP metrics listener when METRICS_ADDR is set (#651).
func listenMetricsFromEnv(p metricsListener, addr string) error {
	if addr == "" {
		return nil
	}
	return p.ListenMetrics(addr)
}
