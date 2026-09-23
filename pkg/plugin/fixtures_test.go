// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Replay of captured libnetwork requests (#644), so tests assert what the daemon sent: stable_lease
// shipped against an assumed CreateEndpoint payload and was reverted (#298).
// Regenerate with `make capture-fixtures`.

const fixtureRoot = "testdata/requests"

type fixtureManifest struct {
	// Engine is the Docker Engine version that sent these requests.
	Engine string `json:"engine"`
	// Captured is the ISO-8601 date of the run.
	Captured string `json:"captured"`
	// Commit is the repository commit the capturing plugin was built from.
	Commit string `json:"commit"`
	// Flow describes the container lifecycle that produced the requests.
	Flow string `json:"flow"`
}

type fixtureCall struct {
	file   string
	method string
	body   []byte
}

type fixtureFlow struct {
	name     string
	manifest fixtureManifest
	calls    []fixtureCall
}

func loadFixtureFlows(t *testing.T) []fixtureFlow {
	t.Helper()

	entries, err := os.ReadDir(fixtureRoot)
	if err != nil {
		t.Fatalf("reading %s: %v\nRegenerate with `make capture-fixtures`.", fixtureRoot, err)
	}

	var flows []fixtureFlow
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		flows = append(flows, loadFixtureFlow(t, filepath.Join(fixtureRoot, e.Name())))
	}

	if len(flows) == 0 {
		t.Fatalf("%s contains no flow directories. These tests would otherwise pass having "+
			"replayed nothing at all. Regenerate with `make capture-fixtures`.", fixtureRoot)
	}
	return flows
}

func loadFixtureFlow(t *testing.T, dir string) fixtureFlow {
	t.Helper()

	flow := fixtureFlow{name: filepath.Base(dir)}

	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("%s: reading manifest.json: %v", dir, err)
	}
	if err := json.Unmarshal(mb, &flow.manifest); err != nil {
		t.Fatalf("%s: parsing manifest.json: %v", dir, err)
	}
	for field, v := range map[string]string{
		"engine":   flow.manifest.Engine,
		"captured": flow.manifest.Captured,
		"commit":   flow.manifest.Commit,
		"flow":     flow.manifest.Flow,
	} {
		if strings.TrimSpace(v) == "" {
			t.Errorf("%s: manifest.json has an empty %q — a capture nobody can date or "+
				"attribute cannot be reviewed for staleness", dir, field)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "manifest.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		_, method, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !ok {
			t.Errorf("%s/%s: filename is not NNNN-<Method>.json", dir, name)
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s/%s: %v", dir, name, err)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			t.Errorf("%s/%s is empty; an empty fixture asserts nothing", dir, name)
			continue
		}
		flow.calls = append(flow.calls, fixtureCall{file: name, method: method, body: body})
	}

	// os.ReadDir sorts and the prefix is zero-padded, so this is the order the daemon issued the calls in.
	sort.Slice(flow.calls, func(i, j int) bool { return flow.calls[i].file < flow.calls[j].file })

	if len(flow.calls) == 0 {
		t.Errorf("%s: no captured calls", dir)
	}
	return flow
}

func newRequestValue(method string) (interface{}, bool) {
	switch method {
	case "NetworkDriver.CreateNetwork":
		return &CreateNetworkRequest{}, true
	case "NetworkDriver.DeleteNetwork":
		return &DeleteNetworkRequest{}, true
	case "NetworkDriver.CreateEndpoint":
		return &CreateEndpointRequest{}, true
	case "NetworkDriver.DeleteEndpoint":
		return &DeleteEndpointRequest{}, true
	case "NetworkDriver.EndpointOperInfo":
		return &InfoRequest{}, true
	case "NetworkDriver.Join":
		return &JoinRequest{}, true
	case "NetworkDriver.Leave":
		return &LeaveRequest{}, true
	}
	return nil, false
}

