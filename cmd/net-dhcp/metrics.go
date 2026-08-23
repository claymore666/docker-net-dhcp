// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

// metricsListener is the part of *plugin.Plugin this wiring needs, so
// the decision below can be tested without building a plugin.
type metricsListener interface {
	ListenMetrics(addr string) error
}

// listenMetricsFromEnv honours METRICS_ADDR (#651): empty means no TCP
// listener at all, which is the default and the only safe posture for a
// process holding CAP_NET_ADMIN on the host network namespace. A
// non-empty value that cannot be bound is returned as an error, so the
// caller can fail startup rather than leave an operator to discover the
// absence from an empty dashboard.
//
// This lives outside main() because main() is only ever executed by the
// cover plugin during the integration run, and that plugin does not set
// METRICS_ADDR — so the branch would otherwise ship untested.
func listenMetricsFromEnv(p metricsListener, addr string) error {
	if addr == "" {
		return nil
	}
	return p.ListenMetrics(addr)
}
