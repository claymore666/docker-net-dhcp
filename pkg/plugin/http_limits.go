// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

// Timeouts and a body cap for the two HTTP servers this plugin runs
// (#709). Both were constructed with none, so a client that opened a
// connection and never completed a request held a goroutine and a
// connection for the process lifetime.
//
// Each value below is chosen against the handler it must not cut short.
// That is the whole difficulty here: copying a set of "sensible" defaults
// is what turns a robustness fix into an outage, because CreateEndpoint
// legitimately holds a DHCP round trip.
const (
	// socketReadHeaderTimeout / socketReadTimeout bound READING a
	// request, not serving it. The client on this socket is dockerd and
	// every request it sends is a small JSON body it writes
	// immediately, so these are generous by an order of magnitude and
	// still close the half-open connection case completely.
	socketReadHeaderTimeout = 10 * time.Second
	socketReadTimeout       = 30 * time.Second

	// socketIdleTimeout bounds a kept-alive connection between
	// requests. libnetwork reuses connections, so this is minutes
	// rather than seconds.
	socketIdleTimeout = 2 * time.Minute

	// socketMaxBodyBytes caps a driver request body. The largest thing
	// the daemon sends us is a CreateNetwork with IPAM data and the
	// network's options map; 1 MiB is far past any of them and far
	// below anything that costs the plugin memory.
	socketMaxBodyBytes = 1 << 20

	// Metrics server. A scrape is a small GET and its handler renders
	// one already-taken snapshot, so unlike the socket server this one
	// can carry a write timeout safely.
	metricsReadHeaderTimeout = 5 * time.Second
	metricsReadTimeout       = 10 * time.Second
	metricsWriteTimeout      = 30 * time.Second
	metricsIdleTimeout       = 60 * time.Second
)

// socketWriteTimeout is deliberately ZERO, and this is the one number in
// the set that had to be reasoned about rather than picked.
//
// http.Server's WriteTimeout is a deadline on the whole exchange
// measured from the start of reading the request — it does not know or
// care that a handler is still working. On this socket the handlers are
// not fast: CreateEndpoint performs a real DHCP acquisition, waits for a
// parent link that may not exist yet, and runs an address-conflict probe
// afterwards. Adding up the constants those paths use:
//
//	linkAwaitTimeout       30s   waiting for the parent interface
//	defaultLeaseTimeout    10s   one DHCP acquisition attempt
//	conflictProbeBudget     2s   the post-lease ARP/route probe
//	preflightProbeBudget    8s   CreateNetwork's opt-in probe
//
// — and `lease_timeout` is an operator-settable network option with no
// upper bound, so the true worst case is not knowable from this file at
// all. A WriteTimeout that fires mid-handler does not merely close a
// connection; it hands libnetwork a truncated response for an endpoint
// the plugin has already created, which is worse than the hung
// connection it would be preventing.
//
// The half-open connection case is closed by the read and idle timeouts
// above, which cannot cut a handler short because they bound only the
// parts of the exchange the CLIENT drives. The client here is dockerd.
//
// socketWorstCaseHandler is the arithmetic above, kept executable:
// TestHTTPLimits_SocketWriteTimeoutCannotCutAHandlerShort asserts any
// future non-zero WriteTimeout exceeds it, so raising one of those
// constants without revisiting this decision goes red.
const socketWriteTimeout = 0

// socketWorstCaseHandler is the longest a socket handler can legitimately
// run using only the budgets this package fixes. It is NOT an upper
// bound on reality — `lease_timeout` is operator-settable — which is
// precisely why socketWriteTimeout is zero.
func socketWorstCaseHandler() time.Duration {
	return linkAwaitTimeout + defaultLeaseTimeout + conflictProbeBudget + preflightProbeBudget
}

// limitBody caps every request body reaching the driver handlers, at the
// server rather than in each handler, so an RPC added later cannot
// forget it.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, socketMaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// warnOnWildcardMetricsBind says something when METRICS_ADDR binds every
// interface.
//
// /metrics carries container MACs and addresses, and the plugin runs
// with "network": {"type": "host"}, so a wildcard bind publishes that
// inventory on every interface the host has — including ones the
// operator was not thinking about when they set the variable. Binding a
// wildcard is a legitimate choice in a private network and is not
// refused; it is said out loud, because the alternative is that it is
// never noticed (#709).
func warnOnWildcardMetricsBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Unparseable addresses fail at net.Listen with a better
		// message than anything this function could produce.
		return false
	}
	// An empty host is the ":9090" form, which binds every interface.
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsUnspecified() {
			return false
		}
	}
	log.WithField("addr", addr).
		Warn("METRICS_ADDR binds every interface; /metrics exposes container MAC addresses and leased IPs to anything that can reach this host")
	return true
}
