// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	dTypes "github.com/docker/docker/api/types"
)

// Each spelling is one a daemon reports: upstream, Debian or Ubuntu `+dfsg1` and `+azure`, the
// older `-ce`, Rancher Desktop `-rd`, and a release candidate (#670).
func TestEngineBelowFloor_VersionSpellings(t *testing.T) {
	const floor = "20.10"

	cases := []struct {
		name    string
		version string
		below   bool
		ok      bool
	}{
		{"the floor itself", "20.10.24", false, true},
		{"a patch of the floor line", "20.10.5", false, true},
		{"the line below the floor", "19.03.15", true, true},
		{"two lines below", "18.09.9", true, true},
		{"the current release", "29.8.0", false, true},
		{"a major above with a lower minor", "23.0.6", false, true},

		{"a minor that is not a decimal fraction", "20.9.1", true, true},
		{"a two-digit minor sorts above a one-digit one", "20.10.0", false, true},

		{"a Debian package suffix", "20.10.5+dfsg1", false, true},
		{"a Debian package suffix below the floor", "19.03.8+dfsg1", true, true},
		{"an Azure build", "24.0.9+azure.1", false, true},
		{"the pre-2017 community spelling", "20.10.7-ce", false, true},
		{"the pre-2017 community spelling below the floor", "19.03.8-ce", true, true},
		{"a Rancher Desktop build", "24.0.7-rd", false, true},
		{"an upstream release candidate", "29.0.0-rc.1", false, true},
		{"a version with no patch at all", "23.0", false, true},

		{"an empty version", "", false, false},
		{"the word unknown", unknownEngineField, false, false},
		{"a captured error message", "OCI runtime exec failed: exec failed", false, false},
		{"a version with a leading v", "v25.0.3", false, false},
		{"a major with no minor", "25", false, false},
		{"a negative major", "-1.0", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			below, ok := engineBelowFloor(tc.version, floor)
			if ok != tc.ok {
				t.Fatalf("engineBelowFloor(%q, %q) ok = %v, want %v", tc.version, floor, ok, tc.ok)
			}
			if below != tc.below {
				t.Errorf("engineBelowFloor(%q, %q) below = %v, want %v", tc.version, floor, below, tc.below)
			}
		})
	}
}

func TestEngineBelowFloor_UnreadableFloorIsNotAVerdict(t *testing.T) {
	if below, ok := engineBelowFloor("19.03.15", "twenty-ten"); ok || below {
		t.Errorf("an unreadable floor gave below=%v ok=%v, want false/false", below, ok)
	}
}

func TestMinEngineVersion_IsAMeasuredLine(t *testing.T) {
	if _, _, ok := versionKey(MinEngineVersion); !ok {
		t.Fatalf("MinEngineVersion %q does not parse as a version", MinEngineVersion)
	}
	if strings.Count(MinEngineVersion, ".") != 1 {
		t.Errorf("MinEngineVersion %q is not major.minor; the matrix measures lines, not patches", MinEngineVersion)
	}
}

func engineFake(version, api string) *fakeDocker {
	return &fakeDocker{versionResult: dTypes.Version{Version: version}, clientVersion: api}
}

func TestProductionEngineVersion_IsABuildTheFloorAdmits(t *testing.T) {
	if _, _, ok := versionKey(ProductionEngineVersion); !ok {
		t.Fatalf("ProductionEngineVersion %q does not parse as a version", ProductionEngineVersion)
	}
	if strings.Count(ProductionEngineVersion, ".") != 2 {
		t.Errorf("ProductionEngineVersion %q is not major.minor.patch; it names a build, not a line", ProductionEngineVersion)
	}
	below, ok := engineBelowFloor(ProductionEngineVersion, MinEngineVersion)
	if !ok {
		t.Fatalf("ProductionEngineVersion %q and MinEngineVersion %q cannot be ordered against each other",
			ProductionEngineVersion, MinEngineVersion)
	}
	if below {
		t.Errorf("ProductionEngineVersion %q is below MinEngineVersion %q: the shipped plugin refuses to start there",
			ProductionEngineVersion, MinEngineVersion)
	}
}

func TestProbeEngine_RefusesBelowTheFloorNamingBoth(t *testing.T) {
	p := &Plugin{docker: engineFake("19.03.15", "1.40")}

	err := p.probeEngine(context.Background())
	if err == nil {
		t.Fatal("probeEngine accepted an engine below the floor")
	}
	if !errors.Is(err, errEngineTooOld) {
		t.Errorf("error %v does not wrap errEngineTooOld", err)
	}
	if !strings.Contains(err.Error(), "19.03.15") {
		t.Errorf("the refusal does not name the engine it saw: %v", err)
	}
	if !strings.Contains(err.Error(), MinEngineVersion) {
		t.Errorf("the refusal does not name the floor %q: %v", MinEngineVersion, err)
	}

	if got := p.engineSnapshot().Version; got != "19.03.15" {
		t.Errorf("engine version after a refusal: got %q want 19.03.15", got)
	}
}

