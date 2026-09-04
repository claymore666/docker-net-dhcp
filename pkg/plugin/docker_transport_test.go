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

// countingBase records what actually reached the wire.
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

// TestReadOnlyTransport_RefusesEveryUnsafeMethod drives the whole
// method table rather than one representative write.
//
// One representative is what a prefix or case-insensitive comparison
// survives, and both of those are the plausible mistakes here: HTTP
// methods are case-sensitive (RFC 9110 section 9.1), so "get" is not
// GET and must be refused, and "GETX" shares GET's prefix.
//
// HEAD is deliberately absent from this table and present in the
// preservation control below: the client library's API-version
// negotiation sends HEAD /_ping, it is safe and body-less, and refusing
// it made the refusal counter read 1 on a plugin that had done nothing.
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

// The preservation control. Without it every case above is satisfied by
// a transport that refuses everything, and the plugin would fail to
// read the daemon at all — which is a strictly worse outcome than the
// one being guarded against.
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

// An empty Method is GET by http.Request's own contract, and refusing
// it would break the client library rather than the threat model.
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

// The record is the half the integration lane reads. A refusal that is
// counted but not recorded leaves the lane with nothing to assert
// against a real daemon.
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

// TestReadOnlyTransport_WritesTheRecordToTheLog pins the one thing the
// in-memory record above cannot: the LINE.
//
// calls() is unexported and lives in the plugin's process. The only way
// the claim "this plugin issues these calls and no others" reaches
// anyone outside is the log line the transport writes the first time it
// sees a shape, and the integration lane parses exactly that line to
// judge the set against a live daemon. So the message text and the two
// field names are an interface, and nothing else in this package
// treats them as one — a rename would leave every unit test green while
// the lane's parser silently found nothing to judge.
//
// It also asserts the ONCE: a line per request would put one entry per
// health poll in the log, which is the reason the record is keyed at
// all.
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

// TestDockerClient_InterfaceNamesOnlyReadMethods is the gate one layer
// above the transport.
//
// The transport refuses a write at run time, which is the right
// backstop and the wrong place to find out. This fails at `go test` the
// moment the narrow interface grows a method that is not a read — the
// one edit that turns "the plugin makes three read calls" from true
// into false, and the edit #691's whole proposal rests on not
// happening.
//
// The allowlist is written out rather than derived from a prefix: an
// "Inspect"/"List" prefix rule would admit `NetworkListAndPrune` and
// refuse nothing anyone would actually add.
func TestDockerClient_InterfaceNamesOnlyReadMethods(t *testing.T) {
	allowed := map[string]bool{
		"NetworkList":      true,
		"NetworkInspect":   true,
		"ContainerInspect": true,
		// Not an API call: closes the local connection pool.
		"Close": true,
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

// TestDockerHostFromEnv drives all three states of the setting. The
// default arm is the one that matters: an operator who sets nothing
// must keep the mounted socket, byte for byte, or this change breaks
// every existing installation to add an option nobody asked for.
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

// TestManifestsDeclareDockerHost closes the other direction of the
// setting: read but not declared means `docker plugin set DOCKER_HOST`
// fails and the documented proxy deployment does not exist.
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

// TestNewDockerClient_InstallsTheReadOnlyTransport is the wiring
// assertion: the refusal is only a contract if it is on the client the
// plugin actually uses.
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

	// A real read through the real client, so the wrapper is proven not
	// to have broken the calls the plugin depends on. The transport is
	// asserted through BEHAVIOUR rather than through its type: the
	// library wraps whatever it is given in an OpenTelemetry transport
	// after the options are applied, so a type assertion here would
	// measure the library's layering and not this plugin's contract.
	if _, err := cli.NetworkList(context.Background(), dNetwork.ListOptions{}); err != nil {
		t.Fatalf("NetworkList through the wrapped client: %v", err)
	}

	// The refusal, driven through the same client's transport chain.
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
