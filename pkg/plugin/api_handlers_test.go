package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	withStateDir(t, t.TempDir())
	return &Plugin{
		joinHints:            make(map[string]joinHint),
		persistentDHCP:       make(map[string]*dhcpManager),
		endpointFingerprints: make(map[string]endpointFingerprint),
	}
}

func TestApiGetCapabilities(t *testing.T) {
	p := newTestPlugin(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/NetworkDriver.GetCapabilities", nil)
	p.apiGetCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var got CapabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Scope != "local" {
		t.Errorf("scope: got %q want local", got.Scope)
	}
	if got.ConnectivityScope != "global" {
		t.Errorf("connectivity scope: got %q want global", got.ConnectivityScope)
	}
}

// decodeErrBody decodes the application/problem+json body into the
// jsonError shape (`{"Err": "..."}`). All wrappers use util.JSONErrResponse
// which writes that schema.
func decodeErrBody(t *testing.T, body []byte) string {
	t.Helper()
	var shape struct {
		Err string `json:"Err"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("error body decode: %v (raw=%q)", err, string(body))
	}
	return shape.Err
}

// TestApiWrappers_BadJSON exercises the JSON-decode failure path of every
// HTTP wrapper in one place. Each wrapper calls util.ParseJSONOrErrorResponse
// first; a malformed body must short-circuit with a 400 application/problem+json
// response and never invoke the underlying network method (which would need
// netlink/docker and crash this unit test).
func TestApiWrappers_BadJSON(t *testing.T) {
	p := newTestPlugin(t)

	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"CreateNetwork", p.apiCreateNetwork},
		{"DeleteNetwork", p.apiDeleteNetwork},
		{"CreateEndpoint", p.apiCreateEndpoint},
		{"EndpointOperInfo", p.apiEndpointOperInfo},
		{"DeleteEndpoint", p.apiDeleteEndpoint},
		{"Join", p.apiJoin},
		{"Leave", p.apiLeave},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
			c.handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content-type: got %q want application/problem+json", ct)
			}
			if msg := decodeErrBody(t, rec.Body.Bytes()); !strings.Contains(msg, "failed to parse") {
				t.Errorf("body: got %q want substring 'failed to parse'", msg)
			}
		})
	}
}

func TestApiCreateNetwork_InvalidModeMaps400(t *testing.T) {
	p := newTestPlugin(t)

	body, err := json.Marshal(CreateNetworkRequest{
		NetworkID: "net-invalid-mode",
		Options: map[string]interface{}{
			"com.docker.network.generic": map[string]interface{}{
				"mode":   "wireguard",
				"parent": "ens18",
			},
		},
		IPv4Data: []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiCreateNetwork(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	msg := decodeErrBody(t, rec.Body.Bytes())
	// errors.Is on the wire isn't possible, but the sentinel's literal text
	// is what makes the response actionable for an operator.
	if !strings.Contains(msg, "invalid mode") {
		t.Errorf("body: got %q want substring 'invalid mode'", msg)
	}
}

func TestApiCreateNetwork_BadIPAMMaps400(t *testing.T) {
	p := newTestPlugin(t)

	body, err := json.Marshal(CreateNetworkRequest{
		NetworkID: "net-bad-ipam",
		Options: map[string]interface{}{
			"com.docker.network.generic": map[string]interface{}{"bridge": "br0"},
		},
		IPv4Data: []*IPAMData{{AddressSpace: "default", Pool: "10.0.0.0/8"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiCreateNetwork(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec.Body.Bytes()); !strings.Contains(msg, "null IPAM") {
		t.Errorf("body: got %q want substring 'null IPAM'", msg)
	}
}

func TestApiCreateNetwork_BridgeMissingMaps400(t *testing.T) {
	p := newTestPlugin(t)

	body, err := json.Marshal(CreateNetworkRequest{
		NetworkID: "net-no-bridge",
		Options:   map[string]interface{}{"com.docker.network.generic": map[string]interface{}{}},
		IPv4Data:  []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiCreateNetwork(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if msg := decodeErrBody(t, rec.Body.Bytes()); !strings.Contains(msg, "bridge required") {
		t.Errorf("body: got %q want substring 'bridge required'", msg)
	}
}

// TestApiDeleteNetwork_NoState verifies the success path of the only
// HTTP wrapper whose underlying method is fully unit-testable: with no
// state on disk and no DHCP managers, DeleteNetwork is a no-op that
// returns 200 with a `{}` body.
func TestApiDeleteNetwork_NoState(t *testing.T) {
	p := newTestPlugin(t)

	body, err := json.Marshal(DeleteNetworkRequest{NetworkID: "net-ghost"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiDeleteNetwork(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("body: got %q want {}", got)
	}
}

// stoppableManager returns a dhcpManager whose Stop() short-circuits
// because startedCh is closed AND startErr is set — that branch
// returns nil immediately without touching netlink/handles.
func stoppableManager(networkID string) *dhcpManager {
	m := &dhcpManager{
		joinReq:   JoinRequest{NetworkID: networkID},
		startedCh: make(chan struct{}),
		startErr:  errStubManager,
	}
	close(m.startedCh)
	return m
}

var errStubManager = errors.New("stub manager (test-only)")

// TestApiDeleteNetwork_DropsOrphanedManagers exercises the
// takeDHCPManagersForNetwork prune introduced for the recovery-then-
// network-removed lifecycle case (#44). Two managers belong to the
// removed network, one to a peer; only the two get evicted.
func TestApiDeleteNetwork_DropsOrphanedManagers(t *testing.T) {
	p := newTestPlugin(t)

	// Seed three managers across two networks. The stub managers'
	// Stop() short-circuits via startErr so DeleteNetwork's wg.Wait
	// completes without touching netlink.
	p.persistentDHCP["ep-A1"] = stoppableManager("net-A")
	p.persistentDHCP["ep-A2"] = stoppableManager("net-A")
	p.persistentDHCP["ep-B1"] = stoppableManager("net-B")

	body, err := json.Marshal(DeleteNetworkRequest{NetworkID: "net-A"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiDeleteNetwork(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// net-A managers gone; net-B's survives.
	if _, ok := p.persistentDHCP["ep-A1"]; ok {
		t.Error("ep-A1 must be evicted by DeleteNetwork(net-A)")
	}
	if _, ok := p.persistentDHCP["ep-A2"]; ok {
		t.Error("ep-A2 must be evicted by DeleteNetwork(net-A)")
	}
	if _, ok := p.persistentDHCP["ep-B1"]; !ok {
		t.Error("ep-B1 belongs to net-B and must survive DeleteNetwork(net-A)")
	}
}

// TestDecodeOpts_RejectsUnknownField verifies the mapstructure decoder
// is configured with ErrorUnused: a typo'd option key fails fast at
// decode time rather than being silently dropped, which is what makes
// `docker network create -o moed=macvlan ...` (typo) surface as a 400
// instead of falling through to default-mode behaviour.
func TestDecodeOpts_RejectsUnknownField(t *testing.T) {
	_, err := decodeOpts(map[string]interface{}{
		"mode":      "macvlan",
		"parent":    "ens18",
		"unknownXX": "value",
	})
	if err == nil {
		t.Fatal("expected error from unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "unknownXX") {
		t.Errorf("err should name the offending field; got %v", err)
	}
}

// TestDecodeOpts_DurationParsing verifies the StringToTimeDurationHookFunc
// is wired so an operator can pass `-o lease_timeout=45s` and get a real
// time.Duration on the receiving side.
func TestDecodeOpts_DurationParsing(t *testing.T) {
	opts, err := decodeOpts(map[string]interface{}{
		"mode":          "macvlan",
		"parent":        "ens18",
		"lease_timeout": "45s",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := opts.LeaseTimeout.Seconds(); got != 45 {
		t.Errorf("LeaseTimeout: got %v want 45s", opts.LeaseTimeout)
	}
}

// TestDecodeOpts_BoolFromString verifies the WeaklyTypedInput coercion
// path: docker passes string-typed driver options on the wire even when
// the operator's intent was a bool, so the decoder must coerce
// "true"/"false" rather than failing.
func TestDecodeOpts_BoolFromString(t *testing.T) {
	opts, err := decodeOpts(map[string]interface{}{
		"mode":             "macvlan",
		"parent":           "ens18",
		"ignore_conflicts": "true",
		"skip_routes":      "false",
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !opts.IgnoreConflicts {
		t.Errorf("IgnoreConflicts: got false, want true")
	}
	if opts.SkipRoutes {
		t.Errorf("SkipRoutes: got true, want false")
	}
}

// TestDecodeOpts_NilInput is the empty-options path libnetwork hits when
// `docker network create` was run without any `-o` flags. It must not
// error — validateModeOptions later catches the missing-bridge case.
func TestDecodeOpts_NilInput(t *testing.T) {
	opts, err := decodeOpts(nil)
	if err != nil {
		t.Fatalf("decode(nil): %v", err)
	}
	if opts.Mode != "" || opts.Bridge != "" || opts.Parent != "" {
		t.Errorf("zero options expected, got %+v", opts)
	}
}

// TestApiCreateNetwork_DecodeOptsError covers the error path where the
// driver-opts payload itself is shaped wrong (a non-map under the
// generic key). decodeOpts returns a wrapped mapstructure error which
// ErrToStatus doesn't recognize → 500. That's the correct shape: the
// caller's request was structurally invalid in a way that bypasses
// our sentinel set.
func TestApiCreateNetwork_DecodeOptsError(t *testing.T) {
	p := newTestPlugin(t)

	// String under the generic key — decodeOpts can't decode that into
	// the DHCPNetworkOptions struct.
	body, err := json.Marshal(CreateNetworkRequest{
		NetworkID: "net-bad-opts",
		Options:   map[string]interface{}{"com.docker.network.generic": "not-a-map"},
		IPv4Data:  []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiCreateNetwork(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	if msg := decodeErrBody(t, rec.Body.Bytes()); !strings.Contains(msg, "decode network options") {
		t.Errorf("body: got %q want substring 'decode network options'", msg)
	}
}

// TestApiCreateNetwork_BridgeAndParentRejected covers the
// ErrModeMismatch branch — a 400 path that goes through validateModeOptions
// rather than validateIPAMData / decodeOpts.
func TestApiCreateNetwork_BridgeAndParentRejected(t *testing.T) {
	p := newTestPlugin(t)

	body, err := json.Marshal(CreateNetworkRequest{
		NetworkID: "net-mode-mismatch",
		Options: map[string]interface{}{
			"com.docker.network.generic": map[string]interface{}{
				"mode":   "macvlan",
				"parent": "ens18",
				"bridge": "br0",
			},
		},
		IPv4Data: []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	p.apiCreateNetwork(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec.Body.Bytes()); !strings.Contains(msg, "option does not apply") {
		t.Errorf("body: got %q want substring 'option does not apply'", msg)
	}
}