// ParseJSONOrErrorResponse decodes with DisallowUnknownFields, so an unmodelled field is a 400 and
// the container does not start; #218 and #125 wait on moby/moby#52870 and #52865 adding fields.
func TestFixtures_NoUnmodelledFields(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		for _, call := range flow.calls {
			v, ok := newRequestValue(call.method)
			if !ok {
				continue
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/"+call.method, bytes.NewReader(call.body))
			if err := util.ParseJSONOrErrorResponse(v, rec, req); err != nil {
				t.Errorf("%s/%s (engine %s): handler would answer %d: %v\n"+
					"The daemon sent a field this package does not model, or a field "+
					"changed type, and ParseJSONOrErrorResponse decodes with "+
					"DisallowUnknownFields — so this is a 400 on a live container "+
					"start, not a warning. Decide whether it matters, then either "+
					"model it or record why it is ignored.",
					flow.name, call.file, flow.manifest.Engine, rec.Code, err)
			}
		}
	}
}

func TestFixtures_DecodeIntoHandlerTypes(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		for _, call := range flow.calls {
			v, ok := newRequestValue(call.method)
			if !ok {
				continue
			}
			if err := json.Unmarshal(call.body, v); err != nil {
				t.Errorf("%s/%s: %v", flow.name, call.file, err)
			}
		}
	}
}

func TestFixtures_RequiredFieldsPresent(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		var sawCreateEndpoint, sawJoin bool

		for _, call := range flow.calls {
			switch call.method {
			case "NetworkDriver.CreateEndpoint":
				sawCreateEndpoint = true
				var req CreateEndpointRequest
				if err := json.Unmarshal(call.body, &req); err != nil {
					t.Errorf("%s/%s: %v", flow.name, call.file, err)
					continue
				}
				if req.NetworkID == "" {
					t.Errorf("%s/%s: NetworkID is empty; state-file paths are derived from it",
						flow.name, call.file)
				}
				if req.EndpointID == "" {
					t.Errorf("%s/%s: EndpointID is empty; it keys the endpoint registry",
						flow.name, call.file)
				}

			case "NetworkDriver.Join":
				sawJoin = true
				var req JoinRequest
				if err := json.Unmarshal(call.body, &req); err != nil {
					t.Errorf("%s/%s: %v", flow.name, call.file, err)
					continue
				}
				if req.NetworkID == "" || req.EndpointID == "" {
					t.Errorf("%s/%s: Join needs both NetworkID and EndpointID; got %q/%q",
						flow.name, call.file, req.NetworkID, req.EndpointID)
				}
				if req.SandboxKey == "" {
					t.Errorf("%s/%s: SandboxKey is empty; the container netns is located from it",
						flow.name, call.file)
				}
			}
		}

		if !sawCreateEndpoint || !sawJoin {
			t.Errorf("flow %q captured CreateEndpoint=%v Join=%v; a lifecycle flow must contain both, "+
				"otherwise it is not recording the path it claims to",
				flow.name, sawCreateEndpoint, sawJoin)
		}
	}
}

func TestFixtures_ReportProvenance(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		t.Logf("flow %-24s engine %-10s captured %s  commit %s  (%d calls) — %s",
			flow.name, flow.manifest.Engine, flow.manifest.Captured,
			flow.manifest.Commit, len(flow.calls), flow.manifest.Flow)
	}
}

// libnetwork hands an ordinary `docker run` endpoint an empty Interface; the plugin leases the address (#298).
func TestFixtures_NoExplicitAddressInOrdinaryFlows(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		for _, call := range flow.calls {
			if call.method != "NetworkDriver.CreateEndpoint" {
				continue
			}
			var req CreateEndpointRequest
			if err := json.Unmarshal(call.body, &req); err != nil {
				t.Errorf("%s/%s: %v", flow.name, call.file, err)
				continue
			}
			ip, err := resolveExplicitV4(req)
			if err != nil {
				t.Errorf("%s/%s: resolveExplicitV4 on a real request: %v", flow.name, call.file, err)
				continue
			}
			if ip != "" {
				t.Errorf("%s/%s (engine %s): the daemon supplied %s at CreateEndpoint. "+
					"None of these flows passes --ip, so the address the plugin leases "+
					"is no longer the only one in play — decide which wins before this "+
					"reaches a user.",
					flow.name, call.file, flow.manifest.Engine, ip)
			}
		}
	}
}
