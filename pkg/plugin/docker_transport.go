// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	docker "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
)

// envDockerHost names the environment variable that points the plugin
// at the Docker API. It is declared in config.json (and in
// config-cover.json) under exactly this name, so an operator can put a
// read-only socket proxy in front of the daemon; the constant is what
// keeps the manifest and the code from drifting apart.
const envDockerHost = "DOCKER_HOST"

// defaultDockerHost is the socket config.json bind-mounts.
const defaultDockerHost = "unix:///run/docker.sock"

// dockerHostFromEnv returns the Docker API endpoint to use. An unset or
// empty value keeps the mounted socket, so an operator who sets nothing
// gets exactly the behaviour every release before this one had.
func dockerHostFromEnv(get func(string) string) string {
	if h := get(envDockerHost); h != "" {
		return h
	}
	return defaultDockerHost
}

// errUnsafeMethodToDaemon is returned instead of sending a request
// whose method is not one of the safe, body-less ones.
var errUnsafeMethodToDaemon = fmt.Errorf("refused: this plugin issues only GET and HEAD requests to the Docker API")

// safeDaemonMethods is the whole set this plugin may send.
//
// MEASURED at 2.0.0-alpha.1: the interface in docker_client.go names
// NetworkList, NetworkInspect and ContainerInspect, all GET — and the
// client library's API-version negotiation sends HEAD /_ping before the
// first of them, falling back to GET only after the HEAD fails. #691's
// three-call list is the plugin's own calls and is right about those;
// it does not name the ping, and a GET-only refusal therefore counted a
// refusal on every startup. A counter that is 1 before anything has
// happened tells an operator nothing.
//
// Both methods are safe and body-less by RFC 9110 sections 9.3.1 and
// 9.3.2: neither can create, start, stop, exec or delete anything. What
// the socket grant makes dangerous (#691) is the methods NOT in this
// set, and that is what is refused.
var safeDaemonMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
}

// readOnlyTransport is the plugin's whole contract with the Docker
// socket, enforced at the only place every call has to pass through.
//
// WHY A TRANSPORT AND NOT A REVIEW OF THE CALL SITES. The socket mount
// is the grant that makes compromise of this plugin equivalent to root
// on the host (#691): anything that can reach the Docker API can start
// a privileged container. What makes that grant reducible is that the
// plugin's entire use of the API is three read calls — and "three read
// calls" is a property of today's call sites, which is to say a
// property nothing enforces. A future NetworkCreate, a library that
// retries with POST, a debug helper: each of them is one line, and each
// of them silently widens what an operator has to trust.
//
// A safe-methods-only round tripper turns that from a claim into a
// refusal. It
// costs one comparison per request, it cannot be bypassed by adding a
// call site, and it is the same predicate an operator's read-only proxy
// would apply — so the plugin fails the same way in front of a proxy as
// it does behind one, rather than discovering the difference in
// production.
//
// It records method and path for every request it passes as well as
// every one it refuses. The record is what makes the claim checkable
// from outside: the integration lane reads the plugin's own log for the
// set of calls a real daemon actually saw, so "only GET" is measured
// against a live daemon rather than asserted about a fake.
type readOnlyTransport struct {
	base http.RoundTripper

	// onRefusal is called once per refused request. It is a callback
	// rather than a direct counter increment so the transport can be
	// constructed and driven in a unit test with no Plugin at all —
	// the arm that must not be reachable only from production.
	onRefusal func()

	mu   sync.Mutex
	seen map[string]int
}

func newReadOnlyTransport(base http.RoundTripper, onRefusal func()) *readOnlyTransport {
	return &readOnlyTransport{base: base, onRefusal: onRefusal, seen: make(map[string]int)}
}

// RoundTrip refuses anything outside safeDaemonMethods before the
// request leaves the process.
//
// The comparison is against the exact method string. Go's http.Request
// carries the method verbatim and an empty Method means GET only after
// the transport defaults it, so that spelling is normalised here and
// nothing else is: a case-insensitive or prefix comparison would let
// "get" and "GETX" through, and HTTP methods are case-sensitive
// (RFC 9110 section 9.1).
func (t *readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	t.record(method, path)

	if !safeDaemonMethods[method] {
		if t.onRefusal != nil {
			t.onRefusal()
		}
		log.WithFields(log.Fields{"method": method, "path": path}).
			Error("Refused an unsafe-method request to the Docker API: this plugin's socket contract is read-only (#691)")
		return nil, fmt.Errorf("%w (%s %s)", errUnsafeMethodToDaemon, method, path)
	}
	return t.base.RoundTrip(req)
}

// record notes a method+path the first time it is seen and logs it
// once. Logging every request would put one line per poll in the log;
// logging the first of each shape gives the lane the SET of calls the
// plugin makes, which is the thing being claimed.
func (t *readOnlyTransport) record(method, path string) {
	key := method + " " + path
	t.mu.Lock()
	n := t.seen[key]
	t.seen[key] = n + 1
	t.mu.Unlock()
	if n == 0 {
		log.WithFields(log.Fields{"method": method, "path": path}).
			Debug("docker-api call")
	}
}

// calls returns the distinct method+path pairs seen so far, sorted.
func (t *readOnlyTransport) calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.seen))
	for k := range t.seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newDockerClient builds the plugin's one Docker API client with the
// read-only transport in front of it.
//
// A TLS endpoint is not supported and never was: nothing here reads
// DOCKER_CERT_PATH or configures a TLS client, so an https:// value
// would reach the daemon unencrypted-or-not-at-all rather than
// silently working. Nor is a proxy on its own unix socket: the plugin
// sees only the paths config.json mounts, and that mount's source is
// fixed there with no settable field. The documented deployment is a
// plain tcp endpoint on the host's loopback, which the plugin reaches
// because it runs with host networking.
//
// TWO CONSTRUCTIONS, ON PURPOSE. client.WithHost configures the dialer
// on a *http.Transport and fails on anything else, so the wrapper
// cannot be installed before the host is applied; and Client.HTTPClient
// returns a COPY, so wrapping the copy of a finished client does not
// reach the client itself. Building once to obtain a correctly dialled
// http.Client, wrapping its transport, and building the real client
// around that is what keeps the dialer the library's own rather than a
// second implementation of it here — which is the version of this that
// would silently stop honouring DOCKER_HOST's tcp and npipe forms.
func newDockerClient(host string, p *Plugin) (*docker.Client, error) {
	dialled, err := docker.NewClientWithOpts(docker.WithHost(host))
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client for %s: %w", host, err)
	}
	httpClient := dialled.HTTPClient()
	_ = dialled.Close()

	var onRefusal func()
	if p != nil {
		onRefusal = func() { p.dockerAPINonGETRefusals.Add(1) }
	}
	httpClient.Transport = newReadOnlyTransport(httpClient.Transport, onRefusal)

	client, err := docker.NewClientWithOpts(
		docker.WithHost(host),
		docker.WithHTTPClient(httpClient),
		docker.WithAPIVersionNegotiation(),
		// Fail fast on hung API calls. Concretely defends against the
		// daemon-startup window where dockerd may be calling into us
		// before it can respond to our own NetworkInspect / etc.
		docker.WithTimeout(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return client, nil
}