func TestProbeEngine_AcceptsTheFloorAndAbove(t *testing.T) {
	for _, v := range []string{MinEngineVersion + ".24", "26.1.4", "29.8.0"} {
		p := &Plugin{docker: engineFake(v, "1.44")}
		if err := p.probeEngine(context.Background()); err != nil {
			t.Errorf("probeEngine(%s): %v", v, err)
		}
		if got := p.engineSnapshot().Version; got != v {
			t.Errorf("engine version: got %q want %q", got, v)
		}
		if got := p.engineSnapshot().APIVersion; got != "1.44" {
			t.Errorf("api version: got %q want 1.44", got)
		}
	}
}

// Docker respawns the plugin during its own startup, when the daemon is routinely not serving (#383).
func TestProbeEngine_AnUnreachableDaemonIsNotARefusal(t *testing.T) {
	f := &fakeDocker{pingErr: errors.New("dial unix /var/run/docker.sock: connect: no such file")}
	p := &Plugin{docker: f}

	out := captureLog(t, func() {
		if err := p.probeEngine(context.Background()); err != nil {
			t.Fatalf("an unreachable daemon must not refuse: %v", err)
		}
	})

	id := p.engineSnapshot()
	if id.Version != unknownEngineField || id.APIVersion != unknownEngineField {
		t.Errorf("identity after an unreachable daemon: %+v, want both %q", id, unknownEngineField)
	}
	if !strings.Contains(out, "the minimum was not checked") {
		t.Errorf("the log does not say the minimum went unchecked: %s", out)
	}
}

func TestProbeEngine_AnUnreadableVersionIsNotARefusal(t *testing.T) {
	p := &Plugin{docker: engineFake("Docker Engine, but not a version", "1.44")}

	out := captureLog(t, func() {
		if err := p.probeEngine(context.Background()); err != nil {
			t.Fatalf("an unreadable version must not refuse: %v", err)
		}
	})

	if !strings.Contains(out, "could not be read as a version") {
		t.Errorf("the log does not say the version was unreadable: %s", out)
	}
}

func TestProbeEngine_AnEmptyVersionIsNoIdentity(t *testing.T) {
	p := &Plugin{docker: engineFake("", "1.44")}

	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("an empty version must not refuse: %v", err)
	}
	if got := p.engineSnapshot().Version; got != unknownEngineField {
		t.Errorf("engine version: got %q want %q", got, unknownEngineField)
	}
}

// The negotiated API is the lower of the two maxima, and a socket proxy can pin an old API before a current engine
// (#670).
func TestEngineFloor_APIBelowFloorWarnsWithoutRefusing(t *testing.T) {
	p := &Plugin{docker: engineFake("29.8.0", "1.24")}

	var err error
	out := captureLog(t, func() { err = p.probeEngine(context.Background()) })
	if err != nil {
		t.Fatalf("a low negotiated API must not refuse: %v", err)
	}
	if !strings.Contains(out, "negotiated Docker API version is below") {
		t.Errorf("the log does not name the disagreement: %s", out)
	}
	if !strings.Contains(out, MinEngineAPIVersion) {
		t.Errorf("the warning does not name the API the floor row reported: %s", out)
	}

	id := p.engineSnapshot()
	if id.Version != "29.8.0" || id.APIVersion != "1.24" {
		t.Errorf("identity: %+v, want engine 29.8.0 and api 1.24", id)
	}
}

func TestEngineFloor_AHighAPIDoesNotRescueAnOldEngine(t *testing.T) {
	p := &Plugin{docker: engineFake("19.03.15", "1.51")}

	if err := p.probeEngine(context.Background()); !errors.Is(err, errEngineTooOld) {
		t.Fatalf("a high negotiated API rescued an engine below the floor: %v", err)
	}
}

func TestReprobeEngine_PublishesWithoutRefusing(t *testing.T) {
	f := &fakeDocker{pingErr: errors.New("no daemon yet")}
	p := &Plugin{docker: f}
	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine: %v", err)
	}

	f.pingErr = nil
	f.versionResult = dTypes.Version{Version: "19.03.15"}
	f.clientVersion = "1.40"

	out := captureLog(t, func() { p.reprobeEngine(context.Background()) })

	if got := p.engineSnapshot().Version; got != "19.03.15" {
		t.Errorf("engine version after the re-probe: got %q want 19.03.15", got)
	}
	if !strings.Contains(out, "below the minimum") {
		t.Errorf("the re-probe did not say the engine is below the minimum: %s", out)
	}
}

