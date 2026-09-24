// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
)

type countingBase struct {
	sent []string
	resp *http.Response
}

func (c *countingBase) RoundTrip(req *http.Request) (*http.Response, error) {
	c.sent = append(c.sent, req.Method+" "+req.URL.Path)
	if c.resp != nil {
		return c.resp, nil
	}
	return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
}

func TestReadOnlyTransport_RefusesEveryUnsafeMethod(t *testing.T) {
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodConnect, http.MethodTrace,
		"get", "Get", "GETX", "head", "PROPFIND",
	} {
		t.Run(method, func(t *testing.T) {
			base := &countingBase{}
			refusals := 0
			tr := newReadOnlyTransport(base, func() { refusals++ })

			req, err := http.NewRequest(method, "http://docker/v1.51/containers/abc/json", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := tr.RoundTrip(req)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("%s reached the daemon", method)
			}
			if !errors.Is(err, errUnsafeMethodToDaemon) {
				t.Errorf("err = %v, want errUnsafeMethodToDaemon", err)
			}
			if len(base.sent) != 0 {
				t.Errorf("the request was sent anyway: %v — a refusal that still writes to the "+
					"daemon is a log line, not a contract", base.sent)
			}
			if refusals != 1 {
				t.Errorf("refusals = %d, want 1: nothing else makes the refusal visible to an operator", refusals)
			}
		})
	}
}

func TestReadOnlyTransport_PassesTheSafeMethods(t *testing.T) {
	base := &countingBase{}
	refusals := 0
	tr := newReadOnlyTransport(base, func() { refusals++ })

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1.51/networks"},
		{http.MethodGet, "/v1.51/networks/n1"},
		{http.MethodGet, "/v1.51/containers/c1/json"},
		{http.MethodHead, "/_ping"},
	} {
		req, err := http.NewRequest(tc.method, "http://docker"+tc.path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s %s was refused: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
	}
	if len(base.sent) != 4 {
		t.Errorf("reached the daemon: %v, want the three reads and the ping", base.sent)
	}
	if refusals != 0 {
		t.Errorf("refusals = %d for read-only traffic, want 0", refusals)
	}
}

// An empty Method is GET by http.Request's contract.
func TestReadOnlyTransport_TreatsAnEmptyMethodAsGET(t *testing.T) {
	base := &countingBase{}
	tr := newReadOnlyTransport(base, nil)

	req, err := http.NewRequest(http.MethodGet, "http://docker/v1.51/networks", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Method = ""
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("an empty method was refused: %v", err)
	}
	resp.Body.Close()

	if got := tr.calls(); len(got) != 1 || !strings.HasPrefix(got[0], "GET ") {
		t.Errorf("recorded %v, want the call recorded under GET", got)
	}
}

func TestReadOnlyTransport_RecordsBothWhatItSentAndWhatItRefused(t *testing.T) {
	base := &countingBase{}
	tr := newReadOnlyTransport(base, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1.51/networks"},
		{http.MethodGet, "/v1.51/networks"},
		{http.MethodPost, "/v1.51/containers/create"},
	} {
		req, _ := http.NewRequest(tc.method, "http://docker"+tc.path, nil)
		if resp, err := tr.RoundTrip(req); err == nil {
			resp.Body.Close()
		}
	}

	got := tr.calls()
	want := []string{"GET /v1.51/networks", "POST /v1.51/containers/create"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls() = %v, want %v", got, want)
	}
}

func TestReadOnlyTransport_WritesTheRecordToTheLog(t *testing.T) {
	var buf strings.Builder
	restoreOut, restoreLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(restoreOut)
		log.SetLevel(restoreLevel)
	})

	tr := newReadOnlyTransport(&countingBase{}, nil)
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, "http://docker/v1.51/networks", nil)
		if resp, err := tr.RoundTrip(req); err == nil {
			resp.Body.Close()
		}
	}

	out := buf.String()
	if n := strings.Count(out, "docker-api call"); n != 1 {
		t.Errorf("the record was logged %d time(s) across three identical requests, want exactly 1.\n%s\n"+
			"The lane reads this line for the SET of shapes the plugin sends; one line per request "+
			"would bury it under the health poll.", n, out)
	}
	for _, want := range []string{`msg="docker-api call"`, "method=GET", "path=/v1.51/networks"} {
		if !strings.Contains(out, want) {
			t.Errorf("the record does not carry %s.\n%s\n"+
				"test/integration/docker_api_readonly_test.go parses this line; a rename here makes "+
				"that test find nothing to judge rather than judge something wrong.", want, out)
		}
	}
}

func TestDockerClient_InterfaceNamesOnlyReadMethods(t *testing.T) {
	allowed := map[string]bool{
		"NetworkList":      true,
		"NetworkInspect":   true,
		"ContainerInspect": true,
		// The engine probe (#670): Ping is GET or HEAD /_ping and ServerVersion is GET /v1.*/version.
		"Ping":          true,
		"ServerVersion": true,
		"Close":         true,
		"ClientVersion": true,
	}

	iface := reflect.TypeOf((*dockerClient)(nil)).Elem()
	if iface.NumMethod() == 0 {
		t.Fatal("dockerClient declares no methods; the check below would pass vacuously")
	}
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		if !allowed[name] {
			t.Errorf("dockerClient declares %q, which is not one of the read calls SECURITY.md tells "+
				"operators this plugin makes. A restricted socket proxy would refuse it and the "+
				"plugin would lose functionality behind one — which is the deployment #691 "+
				"documents as sufficient. Add it here only with the SECURITY.md sentence.", name)
		}
	}
}

