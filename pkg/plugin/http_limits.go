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
// not fast: CreateEndpoint performs a real DHCP acquisition -- which in
// the default conflict_check=wait includes RFC 5227's check before the
// address is used -- and waits for a parent link that may not exist yet.
// Adding up the constants those paths use:
//
//	linkAwaitTimeout       30s   waiting for the parent interface
//	defaultLeaseTimeout          one DHCP acquisition attempt, which
//	                             since M6 INCLUDES RFC 5227 section
//	                             2.1's probe window in the default
//	                             conflict_check=wait, AND one conflict
//	                             found inside it: an acquisition (12.0s
//	                             by dhcp.AcquisitionWindow), RFC 2131
//	                             section 3.1(5)'s mandatory 10s restart
//	                             delay after the DHCPDECLINE, and the
//	                             second acquisition -- 34.0s, read out
//	                             of the library's constants by
//	                             dhcp.ConflictRecoveryWindow. The lane
//	                             measured why the shorter figure is not
//	                             enough; see that function's doc.
//	preflightProbeBudget    8s   CreateNetwork's opt-in probe, which
//	                             runs conflict_check=off and so carries
//	                             no probe window of its own
//
// The post-lease ARP/route probe that used to be the third term is
// gone: the plugin no longer sends a datagram on the parent to make the
// kernel resolve the address. RFC 5227 now runs inside the acquisition
// itself, which is why its cost moved INTO defaultLeaseTimeout rather
// than being dropped from the sum.
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
	return linkAwaitTimeout + defaultLeaseTimeout + preflightProbeBudget
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
// The exposition is aggregate counters plus an instance UUID — no
// endpoint IDs, container names, addresses or MACs, which is the
// property SECURITY.md rests on and TestMetricsExposition_NoPerEndpointIdentifiers
// pins. So what a wildcard bind leaks is not a lease inventory: it is
// the plugin's operational telemetry, and the fact that this host runs
// the plugin at all. The plugin runs with "network": {"type": "host"},
// so the wildcard is the host's every interface — including ones the
// operator was not thinking about when they set the variable. Binding a
// wildcard is a legitimate choice in a private network and is not
// refused; it is said out loud, because the alternative is that it is
// never noticed (#709).
//
// The warning text is load-bearing and was wrong once: it claimed MACs
// and leased IPs, contradicting SECURITY.md in the same release. An
// operator acts on a security warning, so overstating it is not a
// harmless excess of caution — it spends attention on the wrong thing
// and teaches that these warnings are approximate.
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
		Warn("METRICS_ADDR binds every interface; /metrics exposes this plugin's counters and instance ID to anything that can reach this host. It carries no container names, addresses or MACs — bind loopback or a management interface unless every network reaching this host is trusted")
	return true
}
