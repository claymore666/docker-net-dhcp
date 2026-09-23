// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

// Timeouts and a body cap for the two HTTP servers, each set against the handler it must not cut short (#709).
const (
	// socketReadHeaderTimeout and socketReadTimeout bound reading a request; dockerd writes each small JSON body at
	// once (#709).
	socketReadHeaderTimeout = 10 * time.Second
	socketReadTimeout       = 30 * time.Second

	// socketIdleTimeout is minutes because libnetwork reuses connections (#709).
	socketIdleTimeout = 2 * time.Minute

	socketMaxBodyBytes = 1 << 20

	metricsReadHeaderTimeout = 5 * time.Second
	metricsReadTimeout       = 10 * time.Second
	metricsWriteTimeout      = 30 * time.Second
	metricsIdleTimeout       = 60 * time.Second
)

// socketWriteTimeout is zero: http.Server's WriteTimeout runs from the start of reading, so it
// would truncate a CreateEndpoint that holds a DHCP acquisition with RFC 5227's check, and the
// operator-set lease_timeout has no upper bound (#709).
const socketWriteTimeout = 0

func socketWorstCaseHandler() time.Duration {
	return linkAwaitTimeout + defaultLeaseTimeout + preflightProbeBudget
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, socketMaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// warnOnWildcardMetricsBind warns when METRICS_ADDR binds every host interface; the exposition
// carries aggregate counters and no per-endpoint identifiers (#709).
func warnOnWildcardMetricsBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	// An empty host, the ":9090" form, binds every interface.
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsUnspecified() {
			return false
		}
	}
	log.WithField("addr", addr).
		Warn("METRICS_ADDR binds every interface; /metrics exposes this plugin's counters and instance ID to anything that can reach this host. It carries no container names, addresses or MACs — bind loopback or a management interface unless every network reaching this host is trusted")
	return true
}