func TestDockerHostFromEnv(t *testing.T) {
	for _, tc := range []struct{ name, set, want string }{
		{"unset", "", defaultDockerHost},
		{"empty", "", defaultDockerHost},
		{"a proxy over tcp", "tcp://127.0.0.1:2375", "tcp://127.0.0.1:2375"},
		{"a proxy over a unix socket", "unix:///run/docker-proxy.sock", "unix:///run/docker-proxy.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dockerHostFromEnv(func(k string) string {
				if k != envDockerHost {
					t.Fatalf("read %q, want %q", k, envDockerHost)
				}
				return tc.set
			})
			if got != tc.want {
				t.Errorf("dockerHostFromEnv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestManifestsDeclareDockerHost(t *testing.T) {
	for _, name := range pluginManifests {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m struct {
			Env []struct {
				Name     string   `json:"name"`
				Value    string   `json:"value"`
				Settable []string `json:"settable"`
			} `json:"env"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		found := false
		for _, e := range m.Env {
			if e.Name != envDockerHost {
				continue
			}
			found = true
			if e.Value != "" {
				t.Errorf("%s defaults %s to %q; it must default to empty so an operator who sets "+
					"nothing keeps the mounted socket", name, envDockerHost, e.Value)
			}
			if len(e.Settable) == 0 {
				t.Errorf("%s declares %s but not as settable, so no operator can point it anywhere", name, envDockerHost)
			}
		}
		if !found {
			t.Errorf("%s declares no %s; the code reads it (docker_transport.go), so the read-only "+
				"proxy deployment SECURITY.md documents would be unreachable", name, envDockerHost)
		}
	}
}

// The plugin sees only config.json's mounts, whose socket source is not settable, so the default must be one (#725).
func TestDefaultDockerHostIsAMountedPath(t *testing.T) {
	const socketMountSource = "/var/run/docker.sock"

	for _, name := range pluginManifests {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m struct {
			Mounts []struct {
				Source      string   `json:"source"`
				Destination string   `json:"destination"`
				Settable    []string `json:"settable"`
			} `json:"mounts"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		want := strings.TrimPrefix(defaultDockerHost, "unix://")
		found := false
		for _, mt := range m.Mounts {
			if mt.Source != socketMountSource {
				continue
			}
			found = true
			if mt.Destination != want {
				t.Errorf("%s mounts the Docker socket at %q, but defaultDockerHost is %q. An "+
					"operator who sets nothing would get a dial to a path the plugin cannot see",
					name, mt.Destination, defaultDockerHost)
			}
			for _, f := range mt.Settable {
				if f == "source" {
					t.Errorf("%s makes the Docker socket mount's source settable. SECURITY.md tells "+
						"operators a unix-socket proxy is NOT reachable for exactly the opposite "+
						"reason; that paragraph is now wrong and has to change with this", name)
				}
			}
		}
		if !found {
			t.Errorf("%s mounts no %s, so the default endpoint %q resolves to nothing",
				name, socketMountSource, defaultDockerHost)
		}
	}
}

func TestNewDockerClient_InstallsTheReadOnlyTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !safeDaemonMethods[r.Method] {
			t.Errorf("the daemon saw %s %s — the transport did not refuse it", r.Method, r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/networks") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := &Plugin{}
	cli, err := newDockerClient(strings.Replace(srv.URL, "http://", "tcp://", 1), p)
	if err != nil {
		t.Fatalf("newDockerClient: %v", err)
	}
	defer cli.Close()

	if got := p.dockerAPINonGETRefusals.Load(); got != 0 {
		t.Errorf("docker_api_non_get_refusals = %d before any call, want 0", got)
	}

	// Asserted through behaviour, since the client wraps the given transport in an OpenTelemetry one.
	if _, err := cli.NetworkList(context.Background(), dNetwork.ListOptions{}); err != nil {
		t.Fatalf("NetworkList through the wrapped client: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1.51/containers/create", nil)
	if resp, err := cli.HTTPClient().Transport.RoundTrip(req); err == nil {
		resp.Body.Close()
		t.Fatal("the wired client passed a POST to the daemon")
	} else if !strings.Contains(err.Error(), errUnsafeMethodToDaemon.Error()) {
		t.Errorf("the POST failed with %v, which is not this plugin's refusal — it may have been "+
			"sent and rejected by the server instead", err)
	}
	if got := p.dockerAPINonGETRefusals.Load(); got != 1 {
		t.Errorf("docker_api_non_get_refusals = %d after a refused POST, want 1", got)
	}
}

func TestManifestsDescribeTheSafeMethodContract(t *testing.T) {
	if len(safeDaemonMethods) == 0 {
		t.Fatal("safeDaemonMethods is empty; every assertion below would pass vacuously")
	}
	unsafe := []string{
		http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace,
	}

	for _, name := range pluginManifests {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m struct {
			Env []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"env"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		found := false
		for _, e := range m.Env {
			if e.Name != envDockerHost {
				continue
			}
			found = true
			for method := range safeDaemonMethods {
				if !strings.Contains(e.Description, method) {
					t.Errorf("%s describes %s as %q, which does not name %s. The plugin sends it "+
						"(safeDaemonMethods), `docker plugin inspect` is where an operator reads "+
						"this, and a proxy allowlist written from it refuses a request the plugin "+
						"makes.", name, envDockerHost, e.Description, method)
				}
			}
			for _, method := range unsafe {
				if !safeDaemonMethods[method] && strings.Contains(e.Description, method) {
					t.Errorf("%s describes %s as %q, which names %s — a method this plugin refuses "+
						"before sending. The description would tell an operator to allow it.",
						name, envDockerHost, e.Description, method)
				}
			}
		}
		if !found {
			t.Errorf("%s declares no %s", name, envDockerHost)
		}
	}
}
