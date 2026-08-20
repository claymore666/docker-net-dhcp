// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Replay of captured libnetwork requests (#644).
//
// Every other test in this package builds its request values by hand,
// which means it asserts this code against OUR MODEL of what the daemon
// sends. These tests assert against what the daemon actually sent,
// recorded by captureHandler during an integration run.
//
// #298 is why. stable_lease was designed against an assumed
// CreateEndpoint payload, shipped, and was reverted from v1.3.0 once
// the endpoint identity turned out to be unresolvable in the docker-run
// and compose flows. Nothing runnable without a daemon could see it.
//
// Regenerate with `make capture-fixtures` — see
// docs/internals.md#request-fixtures.

const fixtureRoot = "testdata/requests"

// fixtureManifest records WHICH daemon produced a capture. A fixture is
// a recording of one engine version, and a recording nobody can date is
// an assumption that agrees with itself forever.
type fixtureManifest struct {
	// Engine is the Docker Engine version that sent these requests, as
	// reported by `docker version --format {{.Server.Version}}`.
	Engine string `json:"engine"`
	// Captured is the ISO-8601 date of the run.
	Captured string `json:"captured"`
	// Commit is the repository commit the capturing plugin was built
	// from.
	Commit string `json:"commit"`
	// Flow describes the container lifecycle that produced the
	// requests, e.g. "docker run --rm" or "compose up then down".
	Flow string `json:"flow"`
}

type fixtureCall struct {
	file   string // basename, e.g. 0003-NetworkDriver.CreateEndpoint.json
	method string // e.g. NetworkDriver.CreateEndpoint
	body   []byte
}

type fixtureFlow struct {
	name     string
	manifest fixtureManifest
	calls    []fixtureCall
}

// loadFixtureFlows reads every captured flow. A missing or empty
// fixture set is a FAILURE, never a skip: a fixture suite that quietly
// tests nothing is the exact failure mode these fixtures exist to
// replace, and it would report green forever.
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
		// captureHandler writes NNNN-<Method>.json.
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

	// ReadDir sorts, and the sequence prefix is zero-padded, so this is
	// the order the daemon issued the calls in.
	sort.Slice(flow.calls, func(i, j int) bool { return flow.calls[i].file < flow.calls[j].file })

	if len(flow.calls) == 0 {
		t.Errorf("%s: no captured calls", dir)
	}
	return flow
}

// newRequestValue returns a fresh zero value of the request struct this
// package uses for the given libnetwork method, or false if the method
// carries no request body we model.
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

// THE POINT OF THIS FILE.
//
// Decoding with DisallowUnknownFields turns "the daemon sends a field we
// do not model" from something discovered on a privileged runner — or in
// production, or never — into a unit-test failure.
//
// It is the assertion #218 and #125 are both waiting on. Each is blocked
// on moby forwarding a field to a call we already receive
// (netlabel.EndpointName at CreateEndpoint, moby/moby#52870; DstName
// handling at Join, moby/moby#52865). The day an engine carrying either
// one produces a capture, this test names the new field.
//
// A failure here is NOT necessarily a defect. A new field may be
// irrelevant to us. It means the request contract moved and somebody has
// to decide — which is the whole point, because today nothing tells us
// it moved at all.
func TestFixtures_NoUnmodelledFields(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		for _, call := range flow.calls {
			v, ok := newRequestValue(call.method)
			if !ok {
				continue
			}
			dec := json.NewDecoder(bytes.NewReader(call.body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Errorf("%s/%s (engine %s): %v\n"+
					"The daemon sent a field this package does not model, or a field "+
					"changed type. Decide whether it matters, then either model it or "+
					"record why it is ignored.",
					flow.name, call.file, flow.manifest.Engine, err)
			}
		}
	}
}

// Every captured call must decode into the struct the handler uses.
// Separate from the unknown-field test on purpose: this one must stay
// green even while a contract change is being triaged.
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

// The fields the plugin reads must actually arrive. This is the #298
// assertion in its most direct form: stable_lease assumed an identity
// was resolvable at CreateEndpoint, and in the docker-run and compose
// flows it was not.
//
// Asserted per flow, because the flows are exactly where they differed.
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

// A capture is a recording of one engine version. Report which, so a
// reviewer reading a failure knows what produced it without going to
// the manifest.
func TestFixtures_ReportProvenance(t *testing.T) {
	for _, flow := range loadFixtureFlows(t) {
		t.Logf("flow %-24s engine %-10s captured %s  commit %s  (%d calls) — %s",
			flow.name, flow.manifest.Engine, flow.manifest.Captured,
			flow.manifest.Commit, len(flow.calls), flow.manifest.Flow)
	}
}