func TestReprobeEngine_DoesNothingWhenTheIdentityIsKnown(t *testing.T) {
	f := engineFake("26.1.4", "1.45")
	p := &Plugin{docker: f}
	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine: %v", err)
	}
	calls := f.versionCalls

	p.reprobeEngine(context.Background())

	if f.versionCalls != calls {
		t.Errorf("the re-probe asked again: %d version calls, want %d", f.versionCalls, calls)
	}
	if got := p.engineSnapshot().Version; got != "26.1.4" {
		t.Errorf("engine version: got %q want 26.1.4", got)
	}
}

func TestHealthSnapshot_EngineFieldsAreNeverEmpty(t *testing.T) {
	p := newTestPlugin(t)

	h := p.healthSnapshot()
	if h.EngineVersion != unknownEngineField || h.APIVersion != unknownEngineField {
		t.Errorf("health engine fields: got %q/%q, want both %q",
			h.EngineVersion, h.APIVersion, unknownEngineField)
	}
}

func TestHealthSnapshot_PublishesWhatTheDaemonSaid(t *testing.T) {
	p := newTestPlugin(t)
	p.docker = engineFake("26.1.4", "1.45")
	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine: %v", err)
	}

	h := p.healthSnapshot()
	if h.EngineVersion != "26.1.4" {
		t.Errorf("engine_version: got %q want 26.1.4", h.EngineVersion)
	}
	if h.APIVersion != "1.45" {
		t.Errorf("api_version: got %q want 1.45", h.APIVersion)
	}
}

// Measured on the engine matrix rig with com.docker.network.endpoint.ifname=lan0: 28.5.2 and 29.7.2
// kept the driver prefix, 29.8.0 named the interface lan0 (#125, moby/moby#52866).
func TestEngineVersionAppliesIfname_TheMeasuredBoundary(t *testing.T) {
	cases := []struct {
		version string
		applies bool
		known   bool
	}{
		{"29.8.0", true, true},
		{"29.7.2", false, true},
		{"28.5.2", false, true},
		{"20.10.24", false, true},
		{"30.0.0", true, true},
		{unknownEngineField, false, false},
		{"", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			p := &Plugin{}
			p.engine.Store(&engineIdentity{Version: tc.version, APIVersion: "1.51"})

			applies, known := p.engineVersionAppliesIfname()
			if known != tc.known {
				t.Fatalf("known = %v, want %v", known, tc.known)
			}
			if applies != tc.applies {
				t.Errorf("applies = %v, want %v", applies, tc.applies)
			}
		})
	}
}

func TestNoteIfnameRequest_SaysSoWhenTheEngineIgnoresIt(t *testing.T) {
	p := &Plugin{}
	p.engine.Store(&engineIdentity{Version: "28.5.2", APIVersion: "1.51"})

	out := captureLog(t, func() { p.noteIfnameRequest("net1", "ep1", "lan0") })

	if got := p.ifnameUnsupported.Load(); got != 1 {
		t.Errorf("ifname_unsupported: got %d want 1", got)
	}
	for _, want := range []string{"older than the first", "lan0", "28.5.2", MinEngineIfnameVersion} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not carry %q: %s", want, out)
		}
	}
	// "eth0" is the fallback only when the driver prefix is eth and the index is 0.
	if strings.Contains(out, "eth0") {
		t.Errorf("the log names a fallback interface this plugin did not choose: %s", out)
	}
}

func TestNoteIfnameRequest_HonoursOnAnEngineThatApplies(t *testing.T) {
	p := &Plugin{}
	p.engine.Store(&engineIdentity{Version: "29.8.0", APIVersion: "1.56"})

	out := captureLog(t, func() { p.noteIfnameRequest("net1", "ep1", "lan0") })

	if got := p.ifnameUnsupported.Load(); got != 0 {
		t.Errorf("ifname_unsupported: got %d want 0 on an engine that applies the name", got)
	}
	if !strings.Contains(out, "Honoring custom interface name") {
		t.Errorf("the log does not record the honoured request: %s", out)
	}
}

func TestNoteIfnameRequest_CountsNothingOnAnUnknownEngine(t *testing.T) {
	p := &Plugin{}

	out := captureLog(t, func() { p.noteIfnameRequest("net1", "ep1", "lan0") })

	if got := p.ifnameUnsupported.Load(); got != 0 {
		t.Errorf("ifname_unsupported: got %d want 0 when the engine version is unknown", got)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("the log does not say the engine version is unknown: %s", out)
	}
}
