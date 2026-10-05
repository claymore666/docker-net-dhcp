// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-connections/sockets"
	docker "github.com/moby/moby/client"
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
	// logged holds the exact paths already written, so each container's inspect still logs once (#1184).
	logged map[string]struct{}
}

func newReadOnlyTransport(base http.RoundTripper, onRefusal func()) *readOnlyTransport {
	return &readOnlyTransport{base: base, onRefusal: onRefusal, seen: make(map[string]int), logged: make(map[string]struct{})}
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

var idCollections = map[string]bool{
	"containers": true, "networks": true, "volumes": true, "exec": true, "images": true, "plugins": true,
}

var collectionVerbs = map[string]bool{
	"json": true, "create": true, "prune": true, "search": true, "load": true, "get": true, "ls": true,
}

// seenKey collapses an id or name after a collection to `{id}`: one key per call shape, not per container (#1184).
func seenKey(method, path string) string {
	segs := strings.Split(path, "/")
	for i := 1; i < len(segs); i++ {
		if idCollections[segs[i-1]] && !collectionVerbs[segs[i]] && segs[i] != "" {
			segs[i] = "{id}"
		}
	}
	return method + " " + strings.Join(segs, "/")
}

// maxLoggedPaths bounds logged; a full set is cleared, so each path logs at most once more (#1184).
const maxLoggedPaths = 1024

func (t *readOnlyTransport) record(method, path string) {
	key := seenKey(method, path)
	exact := method + " " + path
	t.mu.Lock()
	t.seen[key]++
	_, again := t.logged[exact]
	if !again {
		if len(t.logged) >= maxLoggedPaths {
			t.logged = make(map[string]struct{})
		}
		t.logged[exact] = struct{}{}
	}
	t.mu.Unlock()
	if !again {
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

// newDockerClient wraps a socket transport in the read-only one, with WithHost before WithHTTPClient as it accepts only
// a bare *http.Transport and WithTimeout after it as it sets the client current then (#178), and no TLS (#725).
func newDockerClient(host string, p *Plugin) (*docker.Client, error) {
	hostURL, err := docker.ParseHostURL(host)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client for %s: %w", host, err)
	}
	base := &http.Transport{MaxIdleConns: 6, IdleConnTimeout: 30 * time.Second}
	if err := sockets.ConfigureTransport(base, hostURL.Scheme, hostURL.Host); err != nil {
		return nil, fmt.Errorf("failed to create docker client for %s: %w", host, err)
	}

	var onRefusal func()
	if p != nil {
		onRefusal = func() { p.dockerAPINonGETRefusals.Add(1) }
	}
	httpClient := &http.Client{
		Transport:     newReadOnlyTransport(base, onRefusal),
		CheckRedirect: docker.CheckRedirect,
	}

	client, err := docker.New(
		docker.WithHost(host),
		docker.WithHTTPClient(httpClient),
		// dockerd may call the plugin during its startup before it answers our requests, so calls time out.
		docker.WithTimeout(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return client, nil
}
