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

// envDockerHost is the variable config.json declares so an operator can put a read-only socket proxy in front (#725).
const envDockerHost = "DOCKER_HOST"

const defaultDockerHost = "unix:///run/docker.sock"

func dockerHostFromEnv(get func(string) string) string {
	if h := get(envDockerHost); h != "" {
		return h
	}
	return defaultDockerHost
}

var errUnsafeMethodToDaemon = fmt.Errorf("refused: this plugin issues only GET and HEAD requests to the Docker API")

// The client's version negotiation sends HEAD /_ping before its first call (measured at
// 2.0.0-alpha.1); GET and HEAD are safe and body-less (RFC 9110 sections 9.3.1, 9.3.2, #691).
var safeDaemonMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
}

// readOnlyTransport refuses every method outside safeDaemonMethods and logs each request's method
// and path, since the socket grant is root on the host (#691).
type readOnlyTransport struct {
	base http.RoundTripper

	onRefusal func()

	mu   sync.Mutex
	seen map[string]int
}

func newReadOnlyTransport(base http.RoundTripper, onRefusal func()) *readOnlyTransport {
	return &readOnlyTransport{base: base, onRefusal: onRefusal, seen: make(map[string]int)}
}

// RoundTrip refuses any method outside safeDaemonMethods, compared case-sensitively (RFC 9110 section 9.1).
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

// newDockerClient builds the client twice, since client.WithHost needs a *http.Transport and
// HTTPClient returns a copy; a TLS endpoint is unsupported (#725).
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
		// dockerd may call the plugin during its startup before it answers our requests, so calls time out.
		docker.WithTimeout(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return client, nil
}
